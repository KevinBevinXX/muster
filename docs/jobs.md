# The machine job API

An external orchestrator submits a one-shot **job** — run this prompt on this
agent, optionally at this commit, and give me the result — and polls for its
outcome. A job runs as one turn of an existing, running `claude-code` agent.

This is a first cut. It targets one named agent; choosing an agent from a pool
is not implemented yet (see [What it does not do](#what-it-does-not-do)).

> **Not safe for untrusted commits yet.** A job with a `sha` checks that commit
> out in the agent's own working tree, so nothing isolates the turn from the
> `.claude/` (agent config, hooks) that commit carries. Whether a given turn
> picks it up depends on when the CLI reads it, which has not been checked, so
> assume a commit under review can change its own reviewer. A per-job worktree with `.claude/`
> taken from the default branch comes with worker pools; do not point an
> orchestrator at commits you do not trust until then.

## Auth

Both routes are on the service tier: `MUSTER_SERVICE_TOKEN` as
`Authorization: Bearer …` (or `X-Muster-Token`), plus an `X-Muster-Actor`
header naming the caller ([auth.md](auth.md#machine-credentials)). The actor is
recorded on the job as who requested it.

- No token, or a wrong one: **401**.
- `MUSTER_SERVICE_TOKEN` unset on the server: **503** with `"unarmed": true`.
  The tier fails closed.
- The tier acts as user 1, so a job can name only user 1's agents.

## Submitting a job

```
POST /api/jobs
{
  "agent": "keen-wren",            // required: an existing claude-code agent
  "prompt": "Review the change …", // required, at most 64 KiB
  "repo": "owner/name",            // optional: must be the agent's repository
  "sha": "0123456789abcdef0123456789abcdef01234567", // optional: a full 40-hex commit id
  "result_path": "out/review.md",  // optional: relative to the agent's working directory
  "timeout_s": 1800,               // optional: 1-2100, default 1800
  "idempotency_key": "review-42"   // optional, at most 128 bytes
}
```

**202** `{"job_id": 17, "status": "queued", …}`, the same shape as a read.

Unknown fields are refused, so a misspelt `result_path` is a 400 rather than a
job that returns the wrong thing.

| answer | when |
|---|---|
| 400 | a missing or malformed field — including an abbreviated `sha`: only a full 40-character id is accepted, because `git fetch origin <sha>` cannot fetch an abbreviation |
| 404 | no agent by that name |
| 409 | the agent is not `claude-code`; `repo` is not the agent's repository; `sha` on an agent with no repository |
| 429 | the agent already has 4 jobs queued or running (the per-agent limit); submit again once one finishes |
| 501 | `worker_pool` was given; or `sha` / `result_path` on a deployment with no provisioner |
| 503 | no database, no agent gateway, or the service tier is unarmed |

**Idempotency.** A second submission with an `idempotency_key` already used
records nothing and answers the first job, with **200** and
`"deduplicated": true`. The key is not compared with the rest of the body.

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
  "cc_account": "alpha",
  "started_at": "…",
  "finished_at": "…",
  "created_at": "…",
  "repo": "", "sha": "", "result_path": "", "timeout_s": 1800,
  "session_id": 412
}
```

| `status` | meaning |
|---|---|
| `queued` | accepted; waiting for the agent (one job runs per agent at a time) |
| `running` | the agent's turn is in progress |
| `succeeded` | `result` holds the outcome, verbatim |
| `failed` | `reason` and `error` say why |
| `timed_out` | the job's `timeout_s` elapsed (or ccd's own turn budget did); `reason`, `error` |

`result` is present only on `succeeded`, and `reason` only on `failed` and
`timed_out`. `error` is on every `failed` and `timed_out` job; on a
`succeeded` one it appears only as a note that does not change the outcome,
when the job's result file was left in place (see below).

**The result** is the agent's final reply text. With `result_path`, it is
instead that file's contents, read from the agent's workspace after the turn,
byte for byte. A file over 1 MiB, or one that is not UTF-8 text, fails the job
(`result_read_failed`) rather than being cut or altered. `result_path` may not
contain a `.git` element (400).

**The result file is deleted after a successful read, if the repository does
not track it.** Left in the agent's checkout, the job's own untracked output
would make the next `sha` job's checkout refuse the tree as dirty; after a
successful read its content is in `result`, byte for byte, so nothing is lost.
It is **kept** in two cases:

- **The read failed** (`result_read_failed` or `timed_out`): the file is then
  the only copy of the agent's output. It stays, and the next `sha` job on
  that agent fails `checkout_failed` until it is removed.
- **The repository tracks it** (a `result_path` such as `CHANGELOG.md`):
  deleting it would itself dirty the tree. The job succeeds, and `error` notes
  `the result file <path> is tracked by the repository, so it was left in place; …`.
  **If the turn modified that tracked file, keeping it leaves the tree dirty**,
  and the next `sha` job on that agent fails `checkout_failed` until the change
  is reverted; the note says so.

If the delete itself fails — including when git cannot say whether the file is
tracked (git missing, or it refuses the repository), in which case the file is
kept — the job keeps its status and `error` says
`could not delete the result file <path> …`.

**`reason`** is a stable token a caller can branch on:

| `reason` | |
|---|---|
| `rate_limited`, `auth_failed` | ccd's typed failure for the Claude account the agent runs on (`cc_account`). Re-queue on an agent with another account. |
| other ccd types | `busy`, `capacity`, `not_ready`, `empty_reply`, `turn_failed`, … — ccd's vocabulary, passed through unchanged |
| `timeout` | the job's `timeout_s` elapsed (status `timed_out`) |
| `turn_timeout` | ccd's own turn budget elapsed (status `timed_out`) |
| `checkout_failed` | moving the agent's checkout to `sha` failed — including a dirty working tree, which `error` says |
| `result_read_failed` | the turn succeeded but `result_path` could not be returned |
| `empty_reply` | the turn produced no reply text and no `result_path` was asked for |
| `agent_unavailable` | the agent could not be loaded when the job was due to start |
| `interrupted` | nothing finished the job by its deadline, so the reaper failed it: the server running it stopped, crashed or could not record the outcome (see [restarts](#what-it-does-not-do)) |
| `internal` | muster failed around the turn |

## How a job runs

1. The job waits for its agent: jobs on one agent run one at a time.
2. A new chat session is opened on the agent, on the `claude-code` runtime, so
   ccd runs the job in a Claude Code session of its own rather than the
   agent's main conversation. The prompt and the reply are stored in it, so
   the job is readable on the agent's page (`session_id`).
3. With `sha`, the agent's checkout is moved to that commit, after fetching it
   from `origin` if it is not already present. It is a **detached HEAD**, and
   it stays one after the job. A working tree with any uncommitted change or
   untracked (not ignored) file fails the job `checkout_failed`, with an `error`
   saying the tree is dirty, before anything moves: git would otherwise carry
   those changes onto the job's commit. Nothing is discarded.
4. The prompt is sent as one turn — the same synchronous call `chief ask` makes
   to ccd's `/v1/responses`. The turn is done when ccd answers.
5. With `result_path`, the file is read back through the provisioner's exec,
   then — after a successful read, and only if the repository does not track
   it — deleted the same way.

The job's `timeout_s` covers steps 3–5, not the wait in step 1.

## What it does not do

- **No worker pools.** A job names one agent.
- **No `.claude/` overlay, no per-job worktree.** See the warning at the top:
  a `sha` job is not isolated from that commit's own agent config and hooks.
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
  done some of the work, so re-submitting is the caller's call.
- **At most 4 jobs queued or running per agent.** A fifth submission is a 429.
  A repeated `idempotency_key` is still answered. A job orphaned by a CRASHED
  server counts toward the 4 until the reaper fails it (a graceful shutdown
  fails its own jobs at once).
- **`result_path` is checked as a string, not resolved.** No `..`, no absolute
  path — but a symlink in the workspace is followed, so the file read can be
  outside the working directory.
- **Only the job's own, successfully read, untracked `result_path` file is
  cleaned up.** Anything else the turn leaves in the checkout (another file it
  wrote, an edit), a result file whose read failed, and one written by a turn
  that failed or timed out (it never reaches the read) all stay, and the next
  `sha` job on that agent fails `checkout_failed` until they are removed.
- **A timeout does not stop the agent's turn.** It aborts muster's request to
  ccd and ends the job `timed_out`, but the CLI's turn may run on, and the next
  job on that agent can be refused `busy`.
- **A `sha` checkout is left in place.** The agent's checkout stays at that
  commit, on a detached HEAD, after the job.
- **Not verified end to end against a live ccd from this tree.** The handler
  tests run the runner against fakes of the gateway, the chat store and the
  workspace; the checkout and file read have not run inside a real agent
  instance.
