# Contributing to muster

## 🔴 Read this first: a green `go test ./...` may mean nothing

muster's store logic is tested against a real Postgres. When there is no database
to reach, every one of those tests calls `t.Skipf` — `go test` exits **0**, prints
`ok` for each package, and looks identical to a run that tested everything.

So the first thing to do is make a skip impossible:

```sh
make test-db      # starts a throwaway Postgres, migrates it, prints the exports
export MUSTER_TEST_DATABASE_URL='postgres://muster:muster@127.0.0.1:55432/muster_test?sslmode=disable'
export MUSTER_TEST_REQUIRE_DB=1
go test -race -cover ./...
```

| variable | unset | set |
|---|---|---|
| `MUSTER_TEST_DATABASE_URL` | database-backed tests **skip** | they run against that server |
| `MUSTER_TEST_REQUIRE_DB` | a missing database is a **skip** | a missing database is a **hard failure** |

Both are read in exactly one place, `internal/dbtest`, and a test in
`internal/dbtest/ledger_test.go` fails if a second file ever reads either of them
in code. That is not tidiness: a fixture with its own opinion about whether to
skip is a test that CI's requirement silently does not reach.

If you cannot get Postgres running, **say so in the pull request.** A PR that
says "I could not run the store tests" is far more useful than one whose author
saw green and assumed.

## Starting the database

`make test-db` drives `docker-compose.test.yml`. By hand:

```sh
docker compose -f docker-compose.test.yml up -d --wait
DATABASE_URL='postgres://muster:muster@127.0.0.1:55432/muster_test?sslmode=disable' \
  go run ./cmd/muster-migrate
```

🔴 **The database must be NAMED.** `docker-compose.test.yml` sets
`POSTGRES_DB: muster_test` and that line is load-bearing. `internal/dbtest` uses
the database in your DSN as a `CREATE DATABASE … TEMPLATE` source, and connects
to the `postgres` maintenance database to issue that statement. Leave
`POSTGRES_DB` unset and the server's default database *is* `postgres`, so the
template and the maintenance database are the same one — every other test
binary's admin session is then sitting on the database yours is trying to copy,
and Postgres refuses:

```
source database "postgres" is being accessed by other users (SQLSTATE 55006)
```

⚠ **A single package passes either way,** because Postgres does not count the
asking backend. It only breaks under `go test ./...`, which is how anyone
actually runs it. Four copies of an earlier version of this recipe had already
drifted into a variant with `POSTGRES_DB` missing, which is why the recipe is a
checked-in file now rather than a comment.

The migrate step is not required for correctness — each package would bootstrap
its own schema — but it is the difference between building the schema once and
building it once per package.

## The gates

Everything CI runs, runnable locally, in the order it costs:

```sh
go build ./...                                        # build
go vet ./...                                          # vet
python3 tests/leakscan.py --self-test                 # the gate can REFUSE
python3 tests/leakscan.py                             # the tree is clean
MUSTER_TEST_REQUIRE_DB=1 go test -race -cover ./...   # the suite
make css-check                                        # the committed stylesheet is current
make verb-ledger                                      # the built CLI has exactly its verbs
```

or `make check`, which runs all of them. The browser suite is `make e2e`; see
[docs/testing.md](docs/testing.md) for what it needs.

`nix develop` gives you the pinned Go toolchain, `psql`/`pg_dump`, Python and the
Docker client, and its `shellHook` prints every one of those commands verbatim.

## The leak gate

muster was extracted from a private deployment. `tests/leakscan.py` refuses
credentials, private addresses, lab hostnames, non-public registry references,
operator identities, a closed set of private names, and dated incident
references.

🔴 **Its `--self-test` runs before the scan, and on every invocation.** A scanner
wired to nothing reports zero findings exactly the way a clean tree does. The
self-test proves four things: that a realistic sensitive string is refused, that
the matcher can produce a non-zero count at all, that legitimate content is *not*
refused, and that the files the gate EXEMPTS from the scan — its own fixtures —
spell no sensitive value that is not either objectively safe or declared with a
reason. If any control misbehaves the gate exits **2** — which is "could not
vouch", not a pass.

🔴 **Editing `tests/leakscan.py` is the one place a real value can hide, and one
did.** A negative control there carried a real cluster's LAN address into public
history. If the self-audit refuses a fixture you added, the fix is a value that
is nobody's — a reserved documentation address (RFC 5737) wherever the rule under
test allows one, a pre-2010 date, an invented hostname — not a row in
`EXEMPT_FIXTURE_VALUES`. Add a row only when the rule being controlled cannot
fire on anything else, and write down how you know the value is not real.

Two conventions it enforces that are easy to trip over:

- **Pin a measurement to a commit, not a day.** `MEASURED at \`465f8a3\`: …`
  passes; `MEASURED <a real date>: …` does not. A date plus an observation is an
  incident reference; the mechanism is what is worth keeping. (This bullet cannot
  spell the bad example, because the gate reads this file and refused an earlier
  draft of this very line. That is the gate working, and it is the cheapest
  possible demonstration that it is not wired to nothing.)
- **Fixtures that genuinely need a date use year 2000.** That year is allowed
  everywhere, and so is Go's `2006-01-02` reference instant.

To add a name to the denylist, add the SHA-256 of its lowercase spelling and bump
`DENIED_COUNT`. The names are stored as digests because a plaintext denylist in a
repository that may one day be public *is* the leak it exists to prevent.

## Documentation

The README is for newcomers; everything else lives in [`docs/`](docs/README.md).
Two tests in `internal/modulegate` keep the docs honest, and both run in
`go test ./...`:

- **A new environment variable needs a row in
  [`docs/configuration.md`](docs/configuration.md).** The test parses the Go
  source for every variable it reads, and fails on a variable with no row and on
  a row nothing reads.
- **Relative links and images must resolve,** including `#anchors`.

Screenshots in `docs/img/` are generated: `make screenshots` (see
[docs/testing.md](docs/testing.md#screenshots)). Re-run it when a pictured view
changes rather than editing the images by hand.

## Changing the schema

Add a new numbered file in `internal/db/migrations/`. Do not edit `0001`.

Keep `GENERATED ALWAYS AS IDENTITY` on every `id`;
`internal/db/migrate_test.go` asserts it across the whole schema and will tell
you why.

If you change the set of tables, update `tablesThisSchemaMustHave` in that same
file **deliberately**. It is a ledger: it fails when the set grows *or* shrinks,
so a table appearing without a decision reddens too.

To compare a schema against another database's:

```sh
pg_dump --schema-only --no-owner --no-privileges -d <a> > a.sql
pg_dump --schema-only --no-owner --no-privileges -d <b> > b.sql
diff -u a.sql b.sql
```

⚠ `pg_dump` emits `\restrict`/`\unrestrict` lines carrying a random token, so
strip those before diffing or every comparison differs. Cross-check with `cmp`
as well as `diff`: two tools that fail differently are worth more than one, and
a comparison against a missing operand reports SAME rather than MISSING.

## Tests

Three things this project asks of a test, which are not the usual three:

1. **Watch it fail.** A regression test that has never been red against the
   pre-change code is not known to test anything. Say in the PR which commit or
   mutation you saw it fail against. If it pins an invariant the bug never
   violated, label it an invariant guard — that is a fine thing to be, but it is
   not regression coverage.
2. **Report the pair, never the zero.** "0 violations" is indistinguishable from
   a check wired to nothing. Show that the number can move: `tests/verdict.py`
   prints the count of tests that produced a verdict *beside* the skip count for
   exactly this reason.
3. **Pin relationships, not components.** The defects that survive here live in
   seams — two things each correct alone. A ledger that fails when a set grows
   *or* shrinks catches what a "does X exist" check cannot.

## Pull requests

No CLA, no template, no commit-message convention. Branch, open a PR, and say
what you ran and what you could not run.

⚠ **CI gates the pull-request branch, not the tree its merge creates.** If your
branch is behind `main`, merge `main` into it and re-run before asking for a
merge — a reviewer who approved another PR before yours existed proved nothing
about the combination.
