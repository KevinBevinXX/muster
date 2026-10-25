package opencodehost

import (
	"fmt"
	"strings"
)

// PANE TO SESSION, BY TITLE (docs/design/opencode.md): an opencode TUI names
// its tmux pane "OC | <session title>", and cuts a long title with "…"
// (measured: at 37 characters). Nothing else ties a TUI to its session — its
// command line and environment carry no session id, and the TUI is a thin
// client of the one shared server — so the title is matched against the
// server's ROOT sessions:
//
//   - the prefix "OC | " is required (a TUI on its home screen names no
//     session);
//   - an exact title, or, for a title ending in "…", every title that starts
//     with what precedes it;
//   - EXACTLY ONE root session must match. Two sessions with one title (a
//     default title, a copy) is no match, never a guess.
//
// 🔴 A MATCH IS A HEURISTIC, SO IT IS VIEW-ONLY. A renamed session, a TUI
// that switched sessions, or a later session given the same title can move it.
// muster sends nothing through it: a send needs a launcher record that names
// the session deterministically.
//
// The session's directory is NOT compared with the pane's: measured, it
// differed for 2 of 5 correct matches (a session moved, a TUI started
// elsewhere).

// TitlePrefix starts the pane title of a TUI showing a session.
const TitlePrefix = "OC | "

// titleEllipsis ends a title the TUI cut.
const titleEllipsis = "…"

// Match is how a pane title resolved: the session, or why none.
type Match struct {
	// Session is the one root session the title names; "" for none.
	Session string
	// Title is the title as the pane shows it (prefix stripped).
	Title string
	// Truncated: the pane's title was cut ("…"), so it was a prefix match.
	Truncated bool
	// Why says why there is no session; "" when there is one.
	Why string
}

// MatchTitle resolves pane title paneTitle against sessions (the server's
// root sessions; a child, should one be passed, is never matched).
func MatchTitle(paneTitle string, sessions []Session) Match {
	body, ok := strings.CutPrefix(paneTitle, TitlePrefix)
	if !ok {
		return Match{Why: "the pane's title does not name an opencode session (no \"" + TitlePrefix + "\" prefix)"}
	}
	m := Match{Title: body}
	want, cut := strings.CutSuffix(body, titleEllipsis)
	m.Truncated = cut
	if strings.TrimSpace(want) == "" {
		m.Why = "the pane's title names no session"
		return m
	}
	var hits []string
	for _, s := range sessions {
		if s.ParentID != "" || s.Title == "" {
			continue
		}
		if s.Title == want || (cut && strings.HasPrefix(s.Title, want)) {
			hits = append(hits, s.ID)
		}
	}
	switch len(hits) {
	case 1:
		m.Session = hits[0]
	case 0:
		m.Why = fmt.Sprintf("no opencode session is titled %q", body)
	default:
		m.Why = fmt.Sprintf("%d opencode sessions match the title %q: ambiguous, so none is shown", len(hits), body)
	}
	return m
}
