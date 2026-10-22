package api

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/mirror"
)

// LIVE WORK COUNTS ON A DISPATCHED claude-code AGENT'S CARD
// (docs/design/agent-cards.md (a), "ccd: POST /agent/work"): how many subagents
// and background shells the agent's pod has running now, as ccd last reported
// them — the same mirror.WindowWork, and the same chip, a mirrored window's card
// shows.
//
// 🔴 STALE-SAFE: A REPORT COUNTS ONLY WHILE IT IS REFRESHED. ccd posts on every
// change and re-posts a non-zero report every minute (cmd/ccd/work.go); one not
// seen for agentWorkTTL is dropped, so a pod that died — or a ccd that stopped
// reporting — loses its chip instead of freezing it. The card's status poll
// carries what the card was drawn with (ui.StatusPollPath), so an expiry that no
// POST announces still re-renders the list within one poll.
//
// The route is the relay's (requireRelayAgent): the agent's OWN token, refused
// for the shared tokens, and served on the connect listener
// (ConnectSurfacePrefixes). ⚠ That token is one the CLI and its tool calls hold
// (cmd/ccd/turncred.go cliKeeps), exactly as for /agent/blocked: a tool call can
// post a count. The chip is display only and on that agent's own card, so the
// most a forged report does is mislabel the agent that forged it.

// agentWorkTTL is how long a work report counts without a refresh: three of
// ccd's one-minute heartbeats, as blockedReportTTL is.
const agentWorkTTL = 3 * time.Minute

// agentWorkLedger is the reports, in memory: a muster restart forgets them and
// the next heartbeat (a minute) restores them.
type agentWorkLedger struct {
	mu sync.Mutex
	m  map[int64]agentWorkReport
}

type agentWorkReport struct {
	work mirror.WindowWork
	seen time.Time
}

// AgentWork is what agent id's pod last reported it has running, or the zero
// value (no chip) when it reported nothing, nothing running, or not within
// agentWorkTTL.
func (s *Server) AgentWork(id int64) mirror.WindowWork {
	l := &s.agentWork
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.m[id]
	if !ok {
		return mirror.WindowWork{}
	}
	if s.now().Sub(r.seen) > agentWorkTTL {
		delete(l.m, id)
		return mirror.WindowWork{}
	}
	return r.work
}

// cardWork is what agent id's card may show for status (its live status): its
// report while it is running, and nothing otherwise. 🔴 ONE RULE FOR EVERY DOOR
// THAT STOPS AN AGENT: a stopped pod's ccd cannot post its clear (it is gone),
// so a stopped agent's last report would otherwise stay on its card until the
// TTL. Both the card and its status poll read it, so they cannot disagree.
func (s *Server) cardWork(id int64, status string) mirror.WindowWork {
	if status != agents.StatusRunning {
		return mirror.WindowWork{}
	}
	return s.AgentWork(id)
}

// clearAgentWork forgets agent id's report: it was stopped or destroyed.
func (s *Server) clearAgentWork(id int64) {
	l := &s.agentWork
	l.mu.Lock()
	delete(l.m, id)
	l.mu.Unlock()
}

func (s *Server) registerAgentWorkRoutes(mux Mux) {
	mux.HandleFunc("POST /agent/work", s.requireRelayAgent(s.handleAgentWork))
}

// handleAgentWork serves POST /agent/work: the body is mirror.WindowWork's JSON
// ({"subagents":n,"shells":n,"shellsAtLeast":b}), clamped as a window's is; a
// report with nothing running clears the agent's chip.
func (s *Server) handleAgentWork(w http.ResponseWriter, r *http.Request) {
	a, _ := agentFromContext(r.Context())
	var body mirror.WindowWork
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}
	work := mirror.NormalizeWorkEntry(body)
	now := s.now()
	l := &s.agentWork
	l.mu.Lock()
	if l.m == nil {
		l.m = map[int64]agentWorkReport{}
	}
	prev, was := l.m[a.ID]
	if was && now.Sub(prev.seen) > agentWorkTTL {
		prev, was = agentWorkReport{}, false
	}
	if work.IsZero() {
		delete(l.m, a.ID)
	} else {
		l.m[a.ID] = agentWorkReport{work: work, seen: now}
	}
	l.mu.Unlock()
	if prev.work != work {
		s.broadcast(r.Context(), EventAgentChanged, a.Name)
	}
	w.WriteHeader(http.StatusNoContent)
}
