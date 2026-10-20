# Accounts, sign-in and tokens

## Two modes

`MUSTER_AUTH_MODE` selects one. Any other value refuses to boot.

### `password` (the default)

- One shared operator password, `MUSTER_UI_PASSWORD`, acting as **user 1**, the
  admin.
- If it is unset or shorter than 12 characters, every browser page answers 503
  and `/login` says which variable is missing. It fails closed.
- `POST /login` compares in constant time and is limited to 10 attempts a
  minute.
- The session cookie lasts 30 days. Set `MUSTER_SECURE_COOKIES=1` when muster
  is served over TLS.

### `gotrue`

Per-user accounts through [GoTrue](https://github.com/supabase/auth), the
Supabase auth server.

- **Required:** `MUSTER_GOTRUE_URL`, `MUSTER_GOTRUE_JWT_SECRET` (HS256, at least
  32 characters), `MUSTER_HOOK_TOKEN` and `MUSTER_BROWSER_PORT`.
- **Tokens GoTrue must issue:** issuer `muster-auth`, audience `authenticated`.
- **The browser never talks to GoTrue.** muster calls it server-side and keeps
  the session in `HttpOnly`, `Secure` cookies, which it refreshes itself.
- **Two listeners.** `MUSTER_BROWSER_PORT` is the only listener that serves
  browser sessions. The main port then serves machine tokens only. On the browser
  listener, the shared hook and service secrets are refused with 403.
- **Throttling** is per account: 3 free failures, then a backoff that doubles up
  to 5 minutes. There is also a limit per client address.

### Setting up the first admin

With the GoTrue variables set, `muster-server invite --claim-admin` prints a
single-use link, valid for 24 hours. Opening it binds a new GoTrue login to
user 1.

The GoTrue pair also works in password mode, where it only makes that claim link
usable. That lets you bind the operator's login before you switch modes.

## Users, roles and invites

- **Roles:** there are two, `admin` and `member`.
- **Invites:** an invite always creates a member. Links last 24, 72 (the
  default) or 168 hours, and carry their token in the URL fragment.
  Invites are managed on `/admin/users`.
- **Admin pages:** promote a member, disable or enable a user, send a reset
  link, delete a user's passkeys. The last admin cannot be demoted.
- **From the command line:** `muster-server user disable|enable <id>` does the
  same as the admin page. A running server notices within 10 seconds for open
  streams and within 30 seconds for new requests.
- **Data is per user.** Every user-data row has an `owner_id`, enforced by
  Postgres row-level security (see [schema.md](schema.md)). Each user has their
  own tasks, agents, tokens and chief. An admin's ordinary pages show only the
  admin's own rows; the cross-user views are on `/admin/*`.
- **Credentials:** members bring their own Claude and OpenRouter credentials
  (`/account/credentials`, which needs the connector key). The pool configured in
  the server's environment belongs to the admin.

## Passkeys

Passkeys are GoTrue's own WebAuthn passkeys, proxied through muster. They are
offered only in gotrue mode, and only when GoTrue reports `passkeys_enabled`.

- muster refuses any assertion without user verification.
- A passkey is an **additional** way in. The password stays mandatory, and a
  password reset removes all of the user's passkeys.
- You manage your passkeys at **Account → Passkeys** (`/account/passkeys`).
  Adding one needs a sign-in from the last five minutes.
- Signing in with a passkey needs no username.
- Passkeys can also confirm a step-up; see
  [sending-and-step-up.md](sending-and-step-up.md).

## Machine credentials

| credential | acts as | notes |
|---|---|---|
| Personal API token (`mst_…`) | you: your user id and role | Minted only in the browser at `/account/tokens`, and shown once. Only its sha256 is stored. A token cannot mint another token. Revoking one refuses it on the next request. Disabling a user revokes all of their tokens, and re-enabling them brings none back. |
| `MUSTER_HOOK_TOKEN` | user 1, admin | A shared secret, enforced only when set; while unset, the hook routes are open. gotrue mode requires it. |
| `MUSTER_SERVICE_TOKEN` | user 1 | It also requires an `X-Muster-Actor` header. It fails closed (503 while unset) and must be at least 32 characters. |
| Mirror token (`mmt_…`) | the host it was minted for | Opens `GET /mirror/connect`, the `AskUserQuestion` hook's `/mirror/ask` routes and `POST /mirror/mcp`. Through `/mirror/mcp` a leaked one reaches, beyond the host's tunnel and its questions, the attention asks of that host's own Claude Code sessions (file, wait, cancel), the tasks those sessions created, and — through `attention_answer` — an answer as the operator to any of its owner's open asks not raised by a mirrored session on that host (recorded as via MCP from that host; accepted like any of your answers, **including as `task_dispatch` instructions** — the accepted risk in [attention.md](attention.md#mirrored-claude-code-sessions)) — and, through `connector_request_decide`, an approval as the operator of any of its owner's agents' pending connector requests (recorded and shown as via MCP from that host; the accepted risk in [connectors.md](connectors.md#deciding-a-request-from-your-own-session)), and — through `agent_destroy` — the **teardown of any of its owner's agents but the chief** (the tasks are kept; **no audit row**: the host is named only in muster's server log; also an accepted risk, by operator decision — [attention.md](attention.md#mirrored-claude-code-sessions)) — never another task, another host's session, another owner's ask or request, a secret, or a chief tool. See [mirroring.md](mirroring.md) and [attention.md](attention.md#mirrored-claude-code-sessions). |
| Flow runner token (`mfr_…`) | its owner, confined to one host | See [browser-flows.md](browser-flows.md). |
| Plugin token (`mpl_…`) | its owner, on `/plugin/v1/*` only | Minted at `/plugins` and shown once. Every other route refuses it. See [plugins.md](plugins.md). |
| Device credential (`mdv_…`) | its owner, on `/api/device/*` only | A phone paired with the [Android app](design/android.md). See [Paired devices](#paired-devices). Every other route refuses it. |

The two shared secrets may not start with `mst_`, `mpl_` or `mdv_`. The server
refuses to boot if one does.

## Paired devices

The native Android app gets its own credential, the **device credential**
(`mdv_…`), one per paired phone. It opens `/api/device/*` and nothing else.

**Pairing.** A signed-in page inside the app asks for a one-time pairing code
(`POST /account/devices/pair`). That needs a sign-in from the last five minutes,
as adding a passkey does, because the credential outlives the session. The code
is single use and expires after 60 seconds. The app redeems it at
`POST /api/device/pair` with the phone's name, its app version and its step-up
public key (an EC P-256 key the app generates in the Android Keystore at
pairing). The credential is shown to the app once. Only its sha256 is stored.

- **A device credential cannot mint another.** Only a browser session can ask
  for a pairing code.
- **It is confined to `/api/device/*`.** Every other route refuses it, and the
  device routes refuse every other credential, a session cookie included.
- **Which listener.** In gotrue mode the phone reaches muster through the public
  edge, so the device routes are served on the browser listener as well. The
  shared hook and service secrets stay refused there.

**Revocation** is read from the database on every request, and nothing is
cached. A device is signed out on its next request when:

- you remove it on **Account → Devices**, or the app unpairs itself
  (`DELETE /api/device`);
- the account is disabled;
- the session that paired it signed in at or before the account's session
  cutoff, which a disable or a password reset moves;
- the session that paired it signed in at or before your latest **Sign out
  everywhere** (gotrue mode only). This is the design's recommendation, not an
  operator requirement: it is what "everywhere" is expected to mean for a phone.
  It is stricter than for personal tokens, which Sign out everywhere does not
  revoke.

**Account → Devices** (on `/account`) lists each phone: its name, app version,
when it was paired, when it was last seen, and whether push is registered. Each
has a **Remove** button. Removing a phone also removes its push registration. A
user may pair at most 20 phones.

The device routes:

| route | what it does |
|---|---|
| `GET /api/device/config` | The VAPID public key the app registers for push with, and the operator's push hosts. |
| `PUT /api/device/push` | Registers the phone's UnifiedPush endpoint `{endpoint, p256dh, auth}`. The endpoint must be on an allowed push host ([push.md](push.md#which-push-services)), and muster keeps it only after a test push to it succeeds. A phone holds one registration; a new one replaces it. |
| `DELETE /api/device/push` | Removes the phone's push registration. |
| `DELETE /api/device` | Unpairs the phone. |

Two endpoints help with personal tokens:

- `GET /api/whoami` reports who a credential acts as.
- `GET /api/tokens` and `DELETE /api/tokens/{id}` manage personal tokens. They
  accept a personal token only.

From the CLI these are `muster whoami`, `muster token ls` and
`muster token revoke <id>` ([cli.md](cli.md)).
