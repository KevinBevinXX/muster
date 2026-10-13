# muster

To muster is to assemble a force for duty; a muster roll is the register of who
is present and what they are assigned.

muster is a work queue for coding agents: **tasks** that people and agents both
read and write, **dispatch** that provisions an agent against a task, and a
**chief** that reports on the fleet. It is a service with an HTTP API and its own
Postgres database.

🔴 **muster serves, and it does not yet provision.** This paragraph used to read
"there is no server, no HTTP API, no CLI and no UI in this tree today", and that
was true when it was written and false for the whole of the change that carved
the domain layer in — during which `internal/api` registered 99 routes,
`internal/ui` rendered every view, and **neither linked into any binary**. `go
list -deps ./cmd/...` resolved four packages of this module and `internal/api`
was in none of them: sixteen packages compiled, were tested, and no process
could execute a line of them. Two gates now make that impossible to repeat —
`TestTheHTTPLayerLinksIntoABinary` pins the link graph and
`TestTheServerListensAndServesHealth` binds a real port and drives a real
request. Neither is sufficient alone; see `internal/modulegate`.

What you get today: the task board, the agent surfaces, the connector store,
the machine API, a session-authenticated web UI, and a CLI. (Runbooks and
privilege profiles existed once; both were removed — privileges were superseded
by connectors — and migration `0005` dropped their tables.) What you
do **not** get: agent provisioning. Nothing in this module implements
`api.Provisioner`, so every agent-control route is registered and refuses, and
the server says so on every boot. The four seams that state is made of — what,
why, what closes each, and who checks — are in `cmd/muster-server/doc_seams.go`.

## Quickstart

You need Go (the version in `go.mod`) and Docker. Nix users get both from the
flake.

```sh
git clone https://github.com/ZacxDev/muster && cd muster
go build ./...          # works on a fresh clone: three binaries, no codegen
make test-db            # starts a throwaway Postgres, migrates it, prints 2 exports
make test               # the Go suite, with the database REQUIRED
make leakscan           # the leak gate; its self-test runs first
make css-check          # the stylesheet matches the views, and is not stale
```

There **is** an asset pipeline — a Tailwind build — but its output
(`web/static/app.css`) is **committed**, because a later `go:embed` makes it a
build input and `.gitignore`'s first line says nothing `go build` needs may be
ignored. `make css-check` is what stops a committed stylesheet going stale, and
it is the check the container image runs too. It matters more than it looks:
Tailwind does **not** error on a content glob that matches nothing — it emits a
smaller, perfectly valid file and exits 0.

To run the server against that Postgres:

```sh
export DATABASE_URL="$MUSTER_TEST_DATABASE_URL"
export MUSTER_UI_PASSWORD='something at least as long as the refusal demands'
export MUSTER_STANDALONE=1     # "this deployment has no permission router"
go run ./cmd/muster-server     # then http://127.0.0.1:8105
```

🔴 `MUSTER_STANDALONE=1` is not a convenience flag. Without a permission router
the server has no way to ask whether a session transcript exists, so every
transcript link would render "no transcript recorded" — which is TRUE with no
router and a confident falsehood if you merely *forgot* to configure one. Those
two situations are the same observable, so the server refuses to report ready
until you say which you are in. `/readyz` answers 503 and names the field; every
`MUSTER_*` name is spelled once, in `cmd/muster-server/config.go`.

`make test-db` prints two exports. **Set both.** The second one is the point:

```sh
export MUSTER_TEST_DATABASE_URL='postgres://muster:muster@127.0.0.1:55432/muster_test?sslmode=disable'
export MUSTER_TEST_REQUIRE_DB=1
```

🔴 **Without `MUSTER_TEST_REQUIRE_DB=1`, a bare `go test ./...` on a machine with
no Postgres is GREEN having executed none of the store logic.** Every
database-backed test calls `t.Skipf`, `go test` exits 0, and the output looks
exactly like a run that tested everything. That variable turns the absence of a
database into a hard failure that names itself. CI sets it. Set it locally too.

## What you can run

| you want | run |
|---|---|
| build everything | `go build ./...` |
| the static checks | `go vet ./...` |
| a throwaway Postgres, migrated | `make test-db` |
| the suite, database required | `make test` |
| the suite without a database (skips, and says so) | `go test ./...` |
| apply the schema to any database | `DATABASE_URL=… go run ./cmd/muster-migrate` |
| the leak gate | `make leakscan` |
| everything CI runs | `make check` |
| stop the throwaway Postgres | `make test-db-down` |
| the built CLI's verb set, asserted | `make verb-ledger` |
| a pinned dev shell (nix) | `nix develop` |
| build the CLI (nix) | `nix build .#muster-cli` → `result/bin/muster` |
| run the CLI without installing (nix) | `nix run .#muster-cli -- task ls` |
| build the migrator (nix) | `nix build .#muster-migrate` |
| build the migrator image (nix, Linux) | `nix build .#migrate-image` |
| build the mirror host binary (nix) | `nix build .#muster-mirror` |

### Installing the CLI from somewhere else

`cmd/muster` is a machine client for this service's JSON API, and because this
repository is public it is **fetchable and pinnable by revision** — a downstream
flake adds `muster.url = "github:ZacxDev/muster"` and takes
`muster.packages.${system}.muster-cli`. That is the point of the output
existing: the alternative is packaging a client from a local path, where the
`vendorHash` is only ever correct for whichever checkout a given host happens to
hold.

Two things to know before you wire it up:

- **The output is `muster-cli`; the binary is `muster`.** The attribute is not
  called `muster` because that name belongs to this project's *service*
  (`cmd/muster-server`), which will want an output of its own.
- **The binary answers to whatever name you invoke it as.** `cmd/muster/progname.go`
  reads `argv[0]` and uses it for help text, the error prefix and the
  version-skew note, so symlinking it under another name is supported and
  changes nothing else — same command tree, same config, same service. It is
  there so a host whose tooling matches on a command line can keep matching.

### Credentials: personal API tokens

The CLI presents one credential, read from `MUSTER_TOKEN` (or its older name
`MUSTER_HOOK_TOKEN`, which `MUSTER_TOKEN` beats within the same source) in
`~/.muster/muster.env`, the environment, or `--token`.

- **A personal API token** (`mst_…`) acts as **you** — your user id and role,
  under row-level security — on every machine-tier route, including the LAN
  machine port and the browser (edge) listener. Mint tokens in the browser at
  `/account/tokens`, and only there (it is shown once; only its sha256 is
  stored); a token cannot mint another. `muster token ls` and
  `muster token revoke <id>` manage them, and `muster whoami` says who a
  credential acts as. Revoking a token refuses it on the very next request;
  disabling its user revokes all of that user's tokens, and re-enabling the
  user brings none of them back.
- **The shared hook secret** (`MUSTER_HOOK_TOKEN` on the server) acts as the
  admin, user 1, and so does `MUSTER_SERVICE_TOKEN`. Every holder
  of the shared secret acts as that one user, so a person who wants their own
  identity on the machine tier uses a personal token instead. `token …` refuses
  the hook secret: minting a credential is a person's act.

## The schema

Numbered migrations in `internal/db/migrations/`, applied in order at boot. The
public schema they leave (pinned as a ledger by `internal/db/migrate_test.go`):

- **tasks** — `notes`, `note_comments`, `note_attachments`, `task_sessions`
- **agents** — `agents`, `chat_sessions`, `chat_messages`, `agent_retired_names`,
  `cc_account_marks`
- **connectors** — `connectors`, `connector_grants`, `connector_audit`
- **accounts** — `users`, `invites`, `api_tokens`, `audit_log`
- plus `github_connection`, one row per owner

`0006` gives every user-data table an `owner_id` (backfilled to user 1, the
operator) and makes each parent→child link a composite foreign key over
`(parent_id, owner_id)`, so a row can never point at another user's row.
`agent_retired_names` and `cc_account_marks` stay unowned.

`0007` turns on **row-level security** for those eleven owned tables (ENABLE +
FORCE, one `owner_isolation` policy each): a connection sees and writes only the
rows whose `owner_id` is its `muster.uid` setting, unless `muster.scope = 'all'`.
`internal/db` sets both — and switches to the `muster_app` role, because a
superuser login bypasses RLS — on **every pool acquire**, from the context's
`authz.Principal`; a context with no principal sees nothing and inserts nothing.
`owner_id` now defaults to the principal's id. Today every principal is user 1,
so behaviour is unchanged. psql, `pg_dump` and backups connect as the login role
and still see every row.

🔴 **Restoring a backup into a fresh Postgres:** `muster_app` is a cluster role and
`pg_dump` does not carry it, so the dump's `GRANT … TO muster_app` lines fail
(`ON_ERROR_STOP` stops at the first). Restore without `ON_ERROR_STOP` (or
`CREATE ROLE muster_app NOLOGIN` first), then boot: `Migrate` re-creates the role
and its grants on every boot (`internal/db/approle.go`).

🔴 **Rolling the image back below `0007`:** an older image never sets
`muster.uid`, so every insert it makes fails. Run `--rls-off` with a 3a-or-newer
binary — `/muster-migrate --rls-off` in the CURRENT pod — or the psql path in
`internal/db/rollback/rls_off.sql`, **before** changing the image. The older
image's `/muster-migrate` does not know the flag: it says "nothing to apply",
exits 0, and leaves RLS on. Check `select relrowsecurity from pg_class where
relname='notes'` is `f` (the 3a+ binary prints this for every owned table and
fails if any is wrong). `/muster-migrate --rls-on` re-enables it.

`0001` also creates the runbook and privilege tables (`runbooks`,
`runbook_runs`, `privilege_profiles`, `agent_privileges`, `privilege_requests`)
and `notes.grant_profiles`; `0005` drops them. `0001` is not edited.

`notes` is the task table. The name is historical; it is kept because every id in
it is a task number people cite, and renaming it would be a data migration that
buys a word.

Two properties worth knowing before you change it:

- 🔴 **Every `id` is `GENERATED ALWAYS AS IDENTITY`,** and
  `internal/db/migrate_test.go` asserts it for every identity column in the
  schema, not a list of named ones. Weakening a column to `BY DEFAULT` is
  invisible for months: it changes nothing about how muster writes, and only
  shows up when somebody concludes a hand-written `INSERT` importer is fine.
  Existing rows are meant to arrive by `pg_dump --data-only`, whose `COPY` path
  is exempt from the restriction.
- ⚠ **Several columns are deliberately FK-free** — `task_sessions.session_id`,
  `notes.source_session_id`. They name things in other
  systems, or they are audit rows that must outlive their subjects. Adding a
  foreign key to one of them is not a cleanup.

`0001` is a single statement of the schema's **end state**, not a replay of the
history it was extracted from. Later changes get their own numbered file.

## Testing

`internal/dbtest` gives every Postgres-backed test **package** its own database,
by `CREATE DATABASE … TEMPLATE` from the one your DSN names — so a package
inherits an already-migrated schema instead of rebuilding it, and nothing another
package writes is visible to it. The creates are serialised on one advisory lock,
and that serialisation **degrades rather than fails**: an optimisation that can
redden the gate is worse than no optimisation.

Read `internal/dbtest/dbtest.go`'s package comment before touching it. The
mechanism looks over-built and each part of it is there for a measured failure.

## Limits, stated plainly

- **There is no agent provisioning.** `go build ./...` produces **three**
  binaries — `muster-server` (the service), `muster` (the machine/agent CLI) and
  `muster-migrate` (the schema step) — and the `Dockerfile` ships all three.
  What the server cannot do is create an agent: nothing here implements
  `api.Provisioner`, so dispatch, start, stop, destroy, logs and chat are
  registered routes that refuse. This line previously read "there is no server …
  produces exactly one binary"; both halves were falsified by the carve, and a
  reader who saw a non-empty `RegisterRoutes` and concluded otherwise was reading
  correctly and concluding wrongly.
- **The singleton background loops are ungated, so deploy one replica.**
  `internal/db` carries no lease, so the daily prune and the idle-task reap run
  unguarded. Both are idempotent, so a second replica costs duplicated work
  rather than duplicated effect — but it is not free. See
  `cmd/muster-server/doc_seams.go` entry 4.
- **The `e2e` job is real; `e2e-unit` is still a stub.** `e2e` runs the
  Playwright suite in `e2e/tests/` (Chromium, a locally built server, a seeded
  Postgres) and fails on any skip or on fewer passes than its floor
  (`e2e/verdict.mjs`); `make e2e` is the local spelling. In CI it runs as
  parallel legs, each with its own Postgres (`e2e/legs.json`), and the `e2e`
  check is their aggregate — it refuses a spec that ran in no leg or in two. `e2e-unit` runs nothing,
  says so, and fails the moment a `*.unit.mjs` harness test appears. ⚠ Several
  comments in `internal/ui` still cite upstream spec files (`tasks.spec.ts`,
  `task-board-paging.spec.ts`) that were never carried into `e2e/tests/`.
- **`nix build` tests only the packages the derivation installs, and this line
  used to overstate it.** It read "`nix build` runs the pure-Go tests only",
  which was two claims too many. What a nix build runs is `go vet` and `go test`
  over the same *named* package set — `cmd/muster` and `internal/taskstatus` for
  the CLI, `cmd/muster-migrate` and `internal/db` for the migrator. The
  whole-module `go vet ./...` is the CI `build` job's, not the nix build's.
  `go test ./...` there is not merely weaker, it is **red**: four
  packages assert properties of the *repository* (`internal/agentspec` shells out
  to `git`; `internal/modulegate`, `internal/notes` and `internal/ui` read
  `Dockerfile`, `.dockerignore`, `tailwind.config.js`, `web/css/input.css` and the
  root `testdata/`), and the derivation's `src` is an allowlisted subset that
  carries none of that. `flake.nix`'s `goCheckPhase` states the full list.
  Postgres-backed tests still skip — the sandbox has no database. **The CI `test`
  job is the only green suite.**
- 🔴 **A green `nix flake check` is not evidence a check ran.** Measured on this
  flake: it printed "checking derivation checks.x86_64-linux.muster-cli …
  derivation evaluated to …" and then "running 0 flake checks / all checks
  passed!", because nix skips what is already in the store. It answers "does every
  output evaluate", which is worth having and is not the same question. The CI
  `nix` job therefore runs `nix build` on the named attributes and keeps `flake
  check` as a second, separate step. Until that job existed, **nothing built a nix
  output at all** — and `nix build .#muster-migrate` was red on `main`
  (`internal/api` imports the root-level `web` package, which the source filter did
  not carry) for the flake's whole life, behind a full green tick.
- **The leak gate does not scan itself — it AUDITS itself, which is narrower.**
  `tests/leakscan.py` has to contain realistic sensitive strings (they are its
  negative controls), so it is exempt from the scan and printed as a named skip.
  That exemption was unaudited and something hid in it: a `private-ip` control
  carried the LAN address of a real single-node cluster from this file's first
  commit out into public history, past a hand review, because it looked exactly
  like its synthetic neighbours. Every file in `SKIP_FILES` is now audited by
  VALUE — every address, hostname, registry reference, credential, denied name
  and dated stamp it spells must be objectively unroutable, objectively
  synthetic, or declared in `EXEMPT_FIXTURE_VALUES` with the reason it is safe,
  and anything new fails closed. What that still accepts is written out in the
  module beside the table: a real value somebody declares anyway, prose (a real
  incident narrated in a fixture spells no value an audit can extract — one did),
  IPv6 in any form, and the fixtures of every file the gate actually scans.
- **No agent runtime.** The provisioner can create an instance; nothing here
  talks to it. Sending a message, streaming its tokens, servicing its tool calls
  — and therefore the question of whether an agent that is not one specific
  vendor's container can be driven at all — are not answered here. That absence
  is deliberate: putting the conversation on the provisioner interface is what
  made the original un-implementable by anything else.
- **The provisioner has two drivers and neither has run against a real
  cluster from this tree.** The Kubernetes driver's tests use a fake clientset,
  which structurally cannot see scheduling, image pulls, volume attachment,
  admission, or the exec stream. A green suite there means the manifests are
  what the code says they are, not that a pod came up.
- **The Kubernetes driver restricts no egress by DNS name.** Restricting an
  agent's egress by DNS name is the control that addresses exfiltration by a
  prompt-injected model, and muster does not implement it.
  `Capabilities.Policy` being true does not cover it, and neither does
  `Capabilities.NetworkIsolation`: for the one agent kind that asks for it
  (`claude-code`), the driver writes a per-agent NetworkPolicy that is an
  *address-range* policy — reachable only from muster's own pods, able to reach
  only the cluster's DNS pods and TCP 443 on public IPv4 addresses. Where the
  cluster's pod, service, API-server and node addresses, and the network around
  it, are all private (RFC 1918, link-local or CGNAT), that keeps such an agent
  off the cluster's API, other namespaces and that network; where any of those
  is a public address it does not, and muster does not check which kind of
  cluster it is on. Either way the agent can still resolve names and can still
  send anything it holds to any public HTTPS host. Every other agent gets no
  NetworkPolicy at all. And a NetworkPolicy is only a declaration: the
  cluster's network plugin enforces it or does not, and nothing in muster can
  tell which.

## The provisioner

`internal/provision` is the seam between "an agent should exist, configured like
this" and whatever runs it. It imports nothing outside the standard library, so
wiring the no-op driver costs none of a cluster client's dependency tree.

Four rules define it. Each rule's main clause is pinned by a test in
`provisiontest.RunContract` rather than by prose — with **one named exception**:
rule 2's `ErrNotManaged` clause, which needs a shared backend to be expressible
at all and is therefore pinned in the Kubernetes driver's own ownership tests
instead. `RunContract` has 20 cases and none of them mentions `ErrNotManaged`.
The rules are stated in full in the package doc; in short:

1. **A driver that cannot see its backend returns an error, never an empty
   set.** A caller reads a `List` error as "keep the stored status" and an empty
   `List` as "nothing is running".
2. **`Destroy` returns `nil` only if the instance was removed or was already
   absent.** A bare `return nil` satisfies the compiler and means nothing. A
   FOREIGN object holding the instance's name is neither case: it is
   `ErrNotManaged`, which deliberately does **not** satisfy
   `errors.Is(err, ErrNotFound)` — otherwise `if err != nil && !errors.Is(err,
   ErrNotFound)`, the idiom this rule invites, discards the refusal in silence.
   That refusal also means **nothing was removed**: a driver decides ownership
   before it deletes, so "removed the instance, then reported a permanent
   failure" — a state no retry loop can leave — cannot happen.
3. **`Create` is idempotent; a divergent spec is an error**, not a silent
   overwrite. Note the one asymmetry: `Create`'s refusal over a foreign
   co-named object keeps `ErrDivergentSpec` and does **not** report
   `ErrNotManaged`.
4. **`Capabilities` is useless unless callers branch on it.** Each capability's
   refusal is spelled in exactly one place, and **`capabilities.go`'s doc
   comment is the authority on which one** — deliberately not copied here.
   There used to be a copy, it said "`CheckSpec` and `Grant` … are the only
   places", and it was wrong about `CheckScale` and `Exec`; a partial
   enumeration of the guards reads exactly like a complete one. (`Grant`, and
   the policy capability it guarded, were removed with the privilege subsystem.)

`internal/provision/provisiontest` is the contract suite every driver must pass.
It requires a working driver, a **blind** one and a **restricted** one, and it
does not skip: without the blind driver the first rule is unfalsifiable, and
without the restricted one the capability refusals are never watched to fire.

| driver | what it is for |
|---|---|
| `provision.Noop` | records instead of provisioning; develop everything above the seam without a backend |
| `provision/k8s` | renders its own minimal manifests — Namespace, ServiceAccount, **two** Secrets (confidential env, and separately confidential files), ConfigMap, PVC, Deployment, Service — and installs no chart |

## Mirrored sessions (optional plugin)

A **mirrored session** is a tmux session on one of your own machines, shown in
the Agents view beside muster's provisioned agents, so the view is complete. It
is off unless the server runs with `MUSTER_MIRROR=1`; off, `GET /mirror/connect`
and the mirror terminal routes answer 404 and nothing mirrored is drawn.

**The host dials out; muster never dials in.** A small `muster-mirror` binary on
the host lists its tmux sessions and opens ONE WebSocket to muster,
authenticated by a per-host *mirror token*. muster multiplexes terminal
attaches back down that connection. muster holds no SSH key, address or inbound
route to the host, and the host holds no muster credential but its own token.

- **Not an agent, and not persisted.** muster keeps each connected host's
  session list in memory, under the token's **owner**, and draws it as
  "Mirrored · host" cards. Nothing is written to `agents`: a mirrored session
  cannot be dispatched, chatted with, deleted or mistaken for one (a session
  named `chief` is not the chief). A disconnected host stays on the view as
  `offline` for a day. Only `mirror_tokens` is in the database (owned, under
  row-level security; only each token's sha256 is stored).
- **Read-only unless the host opts in.** `muster-mirror` is read-only by default:
  muster offers no Interactive switch and refuses a rw attach. Read-only is
  enforced by muster, by the host, and by `tmux attach -r`, and the host opens
  only sessions it listed itself.
- 🔴 **`-interactive` is a shell on the host.** An interactive tmux client can
  switch to any session on that tmux server or open a new window, so anyone who
  can use muster's Interactive switch for the host can run commands there as the
  user `muster-mirror` runs as. The "only listed sessions" rule bounds read-only
  viewing; it is not a boundary once an interactive client is attached.
- **Typing needs a fresh sign-in (step-up).** An interactive attach is refused
  (403, before the socket upgrades — and again inside the plugin) unless the
  session signed in within the last 5 minutes. The time is the sign-in's — the
  GoTrue access token's `amr` timestamp, which a refresh does not move; a token
  with no `amr` is never fresh. The terminal page asks "Confirm it's you" (the
  password again, `POST /auth/confirm`, same-origin, throttled like sign-in) and
  remembers the Interactive choice per host for the browser session. Watching
  read-only needs no step-up.
- **A terminal never shrinks the session's window.** The far side (the host's
  `muster-mirror`, or muster itself for an agent pod) reads the tmux window's
  size and gives the browser's client that size unless the browser's own covers
  it; a phone renders the wider session in a scrolling, pinch-zoomable view with
  "Fit width". Hosts running a `muster-mirror` older than this still work — they
  report no size, and the page fits its container as before.
- **Limits:** 8 terminals per host, 64 hosts. A revoked token (or one the
  database cannot re-verify twice running) ends the host's connection and its
  open terminals within 30 s, and removes the host from the view. Repeated bad
  tokens get 429, but a valid token is never refused by that limit. One muster
  replica (the tunnel is in-process state).
- **A rendered view for Claude Code panes (tunnel v2).** When a session's
  active pane runs Claude Code, its card opens a *rendered* view by default:
  prompts and replies as chat bubbles, each tool call as one collapsed row
  (tool, target, ok / error / running), live while a turn runs; thinking is
  never shown, and the Terminal is one tap away. The host finds the session by
  walking the pane's process tree to the `claude` process and reading the
  CLI's own `~/.claude/sessions/<pid>.json`, then tails that transcript and
  streams bounded events (the last 300 at most on open, capped per field;
  never the file). Nothing is stored on the server. `muster-mirror -list
  -claude` prints what a host resolves. Sending a message from the view pastes
  it into the Claude pane (as typed — a leading `/` or `!` included; control
  characters are refused) and needs the host's `-interactive` **and** a
  sign-in from the last 5 minutes, the same gate as typing into the terminal;
  a stale one is asked to confirm and the message is then sent.
- **Rolling upgrade:** this muster accepts tunnel v1 and v2 hosts (a v1 host
  simply has no rendered view), and a v2 `muster-mirror` falls back to v1
  against an older muster — so either side can be upgraded first.

Mint a token where the server runs (it is printed once, owned by the operator),
then install and run the host binary:

```bash
# muster side
muster-server mirror token create <host>      # also: token ls, token revoke <id>

# host side
nix profile install github:ZacxDev/muster#muster-mirror   # or: go install ./cmd/muster-mirror
mkdir -p ~/.config/muster-mirror && install -m600 /dev/stdin ~/.config/muster-mirror/token  # paste the token
muster-mirror -url https://muster.example.com -token-file ~/.config/muster-mirror/token
```

`<host>` is a lowercase DNS label and defaults to the short hostname; a token
minted for one host cannot register another. `-interactive` allows typing,
`-tmux-socket`/`-tmux-socket-path` pick a non-default tmux server, and `-list`
prints what would be registered (`-list -claude` adds each Claude Code pane's
transcript). Every flag also reads a `MUSTER_MIRROR_*`
variable (see `-help`), so a systemd user unit needs no token in its command
line. If muster sits behind an auth proxy, `/mirror/connect` (GET) must bypass
it: the route authenticates its own tokens.

## Cairn memo relay (optional)

[cairn](https://github.com/ZacxDev/cairn) scope memos are short notices filed
against a scope (a repository), which each agent session working on that scope
is shown once. A host session gets them from a hook that runs cairn's client. A
dispatched agent has no client, no credential, and a network policy that keeps
it off private addresses — so muster relays them instead.

**muster holds one read-only credential and delivers memos as turns.** With
`MUSTER_MEMO_URL` (the cairn memo listener's base URL) and `MUSTER_MEMO_TOKEN`
set, `internal/memorelay` asks the listener for each scope's live memos once a
minute and hands every memo an agent has not been sent to that agent through
the agent's own gateway — the path a chat message and a first turn already
take. No credential reaches a pod and no agent's network policy changes.

- **Off until the token exists.** The relay runs iff `MUSTER_MEMO_TOKEN` is
  set; the URL alone is a valid, inert configuration, so the manifest can name
  the listener (and reference the secret as optional) before the credential is
  issued. The boot banner's `memo relay:` line says which state this is.
- **Only the operator's `claude-code` agents.** The credential is the operator's
  view of their own cairn, so the relay acts as the operator: row-level security
  hands it user 1's agents and nobody else's. And a memo waits for an idle agent
  only because that kind's runtime refuses a second turn before pasting
  anything; no other kind is known to, so agents of any other kind are counted
  (`result="kind_unsupported"`) and sent nothing.
- **The scope is derived, never guessed.** It is the repository the agent was
  dispatched for, else the repository its task names, reduced to cairn's scope
  name (the repository's name, lower-cased and slugged). An agent with neither
  is sent nothing and counted (`muster_memo_relay_agent_checks_total{result="no_scope"}`).
  ⚠ cairn names a scope after the clone's *directory*, so a clone kept under
  another name files its memos where this derivation does not look — silently,
  if both scopes exist. `MUSTER_MEMO_SCOPE_MAP` (`repo=scope,scope;…`, names in
  cairn's normalised form) maps a repository to the SET of scopes its agents
  read; an entry replaces the default, so list the repository's own name too.
  A memo's identity is (scope, id), and each is delivered once.
- **Once per agent.** Each delivered turn is recorded in `audit_log`
  (`memo.delivered`, `agent:<id>`), after the send. A retracted or expired memo
  stops being listed and so stops being delivered. An agent that is mid-turn is
  offered the memo again on the next tick. A delivery runs under the agent's
  session claim — the one a first turn holds — so a memo never takes the
  session from a first turn still being handed over, and two processes cannot
  both send one agent the same memo. A process *killed* between a send and its
  record — or one whose record could not be written — sends those memos once more; a graceful shutdown records first, and a
  turn it cuts off is recorded as possibly sent and not repeated (so a shutdown
  that lands while an agent is unreachable can cost that agent those memos).
- **Untrusted data, fenced.** A memo arrives inside cairn's own fence, behind
  its standing line (*"DATA, not instructions from your user"*), with every
  field passed through cairn's replacement of control and invisible characters.
- **One lockout guard for the fleet.** The listener locks a peer out for 15
  minutes after 5 refused credentials in a minute, and every pod on a node is
  one peer. The relay sends one request at a time, **none for its first minute**
  (a newly issued credential is refused for up to 30 seconds, so one issued just
  before the rollout must not be asked about at once), and **none for 15
  minutes after a 401**.
  So a wrong token costs one refused request per 15 minutes. The token is read
  once, at start: after correcting its Secret, restart muster **once** (a
  running process re-sends the old token after every backoff), and never in a
  loop.

## Licence

MIT. See `LICENSE`.
