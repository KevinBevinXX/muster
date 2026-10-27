package agentprovision

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/provision"
)

func ccSpecConfig() agentspec.Config {
	cfg := fixtureSpecConfig()
	cfg.ClaudeCode = &agentspec.ClaudeCodeConfig{Image: "ghcr.io/example-org/claude-code-agent:7c1", Accounts: map[string]string{"acct-x": "sk-ant-oat01-FAKE-cc-3e9a"}}
	cfg.ConnectURL, cfg.ConnectPort = "http://muster.example.test:18091", 18091
	return cfg
}

func ccRow(name string) agents.Agent {
	ag := fixtureAgent()
	ag.Name = name
	ag.Kind, ag.CCAccount, ag.HooksToken = agents.KindClaudeCode, "acct-x", "fixture-hooks-cc"
	ag.GatewaySecret = "test-gateway-secret"
	return ag
}

// noGitHubGrant is the grant lookup answering "holds none" for every agent.
func noGitHubGrant(context.Context, int64) (string, error) { return "", nil }

// jobModeEnv returns every CCD_JOB_PERMISSION_MODE entry of the spec's plain
// environment AND its secrets (the variable must be in neither place for an
// agent that is not opted in, and only in Env for one that is).
func jobModeEnv(spec provision.Spec) (env, secrets []string) {
	for _, e := range spec.Env {
		if e.Name == "CCD_JOB_PERMISSION_MODE" {
			env = append(env, e.Value)
		}
	}
	for _, e := range spec.Secrets {
		if e.Name == "CCD_JOB_PERMISSION_MODE" {
			secrets = append(secrets, e.Value)
		}
	}
	return env, secrets
}

// 🔴 THE BYPASS VARIABLE REACHES ONLY A LISTED POOL'S AGENTS, AT THE SEAM. Built
// through the provisioner's own buildSpec (the agent's real path, with its
// prose): an agent in a listed pool gets CCD_JOB_PERMISSION_MODE=bypassPermissions
// exactly once; an agent in an UNLISTED pool, an UNPOOLED agent, and the AGENT —
// even with a row naming a listed pool — get no such variable at all; and with
// the list empty nobody does.
//
// ⚠ One unlisted pool has a listed one as its prefix,
// so a prefix or substring match would be caught.
func TestOnlyAListedPoolsAgentsAreToldToBypassJobPermissions(t *testing.T) {
	for _, c := range []struct {
		name, agent, pool string
		listed            []string
		want              []string
	}{
		{"listed pool", "review-worker-1", "pr-review", []string{"triage", "pr-review"}, []string{"bypassPermissions"}},
		{"unlisted pool", "build-worker-1", "builds", []string{"triage", "pr-review"}, nil},
		{"pool sharing a prefix with a listed one", "review-worker-2", "pr-review-slow", []string{"pr-review"}, nil},
		{"unpooled agent", "plain-worker", "", []string{"triage", "pr-review"}, nil},
		// The server never lets "" into the list; agentspec holds the line itself.
		{"unpooled agent, list naming the empty pool", "plain-worker-2", "", []string{"", "pr-review"}, nil},
		{"listed pool, list empty", "review-worker-3", "pr-review", nil, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := ccSpecConfig()
			cfg.ClaudeCode.BypassPools = c.listed
			row := ccRow(c.agent)
			row.WorkerPool = c.pool
			a := adapterWith(t, cfg, row)
			a.UseGitHubGrants(noGitHubGrant)
			spec, err := a.buildSpec(context.Background(), row)
			if err != nil {
				t.Fatal(err)
			}
			env, secrets := jobModeEnv(spec)
			if len(secrets) != 0 {
				t.Fatalf("CCD_JOB_PERMISSION_MODE is in the spec's SECRETS: %v", secrets)
			}
			if len(env) != len(c.want) || (len(env) == 1 && env[0] != c.want[0]) {
				t.Fatalf("CCD_JOB_PERMISSION_MODE values = %q, want %q", env, c.want)
			}
		})
	}
}

// grantStore is a connector store holding only connectors and grants; every
// other method is the nil interface's (a panic if called).
type grantStore struct {
	connectors.Store
	conns  []connectors.Connector
	grants []connectors.Grant
	err    error
}

func (g grantStore) ListConnectors(context.Context) ([]connectors.Connector, error) {
	return g.conns, g.err
}
func (g grantStore) ListGrants(context.Context) ([]connectors.Grant, error) { return g.grants, g.err }

// 🔴 A LISTED POOL'S AGENT HOLDING AN APPROVED GRANT THAT REACHES GITHUB IS NEVER
// BYPASSED, through the REAL lookup (connectors.GitHubGrantLookup, the one
// predicate the approval doors use). The control — the same agent with no such
// grant — IS bypassed, so the veto, not some other path, is what removes it.
// A pending github grant, an approved grant on a connector that does not reach
// GitHub, and another agent's approved github grant do not veto; an unreadable
// grant state and no lookup wired at all DO (fail closed), each with a log line
// naming the agent.
func TestAListedPoolsAgentHoldingAGitHubGrantIsNotBypassed(t *testing.T) {
	row := ccRow("review-worker-9")
	row.WorkerPool = "pr-review"
	conns := []connectors.Connector{{Name: "chat", Hosts: []string{"chat.example.com"}}, {Name: "gh-mirror", Hosts: []string{"api.github.com"}}}
	grant := func(agent int64, conn, status string) connectors.Grant {
		return connectors.Grant{ID: agent*10 + int64(len(conn)), AgentID: agent, Connector: conn, Status: status}
	}
	lookup := func(gs ...connectors.Grant) func(context.Context, int64) (string, error) {
		return connectors.GitHubGrantLookup(grantStore{conns: conns, grants: gs})
	}
	for _, c := range []struct {
		name    string
		lookup  func(context.Context, int64) (string, error)
		want    []string
		wantLog string
	}{
		{"control: no grant", lookup(), []string{"bypassPermissions"}, ""},
		{"no connector store", connectors.GitHubGrantLookup(nil), []string{"bypassPermissions"}, ""},
		{"pending github grant", lookup(grant(row.ID, "github", connectors.StatusPending)), []string{"bypassPermissions"}, ""},
		{"approved non-github grant", lookup(grant(row.ID, "chat", connectors.StatusApproved)), []string{"bypassPermissions"}, ""},
		{"another agent's github grant", lookup(grant(row.ID+1, "github", connectors.StatusApproved)), []string{"bypassPermissions"}, ""},
		{"approved github grant", lookup(grant(row.ID, "github", connectors.StatusApproved)), nil, `connector "github", which reaches GitHub`},
		{"approved grant on a github.com-host connector", lookup(grant(row.ID, "gh-mirror", connectors.StatusApproved)), nil, `connector "gh-mirror"`},
		{"grant state unreadable", connectors.GitHubGrantLookup(grantStore{err: errors.New("db down")}), nil, "could not be read (db down)"},
		{"no lookup wired", nil, nil, "no GitHub grant lookup is wired"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := ccSpecConfig()
			cfg.ClaudeCode.BypassPools = []string{"pr-review"}
			a := adapterWith(t, cfg, row)
			var logs bytes.Buffer
			a.log = log.New(&logs, "", 0)
			if c.lookup != nil {
				a.UseGitHubGrants(c.lookup)
			}
			spec, err := a.buildSpec(context.Background(), row)
			if err != nil {
				t.Fatal(err)
			}
			env, _ := jobModeEnv(spec)
			if len(env) != len(c.want) || (len(env) == 1 && env[0] != c.want[0]) {
				t.Fatalf("CCD_JOB_PERMISSION_MODE values = %q, want %q", env, c.want)
			}
			if c.wantLog == "" {
				if logs.Len() != 0 {
					t.Fatalf("unexpected log: %s", logs.String())
				}
				return
			}
			if l := logs.String(); !strings.Contains(l, `agent "review-worker-9"`) || !strings.Contains(l, "WITHOUT it") || !strings.Contains(l, c.wantLog) {
				t.Fatalf("log = %q, want a line naming the agent and %q", l, c.wantLog)
			}
		})
	}
}
