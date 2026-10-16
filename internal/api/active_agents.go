package api

import (
	"sort"
	"time"
)

// activeAgentWindow is how long an agent keeps showing in the notification
// panel's ACTIVE section after its last stream event. While a turn is in flight
// the agent shows its live phase; once the turn ends the entry is cleared, but a
// recently-active agent (last activity within this window) still surfaces with a
// relative "Xm ago" label so a just-finished reply does not vanish instantly.
const activeAgentWindow = 3 * time.Minute

// agentPhase is the in-memory streaming phase for one agent: whether it is
// currently in flight (and showing "thinking…"/"responding"), and the timestamp
// of its last activity (used for both window expiry and the "Xm ago" label).
type agentPhase struct {
	// owner is the user whose agent this is (accounts PR 3b): the registry is
	// one server-wide map, so a reader MUST filter by it (activeAgentsList).
	owner    int64
	phase    string // "thinking" | "responding"
	at       time.Time
	inFlight bool // true while a turn is running; false once it ends (done/error)
}

// ActiveAgent is one entry of the notification panel's ACTIVE section: an agent
// that is streaming right now (InFlight) or was active within activeAgentWindow.
type ActiveAgent struct {
	Name     string
	Phase    string    // "thinking" | "responding"
	At       time.Time // last activity timestamp
	InFlight bool      // currently streaming (live phase) vs. recently active (Xm ago)
}

// markAgentActive records an in-flight streaming phase for agent name, stamped at
// now(). phase is "thinking" or "responding". Guarded by activeAgentsMu (WS turns
// run on goroutines). Setting it when a turn STARTS (with "thinking") makes the
// agent show active immediately, before its first stream event lands.
func (s *Server) markAgentActive(owner int64, name, phase string) {
	if name == "" || owner <= 0 {
		return
	}
	now := s.activeAgentsNow()
	s.activeAgentsMu.Lock()
	s.activeAgents[name] = agentPhase{owner: owner, phase: phase, at: now, inFlight: true}
	s.activeAgentsMu.Unlock()
}

// markAgentIdle ends agent name's in-flight turn (done/error): the entry stays so
// the agent shows as recently-active ("Xm ago") until activeAgentWindow lapses,
// but is no longer marked in-flight (no live phase pill). The timestamp is left
// at the last activity so window expiry is measured from real activity.
func (s *Server) markAgentIdle(name string) {
	if name == "" {
		return
	}
	s.activeAgentsMu.Lock()
	if st, ok := s.activeAgents[name]; ok {
		st.inFlight = false
		s.activeAgents[name] = st
	}
	s.activeAgentsMu.Unlock()
}

// activeAgentsList returns the agents to show in the ACTIVE section: every
// in-flight agent, plus any agent whose last activity was within
// activeAgentWindow. Expired entries are pruned as a side effect. The result is
// sorted most-recently-active first for a stable, useful order.
//
// 🔴 ONLY owner's (accounts PR 3b audit). The map is server-wide and keyed by
// agent name, fed by every user's chat turns; unfiltered, the notifications
// panel's ACTIVE section named other users' in-flight agents. A non-positive
// owner sees nothing.
func (s *Server) activeAgentsList(owner int64) []ActiveAgent {
	now := s.activeAgentsNow()
	s.activeAgentsMu.Lock()
	out := make([]ActiveAgent, 0, len(s.activeAgents))
	for name, st := range s.activeAgents {
		// An idle entry older than the window drops out entirely. An in-flight
		// entry always shows (a long turn must not silently disappear), and its
		// timestamp is refreshed by each stream event anyway.
		if !st.inFlight && now.Sub(st.at) > activeAgentWindow {
			delete(s.activeAgents, name)
			continue
		}
		if owner <= 0 || st.owner != owner {
			continue
		}
		out = append(out, ActiveAgent{Name: name, Phase: st.phase, At: st.at, InFlight: st.inFlight})
	}
	s.activeAgentsMu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out
}
