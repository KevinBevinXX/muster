package mirror

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// AGENT STATE: whether the Claude Code session in a mirrored window is busy,
// idle or waiting on the person, and since when — what the server-side send
// queue (internal/sendqueue) waits on before it sends a queued message.
//
// 🔴 IT COMES FROM THE HOST, NOT FROM A BROWSER. A per-window host that
// advertised Msg.StatusReport puts Claude Code's own session status on every
// window it lists (Window.Status), re-sent whenever it changes; the hub keeps,
// per window, that status and SINCE WHEN it has seen it continuously. Nothing
// here depends on a page being open, which is what lets a queued message be
// sent after the page that queued it has closed.
//
// 🔴 UNKNOWN IS NOT IDLE. A host that never advertised StatusReport (an older
// muster-mirror, or a per-session one), a window whose pane is not Claude Code,
// and a Claude pane whose status the host cannot read are each UNKNOWN, with
// the reason — and the queue sends nothing to an unknown target.
//
// ⚠ "SINCE" IS WHEN MUSTER FIRST SAW THE STATUS, NOT WHEN IT BEGAN. A host that
// (re)connects starts every window's clock at the connect, and a status is only
// as fine-grained as the host's refresh (mirrorhost.Config.Refresh, 5 s): a turn
// shorter than one refresh can go unseen. Both err towards waiting longer.

// windowKey addresses one window of a host: its session and tmux window id.
type windowKey struct{ session, id string }

// statusMark is one window's status and since when it has held continuously.
type statusMark struct {
	status string
	since  time.Time
}

// noteStatus records, per listed window, its status and since when it has been
// that status: a window whose status is unchanged keeps its time, a changed or
// new one starts at now, and a window no longer listed is forgotten.
func (hc *hostConn) noteStatus(now time.Time) {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	next := make(map[windowKey]statusMark, len(hc.windows))
	for _, w := range hc.windows {
		k := windowKey{w.Session, w.ID}
		if old, ok := hc.status[k]; ok && old.status == w.Status {
			next[k] = old
			continue
		}
		next[k] = statusMark{status: w.Status, since: now}
	}
	hc.status = next
}

var (
	// ErrNoStatusReport: the host's muster-mirror does not report whether
	// Claude Code is busy.
	ErrNoStatusReport = errors.New("mirror: this host's muster-mirror does not report whether Claude Code is busy (it predates the send queue), so nothing queued for it is sent; upgrade it, or send by hand")
	// ErrNoStatus: the window runs Claude Code and the host cannot read its
	// status (its sessions file is missing or names no status).
	ErrNoStatus = errors.New("mirror: the host cannot read whether this window's Claude Code session is busy, so nothing queued for it is sent")
)

// AgentState is what the hub knows of the Claude Code session in one window.
type AgentState struct {
	// Status is StatusBusy, StatusIdle or StatusWaiting; "" when unknown.
	Status string
	// Since is when muster first saw Status, continuously since. Zero when
	// unknown.
	Since time.Time
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
	m, ok := hc.status[windowKey{session, id}]
	if !ok || m.status == "" {
		return AgentState{Unknown: fmt.Errorf("%w (host %q, session %q)", ErrNoStatus, host, session)}
	}
	return AgentState{Status: m.status, Since: m.since}
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
