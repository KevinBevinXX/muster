package mirror

import "time"

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

// notePaste records that a paste which may have reached the window was handed
// to it while the window held StatusSince since (read before the paste was
// written). A paste refused before it reached the host, or by the host, is not
// recorded: it started no turn, and recording it would hold the queue for one.
func (h *Hub) notePaste(owner int64, host, session, window string, since int64) {
	if since <= 0 || window == "" {
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
		t = &turnState{}
		h.turns[k] = t
	}
	if since > t.pasted {
		t.pasted = since
	}
}
