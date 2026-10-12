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

🔴 **Rolling the image back below `0007`:** an older image never sets
`muster.uid`, so every insert it makes fails. Run
`/muster-migrate --rls-off` from the CURRENT image first (RLS off,
`owner_id DEFAULT 1`; `internal/db/rollback/rls_off.sql`), then change the image.
`/muster-migrate --rls-on` re-enables it.

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

## Licence

MIT. See `LICENSE`.
