// Package sse implements a small Server-Sent Events broadcaster: subscribers
// register a channel FOR ONE USER, the broadcaster fans an Event out to the
// subscribers of the user who owns it, and subscribers unregister when done.
//
// 🔴 DELIVERY IS BY OWNER, AND AN EVENT WITH NO OWNER REACHES NOBODY (accounts
// PR 3b). This bus used to fan every event to every subscriber: with a second
// user, user B's open board would have received user A's task ids and agent
// names with no handler bug at
// all — row-level security never sees the bus. So an Event names its OwnerID,
// a subscriber names its user, and Broadcast delivers only where they are
// equal. There is no "deliver to everyone" path: every event muster emits is
// about an owned row (the call-site classification is in internal/api's
// broadcast helpers), and a zero owner is a wiring bug that FAILS CLOSED —
// dropped, and reported to the caller through Broadcast's return value —
// rather than a broadcast to all.
//
// The broadcaster is transport-agnostic — it only deals in Event values and
// channels. The HTTP handler that writes the SSE wire format lives in the api
// package and uses Subscribe/Unsubscribe.
package sse

import (
	"strings"
	"sync"
	"sync/atomic"
)

// Event is a single broadcast message. Name maps to the SSE "event:" field
// and Data to the "data:" field (already-serialized, typically JSON).
type Event struct {
	Name string
	Data string
	// OwnerID is the user whose subscribers receive the event. Zero (or
	// negative) reaches nobody. It is never written to the wire.
	OwnerID int64
}

// ValidData reports whether s may be used as an Event's Data without breaking
// the SSE frame it will be written into.
//
// 🔴 A NEWLINE IN Data IS NOT A COSMETIC PROBLEM — IT IS FRAME INJECTION, AND IT
// WALKS ANY ALLOWLIST APPLIED TO Name. The api package writes one frame as
//
//	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Name, ev.Data)
//
// so a Data carrying "\n\nevent: request.resolved\ndata: {}" terminates that frame
// early and emits a SECOND, fully-formed event under a name the caller never had
// to be allowed to send. A carriage return is refused for the same reason — the SSE
// spec treats CR, LF and CRLF alike as line terminators, so "\r" alone ends the line
// in a conforming client while a check that only looked for "\n" read it as
// harmless.
//
// ⚠ IT IS SUFFICIENT AGAINST THAT HAZARD BUT NOT NECESSARY, AND AN EARLIER VERSION
// OF THIS COMMENT OVERSTATED IT. It said an allowlist on Name "is only as strong as
// this check" and that "without it the name field is a suggestion". Measured for
// #871's round 0 and that is wrong wherever the caller ALSO requires Data to be
// valid JSON: forging needs the bare token `event` at the start of a line, and in
// JSON a raw CR/LF is legal only as whitespace between tokens while no JSON token
// spells `event` — so no forging payload is ever valid JSON. The one caller that
// takes untrusted Data (api.handleEventPublish) applies both, and its header records
// the measurement. What this predicate uniquely catches there is valid JSON with a
// raw newline BETWEEN tokens, which truncates the data field but cannot change the
// event name. Keep it — it is two characters of ContainsAny and it holds even if a
// caller relaxes its JSON contract — but do not quote it as the only thing standing
// between an allowlist and a forged event.
//
// ⚠ IT IS A CHECK ON CALLER-SUPPLIED Data, NOT A NEW INVARIANT. Every in-process
// broadcast in this repo already satisfies it by construction, because each one
// passes the output of encoding/json — which escapes both characters inside string
// values and emits neither between tokens. The one path where Data does NOT come
// from a marshaller is an HTTP route that lets another process publish; that is
// the caller this function exists for.
//
// ⚠ IT IS DELIBERATELY NOT ENFORCED INSIDE Broadcast. Broadcast has no way to
// report a refusal — it returns nothing and is called from background goroutines —
// so enforcing there would mean silently dropping an event, which is the failure
// mode this package's own "drop rather than block" comment already warns is
// invisible. The predicate lives here, next to the format it protects, and the one
// caller that can receive untrusted Data applies it and answers 400.
func ValidData(s string) bool {
	return !strings.ContainsAny(s, "\r\n")
}

// Broadcaster fans Events out to all current subscribers. It is safe for
// concurrent use.
type Broadcaster struct {
	mu     sync.RWMutex
	subs   map[uint64]subscriber
	nextID uint64
	buffer int
}

// subscriber is one registered stream and the user it streams for.
type subscriber struct {
	owner int64
	ch    chan Event
}

// New returns a Broadcaster. buffer is the per-subscriber channel buffer
// size; a slow subscriber that fills its buffer will drop events rather than
// block the broadcaster (see Broadcast).
func New(buffer int) *Broadcaster {
	if buffer < 1 {
		buffer = 1
	}
	return &Broadcaster{
		subs:   make(map[uint64]subscriber),
		buffer: buffer,
	}
}

// Subscribe registers a new subscriber streaming for user owner and returns
// its receive channel plus an id used to Unsubscribe. The channel is buffered.
// A subscriber whose owner is not positive receives nothing.
func (b *Broadcaster) Subscribe(owner int64) (uint64, <-chan Event) {
	ch := make(chan Event, b.buffer)
	id := atomic.AddUint64(&b.nextID, 1)
	b.mu.Lock()
	b.subs[id] = subscriber{owner: owner, ch: ch}
	b.mu.Unlock()
	return id, ch
}

// Unsubscribe removes the subscriber with the given id and closes its
// channel. It is idempotent.
func (b *Broadcaster) Unsubscribe(id uint64) {
	b.mu.Lock()
	if sub, ok := b.subs[id]; ok {
		delete(b.subs, id)
		close(sub.ch)
	}
	b.mu.Unlock()
}

// Broadcast delivers ev to every current subscriber of ev.OwnerID. Delivery
// is non-blocking: if a subscriber's buffer is full the event is dropped for
// that subscriber so one slow consumer cannot stall the rest.
//
// It reports false — and delivers to nobody — when ev names no owner. That is
// the fail-closed case in the package doc; the caller logs it.
func (b *Broadcaster) Broadcast(ev Event) bool {
	if ev.OwnerID <= 0 {
		return false
	}
	b.mu.RLock()
	for _, sub := range b.subs {
		if sub.owner != ev.OwnerID {
			continue
		}
		select {
		case sub.ch <- ev:
		default:
			// subscriber buffer full; drop rather than block
		}
	}
	b.mu.RUnlock()
	return true
}

// Count returns the number of active subscribers. Primarily for tests.
func (b *Broadcaster) Count() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}
