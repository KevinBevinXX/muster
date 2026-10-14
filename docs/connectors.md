# Connectors

A **connector** is a stored HTTP credential: a bearer token, a header, or basic
auth. Connectors are sealed with AES-256-GCM. An agent never receives the secret.
It calls muster's proxy instead, at `/connect/{connector}/{host}/{path…}`, and
muster injects the credential.

Access is granted **per agent**. You approve it the first time an agent asks. A
grant lasts until the agent is destroyed, the grant is revoked, or the connector
is deleted. Connectors handle HTTP only.

## Configuration

| variable | meaning |
|---|---|
| `MUSTER_CONNECTOR_ENCRYPTION_KEY` | A 32-byte base64 key. Setting it turns the store on. It must differ from the GitHub key. |
| `MUSTER_CONNECT_PORT` | A second listener that serves **only** the agent-facing routes: `/connect/`, `/agent/connectors`, `/mcp`, `/agent/approvals`, `/agent/blocked` and `/health`. Everything else answers 404 there. |
| `MUSTER_AGENT_CONNECT_URL` | The URL agents are given (as `MUSTER_CONNECT_URL`) to reach that listener. It also opens the `claude-code` agents' NetworkPolicy to the connect port. |

While the key is unset, every connector route answers 503, the Connectors tab is
hidden, and members cannot store credentials of their own. The key also seals
members' stored Claude and OpenRouter credentials.

## Routes

- **For agents** (with their own per-agent token):
  - `GET /agent/connectors[/{name}]`
  - `POST /agent/connectors/{name}/request`
  - the proxy at `/connect/…`
  - `POST /mcp`
- **For you:** `/connectors`, plus the request cards at
  `/ui/connector-requests/*`. A pending request also sends a `connector` push
  notification ([push.md](push.md)).
