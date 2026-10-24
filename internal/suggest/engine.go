package suggest

import (
	"context"
	"log"
	"sort"
	"strconv"
	"sync"
	"time"
)

// THE ENGINE: whether a set is generated, and the in-memory cache it is kept
// in. One per server.
//
// One Suggest call, in order:
//  1. the KILL SWITCH (Off, MUSTER_SUGGEST=off) refuses before anything;
//  2. the owner's TOGGLE (Prefs) refuses an owner who turned it off, and the
//     AUTO-PAUSE (autopause.go) one who rarely taps a chip;
//  3. 🔴 THE FINGERPRINT SKIP: a subject whose cached set was built from the
//     SAME input — or for the SAME turn — gets that set back and NO call is made — and that holds after
//     the set's text expired (only the fingerprint is kept), so an unchanged
//     session is never paid for twice. A new turn is a new input, so a new
//     fingerprint, so exactly one call;
//  4. one call per (subject, turn) at a time: a second view asking for the
//     same turn waits for the first's answer;
//  5. the per-owner and global RATE CAPS, then the concurrency bound;
//  6. the call; its answer through Filter; the survivors cached.
//
// 🔴 TEXT LIVES AT MOST CacheTTL. Sweep (Run, every SweepInterval) drops every
// set's text past it, and Latest never serves an expired one even before the
// sweep reaches it.

// The engine's bounds.
const (
	// PerOwnerPerHour is the operator's own cap for suggestions (separate from
	// focus's, so suggestions never starve it).
	PerOwnerPerHour = 120
	// GlobalPerHour bounds every owner together.
	GlobalPerHour = 1000
	// MaxConcurrent bounds calls in flight across every owner.
	MaxConcurrent = 2
	// CacheTTL is how long a set's text is kept in memory.
	CacheTTL = time.Hour
	// SweepInterval is how often expired text is dropped.
	SweepInterval = time.Minute
	// MaxCached bounds the subjects the cache holds; the least recently
	// generated is evicted past it.
	MaxCached = 2000
)

// Outcome is what one Suggest did.
type Outcome string

const (
	OutcomeGenerated Outcome = "generated"
	OutcomeCached    Outcome = "cached"  // the fingerprint skip: no call
	OutcomeExpired   Outcome = "expired" // the fingerprint skip, text gone: no call
	OutcomeOff       Outcome = "off"     // MUSTER_SUGGEST=off
	OutcomeUserOff   Outcome = "user-off"
	OutcomeFailed    Outcome = "failed"
)

// Prefs is the per-user toggle. Disabled is true when owner turned
// suggestions off; the default (no row) is ON (operator decision).
type Prefs interface {
	Disabled(ctx context.Context, owner int64) (bool, error)
	SetDisabled(ctx context.Context, owner int64, disabled bool) error
}

// KeyChecker is a Generator that can say, without a call, that it cannot
// serve owner (no OpenRouter key).
type KeyChecker interface {
	CanGenerate(ctx context.Context, owner int64) error
}

// Set is one cached suggestion set.
type Set struct {
	Fingerprint string
	Suggestions []Suggestion
	// Turn is the caller's token for the turn the set answers (the window's
	// StatusSince): Latest's reader serves it only while the turn is the same.
	Turn int64
	At   time.Time
	// Problem is why the set is empty when a call failed (Reason), "" else.
	Problem string
}

type cacheKey struct {
	owner   int64
	subject string
}

type cached struct {
	set     Set
	expired bool
}

type flight struct {
	// cancelled: the call ended because its creator's ctx did (the view that
	// asked closed), so a waiter still open asks again.
	cancelled bool
	done      chan struct{}
	set       Set
	out       Outcome
	err       error
}

// Engine generates and caches suggestion sets.
type Engine struct {
	Gen Generator
	// Off is the kill switch (MUSTER_SUGGEST=off): nothing is generated or
	// served.
	Off   bool
	Prefs Prefs
	// Usage keeps each user's counts and pause (autopause.go); nil keeps them
	// in memory.
	Usage UsageStore
	Now   func() time.Time
	// RecheckTimeout bounds the toggle re-check after a paid call (0 =
	// DefaultRecheckTimeout).
	RecheckTimeout time.Duration
	// PerOwner and Global override the hourly caps (0 = the constants).
	PerOwner, Global int
	Logger           *log.Logger

	mu       sync.Mutex
	cache    map[cacheKey]*cached
	inflight map[cacheKey]map[string]*flight
	calls    map[int64][]time.Time
	global   []time.Time
	epochs   map[int64]int // bumped by Forget
	tallies  map[int64]*tally
	// usageEpochs is bumped per owner by Resume (autopause.go).
	usageEpochs map[int64]int
	sem         chan struct{}
	semOnce     sync.Once
	// Calls counts model calls made (a test reads it).
	Calls int
}

// DefaultRecheckTimeout bounds the toggle re-check after a paid call.
const DefaultRecheckTimeout = 5 * time.Second

func (e *Engine) recheckTimeout() time.Duration {
	if e.RecheckTimeout > 0 {
		return e.RecheckTimeout
	}
	return DefaultRecheckTimeout
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Engine) logf(format string, args ...any) {
	if e.Logger != nil {
		e.Logger.Printf(format, args...)
	}
}

func (e *Engine) init() {
	if e.cache == nil {
		e.cache = map[cacheKey]*cached{}
		e.inflight = map[cacheKey]map[string]*flight{}
		e.calls = map[int64][]time.Time{}
		e.epochs = map[int64]int{}
		e.tallies = map[int64]*tally{}
		e.usageEpochs = map[int64]int{}
	}
}

// Enabled is "" when owner may get suggestions now, or the Outcome that says
// why not (Off, UserOff, Paused). A Prefs read error fails CLOSED (UserOff).
func (e *Engine) Enabled(ctx context.Context, owner int64) Outcome {
	if e == nil || e.Off {
		return OutcomeOff
	}
	if e.Prefs != nil {
		off, err := e.Prefs.Disabled(ctx, owner)
		if err != nil {
			e.logf("suggest: owner %d: read the toggle: %v", owner, err)
			return OutcomeUserOff
		}
		if off {
			return OutcomeUserOff
		}
	}
	if e.paused(ctx, owner) {
		return OutcomePaused
	}
	return ""
}

// Suggest is the set for (owner, subject) built from b: the cached one when
// its fingerprint is b's, otherwise a new one (a model call). turn is the
// caller's turn token, kept with the set (Set.Turn).
func (e *Engine) Suggest(ctx context.Context, owner int64, subject string, b Built, turn int64) (Set, Outcome, error) {
	if o := e.Enabled(ctx, owner); o != "" {
		return Set{}, o, nil
	}
	k := cacheKey{owner, subject}
	// 🔴 ONE CALL PER TURN, NOT JUST PER INPUT. Two views of one turn can hold
	// different inputs (a resumed socket has no backfill), so the input's
	// fingerprint alone would pay twice; the turn token (the window's
	// StatusSince) names the turn whatever the view saw. turn 0 (unknown)
	// falls back to the fingerprint.
	flightKey := "f:" + b.Fingerprint
	if turn != 0 {
		flightKey = "t:" + strconv.FormatInt(turn, 10)
	}
	e.mu.Lock()
	e.init()
	if c, ok := e.cache[k]; ok && (c.set.Fingerprint == b.Fingerprint || (turn != 0 && c.set.Turn == turn)) {
		if turn != 0 && c.set.Fingerprint == b.Fingerprint {
			// The same input under a new token (a resumed CLI rewrote its
			// status): the set still answers it.
			c.set.Turn = turn
		}
		set, expired := c.set, c.expired || e.now().Sub(c.set.At) > CacheTTL
		e.mu.Unlock()
		if expired {
			return Set{Fingerprint: set.Fingerprint, Turn: set.Turn}, OutcomeExpired, nil
		}
		return set, OutcomeCached, nil
	}
	if f, ok := e.inflight[k][flightKey]; ok {
		e.mu.Unlock()
		select {
		case <-f.done:
			if f.out == OutcomeGenerated {
				return f.set, OutcomeCached, nil
			}
			// The asker whose call this was went away (its view closed); this
			// one has not, so it asks again rather than inherit that cancel.
			if f.cancelled && ctx.Err() == nil {
				return e.Suggest(ctx, owner, subject, b, turn)
			}
			return f.set, f.out, f.err
		case <-ctx.Done():
			return Set{}, OutcomeFailed, ctx.Err()
		}
	}
	f := &flight{done: make(chan struct{}), out: OutcomeFailed}
	if e.inflight[k] == nil {
		e.inflight[k] = map[string]*flight{}
	}
	e.inflight[k][flightKey] = f
	epoch := e.epochs[owner]
	e.mu.Unlock()
	// Whatever happens below — a panic included — the flight ends, so a later
	// turn end never waits on a call nobody is making.
	defer func() {
		e.mu.Lock()
		delete(e.inflight[k], flightKey)
		if len(e.inflight[k]) == 0 {
			delete(e.inflight, k)
		}
		e.mu.Unlock()
		close(f.done)
	}()

	set, out, err := e.generate(ctx, owner, b, turn)
	f.set, f.out, f.err = set, out, err
	// Judged by the creator's ctx, not the error's type: the provider wraps a
	// cancelled request in its own error (ErrUnreachable).
	f.cancelled = out == OutcomeFailed && ctx.Err() != nil
	// A refused call (no key, the cap, a cancelled wait) caches nothing: the
	// next turn end (or view) may try again. A call that was MADE (set.At is
	// stamped only after one) caches its answer, empty, failed or not, so the
	// same input is never paid for twice — unless the owner turned
	// suggestions off while it ran (Forget has already emptied their cache).
	//
	// The re-check reads the toggle on a ctx that outlives the asker's
	// deadline (a call that was paid for is not dropped for a timeout) but has
	// its OWN, RecheckTimeout: a hung read fails closed (off) instead of
	// holding the flight, and every view waiting on it, open. And
	// Forget's epoch, compared under the lock at the insert, closes the window
	// between that read and the insert.
	if out == OutcomeGenerated || (out == OutcomeFailed && !set.At.IsZero()) {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), e.recheckTimeout())
		o := e.Enabled(rctx, owner)
		cancel()
		if o != "" {
			f.set, f.out, f.err = Set{}, o, nil
			return Set{}, o, nil
		}
		e.mu.Lock()
		if e.epochs[owner] != epoch {
			e.mu.Unlock()
			f.set, f.out, f.err = Set{}, OutcomeUserOff, nil
			return Set{}, OutcomeUserOff, nil
		}
		e.cache[k] = &cached{set: set}
		e.evictLocked()
		e.mu.Unlock()
	}
	return set, out, err
}

func (e *Engine) generate(ctx context.Context, owner int64, b Built, turn int64) (Set, Outcome, error) {
	set := Set{Fingerprint: b.Fingerprint, Turn: turn}
	if kc, ok := e.Gen.(KeyChecker); ok {
		if err := kc.CanGenerate(ctx, owner); err != nil {
			set.Problem = Reason(err)
			return set, OutcomeFailed, err
		}
	}
	if !e.take(owner) {
		set.Problem = Reason(ErrCapped)
		return set, OutcomeFailed, ErrCapped
	}
	if err := e.acquire(ctx); err != nil {
		return set, OutcomeFailed, err
	}
	g, err := e.Gen.Generate(ctx, owner, b.Prompt)
	e.release()
	if err != nil && ctx.Err() != nil {
		// The asker went away (the view closed) mid-call: nothing is cached,
		// so the next view of this turn asks again.
		return set, OutcomeFailed, err
	}
	set.At = e.now()
	if err != nil {
		e.logf("suggest: owner %d: generation failed: %v", owner, err)
		set.Problem = Reason(err)
		return set, OutcomeFailed, err
	}
	kept, _ := Filter(g.Raw, b)
	set.Suggestions = kept
	return set, OutcomeGenerated, nil
}

// take reserves one call for owner under the caps, or reports false.
func (e *Engine) take(owner int64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	per, global := e.PerOwner, e.Global
	if per <= 0 {
		per = PerOwnerPerHour
	}
	if global <= 0 {
		global = GlobalPerHour
	}
	now := e.now()
	cut := now.Add(-time.Hour)
	prune := func(ts []time.Time) []time.Time {
		i := sort.Search(len(ts), func(i int) bool { return ts[i].After(cut) })
		return ts[i:]
	}
	e.global = prune(e.global)
	mine := prune(e.calls[owner])
	if len(mine) >= per || len(e.global) >= global {
		e.calls[owner] = mine
		return false
	}
	e.calls[owner] = append(mine, now)
	e.global = append(e.global, now)
	e.Calls++
	return true
}

func (e *Engine) acquire(ctx context.Context) error {
	e.semOnce.Do(func() { e.sem = make(chan struct{}, MaxConcurrent) })
	select {
	case e.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *Engine) release() { <-e.sem }

// evictLocked drops the least recently generated subjects past MaxCached.
func (e *Engine) evictLocked() {
	if len(e.cache) <= MaxCached {
		return
	}
	type kv struct {
		k  cacheKey
		at time.Time
	}
	all := make([]kv, 0, len(e.cache))
	for k, c := range e.cache {
		all = append(all, kv{k, c.set.At})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
	for _, x := range all[:len(all)-MaxCached] {
		delete(e.cache, x.k)
	}
}

// Latest is (owner, subject)'s newest set while its text is within CacheTTL,
// and whether there is one. It never calls.
func (e *Engine) Latest(owner int64, subject string) (Set, bool) {
	if e == nil {
		return Set{}, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.init()
	c, ok := e.cache[cacheKey{owner, subject}]
	if !ok || c.expired || e.now().Sub(c.set.At) > CacheTTL {
		return Set{}, false
	}
	return c.set, true
}

// Sweep drops the text of every set older than CacheTTL, keeping only its
// fingerprint (the skip). It returns how many sets it emptied.
func (e *Engine) Sweep() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.init()
	n := 0
	now := e.now()
	for _, c := range e.cache {
		if !c.expired && now.Sub(c.set.At) > CacheTTL {
			c.expired = true
			c.set.Suggestions = nil
			n++
		}
	}
	return n
}

// Run sweeps every SweepInterval until ctx ends.
func (e *Engine) Run(ctx context.Context) {
	t := time.NewTicker(SweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.Sweep()
		}
	}
}

// Forget drops owner's cached sets (the toggle turned off: nothing of theirs
// stays in memory).
func (e *Engine) Forget(owner int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.init()
	for k := range e.cache {
		if k.owner == owner {
			delete(e.cache, k)
		}
	}
	// A call already running for owner caches nothing (Suggest compares it).
	e.epochs[owner]++
}
