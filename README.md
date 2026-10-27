# muster

To muster is to assemble a force for duty; a muster roll is the register of who
is present and what they are assigned.

muster is a self-hosted command post for coding agents. It has three parts:

- A **task board**, which people and agents read and write.
- **Dispatch**, which starts an agent on a task.
- **Mirroring**, which shows your own tmux and Claude Code sessions live. You can
  read them and send them a message from your phone.

The server is one Go binary backed by Postgres. The UI is htmx, built
mobile-first, and installs as a PWA. A small host agent, `muster-mirror`, dials
out from each of your machines, so muster never needs an inbound route to them.

<p align="center">
  <img src="docs/img/agents-filter.png" width="240" alt="The Agents list: mirrored tmux windows and a dispatched agent, narrowed by the filter">
  <img src="docs/img/mirror-rendered.png" width="240" alt="A mirrored Claude Code window, rendered as a chat, with the message box and microphone">
  <img src="docs/img/stepup-passkey.png" width="240" alt="The confirm-it's-you dialog offering a passkey before a message is sent">
</p>

## Features

- **Task board.** Tasks have statuses, comments, attachments and tags, and a
  review queue. They are written by people in the browser and by agents through
  the API and CLI. → [docs/tasks.md](docs/tasks.md)
- **Mirrored sessions.** Every tmux window on your machines appears as a card,
  most recently active first. A window running Claude Code renders as a chat.
  Viewing never moves your own terminal. → [docs/mirroring.md](docs/mirroring.md)
- **Message box.** You can send text into any mirrored pane or dispatched agent.
  It works without JavaScript and does not deliver a message twice. Typing into
  a mirrored session requires a recent sign-in, confirmed by passkey or password.
  → [docs/sending-and-step-up.md](docs/sending-and-step-up.md)
- **Dictation.** A microphone on every message box, transcribed by a
  speech-to-text service you choose. → [docs/dictation.md](docs/dictation.md)
- **File attachments.** Attach a file in the message box; the agent gets a
  short-lived link signed by muster, and the transcript shows a thumbnail, not
  the link. Needs an S3-compatible store. → [docs/uploads.md](docs/uploads.md)
- **Agent dispatch.** muster can provision agents on Kubernetes. It is opt-in
  and off by default. An agent holding no task and no repository works on the
  fleet itself: it is given the supervisor instructions and reports on the rest.
  No agent is privileged — every muster tool is offered to all of them. → [docs/provisioner.md](docs/provisioner.md)
- **Accounts.** One shared password, or per-user accounts through GoTrue with
  passkeys, invites, roles and personal API tokens. Row-level security keeps each
  user's data separate. → [docs/auth.md](docs/auth.md)
- **Web push and PWA.** Notifications for review-ready tasks, connector requests
  and more. The app installs to your home screen, with share target, shortcuts
  and a badge. → [docs/push.md](docs/push.md)
- **Browser flows.** A queue of scripted browser jobs, run by a runner you host.
  Every job an agent asks for needs its owner's approval.
  → [docs/browser-flows.md](docs/browser-flows.md)
- **Connectors.** Agents use stored HTTP credentials through muster's proxy and
  never see the secret. → [docs/connectors.md](docs/connectors.md)
- **Memo relay.** Delivers [cairn](https://github.com/ZacxDev/cairn) scope memos
  to dispatched agents. → [docs/memo-relay.md](docs/memo-relay.md)
- **CLI.** `muster`, a machine client for agents and scripts.
  → [docs/cli.md](docs/cli.md)

## Quickstart

You need Go (the version in `go.mod`), Docker and, to mirror a session, tmux.
The [Nix flake](flake.nix) provides the toolchain (`nix develop`).

**No Docker, no Nix, no root?** `./scripts/kbx-quickstart.sh setup && ./scripts/kbx-quickstart.sh start`
brings the whole thing up on a plain machine: it fetches a relocatable Postgres
that runs as an ordinary user, builds the binaries, migrates, mints a mirror
token, and starts the server and the host agent. Everything lands in one
directory you can delete (`~/.local/share/muster`). Mirroring a Claude Code
session needs it to run inside **tmux**; answering Claude Code's pickers from
the phone additionally needs **tmux 3.7+** (`paste-buffer -S`).

```sh
git clone https://github.com/ZacxDev/muster && cd muster
make test-db                       # a throwaway Postgres on 127.0.0.1:55432
docker compose -f docker-compose.test.yml exec postgres createdb -U muster muster_dev

export DATABASE_URL='postgres://muster:muster@127.0.0.1:55432/muster_dev?sslmode=disable'
export MUSTER_UI_PASSWORD='pick-a-password-of-12+-chars'
export MUSTER_STANDALONE=1         # no external permission router
export MUSTER_MIRROR=1             # turn mirrored sessions on
go run ./cmd/muster-server         # migrates, then listens on :8105 (all interfaces)
```

Open <http://127.0.0.1:8105> and sign in with the password. Then mirror your
tmux sessions. Run this in a second shell with the same `DATABASE_URL`:

```sh
go run ./cmd/muster-server mirror token create mybox > ~/.muster-mirror-token
go run ./cmd/muster-mirror -url http://127.0.0.1:8105 -allow-http \
  -host mybox -token-file ~/.muster-mirror-token -interactive
```

Your tmux windows now appear under **Agents**. Drop `-interactive` to make them
read-only. `-allow-http` is for local development only; a real deployment uses
https. When you are done, `make test-db-down` removes the database.

⚠ This quickstart listens on every interface and sets no `MUSTER_HOOK_TOKEN`, so
the machine API is open to anyone who can reach the port. Keep it on a trusted
machine, or set the token ([docs/auth.md](docs/auth.md#machine-credentials)).

The server prints one line per feature at boot, saying what is on, what is off,
and which variable changes it. Every variable is listed in
[docs/configuration.md](docs/configuration.md).

## Architecture

```mermaid
flowchart LR
  browser["Browser / installed PWA"] -->|HTTPS| server["muster-server"]
  cli["muster CLI · agents · scripts"] -->|JSON API + tokens| server
  server --> pg[("Postgres<br/>(row-level security)")]
  server -.->|"sign-in, passkeys (optional)"| gotrue["GoTrue"]
  host["muster-mirror<br/>on each of your machines<br/>(tmux, Claude Code)"] -->|"outbound WebSocket tunnel"| server
  server -.->|"provision (optional)"| k8s["Kubernetes<br/>agent pods"]
  server -.->|optional| stt["speech-to-text"]
  server -.->|optional| push["Web Push services"]
  server -.->|optional| cairn["cairn memo listener"]
```

The details are in [docs/architecture.md](docs/architecture.md): the binaries,
the listeners, and what each component trusts.

## Documentation

- [docs/README.md](docs/README.md) is the index.
- Reference: [configuration](docs/configuration.md), [CLI](docs/cli.md),
  [schema](docs/schema.md).
- Contributing: [testing](docs/testing.md), [CONTRIBUTING.md](CONTRIBUTING.md).
- Background: [limits](docs/limits.md), [design decisions and their history](docs/decisions.md).

## Status and limits

- **Run one replica.** Some background loops and the mirror tunnel keep state
  in memory, and nothing coordinates them across replicas.
- **Agent provisioning is opt-in and lightly proven.** The Kubernetes driver's
  tests use a fake clientset. No end-to-end turn against an agent this tree
  provisioned has been verified in-repo. This repository ships no Kubernetes
  manifests or RBAC.
- **Agent network isolation is address-based.** The `claude-code` agent's
  NetworkPolicy does not restrict egress by DNS name. An agent can still reach
  any public HTTPS host.
- **An interactive mirror is a shell on that host.** `-interactive` lets
  anyone who can use muster's Live keys switch for that host run commands as
  the user that `muster-mirror` runs as.

The full list is in [docs/limits.md](docs/limits.md).

## Licence

MIT. See [LICENSE](LICENSE).
