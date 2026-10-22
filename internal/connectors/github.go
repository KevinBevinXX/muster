package connectors

import (
	"context"
	"strings"
)

// ReachesGitHub reports whether a grant on connector name could put a GitHub
// credential in the agent's pod: the `github` connector, a connector with a
// github.com host, or — failing closed — one whose hosts are unknown.
//
// ONE PREDICATE, THREE DOORS: a pool worker holding an approved grant it
// matches is not eligible for a job, an approval of one for a pool worker is
// refused (internal/api jobs_grants.go), and a worker-pool agent holding one
// is never started with job-lane permission bypass (internal/agentprovision).
func ReachesGitHub(name string, hosts map[string][]string) bool {
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

// Hosts is every connector's hosts, by name.
func Hosts(ctx context.Context, s Store) (map[string][]string, error) {
	list, err := s.ListConnectors(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]string, len(list))
	for _, c := range list {
		out[c.Name] = c.Hosts
	}
	return out, nil
}

// GitHubGrantLookup answers, for one agent, the connector of an APPROVED grant
// it holds that reaches GitHub ([ReachesGitHub]), or "" when it holds none.
//
// A nil store is a deployment with no connector store: no grant can exist, so
// the answer is always "". That is stated here rather than left to a nil
// check at the caller, so the caller can treat a MISSING lookup as unknown.
func GitHubGrantLookup(s Store) func(ctx context.Context, agentID int64) (string, error) {
	return func(ctx context.Context, agentID int64) (string, error) {
		if s == nil {
			return "", nil
		}
		hosts, err := Hosts(ctx, s)
		if err != nil {
			return "", err
		}
		grants, err := s.ListGrants(ctx)
		if err != nil {
			return "", err
		}
		for _, g := range grants {
			if g.AgentID == agentID && g.Usable() && ReachesGitHub(g.Connector, hosts) {
				return g.Connector, nil
			}
		}
		return "", nil
	}
}
