package sendqueue

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestTheDispatcherHoldsAWindowBehindAnEditAndSendsTheSavedText: with the
// window idle long enough, a held head is not sent and neither is the message
// behind it; Save releases it, and the next pass sends the EDITED text first.
func TestTheDispatcherHoldsAWindowBehindAnEditAndSendsTheSavedText(t *testing.T) {
	s, _ := pgStore(t)
	f, c := &fakeTarget{}, &clock{t: time.Now()}
	d := newDispatcher(s, f, c)
	a := mustEnqueue(t, s, 2, mirrorItem("draft wording"))
	mustEnqueue(t, s, 2, mirrorItem("queued behind"))
	h, err := s.Hold(as(2), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.set(idleSince(c.now().Add(-time.Hour)))
	tick(t, d)
	tick(t, d)
	wantSent(t, f, "while the head is held")
	if _, err := s.SaveEdit(as(2), a.ID, h.HoldToken, "final wording", time.Now()); err != nil {
		t.Fatal(err)
	}
	tick(t, d)
	wantSent(t, f, "after the Save", "final wording")
}

// TestTheDispatcherNeverSendsDuringAnEdit is the race at the dispatcher's own
// level: a dispatcher ticks as fast as it can over a window that is always
// idle with a newer status (every turn has ended), while messages are queued,
// held, edited and saved. A send that happens while a hold is KNOWN to be
// committed and its Save has not started is a violation; and every text sent
// is a whole one — the original (the dispatcher won before the hold) or the
// saved one, and every saved edit is sent as edited.
func TestTheDispatcherNeverSendsDuringAnEdit(t *testing.T) {
	s, _ := pgStore(t)
	c := &clock{t: time.Now()}
	// heldConfirmed is the id of the row whose hold is KNOWN committed and whose
	// Save has not started (0: none). Another row being sent meanwhile — the
	// previous one, claimed before this one was queued — is not a violation.
	var heldConfirmed atomic.Int64
	var violations atomic.Int32
	var since atomic.Int64
	since.Store(time.Now().Add(-time.Hour).UnixMilli())
	f := &fakeTarget{}
	d := newDispatcher(s, f, c)
	d.Probe = func(context.Context, Head) AgentState {
		// Idle, observed long ago, with a status time newer than any paste.
		return AgentState{Idle: true, Since: since.Add(1), Observed: c.now().Add(-time.Hour)}
	}
	d.Send = func(ctx context.Context, it Item, st AgentState) Outcome {
		if heldConfirmed.Load() == it.ID {
			violations.Add(1)
		}
		return f.send(ctx, it, st)
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := d.Tick(context.Background()); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	const n = 30
	edits := 0
	for i := range n {
		it := mustEnqueue(t, s, 2, mirrorItem("original "+string(rune('a'+i%26))))
		h, err := s.Hold(as(2), it.ID)
		if err != nil {
			// The dispatcher claimed it first: it is being (or was) sent whole.
			continue
		}
		heldConfirmed.Store(it.ID)
		time.Sleep(3 * time.Millisecond)
		heldConfirmed.Store(0)
		if _, err := s.SaveEdit(as(2), it.ID, h.HoldToken, "edited "+string(rune('a'+i%26)), time.Now()); err != nil {
			t.Fatal(err)
		}
		edits++
	}
	// Let the dispatcher drain what is left, then stop it.
	deadline := time.Now().Add(15 * time.Second)
	for len(f.sent()) < n && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	close(stop)
	<-done
	if v := violations.Load(); v != 0 {
		t.Fatalf("%d message(s) were sent while their edit hold was live", v)
	}
	sent := f.sent()
	if len(sent) != n {
		t.Fatalf("sent %d messages, want %d (each once): %q", len(sent), n, sent)
	}
	edited := 0
	for _, b := range sent {
		switch {
		case strings.HasPrefix(b, "edited ") && len(b) == len("edited a"):
			edited++
		case strings.HasPrefix(b, "original ") && len(b) == len("original a"):
		default:
			t.Fatalf("sent %q: neither a whole original nor a whole edit", b)
		}
	}
	if edited != edits || edits == 0 {
		t.Fatalf("%d edited texts sent for %d saved edits; want every saved edit sent edited, and at least one", edited, edits)
	}
	t.Logf("%d messages: %d held, edited and sent edited; %d claimed by the dispatcher before the hold", n, edits, n-edits)
}

// TestALapsedHoldIsClearedByTheDispatchersPass: once a hold has run out, the
// dispatcher's next pass takes it off the row (held_until and hold_token back
// to NULL) even while the window never goes idle — so a row keeps hold
// columns only while its hold is LIVE, which is what bounds what an older
// image (whose statements trip 0040's send_queue_hold_check on a held row)
// can meet during a rollout or a rollback. The row still says its edit ran out.
// The control is a live hold on another row, which the same pass leaves alone.
func TestALapsedHoldIsClearedByTheDispatchersPass(t *testing.T) {
	s, pool := pgStore(t)
	f, c := &fakeTarget{}, &clock{t: time.Now()}
	f.set(AgentState{Why: "Claude Code is working."})
	d := newDispatcher(s, f, c)
	live := mustEnqueue(t, s, 2, mirrorItem("held for ten minutes"))
	if _, err := s.Hold(as(2), live.ID); err != nil {
		t.Fatal(err)
	}
	shortHold(s, 50*time.Millisecond)
	lapsing := mustEnqueue(t, s, 2, mirrorItem("held briefly"))
	if _, err := s.Hold(as(2), lapsing.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	tick(t, d)
	held := func(id int64) bool {
		var h bool
		if err := pool.QueryRow(authzAll(), `SELECT held_until IS NOT NULL OR hold_token IS NOT NULL FROM send_queue WHERE id = $1`, id).Scan(&h); err != nil {
			t.Fatal(err)
		}
		return h
	}
	if held(lapsing.ID) {
		t.Fatal("a lapsed hold is still on the row after the dispatcher's pass")
	}
	if got, _ := s.Get(as(2), lapsing.ID); !got.HoldLapsed || got.Held || got.State != Queued || got.Body != "held briefly" {
		t.Fatalf("the cleared row: lapsed %t held %t state %s body %q; want it still saying the edit ran out, queued, unchanged",
			got.HoldLapsed, got.Held, got.State, got.Body)
	}
	if !held(live.ID) {
		t.Fatal("CONTROL: the pass cleared a LIVE hold")
	}
	wantSent(t, f, "while busy")
}
