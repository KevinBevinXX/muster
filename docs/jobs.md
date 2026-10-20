# The machine job API

An external orchestrator submits a one-shot **job** — run this prompt on this
agent, optionally at this commit, and give me the result — and polls for its
outcome. A job runs as one turn of an existing, running `claude-code` agent.

This is a first cut. It targets one named agent; choosing an agent from a pool
is not implemented yet (see [What it does not do](#what-it-does-not-do)).

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
  "sha": "0123abc",                // optional: 7-40 lowercase hex
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
| 400 | a missing or malformed field |
| 404 | no agent by that name |
| 409 | the agent is not `claude-code`; `repo` is not the agent's repository; `sha` on an agent with no repository |
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

`result` is present only on `succeeded`, and `reason` / `error` only on
`failed` and `timed_out`.

**The result** is the agent's final reply text. With `result_path`, it is
instead that file's contents, read from the agent's workspace after the turn,
byte for byte. A file over 1 MiB, or one that is not UTF-8 text, fails the job
(`result_read_failed`) rather than being cut or altered.

**`reason`** is a stable token a caller can branch on:

| `reason` | |
|---|---|
| `rate_limited`, `auth_failed` | ccd's typed failure for the Claude account the agent runs on (`cc_account`). Re-queue on an agent with another account. |
| other ccd types | `busy`, `capacity`, `not_ready`, `empty_reply`, `turn_failed`, … — ccd's vocabulary, passed through unchanged |
| `timeout` | the job's `timeout_s` elapsed (status `timed_out`) |
| `turn_timeout` | ccd's own turn budget elapsed (status `timed_out`) |
| `checkout_failed` | moving the agent's checkout to `sha` failed |
| `result_read_failed` | the turn succeeded but `result_path` could not be returned |
| `empty_reply` | the turn produced no reply text and no `result_path` was asked for |
| `agent_unavailable` | the agent could not be loaded when the job was due to start |
| `interrupted` | the server stopped while the job was queued or running |
| `internal` | muster failed around the turn |

## How a job runs

1. The job waits for its agent: jobs on one agent run one at a time.
2. A new chat session is opened on the agent, on the `claude-code` runtime, so
   ccd runs the job in a Claude Code session of its own rather than the
   agent's main conversation. The prompt and the reply are stored in it, so
   the job is readable on the agent's page (`session_id`).
3. With `sha`, the agent's checkout is moved to that commit (detached), after
   fetching it from `origin` if it is not already present. A checkout with
   local changes the move would overwrite fails the job; nothing is discarded.
4. The prompt is sent as one turn — the same synchronous call `chief ask` makes
   to ccd's `/v1/responses`. The turn is done when ccd answers.
5. With `result_path`, the file is read back through the provisioner's exec.

The job's `timeout_s` covers steps 3–5, not the wait in step 1.

## What it does not do

- **No worker pools.** A job names one agent.
- **No survival across a restart.** The turn runs inside the server process. At
  boot, every job left `queued` or `running` is failed with `interrupted`. The
  agent may have done some of the work, so re-submitting is the caller's call.
- **A timeout does not stop the agent's turn.** It aborts muster's request to
  ccd and ends the job `timed_out`, but the CLI's turn may run on, and the next
  job on that agent can be refused `busy`.
- **A `sha` checkout is left in place.** The agent's checkout stays at that
  commit after the job.
- **Not verified end to end against a live ccd from this tree.** The handler
  tests run the runner against fakes of the gateway, the chat store and the
  workspace; the checkout and file read have not run inside a real agent
  instance.
