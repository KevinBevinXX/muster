package hermeshost

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// PANE → SESSION (docs/design/hermes.md, "Pane to session resolution"). The
// caller has already found the pane's Hermes CLI by its process identity (the
// strict rule, internal/mirrorhost's HermesIdentity.CLI) and knows its tty
// and its start time; this resolves the rest:
//
//  1. the tty's breadcrumb, believed only when it is not older than the CLI
//     process (a breadcrumb outlives its CLI; the next CLI on the same tty
//     writes its own only at its first gateway attach);
//  2. its session in state.db, with no `source` filter (it flips from cli to
//     local after the first turn), not archived or hidden;
//  3. the lineage to its tip, following compression only;
//  4. 🔴 a tip that ENDED for any reason other than compression means the
//     terminal switched sessions (/resume, /branch: S3, measured
//     resumed_other, branched, new_session) and the breadcrumb is stale — NO
//     rendered view AND AN UNKNOWN STATUS. Never the old session above a
//     composer that sends to the pane, and never the abandoned tip's idle as
//     this pane's status;
//  5. the status, from the status files of every id in the lineage, believed
//     against the root's lease and the newest row (Believe).
//
// There is no guess and no fallback: anything that does not resolve is "no
// view, unknown status", with the reason.

// PaneSession is what a Hermes pane resolves to.
type PaneSession struct {
	// Breadcrumb is the terminal's breadcrumb as read (zero when it could not
	// be).
	Breadcrumb Breadcrumb
	// Lineage is the conversation, root first, when the breadcrumb resolved;
	// nil otherwise.
	Lineage Lineage
	// Session is the conversation's logical id — the lineage root, which a
	// rotation does not change — or "" when the pane has NO rendered view.
	Session string
	// NoView is why Session is "", "" when it is set.
	NoView string
	// Status is the pane's status; unknown whenever Session is "".
	Status Believed
}

// The reasons a pane has no rendered view, as -list -hermes prints them.
const (
	NoViewNoBreadcrumb  = "no breadcrumb for the pane's terminal (Hermes's session.terminal_continue is off, or the CLI has not attached to its gateway yet)"
	NoViewOldBreadcrumb = "the breadcrumb predates the Hermes process in the pane (it is a previous CLI's)"
	NoViewNoSession     = "the breadcrumb's session is not in state.db"
	NoViewArchived      = "the breadcrumb's session is archived or hidden"
	NoViewSwitched      = "the terminal switched sessions (/resume or /branch)"
)

// Locator resolves Hermes panes for one home.
type Locator struct {
	State  *State
	Status StatusDir
	// Now is the clock (nil = time.Now).
	Now func() time.Time
	// Alive reports whether a process with that pid and /proc start time still
	// runs (the status file's writer).
	Alive func(pid int, start string) bool
}

func (l *Locator) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// Locate resolves the Hermes CLI on terminal tty that started at started.
func (l *Locator) Locate(ctx context.Context, tty string, started time.Time) PaneSession {
	noView := func(ps PaneSession, why string) PaneSession {
		ps.Session, ps.NoView, ps.Status = "", why, Believed{Reason: why}
		return ps
	}
	var ps PaneSession
	bc, err := l.State.Home().ReadBreadcrumb(tty)
	if err != nil {
		if errors.Is(err, ErrNoBreadcrumb) {
			return noView(ps, NoViewNoBreadcrumb)
		}
		return noView(ps, "the breadcrumb could not be read: "+err.Error())
	}
	ps.Breadcrumb = bc
	if started.IsZero() || bc.TS < float64(started.UnixNano())/1e9 {
		return noView(ps, NoViewOldBreadcrumb)
	}
	snap, err := l.State.Snapshot(ctx, bc.SessionID)
	switch {
	case errors.Is(err, ErrNoSession):
		return noView(ps, NoViewNoSession)
	case err != nil:
		return noView(ps, fmt.Sprintf("state.db could not be read: %v", err))
	}
	ps.Lineage = snap.Lineage
	for _, s := range snap.Lineage {
		if s.ID == bc.SessionID && (s.Archived || s.Hidden) {
			return noView(ps, NoViewArchived)
		}
	}
	if tip := snap.Lineage.Tip(); tip.Ended && tip.EndReason != EndReasonCompression {
		return noView(ps, NoViewSwitched)
	}
	f, found := l.Status.Newest(snap.Lineage.IDs())
	ps.Session = snap.Lineage.Root()
	ps.Status = Believe(f, found, snap, l.now(), l.Alive)
	return ps
}
