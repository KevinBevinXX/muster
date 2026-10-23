# The machine job API

An external orchestrator submits a one-shot **job** — run this prompt on this
agent, optionally at this commit, and give me the result — and polls for its
outcome. A job runs as one turn of an existing, running `claude-code` agent: one
it names, or one muster chooses from a named **worker pool**.

> **Who a `sha` job is safe for: commits by trusted members only.** A `sha`
> job is isolated from its commit's agent configuration, and when it runs on a
> **pool** worker, that worker holds no credential that could write to the
> repository (a job naming an agent gets no such guarantee: see below). It is
> NOT isolated from the commit's code. That is enough for reviewing commits written
> by people you already trust with your repository — a teammate's pull
> request. It is **not** enough for a third party's commit (a pull request from
> a fork, an outside contributor): keep those away from the job API until
> workers also get execution isolation, such as an egress policy that stops a
> turn from sending what it can read.
>
> **Isolated:**
>
> - **The commit's agent configuration.** The job runs in a git worktree of its
>   own, outside the agent's checkout, in a fresh Claude Code session whose CLI
>   starts in that worktree. Before the turn, these paths are removed and the
>   repository's default branch's copies (`origin/HEAD`) put back where it has
>   them — or, with `config_ref`, that commit's copies, which must be on the
>   default branch: every `.claude/` directory at any depth (settings with their hooks —
>   commands the CLI runs — agents, and skills, which the CLI also finds in
>   subdirectories); the root `CLAUDE.md`, `CLAUDE.local.md` and `AGENTS.md`
>   (the instructions it loads at launch, `CLAUDE.local.md` being its personal
>   memory file); the root `.mcp.json` (MCP servers, whose commands it starts).
>   The script reads every path list NUL-separated and as bytes (in the C
>   locale), so a name with bytes outside ASCII — valid UTF-8 or not — is
>   checked and restored like any other, whatever locale the agent's instance
>   runs with. With the real CLI, a commit whose `.claude/settings.json` defines a
>   SessionStart hook and whose `.mcp.json` defines a server runs neither in its
>   job, and runs both without this step
>   ([`realcli_test.go`](../internal/agentprovision/realcli_test.go), in CI's
>   image job).
> - **Write access to the repository, on a pool worker.** The commit is
>   fetched anonymously, as
>   pull request `pr`'s head from `https://github.com/<repo>` — not through
>   muster's credentialed proxy, whatever git routing the pod has (in CI, with
>   that routing in force, a plain fetch reaches the proxy and the job's fetch
>   does not) — and the job runs only at that head or one of its ancestors. A
>   pool worker holds no approved grant on any connector that reaches
>   github.com: such a grant is never approved for it, and a worker holding one
>   anyway is skipped ([Worker pools](#worker-pools)). So a pool job's turn has
>   no credential to push, comment or merge with.
> - **A runtime that ignores the worktree.** ccd answers every such turn with
>   the directory its CLI worked in (`X-Muster-CC-Workdir-Used`). When the
>   answer is missing — a ccd from before job worktrees, which would have run
>   the turn in the agent's own checkout — or names another directory, the
>   turn's reply is discarded and the job fails `workdir_unconfirmed`.
>
> **Not isolated:** the turn runs in the worker's pod with the worker's Claude
> account, its network (any public HTTPS host) and its volume. Every other file
> in the commit is the commit's — `CLAUDE.md` / `AGENTS.md` below the root
> (instructions the CLI reads as it works there), build scripts, tests,
> dependencies — and the turn may run them if the prompt leads it there. The
> commit's code can therefore read what the pod holds (the worker's Claude
> credential, the volume) and send it anywhere on the public internet. A named
> agent (not a pool member) may also hold a GitHub grant, and the commit's code
> could use it. And the default branch's restored `CLAUDE.md` may `@import`
> other files by path; if the CLI follows such an import into the worktree, it
> reads the COMMIT's copy of that file. Both are accepted for commits by
> trusted members.

## Auth

Both routes are on the service tier: `MUSTER_SERVICE_TOKEN` as
`Authorization: Bearer …` (or `X-Muster-Token`), plus an `X-Muster-Actor`
header naming the caller ([auth.md](auth.md#machine-credentials)). The actor is
recorded on the job as who requested it.

- No token, or a wrong one: **401**.
- `MUSTER_SERVICE_TOKEN` unset on the server: **503** with `"unarmed": true`.
  The tier fails closed.
- The tier acts as user 1, so a job can name only user 1's agents and pools.

## Submitting a job

```
POST /api/jobs
{
  "agent": "keen-wren",            // an existing claude-code agent — or:
  "worker_pool": "reviewers",      // a pool to choose one from (exactly one of the two)
  "prompt": "Review the change …", // required, at most 64 KiB
  "repo": "owner/name",            // optional: must be the agent's repository (required with worker_pool and sha)
  "sha": "0123456789abcdef0123456789abcdef01234567", // optional: a full 40-hex commit id
  "pr": 42,                        // required with sha (and only with it): the pull request the commit is on
  "config_ref": "89abcdef0123456789abcdef0123456789abcdef", // optional, only with sha: the default-branch commit the agent configuration comes from
  "model": "sonnet",               // optional: the Claude model the job's own session runs on
  "result_path": "out/review.md",  // optional: relative to the job's working directory
  "timeout_s": 1800,               // optional: 1-2100, default 1800
  "idempotency_key": "review-42"   // optional, at most 128 bytes
}
```

**202** `{"job_id": 17, "status": "queued", …}`, the same shape as a read.

Unknown fields are refused, so a misspelt `result_path` is a 400 rather than a
job that returns the wrong thing.

| answer | when |
|---|---|
| 400 | a missing or malformed field — including both or neither of `agent` and `worker_pool`, a `worker_pool` with a `sha` but no `repo`, a `sha` without a `pr` (or a `pr` without a `sha`), a `repo` that is not `owner/name`, and an abbreviated `sha`: only a full 40-character id is accepted; a `config_ref` that is not a full 40-character id, or one without a `sha`; a `model` outside the pattern below |
| 404 | no agent by that name; no agent is a member of that `worker_pool` |
| 409 | the agent is not `claude-code`; `repo` is not the agent's repository; `sha` on an agent with no repository; an `idempotency_key` already used with a different `model` or `config_ref` (below) |
| 429 | the agent already has 4 jobs queued or running (the per-agent limit); for a `worker_pool`, every member that could otherwise take the job has 4, or runs on a Claude account that is cooling down after a rate limit or already running its limit of concurrent jobs (`error` names each, and when a cooldown ends). Submit again once one finishes or the cooldown ends |
| 501 | `sha` / `result_path` on a deployment with no provisioner |
| 503 | no database, no agent gateway, or the service tier is unarmed; or, for a `worker_pool`, no member can take the job for a reason other than the limits above (`error` says why for each) |

**Idempotency.** A second submission with an `idempotency_key` already used
records nothing and answers the first job, with **200** and
`"deduplicated": true` — before any agent is looked up or worker chosen, so a
retry is answered even when its pool now has no eligible worker.

The key is compared with `model` and `config_ref` and with nothing else in the
body. If either differs from the first job's — including one sent where the
first had none, or left out where it had one — the answer is **409** with
`"idempotency_mismatch": true`, the first job's fields, and an `error` naming
the first job and each difference
(`idempotency_key "review-42" already names job 17, which has model "opus", not "sonnet"; use a new key for a different job`).
Nothing is recorded or run. Use a new key to run the job with the new values.

### `model`

The Claude model the job's turn runs on. Absent, the CLI's default, as before.

- **Accepted:** an alias (`opus`, `sonnet`, `haiku`) or a full model id,
  optionally with the CLI's `[1m]` suffix: 1-80 characters of `[a-z0-9.-]`,
  starting with a letter or digit, then optionally `[1m]`
  (`^[a-z0-9][a-z0-9.-]{0,79}(\[1m\])?$`). This is the check a Claude Code
  chat session's model gets, and ccd checks it again before it starts a CLI.
  muster does not check that the account can use the model: a model the CLI
  refuses fails the job with ccd's reason.
- **Where it goes:** the job's own Claude Code session is opened with it, so
  ccd starts that session's CLI with `--model`. The agent's main session and
  every other session are not touched. On a deployment whose chat store cannot
  open a session of the job's own, a job with a `model` fails `internal`
  before its turn — the refusal a `sha` job gets there — rather than run in
  the agent's own session.
- **Read back:** `model` on the job (`""` when none was given).

### `config_ref`

The commit a `sha` job's worktree takes its agent configuration from (the
overlay above), instead of the default branch's tip. Absent, the default
branch's tip (`origin/HEAD`), as before. A caller that recorded the version of
the configuration it reviews with — a hash of a `.claude/agents/` file, say —
sends the commit it read it at, so a merge to the default branch between its
read and the job's start does not change what the job runs with.

- **Accepted:** a full 40-character lowercase hex commit id, on a job with a
  `sha` only (a job without one has no worktree and no overlay).
- **Refused when it is not on the default branch.** The commit must be the
  checkout's `origin/HEAD` or an ancestor of it. If it is not, the default
  branch is fetched anonymously, the same way as the pull request (`HEAD` of
  `https://github.com/<repo>`, into a ref of the job's own, deleted with the
  worktree), and the commit must be its tip or an ancestor. Otherwise the job
  fails `checkout_failed`, and nothing is run. Any other commit — the pull
  request's own head, above all — would hand the commit under review its own
  configuration back.
- **Read back:** `config_ref` on the job (`""` when none was given).

## Worker pools

A worker pool is a name carried by long-lived `claude-code` agents. An agent
becomes a member when it is created with `worker_pool` set. Membership is fixed
at creation; there is no pool table and no other configuration — a pool exists
while some agent carries its name (1-63 of `[a-z0-9-]`, not starting with `-`).

### Creating a pool worker

1. Open the dispatch form (the Agents tab's **+**, or a task's **Dispatch**).
2. Pick **Claude Code** as the agent kind. The **Worker pool** field appears
   under the Claude account; it is not shown for any other kind.
3. Enter the pool's name, for example `reviewers`.
4. Leave the Claude account on **Auto** unless you want to pin one; the account
   pool chooses as it does for any agent. Pick the repository the pool's jobs
   will name (with `repo`), which must be a **public** one (see below).
5. **Dispatch** (or **Save for later** and Start it). A pool job can run only on
   a `running` worker.

The agent's card and page show a **pool** chip, and `GET /api/agents` reports
it as `workerPool`. Repeat with the same name for more workers; each takes its
own account from the pool unless pinned.

The form posts `worker_pool` to `POST /agents` with `kind=claude-code`. With any
other kind, or a name outside the pattern, the server refuses the dispatch with
**409** and the reason, and creates nothing. Neither the `muster` CLI nor the
chief's `task_dispatch` can create an agent in a pool.

**A pool worker holds no GitHub credential.** The one way an agent gets one is
a grant on a connector that reaches github.com — the `github` connector (the
GitHub App's token for the agent's repository, [github-app.md](github-app.md))
or a stored connector with a `github.com` host: approved, it routes all of the
pod's git traffic to github.com through muster's proxy, which adds the
credential. So:

- approving such a grant for a member is **refused**, on the Connectors tab,
  from a mirrored session (`connector_request_decide`), and in the approval
  that comes with entering a credential — including when the request cannot be
  read at that moment. A connector whose hosts muster cannot read counts as
  reaching github.com;
- a member holding such a grant **approved** anyway is not eligible for a job.
  A **pending** one does not count: it can never be approved, and ccd files a
  pending `github` request by itself whenever a clone fails, which would
  otherwise take a worker out of its pool after one failed clone.

An agent joins its pool when it is created, before it can hold any grant, so
with both rules it never holds one. A worker therefore cannot clone a
**private** repository (its clone has no credential, and the access request
ccd files can never be approved): pools serve public repositories, the same
ones a job's anonymous fetch can read.

A job naming a pool runs on the member chosen when it is submitted. A member is
**eligible** when it is a `claude-code` agent whose status is `running`, works
on the job's `repo` (when it names one), holds no approved grant reaching github.com,
runs on a Claude account that is configured, whose current token is not
marked `auth_failed` and that is not cooling down after a `rate_limited`, has
fewer than 4 jobs queued or running, and — when
`MUSTER_AGENT_CC_MAX_CONCURRENT_JOBS` is set (unset is no cap) — would not
take its account past that many jobs at once. Of the
eligible it prefers, in order:

1. a worker with no job queued or running;
2. then the fewest jobs queued;
3. then the account least recently marked `rate_limited` (never, then the
   oldest);
4. then the agent's name, so the choice is deterministic.

Submissions arriving at the same moment are placed one after another: each
choice sees the jobs the ones before it placed, so six submitted at once to two
idle workers are spread over both rather than sent to one until its limit
refuses them.

**The cooldown.** After a turn on an account answers `rate_limited`, the
account rests: no pool job is placed on any worker using it until the reset
time the CLI stated (`resets 9:45pm (UTC)`, `resets Oct 9, 9am (UTC)`), or,
when the message states none muster can read (no zone, for one),
`MUSTER_AGENT_CC_RATE_LIMIT_COOLDOWN` (default `1h`) after the mark. It is
usable again at exactly that time.

**The per-account cap** is opt-in (`MUSTER_AGENT_CC_MAX_CONCURRENT_JOBS`;
unset, there is none). Jobs on one worker run one after another, so the jobs
an account runs at once are its workers with a job queued or running — in any
pool, or none. An idle worker whose account already has
`MUSTER_AGENT_CC_MAX_CONCURRENT_JOBS` such workers is skipped; a busy worker is
not, because a job placed on it queues behind the one it runs. A job naming an
agent directly is not checked against the cap or the cooldown: naming the
agent is the caller's override.

The job records the pool (`worker_pool`) and the worker (`agent`,
`cc_account`). The choice is made once; a job is not moved to another worker
later, and one worker still runs its jobs one at a time.

## Reading a job

```
GET /api/jobs/{id}
```

```json
{
  "job_id": 17,
  "status": "succeeded",
  "result": "…",
  "agent": "keen-wren",
  "worker_pool": "reviewers",
  "cc_account": "alpha",
  "started_at": "…",
  "finished_at": "…",
  "created_at": "…",
  "repo": "", "sha": "", "result_path": "", "timeout_s": 1800,
  "model": "", "config_ref": "",
  "session_id": 412
}
```

`worker_pool` is `""` for a job that named its agent.

| `status` | meaning |
|---|---|
| `queued` | accepted; waiting for the agent (one job runs per agent at a time) |
| `running` | the agent's turn is in progress |
| `succeeded` | `result` holds the outcome, verbatim |
| `failed` | `reason` and `error` say why |
| `timed_out` | the job's `timeout_s` elapsed (or ccd's own turn budget did); `reason`, `error` |

`result` is present only on `succeeded`, and `reason` only on `failed` and
`timed_out`. `error` is on every `failed` and `timed_out` job; on a
`succeeded` one it appears only as a note that does not change the outcome
(a result file or a worktree that could not be removed, below).

**The result** is the agent's final reply text. With `result_path`, it is
instead that file's contents, read after the turn, byte for byte — from the
job's worktree for a `sha` job, from the agent's working directory otherwise.
A file over 1 MiB, or one that is not UTF-8 text, fails the job
(`result_read_failed`) rather than being cut or altered. `result_path` may not
contain a `.git` element (400).

**A `sha` job's worktree is removed after the job**, however it ended —
succeeded, failed, timed out, or muster failing around it — after its result
was read. A removal that fails is noted in `error`
(`could not remove the job's worktree: …`) without changing the status.

**For a job without a `sha`, the result file is deleted after a successful
read, if the repository does not track it.** Left in the agent's working
directory, it would still be there for the next job naming the same
`result_path`, whose turn could then return it as its own if it wrote none;
after a successful read its content is in `result`, byte for byte, so nothing is
lost. It is **kept** in two cases:

- **The read failed** (`result_read_failed` or `timed_out`): the file is then
  the only copy of the agent's output.
- **The repository tracks it** (a `result_path` such as `CHANGELOG.md`):
  deleting it would itself dirty the tree. The job succeeds, and `error` notes
  `the result file <path> is tracked by the repository, so it was left in place, as the turn left it, in the agent's checkout`.

If the delete itself fails — including when git cannot say whether the file is
tracked (git missing, or it refuses the repository), in which case the file is
kept — the job keeps its status and `error` says
`could not delete the result file <path> …`.

**`reason`** is a stable token a caller can branch on:

| `reason` | |
|---|---|
| `rate_limited`, `auth_failed` | ccd's typed failure for the Claude account the agent runs on (`cc_account`). Re-queue on an agent with another account — a pool job submitted again is not given a worker whose account is marked `auth_failed` or is still cooling down after a `rate_limited`, and prefers the account rate-limited longest ago. |
| other ccd types | `busy`, `capacity`, `not_ready`, `empty_reply`, `turn_failed`, … — ccd's vocabulary, passed through unchanged |
| `timeout` | the job's `timeout_s` elapsed (status `timed_out`) |
| `turn_timeout` | ccd's own turn budget elapsed (status `timed_out`) |
| `checkout_failed` | the job's worktree could not be made: the pull request's head could not be fetched anonymously (a private repository, no such pull request, no network), the `sha` is neither that head nor an ancestor of it, the checkout's own git configuration rewrites the fetch URL or adds a header or credential to it, the default branch (`origin/HEAD`) could not be resolved, the `config_ref` is not on the default branch (or the default branch could not be fetched to look for it), the default branch has a symlink among the overlaid paths (it could point at a file the commit controls), or git failed; `error` says which |
| `workdir_unconfirmed` | a `sha` job's turn succeeded, but the agent's runtime did not confirm it ran in the job's worktree (its image predates job worktrees); the reply was discarded — update the agent's image |
| `result_read_failed` | the turn succeeded but `result_path` could not be returned |
| `empty_reply` | the turn produced no reply text and no `result_path` was asked for |
| `agent_unavailable` | the agent could not be loaded when the job was due to start |
| `interrupted` | nothing finished the job by its deadline, so the reaper failed it: the server running it stopped, crashed or could not record the outcome (see [restarts](#what-it-does-not-do)) |
| `internal` | muster failed around the turn — including a `sha` job, or a job with a `model`, on a deployment whose chat store cannot open a Claude Code session of the job's own, which is refused rather than run in the agent's own session |

## How a job runs

1. The job waits for its agent: jobs on one agent run one at a time.
2. A new chat session is opened on the agent, on the `claude-code` runtime
   and with the job's `model` if it has one, so ccd runs the job in a Claude
   Code CLI of its own (a fresh conversation, started with `--model` when there
   is a model) rather than the agent's main one. The prompt and the reply are stored in
   it, so the job is readable on the agent's page (`session_id`).
3. With `sha`, through the provisioner's exec in the agent's instance:
   - `refs/remotes/origin/HEAD` is resolved (asking `origin` once if the ref is
     missing; the job fails `checkout_failed` if it still cannot be);
   - pull request `pr`'s head is fetched anonymously from
     `https://github.com/<repo>` (`+refs/pull/<pr>/head`, into a ref of the
     job's own) with git's global and system configuration ignored — where the
     proxy routing of an agent with a GitHub grant lives — and no credential
     helper or extra header; the job fails `checkout_failed` if the checkout's
     own configuration would still rewrite that URL or add a header or
     credential, or if the `sha` is neither the fetched head nor an ancestor
     of it. `repo` is the agent's repository when the request names none;
   - with `config_ref`, the commit is checked to be on the default branch
     (fetching the default branch anonymously when the checkout's
     `origin/HEAD` does not reach it), and becomes the overlay's source;
   - `git worktree add --detach` creates the job's worktree at
     `<the agent's workspace>/.muster-jobs/<job_id>` — beside the agent's
     checkout, on its volume — and the overlay above is applied to it.

   The agent's checkout, its branch, HEAD and working tree are not touched: an
   agent with uncommitted work is not refused and not changed.
4. The prompt is sent as one turn — the same synchronous call `chief ask` makes
   to ccd's `/v1/responses`. For a `sha` job the turn names the worktree, and
   ccd starts the session's CLI there (it accepts only a job worktree of that
   agent's workspace). The CLI treats a git worktree of the checkout it already
   trusts as trusted, so ccd writes no trust of its own for it. The turn is done
   when ccd answers.
5. With `result_path`, the file is read back through the provisioner's exec.
6. A `sha` job's worktree is removed (`git worktree remove --force`, the
   directory if git left it, `git worktree prune`), and its fetched
   pull-request and default-branch refs deleted. For a job without a
   `sha`, the result file is deleted as above.

The job's `timeout_s` covers steps 3–5, not the wait in step 1; the removal in
step 6 has its own bound.

## What it does not do

- **No isolation from the commit's code, and so no third-party commits.** See
  the note at the top.
- **One job at a time per worker.** Each job has its own worktree and its own
  CLI, but one worker's turns share its Claude account and its pod's memory,
  and running several at once on one worker has not been exercised.
- **No survival across a restart.** The turn runs inside the server process,
  and nothing waits for it on shutdown. On a graceful shutdown the server
  fails the jobs it holds itself — queued ones ("nothing was sent to the
  agent") and running ones ("the agent may have done some of the work") —
  with `interrupted`, within 5 seconds; it never touches another server's
  jobs. A job a crashed server leaves unfinished is
  failed `interrupted` by the reaper, which runs at boot and every minute and
  fails a job only once it is past its own deadline: a `running` job at
  `started_at` + `timeout_s` + 5 minutes, a `queued` one at `created_at` + 2h40m
  (the longest a queued job can wait: 4 jobs × (2100 s + 5 minutes)). So a job
  can read `running` for up to that long after its server died, and a job the
  other server of a rolling update is still running is never failed by the new
  one. The same pass ends a job orphaned by a crash in muster or by an outcome
  it could not record (the write is retried 3 times first). The agent may have
  done some of the work, so re-submitting is the caller's call. A server that
  stops or crashes mid-job also leaves that job's worktree on the agent's
  volume (`.muster-jobs/<job_id>`); nothing removes it later.
- **At most 4 jobs queued or running per agent.** A fifth submission naming
  the agent is a 429; a pool skips a worker at the limit, and is a 429 when
  every worker it could otherwise use is at the limit. A repeated
  `idempotency_key` is still answered. A job orphaned by a CRASHED server
  counts toward the 4 until the reaper fails it (a graceful shutdown fails its
  own jobs at once).
- **A pool is chosen from at submission, not re-balanced.** The marks it reads
  are the account pool's: `auth_failed` under the account's current token, and
  the latest `rate_limited`, whose cooldown is judged at submission: a job
  already queued on a worker whose account is then rate-limited is not moved.
- **`result_path` is checked as a string, not resolved.** No `..`, no absolute
  path — but a symlink in the worktree (or the working directory) is followed,
  so the file read can be outside it.
- **A job without a `sha` runs in the agent's own working directory.** Only
  its successfully read, untracked `result_path` file is cleaned up; anything
  else the turn leaves there stays.
- **A timeout does not stop the agent's turn.** It aborts muster's request to
  ccd and ends the job `timed_out`, but the CLI's turn may run on — in a
  worktree that has since been removed — and the next job on that agent can be
  refused `busy`.
- **Not verified end to end against a live muster and cluster.** The handler
  tests run the runner against fakes of the gateway, the chat store and the
  workspace, and against the real adapter's scripts and real git on the test
  host; the real CLI and ccd are exercised together in the image (CI's image
  job), driven directly rather than through a running muster.
