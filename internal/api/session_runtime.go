package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE CHIEF'S SESSIONS ON MORE THAN ONE RUNTIME (migration 0030,
// internal/agents/session_runtime.go).
//
// A chief chat session is started on a runtime and a model, fixed for its life;
// muster routes each of its turns by them (sessionRoute). One session per agent
// is the DEFAULT: plugin deliveries and `chief ask` go to it (defaultSession).
//
// 🔴 EVERY SESSION THAT EXISTED BEFORE 0030 IS LEGACY AND ROUTES EXACTLY AS IT
// DID. Its turns carry no route header, so a claude-code chief's ccd puts them
// in the one CLI it has always run — which is what keeps the live chief's
// conversation where it is through the upgrade.

// sessionRuntimeStore is the store's 0030 half, when it has one.
func (s *Server) sessionRuntimeStore() (agents.SessionRuntimeStore, bool) {
	st, ok := s.ext.Agents.(agents.SessionRuntimeStore)
	return st, ok
}

// defaultSession is the session a's deliveries go to: its default, elected on
// first use. A store without the 0030 half keeps the pre-0030 answer, the
// latest session.
func (s *Server) defaultSession(ctx context.Context, a agents.Agent) (agents.ChatSession, error) {
	if st, ok := s.sessionRuntimeStore(); ok {
		return st.DefaultSession(ctx, a.ID, a.Name)
	}
	return s.ext.Agents.LatestOrCreateSession(ctx, a.ID, a.Name)
}

// sessionRoute returns ctx carrying the route of a's session sessionKey
// (agents.TurnRoute), for the transport to put on every request of the turn.
//
//	legacy       no route: the agent's own runtime, and for a claude-code agent
//	             ccd's original CLI.
//	claude-code  X-Muster-CC-Session (the key) and X-Muster-CC-Model: ccd runs
//	             the session in a CLI of its own, started with --model.
//	gateway      X-Openclaw-Model (the model override) and, in a claude-code
//	             pod, X-Muster-Runtime: gateway, which ccd answers by handing the
//	             turn to the gateway runtime beside it.
//
// 🔴 A SESSION THAT CANNOT BE READ IS ROUTED AS LEGACY, AND THAT IS THE ONE
// DIRECTION THAT CANNOT MISROUTE A SESSION THAT EXISTED BEFORE 0030. The cost is
// on an explicit session whose row read failed: its turn lands in the default
// CLI, and is logged.
func (s *Server) sessionRoute(ctx context.Context, a agents.Agent, sessionKey string) context.Context {
	st, ok := s.sessionRuntimeStore()
	if !ok || sessionKey == "" {
		return ctx
	}
	sess, err := st.SessionByKey(ctx, a.ID, sessionKey)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			s.logger.Printf("agents: %s: read session %q to route its turn: %v (routing it as a legacy session)", a.Name, sessionKey, err)
		}
		return ctx
	}
	return withSessionRoute(ctx, a, sess)
}

// withSessionRoute is sessionRoute for a caller holding the session row.
func withSessionRoute(ctx context.Context, a agents.Agent, sess agents.ChatSession) context.Context {
	h := http.Header{}
	switch sess.Runtime {
	case agents.SessionRuntimeClaudeCode:
		h.Set(agents.HeaderCCSession, sess.SessionKey)
		if sess.Model != "" {
			h.Set(agents.HeaderCCModel, sess.Model)
		}
	case agents.SessionRuntimeGateway:
		if agents.ResolveKind(a.Kind) == agents.KindClaudeCode {
			h.Set(agents.HeaderRuntime, agents.RuntimeGateway)
		}
		h.Set(agents.HeaderGatewayModel, sess.Model)
	default:
		return ctx
	}
	return agents.WithTurnRoute(ctx, agents.TurnRoute{Runtime: sess.Runtime, Header: h})
}

// sessionRuntimesFor is the explicit runtimes a NEW session of a may be started
// on: the deployment's (MUSTER_CHIEF_SESSION_RUNTIMES), for the chief only, and
// claude-code only for a claude-code chief (ccd is what runs it). Empty: no
// picker.
func (s *Server) sessionRuntimesFor(ctx context.Context, a agents.Agent) []string {
	if len(s.ext.SessionRuntimes) == 0 {
		return nil
	}
	if _, ok := s.sessionRuntimeStore(); !ok || !s.isChiefAgent(ctx, a) {
		return nil
	}
	var out []string
	for _, rt := range agents.SessionRuntimes {
		if !slices.Contains(s.ext.SessionRuntimes, rt) {
			continue
		}
		if rt == agents.SessionRuntimeClaudeCode && agents.ResolveKind(a.Kind) != agents.KindClaudeCode {
			continue
		}
		out = append(out, rt)
	}
	return out
}

// createRuntimeSession opens a's new session on (runtime, model) after checking
// the pair is one a may have. The error is the operator-facing reason.
func (s *Server) createRuntimeSession(ctx context.Context, a agents.Agent, runtime, model string) (agents.ChatSession, error) {
	if !slices.Contains(s.sessionRuntimesFor(ctx, a), runtime) {
		return agents.ChatSession{}, errRuntimeNotOffered
	}
	model, err := agents.ValidateSessionRuntime(runtime, model)
	if err != nil {
		return agents.ChatSession{}, err
	}
	st, _ := s.sessionRuntimeStore()
	return st.CreateSessionOn(ctx, a.ID, a.Name, runtime, model)
}

var errRuntimeNotOffered = errors.New("that runtime is not offered for this agent's sessions " +
	"(only the chief's, and only those MUSTER_CHIEF_SESSION_RUNTIMES names)")

// handleAgentSessionDefault handles POST /agents/{id}/sessions/{sid}/default:
// make session sid agent id's default, then answer like the session-create
// door (the chief panel body when it came from the panel, else a redirect).
func (s *Server) handleAgentSessionDefault(w http.ResponseWriter, r *http.Request) {
	id, err1 := strconv.ParseInt(r.PathValue("id"), 10, 64)
	sid, err2 := strconv.ParseInt(r.PathValue("sid"), 10, 64)
	if err1 != nil || err2 != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	a, err := s.ext.Agents.Get(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		s.logger.Printf("agents: default session lookup %d: %v", id, err)
		http.Error(w, "could not load agent", http.StatusInternalServerError)
		return
	}
	st, ok := s.sessionRuntimeStore()
	if !ok {
		http.Error(w, "this deployment's store has no default session", http.StatusNotImplemented)
		return
	}
	// The session must be this agent's: read through the caller's scope first, so
	// another owner's session id is a 404 and not a moved default.
	sess, err := s.ext.Agents.GetSession(ctx, sid)
	if err != nil || sess.AgentID != a.ID {
		http.NotFound(w, r)
		return
	}
	if err := st.SetDefaultSession(ctx, a.ID, sess.ID); err != nil {
		s.logger.Printf("agents: set default session %d for %d: %v", sess.ID, a.ID, err)
		http.Error(w, "could not set the default session", http.StatusInternalServerError)
		return
	}
	if chatSurfaceFromRequest(r) == ui.ChatSurfaceChiefPanel {
		s.writeChiefPanelBodyFor(ctx, w, a, strconv.FormatInt(sess.ID, 10))
		return
	}
	w.Header().Set("HX-Redirect", "/agents/"+a.Name+"?session="+strconv.FormatInt(sess.ID, 10))
	w.WriteHeader(http.StatusOK)
}
