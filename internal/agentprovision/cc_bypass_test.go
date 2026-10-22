package agentprovision

import (
	"context"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

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
// through the provisioner's own buildSpec (the chief's real path, with its
// prose): an agent in a listed pool gets CCD_JOB_PERMISSION_MODE=bypassPermissions
// exactly once; an agent in an UNLISTED pool, an UNPOOLED agent, and the CHIEF —
// even with a row naming a listed pool — get no such variable at all; and with
// the list empty nobody does. One unlisted pool has a listed one as its prefix,
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
		{"chief, row naming a listed pool", agents.ChiefName, "pr-review", []string{"pr-review"}, nil},
		{"chief of another user", agents.ChiefNameFor(7), "pr-review", []string{"pr-review"}, nil},
		{"listed pool, list empty", "review-worker-3", "pr-review", nil, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := ccSpecConfig()
			cfg.ClaudeCode.BypassPools = c.listed
			row := ccRow(c.agent)
			row.WorkerPool = c.pool
			spec, err := adapterWith(t, cfg, row).buildSpec(context.Background(), row)
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
