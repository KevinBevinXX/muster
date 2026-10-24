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

### Mirror tokens and session routing

With [session routing](plugins.md#routing-a-message-from-your-own-session) on,
`POST /mirror/mcp` also offers `plugin_messages_list` and `plugin_message_route`.
Through them a mirror token **reads its owner's 30 most recent routable inbound
plugin messages** (the sender with its `UNVERIFIED` mark, the subject, the first
280 characters of the body, the route state) and **routes any of them into any
of its owner's mirrored Claude Code sessions that can take one**. The text
pasted is always the stored message, fenced as untrusted, never words the
caller writes, and the `session_route.routed` audit row names the host and
session the route came from.

🔴 **Accepted risk: a mirrored session can route a message to itself and
answer it.** Any of the owner's mirrored sessions — or anyone holding a leaked
mirror token — can route any of the owner's recent repliable messages **to
itself** and answer it with `route_reply`. When the sender is verified and on
the owner's allowlist for that plugin (`MUSTER_ROUTE_RELAY_ALLOW`), that answer
is **sent with no human review**. For any other sender it becomes a pending
reply draft for the owner — and **if this muster has the `plugin_draft_decide`
tool**, the same session can approve that draft, its own reply, over the same
door, so that reply too reaches the outside sender with no other review.
Revoke a token you suspect.

## Paired devices

The native Android app gets its own credential, the **device credential**
(`mdv_…`), one per paired phone. It opens `/api/device/*` and nothing else.

**Pairing.** A signed-in page inside the app asks for a one-time pairing code
(`POST /account/devices/pair`). That needs a sign-in from the last five minutes,
as adding a passkey does, because the credential outlives the session. The code
is single use and expires after 60 seconds. The app redeems it at
`POST /api/device/pair` with the phone's name, its app version and its step-up
key: an EC P-256 public key the app generates in the Android Keystore at
pairing, its type, and the Keystore security level the app reports. The
credential is shown to the app once. Only its sha256 is stored.

**The step-up key** ([design](design/android.md#the-step-up-key)):

- A `biometric` key (a fresh strong biometric on every use) is accepted.
- A `biometric_or_credential` key (the device PIN also works) is accepted only
  when `MUSTER_ANDROID_ALLOW_PIN_KEYS` is set. It is off by default, because such
  a key survives a new fingerprint being enrolled.
- A key the app reports as `software` is refused. Until key attestation exists,
  muster trusts the app's report.
- A phone without an acceptable key pairs **read-only**, with no key.

- **A device credential cannot mint another.** Only a browser session can ask
  for a pairing code.
- **It is confined to `/api/device/*`.** Every other route refuses it, and the
  device routes refuse every other credential, a session cookie included.
- **Which listener.** In gotrue mode the phone reaches muster through the public
  edge, so the device routes are served on the browser listener as well. The
  shared hook and service secrets stay refused there.

**Revocation.** These end a phone, and each one **deletes** it: its row, its
push registration (so it gets no more notifications), its place in the 20-phone
limit and its line on Account → Devices.

- you remove it on **Account → Devices**, or the app unpairs itself
  (`DELETE /api/device`);
- the account is disabled;
- a password reset (it moves the account's session cutoff);
- **Sign out everywhere** (gotrue mode only). This is the design's
  recommendation, not an operator requirement: it is what "everywhere" is
  expected to mean for a phone. It is stricter than for personal tokens, which
  Sign out everywhere does not revoke. If muster cannot record the sign-out, or
  cannot tell who you are, the page says it did not finish rather than
  pretending it did.

As a second wall, every request also checks the account from the database,
with nothing cached: a phone whose pairing sign-in is at or before the account's
session cutoff or latest Sign out everywhere, or whose account is disabled, is
refused, and is not sent notifications, even if its row somehow survived.

**Throttles.** Pairing is limited to 10 attempts a minute per client address.
A device route request with a valid credential is charged only to its phone:
60 a minute. A refused request (no, a malformed, or an unknown or revoked
credential) is charged to its address: 120 a minute, then 429. So junk sent
from an address a phone shares cannot lock that phone out. Registering push,
which makes muster send a test push, is limited to 5 a minute per phone. The
address is the one the edge reports: on the browser listener that is
`CF-Connecting-IP` (or the first `X-Forwarded-For` entry), which only an edge in
front of muster can be trusted to set.

**Account → Devices** (on `/account`) lists each phone: its name, app version,
when it was paired, when it was last seen, whether push is registered, and its
step-up key type as the app reported it (biometric, biometric or PIN, or none:
read-only), labelled "(reported)" until key attestation exists. Each
has a **Remove** button. Removing a phone also removes its push registration. A
user may pair at most 20 phones.

The device routes:

| route | what it does |
|---|---|
| `GET /api/device/config` | The server version; push (whether it is served, the VAPID public key the app registers with, and the operator's push hosts, for the app's pairing check); whether PIN keys are allowed; and the feature switches (`push`, and `actions`, `bubbles`, `state`, which stay false until their phases ship). |
| `PUT /api/device/push` | Registers the phone's UnifiedPush endpoint `{endpoint, p256dh, auth}`. The endpoint must be on an allowed push host ([push.md](push.md#which-push-services)), and muster keeps it only after a test push to it succeeds. A phone holds one registration; a new one replaces it. |
| `DELETE /api/device/push` | Removes the phone's push registration. |
| `DELETE /api/device` | Unpairs the phone. |

Two endpoints help with personal tokens:

- `GET /api/whoami` reports who a credential acts as.
- `GET /api/tokens` and `DELETE /api/tokens/{id}` manage personal tokens. They
  accept a personal token only.

From the CLI these are `muster whoami`, `muster token ls` and
`muster token revoke <id>` ([cli.md](cli.md)).
