package api

import (
	"context"
	"fmt"
	"strings"

	"github.com/ZacxDev/muster/internal/connectors"
)

// A WORKER-POOL MEMBER HOLDS NO CREDENTIAL FOR GITHUB.
//
// A commit job reviews code that may not be the operator's own, so the worker
// running it must not be able to write to the repository under review. The one
// way an agent gets a GitHub credential is a grant on a connector that reaches
// github.com — the `github` connector (the GitHub App's one-repository token,
// contents:write, docs/github-app.md) or any stored connector with a github.com
// host: with such a grant APPROVED, ccd routes every https://github.com/ git
// request of the pod through muster's proxy, which adds the credential
// (internal/podagent gitroute.go). So the rule, in one predicate
// (grantReachesGitHub) applied at two doors:
//
//   - a pool worker holding a LIVE grant (pending or approved — a pending one is
//     an approval away from a credential) on such a connector is not eligible
//     for a job (pickWorker → chooseWorker);
//   - an approval of such a grant for a pool worker is refused
//     (refusePoolWorkerGitHubGrant), at every approval door: the Connectors
//     tab, a mirrored session's connector_request_decide, and the approval that
//     comes with entering a credential.
//
// A worker joins its pool at creation, before it can hold any grant, so with
// both doors it never holds one. ⚠ A worker on a PRIVATE repository therefore
// cannot clone it (ccd's clone has no credential, and the request ccd files
// for one can never be approved): pool workers serve public repositories, the
// same ones a job's anonymous fetch can read.

// grantReachesGitHub reports whether a grant on connector name could put a
// GitHub credential in the agent's pod: the `github` connector, a connector
// with a github.com host, or — failing closed — one whose hosts are unknown.
func grantReachesGitHub(name string, hosts map[string][]string) bool {
	if name == "github" {
		return true
	}
	hs, known := hosts[name]
	if !known {
		return true
	}
	for _, h := range hs {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "github.com" || strings.HasSuffix(h, ".github.com") {
			return true
		}
	}
	return false
}

// connectorHosts is every connector's hosts, by name. nil with no connector
// store.
func (s *Server) connectorHosts(ctx context.Context) (map[string][]string, error) {
	if s.ext.Connectors == nil {
		return nil, nil
	}
	list, err := s.ext.Connectors.ListConnectors(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]string, len(list))
	for _, c := range list {
		out[c.Name] = c.Hosts
	}
	return out, nil
}

// gitHubGrants maps each agent among ids that holds a live grant reaching
// GitHub to that grant's connector. Empty with no connector store.
func (s *Server) gitHubGrants(ctx context.Context, ids []int64) (map[int64]string, error) {
	out := map[int64]string{}
	if s.ext.Connectors == nil || len(ids) == 0 {
		return out, nil
	}
	hosts, err := s.connectorHosts(ctx)
	if err != nil {
		return nil, err
	}
	want := make(map[int64]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	grants, err := s.ext.Connectors.ListGrants(ctx)
	if err != nil {
		return nil, err
	}
	for _, g := range grants {
		if want[g.AgentID] && grantReachesGitHub(g.Connector, hosts) {
			out[g.AgentID] = g.Connector
		}
	}
	return out, nil
}

// errPoolWorkerGitHubGrant is the refusal to approve a GitHub-reaching grant
// for a worker-pool member.
type errPoolWorkerGitHubGrant struct{ agent, pool, connector string }

func (e errPoolWorkerGitHubGrant) Error() string {
	return fmt.Sprintf("%s is a member of worker pool %s, and a pool worker holds no GitHub credential "+
		"(connector %s reaches github.com); the request was not approved", e.agent, e.pool, e.connector)
}

// refusePoolWorkerGitHubGrant is errPoolWorkerGitHubGrant when approving grant
// g would give a worker-pool member a GitHub credential, else nil.
func (s *Server) refusePoolWorkerGitHubGrant(ctx context.Context, g connectors.Grant) error {
	a, err := s.ext.Agents.Get(ctx, g.AgentID)
	if err != nil {
		return fmt.Errorf("%w: %w", errGrantUnreadable, err)
	}
	if a.WorkerPool == "" {
		return nil
	}
	hosts, err := s.connectorHosts(ctx)
	if err != nil {
		return err
	}
	if grantReachesGitHub(g.Connector, hosts) {
		return errPoolWorkerGitHubGrant{agent: a.Name, pool: a.WorkerPool, connector: g.Connector}
	}
	return nil
}
