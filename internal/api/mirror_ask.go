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

	"github.com/ZacxDev/muster/internal/askdrive"
	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/cctranscript"
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
//     not this question, untouched.
//
// 🔴 AN ANSWER IS A WRITE INTO THE OPERATOR'S SESSION, SO IT PASSES EXACTLY THE
// GATES A SEND PASSES (mirrorSend): a same-origin request from the session's
// owner (another owner's host is not found), a host that advertised
// -interactive, and a FRESH sign-in (internal/stepup). Only then are the answers
// read and checked against the question set (askq.FromForm, askdrive.Plan).
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
	AnswerWindow(ctx context.Context, host, session, window string, qs []askq.Question, choices []askq.Choice) error
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
	v := ui.MirrorAskLiveView{Ref: mirrorRef(e), ToolID: x.Key.ToolUseID, Questions: x.Questions, Answers: x.Answers, Note: x.Note}
	switch x.State {
	case askq.Pending:
		v.State = ui.AskStatePending
	case askq.Typing:
		v.State, v.Answers = ui.AskStateTyping, x.Typed
	case askq.Failed:
		v.State = ui.AskStateFailed
	case askq.Answered:
		v.State = ui.AskStateAnsweredTerminal
		if x.By == askq.ByMuster {
			v.State = ui.AskStateAnswered
		}
	case askq.Declined:
		v.State = ui.AskStateDeclined
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

// askTypeFailure is what a card says when muster could not type the answer
// in: what happened, and that the terminal has the question.
func askTypeFailure(err error) string {
	switch {
	case errors.Is(err, mirror.ErrAskNoPicker):
		return "The question is no longer on the terminal's screen — it was answered or dismissed there."
	case errors.Is(err, mirror.ErrAskTouched):
		return "Someone has started answering it in the terminal — finish it there."
	case errors.Is(err, mirror.ErrAskLost):
		return "Couldn't type it in: the terminal's picker changed while typing, and nothing was submitted. Answer it in the terminal."
	case errors.Is(err, mirror.ErrAskUnsupported):
		return "This host's muster-mirror cannot type an answer into the terminal — answer it there."
	case errors.Is(err, mirror.ErrHostOffline), errors.Is(err, mirror.ErrHostDisconnected):
		return "The host went offline — answer it in the terminal."
	}
	return "Couldn't type it in — answer it in the terminal."
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
	if _, err := askdrive.Plan(entry.Questions, choices); err != nil {
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
	if err := a.AnswerWindow(r.Context(), h.Host, e.Session, e.Window, entry.Questions, choices); err != nil {
		s.logger.Printf("mirror: typing an answer into %s/%s%s failed: %v", h.Host, e.Session, e.Window, err)
		failed, _ := s.asks.TypingFailed(key, askTypeFailure(err))
		s.writeAskLive(w, http.StatusConflict, questionCard(h, e, failed))
		return
	}
	s.logger.Printf("mirror: typed an answer into %s/%s%s (%s)", h.Host, e.Session, e.Window, got.ID)
	// The watcher resolves it from the CLI's record; a page reading this
	// before then shows "typing".
	now, _ := s.asks.Find(key)
	s.writeAskLive(w, http.StatusOK, questionCard(h, e, now))
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
var askWatchRetry = 2 * time.Second

// askWatchers is the set of running watches.
type askWatchers struct {
	mu      sync.Mutex
	running map[askWatchKey]bool
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
func (s *Server) runAskWatch(ctx context.Context, k askWatchKey) {
	open := func() bool { return len(s.asks.OpenIn(k.owner, askSourceMirror, k.host, k.session)) > 0 }
	for ctx.Err() == nil && open() {
		e, found := s.askSessionWindow(ctx, k)
		if found {
			wctx, cancel := context.WithCancel(ctx)
			// Stop once nothing is open (resolved here, or expired).
			stop := make(chan struct{})
			go func() {
				t := time.NewTicker(askWatchRetry)
				defer t.Stop()
				for {
					select {
					case <-stop:
						return
					case <-t.C:
						if !open() {
							cancel()
							return
						}
					}
				}
			}()
			_ = s.ext.Mirror.WatchWindow(wctx, e.Host, e.Session, e.Window, "", 0, func(m mirror.Msg) error {
				if m.Gen != "" && m.Gen != k.session {
					cancel() // another conversation is in the window now
					return nil
				}
				s.resolveAsks(k, m.Events)
				if !open() {
					cancel()
				}
				return nil
			})
			close(stop)
			cancel()
		}
		select {
		case <-ctx.Done():
		case <-time.After(askWatchRetry):
		}
	}
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
