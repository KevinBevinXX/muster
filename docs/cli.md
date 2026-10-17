# The `muster` CLI

`cmd/muster` is a machine client for muster's JSON API, used by agents, scripts
and you. It builds as the `muster` binary.

## Verbs

The verb set is pinned in two places:

- `tests/verb-ledger.sh`, run by `make verb-ledger`, interrogates the built
  binary;
- `cmd/muster/tree_test.go` walks the command tree.

| group | verbs |
|---|---|
| tasks | `task ls`, `task get`, `task create`, `task comment`, `task status` |
| an agent's own task | `agent task get`, `agent task comment`, `agent task status` |
| agents | `agent ls`, `agent messages`, `agent resolve`, `chief ask` |
| browser flows | `flow enqueue`, `flow ls`, `flow status` |
| credentials | `token ls`, `token revoke`, `whoami` |
| session routing | `route reply` (a mirrored session's answer to a [routed message](plugins.md#routing-a-message-to-a-session)) |

Global flags: `--api-url`, `--token`, `--env-file` (default
`~/.muster/muster.env`) and `--timeout`.

## Configuration

The CLI looks for each setting in three places, from lowest to highest priority:

1. the env file;
2. the environment;
3. flags.

| setting | variable | flag |
|---|---|---|
| API URL (**no default**; it must be an absolute http(s) URL) | `MUSTER_API_URL` | `--api-url` |
| credential | `MUSTER_TOKEN` (older name: `MUSTER_HOOK_TOKEN`; `MUSTER_TOKEN` wins within one source) | `--token` |

The usual credential is a **personal API token** (`mst_…`). Mint it in the
browser at `/account/tokens`; it acts as you ([auth.md](auth.md)). Tokens cannot
be minted from the CLI. `muster token ls` and `muster token revoke <id>` manage
them, and `muster whoami` says who a credential acts as.

## Installing it from elsewhere

This repository is public, so the CLI can be fetched and pinned by revision. In
a downstream flake, add `muster.url = "github:ZacxDev/muster"` and use
`muster.packages.${system}.muster-cli`.

- **The output is `muster-cli`, and the binary is `muster`.** The output is not
  named `muster`, because that name belongs to the service.
- **The binary answers to whatever name you invoke it as.** It uses `argv[0]` for
  its help text, its error prefix and its version-skew note. So you can symlink
  it under another name, and nothing else changes.

```sh
nix run github:ZacxDev/muster#muster-cli -- task ls
go build -o muster ./cmd/muster         # or plain Go
```

## Other binaries

| binary | what it is |
|---|---|
| `muster-server` | The service. Subcommands: `mirror token create <host>`, `mirror token ls`, `mirror token revoke <id>`, `vapid generate`, `invite --claim-admin`, `user disable <id>`, `user enable <id>`. |
| `muster-migrate` | Applies the schema to `DATABASE_URL`. The server also migrates itself at boot. `--rls-off` and `--rls-on` are the row-level-security rollback tools ([schema.md](schema.md)). |
| `muster-mirror` | The host agent for [mirrored sessions](mirroring.md). |
| `ccd` | The supervisor inside the `claude-code` agent image ([provisioner.md](provisioner.md)). |

The container image (`Dockerfile`) ships `/muster-server`, `/muster-migrate` and
`/agent-cli/muster`. The flake builds `muster-cli`, `muster-migrate`,
`muster-mirror` and a migrator image (`migrate-image`).
