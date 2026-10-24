package suggest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// countingGen is a fake model: it counts calls and answers one grounded pick.
type countingGen struct {
	mu    sync.Mutex
	calls int
	err   error
	block chan struct{}
}

func (g *countingGen) Generate(ctx context.Context, _ int64, _ string) (Generation, error) {
	g.mu.Lock()
	g.calls++
	block, err := g.block, g.err
	g.mu.Unlock()
	if block != nil {
		<-block
	}
	if err != nil {
		return Generation{}, err
	}
	return Generation{Raw: []Raw{{Text: "Go with (a), checkpoint every 500 rows", Kind: KindPick, Cites: []string{"R"}, Quote: "(a) checkpoint every 500 rows"}}}, nil
}

func (g *countingGen) n() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

type memPrefs struct{ off map[int64]bool }

func (m *memPrefs) Disabled(_ context.Context, o int64) (bool, error) { return m.off[o], nil }
func (m *memPrefs) SetDisabled(_ context.Context, o int64, d bool) error {
	m.off[o] = d
	return nil
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func turnInput(t *testing.T, reply string) Built {
	t.Helper()
	b, ok := Build(Input{Tail: []Entry{{Role: RoleAssistant, Text: reply}}})
	if !ok {
		t.Fatal("no reply")
	}
	return b
}

const turn1 = "Two ways: (a) checkpoint every 500 rows, (b) re-run by batch id. Which do you prefer?"
const turn2 = "Done with (a): (a) checkpoint every 500 rows is in. Should I open the PR?"

// TestTheSameInputNeverCallsTwiceAndANewTurnCallsExactlyOnce: the fingerprint
// skip, and its positive control.
func TestTheSameInputNeverCallsTwiceAndANewTurnCallsExactlyOnce(t *testing.T) {
	g := &countingGen{}
	e := &Engine{Gen: g}
	ctx := context.Background()
	b1 := turnInput(t, turn1)
	set, out, err := e.Suggest(ctx, 1, "s", b1, 10)
	if err != nil || out != OutcomeGenerated || len(set.Suggestions) != 1 || g.n() != 1 {
		t.Fatalf("first: %v %v %+v calls=%d", out, err, set, g.n())
	}
	for i := 0; i < 5; i++ {
		if _, out, _ := e.Suggest(ctx, 1, "s", b1, 10); out != OutcomeCached {
			t.Fatalf("repeat %d: %v", i, out)
		}
	}
	if g.n() != 1 {
		t.Fatalf("an unchanged input called the model %d times", g.n())
	}
	if _, out, _ := e.Suggest(ctx, 1, "s", turnInput(t, turn2), 11); out != OutcomeGenerated || g.n() != 2 {
		t.Fatalf("a new turn: %v, calls %d (want exactly 2)", out, g.n())
	}
	// Another subject with the same input is its own entry.
	if _, out, _ := e.Suggest(ctx, 1, "other", b1, 10); out != OutcomeGenerated || g.n() != 3 {
		t.Fatalf("another subject: %v calls %d", out, g.n())
	}
}

// TestTextExpiresAfterTheTTLButTheSkipHolds: past CacheTTL, Sweep drops the
// text and Latest serves nothing — and the same input still makes no call.
func TestTextExpiresAfterTheTTLButTheSkipHolds(t *testing.T) {
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	g := &countingGen{}
	e := &Engine{Gen: g, Now: c.now}
	b := turnInput(t, turn1)
	if _, out, _ := e.Suggest(context.Background(), 1, "s", b, 10); out != OutcomeGenerated {
		t.Fatal(out)
	}
	if s, ok := e.Latest(1, "s"); !ok || len(s.Suggestions) != 1 {
		t.Fatal("positive control: the set is served within the TTL")
	}
	c.t = c.t.Add(CacheTTL + time.Second)
	if _, ok := e.Latest(1, "s"); ok {
		t.Fatal("an expired set was served before the sweep")
	}
	if n := e.Sweep(); n != 1 {
		t.Fatalf("swept %d", n)
	}
	e.mu.Lock()
	text := e.cache[cacheKey{1, "s"}].set.Suggestions
	e.mu.Unlock()
	if text != nil {
		t.Fatal("the sweep left the text in memory")
	}
	if _, out, _ := e.Suggest(context.Background(), 1, "s", b, 10); out != OutcomeExpired || g.n() != 1 {
		t.Fatalf("an expired, unchanged input: %v, calls %d", out, g.n())
	}
}

// TestTheRateCapRefusesThe121stCallInAnHour: 120 per owner per hour; the
// 121st is refused without a call; another owner is unaffected; an hour later
// it is allowed again.
func TestTheRateCapRefusesThe121stCallInAnHour(t *testing.T) {
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	g := &countingGen{}
	e := &Engine{Gen: g, Now: c.now}
	ctx := context.Background()
	for i := 0; i < PerOwnerPerHour; i++ {
		b := turnInput(t, turn1+" v"+string(rune('a'+i%26))+string(rune('a'+i/26)))
		if _, out, err := e.Suggest(ctx, 1, "s", b, int64(i)); out != OutcomeGenerated {
			t.Fatalf("call %d: %v %v", i, out, err)
		}
	}
	b := turnInput(t, turn2)
	if _, out, err := e.Suggest(ctx, 1, "s", b, 999); out != OutcomeFailed || !errors.Is(err, ErrCapped) || g.n() != PerOwnerPerHour {
		t.Fatalf("the 121st: %v %v calls %d", out, err, g.n())
	}
	if _, out, _ := e.Suggest(ctx, 2, "s", b, 1); out != OutcomeGenerated {
		t.Fatalf("another owner: %v", out)
	}
	c.t = c.t.Add(time.Hour + time.Second)
	if _, out, _ := e.Suggest(ctx, 1, "s", b, 999); out != OutcomeGenerated {
		t.Fatalf("an hour later: %v", out)
	}
}

// TestTheKillSwitchAndTheToggleMakeNoCall.
func TestTheKillSwitchAndTheToggleMakeNoCall(t *testing.T) {
	g := &countingGen{}
	prefs := &memPrefs{off: map[int64]bool{2: true}}
	e := &Engine{Gen: g, Prefs: prefs, Off: true}
	b := turnInput(t, turn1)
	if _, out, _ := e.Suggest(context.Background(), 1, "s", b, 1); out != OutcomeOff {
		t.Fatal(out)
	}
	e.Off = false
	if _, out, _ := e.Suggest(context.Background(), 2, "s", b, 1); out != OutcomeUserOff {
		t.Fatal(out)
	}
	if g.n() != 0 {
		t.Fatalf("%d calls while off", g.n())
	}
	if _, out, _ := e.Suggest(context.Background(), 1, "s", b, 1); out != OutcomeGenerated || g.n() != 1 {
		t.Fatalf("positive control: owner 1 is on by default: %v", out)
	}
}

// TestTwoViewsOfOneTurnMakeOneCall: concurrent asks for the same input share
// one call.
func TestTwoViewsOfOneTurnMakeOneCall(t *testing.T) {
	g := &countingGen{block: make(chan struct{})}
	e := &Engine{Gen: g}
	b := turnInput(t, turn1)
	var wg sync.WaitGroup
	outs := make([]Outcome, 2)
	for i := range outs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, outs[i], _ = e.Suggest(context.Background(), 1, "s", b, 1)
		}(i)
	}
	for g.n() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(g.block)
	wg.Wait()
	if g.n() != 1 {
		t.Fatalf("%d calls for one input (%v)", g.n(), outs)
	}
}

// TestAFailedCallIsCachedButARefusedOneIsNot: a call that was made and
// failed is not repeated for the same input; a key lookup that refused before
// any call is retried.
func TestAFailedCallIsCachedButARefusedOneIsNot(t *testing.T) {
	g := &countingGen{err: ErrMalformed}
	e := &Engine{Gen: g}
	b := turnInput(t, turn1)
	if _, out, _ := e.Suggest(context.Background(), 1, "s", b, 1); out != OutcomeFailed {
		t.Fatal(out)
	}
	if _, out, _ := e.Suggest(context.Background(), 1, "s", b, 1); out != OutcomeCached || g.n() != 1 {
		t.Fatalf("a failed input was called again: %v %d", out, g.n())
	}
	kc := &keyless{}
	e2 := &Engine{Gen: kc}
	for i := 0; i < 2; i++ {
		_, out, _ := e2.Suggest(context.Background(), 1, "s", b, 1)
		if out != OutcomeFailed {
			t.Fatal(out)
		}
	}
	if kc.checks != 2 || kc.calls != 0 {
		t.Fatalf("checks %d calls %d", kc.checks, kc.calls)
	}
}

type keyless struct{ checks, calls int }

func (k *keyless) CanGenerate(context.Context, int64) error { k.checks++; return ErrKeyLookup }
func (k *keyless) Generate(context.Context, int64, string) (Generation, error) {
	k.calls++
	return Generation{}, nil
}

// ctxGen blocks until its context ends, then fails with that.
type ctxGen struct{ calls int }

func (g *ctxGen) Generate(ctx context.Context, _ int64, _ string) (Generation, error) {
	g.calls++
	if g.calls == 1 {
		<-ctx.Done()
		// As the OpenRouter generator does: the cancel wrapped in its own
		// error, not a bare ctx error.
		return Generation{}, fmt.Errorf("%w: %v", ErrUnreachable, ctx.Err())
	}
	return Generation{Raw: []Raw{{Text: "Go with (a), checkpoint every 500 rows", Kind: KindPick, Cites: []string{"R"}, Quote: "(a) checkpoint every 500 rows"}}}, nil
}

// TestACallCancelledByTheViewClosingIsNotCached: the view that asked went
// away mid-call, so the next view of the same turn asks again.
func TestACallCancelledByTheViewClosingIsNotCached(t *testing.T) {
	g := &ctxGen{}
	e := &Engine{Gen: g}
	b := turnInput(t, turn1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	if _, out, _ := e.Suggest(ctx, 1, "s", b, 1); out != OutcomeFailed {
		t.Fatal(out)
	}
	if _, out, _ := e.Suggest(context.Background(), 1, "s", b, 1); out != OutcomeGenerated || g.calls != 2 {
		t.Fatalf("after a cancelled call: %v, calls %d", out, g.calls)
	}
}

// TestOneTurnIsOneCallWhateverInputTheViewSaw: two views of the SAME turn
// with different inputs (a resumed socket saw less) share one call; the next
// turn calls again.
func TestOneTurnIsOneCallWhateverInputTheViewSaw(t *testing.T) {
	g := &countingGen{}
	e := &Engine{Gen: g}
	full, partial := turnInput(t, turn1), turnInput(t, turn1+" ")
	partial.Fingerprint = "a-different-input"
	if _, out, _ := e.Suggest(context.Background(), 1, "s", full, 42); out != OutcomeGenerated {
		t.Fatal(out)
	}
	if set, out, _ := e.Suggest(context.Background(), 1, "s", partial, 42); out != OutcomeCached || g.n() != 1 || set.Turn != 42 {
		t.Fatalf("a second view of turn 42: %v, calls %d", out, g.n())
	}
	if _, out, _ := e.Suggest(context.Background(), 1, "s", partial, 43); out != OutcomeGenerated || g.n() != 2 {
		t.Fatalf("turn 43: %v, calls %d", out, g.n())
	}
}

// TestTurningOffDuringACallCachesNothing: the owner turned suggestions off
// (and Forget emptied their cache) while a call ran: its answer is not kept.
func TestTurningOffDuringACallCachesNothing(t *testing.T) {
	g := &countingGen{block: make(chan struct{})}
	prefs := &memPrefs{off: map[int64]bool{}}
	e := &Engine{Gen: g, Prefs: prefs}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = e.Suggest(context.Background(), 1, "s", turnInput(t, turn1), 7)
	}()
	for g.n() == 0 {
		time.Sleep(time.Millisecond)
	}
	// The toggle alone (no Forget): the re-check is what keeps it out.
	prefs.SetDisabled(context.Background(), 1, true)
	close(g.block)
	<-done
	e.mu.Lock()
	n := len(e.cache)
	e.mu.Unlock()
	if n != 0 {
		t.Fatal("an answer for an owner who turned suggestions off was cached")
	}
}

// TestAWaiterDoesNotInheritTheCreatorsCancel: the view that started a call
// closed; another view of the same turn, still open, asks again and gets a set.
func TestAWaiterDoesNotInheritTheCreatorsCancel(t *testing.T) {
	g := &ctxGen{}
	e := &Engine{Gen: g}
	b := turnInput(t, turn1)
	ctx, cancel := context.WithCancel(context.Background())
	created := make(chan struct{})
	go func() {
		close(created)
		_, _, _ = e.Suggest(ctx, 1, "s", b, 5)
	}()
	<-created
	for {
		e.mu.Lock()
		n := len(e.inflight)
		e.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	res := make(chan Outcome, 1)
	go func() {
		_, out, _ := e.Suggest(context.Background(), 1, "s", b, 5)
		res <- out
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if out := <-res; out != OutcomeGenerated {
		t.Fatalf("the open view got %v", out)
	}
}

// TestForgetDuringACallCachesNothing: Forget (the toggle turned off) landed
// after the call began but before its answer was cached, and the toggle read
// still said on (the race between the re-check and the insert): the epoch
// keeps the answer out.
func TestForgetDuringACallCachesNothing(t *testing.T) {
	g := &countingGen{block: make(chan struct{})}
	e := &Engine{Gen: g}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = e.Suggest(context.Background(), 1, "s", turnInput(t, turn1), 7)
	}()
	for g.n() == 0 {
		time.Sleep(time.Millisecond)
	}
	e.Forget(1)
	close(g.block)
	<-done
	if _, ok := e.Latest(1, "s"); ok {
		t.Fatal("an answer that finished after Forget was cached")
	}
}

// hangingPrefs answers the first read (the gate before the call) and hangs
// every later one until its ctx ends — a database that stopped answering.
type hangingPrefs struct {
	mu    sync.Mutex
	reads int
}

func (h *hangingPrefs) Disabled(ctx context.Context, _ int64) (bool, error) {
	h.mu.Lock()
	h.reads++
	n := h.reads
	h.mu.Unlock()
	if n == 1 {
		return false, nil
	}
	<-ctx.Done()
	return false, ctx.Err()
}
func (h *hangingPrefs) SetDisabled(context.Context, int64, bool) error { return nil }

// TestAHungToggleReadAfterAPaidCallFailsClosedInTime: the re-check after the
// call has its own deadline even though the asker's ctx has none; it fails
// closed (off, nothing cached) instead of holding the flight open.
func TestAHungToggleReadAfterAPaidCallFailsClosedInTime(t *testing.T) {
	g := &countingGen{}
	e := &Engine{Gen: g, Prefs: &hangingPrefs{}, RecheckTimeout: 50 * time.Millisecond}
	done := make(chan Outcome, 1)
	go func() {
		_, out, _ := e.Suggest(context.Background(), 1, "s", turnInput(t, turn1), 9)
		done <- out
	}()
	select {
	case out := <-done:
		if out != OutcomeUserOff || g.n() != 1 {
			t.Fatalf("outcome %v, calls %d", out, g.n())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the re-check hung the call past its deadline")
	}
	e.mu.Lock()
	n := len(e.cache) + len(e.inflight)
	e.mu.Unlock()
	if n != 0 {
		t.Fatal("a call whose re-check failed was cached, or its flight stayed open")
	}
}
