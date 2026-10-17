package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/stepup"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// ASKUSERQUESTION, ANSWERED FROM THE RENDERED VIEW (internal/askq).
//
// Two audiences:
//
//   - THE HOOK (`muster-mirror ask`, a PreToolUse hook on the operator's own
//     machine) — /mirror/ask: it registers the call's questions and long-polls
//     for answers, authenticated by its host's MIRROR TOKEN (the same door as
//     /mirror/connect, mirror.Service.AuthenticateHost). A question is accepted
//     only for a Claude session the token's own host lists right now, and only
//     on an -interactive host; anything else is refused and the hook leaves the
//     question to the terminal's picker.
//   - THE PERSON (the rendered view's live question region) —
//     ui.MirrorAsksPath, the window's held questions (GET), and
//     ui.MirrorAskPath, one question set's answers (POST).
//
// 🔴 AN ANSWER IS A WRITE INTO THE OPERATOR'S SESSION, SO IT PASSES EXACTLY THE
// GATES A SEND PASSES (mirrorSend): a same-origin request from the session's
// owner (another owner's host is not found), a host that advertised
// -interactive, and a FRESH sign-in (internal/stepup). Only then are the answers
// read, and they are checked against the question set (askq.FromForm): an
// unknown question, a label that is not an option, a missing answer, an
// oversized one — refused.
//
// 🔴 NOTHING ANSWERS A QUESTION BUT A PERSON. The hook prints answers only when
// muster says a person gave them; every other outcome — muster down, a refused
// registration, a timeout, a withdrawn question — prints nothing, and the CLI
// shows its own picker.
// ---------------------------------------------------------------------------

// mirrorAsker is what the ask routes need from the mirror plugin beyond the
// Mirror interface. *mirror.Service implements it; a plugin that does not has
// no ask routes (404, so the hook falls back to the terminal).
type mirrorAsker interface {
	AuthenticateHost(w http.ResponseWriter, r *http.Request) (owner int64, host string, ok bool)
	HostClaudeSession(owner int64, host, claudeSession string) (listed, interactive bool)
	SetWaiting(fn func(owner int64, host, claudeSession string) bool)
	Nudge(owner int64)
}

// askSourceMirror is the askq.Key Source of a question a mirror host's hook
// asked; its Scope is the host's name.
const askSourceMirror = "mirror"

// The hook's protocol bounds.
const (
	// mirrorAskBodyMax bounds a registration's body: the questions within
	// askq's bounds fit with room to spare.
	mirrorAskBodyMax = 256 << 10
	// mirrorAskMaxWait bounds one long-poll.
	mirrorAskMaxWait = 25 * time.Second
	// mirrorAskFormMax bounds an answer form.
	mirrorAskFormMax = 64 << 10
)

// mirrorAskListWait is how long a registration waits for the host to list
// its Claude session: past the host's 5 s refresh (mirrorhost.Config.Refresh).
const mirrorAskListWait = 8 * time.Second

// askListWait is mirrorAskListWait, or a test's override.
func (s *Server) askListWait() time.Duration {
	if s.askListWaitOverride > 0 {
		return s.askListWaitOverride
	}
	return mirrorAskListWait
}

// toolUseIDPattern is a tool_use id as accepted here ("toolu_…" in practice).
var toolUseIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// Questions is the pending-question store: what the hook registered, keyed
// surface-neutrally (askq.Key). An attention view lists a user's waiting
// questions with Questions().Pending(owner).
func (s *Server) Questions() *askq.Store { return s.asks }

// wireAsks connects the question store to the mirror plugin: a pending
// question marks its window WAITING (Claude Code itself says "busy" while the
// hook holds the call), and every change nudges the owner's open views.
func (s *Server) wireAsks(m Mirror) {
	a, ok := m.(mirrorAsker)
	if !ok || s.asks == nil {
		return
	}
	store := s.asks
	a.SetWaiting(func(owner int64, host, claudeSession string) bool {
		return store.Waiting(owner, askSourceMirror, host, claudeSession)
	})
	store.OnChange(func(e askq.Entry) {
		if e.Key.Source == askSourceMirror {
			a.Nudge(e.Key.Owner)
		}
	})
}

func (s *Server) registerMirrorAskRoutes(mux Mux) {
	// 🔴 NO SESSION: the hook is a host, authenticated by its mirror token
	// alone (mirror.Service.AuthenticateHost) — like /mirror/connect.
	mux.HandleFunc("POST /mirror/ask", s.handleMirrorAskOpen)
	mux.HandleFunc("GET /mirror/ask/{id}", s.handleMirrorAskPoll)
	mux.HandleFunc("DELETE /mirror/ask/{id}", s.handleMirrorAskWithdraw)
	// The person: the card's state, and its answers.
	mux.HandleFunc("GET "+ui.MirrorAsksPath, s.requireSession(s.handleMirrorAsks))
	mux.HandleFunc("POST "+ui.MirrorAskPath, s.requireOperatorWrite(s.handleMirrorAskAnswer))
}

// askHost authenticates the hook's request; ok false has answered.
func (s *Server) askHost(w http.ResponseWriter, r *http.Request) (a mirrorAsker, owner int64, host string, ok bool) {
	if s.ext.Mirror == nil || s.asks == nil {
		http.NotFound(w, r)
		return nil, 0, "", false
	}
	a, can := s.ext.Mirror.(mirrorAsker)
	if !can {
		http.NotFound(w, r)
		return nil, 0, "", false
	}
	owner, host, ok = a.AuthenticateHost(w, r)
	return a, owner, host, ok
}

// mirrorAskOpenRequest is the hook's registration: the PreToolUse payload's
// session_id, tool_use_id and tool_input, and how long it will hold the call.
type mirrorAskOpenRequest struct {
	SessionID   string          `json:"sessionId"`
	ToolUseID   string          `json:"toolUseId"`
	ToolInput   json.RawMessage `json:"toolInput"`
	HoldSeconds int             `json:"holdSeconds"`
}

// mirrorAskState is what the hook reads back.
type mirrorAskState struct {
	ID      string            `json:"id"`
	State   askq.State        `json:"state"`
	Answers map[string]string `json:"answers,omitempty"`
}

// handleMirrorAskOpen is POST /mirror/ask: 201 {id, state} for a question the
// token's host may ask; 404 for a Claude session that host does not list now;
// 409 for a read-only host; 400 for a question set muster will not show.
func (s *Server) handleMirrorAskOpen(w http.ResponseWriter, r *http.Request) {
	a, owner, host, ok := s.askHost(w, r)
	if !ok {
		return
	}
	var req mirrorAskOpenRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, mirrorAskBodyMax)).Decode(&req); err != nil {
		http.Error(w, "the registration is not JSON within its size bound", http.StatusBadRequest)
		return
	}
	if !mirror.ValidClaudeSession(req.SessionID) || !toolUseIDPattern.MatchString(req.ToolUseID) {
		http.Error(w, "a registration needs the call's session_id and tool_use_id", http.StatusBadRequest)
		return
	}
	qs, err := askq.ParseInput(req.ToolInput)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// A conversation's FIRST call can arrive before the host's next refresh
	// has reported its session (a transcript the CLI created with the prompt):
	// wait that refresh out before calling the session unknown.
	listed, interactive := a.HostClaudeSession(owner, host, req.SessionID)
	for until := time.Now().Add(s.askListWait()); !listed && time.Now().Before(until) && r.Context().Err() == nil; {
		time.Sleep(250 * time.Millisecond)
		listed, interactive = a.HostClaudeSession(owner, host, req.SessionID)
	}
	switch {
	case !listed:
		http.Error(w, "this host does not list that Claude session now", http.StatusNotFound)
		return
	case !interactive:
		http.Error(w, mirror.ErrInteractiveRefused.Error(), http.StatusConflict)
		return
	}
	e, err := s.asks.Open(askq.Key{Owner: owner, Source: askSourceMirror, Scope: host, Session: req.SessionID, ToolUseID: req.ToolUseID},
		qs, time.Duration(req.HoldSeconds)*time.Second)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, askq.ErrFull) {
			status = http.StatusServiceUnavailable
		}
		http.Error(w, err.Error(), status)
		return
	}
	s.logger.Printf("mirror: %s asks %d question(s) in session %s (%s)", host, len(qs), req.SessionID, e.ID)
	s.writeJSON(w, http.StatusCreated, mirrorAskState{ID: e.ID, State: e.State})
}

// handleMirrorAskPoll is GET /mirror/ask/{id}?wait=<seconds>: the question's
// state, after waiting up to wait (capped) for it to leave pending.
func (s *Server) handleMirrorAskPoll(w http.ResponseWriter, r *http.Request) {
	_, owner, host, ok := s.askHost(w, r)
	if !ok {
		return
	}
	wait := time.Duration(0)
	if n, err := strconv.Atoi(r.URL.Query().Get("wait")); err == nil && n > 0 {
		wait = min(time.Duration(n)*time.Second, mirrorAskMaxWait)
	}
	e, err := s.asks.Poll(r.Context(), r.PathValue("id"), owner, askSourceMirror, host, wait)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	out := mirrorAskState{ID: e.ID, State: e.State}
	if e.State == askq.Answered {
		out.Answers = e.Answers
	}
	s.writeJSON(w, http.StatusOK, out)
}

// handleMirrorAskWithdraw is DELETE /mirror/ask/{id}: the hook gave up holding
// the call (the terminal's picker has it now).
func (s *Server) handleMirrorAskWithdraw(w http.ResponseWriter, r *http.Request) {
	_, owner, host, ok := s.askHost(w, r)
	if !ok {
		return
	}
	if err := s.asks.Withdraw(r.PathValue("id"), owner, askSourceMirror, host); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// askWindow resolves a card request's (host, session, window) against the
// viewing principal's hosts: the window's Claude session, and the entry and
// its host. ok false has answered (404: not the principal's host, not a
// listed window, or no Claude session there).
func (s *Server) askWindow(w http.ResponseWriter, r *http.Request, host, session, window string) (h mirror.HostView, e mirror.SessionView, ok bool) {
	if s.ext.Mirror == nil || s.asks == nil {
		http.NotFound(w, r)
		return h, e, false
	}
	h, e, listed, _, found := s.mirrorEntry(r, host, session, window)
	if !found || !listed || e.ClaudeSession == "" {
		http.NotFound(w, r)
		return h, e, false
	}
	return h, e, true
}

// askKey is the key of the call tool in e's Claude session, for the request's
// owner.
func askKey(r *http.Request, h mirror.HostView, e mirror.SessionView, tool string) askq.Key {
	return askq.Key{Owner: requestOwner(r.Context()), Source: askSourceMirror, Scope: h.Host, Session: e.ClaudeSession, ToolUseID: tool}
}

// askLiveState is an entry's card state.
func askLiveState(st askq.State) string {
	switch st {
	case askq.Pending:
		return ui.AskStatePending
	case askq.Answered:
		return ui.AskStateAnswered
	default:
		return ui.AskStateWithdrawn
	}
}

// writeAskLive answers with one card of the live region, its state in
// X-Ask-State.
func (s *Server) writeAskLive(w http.ResponseWriter, status int, v ui.MirrorAskLiveView) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Ask-State", v.State)
	w.WriteHeader(status)
	if err := ui.MirrorAskLive(v).Render(w); err != nil {
		s.logger.Printf("mirror: render question card: %v", err)
	}
}

// mirrorAskShownFor is how long a resolved question stays in the live region
// (until the conversation draws the call, which it usually does at once).
const mirrorAskShownFor = 3 * time.Minute

// handleMirrorAsks is GET ui.MirrorAsksPath?host&session&window: the window's
// live region — a card for every question its Claude session's ask hook is
// holding, and for each resolved in the last mirrorAskShownFor.
func (s *Server) handleMirrorAsks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	h, e, ok := s.askWindow(w, r, q.Get("host"), q.Get("session"), q.Get("window"))
	if !ok {
		return
	}
	var vs []ui.MirrorAskLiveView
	for _, x := range s.asks.ForSession(requestOwner(r.Context()), askSourceMirror, h.Host, e.ClaudeSession, mirrorAskShownFor) {
		vs = append(vs, ui.MirrorAskLiveView{Ref: mirrorRef(e), ToolID: x.Key.ToolUseID, State: askLiveState(x.State), Questions: x.Questions, Answers: x.Answers})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := ui.MirrorAsksLive(vs).Render(w); err != nil {
		s.logger.Printf("mirror: render question cards: %v", err)
	}
}

// handleMirrorAskAnswer is POST ui.MirrorAskPath: a person's answers. 200 with
// the answered card; 403 + X-Muster-Step-Up for a stale sign-in; 409 with the
// card's state when the question is not pending (went back to the terminal,
// answered differently); 400 for answers the question set refuses.
func (s *Server) handleMirrorAskAnswer(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, mirrorAskFormMax)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "the answers are too large", http.StatusRequestEntityTooLarge)
		return
	}
	h, e, ok := s.askWindow(w, r, r.PostFormValue("host"), r.PostFormValue("session"), r.PostFormValue("window"))
	if !ok {
		return
	}
	tool := r.PostFormValue("tool")
	if !toolUseIDPattern.MatchString(tool) {
		http.NotFound(w, r)
		return
	}
	key := askKey(r, h, e, tool)
	if !h.Interactive {
		http.Error(w, mirror.ErrInteractiveRefused.Error(), http.StatusConflict)
		return
	}
	if !stepup.Fresh(r.Context(), s.stepUpNow()) {
		refuseStale(w)
		return
	}
	entry, found := s.asks.Find(key)
	if !found {
		http.NotFound(w, r)
		return
	}
	v := ui.MirrorAskLiveView{Ref: mirrorRef(e), ToolID: key.ToolUseID, Questions: entry.Questions}
	if entry.State == askq.Withdrawn {
		v.State = ui.AskStateWithdrawn
		s.writeAskLive(w, http.StatusConflict, v)
		return
	}
	answers, err := askq.FromForm(entry.Questions, r.PostForm)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	got, err := s.asks.Answer(key, answers)
	switch {
	case err == nil:
		s.logger.Printf("mirror: question %s in %s/%s answered", got.ID, h.Host, e.Session)
		v.State, v.Answers = ui.AskStateAnswered, got.Answers
		s.writeAskLive(w, http.StatusOK, v)
	case errors.Is(err, askq.ErrAnswered):
		v.State, v.Answers = ui.AskStateAnswered, got.Answers
		s.writeAskLive(w, http.StatusConflict, v)
	case errors.Is(err, askq.ErrNotPending):
		v.State = ui.AskStateWithdrawn
		s.writeAskLive(w, http.StatusConflict, v)
	case errors.Is(err, askq.ErrNotFound):
		http.NotFound(w, r)
	default:
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}
