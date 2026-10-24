package api

import (
	"context"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/ui"
)

// latestSessionIDs is agents.Store.LatestSessionIDs for a batch of agent ids,
// best-effort: a failed read is logged and gives an empty map, so every link
// falls back to the bare page (which still redirects to the right session).
// It creates nothing — a GET of a list must not mint chat sessions.
func (s *Server) latestSessionIDs(ctx context.Context, ids []int64) map[int64]int64 {
	if s.ext.Agents == nil || len(ids) == 0 {
		return map[int64]int64{}
	}
	m, err := s.ext.Agents.LatestSessionIDs(ctx, ids)
	if err != nil {
		s.logger.Printf("agents: latest sessions for %d agents: %v (links fall back to the bare page)", len(ids), err)
		return map[int64]int64{}
	}
	return m
}

// agentPageHref is a's page as an in-app link names it: ui.AgentSessionHref on
// the agent's latest session, read on ctx. One agent; a list uses
// latestSessionIDs once for the whole page.
func (s *Server) agentPageHref(ctx context.Context, a agents.Agent) string {
	return ui.AgentSessionHref(a.Name, s.latestSessionIDs(ctx, []int64{a.ID})[a.ID])
}
