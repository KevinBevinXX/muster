package main

import (
	"context"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/ccpool"
	"github.com/ZacxDev/muster/internal/credentials"
)

// kindSet implements api.AgentKinds over the configured kinds, the claude-code
// account pool (nil when that kind is not enabled) and the credential plane.
//
// 🔴 EVERY ANSWER IS FOR AN OWNER, AND COMES FROM THE PLANE. The pool's Source
// IS the plane, so the names listed, the account selected and the token a pod
// later gets are one decision about one owner.
type kindSet struct {
	kinds []string
	pool  *ccpool.Pool
	plane *credentials.Plane
}

// Enabled is in agents.Kinds order — gateway first — whatever order
// MUSTER_AGENT_KINDS was written in: the picker checks its first option, and
// the default must be the kind every deployment had before kinds existed.
func (k kindSet) Enabled() []string {
	on := map[string]bool{}
	for _, kind := range k.kinds {
		on[kind] = true
	}
	var out []string
	for _, kind := range agents.Kinds {
		if on[kind] {
			out = append(out, kind)
		}
	}
	return out
}

func (k kindSet) ClaudeAccounts(ctx context.Context, owner int64) ([]string, error) {
	if k.pool == nil {
		return nil, nil
	}
	return k.pool.Names(ctx, owner)
}

func (k kindSet) SelectClaudeAccount(ctx context.Context, owner int64, pin string) (string, error) {
	if k.pool == nil {
		return "", ccpool.ErrNoUsableAccount
	}
	return k.pool.Select(ctx, owner, pin)
}

func (k kindSet) Unavailable(ctx context.Context, owner int64) map[string]string {
	if k.plane == nil {
		return map[string]string{}
	}
	return k.plane.Unavailable(ctx, owner, k.Enabled())
}
