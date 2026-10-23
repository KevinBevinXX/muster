# The GitHub App

A GitHub App is the GitHub identity of muster's **admins** when one is
configured. It replaces the per-user OAuth connection (`GITHUB_CLIENT_ID`,
`MUSTER_GITHUB_TOKEN`), which is **deprecated**. The OAuth connection stays as
the fallback for a deployment without an App, and for members (see below).

With the App configured:

- **Agents get one-repository tokens.** The `github` connector is backed by
  the App. Each request an approved agent makes through muster's proxy
  (`/connect/github/…`, which is how `ccd` clones, fetches and pushes) carries
  an installation token that GitHub issues for **that agent's one repository**.
  The token has exactly `contents: write`, `pull_requests: write`,
  `checks: read` and `statuses: read`, plus the `metadata: read` that GitHub
  always adds. muster caches each token in memory until five minutes before it
  expires (GitHub issues them for one hour), and drops it early if GitHub
  answers 401. It is never stored, logged or given to the agent.
- **Other repositories are reached anonymously.** With an approved grant, all
  of the agent's git traffic to github.com goes through the proxy. A request
  for any repository other than the agent's own carries **no credential at
  all**, so a public dependency or submodule clones as it would for anyone and
  a private one fails as it would for anyone. The App token is never attached
  to it. muster compares the path's repository without case, after fully
  decoding it. It refuses, before minting anything, any path whose repository
  it cannot decide: a `.` or `..` segment, a doubly encoded or backslashed
  path, a leading empty segment, or `/repositories/<id>/…` (use
  `/repos/<owner>/<name>/…`). API paths that name no repository (`/graphql`,
  for example) carry the token, which can reach only the agent's repository.
- **An agent with no repository gets no github access** while the App is
  configured. A chief, for example, has no repository to scope a token to.
- **You approve each repository once.** The first agent's `github` request
  for a repository waits for you under Connectors; approving it is shared
  with every later agent of yours dispatched on **that same repository**
  ([connectors.md](connectors.md#shared-approvals)), and never with an agent
  on another repository. The App changes where the credential comes from, not
  who decides. A request filed before the App was configured (when no
  `github` connector existed) shows **Approve** once the App is configured.
  Approve it and the agent's next clone retry (every 30 s) succeeds, with no
  new dispatch.
- **A machine-job pool worker is never approved.** Approving a `github` grant
  (or one on any connector reaching github.com) for an agent in a worker pool
  is refused, and a worker holding one is skipped for jobs: a pool worker holds
  no GitHub credential ([jobs.md](jobs.md#worker-pools)).
- **The Repos tab lists the installation's repositories** (for an admin). So do
  the dispatch pickers and the "connected repository" check that every
  claude-code dispatch and `task_dispatch` applies. The OAuth connection is not
  consulted for an admin while the App is configured.
- **Only installations on `MUSTER_GITHUB_APP_ACCOUNTS` count, matched by the
  account's numeric id.** The App has to be installable on "Any account", so
  anyone can install it on their own repositories. An installation on an
  account that is not on the list adds no repository, gets no token and passes
  no check. muster logs it once, by login. The match is on the **id**, never
  the login. A login can be renamed, and the freed name can then be registered
  by someone else, who could install the App under it. So an installation whose
  login matches but whose id does not is ignored. An installation whose id
  matches under a new login (your own rename) is still used, and muster logs
  the rename so you can update the entry.
- The `github` connector card under Connectors says it is provided by the App.
  There is no Rotate or Delete. An admin's stored `github` connector (a
  personal access token) is hidden while the App is configured, and comes back
  unchanged if you remove the App.
- **Changing the source re-opens approvals.** Each approval records whether it
  was for the App or for a stored token. If you configure the App over a stored
  `github` token, or remove it from under approvals made for it, the affected
  agents' `github` grants go back to **pending**, and you approve them again.
  An approval never silently changes what it grants.

**The App is the admins', not every user's.** It belongs to the deployment.
On a multi-user muster, only admin-role users see it: their agents get the
App-backed `github` connector, and their dispatch checks and Repos tab list its
repositories. For a **member**, muster behaves as if no App were configured.
Their agents do not see the App connector, their `github` requests cannot be
approved onto it (whoever clicks Approve), and their repository list and
dispatch checks come from their own OAuth connection.

## Register the App

Sign in to GitHub as the account that will **own** the App (a personal
account is fine). Go to **Settings → Developer settings → GitHub Apps → New
GitHub App** (`https://github.com/settings/apps/new`) and fill in:

| field | value |
|---|---|
| GitHub App name | Any unique name, e.g. `muster-<yourname>` |
| Description | Optional |
| Homepage URL | Required by the form. Your muster's URL or the repository URL. |
| Callback URL | **Leave empty.** muster does not sign users in with the App. |
| Expire user authorization tokens | Leave as is (it does not apply). |
| Request user authorization (OAuth) during installation | **Unchecked** |
| Enable Device Flow | **Unchecked** |
| Setup URL | Leave empty |
| Webhook → Active | **Unchecked.** muster uses no webhooks; the Webhook URL is then not required. |
| Repository permissions → Contents | **Read and write** |
| Repository permissions → Pull requests | **Read and write** |
| Repository permissions → Checks | **Read-only** |
| Repository permissions → Commit statuses | **Read-only** |
| Repository permissions → Metadata | **Read-only** (GitHub selects it, and it cannot be removed) |
| Every other repository permission | No access |
| Organization permissions | All: No access |
| Account permissions | All: No access |
| Subscribe to events | None |
| Where can this GitHub App be installed? | **Any account.** "Only on this account" cannot be installed on an organization that you, the owner, do not own. |

Click **Create GitHub App**. On the App's page:

1. Note the **App ID** (under "About"). It is `MUSTER_GITHUB_APP_ID`.
2. Under **Private keys**, click **Generate a private key**. GitHub downloads a
   `.pem` file. That file is `MUSTER_GITHUB_APP_PRIVATE_KEY`. Keep it only in
   the Secret below. If it leaks, generate a new one and delete the old one on
   this page.

muster only ever asks for exactly those four repository permissions (Contents
and Pull requests read and write, Checks and Commit statuses read), whatever
the App was granted. If the App lacks one of them, GitHub refuses the token
(a 422 on the agent's card). But give the App no more than this table, so that a leaked
key cannot do more.

## Install it on the organization

On the App's page, click **Install App**, then **Install** next to the
organization (for example, the one whose repositories your agents work in).
Choose **Only select repositories** and pick the repositories agents may work
in. Click **Install**.

If you are not an owner of that organization, GitHub sends an installation
**request** instead. GitHub notifies the organization's owners. One of them
must approve it (the organization's settings, under **Third-party Access →
GitHub Apps**) before muster can mint any token.

To add or remove repositories later, go to the organization's **Settings →
Third-party Access → GitHub Apps → <your App> → Configure → Repository access**. muster's repository list
is cached for a minute.

muster picks the installation whose account matches the repository's owner.
That account must be on `MUSTER_GITHUB_APP_ACCOUNTS`.

## Configure muster

| variable | value |
|---|---|
| `MUSTER_GITHUB_APP_ID` | The App ID |
| `MUSTER_GITHUB_APP_PRIVATE_KEY` | The whole `.pem` file's contents. A value with its line breaks written as `\n` is accepted too. |
| `MUSTER_GITHUB_APP_ACCOUNTS` | **Required.** Comma-separated `<login>:<id>` entries for the accounts whose installations muster may use, e.g. `example-org:12345,your-user:67890`. Get each id with `gh api users/<login> -q .id` (it works for organizations too). An entry without an id refuses to boot. |

The App also needs the connector store (`MUSTER_CONNECTOR_ENCRYPTION_KEY`,
[connectors.md](connectors.md)), because agents reach GitHub only through the
proxy, and their grants live in that store. muster refuses to boot if the App ID
is set without the key (or the other way round), if the key does not parse, if
the account allowlist is empty, or if there is no connector store.

On Kubernetes, keep all three in one Secret and load it with `envFrom`:

```sh
kubectl -n <muster-namespace> create secret generic muster-github-app \
  --from-literal=MUSTER_GITHUB_APP_ID=<app id> \
  --from-literal=MUSTER_GITHUB_APP_ACCOUNTS=<org>:<org id>,<your-user>:<your id> \
  --from-file=MUSTER_GITHUB_APP_PRIVATE_KEY=./<app-name>.<date>.private-key.pem
```

```yaml
envFrom:
  - secretRef:
      name: muster-github-app
```

The Secret's keys are `MUSTER_GITHUB_APP_ID`, `MUSTER_GITHUB_APP_PRIVATE_KEY`
and `MUSTER_GITHUB_APP_ACCOUNTS`.

## Check it

At boot muster prints a `github app: ON — …` line, then lists the App's
installations in the background:

- `github app: App <id> authenticates and is installed on <accounts>`: it works.
- `github app: the boot check FAILED: GitHub refused the GitHub App's credentials (401) …`:
  the ID and key do not belong to the same App, the key was deleted, or the
  server's clock is more than a minute off. The App's JWT is valid from 60 s
  before to 9 minutes after muster signs it.
- `… is installed on NO account`: install it (above) on an account in
  `MUSTER_GITHUB_APP_ACCOUNTS`.
- `github app: RENAMED allowlisted account(s), still used by id: <listed>→<current> (<id>)`:
  an account you listed was renamed on GitHub. It still works. Update its entry
  to the new login.
- `github app: ignoring the installation on "<login>": not on MUSTER_GITHUB_APP_ACCOUNTS`:
  someone installed the App on an account you did not list. muster uses
  nothing from it.

A failure that blocks agents is also shown on the connector request card and on
each affected agent's card (`GitHub: …`). Examples are the credentials refused,
a rate limit, or a repository that is not in the installation (GitHub's 422).
The Repos tab shows it above the list.

## The kickoff wait

A claude-code agent whose repository cannot be cloned yet holds its first turn.
It waits up to `MUSTER_CHECKOUT_WAIT_TIMEOUT` (default `24h`) for your approval,
then goes to `error`. The wait is measured from the agent's last status change
(its `updated_at`). Recording the wait does not move that time. So raising the
variable and restarting muster also extends an agent that is already waiting:
its deadline becomes `updated_at` plus the new value.

## Removing the OAuth connection

The OAuth code (`/github/connect`, the per-user token store,
`MUSTER_GITHUB_TOKEN`) is still in this release as the fallback. It will be
removed once the App has been verified on a live deployment.
