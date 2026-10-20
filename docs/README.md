# muster documentation

Start with the [project README](../README.md) for the pitch and a quickstart.

**Using it**

- [tasks.md](tasks.md) — the task board, statuses, tags, the reaper, GitHub
- [mirroring.md](mirroring.md) — mirrored tmux and Claude Code sessions
- [focus.md](focus.md) — what each session is working on, and how the chief finds one
- [attention.md](attention.md) — the attention queue: everything waiting on you, the agents' ask-and-wait tools, and answering an ask from your own mirrored session (`attention_answer`)
- [sending-and-step-up.md](sending-and-step-up.md) — the message box, and the step-up check that guards it
- [dictation.md](dictation.md) — the microphone on every message box
- [uploads.md](uploads.md) — attaching files in the composer; the signed link an agent downloads them through
- [push.md](push.md) — Web Push and the installable app
- [provisioner.md](provisioner.md) — dispatching agents, agent kinds, the chief, the provisioner seam
- [chief.md](chief.md) — the chief on each agent kind, and replacing it (runbook)
- [browser-flows.md](browser-flows.md) — the browser-job queue and its runner protocol
- [connectors.md](connectors.md) — credentials agents use without seeing them, and deciding an agent's request from your own mirrored session (`connector_request_decide`)
- [github-app.md](github-app.md) — the GitHub App: muster's GitHub identity, and one-repository tokens for agents
- [memo-relay.md](memo-relay.md) — cairn memos delivered to dispatched agents
- [plugins.md](plugins.md) — out-of-process plugins that bring messages to your chief agent
- [jobs.md](jobs.md) — the machine job API: an external orchestrator runs one prompt on a claude-code agent and reads the result back

**Running it**

- [architecture.md](architecture.md) — binaries, listeners, packages, trust
- [configuration.md](configuration.md) — every environment variable, checked by a test
- [auth.md](auth.md) — password or GoTrue sign-in, passkeys, invites, API tokens
- [cli.md](cli.md) — the `muster` CLI and the other binaries
- [schema.md](schema.md) — tables, migrations, row-level security, restore and rollback

**Working on it**

- [testing.md](testing.md) — the Go suite, the gates, the browser suite, screenshots
- [limits.md](limits.md) — what it does not do, or has not proven
- [decisions.md](decisions.md) — why things are the way they are
- [design/android.md](design/android.md) — proposed: a native Android app (actionable notifications, bubbles, widgets, live status)
- [design/hermes.md](design/hermes.md) — proposed: Hermes mirroring and the `hermes` agent kind
- [../CONTRIBUTING.md](../CONTRIBUTING.md)
