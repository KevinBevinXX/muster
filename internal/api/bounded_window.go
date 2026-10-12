package api

import (
	"container/list"
	"sync"
	"time"
)

// boundedWindow is a fixed-window counter per key with a HARD size cap and
// least-recently-touched eviction: every operation is O(1) under its own
// mutex.
//
// 🔴 IT REPLACES keyedWindow, WHOSE MAP GREW WITHOUT BOUND AND WHOSE PRUNE WAS
// AN O(n) SCAN UNDER THE LOCK. An attacker choosing a new key per request (a
// junk refresh token each time) grew it past 10k, after which every take paid
// a full scan (~0.8 ms at 50k), serialising every refresh in the process.
//
// ⚠ EVICTION FORGETS A COUNT. That is acceptable for the keys this type is
// used with — a refresh-token hash (each junk token is its own key anyway) and
// the set of refresh tokens this process issued — and it is why the per-ACCOUNT
// sign-in backoff, whose forgetting would matter, has its own guarded
// eviction (signinLimiter).
type boundedWindow struct {
	mu    sync.Mutex
	cap   int // 0 = boundedWindowCap
	items map[string]*list.Element
	order *list.List // front = most recently touched
}

type boundedEntry struct {
	key   string
	start time.Time
	n     int
}

// boundedWindowCap is the cap a zero-value boundedWindow uses.
const boundedWindowCap = 10000

// entryLocked returns key's entry, resetting it once window passed; it creates
// one (evicting the oldest past the cap) when create is true.
func (b *boundedWindow) entryLocked(key string, now time.Time, window time.Duration, create bool) *boundedEntry {
	if b.items == nil {
		b.items, b.order = map[string]*list.Element{}, list.New()
		if b.cap <= 0 {
			b.cap = boundedWindowCap
		}
	}
	if e, ok := b.items[key]; ok {
		b.order.MoveToFront(e)
		en := e.Value.(*boundedEntry)
		if now.Sub(en.start) >= window {
			en.start, en.n = now, 0
		}
		return en
	}
	if !create {
		return nil
	}
	en := &boundedEntry{key: key, start: now}
	b.items[key] = b.order.PushFront(en)
	for len(b.items) > b.cap {
		oldest := b.order.Back()
		b.order.Remove(oldest)
		delete(b.items, oldest.Value.(*boundedEntry).key)
	}
	return en
}

// take counts one hit for key if fewer than limit are in the current window,
// in one step.
func (b *boundedWindow) take(key string, now time.Time, window time.Duration, limit int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	en := b.entryLocked(key, now, window, true)
	if en.n >= limit {
		return false
	}
	en.n++
	return true
}

// count returns key's hits in the current window (0 for an unknown key).
func (b *boundedWindow) count(key string, now time.Time, window time.Duration) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if en := b.entryLocked(key, now, window, false); en != nil {
		return en.n
	}
	return 0
}

// add counts one hit for key.
func (b *boundedWindow) add(key string, now time.Time, window time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.entryLocked(key, now, window, true).n++
}

// size is the number of keys held (tests).
func (b *boundedWindow) size() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.items)
}
