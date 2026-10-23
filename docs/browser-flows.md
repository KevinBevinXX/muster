# Browser flows

A **browser flow** is a named, scripted job that runs in a real browser that you
host, such as reading or posting on a site you are signed into. muster keeps the
queue. A **runner** that you operate claims jobs and reports their results.

- The catalogue of flows is compiled into muster (`internal/flows`).
- The store is on whenever there is a database.
- **This repository contains no runner.** The protocol is below, and the e2e
  suite drives it with a plain HTTP client.

A job moves through these states: `queued` → `running` (held for 2 minutes and
extended by heartbeats) → `done`, `failed` or `needs_operator`.

A `queued` job that is not claimed before its ttl (`--ttl`, default 15 minutes)
becomes `expired`. Only one job runs at a time on each of your browser
instances (owner, host, instance), so a job can wait behind a running one.
**That wait does not count against its ttl, up to a ceiling per blocking
job.** The job's `expiresAt` stays the deadline as enqueued.

The ceiling is `MaxTTLPause` = **90 minutes for each job it waits behind**,
not in total. It is sized from the longest legitimate hold of one instance, a
muse dispatch: the 10-minute pacing gap, plus `wait` up to 3600 s, plus 240 s
of runner slack, is 74 minutes, and the rest is headroom. Exactly:

- While blocked, a job's deadline is `expiresAt` + the credit it already has
  + the time the job blocking it has held it, counted up to 90 minutes.
- When that job finishes (or its lease runs out), the time it blocked (up to
  90 minutes) is added to the credit of every job still queued behind it.

So a job behind two long dispatches survives both. A blocked job expires only
if its own ttl had already run out before the block began, or if the job
blocking it has run longer than 90 minutes (a runner can keep heartbeating a
wedged flow forever). In that case the queue behind a wedged job clears within
90 minutes of the block starting, plus each job's remaining ttl, and an
owner's pending cap (20) always clears. The expiry detail says which of the
two happened.

Cancelling (`POST /api/flows/jobs/{id}/cancel`, or the button on `/flows`)
withdraws a `queued` job and dismisses a `needs_operator` one. A `running` job
answers 409: it is the runner's until it reports, so stop the runner to stop it
now.

## The catalogue

| flow | class | parameters |
|---|---|---|
| `muse.poll` | read | `thread` (a side chat's title), `task` |
| `muse.send` | write | `text` (required), `thread` **or** `session`, `marker`, `wait`, `task` |

- `session` is a Muse thread's uuid, lowercase. It opens that thread directly,
  and it cannot be combined with `thread`.
- `session`, `marker` and `wait` are screened like any agent-composed text when
  an agent files a request (only `task` is exempt), so an agent cannot ask for
  a dispatch; the dispatch client enqueues with a personal token.
- `marker` (`md-` and 6 hex digits) needs `session`. The send completes only
  when the reply's last line is `DONE <marker>`.
- `wait` is how many seconds the flow waits for the reply, from 1 to 3600. The
  runner gives the job `wait` + 240 seconds before it times out.

A dispatch job can stay `running` for a long time and still be healthy: up to
about 10 minutes waiting out Muse's pacing gap, then `wait`, then 240 seconds.
Nothing caps how long a job may run (the jobs queued behind it still expire once it has held them past the 90-minute per-blocker ceiling above). A job is only taken from its runner when
the runner stops heartbeating and the 2-minute lease runs out.

## Three ways in

| door | who | routes |
|---|---|---|
| The **page** | you, signed in | `/flows`: enqueue and cancel jobs, mint and revoke runner tokens, approve agents' requests |
| The **API** | your personal `mst_` token | `POST /api/flows/jobs`, `GET /api/flows/jobs`, `GET /api/flows/jobs/{id}`, `POST /api/flows/jobs/{id}/cancel`. The shared hook and service secrets are refused. The CLI's `muster flow enqueue \| ls \| status \| cancel` uses these. |
| The **runner** | an `mfr_` runner token | `POST /runner/flows/claim` (a 25-second long-poll, which answers 204 when there is nothing to do), `POST /runner/flows/{id}/heartbeat`, `POST /runner/flows/{id}/result` |

A runner token is minted on `/flows` for one owner and one host. It acts as its
owner and is confined to that host. The runner sends it as
`Authorization: Bearer …` or as `X-Muster-Token`.

## Agents ask, owners approve

An agent cannot enqueue a job. Through its tools (also exposed over MCP), it can
call:

- `browser_flow_request`, which files a **request**, never a job;
- `browser_flow_status`.

The request's owner approves or denies it on `/flows` (`POST /flows/requests/{id}/approve`
or `/deny`). A request expires after one hour, and nothing approves one
automatically.

Text an agent composes for a flow is screened before it is filed. Generic
patterns (addresses, host names, token shapes) are always refused. So is every
word in `MUSTER_FLOW_AGENT_DENY_WORDS`: a comma-separated list of your
deployment's own host, cluster and project names. That list is configuration,
not source, because this repository is public. While it is unset, the server
logs a warning at boot.
