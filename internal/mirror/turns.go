package mirror

import (
	"strings"
	"time"
)

// TURNS: per mirrored window, what the send queue (internal/sendqueue) needs to
// tell "the turn a paste started has ended" without comparing two clocks.
//
// 🔴 HOST TIMES ARE COMPARED ONLY WITH HOST TIMES, AND DURATIONS ARE MEASURED
// ON MUSTER'S CLOCK. A window's Window.StatusSince is Claude Code's own
// statusUpdatedAt, on the HOST's clock. The hub records, at EVERY paste into
// the window (a person's Send and a queued message alike), the StatusSince it
// held then (Pasted). The queue releases the next message only once the host
// reports a StatusSince strictly NEWER than that — a later status write, which
// is the turn the paste started — and measures how long the window has been
// idle from when the hub first OBSERVED that stamp (Observed), on muster's own
// clock. A host clock that is minutes ahead or behind therefore neither stalls
// the queue nor releases it early: neither comparison ever mixes the clocks.
//
// 🔴 A QUEUED PASTE IS REFUSED IF ANOTHER PASTE GOT IN FIRST. It carries the
// turn it was decided on (ExpectTurn); beginPaste admits it only if no other
// paste is in flight and the window's Since and Pasted are still that turn's,
// in the same critical section that admits a person's Send. So a queued paste
// never follows a Send into its turn. The reverse is NOT prevented: a person's
// Send is never refused, so a Send made while an admitted queued paste is
// still in flight (handed to the host, not yet answered: one paste round-trip)
// is pasted too, and the two can land in one turn.
//
// They are kept on the hub, not on a connection, so a host that reconnects
// keeps them; a revoked host's go with it, and a window the host no longer
// lists is forgotten at its next list.

// turnKey is one window of one owner's host.
type turnKey struct {
	owner         int64
	host, session string
	window        string
}

// turnState is what the hub has seen of one window's Claude Code status.
type turnState struct {
	// status and since are the latest status and StatusSince the host
	// reported; observed is when (muster's clock) the hub first saw that pair.
	status   string
	since    int64
	observed time.Time
	// pasted is the largest StatusSince the window held when something was
	// pasted into it (0: nothing since the hub started).
	pasted int64
	// inflight counts the pastes into the window handed out (beginPaste) and
	// not yet settled (endPaste): a queued paste is refused while any is.
	inflight int
	// guardWhy, guardUntil and guardN are the Claude prompt guard's HOLD
	// (guardHold): the host refused a paste on what it read at the moment of
	// typing — a dialog, a non-empty input box, a screen it could not read —
	// guardN times in a row; the window is held, with guardWhy, until
	// guardUntil (muster's clock). Cleared by a new status (observe) and by a
	// paste that landed (endPaste).
	guardWhy   string
	guardUntil time.Time
	guardN     int
}

// Guard backoff: the first hold is guardHoldFirst, each refusal in a row
// doubles it, up to guardHoldMax. So a refusal the status cannot see (a dialog
// on screen while the status says idle, text left in the box) is retried
// after 4 s, 8 s, 16 s, then every 30 s — not at every dispatcher pass.
const (
	guardHoldFirst = 4 * time.Second
	guardHoldMax   = 30 * time.Second
)

// guardHold holds the window after the host's prompt guard refused a paste
// into it, for why (the host's words), backing off (guardHoldFirst doubling
// to guardHoldMax).
func (h *Hub) guardHold(owner int64, host, session, window, why string) {
	if window == "" {
		return
	}
	k := turnKey{owner, host, session, window}
	now := h.now()
	h.turnsMu.Lock()
	defer h.turnsMu.Unlock()
	if h.turns == nil {
		h.turns = map[turnKey]*turnState{}
	}
	t := h.turns[k]
	if t == nil {
		t = &turnState{}
		h.turns[k] = t
	}
	t.guardN++
	d := guardHoldMax
	if t.guardN <= 4 {
		d = min(guardHoldFirst<<(t.guardN-1), guardHoldMax)
	}
	t.guardWhy, t.guardUntil = why, now.Add(d)
}

// guardHeld is t's hold reason at now, "" when it is not held.
func (t turnState) guardHeld(now time.Time) string {
	if t.guardWhy == "" || !now.Before(t.guardUntil) {
		return ""
	}
	return "Held: muster-mirror refused it at the moment of typing — " +
		strings.TrimSuffix(strings.TrimPrefix(t.guardWhy, "Not sent: "), " Nothing was typed into the pane.") +
		" It tries again at " + t.guardUntil.UTC().Format("15:04:05") + " UTC."
}

// observe records hc's window list at now: a window whose (status, time) pair
// changed is stamped observed now, an unchanged one keeps its stamp, and a
// window the host no longer lists is forgotten.
func (h *Hub) observe(owner int64, host string, windows []Window, now time.Time) {
	h.turnsMu.Lock()
	defer h.turnsMu.Unlock()
	if h.turns == nil {
		h.turns = map[turnKey]*turnState{}
	}
	listed := make(map[turnKey]bool, len(windows))
	for _, w := range windows {
		k := turnKey{owner, host, w.Session, w.ID}
		listed[k] = true
		if w.Status == "" || w.StatusSince <= 0 {
			continue
		}
		t := h.turns[k]
		if t == nil {
			t = &turnState{}
			h.turns[k] = t
		}
		if t.status != w.Status || t.since != w.StatusSince || t.observed.IsZero() {
			t.status, t.since, t.observed = w.Status, w.StatusSince, now
			// A new status (the prompt answered, a turn ran): the guard's hold
			// and its backoff start over.
			t.guardWhy, t.guardUntil, t.guardN = "", time.Time{}, 0
		}
	}
	for k := range h.turns {
		if k.owner == owner && k.host == host && !listed[k] {
			delete(h.turns, k)
		}
	}
}

// forgetTurns drops every window of (owner, host): a revoked host.
func (h *Hub) forgetTurns(owner int64, host string) {
	h.turnsMu.Lock()
	defer h.turnsMu.Unlock()
	for k := range h.turns {
		if k.owner == owner && k.host == host {
			delete(h.turns, k)
		}
	}
}

// turn is a copy of what the hub has seen of one window (zero: nothing).
func (h *Hub) turn(owner int64, host, session, window string) turnState {
	h.turnsMu.Lock()
	defer h.turnsMu.Unlock()
	if t := h.turns[turnKey{owner, host, session, window}]; t != nil {
		return *t
	}
	return turnState{}
}

// beginPaste admits a paste into the window, which holds StatusSince since
// now, and returns the status time to record for it (endPaste). With expect
// (a queued paste, decided on that turn) it is refused — ErrSuperseded, nothing
// admitted — when another paste into the window is in flight, or the window's
// Since or Pasted is no longer expect's. The check and the admission are one
// critical section under turnsMu, so a queued message is never admitted after
// a person's Send on its turn. A Send is never refused, though: one that
// arrives while an admitted queued paste is still in flight (within that
// paste's round-trip to the host) is admitted as well, and both can land in
// the same turn.
func (h *Hub) beginPaste(owner int64, host, session, window string, since int64, expect *Turn) (int64, error) {
	if window == "" {
		// A per-session host's session: no turns are kept (no status report).
		if expect != nil {
			return 0, &SupersededError{}
		}
		return since, nil
	}
	k := turnKey{owner, host, session, window}
	h.turnsMu.Lock()
	defer h.turnsMu.Unlock()
	if h.turns == nil {
		h.turns = map[turnKey]*turnState{}
	}
	t := h.turns[k]
	if t == nil {
		t = &turnState{}
		h.turns[k] = t
	}
	if expect != nil && (t.inflight > 0 || since != expect.Since || t.pasted != expect.Pasted) {
		return 0, &SupersededError{Pasted: t.pasted}
	}
	t.inflight++
	return since, nil
}

// endPaste settles a paste beginPaste admitted. landed: it may have reached
// the window (confirmed, or handed over and never answered), and so is
// recorded — Pasted rises to since, never lowers. A paste refused before it
// reached the host, or by the host, is not recorded: it started no turn, and
// recording it would hold the queue for one.
func (h *Hub) endPaste(owner int64, host, session, window string, since int64, landed bool) {
	if window == "" {
		return
	}
	k := turnKey{owner, host, session, window}
	h.turnsMu.Lock()
	defer h.turnsMu.Unlock()
	if h.turns == nil {
		h.turns = map[turnKey]*turnState{}
	}
	t := h.turns[k]
	if t == nil {
		// Forgotten mid-paste (the host stopped listing the window).
		t = &turnState{}
		h.turns[k] = t
	} else if t.inflight > 0 {
		t.inflight--
	}
	if landed && since > t.pasted {
		t.pasted = since
	}
	if landed {
		t.guardWhy, t.guardUntil, t.guardN = "", time.Time{}, 0
	}
}
