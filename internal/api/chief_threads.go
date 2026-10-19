package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// THREAD SEARCH FOR THE CHIEF PANEL
//
// 🔴 OPERATOR-ONLY, AND THAT IS THE WHOLE SECURITY STATEMENT OF THIS FILE. The
// response carries MESSAGE CONTENT — a snippet cut out of a chat_messages body —
// which is the exact sensitivity a neighbouring PR is being held over. So it is
// registered behind requireSession (a signed operator session cookie) and behind
// NOTHING ELSE: not the shared hook token, not a per-agent hooks token. The two
// non-operator credentials that reach other routes in this package must get a
// 401 here, and api.TestChiefThreadSearchIsOperatorOnly asserts both of them.
//
// 🔴 IT SEARCHES kind='text' ONLY. Store.SearchSessions carries the reason at
// length; the short version is that tool_result rows are file contents and
// command output, so including them would make every query match noise AND would
// paste raw file bytes on screen as a snippet.
//
// ⚠ IT IS SCOPED TO THE CHIEF'S OWN THREADS. The panel talks to one agent, so the
// search takes that agent's id and no caller can widen it — a route keyed on an
// arbitrary agent is what got GET /api/agents/{name}/messages narrowed.
// ---------------------------------------------------------------------------

// handleChiefThreadSearch serves GET /ui/chief/threads/search: the thread ROWS
// matching ?q=, or the plain recent list when q is blank.
//
// 🔴 A BLANK QUERY RENDERS THE RECENT LIST RATHER THAN AN EMPTY RESULT SET. The
// search input swaps this partial OVER the list, so "cleared the box" and "typed
// something that matches nothing" are the same request shape from the client's
// point of view — and answering the first with "No thread matches that." would
// leave the operator unable to get their list back without closing the panel.
func (s *Server) handleChiefThreadSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	write := func(v ui.ChiefThreadsView) {
		if err := ui.RenderChiefThreadRows(w, v); err != nil {
			s.logger.Printf("chief: render thread rows: %v", err)
		}
	}

	q := strings.TrimSpace(r.URL.Query().Get("q"))

	// 🔴 EVERY FAILURE RENDERS AS ROWS, NEVER AS AN HTTP ERROR — the same rule the
	// panel body obeys, and for the same reason: this partial is swapped into a
	// view the operator is looking at, and htmx does not swap a non-2xx body, so a
	// 500 leaves the previous matches on screen as though they were the answer.
	//
	// 🔴 BUT A FAILURE IS SAID TO BE A FAILURE, AND IT DID NOT USED TO BE. Every path
	// below rendered "No thread matches that." — a Postgres error (a NUL byte in `q`
	// is enough), a chief that has gone missing, and a genuinely empty result set were
	// ONE SENTENCE, distinguishable only in a server log the operator never reads. The
	// 200 is the right call and it is kept; what changes is that the honest answer to
	// "did my query miss?" is now on screen. ui.ChiefThreadsView.Failed carries it and
	// the rendered container names the state in an attribute, so the guard asserts the
	// STATE rather than the words.
	failed := func() { write(ui.ChiefThreadsView{Query: q, Failed: true}) }

	// 🔴 A NO-DATABASE BOOT IS NOT A READ THAT MIGHT WORK NEXT TIME, AND THE DEFAULT
	// FAILURE SENTENCE ENDS "Try again." There is no store to ask, so a retry cannot
	// succeed — now or ever — and the instruction can only waste the operator's time.
	// The STATE stays `error` (it is still the absence of an answer, which is what
	// distinguishes it from `empty`); only the sentence is replaced. It matches
	// writeChiefPanelBody's own no-database reason, because it is the same fact about
	// the same deployment.
	//
	// ⚠ REACHABLE ONLY BY HITTING THIS ROUTE DIRECTLY. A no-database panel renders the
	// configuration statement and no search box at all, so nothing on screen asks this
	// question — which is exactly why the wrong sentence sat here unnoticed.
	if s.ext.Agents == nil {
		write(ui.ChiefThreadsView{
			Query:  q,
			Failed: true,
			Reason: "This muster is running without a database, so it has no threads to search.",
		})
		return
	}
	a, err := s.chiefAgent(ctx)
	if err != nil {
		if !errors.Is(err, ErrNoChief) {
			s.logger.Printf("chief: thread search resolve agent: %v", err)
		}
		// ⚠ ErrNoChief IS A FAILURE *HERE*, THOUGH IT IS A CONFIGURATION STATEMENT IN
		// writeChiefPanelBody, AND THE DIFFERENCE IS THE CALLER. That renderer answers a
		// panel OPEN, where naming the missing display name is an instruction a reader
		// can act on. This route answers a keystroke inside a panel that has ALREADY
		// resolved a chief and drawn a search box; "no thread matches that" would be a
		// verdict on a search nothing performed. The configuration statement stays where
		// it can be read — the next panel open renders it.
		failed()
		return
	}

	// 🔴 THE ACTIVE THREAD IS READ FROM THE QUERY STRING AND resolveSession IS NOT
	// CALLED, DELIBERATELY. resolveSession ends in LatestOrCreateSession, which
	// CREATES a chat_sessions row when the agent has none — so wiring it in here
	// would make a keystroke in a search box a write, and every debounced
	// keystroke another one. The panel renders its own active id into this route's
	// url (see chiefThreadList), so a plain comparison is all that is needed, and a
	// request without it simply highlights nothing.
	activeID, _ := strconv.ParseInt(r.URL.Query().Get("session"), 10, 64)

	if q == "" {
		list, err := s.ext.Agents.ListSessions(ctx, a.ID)
		if err != nil {
			s.logger.Printf("chief: thread search list sessions: %v", err)
			// 🔴 THE WORST OF THE THREE, AND THE REASON Failed IS NOT COSMETIC. This path
			// used to render "No threads yet." — telling the operator their entire thread
			// history was gone because ONE read failed.
			failed()
			return
		}
		write(ui.ChiefThreadsView{Threads: s.sessionViews(ctx, a, list, activeID)})
		return
	}

	matches, err := s.ext.Agents.SearchSessions(ctx, a.ID, q, agents.SearchSessionsMaxLimit)
	if err != nil {
		s.logger.Printf("chief: thread search %q: %v", q, err)
		failed()
		return
	}
	found := make([]agents.ChatSession, 0, len(matches))
	for _, m := range matches {
		found = append(found, m.ChatSession)
	}
	views := s.sessionViews(ctx, a, found, activeID)
	for i, m := range matches {
		views[i].Snippet = m.Snippet
	}
	write(ui.ChiefThreadsView{Threads: views, Query: q, Searched: true})
}
