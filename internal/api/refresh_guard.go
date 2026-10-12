package api

import (
	"container/list"
	"sync"
	"time"
)

// refreshGuard is the per-subject FAILED-refresh cap, as a RESERVATION: a
// refresh takes a failure slot (and the subject's single in-flight slot)
// BEFORE GoTrue is called, and release refunds it unless GoTrue judged the
// refresh token bad.
//
// 🔴 CHECK, CALL, THEN RECORD LET A PARALLEL FLOOD THROUGH. The first version
// counted a failure only after GoTrue answered, so 200 parallel junk refreshes
// for one user all passed the check: measured, 164-200 of them reached GoTrue
// against a limit of 10. Now at most one unexempted refresh per subject is in
// flight and the count is taken up front
// (TestAJunkRefreshFloodIsBoundedAndSparesTheVictim, run in parallel).
//
// Bounded like boundedWindow: capped at boundedWindowCap, least-recently-touched
// eviction that skips in-flight entries, its own mutex.
type refreshGuard struct {
	mu    sync.Mutex
	items map[string]*list.Element // value: *guardEntry
	order *list.List
	gen   uint64
}

type guardEntry struct {
	key      string
	start    time.Time
	n        int
	inFlight bool
	// gen is DEFENSIVE: an entry is evicted while in flight only when every
	// entry is in flight; a stale release is then a no-op.
	gen uint64
}

// refreshTicket is one reserved refresh; release it exactly once.
type refreshTicket struct {
	key string
	gen uint64
}

// reserve takes a failure slot for key if fewer than limit failures are in
// the window and none of key's refreshes is in flight.
func (g *refreshGuard) reserve(key string, now time.Time, window time.Duration, limit int) (refreshTicket, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.items == nil {
		g.items, g.order = map[string]*list.Element{}, list.New()
	}
	var en *guardEntry
	if e, ok := g.items[key]; ok {
		g.order.MoveToFront(e)
		en = e.Value.(*guardEntry)
		if !en.inFlight && now.Sub(en.start) >= window {
			en.start, en.n = now, 0
		}
	} else {
		g.gen++
		en = &guardEntry{key: key, start: now, gen: g.gen}
		g.items[key] = g.order.PushFront(en)
		for len(g.items) > boundedWindowCap {
			victim := g.order.Back()
			for e := victim; e != nil; e = e.Prev() {
				if !e.Value.(*guardEntry).inFlight {
					victim = e
					break
				}
			}
			g.order.Remove(victim)
			delete(g.items, victim.Value.(*guardEntry).key)
		}
	}
	if en.inFlight || en.n >= limit {
		return refreshTicket{}, false
	}
	en.n++
	en.inFlight = true
	return refreshTicket{key: key, gen: en.gen}, true
}

// release ends a reserved refresh; failed keeps the failure slot.
func (g *refreshGuard) release(t refreshTicket, failed bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.items[t.key]
	if !ok {
		return
	}
	en := e.Value.(*guardEntry)
	if en.gen != t.gen {
		return // defensive: see guardEntry.gen
	}
	en.inFlight = false
	if !failed && en.n > 0 {
		en.n--
	}
}

// size is the number of keys held (tests).
func (g *refreshGuard) size() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.items)
}
