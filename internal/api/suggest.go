package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/suggest"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// SUGGESTED REPLIES' server half (internal/suggest): when a set is generated,
// for which sessions, and the routes the composer reads.
//
// 🔴 ONLY WHILE A RENDERED VIEW IS OPEN. The trigger lives in the rendered
// view's own socket (handleMirrorViewWS feeds a suggestTracker every batch it
// relays): a session nobody is looking at never generates. The socket already
// carries the transcript, so a generation reads nothing from the host that the
// view did not already ask for. The socket also tells the page what happens
// ({"type":"suggest","state":"pending"|"ready"|"none"}, suggestSocketMsg);
// the page reads the set itself from GET /mirror/view/suggest.
//
// 🔴 AT TURN END ONLY. The tracker fires when the host's busy marker falls
// (a turn ended while the view was open), when the status's TIME moves to a
// non-busy status (a turn the host never saw busy), or when the backfill of a
// view opened on an idle session completes (the prompt index, which follows
// it). Then the window must be IDLE by its own status — not busy, not WAITING
// (a permission prompt or a question: a paste there would answer the prompt),
// not unknown — and the final reply must hand control back
// (suggest.HandsBack).
//
// 🔴 THE TURN'S OWN STATUS, BY ITS TOKEN. A host that sends Msg.StatusSince
// names, on the very marker that ends the turn, the status token of the turn's
// own idle; the turn end waits for the window list to read THAT token, and
// gives up when it reads a later one. So a socket that is backed up (the turn
// already over when its busy marker arrives) and two turns back to back with
// no idle seen between them are each judged by their own token, never by the
// idle from before them. 🔴 A HOST THAT SENDS NO TOKEN (an older
// muster-mirror) GETS NO CHIPS: without it a turn end cannot be told from the
// idle before the turn (the hosts get the new muster-mirror in the same
// rollout).
//
// 🔴 WITHIN ui.SuggestCutoff OR NOT AT ALL. Nothing is generated once the
// cutoff has passed since the turn ended, and the page shows no chip that
// arrives after it.
//
// 🔴 THE EXCLUSIONS (suggestBlocked), checked at generation AND again whenever
// the composer reads the set — so a set made while idle is not served once the
// session is busy or waiting again:
//   - MUSTER_SUGGEST=off, the owner's toggle;
//   - a window that is not a Claude Code pane with a session id: Hermes, a
//     plain terminal (the Terminal view has no tracker at all);
//   - a host that is offline, has no rendered view, or is not -interactive
//     (the composer is disabled there);
//   - a status other than idle;
//   - an OPEN question card (askq) for the session;
//   - an OPEN attention ask raised by the session — whether or not this device
//     hid it ("Hide until next ask" is a per-device display choice; hidden is
//     not answered, and the server never sees it, so every open ask counts).
//
// 🔴 NEVER SENT. Nothing here pastes; GET /mirror/view/suggest only returns
// text, and the composer only fills.
// ---------------------------------------------------------------------------

// The trigger's bounds.
const (
	// suggestTrackEntries is how many conversation entries a tracker keeps
	// (suggest.Build reads only the newest few, and R).
	suggestTrackEntries = 64
	// suggestTrackAsks is how many of the operator's messages a tracker keeps
	// (suggest.Build then keeps the newest within suggest.MaxAsksBytes).
	suggestTrackAsks = 200
	// suggestIdleWait bounds how long a turn end waits for the window's status
	// to read idle: the transcript's busy marker and the window list are two
	// channels from the host and need not land together (a host that sends
	// the status token nudges its list with the marker; an older one re-reads
	// it every 5 s). It is the chips' cutoff, measured from the turn end: past
	// it nothing would be shown, so nothing is generated.
	suggestIdleWait = ui.SuggestCutoff
	// suggestIdlePoll is how often it looks.
	suggestIdlePoll = 250 * time.Millisecond
	// suggestTimeout bounds one generation end to end.
	suggestTimeout = 40 * time.Second
)

// suggestTracker is one rendered view socket's running copy of what a
// suggestion needs: the newest entries, the operator's asks, and whether a
// turn is running. It is fed every batch the socket relays (observe).
type suggestTracker struct {
	mu      sync.Mutex
	entries []suggest.Entry
	asks    []askRef
	askBase int
	// full is the whole text of every prompt the tracker has read a user
	// record of, by its line (askRef.line).
	full map[int64]string
	// indexed: the host sent a prompt index, so asks come from it; without
	// one (an older host) the tail's own prompts stand in.
	indexed bool
	busy    bool
	// started: the tracker has seen a batch (so a busy marker it sees after
	// is a transition, not the stream's first word).
	started bool
	// resumed: the socket resumed at an offset, so the tracker holds only
	// what came after it — no backfill, no prompt index. It ends no turn
	// until a prompt, a Reset or a prompt index gives it a whole turn (so a
	// reloaded page shows no row for the turn it reloaded into).
	resumed bool
	// since is the newest status token the STREAM named (Msg.StatusSince;
	// 0 from an older host), and end what the newest turn end saw.
	since int64
	end   suggestEnd
	// opened: the newest turn end is the backfill of a view opened on an
	// idle session (suggestEnd.open).
	opened bool
}

// suggestEnd is one turn end: the stream's status token at it (0 = the host
// sends none: no chips) and when muster saw it.
type suggestEnd struct {
	since int64
	at    time.Time
	// open: the turn end is the backfill of a view OPENED on an idle session,
	// not a turn that ended while the view was open. Its set is served, but
	// the page does not count it in the auto-pause's tally (suggestSocketMsg.Open):
	// opening a session to read it is not a decision to ignore its chips.
	open bool
}

// lastEnd is the newest turn end observe reported.
func (t *suggestTracker) lastEnd() suggestEnd {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.end
}

// movedOn reports whether the STREAM has moved past turn end e: a newer
// status token, or a turn running (busy). The window list can lag the stream
// by a refresh, so the list alone cannot say a set is still for "now".
func (t *suggestTracker) movedOn(e suggestEnd) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.busy || t.since != e.since
}

// token is the newest status token the stream named, 0 for none.
func (t *suggestTracker) token() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.since
}

// observe folds m in and reports whether it ends a turn: the busy marker fell,
// the status token moved to a non-busy status, or the backfill of an idle
// session completed (its prompt index).
func (t *suggestTracker) observe(m mirror.Msg) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.opened = false
	ended := t.observeLocked(m)
	if ended {
		t.end = suggestEnd{since: t.since, at: time.Now(), open: t.opened}
	}
	return ended
}

func (t *suggestTracker) observeLocked(m mirror.Msg) bool {
	if m.Reset {
		t.entries, t.asks, t.askBase, t.indexed, t.resumed, t.full = nil, nil, 0, false, false, nil
		// A new conversation: its backfill names its own token.
		t.since = 0
	}
	// The status token moved (a token already known, then another): a turn
	// ran, whether or not the busy flag was ever seen up. The first token a
	// stream names (its backfill) is not a move.
	moved := m.StatusSince != 0 && t.since != 0 && m.StatusSince != t.since
	if m.StatusSince != 0 {
		t.since = m.StatusSince
	}
	for _, ev := range m.Events {
		en, ok := suggestEntryOf(ev)
		if !ok {
			continue
		}
		t.entries = append(t.entries, en)
		if en.Role == suggest.RoleUser {
			// The prompt WHOLE, as its user record has it: what the asks are
			// built from, wherever the index names the same prompt by Line.
			if ev.Line > 0 {
				t.rememberFull(ev.Line, ev.Text)
			}
			if t.indexed {
				t.asks = append(t.asks, askRef{line: ev.Line, text: ev.Text})
			}
			// A prompt after the resume point: from here the tail holds whole
			// turns (asks then come from the tail, as for an older host).
			t.resumed = false
		}
	}
	if len(t.entries) > suggestTrackEntries {
		t.entries = append([]suggest.Entry(nil), t.entries[len(t.entries)-suggestTrackEntries:]...)
	}
	if m.PromptIndex {
		t.indexed = true
		t.asks = t.asks[:0]
		for _, p := range m.Prompts {
			t.asks = append(t.asks, askRef{line: p.Line, text: p.Text})
		}
		t.askBase = m.PromptsMore
	}
	if len(t.asks) > suggestTrackAsks {
		drop := len(t.asks) - suggestTrackAsks
		t.askBase += drop
		t.asks = append([]askRef(nil), t.asks[drop:]...)
	}
	if m.PromptIndex {
		t.resumed = false
	}
	wasBusy, started := t.busy, t.started
	t.busy, t.started = m.Busy, true
	switch {
	case t.resumed:
		return false
	case m.PromptIndex && !m.Busy:
		t.opened = true
		return true
	case started && wasBusy && !m.Busy:
		return true
	case started && moved && !m.Busy:
		return true
	}
	return false
}

// askRef is one of the operator's messages: where it is in the transcript
// (cctranscript Event.Line / PromptRef.Line, 0 unknown) and its text as the
// tracker first had it (the index's first line, or the whole prompt).
type askRef struct {
	line int64
	text string
}

// rememberFull keeps a prompt's whole text by its line, at most
// suggestTrackAsks of them (the oldest forgotten first).
func (t *suggestTracker) rememberFull(line int64, text string) {
	if t.full == nil {
		t.full = map[int64]string{}
	}
	t.full[line] = text
	for len(t.full) > suggestTrackAsks {
		oldest := int64(-1)
		for l := range t.full {
			if oldest < 0 || l < oldest {
				oldest = l
			}
		}
		delete(t.full, oldest)
	}
}

// input is the tracker's state as a suggest.Input (focus not filled). Each
// ask is the prompt WHOLE when the tracker read its user record (the view's
// backfill and everything since), and the prompt index's first line only for
// a prompt older than that.
func (t *suggestTracker) input() suggest.Input {
	t.mu.Lock()
	defer t.mu.Unlock()
	in := suggest.Input{Tail: append([]suggest.Entry(nil), t.entries...)}
	if t.indexed {
		in.AskBase = t.askBase
		for _, a := range t.asks {
			text := a.text
			if full, ok := t.full[a.line]; ok && a.line > 0 {
				text = full
			}
			in.Asks = append(in.Asks, text)
		}
		return in
	}
	for _, e := range t.entries {
		if e.Role == suggest.RoleUser {
			in.Asks = append(in.Asks, e.Text)
		}
	}
	return in
}

// suggestEntryOf is one transcript event as a suggestion's entry. The SAME
// redaction rule as focus (focusEntryOf): a prompt and a reply as written, a
// tool call as its name and file path only (focus.ToolLine, from its input —
// never ev.Target, which for Bash is the command line), and NOTHING else: no
// tool output, no thinking, no notices — and, unlike focus, no errors.
func suggestEntryOf(ev cctranscript.Event) (suggest.Entry, bool) {
	switch ev.Kind {
	case cctranscript.KindPrompt:
		return suggest.Entry{Role: suggest.RoleUser, Text: ev.Text}, true
	case cctranscript.KindText:
		return suggest.Entry{Role: suggest.RoleAssistant, Text: ev.Text}, true
	case cctranscript.KindToolCall:
		return suggest.Entry{Role: suggest.RoleTool, Text: focus.ToolLine(ev.Tool, ev.Input)}, true
	}
	return suggest.Entry{}, false
}

// The exclusion reasons (suggestBlocked). Fixed words: they reach the
// composer's JSON, and carry nothing from the session.
const (
	suggestBlockedOff        = "suggestions are off"
	suggestBlockedUserOff    = "you turned suggestions off"
	suggestBlockedPaused     = "suggestions are paused: you rarely used them (turn them back on in Account)"
	suggestBlockedNotClaude  = "not a Claude Code session"
	suggestBlockedNoView     = "no rendered view"
	suggestBlockedReadOnly   = "the host is read-only"
	suggestBlockedNotIdle    = "the session is not idle"
	suggestBlockedGuardHeld  = "the prompt guard is holding the session"
	suggestBlockedQuestion   = "a question is open"
	suggestBlockedAttention  = "an attention ask is open"
	suggestBlockedNoSuggests = ""
)

// suggestBlocked is why (owner's) window e of host h may not show or generate
// suggestions now, "" when it may. It reads the window's status, the question
// store and the attention queue — never the transcript.
func (s *Server) suggestBlocked(ctx context.Context, h mirror.HostView, e mirror.SessionView) string {
	eng := s.ext.Suggest
	switch eng.Enabled(ctx, requestOwner(ctx)) {
	case suggest.OutcomeOff:
		return suggestBlockedOff
	case suggest.OutcomeUserOff:
		return suggestBlockedUserOff
	case suggest.OutcomePaused:
		return suggestBlockedPaused
	}
	if !e.Claude || e.ClaudeSession == "" || e.HermesView() || e.Runtime != "" {
		return suggestBlockedNotClaude
	}
	if !e.HasRenderedView() {
		return suggestBlockedNoView
	}
	if !h.Interactive {
		return suggestBlockedReadOnly
	}
	st := s.ext.Mirror.AgentState(ctx, h.Host, e.Session, e.Window)
	if st.Unknown != nil || st.Status != mirror.StatusIdle {
		return suggestBlockedNotIdle
	}
	// The Claude prompt guard (#255) is holding the window: what is on its
	// screen is not the idle prompt the status says (a dialog, text in the
	// box), so nothing is offered to type there.
	if st.GuardHeld != "" {
		return suggestBlockedGuardHeld
	}
	if s.asks != nil && len(s.asks.OpenIn(requestOwner(ctx), askSourceMirror, h.Host, e.ClaudeSession)) > 0 {
		return suggestBlockedQuestion
	}
	if s.ext.Attention != nil {
		open, err := s.ext.Attention.Open(ctx)
		if err != nil {
			// Fail closed: an ask we cannot rule out blocks.
			return suggestBlockedAttention
		}
		for _, a := range open {
			if a.Asker.MirrorHost == h.Host && a.Asker.MirrorSession == e.ClaudeSession {
				return suggestBlockedAttention
			}
		}
	}
	return suggestBlockedNoSuggests
}

// suggestTurn is suggestTurnAt for the tracker's newest turn end, with no
// pending callback.
func (s *Server) suggestTurn(ctx context.Context, host, session, window string, t *suggestTracker) bool {
	return s.suggestTurnAt(ctx, host, session, window, t, t.lastEnd(), nil)
}

// suggestTurnAt is the WORK of one turn end on (host, session, window): wait
// for the window to read idle — by the turn's own status token when the
// stream named one — check the exclusions and the pre-gate, build the input
// and ask the engine. pending, when set, is called with how long ago the turn
// ended just before the engine is asked (the page reserves the chip row). It
// reports whether a set was newly generated or served from the cache (so the
// socket tells the page to fetch it).
func (s *Server) suggestTurnAt(ctx context.Context, host, session, window string, t *suggestTracker, end suggestEnd, pending func(time.Duration)) bool {
	if s.ext.Suggest == nil || s.ext.Mirror == nil {
		return false
	}
	if end.at.IsZero() {
		end.at = time.Now()
	}
	ctx, cancel := context.WithTimeout(ctx, suggestTimeout)
	defer cancel()
	in := t.input()
	reply, _, ok := suggest.FinalReply(in.Tail)
	if !ok || !suggest.HandsBack(reply) {
		return false
	}
	h, e, ok := s.suggestWindow(ctx, host, session, window)
	if !ok {
		return false
	}
	// The status may trail the stream's busy marker: wait (bounded, from the
	// turn end) for idle, and give up the moment it is anything that blocks
	// for another reason.
	deadline := end.at.Add(s.suggestIdleWait())
	for {
		why := s.suggestBlocked(ctx, h, e)
		st := s.ext.Mirror.AgentState(ctx, h.Host, e.Session, e.Window)
		switch {
		case st.Since > end.since:
			// 🔴 A LATER STATUS THAN THE TURN'S OWN: the window moved on (a new
			// turn, a prompt). That status's own marker ends its own turn. 🔴 This
			// is also NO TOKEN, NO CHIPS: a host that names none (an older
			// muster-mirror) ends turns at token 0, which every listed status is
			// later than — its turn's own status cannot be named, so the idle
			// before the turn could be taken for it (TestNoTokenNoChips).
			return false
		case st.Since != end.since && (why == "" || why == suggestBlockedNotIdle):
			// The list has not caught up with the stream: whatever it reads
			// (the idle or the busy from BEFORE this turn's end) is not this
			// turn's status. Wait for the token itself.
			why = suggestBlockedNotIdle
		}
		if why == "" {
			break
		}
		if why != suggestBlockedNotIdle || time.Now().After(deadline) {
			return false
		}
		// A permission prompt or a question ends the wait at once — its own
		// (not one from before the turn ended). Busy and unknown
		// are what a lagging status reads (the stream said the turn ended), so
		// those are waited out until the deadline.
		if st.Status == mirror.StatusWaiting && st.Since >= end.since {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(suggestIdlePoll):
		}
	}
	// 🔴 THE TURN TOKEN IS READ WITH THE INPUT, and checked again just before
	// the engine is asked: a turn that started (and even ended) while the
	// checks ran would otherwise stamp THIS input's set with the NEXT turn's
	// token, and GET would serve it as that turn's.
	turn := s.ext.Mirror.AgentState(ctx, h.Host, e.Session, e.Window).Since
	if turn != end.since {
		return false
	}
	in = t.input()
	if reply2, _, ok := suggest.FinalReply(in.Tail); !ok || reply2 != reply {
		return false
	}
	in.Focus, in.Now = s.suggestFocusHistory(ctx, e.ClaudeSession), time.Now()
	b, ok := suggest.Build(in)
	if !ok {
		return false
	}
	if turn == 0 || s.ext.Mirror.AgentState(ctx, h.Host, e.Session, e.Window).Since != turn {
		return false
	}
	// 🔴 PAST THE CUTOFF NOTHING IS SPENT: the page would not show it.
	since := time.Since(end.at)
	if since >= s.suggestIdleWait() {
		return false
	}
	// An owner with no OpenRouter key gets no set: say nothing (no row is
	// reserved for one that cannot come).
	if kc, ok := s.ext.Suggest.Gen.(suggest.KeyChecker); ok && kc.CanGenerate(ctx, requestOwner(ctx)) != nil {
		return false
	}
	// 🔴 THE STREAM MOVED ON (a new prompt, busy again) while the list still
	// read this turn's idle: nothing for a turn that is over.
	if t.movedOn(end) {
		return false
	}
	if pending != nil {
		pending(since)
	}
	set, out, err := s.ext.Suggest.Suggest(ctx, requestOwner(ctx), suggestSubject(h.Host, e), b, turn)
	if err != nil && !errors.Is(err, suggest.ErrCapped) {
		s.logger.Printf("suggest: %s@%s %s: %s", e.Session, h.Host, e.Window, suggest.Reason(err))
	}
	switch out {
	case suggest.OutcomeGenerated, suggest.OutcomeCached:
		return len(set.Suggestions) > 0 && set.Turn == turn && !t.movedOn(end)
	}
	return false
}

func (s *Server) suggestIdleWait() time.Duration {
	if s.suggestIdleWaitFor > 0 {
		return s.suggestIdleWaitFor
	}
	return suggestIdleWait
}

// suggestWindow is the listed entry (host, session, window) of ctx's
// principal, as mirrorEntry resolves it, and whether there is one.
func (s *Server) suggestWindow(ctx context.Context, host, session, window string) (mirror.HostView, mirror.SessionView, bool) {
	r, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
	h, e, listed, _, ok := s.mirrorEntry(r, host, session, window)
	return h, e, ok && listed
}

// suggestSubject is the cache key of e's conversation: per host and Claude
// session, so a conversation moved to another window keeps its set.
func suggestSubject(host string, e mirror.SessionView) string {
	return host + "\x00" + e.ClaudeSession
}

// suggestSocketMsg is what a view's socket tells its page about suggestions:
// State "pending" (a set is being made: reserve the row), "ready" (fetch it)
// or "none" (there is none for this turn, or the turn moved on: take the row
// away). MS is how long ago the turn ended, so the page can hold the
// ui.SuggestCutoff from the turn end itself. Nothing from the session.
type suggestSocketMsg struct {
	Type  string `json:"type"` // "suggest"
	State string `json:"state"`
	MS    int64  `json:"ms,omitempty"`
	// Open: the set answers a view's OPEN on an idle session (suggestEnd.open),
	// which the page shows but does not report for the auto-pause.
	Open bool `json:"open,omitempty"`
}

// suggestOnTurnEnd runs suggestTurnAt for a view socket's turn end and tells
// the page: pending just before the engine is asked, then ready or none. One
// at a time per socket: a turn end during a running generation waits for it.
func (s *Server) suggestOnTurnEnd(ctx context.Context, host, session, window string, t *suggestTracker, end suggestEnd, gate chan struct{}, tell func(suggestSocketMsg)) {
	if s.ext.Suggest == nil {
		return
	}
	safeGo(s.logger, "suggest: a turn ended in a rendered view", func() {
		select {
		case gate <- struct{}{}:
		case <-ctx.Done():
			return
		}
		defer func() { <-gate }()
		pending := func(d time.Duration) {
			tell(suggestSocketMsg{Type: "suggest", State: "pending", MS: d.Milliseconds(), Open: end.open})
		}
		ok := s.suggestTurnAt(ctx, host, session, window, t, end, pending)
		if ctx.Err() != nil {
			return
		}
		if ok {
			tell(suggestSocketMsg{Type: "suggest", State: "ready", MS: time.Since(end.at).Milliseconds(), Open: end.open})
			return
		}
		tell(suggestSocketMsg{Type: "suggest", State: "none"})
	})
}

// suggestView is GET /mirror/view/suggest's answer.
type suggestView struct {
	// AI is always true: the composer labels every chip as AI-generated.
	AI          bool                 `json:"ai"`
	Label       string               `json:"label"`
	Suggestions []suggest.Suggestion `json:"suggestions"`
	// Fingerprint names the set (the composer's dismissal is per set).
	Fingerprint string `json:"fingerprint,omitempty"`
	// Reason is why there are none, in fixed words; "" when there are some.
	Reason string `json:"reason,omitempty"`
}

const suggestLabel = "AI-suggested replies"

// registerSuggestRoutes registers the composer's routes. Each answers 404
// when the server has no engine (no OpenRouter wiring at all);
// MUSTER_SUGGEST=off keeps the engine and answers "off".
func (s *Server) registerSuggestRoutes(mux Mux) {
	mux.HandleFunc("GET "+ui.SuggestPath, s.requireSession(s.handleMirrorSuggest))
	mux.HandleFunc("POST "+ui.SuggestEventPath, s.requireOperatorWrite(s.handleMirrorSuggestEvent))
	mux.HandleFunc("POST /account/suggest", s.requireOperatorWrite(s.handleAccountSuggest))
}

// handleMirrorSuggestEvent is POST /mirror/view/suggest/event: the chip row
// reporting what happened to a set — event=shown|filled|sent, the set's
// fingerprint, and for a send dist, the edit distance between what a tap
// filled and what was sent. NUMBERS ONLY: a text field is refused, nothing is
// stored, and the counts feed the auto-pause (suggest.Engine.Record).
func (s *Server) handleMirrorSuggestEvent(w http.ResponseWriter, r *http.Request) {
	if s.ext.Suggest == nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "the form could not be read", http.StatusBadRequest)
		return
	}
	f := r.PostForm
	h, e, listed, _, ok := s.mirrorEntry(r, f.Get("host"), f.Get("session"), f.Get("window"))
	if !ok || !listed {
		http.NotFound(w, r)
		return
	}
	// 🔴 NO TEXT LEAVES THE PAGE: only these fields are read, each a fixed
	// word or a number, and a request that carries anything else is refused —
	// so a page that started sending text would fail loudly, not leak quietly.
	for k := range f {
		switch k {
		case "host", "session", "window", "fingerprint", "event", "dist":
		default:
			http.Error(w, "unknown field "+strconv.Quote(k), http.StatusBadRequest)
			return
		}
	}
	ev, fp := f.Get("event"), f.Get("fingerprint")
	if !suggestFingerprintRe.MatchString(fp) {
		http.Error(w, "fingerprint is a set's fingerprint", http.StatusBadRequest)
		return
	}
	dist := 0
	if v := f.Get("dist"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 1_000_000 {
			http.Error(w, "dist is a number", http.StatusBadRequest)
			return
		}
		dist = n
	}
	switch ev {
	case suggest.EventShown, suggest.EventFilled, suggest.EventSent:
	default:
		http.Error(w, "event is shown, filled or sent", http.StatusBadRequest)
		return
	}
	s.ext.Suggest.Record(r.Context(), requestOwner(r.Context()), suggestSubject(h.Host, e), fp, ev, dist)
	w.WriteHeader(http.StatusNoContent)
}

// suggestFingerprintRe is a set's fingerprint (suggest.Built.Fingerprint).
var suggestFingerprintRe = regexp.MustCompile(`^[0-9a-f]{16,128}$`)

// handleMirrorSuggest is GET /mirror/view/suggest?host=&session=[&window=]:
// the window's current set, if the exclusions allow one NOW and it answers
// the window's current turn. It never generates.
func (s *Server) handleMirrorSuggest(w http.ResponseWriter, r *http.Request) {
	if s.ext.Suggest == nil {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	h, e, listed, _, ok := s.mirrorEntry(r, q.Get("host"), q.Get("session"), q.Get("window"))
	if !ok || !listed {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	v := suggestView{AI: true, Label: suggestLabel, Suggestions: []suggest.Suggestion{}}
	if why := s.suggestBlocked(r.Context(), h, e); why != "" {
		v.Reason = why
		s.writeJSON(w, http.StatusOK, v)
		return
	}
	set, has := s.ext.Suggest.Latest(requestOwner(r.Context()), suggestSubject(h.Host, e))
	// 🔴 THE SET MUST ANSWER THIS TURN: a set is served only while the
	// window's status is still the one it was generated in (StatusSince), so a
	// turn that ran and ended since — with no view open to regenerate — never
	// shows the previous turn's chips.
	if has && set.Turn != 0 && set.Turn == s.ext.Mirror.AgentState(r.Context(), h.Host, e.Session, e.Window).Since {
		v.Suggestions, v.Fingerprint = set.Suggestions, set.Fingerprint
		if len(set.Suggestions) == 0 {
			v.Reason = set.Problem
		}
	}
	s.writeJSON(w, http.StatusOK, v)
}

// handleAccountSuggest is POST /account/suggest (on=1|0): the caller's own
// toggle. Off also drops their cached sets.
func (s *Server) handleAccountSuggest(w http.ResponseWriter, r *http.Request) {
	if s.ext.Suggest == nil {
		http.NotFound(w, r)
		return
	}
	eng := s.ext.Suggest
	if eng.Prefs == nil {
		// No database: nothing to store — but turning them back ON still
		// resumes a user the auto-pause paused (in memory).
		if r.PostFormValue("on") == "1" && requestOwner(r.Context()) > 0 {
			eng.Resume(r.Context(), requestOwner(r.Context()))
			if r.PostFormValue("back") == "account" {
				http.Redirect(w, r, accountPath+"?suggest=saved#suggest", http.StatusSeeOther)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Error(w, "suggestion preferences need a database", http.StatusServiceUnavailable)
		return
	}
	var disabled bool
	switch r.PostFormValue("on") {
	case "1":
	case "0":
		disabled = true
	default:
		http.Error(w, "on is 1 or 0", http.StatusBadRequest)
		return
	}
	owner := requestOwner(r.Context())
	if owner <= 0 {
		http.Error(w, "no user", http.StatusForbidden)
		return
	}
	if err := eng.Prefs.SetDisabled(r.Context(), owner, disabled); err != nil {
		s.logger.Printf("suggest: owner %d: save the toggle: %v", owner, err)
		http.Error(w, "could not save the setting", http.StatusInternalServerError)
		return
	}
	if disabled {
		eng.Forget(owner)
	}
	// On again (or off): a paused user starts over (suggest.Engine.Resume).
	eng.Resume(r.Context(), owner)
	// The Account page's own form (back=account) comes back to the page.
	if r.PostFormValue("back") == "account" {
		http.Redirect(w, r, accountPath+"?suggest=saved#suggest", http.StatusSeeOther)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// RunSuggestSweep drops suggestion text past its TTL until ctx ends. Started
// by cmd/muster-server; nothing without an engine.
func (s *Server) RunSuggestSweep(ctx context.Context) {
	if s.ext.Suggest == nil {
		return
	}
	s.ext.Suggest.Run(ctx)
}

// suggestFocusHistory is the session's focus over time, oldest first: the
// newest suggest.MaxFocusEntries rows of its focus history (internal/focus,
// read under the caller's row-level security), or its current focus alone
// when there is no history; nil with no focus store or on a read error.
func (s *Server) suggestFocusHistory(ctx context.Context, sid string) []suggest.FocusEntry {
	if s.ext.Focus == nil {
		return nil
	}
	sub := focus.MirrorSubject(sid)
	page, err := s.ext.Focus.History(ctx, sub, "", suggest.MaxFocusEntries)
	if err != nil {
		s.logger.Printf("suggest: read the focus history: %v", err)
		return nil
	}
	var out []suggest.FocusEntry
	for i := len(page.Entries) - 1; i >= 0; i-- { // the page is newest first
		h := page.Entries[i]
		out = append(out, suggest.FocusEntry{Title: h.Title, Description: h.Description, At: h.CreatedAt})
	}
	if len(out) == 0 {
		if f, has, err := s.ext.Focus.Get(ctx, sub); err == nil && f.Has() && has {
			out = append(out, suggest.FocusEntry{Title: f.Title, Description: f.Description, At: f.UpdatedAt})
		}
	}
	return out
}
