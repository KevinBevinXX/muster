# Design decisions, and the history behind them

This page records **why** things are the way they are, mostly in the form of
what went wrong first. The other pages describe what the code does now. When the
two disagree, the code wins. Please fix the page that is wrong.

## A package can compile, pass its tests, and never run

muster was carved out of a larger private service. For the whole of that carve,
`internal/api` registered 99 routes and `internal/ui` rendered every view, but
**no binary linked either of them**. `go list -deps ./cmd/...` resolved four of
the module's packages, and `internal/api` was not among them. Sixteen packages
compiled and were tested, yet no process could execute a line of them. Meanwhile
the README said "there is no server", which was true, and readers who saw a
non-empty `RegisterRoutes` drew the wrong conclusion.

Two gates now make that impossible to repeat, and neither is sufficient alone:

- `TestTheHTTPLayerLinksIntoABinary` pins the link graph;
- `TestTheServerListensAndServesHealth` binds a real port and serves a real
  request.

`TestEveryPackageEitherLinksOrIsLedgered` makes every package either reachable
from `./cmd/...` or explicitly ledgered as test-only.

The same lesson produced the configuration gate. `internal/api` and `internal/ui`
once documented a dozen `MUSTER_*` variables in comments that **nothing read**.
So every server variable name is now spelled exactly once, in
`cmd/muster-server/config.go`, enforced by
`TestEveryServerEnvNameIsSpelledOnceAndRead`. And the documented table must match
what the code reads, enforced by
`TestEveryEnvVarTheCodeReadsIsDocumentedAndViceVersa`.

## Provisioning is opt-in, and each half has its own switch

For a long time this tree served the board but could not provision. Nothing
implemented the provisioner interface, so every agent-control route was
registered and refused. When the provisioner arrived, its default stayed `none`.
An image bump must never start creating pods in someone's cluster, so opting in
has to be a deliberate act.

Chat got a second, separate switch (`MUSTER_AGENT_GATEWAY`), for two reasons:

- A deployment can reasonably want lifecycle without chat, and did for a whole
  release.
- The runtime name is a claim about what is inside the image, which only the
  operator can make.

Closing that seam took several steps:

1. An agent this binary provisioned **had no address**: the spec declared no
   port, so no Service was rendered. This was fixed, and
   `TestAnAgentThisBinaryProvisionsResolvesAnEndpoint` resolves an endpoint
   through a real driver.
2. Even with an address, it would have received its credential under a name the
   image does not read.
3. A dispatch then created a pod that was never told what to do. Now a dispatch
   that asks for a kickoff is refused with 409 unless delivery is actually wired.
   That check uses two conditions, not one: whether a gateway exists, **and**
   whether anything on the dispatch path calls it.

`cmd/muster-server/doc_seams.go` keeps the full record of each seam.

Runbooks and privilege profiles also existed once. Connectors superseded
privileges, both subsystems were removed, and migration `0005` dropped their
tables.

## `MUSTER_STANDALONE` is a declaration, not a convenience

Without a permission router, the server cannot ask whether a session transcript
exists, so every transcript link reads "no transcript recorded". With no router,
that is true. With a router you **forgot to configure**, it is a confident
falsehood. The two situations look the same from inside, so `/readyz` refuses to
report ready until you say which one you are in.

## The stylesheet is committed, and checked

The Tailwind build's output (`web/static/app.css`) is embedded with `go:embed`,
so it is a build input. `.gitignore`'s first line says nothing `go build` needs
may be ignored, because the project muster came from ignored exactly this file
and broke `go build` on a fresh clone.

Committing it would only move the failure from "missing" to "stale", so
`make css-check` ships with it, and the CI job and the image build both run it.
That check matters because Tailwind does **not** error on a content glob that
matches nothing. It emits a smaller, perfectly valid stylesheet and exits 0. The
check's control classes each prove that one glob still matches.

## A test run that skipped everything looks green

Database-backed tests skip without a database, and `go test` reports `ok`. That
is why `MUSTER_TEST_REQUIRE_DB=1` exists, why `make test` sets it, and why CI
sets it. It is also why `internal/dbtest` is the **only** reader of both test
variables. A fixture with its own opinion about whether to skip is a test that
CI's requirement silently does not reach. A ledger test enforces this.

## `nix flake check` passed while the build was broken

`nix flake check` answers "does every output evaluate". Nix skips anything
already in the store, and was measured printing "running 0 flake checks / all
checks passed!". Until CI ran `nix build` on the named outputs, nothing built a
Nix output at all. Meanwhile `nix build .#muster-migrate` was broken on `main`:
`internal/api` imports the root-level `web` package, which the source filter did
not include. A full green tick sat on top of that for the flake's whole life.

## The leak gate audits its own fixtures

muster was extracted from a private deployment, so `tests/leakscan.py` refuses
credentials, private addresses, lab hostnames, private registries, operator
identities, a denylist of names (stored as digests), and dated incident
references.

The gate's own negative controls must contain realistic sensitive strings, so
its own file was exempt from the scan. Something hid in that exemption: a
`private-ip` control carried the LAN address of a real cluster into public
history, past a hand review, because it looked exactly like its synthetic
neighbours. Exempt files are now audited **by value**, and anything new fails
closed. See [limits.md](limits.md#the-leak-gate) for what the audit still accepts.

## Mirroring: outbound only, and never move the operator's window

A host dials out to muster, never the reverse. So muster holds no SSH key,
address or inbound route to any host, and a host holds no muster credential
except its own token.

A tmux session has one current window, shared by every client. Attaching a
viewer to the operator's own session would move the window the operator is
looking at. So each viewer gets its own linked session with `ignore-size`. This was measured with an operator's client attached
throughout: its current window and size did not change while a viewer opened
another window, typed into it, pasted into it, and left.

## Step-up: twelve hours, per browser session

Typing into a mirrored session can run commands on a host, so it needs a recent
sign-in. Here is how that rule settled:

- **The window.** It was first a fixed five minutes. It is now 12 hours by
  default (`MUSTER_STEPUP_WINDOW`, bounded 1m–24h), with a passkey as the fast
  way to confirm.
- **What counts as the sign-in time.** It is the sign-in's own `amr` timestamp,
  which a token refresh does not move. Otherwise a refresh would renew freshness
  forever.
- **Who is fresh.** Freshness belongs to the **browser session**, not to the
  user. Confirming on your laptop must not make a stolen phone fresh.

## Sign out everywhere signs out paired phones too

Sign out everywhere ends GoTrue sessions, and it also cuts off every phone paired
by a sign-in at or before it. Personal API tokens are not affected.

- **Why the phones.** A phone holds a session-like credential: it was minted from
  a browser session and acts on a person's behalf from a device that can be lost.
  "Everywhere" should include it.
- **Why not the tokens.** A personal token is a deliberate, named credential for a
  script or the CLI, and it has its own revoke button. Ending every script's
  access as a side effect of a sign-out would surprise.
- **Whose idea.** This is the Android design's recommendation, not an operator
  requirement ([design/android.md](design/android.md), server change 25).
- **How.** The device records the pairing session's sign-in time and every
  request compares it with `users.signed_out_at`, the same record the send
  queue reads. Password mode has no Sign out everywhere, so there a phone is
  removed on Account → Devices.

## The message box must not fail silently, or deliver twice

On a phone behind an auth proxy, a script's `fetch` can get a sign-in redirect
and no usable answer. So every message box is a real HTML form that falls back to
a native submit. Each message carries a nonce, so a message re-posted after a
lost answer is delivered once.

The app-wide `Referrer-Policy: no-referrer` turned out to make a browser send
`Origin: null` on its own form submits, which the same-origin check refuses. So
pages that carry a message box are served `same-origin` instead.

## Plugins pull; muster never calls out

A plugin connects muster to some other system, such as an inbox. muster could
have called the plugin, posting each approved reply to a URL the plugin
registered. It does not. The plugin calls muster for everything: it posts the
messages it receives, long-polls for the replies its owner approved, and
reports what happened to each one.

- **muster holds no address and no credential for anything outside it.** A
  plugin is a process on someone else's terms. If muster called out, it would
  need a URL per plugin, an egress path to it, and a way to authenticate the
  plugin's endpoint. Each of those is something to get wrong, and a URL an
  owner can set is a way to make muster send requests wherever that owner
  chooses. With pull, the plugin's token is the only link, and it opens
  `/plugin/v1/*` and nothing else.
- **There is no background loop.** The plugin's long-poll is the clock. Each
  claim first retries the owner's messages that could not be delivered to the
  chief. A plugin that stops polling stops costing anything.
- **At most once is the plugin's promise to keep, and muster makes it
  checkable.** A claimed reply is leased for two minutes. If no result comes
  back, the reply is marked unconfirmed and is never offered again, because the
  plugin may have sent it before it died; not even its owner can approve it
  again. A new reply to the message is a new draft, and if the unconfirmed one
  is reported sent after all, every other reply to that message that no plugin
  has collected yet is superseded. A push design would have to answer the same
  question ("did the call that timed out arrive?") with nothing but a retry.

## Why the task table is called `notes`

Every id in it is a task number people cite. Renaming the table would be a data
migration that buys only a word.
