package api

import (
	"context"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
)

// AgentState is the send queue's view of the fake host: what agentState says,
// for user 1's host "ws" only — another user's host is offline to them, as in
// the hub. The zero value is a host that reports no status (an older
// muster-mirror), which is what every test written before the queue expects.
// Each call records the door its principal came through (agentStateVias).
func (f *fakeMirror) AgentState(ctx context.Context, host, _, _ string) mirror.AgentState {
	p, _ := authz.From(ctx)
	f.mu.Lock()
	f.agentStateVias = append(f.agentStateVias, p.Via)
	f.mu.Unlock()
	if f.user(ctx) != 1 || host != "ws" {
		return mirror.AgentState{Unknown: mirror.ErrHostOffline}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.agentState == nil {
		return mirror.AgentState{Unknown: mirror.ErrNoStatusReport}
	}
	return *f.agentState
}

// AgentState: the isolation walk's host reports no status.
func (m *isoMirror) AgentState(ctx context.Context, host, _, _ string) mirror.AgentState {
	if !m.mine(ctx) || host != m.host {
		return mirror.AgentState{Unknown: mirror.ErrHostOffline}
	}
	return mirror.AgentState{Unknown: mirror.ErrNoStatusReport}
}
