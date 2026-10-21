# The machine job API

An external orchestrator submits a one-shot **job** — run this prompt on this
agent, optionally at this commit, and give me the result — and polls for its
outcome. A job runs as one turn of an existing, running `claude-code` agent: one
it names, or one muster chooses from a named **worker pool**.

> **What a `sha` job is isolated from, and what it is not.** A job with a `sha`
> runs in a git worktree of its own at that commit, outside the agent's
> checkout, in a fresh Claude Code session whose CLI starts in that worktree.
> Before the turn, the commit's own agent configuration is replaced by the
> repository's default branch's (`origin/HEAD`):
>
> - every `.claude/` directory, at any depth — project settings (hooks:
>   commands the CLI runs), agents and skills, which the CLI also discovers in
>   subdirectories;
> - the root `CLAUDE.md` and `AGENTS.md` — the instructions the CLI loads at
>   launch;
> - the root `.mcp.json` — MCP servers, whose commands the CLI starts.
>
> Each is removed, then the default branch's copy restored where it has one.
> So the commit cannot hook, configure or add MCP servers to its own reviewer:
> with the real CLI, a commit whose `.claude/settings.json` defines a
> SessionStart hook and whose `.mcp.json` defines a server runs neither in its
> job, and runs both without the overlay
> ([`realcli_test.go`](../internal/agentprovision/realcli_test.go), in CI's
> image job).
>
> **Not isolated:** the turn runs in the worker's pod, with the worker's Claude
> account, its network (public HTTPS) and its volume. Every other file in the
> commit is the commit's — `CLAUDE.md` / `AGENTS.md` files below the root
> (instructions the CLI reads as it works there), build scripts, tests — and the
> turn may run them if the prompt leads it to. The overlay stops the commit
> from configuring its reviewer; it does not make the commit's code safe to
> execute. Point a worker at commits you would let run in that pod.

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
| 400 | a missing or malformed field — including both or neither of `agent` and `worker_pool`, a `worker_pool` with a `sha` but no `repo`, and an abbreviated `sha`: only a full 40-character id is accepted, because `git fetch origin <sha>` cannot fetch an abbreviation |
| 404 | no agent by that name; no agent is a member of that `worker_pool` |
| 409 | the agent is not `claude-code`; `repo` is not the agent's repository; `sha` on an agent with no repository |
| 429 | the agent already has 4 jobs queued or running (the per-agent limit); submit again once one finishes |
| 501 | `sha` / `result_path` on a deployment with no provisioner |
| 503 | no database, no agent gateway, or the service tier is unarmed; or, for a `worker_pool`, no member can take the job (`error` says why for each) or the deployment cannot read its Claude accounts' marks |

**Idempotency.** A second submission with an `idempotency_key` already used
records nothing and answers the first job, with **200** and
`"deduplicated": true` — before any agent is looked up or worker chosen, so a
retry is answered even when its pool now has no eligible worker. The key is not
compared with the rest of the body.

## Worker pools

A worker pool is a name carried by long-lived `claude-code` agents. An agent
becomes a member when it is created with `worker_pool` set (the dispatch form's
`worker_pool` field on `POST /agents`, with `kind=claude-code`; any other kind
is refused). Membership is fixed at creation; there is no pool table and no
other configuration — a pool exists while some agent carries its name (1-63 of
`[a-z0-9-]`, not starting with `-`).

A job naming a pool runs on the member chosen when it is submitted. A member is
**eligible** when it is a `claude-code` agent whose status is `running`, works
on the job's `repo` (when it names one), runs on a Claude account that is
configured and whose current token is not marked `auth_failed`, and has fewer
than 4 jobs queued or running. Of the eligible it prefers, in order:

1. a worker with no job queued or running;
2. then the fewest jobs queued;
3. then the account least recently marked `rate_limited` (never, then the
   oldest);
4. then the agent's name, so the choice is deterministic.

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
| `rate_limited`, `auth_failed` | ccd's typed failure for the Claude account the agent runs on (`cc_account`). Re-queue on an agent with another account — a pool job submitted again is not given a worker whose account is marked `auth_failed`, and prefers the account rate-limited longest ago. |
| other ccd types | `busy`, `capacity`, `not_ready`, `empty_reply`, `turn_failed`, … — ccd's vocabulary, passed through unchanged |
| `timeout` | the job's `timeout_s` elapsed (status `timed_out`) |
| `turn_timeout` | ccd's own turn budget elapsed (status `timed_out`) |
| `checkout_failed` | the job's worktree could not be made: the commit could not be fetched, the default branch (`origin/HEAD`) could not be resolved, the default branch has a symlink among the overlaid paths (it could point at a file the commit controls), or git failed; `error` says which |
| `result_read_failed` | the turn succeeded but `result_path` could not be returned |
| `empty_reply` | the turn produced no reply text and no `result_path` was asked for |
| `agent_unavailable` | the agent could not be loaded when the job was due to start |
| `interrupted` | nothing finished the job by its deadline, so the reaper failed it: the server running it stopped, crashed or could not record the outcome (see [restarts](#what-it-does-not-do)) |
| `internal` | muster failed around the turn — including a `sha` job on a deployment whose chat store cannot open a Claude Code session of the job's own, which is refused rather than run in the agent's own session |

## How a job runs

1. The job waits for its agent: jobs on one agent run one at a time.
2. A new chat session is opened on the agent, on the `claude-code` runtime, so
   ccd runs the job in a Claude Code CLI of its own (a fresh conversation)
   rather than the agent's main one. The prompt and the reply are stored in
   it, so the job is readable on the agent's page (`session_id`).
3. With `sha`, through the provisioner's exec in the agent's instance:
   - `refs/remotes/origin/HEAD` is resolved (asking `origin` once if the ref is
     missing; the job fails `checkout_failed` if it still cannot be);
   - the commit is fetched from `origin` if it is not already present;
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
   directory if git left it, `git worktree prune`). For a job without a
   `sha`, the result file is deleted as above.

The job's `timeout_s` covers steps 3–5, not the wait in step 1; the removal in
step 6 has its own bound.

## What it does not do

- **No isolation beyond the overlaid paths.** See the note at the top.
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
  the agent is a 429; a pool skips a worker at the limit. A repeated
  `idempotency_key` is still answered. A job orphaned by a CRASHED server
  counts toward the 4 until the reaper fails it (a graceful shutdown fails its
  own jobs at once).
- **A pool is chosen from at submission, not re-balanced.** The marks it reads
  are the account pool's: `auth_failed` under the account's current token, and
  the latest `rate_limited`. There is no cooldown window: an account
  rate-limited a minute ago is only ranked after one rate-limited earlier.
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
