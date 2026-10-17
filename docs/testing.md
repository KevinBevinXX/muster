# Testing

[CONTRIBUTING.md](../CONTRIBUTING.md) has the short version. This page covers
what each suite is, and what a green result does and does not mean.

## The Go suite

```sh
make test-db                     # throwaway Postgres on 127.0.0.1:55432, template migrated
export MUSTER_TEST_DATABASE_URL='postgres://muster:muster@127.0.0.1:55432/muster_test?sslmode=disable'
export MUSTER_TEST_REQUIRE_DB=1
go test -race ./...              # or: make test (sets both for you)
make test-db-down
```

🔴 **Without `MUSTER_TEST_REQUIRE_DB=1`, a bare `go test ./...` on a machine with
no Postgres passes without having run any of the store logic.** Every
database-backed test calls `t.Skipf`, `go test` exits 0, and the output looks
exactly like a run that tested everything. That variable turns a missing database
into a hard failure that names itself. CI sets it; set it locally too.

`MUSTER_TEST_PG_PORT=55433 make test-db` gives a second checkout its own database.

`internal/dbtest` gives every Postgres-backed test **package** its own database,
created with `CREATE DATABASE … TEMPLATE` from the one your DSN names. So a
package inherits an already-migrated schema instead of rebuilding it, and nothing
another package writes is visible to it.

The creates are serialised on one advisory lock. If that lock is unavailable, the
serialisation degrades rather than fails: an optimisation that can make the gate
fail is worse than none. Read the package comment in `internal/dbtest/dbtest.go`
before touching it. The mechanism looks over-built, but each part of it is there
for a failure that actually happened.

## The gates

| target | what it checks |
|---|---|
| `go vet ./...` | static checks |
| `make test` | the Go suite, with the database required |
| `make leakscan` | The public-repo leak gate. Its self-test runs first, so it can be seen to refuse. |
| `make css-check` | The committed `web/static/app.css` is exactly what the views produce, and each Tailwind content glob still matches something. |
| `make verb-ledger` | The **built** CLI exposes exactly its specified verbs. |
| `make check` | All of the above. |
| `make e2e` | The Playwright suite (see below). |

Two documentation gates run inside `go test ./internal/modulegate/`:

- `TestEveryEnvVarTheCodeReadsIsDocumentedAndViceVersa` checks that the
  variables the code reads (collected by parsing the source) match the table in
  [configuration.md](configuration.md), in both directions.
- `TestEveryRelativeDocLinkResolves` checks that every relative link and image in
  `README.md`, `CONTRIBUTING.md` and `docs/` points at a file that exists, and
  that every `#anchor` points at a heading.

## The browser suite (`e2e/`)

The browser suite is Playwright on Chromium. It runs against locally built
servers and a seeded Postgres database whose name must end in `_e2e`, because the
seed truncates it. Some specs also start a real GoTrue in Docker
(`e2e/gotrue/compose.yml`). Mirrored-session specs run a real `muster-mirror`
over a real tunnel. Only tmux is faked: a build with the `e2emirror` tag. The
default viewport is a phone, 390×844.

```sh
cd e2e && npm ci && npx playwright install chromium && cd ..
MUSTER_E2E_DATABASE_URL='postgres://muster:muster@127.0.0.1:55432/muster_e2e?sslmode=disable' make e2e
```

Create the `_e2e` database first, for example with
`docker compose -f docker-compose.test.yml exec postgres createdb -U muster muster_e2e`.
`MUSTER_E2E_PORT` moves the whole port block (default 18181), so two checkouts
can run side by side.

**Layout shift** is gated in the browser suite too, by `e2e/tests/cls.spec.ts`,
at 390×844 (touch) and 1280×800. What it measures, exactly:

- every page's **load**: the page is opened, its htmx loads are let land, and
  the summed `layout-shift` entries (input-caused ones excluded) must stay under
  0.01. Each page must also show the content it is for, so a page that drew
  nothing cannot pass as still. The page list comes from
  `internal/api/testdata/routes.golden`: a new `GET` route fails the spec until
  it is measured there or named as not a page;
- the chief panel opened, and restored open on a load;
- **after a load**, started by script so nothing counts as input: the Agents
  list re-rendering with nothing changed, an agent arriving while the list is
  open (held behind the "N new ↑" pill, so nothing moves until the reader taps
  it — with a positive control: a fresh arrival APPLIED at once must read a
  shift above 0), an in-page tab switch, and a boosted navigation and the
  loads it starts. ⚠ Chromium's layout-shift entries leave out a node MOVED in
  the DOM, so a re-order done by moving cards reads 0 however far they jump;
  the hold's own e2e (agents-search) pins the DOM order instead;
- a tapped tool-group expand is excluded as input, and the same expand by
  script is counted (the spec's own check of its exclusion).

`e2e/verdict.mjs` fails the run on any skip, or on fewer passes than its floor.
In CI the suite runs as parallel legs, each with its own Postgres
(`e2e/legs.json`). The `e2e` check aggregates them, and refuses a spec that ran
in no leg or in two.

## Screenshots

The images in [`docs/img/`](img) are generated, not hand-made:

```sh
MUSTER_E2E_DATABASE_URL='…/muster_e2e?sslmode=disable' make screenshots
```

This runs `e2e/docshots/docs.spec.ts`. It uses only the seeded fixture board and
a fake mirrored host, so no real data appears in it. It is opt-in and never part
of `make e2e`. It drives:

- a gotrue-mode server against the real GoTrue image;
- Chromium's virtual passkey authenticator;
- a fake speech-to-text service.

It captures at 390×844, then compresses each PNG. `npm run capture` in `e2e/` is
a separate generator, for the PWA manifest's install-sheet screenshots.

## What green does not prove

- **`nix build` tests only the packages each derivation installs.** That is
  `cmd/muster` and `internal/taskstatus` for the CLI, and `cmd/muster-migrate` and
  `internal/db` for the migrator. A `go test ./...` inside the derivation would
  fail, not just be weaker. Several packages assert properties of the
  repository: they shell out to `git`, or read `Dockerfile`,
  `tailwind.config.js` and similar files, and the derivation's `src` is an
  allowlisted subset that carries none of them. `flake.nix`'s `goCheckPhase`
  states the full list. **The CI `test` job is the only fully green suite.**
- 🔴 **A green `nix flake check` is not evidence that a check ran.** Nix skips
  anything already in the store, and can print "running 0 flake checks / all
  checks passed!". It answers "does every output evaluate". So CI's `nix` job runs
  `nix build` on the named outputs, and keeps `flake check` as a separate step.
- **The live-environment controls are not in CI.** `make test-liveenv` needs a
  real coding-agent session and a reachable agent runtime. It is the only thing
  that proves a real runtime accepts muster's chat protocol.
