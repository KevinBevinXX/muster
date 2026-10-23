package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/metrics"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/stepup"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// ASKUSERQUESTION IN BOTH PLACES AT ONCE (internal/askq; an operator decision:
// "it should show instantly in both").
//
// When a mirrored session calls AskUserQuestion, the terminal draws its own
// picker at once AND the window's rendered view shows the question at once;
// whichever is answered first wins, and the other side shows how it went.
//
//   - THE HOOK (`muster-mirror ask`, a PreToolUse hook on the operator's own
//     machine) — POST /mirror/ask — registers the call's questions and returns
//     at once, authenticated by its host's MIRROR TOKEN (the same door as
//     /mirror/connect). It never holds the call: the picker shows at once.
//   - THE WATCHER (askWatch) reads the window's transcript, through the host,
//     for the call's result — the CLI's own record of which answer it took —
//     and resolves the question: answered here, answered in the terminal, or
//     dismissed there.
//   - THE PERSON answers in the live region (ui.MirrorAsksPath) by POSTing
//     ui.MirrorAskPath; muster asks the host to TYPE the answer into the
//     picker (mirror.Hub.AnswerWindow → internal/askdrive), which reads the
//     pane before and after every key and never types into a picker that is
//     not this question, untouched — or exactly as muster's own earlier typing
//     left it: a typing that stopped part way records what it typed
//     (askq.Entry.Left), and trying again from the card continues it.
//
// 🔴 AN ANSWER IS A WRITE INTO THE OPERATOR'S SESSION, SO IT PASSES EXACTLY THE
// GATES A SEND PASSES (mirrorSend): a same-origin request from the session's
// owner (another owner's host is not found), a host that advertised
// -interactive, and a FRESH sign-in (internal/stepup). Only then are the answers
// read and checked against the question set (askq.FromForm).
//
// 🔴 WHICH ANSWER WON IS THE CLI'S WORD. Muster's answer is "typing" until the
// transcript records the call's result; if the person in the terminal got there
// first, the host finds no untouched picker and types nothing, and the card
// shows the terminal's answer.
// ---------------------------------------------------------------------------

// mirrorAsker is what the ask routes need from the mirror plugin beyond the
// Mirror interface. *mirror.Service implements it; a plugin that does not has
// no ask routes (404 — the question is then the terminal's only).
type mirrorAsker interface {
	AuthenticateHost(w http.ResponseWriter, r *http.Request) (owner int64, host string, ok bool)
	AnswerWindow(ctx context.Context, host, session, window string, qs []askq.Question, choices []askq.Choice, prior *askq.Progress) error
	Nudge(owner int64)
}

// askSourceMirror is the askq.Key Source of a question a mirror host's hook
// asked; its Scope is the host's name.
const askSourceMirror = "mirror"

const (
	// mirrorAskBodyMax bounds a registration's body: the questions within
	// askq's bounds fit with room to spare.
	mirrorAskBodyMax = 256 << 10
	// mirrorAskFormMax bounds an answer form.
	mirrorAskFormMax = 64 << 10
	// mirrorAskShownFor is how long a resolved question stays in the live
	// region (until the conversation draws the call, which it usually does at
	// once).
	mirrorAskShownFor = 3 * time.Minute
)

// toolUseIDPattern is a tool_use id as accepted here ("toolu_…" in practice).
var toolUseIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// Questions is the question store, keyed surface-neutrally (askq.Key). An
// attention view lists a user's open questions with Questions().Pending(owner).
func (s *Server) Questions() *askq.Store { return s.asks }

// RunAskSweep runs the question store's expiry every interval until ctx ends.
func (s *Server) RunAskSweep(ctx context.Context, interval time.Duration) {
	if s.asks != nil {
		s.asks.RunSweep(ctx, interval)
	}
}

// wireAsks connects the question store to the mirror plugin: every change
// nudges the owner's open views and the attention queue.
func (s *Server) wireAsks(m Mirror) {
	a, ok := m.(mirrorAsker)
	if !ok || s.asks == nil {
		return
	}
	s.asks.OnChange(func(e askq.Entry) {
		if e.Key.Source == askSourceMirror {
			a.Nudge(e.Key.Owner)
			s.AttentionMirrorChanged(e.Key.Owner)
			s.askNotify(askWatchKey{e.Key.Owner, e.Key.Scope, e.Key.Session})
		}
	})
}

func (s *Server) registerMirrorAskRoutes(mux Mux) {
	// 🔴 NO SESSION: the hook is a host, authenticated by its mirror token
	// alone (mirror.Service.AuthenticateHost) — like /mirror/connect.
	mux.HandleFunc("POST /mirror/ask", s.handleMirrorAskOpen)
	// The person: the live region, and an answer.
	mux.HandleFunc("GET "+ui.MirrorAsksPath, s.requireSession(s.handleMirrorAsks))
	mux.HandleFunc("POST "+ui.MirrorAskPath, s.requireOperatorWrite(s.handleMirrorAskAnswer))
	mux.HandleFunc("POST "+ui.MirrorAskCancelPath, s.requireOperatorWrite(s.handleMirrorAskCancel))
}

// mirrorAskCanceller is the plugin's half of dismissing a picker
// (mirror.Service.CancelAsk). A plugin without it has no Cancel control.
type mirrorAskCanceller interface {
	CancelAsk(ctx context.Context, host, session, window string) (note string, err error)
}

// mirrorAskOpenRequest is the hook's registration: the PreToolUse payload's
// session_id, tool_use_id and tool_input, under the hook's own id for it.
type mirrorAskOpenRequest struct {
	ID        string          `json:"id"`
	SessionID string          `json:"sessionId"`
	ToolUseID string          `json:"toolUseId"`
	ToolInput json.RawMessage `json:"toolInput"`
}

// handleMirrorAskOpen is POST /mirror/ask: 201 {id, state}, at once — the
// terminal's picker is waiting for the hook to return.
//
// The question is the token's own owner's and host's (askq.Key): muster shows
// it only in a window that host lists as running that Claude session, and only
// to that owner, so a host can register nothing anywhere else.
func (s *Server) handleMirrorAskOpen(w http.ResponseWriter, r *http.Request) {
	if s.ext.Mirror == nil || s.asks == nil {
		http.NotFound(w, r)
		return
	}
	a, can := s.ext.Mirror.(mirrorAsker)
	if !can {
		http.NotFound(w, r)
		return
	}
	owner, host, ok := a.AuthenticateHost(w, r)
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
	e, err := s.asks.Open(askq.Key{Owner: owner, Source: askSourceMirror, Scope: host, Session: req.SessionID, ToolUseID: req.ToolUseID}, qs, req.ID)
	if err != nil {
		status := http.StatusBadRequest
		switch {
		case errors.Is(err, askq.ErrFull):
			status = http.StatusServiceUnavailable
		case errors.Is(err, askq.ErrIDTaken):
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	s.logger.Printf("mirror: %s asks %d question(s) in session %s (%s)", host, len(qs), req.SessionID, e.ID)
	s.watchAsks(owner, host, req.SessionID)
	s.writeJSON(w, http.StatusCreated, map[string]any{"id": e.ID, "state": e.State})
}

// askWindow resolves a card request's (host, session, window) against the
// viewing principal's hosts: the entry (whose ClaudeSession keys its
// questions; "" for a window with no Claude session, which has none) and its
// host. ok false has answered (404: not the principal's host, or not a listed
// window).
func (s *Server) askWindow(w http.ResponseWriter, r *http.Request, host, session, window string) (h mirror.HostView, e mirror.SessionView, ok bool) {
	if s.ext.Mirror == nil || s.asks == nil {
		http.NotFound(w, r)
		return h, e, false
	}
	h, e, listed, _, found := s.mirrorEntry(r, host, session, window)
	if !found || !listed {
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

// questionCard is entry x as a card of e's window, h being its host.
func questionCard(h mirror.HostView, e mirror.SessionView, x askq.Entry) ui.MirrorAskLiveView {
	v := ui.MirrorAskLiveView{Ref: mirrorRef(e), ToolID: x.Key.ToolUseID, Questions: x.Questions, Answers: x.Answers, Note: x.Note,
		WindowName: e.DisplayTitle(), CanCancel: h.Interactive && h.Online && h.AskCancel}
	switch {
	case x.State == askq.Pending:
		v.State = ui.AskStatePending
	case x.State == askq.Typing:
		v.State, v.Answers = ui.AskStateTyping, x.Typed
	case x.State == askq.Failed:
		v.State, v.Retry = ui.AskStateFailed, x.Retry
		if x.Left != nil {
			v.Chosen = x.Left.Choices
		}
	case x.State == askq.Answered:
		v.State = ui.AskStateAnsweredTerminal
		if x.By == askq.ByMuster {
			v.State = ui.AskStateAnswered
		}
	case x.State == askq.Declined:
		v.State = ui.AskStateDeclined
	case x.State == askq.Superseded:
		v.State = ui.AskStateSuperseded
	default:
		v.State = ui.AskStateExpired
	}
	switch {
	case !h.Interactive:
		v.Unanswerable = ui.MirrorReadOnlyNote
	case !h.Online:
		v.Unanswerable = "This host is offline — answer it in the terminal."
	case !h.AskAnswer:
		v.Unanswerable = "This host's muster-mirror cannot type an answer into the terminal — update it, or answer in the terminal."
	}
	return v
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

// handleMirrorAsks is GET ui.MirrorAsksPath?host&session&window: the window's
// live region — a card for every question its Claude session is asking, and
// for each resolved in the last mirrorAskShownFor.
func (s *Server) handleMirrorAsks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	h, e, ok := s.askWindow(w, r, q.Get("host"), q.Get("session"), q.Get("window"))
	if !ok {
		return
	}
	var vs []ui.MirrorAskLiveView
	if e.ClaudeSession != "" {
		for _, x := range s.asks.ForSession(requestOwner(r.Context()), askSourceMirror, h.Host, e.ClaudeSession, mirrorAskShownFor) {
			vs = append(vs, questionCard(h, e, x))
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := ui.MirrorAsksLive(vs).Render(w); err != nil {
		s.logger.Printf("mirror: render question cards: %v", err)
	}
}

// askPartlyFilled ends a failure note when muster may have typed part of the
// answer before it stopped.
const askPartlyFilled = " The picker may be partly filled in — check what it shows before you submit it in the terminal."

// The failure notes a card can show, by what happened.
const (
	// askNoteLost: muster's own keys stopped part way (the picker did not
	// show what a key should have produced). Muster recorded what it typed,
	// so trying again continues it.
	askNoteLost = "Muster lost track of the terminal's picker part-way through typing your answer, and nothing was submitted. " +
		"Try again: muster continues from where it stopped."
	// askNoteLostUnknown: as askNoteLost, with nothing recorded to continue.
	askNoteLostUnknown = "Muster lost track of the terminal's picker while typing your answer, and nothing was submitted." + askPartlyFilled
	// askNoteTouched: the picker is not as muster left it — someone typed in
	// the terminal.
	askNoteTouched = "Someone else has started answering it in the terminal — finish it there."
)

// askTypeFailure is what a card says when muster could not type the answer
// in: what happened, and what to do next — try again here, or answer in the
// terminal; and, when keys may have been typed before it stopped and muster
// cannot continue them, that the picker may be partly filled in. resumable
// says muster recorded what it typed (the next answer continues it); typed
// that keys of muster's reached the picker before it stopped.
func askTypeFailure(err error, resumable, typed bool) string {
	switch {
	case errors.Is(err, mirror.ErrAskNoPicker):
		return "The question is no longer on the terminal's screen — it was answered or dismissed there."
	// Before lost and touched: muster could not read the question — which
	// says nothing about anyone in the terminal, so the card does not either.
	case errors.Is(err, mirror.ErrAskUnreadable) && errors.Is(err, mirror.ErrAskLost):
		return "Couldn't type it in: the question went off the terminal's screen while typing (its picker is taller than the terminal), and nothing was submitted." + askPartlyFilled
	case errors.Is(err, mirror.ErrAskUnreadable):
		return "Couldn't read this question off the terminal's screen as the one muster was sent — answer it in the terminal."
	case errors.Is(err, mirror.ErrAskTouched) && typed:
		return askNoteTouched + askPartlyFilled
	case errors.Is(err, mirror.ErrAskTouched):
		return askNoteTouched
	case errors.Is(err, mirror.ErrAskBusy):
		return "Another answer is being typed into this terminal — wait for it, then try again (or answer in the terminal)."
	case errors.Is(err, mirror.ErrAskUnrecognised):
		return "Couldn't recognise the terminal's picker (Claude Code may have changed) — answer it in the terminal."
	case errors.Is(err, mirror.ErrAskLost) && resumable:
		return askNoteLost
	case errors.Is(err, mirror.ErrAskLost):
		return askNoteLostUnknown
	case errors.Is(err, mirror.ErrAskUnsupported):
		return "This host's muster-mirror cannot type an answer into the terminal — answer it there."
	case errors.Is(err, mirror.ErrAskNotSent) && (errors.Is(err, mirror.ErrHostOffline) || errors.Is(err, mirror.ErrHostDisconnected)):
		return "The host is offline — answer it in the terminal."
	case errors.Is(err, mirror.ErrAskNotSent):
		return "Couldn't send the answer to the host — answer it in the terminal."
	case errors.Is(err, mirror.ErrHostOffline), errors.Is(err, mirror.ErrHostDisconnected):
		return "The host went offline while typing." + askPartlyFilled
	}
	return "Couldn't type it in." + askPartlyFilled
}

// askFailure is how a drive that ended in err (AnswerWindow's) leaves the
// question: what the card says, whether it can be tried again from muster,
// and what the picker is known to hold of muster's — choices' first keys
// (mirror.AskKeys). A drive whose outcome is unknown records nothing it may
// have typed (askq.Failure.Settled).
func askFailure(err error, choices []askq.Choice) askq.Failure {
	f := askq.Failure{Settled: mirror.AskOutcomeKnown(err)}
	if f.Settled && mirror.AskKeys(err) > 0 {
		f.Left = &askq.Progress{Choices: choices, Keys: mirror.AskKeys(err)}
	}
	// Lost part way with what it typed recorded: continue it — the card
	// resends exactly that answer (ui: a fixed retry). Busy: wait.
	resumable := f.Left != nil && errors.Is(err, mirror.ErrAskLost) && !errors.Is(err, mirror.ErrAskUnreadable)
	f.Retry = resumable || errors.Is(err, mirror.ErrAskBusy)
	f.Note = askTypeFailure(err, resumable, mirror.AskKeys(err) > 0)
	return f
}

// askTypingResult is the muster_mirror_ask_typing_total label for an
// AnswerWindow outcome.
func askTypingResult(err error) string {
	switch {
	case err == nil:
		return "typed"
	case errors.Is(err, mirror.ErrAskNotSent):
		return "not-sent"
	case errors.Is(err, mirror.ErrAskNoPicker):
		return "no-picker"
	case errors.Is(err, mirror.ErrAskUnreadable):
		return "unreadable"
	case errors.Is(err, mirror.ErrAskTouched):
		return "touched"
	case errors.Is(err, mirror.ErrAskLost):
		return "lost"
	case errors.Is(err, mirror.ErrAskUnrecognised):
		return "unrecognised"
	case errors.Is(err, mirror.ErrAskBusy):
		return "busy"
	case errors.Is(err, mirror.ErrAskUnsupported):
		return "unsupported"
	case errors.Is(err, mirror.ErrAskTimeout):
		return "timeout"
	}
	return "error"
}

// handleMirrorAskAnswer is POST ui.MirrorAskPath: a person's answers, typed
// into the terminal's picker. 200 with the card (typing: the CLI's record of
// the result is what resolves it); 403 + X-Muster-Step-Up for a stale sign-in;
// 409 with the card when the question is not open, or when typing failed (the
// card says why, and that the terminal has it); 400 for answers the question
// set (or the picker) cannot take.
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
	if !h.Interactive {
		http.Error(w, mirror.ErrInteractiveRefused.Error(), http.StatusConflict)
		return
	}
	if !stepup.Fresh(r.Context(), s.stepUpNow()) {
		refuseStale(w)
		return
	}
	tool := r.PostFormValue("tool")
	if !toolUseIDPattern.MatchString(tool) {
		http.Error(w, "an answer names the call it answers (tool)", http.StatusBadRequest)
		return
	}
	if e.ClaudeSession == "" {
		http.NotFound(w, r)
		return
	}
	key := askKey(r, h, e, tool)
	entry, found := s.asks.Find(key)
	if !found {
		http.NotFound(w, r)
		return
	}
	if !entry.State.Open() {
		s.writeAskLive(w, http.StatusConflict, questionCard(h, e, entry))
		return
	}
	choices, answers, err := askq.FromForm(entry.Questions, r.PostForm)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a, can := s.ext.Mirror.(mirrorAsker)
	if !can || !h.AskAnswer {
		http.Error(w, mirror.ErrAskUnsupported.Error(), http.StatusConflict)
		return
	}
	got, again, err := s.asks.BeginTyping(key, answers)
	switch {
	case errors.Is(err, askq.ErrNotFound):
		http.NotFound(w, r)
		return
	case errors.Is(err, askq.ErrBusy), errors.Is(err, askq.ErrClosed):
		s.writeAskLive(w, http.StatusConflict, questionCard(h, e, got))
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	case again:
		// The same answers again (a double submit): already typed, or being
		// typed — never typed twice.
		s.writeAskLive(w, http.StatusOK, questionCard(h, e, got))
		return
	}
	// 🔴 NOT THE REQUEST'S CONTEXT: a page that goes away (a dropped
	// connection, a closed tab) must not end the wait while the host may
	// still be typing — that would mark the answer failed and let another
	// drive start into the same picker. The hub bounds the wait itself.
	// got.Left is what an earlier drive is known to have left in the picker:
	// the host continues it when the picker shows exactly that.
	err = a.AnswerWindow(context.WithoutCancel(r.Context()), h.Host, e.Session, e.Window, entry.Questions, choices, got.Left)
	metrics.MirrorAskTyping.WithLabelValues(askTypingResult(err)).Inc()
	if err != nil {
		s.logger.Printf("mirror: typing an answer into %s/%s%s failed: %v", h.Host, e.Session, e.Window, err)
		failed, _ := s.asks.TypingFailed(key, askFailure(err, choices))
		s.writeAskLive(w, http.StatusConflict, questionCard(h, e, failed))
		return
	}
	s.asks.TypingDone(key)
	s.logger.Printf("mirror: typed an answer into %s/%s%s (%s)", h.Host, e.Session, e.Window, got.ID)
	// The watcher resolves it from the CLI's record; a page reading this
	// before then shows "typing".
	now, _ := s.asks.Find(key)
	s.writeAskLive(w, http.StatusOK, questionCard(h, e, now))
}

// askCancelFailure is what a card says when muster could not dismiss its
// picker: what happened, and whether the Escape was sent.
func askCancelFailure(err error) string {
	switch {
	case errors.Is(err, mirror.ErrAskNoPicker):
		return "The question is no longer on the terminal's screen — nothing was sent."
	case errors.Is(err, mirror.ErrAskBusy):
		return "An answer is being typed into this terminal — nothing was sent. Try again once it is done."
	case errors.Is(err, mirror.ErrAskTouched):
		return "The terminal's picker changed while muster was reading it — nothing was sent."
	case errors.Is(err, mirror.ErrAskUnreadable), errors.Is(err, mirror.ErrAskUnrecognised):
		return "Couldn't read the terminal's picker as this question — nothing was sent. Press Esc in the terminal."
	case errors.Is(err, mirror.ErrAskCancelUnsupported), errors.Is(err, mirror.ErrAskUnsupported):
		return "This host's muster-mirror cannot dismiss a question — press Esc in the terminal."
	case errors.Is(err, mirror.ErrAskNotSent) && (errors.Is(err, mirror.ErrHostOffline) || errors.Is(err, mirror.ErrHostDisconnected)):
		return "The host is offline — nothing was sent."
	case errors.Is(err, mirror.ErrAskNotSent):
		return "Couldn't reach the host — nothing was sent."
	}
	return "Couldn't dismiss it — check the terminal."
}

// handleMirrorAskCancel is POST ui.MirrorAskCancelPath: dismiss a question's
// picker in the terminal with ONE Escape (the host's askdrive.Cancel). 200
// with the card, saying the Esc was sent (the CLI's record of the call ending
// unanswered then resolves it, as any dismissal); 409 with the card when the
// question is not open,
// or with what happened (text) when the host refused; 404 for a question
// that is not the request's owner's.
//
// 🔴 THE SAME GATES AS AN ANSWER BUT ONE (handleMirrorAskAnswer): a
// same-origin request from the session's owner (requireOperatorWrite;
// another owner's host is not found), a host that advertised -interactive
// and AskCancel — and NO step-up re-authentication, an operator decision:
// the page asks for confirmation in a dialog naming the window. The host
// checks the rest: a picker on screen, the pane's one-drive lock.
func (s *Server) handleMirrorAskCancel(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, mirrorAskFormMax)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "the request is too large", http.StatusRequestEntityTooLarge)
		return
	}
	h, e, ok := s.askWindow(w, r, r.PostFormValue("host"), r.PostFormValue("session"), r.PostFormValue("window"))
	if !ok {
		return
	}
	if !h.Interactive {
		http.Error(w, mirror.ErrInteractiveRefused.Error(), http.StatusConflict)
		return
	}
	tool := r.PostFormValue("tool")
	if !toolUseIDPattern.MatchString(tool) {
		http.Error(w, "a dismissal names the call it dismisses (tool)", http.StatusBadRequest)
		return
	}
	if e.ClaudeSession == "" {
		http.NotFound(w, r)
		return
	}
	key := askKey(r, h, e, tool)
	entry, err := s.asks.Dismissing(key)
	switch {
	case errors.Is(err, askq.ErrNotFound):
		http.NotFound(w, r)
		return
	case errors.Is(err, askq.ErrClosed), errors.Is(err, askq.ErrBusy):
		s.writeAskLive(w, http.StatusConflict, questionCard(h, e, entry))
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	c, can := s.ext.Mirror.(mirrorAskCanceller)
	if !can || !h.AskCancel {
		http.Error(w, askCancelFailure(mirror.ErrAskCancelUnsupported), http.StatusConflict)
		return
	}
	// Not the request's context, as for an answer: a page that goes away must
	// not end the wait while the host may be sending the key.
	note, err := c.CancelAsk(context.WithoutCancel(r.Context()), h.Host, e.Session, e.Window)
	if err != nil {
		s.logger.Printf("mirror: dismissing a question in %s/%s%s failed: %v", h.Host, e.Session, e.Window, err)
		http.Error(w, askCancelFailure(err), http.StatusConflict)
		return
	}
	s.logger.Printf("mirror: dismissed a question in %s/%s%s with Escape (%s)", h.Host, e.Session, e.Window, entry.ID)
	v := questionCard(h, e, entry)
	// The host's note says the picker is still showing (the Escape did not
	// take): the question is still the person's to answer or dismiss.
	v.Sent, v.SentShowing = note, note != ""
	if note == "" {
		v.Sent = "Esc sent — the question left the terminal's screen."
	}
	s.writeAskLive(w, http.StatusOK, v)
}

// ---------------------------------------------------------------------------
// THE QUESTION WATCHER: for every (owner, host, Claude session) with open
// questions, the window's transcript, read through the host (WatchWindow), for
// each call's result — a tool_result whose record carries toolUseResult
// {questions, answers} (cctranscript.Event.Ask) answered it; one without
// answers (a dismissed picker) declined it. One watch per session, ended once
// it has no open question; restarted while it has one (a host reconnect, a
// window not listed yet).
// ---------------------------------------------------------------------------

type askWatchKey struct {
	owner         int64
	host, session string
}

// askWatchRetry is how long a watch waits before looking again for the window
// (not listed yet, host offline) or after its stream ended.
const askWatchRetry = 2 * time.Second

// askGoneGrace is how long a session with open questions may be missing from
// its host's windows (the host reconnecting, Claude restarting) before its
// questions are expired as gone.
const askGoneGrace = 30 * time.Second

// askGoneNote is the card's note for a question whose session went away.
const askGoneNote = "Claude's session in this window ended (or the window closed) before the question was answered."

// askWatchers is the set of running watches.
type askWatchers struct {
	mu      sync.Mutex
	running map[askWatchKey]bool
	// retry replaces askWatchRetry, grace askGoneGrace (tests; 0 = default).
	retry, grace time.Duration
	// subs are the rendered views' sockets listening for k's questions
	// (askSubscribe).
	subs map[askWatchKey]map[chan struct{}]bool
}

func (w *askWatchers) goneGrace() time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.grace > 0 {
		return w.grace
	}
	return askGoneGrace
}

func (w *askWatchers) retryEvery() time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.retry > 0 {
		return w.retry
	}
	return askWatchRetry
}

// askPrincipal is ctx acting as owner, through authz.ViaAsk: how the watcher
// reads that owner's window's transcript. Its one caller is watchAsks, handed
// the owner of the question a host's hook registered; internal/authz's
// withPrincipalLedger lists it.
func (s *Server) askPrincipal(ctx context.Context, owner int64) context.Context {
	return authz.WithPrincipal(ctx, s.principalFor(ctx, owner, authz.ViaAsk))
}

// watchAsks makes sure a watch runs for (owner, host, claudeSession).
func (s *Server) watchAsks(owner int64, host, claudeSession string) {
	k := askWatchKey{owner, host, claudeSession}
	s.askWatch.mu.Lock()
	if s.askWatch.running == nil {
		s.askWatch.running = map[askWatchKey]bool{}
	}
	if s.askWatch.running[k] {
		s.askWatch.mu.Unlock()
		return
	}
	s.askWatch.running[k] = true
	s.askWatch.mu.Unlock()
	safeGo(s.logger, "mirror: watch a session's questions", func() {
		defer func() {
			s.askWatch.mu.Lock()
			delete(s.askWatch.running, k)
			s.askWatch.mu.Unlock()
			// A question opened as this watch was ending: watch again.
			if len(s.asks.OpenIn(owner, askSourceMirror, host, claudeSession)) > 0 && s.streams.context().Err() == nil {
				s.watchAsks(owner, host, claudeSession)
			}
		}()
		s.runAskWatch(s.askPrincipal(s.streams.context(), owner), k)
	})
}

// runAskWatch is one session's watch, until it has no open question or the
// server ends.
//
// 🔴 A SESSION THAT GOES AWAY TAKES ITS QUESTIONS WITH IT: once the host has
// been ONLINE and not listed the Claude session for askGoneGrace in all
// (Claude exited, the window closed), its open questions are expired with
// askGoneNote — not left "asking" until MaxOpen. Time the host is OFFLINE does
// not count: a host reconnecting (its backoff reaches a minute) is not a
// session ending. And an expired question is still watched while retained: a
// result that turns up later (the session came back) still resolves it.
func (s *Server) runAskWatch(ctx context.Context, k askWatchKey) {
	open := func() bool { return len(s.asks.OpenIn(k.owner, askSourceMirror, k.host, k.session)) > 0 }
	watched := func() bool { return len(s.asks.Watched(k.owner, askSourceMirror, k.host, k.session)) > 0 }
	var missing time.Duration // online and not listing the session, in all
	var missingAt time.Time   // when that was last seen (zero: not now)
	for ctx.Err() == nil && watched() {
		e, found := s.askSessionWindow(ctx, k)
		now := time.Now()
		switch {
		case found:
			missing, missingAt = 0, time.Time{}
		case !s.askHostOnline(ctx, k.host):
			missingAt = time.Time{} // paused while the host is away
		default:
			if !missingAt.IsZero() {
				missing += now.Sub(missingAt)
			}
			missingAt = now
			if missing >= s.askWatch.goneGrace() && open() {
				if n := s.asks.Gone(k.owner, askSourceMirror, k.host, k.session, askGoneNote); n > 0 {
					s.logger.Printf("mirror: %d question(s) of a Claude session on %s expired: the session is gone", n, k.host)
				}
			}
		}
		if found {
			wctx, cancel := context.WithCancel(ctx)
			// Stop once nothing is open (resolved here, or expired).
			stop := make(chan struct{})
			safeGo(s.logger, "mirror: end a question watch", func() {
				t := time.NewTicker(s.askWatch.retryEvery())
				defer t.Stop()
				for {
					select {
					case <-stop:
						return
					case <-t.C:
						// Nothing open any more, or the session left its
						// window: end this watch (the loop above decides).
						if _, listed := s.askSessionWindow(ctx, k); !watched() || !listed {
							cancel()
							return
						}
					}
				}
			})
			_ = s.ext.Mirror.WatchWindow(wctx, e.Host, e.Session, e.Window, "", 0, func(m mirror.Msg) error {
				if m.Gen != "" && m.Gen != k.session {
					cancel() // another conversation is in the window now
					return nil
				}
				s.resolveAsks(k, m.Events)
				if !watched() {
					cancel()
				}
				return nil
			})
			close(stop)
			cancel()
		}
		select {
		case <-ctx.Done():
		case <-time.After(s.askWatch.retryEvery()):
		}
	}
}

// askHostOnline reports whether owner's host is online now.
func (s *Server) askHostOnline(ctx context.Context, host string) bool {
	for _, h := range s.ext.Mirror.Hosts(ctx) {
		if h.Host == host {
			return h.Online
		}
	}
	return false
}

// askSessionWindow is the window of k's host that runs k's Claude session.
func (s *Server) askSessionWindow(ctx context.Context, k askWatchKey) (mirror.SessionView, bool) {
	for _, h := range s.ext.Mirror.Hosts(ctx) {
		if h.Host != k.host || !h.Online {
			continue
		}
		for _, e := range h.Entries() {
			if e.Claude && e.ClaudeSession == k.session {
				return e, true
			}
		}
	}
	return mirror.SessionView{}, false
}

// resolveAsks records the results among evs of k's open questions.
func (s *Server) resolveAsks(k askWatchKey, evs []cctranscript.Event) {
	for _, ev := range evs {
		if ev.Kind != cctranscript.KindToolResult || ev.ToolID == "" {
			continue
		}
		var answers map[string]string
		if ev.Ask != nil && ev.Ask.Answers != nil {
			answers = ev.Ask.Answers
		}
		// Only the calls this session asked: any other tool's result names a
		// tool id with no question, and Resolve leaves it alone.
		if e, ok := s.asks.Resolve(k.owner, askSourceMirror, k.host, k.session, ev.ToolID, answers); ok {
			s.logger.Printf("mirror: question %s in %s resolved: %s %s", e.ID, k.host, e.State, strings.TrimSpace(e.By))
		}
	}
}

// askSubscribe is a channel that receives (coalesced) whenever a question of
// k changes, and its cancel: what a rendered view's socket listens on.
func (s *Server) askSubscribe(k askWatchKey) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.askWatch.mu.Lock()
	if s.askWatch.subs == nil {
		s.askWatch.subs = map[askWatchKey]map[chan struct{}]bool{}
	}
	if s.askWatch.subs[k] == nil {
		s.askWatch.subs[k] = map[chan struct{}]bool{}
	}
	s.askWatch.subs[k][ch] = true
	s.askWatch.mu.Unlock()
	return ch, func() {
		s.askWatch.mu.Lock()
		delete(s.askWatch.subs[k], ch)
		if len(s.askWatch.subs[k]) == 0 {
			delete(s.askWatch.subs, k)
		}
		s.askWatch.mu.Unlock()
	}
}

// askNotify tells k's subscribers that a question of k changed.
func (s *Server) askNotify(k askWatchKey) {
	s.askWatch.mu.Lock()
	defer s.askWatch.mu.Unlock()
	for ch := range s.askWatch.subs[k] {
		select {
		case ch <- struct{}{}:
		default: // one pending signal is enough
		}
	}
}
