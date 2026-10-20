# Agents: dispatch, kinds, the chief, and the provisioner

muster can start a coding agent against a task, talk to it, and stop it. **All of
this is opt-in and off by default.** On a default deployment, every agent-control
route is registered but answers **503**, with `"provisionerUnwired": true` in the
body. The boot banner says `agent provisioning: UNWIRED`.

The Agents list still works without it: it shows mirrored sessions
([mirroring.md](mirroring.md)) and any agent rows already stored.

## The two switches

| switch | values | what it turns on |
|---|---|---|
| `MUSTER_AGENT_PROVISIONER` | `none` (default), `noop`, `kubernetes` | **Lifecycle:** dispatch, start, stop, destroy, logs, terminal. |
| `MUSTER_AGENT_GATEWAY` | `none` (default), `hooks-sha256` | **Chat:** messages to a running agent, plus delivery of the first turn (the "kickoff"). |

`noop` records specs and creates nothing. Use it to develop everything above the
provisioner without a cluster. `kubernetes` uses **in-cluster credentials only**.

The boot checks, all refused at boot rather than at the first dispatch:

- Any provisioner other than `none` requires `MUSTER_AGENT_IMAGE_REPO` and
  `MUSTER_AGENT_API_URL`.
- A gateway requires a provisioner, plus `MUSTER_AGENT_GATEWAY_MODEL`,
  `MUSTER_AGENT_RUNTIME_CONFIG` and `MUSTER_AGENT_RUNTIME_INSTALL`.
- The runtime-config pair set without a gateway also refuses to boot.

What happens when you dispatch:

- **No provisioner:** 503.
- **A provisioner but no gateway:** a dispatch that asks for a kickoff is refused
  with **409**, and nothing is created. "Save for later" followed by Start still
  works, but the agent is never told what to do.
- **Both:** the agent is created. A background deliverer then sends the task as
  its first turn once the instance is ready, every 10 seconds, at most once per
  agent. A failed kickoff is shown on the card, with the error.

### What a Kubernetes deployment needs that this repository does not ship

- **RBAC for muster's ServiceAccount:** create, read, update and delete on
  Namespaces, ServiceAccounts, Secrets, ConfigMaps, Deployments and Services;
  PersistentVolumeClaims when an agent gets a volume; `create` on `pods/exec`;
  and NetworkPolicies when `claude-code` is enabled. The Kubernetes driver's
  `render.go` is the authority on what it creates.
- **The muster Deployment itself:** this repository ships a container image
  definition (`Dockerfile`), but no manifests.

## Agent kinds

`MUSTER_AGENT_KINDS` is a comma list of `gateway`, `claude-code` and `hermes`. It
defaults to `gateway`, and `gateway` must always be in it.

- **`gateway`** runs your agent image (`MUSTER_AGENT_IMAGE_REPO:TAG`), which
  serves an HTTP chat gateway. muster derives the bearer token the pod accepts as
  `sha256("gw-" + <the agent's token>)`.
- **`claude-code`** runs the image built from
  [`images/claude-code-agent`](../images/claude-code-agent): the Claude Code CLI
  in tmux, supervised by `ccd` ([`cmd/ccd`](../cmd/ccd)).
  - `ccd` serves `POST /v1/responses` on port 18789. It pastes each prompt as a
    bracketed paste and reads the reply from the transcript.
  - The image refuses to start if any API-key-style Anthropic variable is set,
    so it can only use a Claude subscription token.
  - It requires `MUSTER_AGENT_CC_IMAGE`, pinned to a tag or digest.
  - It requires `MUSTER_AGENT_CC_ACCOUNTS`, plus one
    `MUSTER_AGENT_CC_TOKEN_<NAME>` per account.
  - On Kubernetes it requires both `MUSTER_AGENT_NETPOL_FROM_*` variables.

  **The account pool.** Each new agent is assigned an account: first excluding
  accounts marked `auth_failed`, then the least recently rate-limited, then the
  one with the fewest live agents. A pin at dispatch overrides this. An agent
  keeps its account for life. The token goes only into that agent's own Secret,
  as `CLAUDE_CODE_OAUTH_TOKEN`. The pool configured in the environment belongs
  to the admin; members use credentials they stored themselves
  ([auth.md](auth.md)).

  **The pod.** It runs as uid 1000 with no ServiceAccount token, and has a
  `/data` volume (`MUSTER_AGENT_CC_STORAGE_SIZE`, default `10Gi`). Its
  NetworkPolicy:
  - admits connections only from muster's pods;
  - allows egress only to cluster DNS and TCP 443 on public IPv4 addresses, plus
    muster's connect port.

  See [limits.md](limits.md) for what that does and does not stop.

  **A workspace ccd cannot write in.** On a volume first used by an image up to
  0.49.0, `/data/workspace` may be root-owned 0755. If it is **empty**, `ccd
  seed` replaces it with one owned by uid 1000 and logs `ccd seed: replaced …`.
  If it is **not empty** or is a **symlink**, nothing is removed: the pod
  crash-loops with `ccd seed: <dir> is a directory this process (uid 1000) cannot
  create files in …` (or `… is a symlink …`). `kubectl exec` into the agent is
  impossible because its pod never starts. Instead, in the agent's namespace
  (`<MUSTER_AGENT_NAMESPACE_PREFIX><agent>`, e.g. `muster-agent-<agent>`), run a
  debug pod as root that mounts the agent's PVC (`<agent>-workspace`) at `/data`
  and run `chown -R 1000 /data/workspace && chmod u+rwx /data/workspace` there —
  one command whoever owns it (for a symlink, the error names the link's target:
  run it on that). Then delete the agent's pod so it restarts.

  **Permission prompts.** These go to `ccd approve`. If
  `MUSTER_APPROVAL_GATE_URL` is configured, it relays the prompt to the
  permission router; otherwise the prompt falls back to the agent's own terminal.

- **`hermes`** runs the image built from
  [`images/hermes-agent`](../images/hermes-agent/flake.nix): Hermes at one pinned
  upstream commit, run as `hermes gateway run` with its API server on loopback,
  supervised by `hermesd` ([`cmd/hermesd`](../cmd/hermesd);
  [design](design/hermes.md#the-hermes-agent-kind)).
  - `hermesd` serves `POST /v1/responses` on port 18789. Each turn is one
    Hermes run (`POST /v1/runs`, its SSE events); the reply is the run's output.
  - It runs on the **OpenRouter key** the `gateway` kind uses
    (`MUSTER_AGENT_OPENROUTER_KEY`, or a member's own), placed only in the
    agent's own Secret as `OPENROUTER_API_KEY`; Hermes strips it from the
    agent's shell. The model is the agent's (or `MUSTER_AGENT_MODEL`), an
    OpenRouter model.
  - It requires `MUSTER_AGENT_HERMES_IMAGE`, pinned to a tag or digest, and on
    Kubernetes both `MUSTER_AGENT_NETPOL_FROM_*` variables. The pod is the
    `claude-code` pod's profile: uid 1000, no ServiceAccount token, a `/data`
    volume (`MUSTER_AGENT_HERMES_STORAGE_SIZE`, default `10Gi`), the same
    NetworkPolicy — and a termination grace of 60 s, over Hermes's 30 s drain.
  - **Instructions** are sent on every run (Hermes's `instructions`), from
    `/etc/hermesd/instructions.md`, outside every volume. A worker gets none,
    as a `claude-code` worker gets none.
  - **Approvals.** A command Hermes flags is relayed to muster's approval queue
    (the card a `claude-code` prompt makes) and answered `once` on approve.
    Anything else — a deny, no answer within 240 s, no relay — is answered
    `deny` by `hermesd`, before Hermes's own 300 s timeout.
  - **Restarts.** `hermesd` keeps its own record of every turn on the volume and,
    after every start of the gateway, resolves what a crash left unsettled
    before it takes a turn (`/healthz` reports `recovering` meanwhile; turns are
    `not_ready`). A turn whose outcome it cannot know is answered `may_have_run`,
    never re-sent; a run that outlived its request is reported at the start of
    the session's next reply.
  - Not in this release: a Terminal view (`agents.terminal` refuses the kind), a
    `hermes` chief (refused: its MCP client against muster's tools is
    unverified), questions from the agent (`clarify`, off for API-server turns).
  - A repository is checked out as for `claude-code` (next section), by
    `hermesd` (`HERMESD_REPO`, `HERMESD_REPO_BRANCH`, `HERMESD_WORKSPACE`), and
    Hermes runs in it, loading its `AGENTS.md`. Only a connected repository is
    accepted.

### A claude-code agent's repository

A claude-code agent can be dispatched with a repository (`owner/name`, and a
branch), from the dispatch forms, the API or the chief's `task_dispatch`, like a
gateway agent — but only one of your **connected** GitHub repositories, from
every door (its session starts pre-trusted in the checkout, so the repository's
own `.claude/` runs beside the subscription token; see below). An agent without
one is exactly what it was before: no clone, the CLI in `/data/workspace`, the
same resume (`--resume` of its own conversation by id).

- **The checkout.** muster gives the pod `CCD_REPO`, `CCD_REPO_BRANCH` (when
  there is a branch) and `CCD_WORKSPACE=/data/workspace/<name>`. Before it
  starts the CLI, `ccd` clones `https://github.com/<owner/name>` there, on the
  agent's volume, and then starts the CLI **in the checkout**. The session's
  project is the repository: its `CLAUDE.md` and `.claude/` (project skills in
  `.claude/skills`, project settings) are the session's, alongside the managed
  `/etc/claude-code/CLAUDE.md`. An existing checkout (a restarted pod) is used
  as it is, never re-cloned or reset.
- **Credentials.** Only through the `github` connector
  ([connectors.md](connectors.md)): git reaches GitHub through muster's proxy
  once the agent holds an approved grant. With a [GitHub App](github-app.md)
  the proxy injects a token limited to the agent's own repository. No token is placed in the pod. A
  public repository clones without one.
- **Until the clone succeeds the CLI does not start.** Every turn answers
  `503 not_ready` with the reason (git's error, and what the `github` connector
  needs), so the kickoff's failure on the card says why; `/healthz` stays 200
  and reports the checkout under `repo`; `ccd` retries every 30 seconds. When
  the clone fails and the agent holds no `github` grant at all, `ccd` files the
  agent's request for it **once** (the same request the session's
  `connector_request` tool would file); approve it under Connectors and the
  next retry clones. A denied or revoked grant is not asked for again.
- **The first turn waits for the checkout.** While `/healthz` reports the
  checkout as `cloning` or `awaiting_grant` (the agent's `github` request is
  pending), muster does not send the kickoff and does not mark it sent; it
  looks again every 10 seconds. The card reads "Waiting for your github
  approval" with a link to Connectors. Once the checkout is ready the kickoff
  is delivered as usual, exactly once — no re-send by hand. After 24 hours of
  waiting (`MUSTER_CHECKOUT_WAIT_TIMEOUT`, measured from the agent's last status
  change) the agent goes to `error`: "the github grant was not approved". Any
  other clone failure (`failed`: a denied or revoked grant, an approved grant
  git cannot use) is not
  waited on: the kickoff is sent, and its `not_ready` reason is recorded.
- ⚠ **The repository's own `.claude/settings.json` applies to the session**
  (project settings: hooks, permissions, `env`). `ccd` strips `env` and
  `apiKeyHelper` only from the CLI's user settings, so dispatch only onto
  repositories you trust with the agent's subscription token.

To start one from the chief: connect the repository on the Repos tab (it
must be one of your connected repositories), make sure a `github` connector
exists, then have the chief call `task_dispatch` with `repo`.

## Talking to an agent

When a gateway is configured, three routes are live:

- `POST /api/agents/{name}/messages` takes a machine token (`muster agent …`,
  `muster chief ask`).
- `GET /agents/{name}/ws` is the chat socket.
- `POST /agents/{name}/send` is the message box (see
  [sending-and-step-up.md](sending-and-step-up.md)).

A `claude-code` agent has the same Rendered | Terminal switch as a mirrored
session.

## Destroying an agent

Two doors take the same teardown (`destroyAgent` in `internal/api/agents.go`):

- **Delete** on the Agents page (`DELETE /agents/{id}`, a signed-in session);
- **`agent_destroy {name | id}`** from one of your own mirrored Claude Code
  sessions (`POST /mirror/mcp` only — see
  [attention.md](attention.md#the-agent-tools)), so a stuck agent can be torn
  down without a browser. `muster agent ls` lists the names it takes.

Both refuse the chief before anything starts (use `muster chief replace` for
it; [chief.md](chief.md)), then run the provisioner's `Destroy` in the
background: the instance, then the row — and the row is **kept** (status
`error`) when the instance could not be removed (rule 2 below). What goes with
the row is the same on both doors: the agent's attention asks are deleted
(migration 0025's `ON DELETE CASCADE`; nothing on this path sends the
"resolved" message that would clear a notification already on your phone), and
a turn it is running is not stopped first — the pod's teardown ends it.

After a **successful** teardown, on both doors:

- if the agent was the one the chief's `task_dispatch` started on its task, the
  chief's one start on that task is **released** (an
  `task.chief_dispatch_released` row in the chief-dispatch ledger; nothing is
  deleted), so the chief may start one new agent there — see
  [chief.md](chief.md). ⚠ This is new for the Agents page's Delete too.
- your open tabs re-fetch: `agent.changed` and `attention_queue.changed` are
  broadcast to you.

**The task is kept on both doors** (unlike deleting the task, which tears its
agent down). Only `agent_destroy` also moves it back: a task still
`in_progress` with no other live agent goes to `open` (status writer
`agent-destroy-reopen`), which `task_dispatch` needs (open, ungated, no live
agent). The write is conditional — it moves the task only while it is still
`in_progress` — so a status you set meanwhile is kept. After the browser's
Delete the task stays as it was; reopen it on its card.

Neither door writes an audit row for the destroy: an `agent_destroy` call's
host and session are in muster's server log only.

## The chief

The **chief** is an ordinary agent row: your fleet agent. Its slug is `chief` for
user 1 and `chief-<id>` for any other user, so each user has their own.

- `POST /chief/provision` starts your chief if it exists. Otherwise it creates
  one and dispatches it with a fixed introduction ("You are chief, muster's fleet
  agent…").
- The chief panel (`/ui/chief/panel`) is the agent chat view mounted in a side
  panel.
- `muster chief ask` sends it a question from the CLI.
- Only a `gateway` chief receives the cairn coordinates (`MUSTER_AGENT_CAIRN_URL` and
  `MUSTER_AGENT_CAIRN_TOKEN`).
- It can run on either kind, and `muster chief replace` moves it from one kind
  to another. See [chief.md](chief.md).

## The provisioner seam (`internal/provision`)

`internal/provision` sits between "an agent should exist, configured like this"
and whatever runs it. It imports nothing outside the standard library, so wiring
the no-op driver brings in none of a cluster client's dependencies.

| driver | what it is for |
|---|---|
| `provision.Noop` | Records instead of provisioning. Use it to develop everything above the seam without a backend. |
| `provision/k8s` | Renders its own minimal manifests and installs no chart: Namespace, ServiceAccount, **two** Secrets (confidential environment, and separately confidential files), ConfigMap, PVC, Deployment, Service, and a NetworkPolicy for `claude-code`. |

`internal/agentprovision` adapts a driver to the API's lifecycle interface.
`internal/agentgateway` speaks the chat protocol over the same driver.

Four rules define a driver. Each rule's main clause is pinned by a test in
`provisiontest.RunContract` rather than by prose, with **one named exception**.
Rule 2's `ErrNotManaged` clause needs a shared backend to be expressible at all,
so it is pinned in the Kubernetes driver's own ownership tests instead.
`RunContract` has 20 cases, and none of them mentions `ErrNotManaged`. The rules
are stated in full in the package doc; in short:

1. **A driver that cannot see its backend returns an error, never an empty
   set.** A caller reads a `List` error as "keep the stored status", and an empty
   `List` as "nothing is running".
2. **`Destroy` returns `nil` only if the instance was removed or was already
   absent.**
   - A FOREIGN object holding the instance's name is neither of those. It is
     `ErrNotManaged`, which deliberately does **not** satisfy
     `errors.Is(err, ErrNotFound)`.
   - That refusal also means **nothing was removed**: a driver decides ownership
     before it deletes anything.
3. **`Create` is idempotent; a divergent spec is an error**, not a silent
   overwrite. Note the one asymmetry: when `Create` refuses because a foreign
   object has the same name, the error stays `ErrDivergentSpec`. It does **not**
   report `ErrNotManaged`.
4. **`Capabilities` is useless unless callers branch on it.** Each capability's
   refusal is spelled in exactly one place, and `capabilities.go`'s doc comment
   is the authority on which.

`internal/provision/provisiontest` is the contract suite every driver must pass.
It requires a working driver, a **blind** one and a **restricted** one, and it
does not skip.

- Without the blind driver, rule 1 cannot be tested.
- Without the restricted driver, the capability refusals are never seen to fire.
