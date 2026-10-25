# opencode: what muster can rely on from its server

Notes for muster's opencode runtime (the stub in `internal/mirror/runtime.go`
and `internal/mirrorhost/runtime.go` today; a read-only view and an API send
next). Facts are read from upstream source, <https://github.com/anomalyco/opencode>,
at commit `6ec5682` (opencode v2.0.32+6ec5682). They are not measured against
a running server.

## Credentials: can muster hold a dedicated, revocable token?

**Dedicated and terminal-free, yes. Individually revocable or scoped, no.**

### How a token is issued

Issuing a token takes two HTTP calls. This is what `opencode pair` does
(`packages/cli/src/commands/handlers/pair.ts`), and nothing needs a terminal.

1. **`POST /api/pair`** returns `{code, expires_in}`.
   - The code is single-use: 16 random bytes, base64url, held in the server's
     memory with a 5-minute TTL (`packages/server/src/pairing.ts`).
   - The route is under `/api`, so it needs the server's credential: the HTTP
     Basic password (user `opencode`), or an existing token.
2. **`GET /auth/connect/{code}`** redeems the code
   (`packages/server/src/handlers/server.ts`).
   - It is unauthenticated (`security: []` in `packages/protocol/openapi.json`).
     The code is the credential.
   - With `Accept: application/json` it returns `{token}`.
   - A browser navigation (an `Accept` with `text/html`) gets a cookie,
     `opencode_session_<port>`, instead.

### What the token is (`packages/server/src/auth.ts`)

- It is `"<expires>.<sig>"`. The signature is base64url
  HMAC-SHA256(key, expires), with `key = HMAC-SHA256(password,
  "opencode-session-v1")`.
- It lives 30 days (`SESSION_TTL_SECONDS`).
- It is **accepted anywhere the password is** ("Session tokens issued by pairing
  links are accepted anywhere the password is"). It goes in HTTP Basic as the
  password, with user `opencode`.

### What the token cannot do

- **No scope.** A token reaches the whole API: every session's prompt,
  interrupt, permission and form replies.
- **No per-token revocation.** Tokens are stateless; the server keeps no list.
  The only way to revoke one is to rotate the server password, which revokes
  every token at once ("rotating the password revokes every session"). That
  also disconnects every other paired client.
- **Self-renewal.** A holder of a valid token can call `POST /api/pair` and
  redeem a fresh 30-day token, indefinitely. So a client can renew itself
  without a terminal, but so can a leaked token, until the password is rotated.

### Consequences for muster's API phase

- Muster can hold a credential of its own, distinct from the service password.
  It belongs in a mode-600 file referenced by path, never in this repository.
- "Revoke muster's access" means "rotate the opencode service password". Say
  so in the operator docs; there is no narrower switch.
- Renew before the 30 days run out, and alarm when renewal fails. A token that
  silently expires is an unknown status, and unknown fails closed.
- Per-client revocation or a scoped token would need an upstream change. None
  exists at `6ec5682`.
