# Agents: dispatch, kinds, the agent, and the provisioner

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

`MUSTER_AGENT_KINDS` is a comma list of `gateway` and `claude-code`. It
defaults to `gateway`, and `gateway` must always be in it.

A `hermes` kind existed (up to `d46b22e1`) and was removed. A `hermes` entry
in `MUSTER_AGENT_KINDS` is now an unknown kind and refuses to boot, and
migration `0047` refuses to run while any `hermes` agent row exists: destroy
those agents with a build that still has the kind before upgrading.

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
 accounts marked `auth_failed`, then any account still cooling down after a
 `rate_limited` (until the reset time the CLI stated, else
 `MUSTER_AGENT_CC_RATE_LIMIT_COOLDOWN`, default `1h`) ranked last — but still
 chosen when it is all there is — then the least recently rate-limited, then
 the one with the fewest live agents. A pin at dispatch overrides this. An agent
 keeps its account for life. The token goes only into that agent's own Secret,
 as `CLAUDE_CODE_OAUTH_TOKEN`. The pool configured in the environment belongs
 to the admin; members use credentials they stored themselves
 ([auth.md](auth.md)).

 **A worker pool.** The dispatch forms' **Worker pool** field, shown only
 while Claude Code is picked, makes the new agent a member of that pool of
 the machine job API ([jobs.md](jobs.md#worker-pools)). Leave it empty for an
 ordinary agent.

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

### A claude-code agent's repository

A claude-code agent can be dispatched with a repository (`owner/name`, and a
branch), from the dispatch forms, the API or the agent's `task_dispatch`, like a
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
- **Machine jobs at a commit do not use the checkout.** A [job](jobs.md) with a
 `sha` fetches its pull request's head anonymously from github.com (around any
 proxy routing the pod has) and runs in a git worktree of its own at
 `/data/workspace/.muster-jobs/<job id>` — beside the checkout, on the same
 volume — with the commit's agent configuration replaced by the default
 branch's, in a Claude Code session whose CLI `ccd` starts in that directory
 (`X-Muster-CC-Workdir`). `ccd` accepts only that exact directory shape; the
 CLI trusts it as a worktree of the trusted checkout. The worktree is removed
 when the job ends. The checkout, its branch and its working tree are never
 moved.

To start one from the agent: connect the repository on the Repos tab (it
must be one of your connected repositories), make sure a `github` connector
exists, then have the agent call `task_dispatch` with `repo`.

## Talking to an agent

When a gateway is configured, three routes are live:

- `POST /api/agents/{name}/messages` takes a machine token (`muster agent …`,
 `muster agent ask`).
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

Both refuse the agent before anything starts (use `muster agent replace` for
it; ), then run the provisioner's `Destroy` in the
background: the instance, then the row — and the row is **kept** (status
`error`) when the instance could not be removed (rule 2 below). What goes with
the row is the same on both doors: the agent's attention asks are deleted
(migration 0025's `ON DELETE CASCADE`; nothing on this path sends the
"resolved" message that would clear a notification already on your phone), and
a turn it is running is not stopped first — the pod's teardown ends it.

After a **successful** teardown, on both doors:

- if the agent was the one the agent's `task_dispatch` started on its task, the
 agent's one start on that task is **released** (an
 `task.chief_dispatch_released` row in the agent-dispatch ledger; nothing is
 deleted), so the agent may start one new agent there — see
 . ⚠ This is new for the Agents page's Delete too.
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

A task becoming `complete` **stops** (never destroys) the agent the agent started for it: see [plugins.md](plugins.md#completing-a-task-stops-the-dispatched-agent).

### Reaping stopped agents

An agent whose stored status has been `stopped` for
`MUSTER_AGENT_REAP_STOPPED_AFTER` (default `336h`, 14 days) is destroyed by
the **stopped-agent reaper**, through the same teardown as the two doors above
(`destroyAgent`, acting as the agent's owner): the agent is refused, then the
provisioner's `Destroy` removes the instance and then the row, and after a
successful teardown the agent's start on the agent's task is released and your
open tabs are told, exactly as for the Agents page's Delete. Like that Delete,
it leaves the agent's task as it was.

- **The clock** is `agents.stopped_at` (migration 0045): the time the row last
 moved into `stopped`, stamped by a database trigger (today the writers of
 `stopped` are Stop and a saved dispatch). Writing `stopped` over a row that
 already is `stopped` does not move it. **Start** moves the row out of
 `stopped`, so the next Stop starts a new clock: a restarted agent that is
 stopped again waits a full window. Agents already stopped when 0045 was
 applied have their clock started at that moment.
- **Never reaped:** the agent (either kind, [below](#the-agent)); an agent in
 any other status, `error` included; a [pool worker](jobs.md#worker-pools)
 (`worker_pool` set); and an agent that **never ran** — saved for later and
 never started. "Ran" is `agents.first_started_at` (migration 0045), stamped
 by the same trigger the first time the row is written `provisioning` or
 `running`: a dispatch writes `provisioning` only after the instance was
 created, and Start only after it was brought up, while a save writes
 `stopped` straight away and a refused or failed dispatch writes `error`.
 Started once, it stays stamped. When 0045 was applied, a row counted as
 having run only if it was `running` or a first turn had been handed to an
 instance (`kicked_off`, a recorded kickoff pod, or a delivery attempt);
 every other row, including a stopped agent that was started but never got
 its first turn, counts as never started until it is Started again.
- **When:** the reaper rides the idle-task reaper's sweep, every 30 minutes,
 on every pod (`muster-server` installs no leader gate for these loops; see
 [limits.md](limits.md#deployment)). Before it starts a teardown it claims the agent with one
 conditional update that re-checks the stored conditions above (status,
 pool, ran, window) and that no other claim is live, so two pods never both
 destroy an agent, and an agent Started after the sweep read it (now
 `provisioning` or `running`, or stopped again with a fresh `stopped_at`) is
 left alone. A claim excludes other claims for an hour.
- **The instance is checked too.** A stored `stopped` is not proof the
 instance is down: Start brings the instance up before it writes
 `provisioning`, so a Start whose status write failed leaves a running
 instance under a `stopped` row. Before claiming anything, each pass reads
 the provisioner's instances and skips (and logs: `agent-reap: agent <id>
 (<name>) is stored stopped but its instance reads <status>; not reaped`)
 any due agent whose instance reads anything but stopped, by the same rule
 the Agents page uses. No instance at all, or one scaled to zero, counts as
 stopped. If the instances cannot be read, or the read has not answered
 within 30 seconds, the pass destroys nothing.
- **The record** is one server log line per agent whose teardown it started
 (`agent-reap: destroying agent <id> (<name>) of user <owner>, task <id or
 none>: stopped since <time>, longer than <window>`). No task comment and no
 audit row are written. A teardown that could not remove the instance is
 logged and keeps the row with status `error` (rule 2 below), so it is not
 retried; one that removed the instance but not the row leaves it `stopped`,
 and it is claimed again once the hour has passed.
- **Off:** `MUSTER_AGENT_REAP_STOPPED_AFTER=0` (or `off`). The boot log says
 which (`agent reaper: sweeping every …` or `agent reaper: off`).

⚠ A Start in flight — instance being brought up, `provisioning` not yet
written — whose instance the pass's read did not yet see as up still reads
`stopped` on both counts. A claim landing then wins, and the teardown removes
what the Start brought up: the same race as pressing Start and Delete at once.

## The agent

The **agent** is an ordinary agent row: your fleet agent. Its slug is `agent` for
user 1 and `agent-<id>` for any other user, so each user has their own.

- `POST /agent/provision` starts the target agent if it exists. Otherwise it creates
 one and dispatches it with a fixed introduction ("You are agent, muster's fleet
 agent…").
- The agent panel (`/ui/agent/panel`) is the agent chat view mounted in a side
 panel.
- `muster agent ask` sends it a question from the CLI.
- Only a `gateway` agent receives the cairn coordinates (`MUSTER_AGENT_CAIRN_URL` and
 `MUSTER_AGENT_CAIRN_TOKEN`).
- It can run on either kind, and `muster agent replace` moves it from one kind
 to another. See .

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
