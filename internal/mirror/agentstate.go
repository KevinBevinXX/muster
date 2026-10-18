package mirror

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// AGENT STATE: whether the Claude Code session in a mirrored window is busy,
// idle or waiting on the person, and SINCE WHEN — what the server-side send
// queue (internal/sendqueue) waits on before it sends a queued message.
//
// 🔴 IT COMES FROM THE HOST, NOT FROM A BROWSER. A per-window host that
// advertised Msg.StatusReport puts Claude Code's own session status on every
// window it lists (Window.Status), with the time Claude Code itself wrote it
// (Window.StatusSince: its sessions file's statusUpdatedAt), re-sent whenever
// either changes. Nothing here depends on a page being open, which is what lets
// a queued message be sent after the page that queued it has closed.
//
// 🔴 "SINCE" IS THE CLI'S OWN TIMESTAMP, ON THE HOST'S CLOCK. So a turn that
// starts and ends between two host refreshes still moves it — the status reads
// idle both times, and its time is newer. It is compared only with Pasted (also
// a host time); how long the window has been idle is measured from Observed, on
// muster's clock (turns.go).
//
// 🔴 UNKNOWN IS NOT IDLE. A host that never advertised StatusReport (an older
// muster-mirror, or a per-session one), a window whose pane is not Claude Code,
// a Claude pane whose status the host cannot read, and a status that comes
// WITHOUT its time are each UNKNOWN, with the reason — and the queue sends
// nothing to an unknown target.

var (
	// ErrNoStatusReport: the host's muster-mirror does not report whether
	// Claude Code is busy.
	ErrNoStatusReport = errors.New("mirror: this host's muster-mirror does not report whether Claude Code is busy (it predates the send queue), so nothing queued for it is sent; upgrade it, or send by hand")
	// ErrNoStatus: the window runs Claude Code and the host cannot read its
	// status (its sessions file is missing or names no status).
	ErrNoStatus = errors.New("mirror: the host cannot read whether this window's Claude Code session is busy, so nothing queued for it is sent")
	// ErrNoStatusSince: the host reports the status but not when Claude Code
	// wrote it (an older muster-mirror, or a Claude Code whose sessions file has
	// no statusUpdatedAt). Without it a turn cannot be told from no turn.
	ErrNoStatusSince = errors.New("mirror: the host does not report since when this window's Claude Code session has been idle, so nothing queued for it is sent; upgrade muster-mirror (or Claude Code), or send by hand")
)

// AgentState is what the hub knows of the Claude Code session in one window.
type AgentState struct {
	// Status is StatusBusy, StatusIdle or StatusWaiting; "" when unknown.
	Status string
	// Since is when Claude Code wrote Status (its statusUpdatedAt), in Unix
	// milliseconds ON THE HOST'S CLOCK. 0 when unknown.
	Since int64
	// Observed is when the hub first saw this (Status, Since) pair, on
	// MUSTER's clock: what an idle duration is measured from.
	Observed time.Time
	// Pasted is the Since the window held at its latest paste through this
	// hub (a Send or a queued message), on the host's clock; 0 for none.
	Pasted int64
	// Unknown is why Status is unknown; nil when it is known.
	Unknown error
}

// AgentState is the state of the Claude Code session in owner's host's
// (session, window) — window "" being the session's active window — or why it
// is unknown. Another owner's host answers ErrHostOffline, like a host that
// does not exist.
func (h *Hub) AgentState(owner int64, host, session, window string) AgentState {
	h.mu.Lock()
	hc := h.hosts[hostKey{owner, host}]
	waiting := h.waiting
	h.mu.Unlock()
	if hc == nil {
		return AgentState{Unknown: fmt.Errorf("%w (host %q)", ErrHostOffline, host)}
	}
	id, claude, err := hc.target(session, window)
	if err != nil {
		return AgentState{Unknown: err}
	}
	hc.mu.Lock()
	defer hc.mu.Unlock()
	switch {
	case !hc.statusReport || hc.windows == nil:
		return AgentState{Unknown: fmt.Errorf("%w (host %q)", ErrNoStatusReport, host)}
	case !claude:
		return AgentState{Unknown: fmt.Errorf("%w (host %q, session %q)", ErrNotClaude, host, session)}
	}
	for _, w := range hc.windows {
		if w.Session != session || w.ID != id {
			continue
		}
		switch {
		case w.Status == "":
			return AgentState{Unknown: fmt.Errorf("%w (host %q, session %q)", ErrNoStatus, host, session)}
		case w.StatusSince <= 0:
			return AgentState{Unknown: fmt.Errorf("%w (host %q, session %q)", ErrNoStatusSince, host, session)}
		}
		st := AgentState{Status: w.Status, Since: w.StatusSince}
		// A question held by the ask hook: waiting on a person, whatever Claude
		// Code's own file says (Hub.SetWaiting).
		if waiting != nil && w.ClaudeSession != "" && waiting(owner, host, w.ClaudeSession) {
			st.Status = StatusWaiting
		}
		t := h.turn(owner, host, session, id)
		if t.since == w.StatusSince && t.status == w.Status {
			st.Observed = t.observed
		}
		st.Pasted = t.pasted
		return st
	}
	return AgentState{Unknown: fmt.Errorf("%w (host %q, session %q)", ErrNoStatus, host, session)}
}

// AgentState is Hub.AgentState for the viewing principal's host.
func (s *Service) AgentState(ctx context.Context, host, session, window string) AgentState {
	o, err := owner(ctx)
	if err != nil {
		return AgentState{Unknown: err}
	}
	return s.hub.AgentState(o, host, session, window)
}

// ErrPasteUnconfirmed marks a paste that was HANDED TO THE HOST and never
// confirmed either way — the host's answer timed out, the caller gave up, or
// the connection ended first. It may have been pasted. (A paste refused before
// it was written to the host, or refused by the host, carries no such mark: it
// was not pasted.)
var ErrPasteUnconfirmed = errors.New("mirror: the message was handed to the host, which did not confirm it")

// unconfirmed is err marked ErrPasteUnconfirmed; its text is err's own.
type unconfirmed struct{ err error }

func (u unconfirmed) Error() string   { return u.err.Error() }
func (u unconfirmed) Unwrap() []error { return []error{u.err, ErrPasteUnconfirmed} }
