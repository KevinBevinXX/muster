package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/ui"
)

// handleChiefPanel serves GET /ui/chief/panel: the chief slide-out's body.
//
// 🔴 IT RE-MOUNTS THE EXISTING AGENT CHAT RATHER THAN BUILDING A SECOND ONE.
// The view it renders is the SAME ui.AgentDetailView handleAgentDetail builds,
// so the panel's #chat-log is fed by the same GET /ui/agents/{name}/chat-log on
// the same `sse:chat.reply`, and its #chat-form opens the same WebSocket. A
// second chat surface would mean a second transcript, a second unread ledger and
// a second place for a streaming bug to live — and the session drawer, the
// markdown pass and the kickoff-stream reveal would all have to be re-derived.
//
// 🔴 EVERY FAILURE HERE RENDERS AS PROSE, NOT AS AN HTTP ERROR. This partial is
// swapped into a panel the operator just opened; a 500 would leave them looking
// at whatever htmx does with an error body, which on this page is nothing at
// all. "No agent carries the chief display name" is a configuration statement a
// reader can act on, and it is the honest answer in every deployment that has
// not dispatched a chief yet.
func (s *Server) handleChiefPanel(w http.ResponseWriter, r *http.Request) {
	s.writeChiefPanelBody(r.Context(), w, r.URL.Query().Get("session"))
}

// writeChiefPanelBody renders the panel body for one session parameter.
//
// 🔴 IT IS A FUNCTION RATHER THAN A HANDLER BECAUSE A SECOND ROUTE ANSWERS WITH
// THIS EXACT PARTIAL. handleAgentSessionCreate's panel branch must return the
// panel body on the NEW session, and a panel that got its body from two renderers
// would be two panels: one of them would miss the out-of-band launcher slot, or the
// active-session mark the memory is keyed on, or the surface that routes around the
// containing-block hazard — and it would miss it silently, on one path only.
func (s *Server) writeChiefPanelBody(ctx context.Context, w http.ResponseWriter, sessionParam string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	write := func(v ui.ChiefPanelView) {
		if err := ui.RenderChiefPanelBody(w, v); err != nil {
			s.logger.Printf("chief: render panel body: %v", err)
		}
	}

	if s.ext.Agents == nil {
		write(ui.ChiefPanelView{Reason: "This muster is running without a database, so it has no agents to talk to."})
		return
	}
	a, err := s.chiefAgent(ctx)
	if err != nil {
		if errors.Is(err, ErrNoChief) {
			write(ui.ChiefPanelView{})
			return
		}
		s.logger.Printf("chief: resolve agent: %v", err)
		write(ui.ChiefPanelView{Reason: "Could not read the agent list. The panel will work again once it can."})
		return
	}
	s.writeChiefPanelBodyFor(ctx, w, a, sessionParam)
}

// writeChiefPanelBodyFor renders the panel body for an ALREADY-RESOLVED chief.
//
// 🔴 IT EXISTS SO THE NEW-THREAD POST RESOLVES THE CHIEF EXACTLY ONCE. That handler
// must know which agent it is writing to BEFORE it writes (see
// handleAgentSessionCreate's panel branch), and re-resolving here would give the
// write and the render two independent answers: a display-name move landing between
// them would create the thread on one agent and draw the other's panel — the same
// wrong-agent split, in a narrower window. One resolution, one subject.
//
// ⚠ The header on writeChiefPanelBody still holds: this is the ONLY renderer of the
// panel body, and that function is now its chief-resolving front door rather than a
// second copy.
func (s *Server) writeChiefPanelBodyFor(ctx context.Context, w http.ResponseWriter, a agents.Agent, sessionParam string) {
	// Set again rather than assumed: this is reachable directly from the new-thread
	// POST, which has written no headers of its own.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	write := func(v ui.ChiefPanelView) {
		if err := ui.RenderChiefPanelBody(w, v); err != nil {
			s.logger.Printf("chief: render panel body: %v", err)
		}
	}

	// The live status, computed the same way handleAgentDetail computes it, so the
	// provisioning indicator self-clears once the pod is up.
	status := s.liveStatus(ctx, a)

	// 🔴 THE PANEL OPENS ON THE CHIEF'S LATEST HUMAN SESSION, WHICH IS NOT THE
	// RECAP SESSION. recapSessionKey deliberately mints a separate conversation so
	// up to 120 machine-generated summaries per fleet refresh do not bury the
	// operator's own thread — on this very surface. Reusing the latest session
	// here is what puts the operator back where they left off.
	//
	// 🔴 AND A FAILED SESSION-LIST READ IS CARRIED TO THE VIEW, NOT DROPPED. This is
	// the FREQUENT path — #chief-panel-body refetches on every open — and it rendered
	// "No threads yet." whenever ListSessions failed, which tells the operator their
	// entire thread history is gone because one read broke. The search route already
	// answered honestly; this one is where the operator lands first.
	active, sessions, listErr := s.resolveSessions(ctx, a, sessionParam)
	if err := s.ext.Agents.MarkSessionRead(ctx, active.ID); err != nil {
		s.logger.Printf("chief: mark session %d read: %v", active.ID, err)
	}
	msgs, _ := s.ext.Agents.ListRecentChatMessages(ctx, active.ID, chatHistoryCap)
	lines := make([]ui.ChatLine, 0, len(msgs))
	for _, m := range msgs {
		lines = append(lines, ui.ChatLine{Role: m.Role, Content: m.Content, Kind: m.Kind, ToolID: m.ToolID, ToolName: m.ToolName, ToolOK: m.ToolOK, ID: m.ID})
	}
	noteID, taskStatus := s.agentTaskStatus(ctx, a)

	write(ui.ChiefPanelView{
		Configured:    true,
		ThreadsFailed: listErr != nil,
		Chat: ui.AgentDetailView{
			ID: a.ID, Name: a.Name, DisplayName: a.DisplayName, Status: status,
			Repo: a.Repo, Model: a.Model,
			Messages: lines, Sessions: sessions, ActiveSessionID: active.ID,
			NoteID: noteID, TaskStatus: taskStatus,
			// 🔴 THE SURFACE IS WHAT ROUTES AROUND THE CONTAINING-BLOCK HAZARD.
			// agentChatPane's sessionDrawer is `position: fixed`, and ChiefPanel's
			// transform makes the panel its containing block — so inside the panel it
			// would render at the panel's own right edge and go off-screen and inert
			// with it. This field is what makes the panel render chiefThreadList (an
			// absolutely-positioned sub-view of its own column) instead, and it is
			// also what puts the zero-message intro on screen. See ui.ChatSurface.
			Surface: ui.ChatSurfaceChiefPanel,
			// The panel's message box has the microphone too (transcribe.go).
			Dictate: s.dictatePath(),
		},
	})
}
