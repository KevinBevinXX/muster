# Configuration

Every muster binary is configured through environment variables. This page lists
**every variable any Go code in this repository reads**, grouped by the binary
that reads it.

The list is enforced. `TestEveryEnvVarTheCodeReadsIsDocumentedAndViceVersa`
(in `internal/modulegate/envdoc_test.go`) parses the module's Go source, collects
every variable name it reads, and compares that set with the first column of the
tables below. It fails if the code reads a variable that has no row here, and if
a row names a variable that nothing reads.

What that test covers:

- All non-test `.go` files in the module, including build-tagged test fixtures.
  Those fixture variables are listed under [Test and e2e harness](#test-and-e2e-harness).
- Variables read in `_test.go` files are **not** included. They configure tests,
  not anything that ships. The Makefile's `test-liveenv` target documents its own.
- Non-Go code is **not** included: the Playwright harness's `MUSTER_E2E_*`
  variables (`e2e/lib/env.ts`), the Dockerfile and the Makefile.

How values are parsed:

- **Booleans.** In `muster-server`, a boolean is on only for `1`, `true`, `yes`
  or `on` (case-insensitive). Anything else, including a typo, is off. In
  `muster-mirror`, a boolean is on for `1` or `true`.
- **Durations** use Go syntax: `90s`, `30m`, `12h`.
- **Keys** are 32 random bytes, base64-encoded. One way to make one:
  `head -c32 /dev/urandom | base64`.

The server checks its configuration at boot and refuses to start on an invalid
or contradictory combination. After it starts, it logs one line per feature
saying whether that feature is on, off or degraded, and why.

## muster-server: core

| variable | default | required | what it does |
|---|---|---|---|
| `DATABASE_URL` | none | yes | The Postgres DSN. Migrations run at boot. `muster-migrate` and the `muster-server mirror token …` subcommands read it too. |
| `MUSTER_PORT` | `8105` | no | The main listener. |
| `MUSTER_PUBLIC_URL` | none | for invites, passkeys and GitHub OAuth | The external base URL. It is used to build links (invites, the admin claim, the GitHub callback). |
| `MUSTER_STANDALONE` | off | one of this or the router | Declares that there is no permission router. If neither this nor a router is configured, `/readyz` answers 503 and names the missing setting. |
| `MUSTER_TASK_REAP_AFTER` | `168h` | no | How long a task can sit idle before the reaper tags it `stale`. The reaper runs every 30 minutes. |
| `MUSTER_SEND_QUEUE_IDLE` | `30s` | no | How long a mirrored Claude Code session must have been idle (by Claude Code's own `statusUpdatedAt`) before the next queued message is sent to it. Range: `1s`–`1h`. |
| `MUSTER_FOCUS_INTERVAL` | `15m` | no | How often the server recomputes each session's [focus](focus.md) with the summarizer. `0` turns the periodic pass off (the chief's `focus_refresh` still works). Range otherwise: `1m`–`24h`. |
| `MUSTER_FOCUS_RETENTION` | `168h` | no | How long a mirrored session's [focus](focus.md) is kept after it was last seen on any host. An hourly sweep deletes it, whether or not the refresh pass runs. A Claude session id is never reused, so a week unseen is a finished conversation (and still outlasts a laptop closed over a long weekend). Range: `1h`–`2160h`. |
| `MUSTER_TAG_AUTODISPATCH` | off | no | Reserved. When on, eligible tasks are logged and counted in a metric. Nothing is dispatched. |

## muster-server: sign-in and machine credentials

See [auth.md](auth.md).

| variable | default | required | what it does |
|---|---|---|---|
| `MUSTER_AUTH_MODE` | `password` | no | `password` means one shared operator password, acting as user 1. `gotrue` means per-user accounts through GoTrue. |
| `MUSTER_UI_PASSWORD` | none | in password mode | The operator password. If it is unset or shorter than 12 characters, every browser page answers 503. This fails closed. |
| `MUSTER_SECURE_COOKIES` | off | behind TLS | Marks the password-mode session cookie `Secure`. The cookies used in gotrue mode are always `Secure`. |
| `MUSTER_GOTRUE_URL` | none | in gotrue mode | The base URL of muster's GoTrue. Set it together with the secret, or set neither. |
| `MUSTER_GOTRUE_JWT_SECRET` | none | in gotrue mode | The HS256 secret GoTrue signs with. At least 32 characters. |
| `MUSTER_BROWSER_PORT` | none | in gotrue mode | A second listener, the only one that serves browser sessions in gotrue mode. On that mode's browser listener, the shared tokens below are refused with 403. |
| `MUSTER_HOOK_TOKEN` | none (machine routes open) | in gotrue mode | A shared machine secret that acts as user 1, the admin. It is enforced only when set. |
| `MUSTER_SERVICE_TOKEN` | none (routes answer 503) | no | A service-to-service secret that acts as user 1. Requests must also send an `X-Muster-Actor` header. At least 32 characters. |
| `MUSTER_STEPUP_WINDOW` | `12h` | no | How recent a sign-in must be before you can type into a mirrored session. Range: `1m`–`24h`. |

## muster-server: optional features

| variable | default | required | what it does |
|---|---|---|---|
| `MUSTER_MIRROR` | off | no | Turns on [mirrored sessions](mirroring.md). |
| `MUSTER_PLUGINS` | off | no | Turns on [plugins](plugins.md): owners mint plugin tokens on `/plugins`, and plugins post messages that reach the owner's chief agent. Needs a database. Off, every `/plugin/*` route and `/plugins` answer 404. |
| `MUSTER_PLUGIN_TASK_NAMESPACES` | none | no | Comma-separated tag namespaces a plugin may file tasks by, through [`POST /plugin/v1/tasks`](plugins.md#post-pluginv1tasks-one-task-per-upstream-record). Unset, that route answers 503. A namespace muster already uses (`gate`, `auto`, `initiative`, `project`, `superseded-by`) refuses to boot. |
| `MUSTER_SESSION_ROUTING` | off | no | `1` turns on [session routing](plugins.md#routing-a-message-to-a-session): the chief may hand a plugin message to one of its owner's sessions, and the session's answer becomes the reply. Needs `MUSTER_PLUGINS=1`. |
| `MUSTER_TASK_ROUTING` | off | no | `1` turns on [task routing](plugins.md#routing-a-task-to-the-chief): a plugin filing a task with `route_to_chief` hands its notice on by kind (an assignment to the owner's chief; an update to the chief only while a live agent works the task; a mention to that agent or the attention queue), which puts it in the attention queue (`task_route_attention`) or forwards it to the agent working it (`task_forward`). Needs `MUSTER_PLUGINS=1`. Off, `route_to_chief` is ignored and those tools are not offered. |
| `MUSTER_CHIEF_DISPATCH_OWNERS` | none | no | The user ids, comma-separated, who opted in to their chief starting agents. Non-empty (with `MUSTER_TASK_ROUTING=1`) offers the chief `task_dispatch`: start an agent on an open, ungated task, at most 5 of them live and 10 a day, once per task (again after you destroy the agent it started), for the listed owners only. Unset, `task_dispatch` is not offered. A value that is not a list of positive integers refuses to boot. |
| `MUSTER_CHIEF_SESSION_RUNTIMES` | none | no | The runtimes a [chief](chief.md#sessions-on-more-than-one-runtime) chat session may be started on, comma-separated: `claude-code` (a Claude Code CLI of its own, with the Claude model picked for it; needs `claude-code` in `MUSTER_AGENT_KINDS` and a claude-code chief) and `gateway` (an OpenRouter model, on the gateway runtime). Unset, there is no picker and every session is on the chief's own runtime. It does not turn the [default session](chief.md#sessions-on-more-than-one-runtime) on or off: plugin deliveries and `chief ask` go to the chief's default session either way. An unknown or repeated entry, or `claude-code` without that kind, refuses to boot. |
| `MUSTER_ROUTE_RELAY_ALLOW` | none | no | The senders whose routed answers are sent **without** the owner's approval, per user and per plugin: `<user id>:<plugin name>:<address>` or `<user id>:<plugin name>:*@<domain>`, comma-separated, ASCII addresses only. An entry matches only a message that plugin delivered **and** marked `sender_verified` ([plugins](plugins.md#sender_verified)). Everyone else's answer waits for approval like any reply draft. A value that does not parse (including the older `<user id>:<address>` form) refuses to boot. |
| `MUSTER_STT_URL` | none | with the token | The base URL (`http(s)://host[:port]`) of the speech-to-text service used for [dictation](dictation.md). |
| `MUSTER_STT_TOKEN` | none | no | The bearer token for that service. Setting it turns dictation on. A token without a URL refuses to boot. |
| `MUSTER_S3_ENDPOINT` | none | for [file attachments](uploads.md) | The S3-compatible store attached files are kept in, as `http(s)://host[:port]` (path-style; MinIO works). Attachments are on only when this, `MUSTER_S3_BUCKET`, both keys, `MUSTER_UPLOAD_SIGNING_KEY`, `MUSTER_PUBLIC_URL` and a database are all set. A partial or invalid set leaves them **off**, says why in the boot banner, and never refuses to boot. |
| `MUSTER_S3_BUCKET` | none | for file attachments | The bucket. Objects are written under `uploads/<random id>`. |
| `MUSTER_S3_ACCESS_KEY` | none | for file attachments | An access key that may `PutObject`, `GetObject`, `DeleteObject` and `AbortMultipartUpload` under `uploads/` in that bucket, plus `ListBucket`, `ListBucketMultipartUploads` and `GetLifecycleConfiguration` on the bucket ([policy](uploads.md#configuration)). |
| `MUSTER_S3_SECRET_KEY` | none | for file attachments | Its secret key. |
| `MUSTER_S3_REGION` | `us-east-1` | no | The signing region. |
| `MUSTER_UPLOAD_SIGNING_KEY` | none | for file attachments | At least 32 bytes. The key the links handed to agents are signed with (HMAC-SHA256). Rotating it ends every link already given out. |
| `MUSTER_UPLOAD_MAX_BYTES` | `26214400` (25 MiB) | no | The largest file that can be attached, in bytes (1 to 104857600). Files are streamed to the store, so this bounds the bucket, not memory. |
| `MUSTER_UPLOAD_LINK_TTL` | `24h` | no | How long a link handed to an agent works (a Go duration, `1m`–`168h`). |
| `MUSTER_VAPID_PUBLIC_KEY` | none | no | [Web push](push.md). The three VAPID variables are all-or-none. Generate a keypair with `muster-server vapid generate`. |
| `MUSTER_VAPID_PRIVATE_KEY` | none | no | The private half of that keypair. It must match the public key. |
| `MUSTER_VAPID_SUBJECT` | none | no | A `mailto:` or `https:` contact address that is sent to push services. |
| `MUSTER_ANDROID_ALLOW_PIN_KEYS` | off | no | `1` lets a phone with no strong biometric pair the [Android app](design/android.md#the-step-up-key) with a biometric-or-PIN step-up key. Off, such a phone pairs read-only. A biometric-or-PIN key survives a new fingerprint being enrolled, so anyone who knows the PIN can use it. |
| `MUSTER_PUSH_EXTRA_HOSTS` | none | no | Your own push hosts, such as a self-hosted ntfy for the [Android app](design/android.md). Paired phones may register on them; browsers may not. Exact host names, separated by commas or spaces: no scheme, port, path or wildcard. Each is reached on `https` and the default port only. A bad entry stops the server from starting. See [push.md](push.md#which-push-services). |
| `MUSTER_MEMO_URL` | none | with the token | The base URL of the cairn memo listener, for the [memo relay](memo-relay.md). |
| `MUSTER_MEMO_TOKEN` | none | no | A read-only cairn credential. The relay runs only while it is set, and also needs a database and an agent gateway. |
| `MUSTER_MEMO_SCOPE_MAP` | none | no | `repo=scope,scope;…`. Use it when a repository's memos are filed under other scope names. |
| `MUSTER_FLOW_AGENT_DENY_WORDS` | none (logs a warning) | no | Comma-separated names (your hosts, clusters or projects) that an agent may not put into [browser-flow](browser-flows.md) text, or into a [plugin](plugins.md) reply draft. |
| `MUSTER_CONNECTOR_ENCRYPTION_KEY` | none | no | A 32-byte base64 key. Setting it turns on the [connector store](connectors.md), and with it members' own stored credentials. |
| `MUSTER_CONNECT_PORT` | none | with connectors | A listener that serves only the agent-facing connector, approval and MCP routes. |
| `MUSTER_AGENT_CONNECT_URL` | none | with connectors | The URL agents are given (as `MUSTER_CONNECT_URL`) for reaching the connect port. |
| `MUSTER_GITHUB_APP_ID` | none | with the key and the accounts | The numeric App ID of a [GitHub App](github-app.md). Setting it, the key and the accounts makes the App the GitHub identity of muster's **admins**: their Repos tab and connected-repository checks list its installation repositories, and the `github` connector gives each approved agent of theirs a 1-hour token limited to its own repository (other repositories are reached anonymously; an agent with no repository gets no github access). Members keep their own OAuth connection. It needs `MUSTER_CONNECTOR_ENCRYPTION_KEY`, and takes precedence over the OAuth connection below. |
| `MUSTER_GITHUB_APP_PRIVATE_KEY` | none | with the ID | The App's private key: the PEM file GitHub generated, as the value (mount it from a Secret). A key that does not parse refuses to boot. |
| `MUSTER_GITHUB_APP_ACCOUNTS` | none | with the App | Comma-separated `<login>:<id>` entries, one per GitHub account (user or organization), e.g. `example-org:12345,your-user:67890`. Find an id with `gh api users/<login> -q .id`. Matching uses the **numeric id**, because a login can be renamed and then registered by someone else; the login is a label. An installation whose id matches under another login (a rename) is still used and logged. One whose login matches but whose id does not is ignored. Only the App's installations on these accounts are used; any other installation (anyone can install a public App) is ignored and logged once. Empty, an entry with no id, or an id listed twice refuses to boot. Only User and Organization accounts match (an Enterprise installation's id is from another numbering space). |
| `MUSTER_CHECKOUT_WAIT_TIMEOUT` | `24h` | no | How long a claude-code agent's first turn waits for its repository checkout (the owner's `github` approval) before the agent is marked error. A Go duration, `1h`–`720h`. It is measured from the agent's last status change, so raising it also extends agents already waiting. |
| `MUSTER_GITHUB_ENCRYPTION_KEY` | none | no | A 32-byte base64 key for storing a GitHub token (the OAuth connection, deprecated when a GitHub App is configured). Setting it enables the Repos tab. |
| `MUSTER_GITHUB_TOKEN` | none | no | Connects this GitHub token at boot (OAuth connection; deprecated when a GitHub App is configured). |
| `GITHUB_CLIENT_ID` | none | no | The GitHub OAuth app's client ID. Setting it and the secret enables `/github/connect`. Deprecated: a [GitHub App](github-app.md) replaces it. |
| `GITHUB_CLIENT_SECRET` | none | no | The GitHub OAuth app's client secret. |
| `MUSTER_FARO_URL` | none | no | A Grafana Faro collector URL. Setting it turns on front-end real-user monitoring. |
| `MUSTER_FARO_API_KEY` | none | no | The Faro API key. It is sent to the browser, so it is public. |
| `MUSTER_FARO_TRACING` | off | no | Turns on Faro tracing as well. |

## muster-server: permission router and approvals

muster can also run beside a separate permission router: a service that records
Claude Code sessions and routes their permission prompts. That router is not
part of this repository. Without one, set `MUSTER_STANDALONE=1`.

| variable | default | required | what it does |
|---|---|---|---|
| `MUSTER_ROUTER_URL` | none | no | The router's base URL. It provides transcript links, notifications and the directory picker. It refuses to boot together with `MUSTER_STANDALONE`. |
| `MUSTER_ROUTER_TOKEN` | none | with the URL | The router's credential. |
| `MUSTER_ROUTER_ACTOR` | `muster` | no | The actor name muster sends to the router. |
| `MUSTER_APPROVAL_GATE_URL` | none | no | The router's gate endpoint. Agents' permission prompts are relayed there. Unset, a prompt falls back to the agent's own terminal. |
| `MUSTER_APPROVAL_GATE_TOKEN` | none | with the URL | The credential for that gate. |

## muster-server: agent provisioning

Provisioning is off by default. See [provisioner.md](provisioner.md) for what
each setting does and which combinations are valid.

| variable | default | required | what it does |
|---|---|---|---|
| `MUSTER_AGENT_PROVISIONER` | `none` | to dispatch | `none`, `noop` (records specs and creates nothing), or `kubernetes` (uses in-cluster credentials). |
| `MUSTER_AGENT_GATEWAY` | `none` | to chat / kick off | `hooks-sha256`. Turns on agent chat and first-turn delivery. |
| `MUSTER_AGENT_GATEWAY_MODEL` | none | with a gateway | The value the agent runtime expects in the wire `model` field. |
| `MUSTER_AGENT_GATEWAY_PORT` | `18789` | no | The port the agent's gateway listens on. Each instance's Service declares it. |
| `MUSTER_AGENT_RUNTIME_CONFIG` | none | with a gateway | The agent runtime's config file contents. |
| `MUSTER_AGENT_RUNTIME_INSTALL` | none | with a gateway | The script that installs that config file in the instance. |
| `MUSTER_AGENT_IMAGE_REPO` | none | with a provisioner | The image repository for `gateway`-kind agents. |
| `MUSTER_AGENT_IMAGE_TAG` | `latest` | no | The tag for that image. |
| `MUSTER_AGENT_API_URL` | none | with a provisioner | The muster URL agents use to call back. |
| `MUSTER_AGENT_MODEL` | none (the runtime decides) | no | The default model for agents. |
| `MUSTER_AGENT_OPENROUTER_KEY` | none | no | The admin's OpenRouter key for agents. Members bring their own keys. |
| `MUSTER_AGENT_NAMESPACE_PREFIX` | `devpod-` | no | Each agent's namespace is this prefix plus the agent name. |
| `MUSTER_AGENT_NAMESPACE_SHARED` | off | no | Puts every agent in `MUSTER_AGENT_NAMESPACE` instead of its own namespace. |
| `MUSTER_AGENT_NAMESPACE` | none | when shared | The shared namespace. |
| `MUSTER_AGENT_WORKSPACE_PERSIST` | off | no | Gives each agent a workspace volume that survives restarts. |
| `MUSTER_AGENT_WORKSPACE_STORAGE_CLASS` | the cluster default | no | The StorageClass for that volume. |
| `MUSTER_AGENT_ENDPOINT_TEMPLATE` | the driver's (`{{.Name}}.{{.Group}}.svc` on Kubernetes) | no | How muster builds an agent's address from its name and group. |
| `MUSTER_AGENT_CAIRN_URL` | none | no | Cairn coordinates given to the chief only. Set it together with the token, or set neither. |
| `MUSTER_AGENT_CAIRN_TOKEN` | none | no | The credential for that cairn. |
| `MUSTER_AGENT_KINDS` | `gateway` | no | A comma list of the agent kinds a dispatch may ask for: `gateway`, `claude-code`, `hermes`. |
| `MUSTER_AGENT_CC_IMAGE` | none | with `claude-code` | The pinned image for `claude-code` agents, built from `images/claude-code-agent`. |
| `MUSTER_AGENT_CC_ACCOUNTS` | none | with `claude-code` | A comma list of Claude account names (`[a-z0-9-]`, 1–32 characters each). |
| `MUSTER_AGENT_CC_TOKEN_<NAME>` | none | for each account | That account's `claude setup-token`. `<NAME>` is the account name upper-cased, with `-` written as `_`. |
| `MUSTER_AGENT_CC_STORAGE_SIZE` | `10Gi` | no | The size of each `claude-code` agent's `/data` volume. |
| `MUSTER_AGENT_HERMES_IMAGE` | none | with `hermes` | The pinned image for `hermes` agents, built from `images/hermes-agent` (`make hermes-image`). Its provider credential is the OpenRouter key (`MUSTER_AGENT_OPENROUTER_KEY`, or a member's own); it has none of its own. |
| `MUSTER_AGENT_HERMES_STORAGE_SIZE` | `10Gi` | no | The size of each `hermes` agent's `/data` volume. 🔴 Its storage class must give a **local filesystem**, not NFS or virtiofs: Hermes's `state.db` is SQLite in WAL mode. The pod's root filesystem is read-only (a writable `/tmp` emptyDir is mounted). |
| `MUSTER_AGENT_NETPOL_FROM_NAMESPACE` | none | `claude-code` or `hermes` on Kubernetes | The namespace muster runs in. The agent's NetworkPolicy only admits muster. |
| `MUSTER_AGENT_NETPOL_FROM_POD_LABELS` | none | `claude-code` or `hermes` on Kubernetes | Comma-separated `key=value` labels on muster's pods. |

The install script receives each `gateway`-kind agent's settings as JSON in
`MUSTER_CONFIG`. It is the script's job to copy them into the runtime's own
configuration:

| key | value | the runtime's setting |
|---|---|---|
| `workspace` | always: the agent's workspace (`/data/workspace` by default), where muster puts its `AGENTS.md` | the agent workspace (`agents.defaults.workspace`). Without it the runtime uses a directory under its `HOME` and never reads that `AGENTS.md`. |
| `model` | only when a model is named: `{"primary": …, "fallbacks": […]}` | the default model (`agents.defaults.model`) |

For example, in a `jq` filter that reads `MUSTER_CONFIG` as `$mc`:
`| if ($mc[0].workspace // null) then .agents.defaults.workspace = $mc[0].workspace else . end`.

## muster-mirror (host agent)

Each setting is also a flag (`-url`, `-token-file`, …). See
[mirroring.md](mirroring.md).

| variable | default | required | what it does |
|---|---|---|---|
| `MUSTER_MIRROR_URL` | none | yes, except with `-list` | The muster base URL to connect to. |
| `MUSTER_MIRROR_TOKEN_FILE` | none | this or the token | A file holding this host's mirror token. If both are set, the file wins. |
| `MUSTER_MIRROR_TOKEN` | none | this or the file | The mirror token itself. |
| `MUSTER_MIRROR_HOST` | the short hostname | no | This host's name. It must match the name the token was minted for. |
| `MUSTER_MIRROR_INTERACTIVE` | off | no | Allows typing and sending messages. The default is read-only. |
| `MUSTER_MIRROR_HERMES_SEND` | off | no | With `MUSTER_MIRROR_INTERACTIVE`: lets muster send to Hermes windows, through the gated send (screen check, Escapes, a database-confirmed submit; [hermes.md](design/hermes.md)). Turn it on per host only after `make test-livehermes` is green against that host's Hermes, and **never** on a host whose Hermes uses vi mode (`display.vim_mode`). |
| `MUSTER_MIRROR_TMUX_SOCKET` | tmux's default | no | Uses a different tmux server: `tmux -L <name>`. |
| `MUSTER_MIRROR_TMUX_SOCKET_PATH` | none | no | `tmux -S <path>`. It wins over the socket name. |
| `MUSTER_MIRROR_LABELS_FILE` | none | no | A JSON object `{"<session>": "<codename>"}` of display labels. |
| `MUSTER_MIRROR_PANE_MAP_DIR` | none | no | A directory of per-pane Claude Code records, kept current by a hook. |
| `MUSTER_MIRROR_SNIPPETS_FILE` | `$XDG_CONFIG_HOME/muster-mirror/snippets.json` | no | A JSON array of `{trigger, label, body}`: the prewritten messages the Rendered view's message bar offers after `:`. Re-read when it changes; missing or invalid = none. |
| `MUSTER_MIRROR_MCP_LISTEN` | none (off) | no | A loopback address (`127.0.0.1:<port>` or `[::1]:<port>`) on which to serve this host's Claude Code sessions muster's attention and task tools as an MCP endpoint (`http://<addr>/mcp`); any other address is refused. Optional: if it cannot start, the host logs why and mirrors without it. See [attention.md](attention.md#mirrored-claude-code-sessions). |
| `MUSTER_MIRROR_MCP_SECRET_FILE` | `$XDG_CONFIG_HOME/muster-mirror/mcp-secret` | no | The local MCP endpoint's secret, which every call must present. Created (0600, in a 0700 directory) when missing; a file readable by others is refused. |
| `XDG_CONFIG_HOME` | `~/.config` | no | Where the default snippets file is looked for. |
| `MUSTER_MIRROR_ALLOW_HTTP` | off | no | Allows a plain-`http` muster URL. Use it for tests only. |
| `MUSTER_MIRROR_WRITE_HOOK_CONFIG` | off | no | Set it on the host's managed `muster-mirror` service only (or pass `-write-hook-config`). It makes the daemon record its URL and token file path for `muster-mirror ask` when it starts. |
| `MUSTER_MIRROR_HOOK_CONFIG` | `~/.muster/mirror-hook.json` | no | Where `muster-mirror` records its URL and token file path (never the token) for `muster-mirror ask`, and where the hook reads them. |
| `MUSTER_MIRROR_HERMES_HOME` | `$HERMES_HOME`, else `~/.hermes` | no | The Hermes home a Hermes window's session is resolved from, read-only: only `state.db`, `terminal-sessions/tty-*` and `shell-hooks-allowlist.json`. Also where `muster-mirror hermes-allowlist` writes. See [mirroring.md](mirroring.md#hermes-windows). |
| `HERMES_HOME` | `~/.hermes` | no | Hermes's own home variable, the default of `MUSTER_MIRROR_HERMES_HOME`. |
| `MUSTER_MIRROR_HERMES_STATUS_DIR` | `~/.muster/hermes-status` | no | Where `muster-mirror hermes-hook` writes one status file per Hermes session (0700 directory, 0600 files), and where the daemon reads them. Recorded for the hook with `-write-hook-config`. |
| `CLAUDE_CONFIG_DIR` | `~/.claude` | no | Claude Code's config directory, where `sessions/` and `projects/` are read from, and the user's `skills/`, `commands/` and enabled plugins the composer's `/` menu lists. `ccd` reads it too. |

## muster (CLI)

See [cli.md](cli.md). The CLI looks for each setting in three places, from
lowest to highest priority: `~/.muster/muster.env`, then the environment, then
flags.

| variable | default | required | what it does |
|---|---|---|---|
| `MUSTER_API_URL` | none | yes | The muster base URL. It has no default. |
| `MUSTER_TOKEN` | none | yes | A personal API token (`mst_…`), or a shared machine secret. |
| `HOME` | the system's | no | Used to find `~/.muster/muster.env`. |
| `CLAUDE_CODE_SESSION_ID` | none | no | Sent with task writes as the session that made them. |
| `CLAUDE_SESSION_ID` | none | no | A fallback spelling of the variable above. |

`MUSTER_HOOK_TOKEN` is also accepted as an older name for `MUSTER_TOKEN`. If both
are set in the same place, `MUSTER_TOKEN` wins.

## ccd (inside the claude-code agent image)

`ccd` runs inside the `claude-code` agent container. muster sets these
variables when it provisions an agent, so you normally never set them yourself.

| variable | default | required | what it does |
|---|---|---|---|
| `MUSTER_GATEWAY_BEARER` | none | this or `HOOKS_TOKEN` | The bearer token `ccd`'s `/v1/responses` accepts. |
| `HOOKS_TOKEN` | none | this or the bearer | The token the bearer is derived from. If both are set, they must agree. |
| `CCD_GATEWAY_UPSTREAM` | none | no | The gateway runtime beside `ccd` (a `claude-code` chief's sidecar), on loopback. |
| `CCD_GATEWAY_UPSTREAM_BEARER` | none | with `CCD_GATEWAY_UPSTREAM` | That runtime's own credential, which `ccd` presents when it hands a gateway turn on. It is never `ccd`'s bearer. Without it, `ccd` refuses gateway turns. |
| `MUSTER_CONNECT_URL` | none | no | muster's connect listener. `ccd approve` and `ccd connect-git` use it. |
| `CLAUDE_CODE_OAUTH_TOKEN` | none | this or a `/login` file | The Claude credential, taken from the agent's Secret. `ccd` treats either one as "has a credential". |
| `CCD_LISTEN` | `:18789` | no | The gateway address. |
| `CCD_HOOK_SOCKET` | none | no | Set by `ccd` itself in the environment of each Claude Code CLI it starts: the unix socket its hook listener is on, in a directory only `ccd`'s user can enter. `ccd hook` posts each hook event there. Not for an operator to set. |
| `CCD_HOOK_SECRET` | none | no | Set by `ccd` itself beside `CCD_HOOK_SOCKET`: the per-process credential every hook must carry. A hook without it is refused and changes nothing. It keeps other containers in the pod out; it does not stop a process of `ccd`'s own user (every tool the CLI runs inherits it). Not for an operator to set. |
| `CCD_HOOK_DISABLED` | off | no | With `1`, `ccd hook` does nothing. |
| `CCD_HOOK_LOG` | none | no | Appends every raw hook payload to this file, for debugging. It includes prompt text. |
| `CCD_TMUX_SOCKET` | tmux's default | no | `tmux -L` socket name. |
| `CCD_TMUX_CONF` | tmux's default | no | `tmux -f` config file. |
| `CCD_TMUX_TARGET` | `cc` | no | The tmux session the CLI runs in. |
| `CCD_WORKSPACE` | the current directory | no | The CLI's working directory. muster sets it to the checkout (`/data/workspace/<name>`) for an agent dispatched with a repository, and leaves the image's `/data/workspace` otherwise. |
| `CCD_REPO` | none | no | `owner/name`. `ccd` clones `https://github.com/<it>` into `CCD_WORKSPACE` and starts the CLI only once the checkout exists ([provisioner.md](provisioner.md#a-claude-code-agents-repository)). muster sets it for an agent dispatched with a repository. |
| `CCD_REPO_BRANCH` | the remote's default | no | The branch `ccd` clones. |
| `CCD_SUPERVISE` | off | no | With `1`, `ccd` creates the tmux session and keeps `claude` running in it. |
| `CCD_CLAUDE_BIN` | `claude` | no | The path to the Claude Code CLI. |
| `CCD_GATEWAY_UPSTREAM` | none | no | Where the gateway runtime beside `ccd` listens, as `http://127.0.0.1:<port>` (loopback only; muster sets it for a `claude-code` chief whose sessions may use the `gateway` runtime). A turn carrying `X-Muster-Runtime: gateway` is handed to it after `ccd` checks the bearer. Unset, such a turn is `not_ready`. |
| `CCD_LANE` | none | no | Set by `ccd` itself in the environment of each extra Claude Code CLI it starts (one per chief session on the `claude-code` runtime), so `ccd hook` reports that CLI's hooks as that session's. Not for an operator to set. |
| `CCD_MAX_SESSIONS` | `3` | no | How many Claude Code CLIs `ccd` runs **beside** the original one, one per chief session started on the `claude-code` runtime (each its own tmux session `cc-<id>`, started with that session's `--model`). At the cap, the least recently used idle one is stopped; its conversation resumes on its next turn. When all are mid-turn, a new session's turn is refused with a typed `capacity` 503. `0` turns this off: such a turn is `not_ready`. Each CLI costs roughly 250–550 MiB of the pod's 3 GiB. |
| `CCD_SUBMIT_TIMEOUT` | `20s` | no | How long to wait for a pasted prompt to be accepted. |
| `CCD_TURN_TIMEOUT` | `30m` | no | The deadline for a whole turn. |
| `CCD_APPROVAL_DEADLINE` | `170s` | no | How long `ccd approve` waits for a relayed decision before falling back to the terminal prompt. |
| `CCD_LIVECHECK_HOOKS_TOKEN` | none | for `livecheck` | The token for `cmd/ccd/livecheck`, a manual probe of a live agent. |

## hermesd (inside the hermes agent image)

`hermesd` runs inside the `hermes` agent container, in front of one `hermes
gateway run` ([design](design/hermes.md#the-hermes-agent-kind)). muster sets these
variables when it provisions a `hermes` agent, so you normally never set them
yourself. `MUSTER_GATEWAY_BEARER` and `HOOKS_TOKEN` are read as `ccd` reads
them (above), and are not passed on to Hermes.

| variable | default | required | what it does |
|---|---|---|---|
| `HERMES_HOME` | `/data/hermes` | no | Hermes's home on the volume. `hermesd` creates it, the agent shell's HOME (`$HERMES_HOME/home`) and `TMPDIR` (`$HERMES_HOME/cache/tmp`), renders `config.yaml` there on every start, and refuses to start Hermes while a `.env` exists there. |
| `HERMESD_WORKSPACE` | `/data/workspace` | no | The agent's working directory: the rendered `terminal.cwd`. muster sets it to the checkout (`/data/workspace/<name>`) for an agent dispatched with a repository. |
| `HERMESD_REPO` | none | no | `owner/name`. `hermesd` clones `https://github.com/<it>` into `HERMESD_WORKSPACE` before it starts Hermes, as `ccd` does with `CCD_REPO`. |
| `HERMESD_REPO_BRANCH` | the remote's default | no | The branch `hermesd` clones. |
| `HERMESD_INSTRUCTIONS` | `/etc/hermesd/instructions.md` | no | muster's instructions, sent as `instructions` on every Hermes run. muster places the file outside every volume. An absent file is no instructions. |
| `HERMESD_MODEL` | none | no | The OpenRouter model slug Hermes runs. muster sets it from the agent's model or `MUSTER_AGENT_MODEL`. |
| `HERMESD_MODEL_BASE_URL` | none | no | **Test-only.** An OpenAI-compatible base URL used instead of OpenRouter, so the image smoke test runs the production image against a stub model. Its key is still `${OPENROUTER_API_KEY}`, expanded by Hermes. muster never sets it, and `hermesd` logs it at boot whenever it is set. |
| `HERMESD_LISTEN` | `:18789` | no | muster's agent wire. |
| `HERMESD_API_PORT` | `8642` | no | Hermes's API server, bound to `127.0.0.1` only. |
| `HERMESD_RELAY_TIMEOUT` | `240s` | no | How long an approval waits for muster's relay before `hermesd` answers `deny` itself. It must be shorter than the rendered `approvals.timeout` (300 s); `hermesd` refuses to start otherwise. |

Fixed, not configurable: `hermesd`'s own state is `/data/hermesd` (the API server
key, generated once, mode 0600, and the session map with its intents); the
gateway is the image's `hermes`; a turn's budget is 30 minutes, after which
`hermesd` stops the run (`POST /v1/runs/{id}/stop`) and answers `turn_timeout`.

## Test and e2e harness

These variables are read by non-test code that exists only for testing. That
code is `internal/dbtest`, plus binaries built with the `e2eterm` or `e2emirror`
build tag, which the Playwright suite uses. The shipped binaries do not read them.

| variable | default | required | what it does |
|---|---|---|---|
| `MUSTER_TEST_DATABASE_URL` | none (DB tests skip) | for store tests | The Postgres template database for the Go suite. See [testing.md](testing.md). |
| `MUSTER_TEST_REQUIRE_DB` | off | in CI | Turns a missing database into a failure instead of a skip. |
| `MUSTER_E2E_SUB` | none | for `e2e/mkmember` | A GoTrue user id for the e2e member fixture. |
| `MUSTER_E2E_EMAIL` | none | for `e2e/mkmember` | That fixture user's email. |
| `MUSTER_E2E_AGENT` | none | for `e2e/mkchat` | The name of the agent the mirror-chat spec's chat fixture creates (or deletes). |
| `MUSTER_E2E_DELETE` | off | no | (`e2e/mkchat`) `1` deletes that agent instead of creating it. |
| `MUSTER_E2E_FOCUS` | none | no | (`e2e/mkchat`) A JSON array of `{subject, title, description, history}` (or `{subject, error}`, a failed refresh's reason; `history` writes that many earlier focuses first, for the focus history): session focus to write for the agent it creates (`"self"`) or for a mirrored session (`"mirror:<claude session id>"`), used by the focus spec. Deleting also deletes the mirrored ones it names. |
| `MUSTER_E2E_DISPLAY_NAME` | none | no | (`e2e/mkchat`) The agent's display name; `chief` makes it the chief panel's agent. |
| `MUSTER_E2E_ACTIVE_MINUTES_AGO` | none | no | (`e2e/mkchat`) Backdates the agent's activity (its row's `created_at`/`updated_at` and every message) to that many minutes ago, so a spec knows where it sorts on the Agents list. |
| `MUSTER_E2E_TASK` | none | no | (`e2e/mkchat`) Also creates a `ready_for_review` task with this title as the agent's source task (its page then shows Mark complete). Deleting with it set deletes that task too. |
| `MUSTER_E2E_THREADS` | off | no | (`e2e/mkchat`) `1` reads several threads (title, age, messages) from stdin instead of one message list. |
| `MUSTER_E2E_KIND` | the default kind | no | (`e2e/mkchat`) The agent's kind, e.g. `claude-code`. |
| `MUSTER_E2E_CC_ACCOUNT` | none | no | (`e2e/mkchat`) The Claude account a `claude-code` agent names. Only a name: nothing is provisioned. |
| `MUSTER_E2E_HOLD_READY_DIR` | none | no | (`e2eterm`) Holds fake instances not-ready until a file named after each instance appears in this directory. |
| `MUSTER_E2E_TERM_WINDOW` | none | no | (`e2eterm`) The `CxR` window size the fake agent pod reports. |
| `MUSTER_E2E_GITHUB_API` | none | no | (`e2eterm`) Points every GitHub API call at this base URL (a fake GitHub), so the dispatch dialog's repo picker can be driven with fixture repos. |
| `MUSTER_E2E_OPENROUTER_MODELS` | none | no | (`e2eterm`) Points the model search at this URL instead of OpenRouter's public model list (a fake catalogue). |
| `MUSTER_E2E_AGENT_ENDPOINT` | none | no | (`e2eterm`) Points every agent's gateway endpoint at this loopback URL (`http://127.0.0.1:<port>`, anything else refused): the answer-delivery spec's fake runtime. |
| `MUSTER_E2E_GOTRUE_SKEW` | none (`30s`, `gotrue.DefaultSkew`) | no | (`e2eterm`) The GoTrue access-token verifier's clock skew, a Go duration, so the specs that outlive an access token wait seconds instead of 35 s. |
| `MUSTER_MIRROR_E2E_REFRESH` | none (`5s`) | no | (`e2emirror`) How often the fake host re-reads its windows (`mirrorhost.Config.Refresh`), a Go duration. |
| `MUSTER_MIRROR_E2E_SESSIONS` | none | no | (`e2emirror`) The fake tmux server's session names, comma-separated. |
| `MUSTER_MIRROR_E2E_WINDOWS_FILE` | none | no | (`e2emirror`) A per-window fake. Each line is `session\|@id\|index\|active\|command\|activity\|transcript`. |
| `MUSTER_MIRROR_E2E_HERMES_HOME` | none | no | (`e2emirror`) A scratch Hermes home: the fake makes `state.db` there from Hermes's pinned schema and inserts each new line of `rows.jsonl` as a message, as Hermes's gateway would. |
| `MUSTER_MIRROR_E2E_HERMES_WINDOWS_FILE` | none | no | (`e2emirror`) Hermes windows added to a per-window fake. Each line is `session\|@id\|index\|active\|activity\|hermes session id\|status`; an empty session id is a window with no rendered view. |
| `MUSTER_MIRROR_E2E_CLAUDE` | none | no | (`e2emirror`) `<session>=<transcript path>`: plays that session as a Claude Code pane. |
| `MUSTER_MIRROR_E2E_NO_PROMPT_INDEX` | unset | no | (`e2emirror`) `1`: the host sends no prompt index and no prompt places, as one that predates them (`mirrorhost.Config.NoPromptIndex`). |
| `MUSTER_MIRROR_E2E_NO_CATALOG` | unset | no | (`e2emirror`) `1`: the host advertises no composer-menu catalog and answers no request, as one that predates it (`mirrorhost.Config.NoCatalog`). |
| `MUSTER_MIRROR_E2E_NO_SUBAGENTS` | unset | no | (`e2emirror`) `1`: the host advertises no subagent reporting — no subagent summaries on a view's stream, no tail request answered — as one that predates the subagent cards (`mirrorhost.Config.NoSubagents`). |
| `MUSTER_MIRROR_E2E_CWD` | unset | no | (`e2emirror`) The working directory of every window of the per-window fake: where its project `.claude` is found. |
| `MUSTER_MIRROR_E2E_PASTE_FILE` | none | no | (`e2emirror`) Where pastes into non-Claude panes are recorded. |
| `MUSTER_MIRROR_E2E_ACTIVE_FILE` | none | no | (`e2emirror`) `session=index` lines: each session's active window. |
| `MUSTER_MIRROR_E2E_ACTIVITY_FILE` | none | no | (`e2emirror`) `session=<unix seconds>` lines: each session's last activity. |
| `MUSTER_MIRROR_E2E_WINDOW` | none | no | (`e2emirror`) The `CxR` window size the fake reports. |
| `MUSTER_MIRROR_E2E_SCROLL_FILE` | none | no | (`e2emirror`) Turns on copy-mode scrolling and records each scroll (`<pane> <lines> pos=<n>`) and cancel (`<pane> cancel`). While `<file>.mode` exists every pane is in another mode, and a scroll is refused as one. Unset, the fake is an older host that cannot scroll. |
| `MUSTER_MIRROR_E2E_NO_STATUS` | none | no | (`e2emirror`) `1`: the per-window fake does not advertise Claude Code status, as an older muster-mirror does. (Its status and time otherwise come from `<transcript>.status`, `{"status", "statusUpdatedAt"}`; without that file it reports a status with no time.) |
