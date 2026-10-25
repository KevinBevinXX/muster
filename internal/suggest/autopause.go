package suggest

import "context"

// THE AUTO-PAUSE (operator decision): suggestions pause for a user who rarely
// uses them — fewer than AutoPauseRatePercent of the sets shown were tapped,
// once AutoPauseMinShown sets have been shown — so the chip row cannot become
// a row of ignored noise.
//
// 🔴 NUMBERS ONLY, AND PERSISTED. What the composer reports (Record) is an
// event name, the set's fingerprint and, for a send, the edit distance between
// what a tap filled and what was sent: never any text. The COUNTS — shown,
// filled, sent, the edit-distance sum and the pause — are kept per user in
// Usage (suggest_usage, migration 0049) when the engine has one, so a pause
// and the Account readout survive a restart; without one (no database) they
// live in memory. The fingerprints that make "a set counts once" are always
// in memory (bounded), and are never stored.
//
// 🔴 A SET COUNTS ONCE. "shown" counts a fingerprint once per user however
// often a page re-renders it, and only a fingerprint the engine actually
// served that user (their newest set for that subject); "filled" and "sent"
// count only a fingerprint already counted as shown, once each — so the fill
// rate is at most 1 and cannot be inflated by repeating an event.
//
// Turning suggestions back on (the toggle, on=1) resumes a paused user and
// starts their counts over (Resume); a user who was not paused keeps them.

// The auto-pause's bounds.
const (
	// AutoPauseMinShown is how many sets must have been shown before the rate
	// is judged.
	AutoPauseMinShown = 200
	// AutoPauseRatePercent: below this percentage of shown sets filled, the
	// user is paused.
	AutoPauseRatePercent = 5
	// maxTallied bounds the fingerprints remembered per user (the oldest are
	// forgotten first; their counts stay).
	maxTallied = 1000
)

// The events the composer reports.
const (
	EventShown  = "shown"
	EventFilled = "filled"
	EventSent   = "sent"
)

// OutcomePaused is Enabled's answer for a user the auto-pause paused.
const OutcomePaused Outcome = "paused"

// Tally is one user's counts.
type Tally struct {
	Shown, Filled, Sent int64
	// EditSum is the sum of the edit distances reported with Sent.
	EditSum int64
	Paused  bool
}

// FillPercent is the percentage of shown sets that were filled (0 with none).
func (t Tally) FillPercent() float64 {
	if t.Shown == 0 {
		return 0
	}
	return 100 * float64(t.Filled) / float64(t.Shown)
}

// MeanEdit is the mean edit distance of a send after a tap (0 with none).
func (t Tally) MeanEdit() float64 {
	if t.Sent == 0 {
		return 0
	}
	return float64(t.EditSum) / float64(t.Sent)
}

// shouldPause is THE RULE: judged once enough sets were shown.
func (t Tally) shouldPause() bool {
	return t.Shown >= AutoPauseMinShown && t.Filled*100 < t.Shown*AutoPauseRatePercent
}

// UsageStore keeps each user's counts (PGUsage: suggest_usage, migration 0049).
// ctx acts as the owner it names (row-level security).
type UsageStore interface {
	// Get is owner's counts (zero with none).
	Get(ctx context.Context, owner int64) (Tally, error)
	// Add adds to owner's counts and returns them after.
	Add(ctx context.Context, owner int64, shown, filled, sent, edit int64) (Tally, error)
	// SetPaused records the pause.
	SetPaused(ctx context.Context, owner int64, paused bool) error
	// Reset starts owner's counts over, un-paused.
	Reset(ctx context.Context, owner int64) error
}

type tally struct {
	// counts is the in-memory tally (no Usage), or the last persisted pause
	// seen (loaded) when there is one.
	counts Tally
	loaded bool
	// flags is what has been counted per fingerprint (1 shown, 2 filled,
	// 4 sent), and order the fingerprints oldest first, for the bound.
	flags map[string]uint8
	order []string
}

func (e *Engine) tallyLocked(owner int64) *tally {
	e.init()
	t := e.tallies[owner]
	if t == nil {
		t = &tally{flags: map[string]uint8{}}
		e.tallies[owner] = t
	}
	return t
}

// Record counts one composer event for owner: ev is EventShown, EventFilled
// or EventSent, fp the set's fingerprint, subject the conversation it was
// served for, dist the edit distance (EventSent only). It reports whether the
// event counted (a repeat, an unknown event or fingerprint does not).
func (e *Engine) Record(ctx context.Context, owner int64, subject, fp, ev string, dist int) bool {
	if e == nil || fp == "" || owner <= 0 {
		return false
	}
	var bit uint8
	switch ev {
	case EventShown:
		bit = 1
	case EventFilled:
		bit = 2
	case EventSent:
		bit = 4
	default:
		return false
	}
	if dist < 0 {
		dist = 0
	}
	e.mu.Lock()
	t := e.tallyLocked(owner)
	have, known := t.flags[fp]
	switch {
	case have&bit != 0:
		e.mu.Unlock()
		return false
	case bit == 1:
		// Only a set this engine served this owner for that subject.
		c, ok := e.cache[cacheKey{owner, subject}]
		if !ok || c.set.Fingerprint != fp || len(c.set.Suggestions) == 0 {
			e.mu.Unlock()
			return false
		}
	case !known || have&1 == 0:
		e.mu.Unlock()
		return false
	}
	if !known {
		t.order = append(t.order, fp)
		if len(t.order) > maxTallied {
			delete(t.flags, t.order[0])
			t.order = t.order[1:]
		}
	}
	t.flags[fp] = have | bit
	var d Tally
	switch bit {
	case 1:
		d.Shown = 1
	case 2:
		d.Filled = 1
	case 4:
		d.Sent, d.EditSum = 1, int64(dist)
	}
	if e.Usage == nil {
		t.counts.Shown += d.Shown
		t.counts.Filled += d.Filled
		t.counts.Sent += d.Sent
		t.counts.EditSum += d.EditSum
		pause := !t.counts.Paused && t.counts.shouldPause()
		if pause {
			t.counts.Paused = true
		}
		c := t.counts
		e.mu.Unlock()
		if pause {
			e.logPaused(owner, c)
		}
		return true
	}
	epoch := e.usageEpochs[owner]
	e.mu.Unlock()
	tot, err := e.Usage.Add(ctx, owner, d.Shown, d.Filled, d.Sent, d.EditSum)
	if err != nil {
		e.logf("suggest: owner %d: record %s: %v", owner, ev, err)
		return true
	}
	if !tot.Paused && tot.shouldPause() {
		if err := e.Usage.SetPaused(ctx, owner, true); err != nil {
			e.logf("suggest: owner %d: record the pause: %v", owner, err)
		} else {
			tot.Paused = true
			e.logPaused(owner, tot)
		}
	}
	e.mu.Lock()
	if e.usageEpochs[owner] == epoch {
		t = e.tallyLocked(owner)
		t.counts.Paused, t.loaded = tot.Paused, true
	}
	e.mu.Unlock()
	return true
}

func (e *Engine) logPaused(owner int64, t Tally) {
	e.logf("suggest: owner %d: paused — %d of %d shown sets were tapped (under %d%%)", owner, t.Filled, t.Shown, AutoPauseRatePercent)
}

// Stats is owner's counts: persisted (Usage) or in memory.
func (e *Engine) Stats(ctx context.Context, owner int64) (Tally, error) {
	if e == nil {
		return Tally{}, nil
	}
	if e.Usage != nil {
		return e.Usage.Get(ctx, owner)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if t := e.tallies[owner]; t != nil {
		return t.counts, nil
	}
	return Tally{}, nil
}

// Resume un-pauses owner, starting their counts over; a user who was not
// paused keeps them.
func (e *Engine) Resume(ctx context.Context, owner int64) {
	if e == nil {
		return
	}
	if !e.paused(ctx, owner) {
		return
	}
	// 🔴 THE STORE FIRST, THEN MEMORY, AND AN EPOCH: a paused() that read the
	// store BEFORE the reset (paused) must not cache that read after the
	// memory was cleared — it would stick until a restart. Resume bumps the
	// owner's epoch; paused() caches only a read whose epoch is unchanged.
	if e.Usage != nil {
		if err := e.Usage.Reset(ctx, owner); err != nil {
			e.logf("suggest: owner %d: resume: %v", owner, err)
			return
		}
	}
	e.mu.Lock()
	e.init()
	delete(e.tallies, owner)
	e.usageEpochs[owner]++
	e.mu.Unlock()
}

// paused reports whether the auto-pause paused owner: persisted (read once,
// then known from Record), or in memory. A read that fails reads as NOT
// paused (the pause is a convenience, never a safety gate) and is retried.
func (e *Engine) paused(ctx context.Context, owner int64) bool {
	e.mu.Lock()
	t := e.tallies[owner]
	if e.Usage == nil || (t != nil && t.loaded) {
		p := t != nil && t.counts.Paused
		e.mu.Unlock()
		return p
	}
	e.init()
	epoch := e.usageEpochs[owner]
	e.mu.Unlock()
	got, err := e.Usage.Get(ctx, owner)
	if err != nil {
		e.logf("suggest: owner %d: read the counts: %v", owner, err)
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.usageEpochs[owner] != epoch {
		// A Resume ran while this read was out: it is stale; cache nothing.
		return false
	}
	t = e.tallyLocked(owner)
	t.counts.Paused, t.loaded = got.Paused, true
	return got.Paused
}
