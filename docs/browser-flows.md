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

## Three ways in

| door | who | routes |
|---|---|---|
| The **page** | you, signed in | `/flows`: enqueue and cancel jobs, mint and revoke runner tokens, approve agents' requests |
| The **API** | your personal `mst_` token | `POST /api/flows/jobs`, `GET /api/flows/jobs`, `GET /api/flows/jobs/{id}`. The shared hook and service secrets are refused. The CLI's `muster flow enqueue \| ls \| status` uses these. |
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
