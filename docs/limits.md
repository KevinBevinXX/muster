# Limits, stated plainly

## Deployment

- **Run one replica.** `internal/db` holds no lease, so the daily retention pass
  and the idle-task reaper run on every pod. Both are idempotent, so a second
  replica duplicates work rather than effects. But other state lives only in
  memory: the mirror tunnel, the approval-relay ledger, and the send-nonce table.
  A second replica would split all three. The kickoff, status and memo loops use
  per-row claims and are safe either way.
- **No Kubernetes manifests ship here.** That includes the RBAC muster's
  ServiceAccount needs for the `kubernetes` driver. Without it, every dispatch
  fails with a 403 from the API server, which looks like a driver bug rather than
  a missing Role.

## Agents

- **The provisioner has two drivers, and neither has run against a real cluster
  from this tree.** The Kubernetes driver's tests use a fake clientset. A fake
  cannot see scheduling, image pulls, volume attachment, admission, or the exec
  stream. A green suite there means the manifests are what the code says, not
  that a pod came up.
- **A first turn against an agent this binary provisioned has not been verified
  in-repo.** `internal/agentkickoff`'s seam test runs the real adapter, deliverer
  and gateway against a fake runtime over HTTP. A real turn needs
  `make test-liveenv` against a live runtime.
- **The UI offers Dispatch on a deployment that will refuse it.** Without a
  gateway, the refusal is shown once you click (a 409 toast), but the button is
  not disabled in advance.
- 🔴 **The Kubernetes driver does not restrict egress by DNS name.** Restricting
  egress by name is the control against a prompt-injected model sending data out,
  and muster does not implement it.

  For a `claude-code` agent, the driver writes an *address-range* NetworkPolicy.
  Only muster's own pods can reach the agent. The agent can reach only the
  cluster's DNS pods and TCP 443 on public IPv4 addresses.
  - Where the cluster's pod, service, API-server and node addresses, and the
    network around it, are all private (RFC 1918, link-local or CGNAT), that
    keeps the agent off the cluster's API, other namespaces and that network.
  - Where any of those is public, it does not. muster does not check which kind
    of cluster it is on.

  Either way, the agent can still resolve names and send anything it holds to any
  public HTTPS host. A `gateway`-kind agent gets no NetworkPolicy at all. And a
  NetworkPolicy is only a declaration: the cluster's network plugin enforces it
  or does not, and muster cannot tell which.
- **`MUSTER_TAG_AUTODISPATCH` dispatches nothing.** It only logs and counts
  eligible tasks.

## Mirroring

- 🔴 **`-interactive` is a shell on the host** for anyone who can use muster's
  Interactive switch for it ([mirroring.md](mirroring.md#read-only-interactive-and-sending)).
- **While a window is being viewed, its viewer session is visible to tmux
  tooling** (`tmux ls`, session pickers, hooks).
- **Per host:** 512 windows, 8 terminals, 8 rendered views. **Per server:** 64
  hosts.
- **Dispatched agents' terminals and sends are not step-up gated.** Only mirrored
  sessions are.

## The leak gate

**The leak gate does not scan its own file; it audits it, which is narrower.**
`tests/leakscan.py` has to contain realistic sensitive strings, because they are
its negative controls. So it is exempt from the scan, and listed as a named skip.

Every file in `SKIP_FILES` is audited **by value** instead. Every address,
hostname, registry reference, credential, denied name and dated stamp it contains
must be one of:

- objectively unroutable;
- objectively synthetic;
- declared in `EXEMPT_FIXTURE_VALUES`, with the reason it is safe.

Anything new fails closed.

What the audit still accepts is written out in the module beside the table:

- a real value that somebody declares anyway;
- prose (a real incident described in a fixture contains no value an audit can
  extract);
- IPv6 in any form;
- the fixtures of every file the gate does scan.
