package suggest

import (
	"context"
	"strconv"
	"sync"
	"testing"
)

// stats is e.Stats for owner, failing the test on an error.
func stats(t *testing.T, e *Engine, owner int64) Tally {
	t.Helper()
	st, err := e.Stats(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// served has e generate n distinct sets for owner 1 (subject "s<i>"), and
// returns each set's fingerprint.
func served(t *testing.T, e *Engine, n int) []string {
	t.Helper()
	var fps []string
	for i := 0; i < n; i++ {
		b := turnInput(t, turn1+" (round "+strconv.Itoa(i)+")")
		set, out, err := e.Suggest(context.Background(), 1, "s"+strconv.Itoa(i), b, int64(1000+i))
		if err != nil || out != OutcomeGenerated || len(set.Suggestions) == 0 {
			t.Fatalf("set %d: %v %v %+v", i, out, err, set)
		}
		fps = append(fps, set.Fingerprint)
	}
	return fps
}

// TestTheAutoPauseIsUnderFivePercentFilledAfterTwoHundredShown: the rule at
// both of its boundaries — 199 shown with no fill is not judged; 200 shown
// with 9 filled (4.5 %) pauses, with 10 (5 %) does not. Paused, Enabled says
// so and Suggest makes no call; Resume (the toggle back on) clears it.
func TestTheAutoPauseIsUnderFivePercentFilledAfterTwoHundredShown(t *testing.T) {
	run := func(shown, everyNth, skipFills int) (*Engine, *countingGen) {
		g := &countingGen{}
		e := &Engine{Gen: g, PerOwner: 10_000, Global: 10_000}
		fps := served(t, e, shown)
		fills := 0
		for i, fp := range fps {
			if !e.Record(context.Background(), 1, "s"+strconv.Itoa(i), fp, EventShown, 0) {
				t.Fatalf("shown %d did not count", i)
			}
			if everyNth > 0 && i%everyNth == 0 {
				fills++
				if fills <= skipFills {
					continue
				}
				if !e.Record(context.Background(), 1, "s"+strconv.Itoa(i), fp, EventFilled, 0) {
					t.Fatalf("filled %d did not count", i)
				}
			}
		}
		return e, g
	}
	ctx := context.Background()

	if e, _ := run(199, 0, 0); e.Enabled(ctx, 1) != "" || stats(t, e, 1).Paused {
		t.Fatalf("199 shown, none filled: judged too early: %+v", stats(t, e, 1))
	}
	if e, _ := run(200, 20, 0); e.Enabled(ctx, 1) != "" || stats(t, e, 1).Filled != 10 {
		t.Fatalf("200 shown, 10 filled (5%%): paused: %+v", stats(t, e, 1))
	}
	e, g := run(200, 20, 1) // 9 filled: 4.5 %
	if st := stats(t, e, 1); !st.Paused || st.Shown != 200 || st.Filled != 9 {
		t.Fatalf("200 shown, 9 filled: %+v", st)
	}
	if o := e.Enabled(ctx, 1); o != OutcomePaused {
		t.Fatalf("Enabled: %q", o)
	}
	before := g.n()
	if _, out, _ := e.Suggest(ctx, 1, "fresh", turnInput(t, turn2), 5); out != OutcomePaused || g.n() != before {
		t.Fatalf("paused, Suggest: %v, calls %d → %d", out, before, g.n())
	}
	// Another owner is not paused by owner 1's tally.
	if o := e.Enabled(ctx, 2); o != "" {
		t.Fatalf("owner 2: %q", o)
	}
	e.Resume(ctx, 1)
	if o := e.Enabled(ctx, 1); o != "" || stats(t, e, 1) != (Tally{}) {
		t.Fatalf("resumed: %q %+v", o, stats(t, e, 1))
	}
}

// gateUsage is a UsageStore that is paused until Reset, whose Get can be
// held open (hold) to race a Resume.
type gateUsage struct {
	mu     sync.Mutex
	paused bool
	hold   chan struct{} // non-nil: the next Get reads, then waits on it
	read   chan struct{} // closed when that Get has read
}

func (g *gateUsage) Get(context.Context, int64) (Tally, error) {
	g.mu.Lock()
	t := Tally{Shown: AutoPauseMinShown, Paused: g.paused}
	hold, read := g.hold, g.read
	g.hold = nil
	g.mu.Unlock()
	if hold != nil {
		close(read)
		<-hold
	}
	return t, nil
}
func (g *gateUsage) Add(context.Context, int64, int64, int64, int64, int64) (Tally, error) {
	return Tally{}, nil
}
func (g *gateUsage) SetPaused(_ context.Context, _ int64, p bool) error {
	g.mu.Lock()
	g.paused = p
	g.mu.Unlock()
	return nil
}
func (g *gateUsage) Reset(context.Context, int64) error {
	g.mu.Lock()
	g.paused = false
	g.mu.Unlock()
	return nil
}

// TestAResumeIsNotUndoneByAReadThatRacedIt (final pass): a paused() read the
// store BEFORE a Resume reset it, and lands after Resume cleared memory. That
// stale "paused" must not be cached — the user would stay paused until a
// restart. After the race, the user is not paused.
func TestAResumeIsNotUndoneByAReadThatRacedIt(t *testing.T) {
	ctx := context.Background()
	g := &gateUsage{paused: true}
	e := &Engine{Gen: &countingGen{}, Usage: g}
	if o := e.Enabled(ctx, 1); o != OutcomePaused {
		t.Fatalf("setup: %q", o)
	}
	// Forget what Enabled cached, so the next paused() reads the store.
	e.mu.Lock()
	delete(e.tallies, 1)
	e.mu.Unlock()
	hold, read := make(chan struct{}), make(chan struct{})
	g.mu.Lock()
	g.hold, g.read = hold, read
	g.mu.Unlock()
	done := make(chan struct{})
	go func() { e.Enabled(ctx, 1); close(done) }() // reads paused=true, then waits
	<-read
	// Resume: its own paused() check reads the store (not held) — paused —
	// then resets it and clears memory, while the first read is still out.
	e.Resume(ctx, 1)
	close(hold)
	<-done
	if o := e.Enabled(ctx, 1); o != "" {
		t.Fatalf("the stale read stuck: %q", o)
	}
}

// TestASetCountsOnceAndOnlyWhenItWasServed: a repeated event counts once; a
// fill or a send of a set never counted as shown does not count; a "shown" of
// a fingerprint this engine did not serve that owner for that subject does
// not count; an unknown event does not count. A send adds its edit distance.
func TestASetCountsOnceAndOnlyWhenItWasServed(t *testing.T) {
	e := &Engine{Gen: &countingGen{}}
	fp := served(t, e, 1)[0]
	cases := []struct {
		owner      int64
		subject    string
		fp, ev     string
		dist       int
		wantCounts bool
	}{
		{1, "s0", fp, EventFilled, 0, false}, // filled before shown
		{1, "s0", fp, EventSent, 3, false},   // sent before shown
		{1, "s0", "f-not-served", EventShown, 0, false},
		{1, "other", fp, EventShown, 0, false}, // served for s0, not other
		{2, "s0", fp, EventShown, 0, false},    // another owner's
		{1, "s0", fp, "dismissed", 0, false},
		{1, "s0", fp, EventShown, 0, true},
		{1, "s0", fp, EventShown, 0, false}, // a repeat
		{1, "s0", fp, EventFilled, 0, true},
		{1, "s0", fp, EventFilled, 0, false},
		{1, "s0", fp, EventSent, 7, true},
		{1, "s0", fp, EventSent, 7, false},
	}
	for i, c := range cases {
		if got := e.Record(context.Background(), c.owner, c.subject, c.fp, c.ev, c.dist); got != c.wantCounts {
			t.Fatalf("case %d %+v: counted=%v", i, c, got)
		}
	}
	if st := stats(t, e, 1); st != (Tally{Shown: 1, Filled: 1, Sent: 1, EditSum: 7}) {
		t.Fatalf("tally %+v", st)
	}
}
