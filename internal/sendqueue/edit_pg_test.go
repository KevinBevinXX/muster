package sendqueue

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// EDITING A QUEUED MESSAGE: the hold's state machine (Hold → SaveEdit, Hold →
// ReleaseEdit, Hold → expiry), its refusals, and the dispatcher's claim
// skipping a held row — including the race between a claim and a hold.

// shortHold is a store whose edit hold lasts holdFor (on Postgres's clock).
func shortHold(s *PGStore, holdFor time.Duration) *PGStore { s.hold = holdFor; return s }

// TestAnEditIsSavedInPlace: a queued message is held for an edit (Held, a
// token, the original text still stored), Save replaces its text under that
// hold and releases it, and the row keeps its id, its queued time and its place
// ahead of the message queued after it. The edit's sign-in is stored.
func TestAnEditIsSavedInPlace(t *testing.T) {
	s, _ := pgStore(t)
	a := mustEnqueue(t, s, 2, mirrorItem("first, as typed"))
	b := mustEnqueue(t, s, 2, mirrorItem("second"))
	h, err := s.Hold(as(2), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !h.Held || h.HoldLapsed || len(h.HoldToken) != 32 || h.Body != "first, as typed" {
		t.Fatalf("held row = held %t lapsed %t token %q body %q; want held, a 32-char token, the original text",
			h.Held, h.HoldLapsed, h.HoldToken, h.Body)
	}
	if d := time.Until(h.HeldUntil); d < EditHold-time.Minute || d > EditHold+time.Minute {
		t.Fatalf("held until %s from now; want about %s", d, EditHold)
	}
	signedIn := time.Now().Add(-time.Hour).Truncate(time.Microsecond)
	saved, err := s.SaveEdit(as(2), a.ID, h.HoldToken, "first, as edited", signedIn)
	if err != nil {
		t.Fatal(err)
	}
	if saved.ID != a.ID || saved.Body != "first, as edited" || saved.Held || saved.HoldToken != "" || !saved.HeldUntil.IsZero() {
		t.Fatalf("saved row = %+v; want the same id, the new text, no hold", saved)
	}
	if !saved.CreatedAt.Equal(a.CreatedAt) || !saved.EditStepUpAt.Equal(signedIn) || saved.EditedAt.IsZero() || !saved.StepUpAt.Equal(a.StepUpAt) {
		t.Fatalf("saved row: created %s (was %s), edit sign-in %s (want %s), edited %s, queue sign-in %s (was %s)",
			saved.CreatedAt, a.CreatedAt, saved.EditStepUpAt, signedIn, saved.EditedAt, saved.StepUpAt, a.StepUpAt)
	}
	items, _ := s.List(as(2), win)
	if got := bodies(items); len(got) != 2 || got[0] != "queued:first, as edited" || got[1] != "queued:second" || items[1].ID != b.ID {
		t.Fatalf("the queue after the edit = %v; want the edited message still first", got)
	}
}

// TestCancellingAnEditLeavesTheTextAndReleasesTheHold: Cancel (ReleaseEdit)
// with the live token releases the hold and changes nothing else; a second
// release finds nothing to release.
func TestCancellingAnEditLeavesTheTextAndReleasesTheHold(t *testing.T) {
	s, _ := pgStore(t)
	a := mustEnqueue(t, s, 2, mirrorItem("leave me be"))
	h, err := s.Hold(as(2), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	it, released, err := s.ReleaseEdit(as(2), a.ID, h.HoldToken)
	if err != nil || !released {
		t.Fatalf("release: %v, %v", released, err)
	}
	if it.Held || it.HoldLapsed || it.HoldToken != "" || it.Body != "leave me be" || !it.EditedAt.IsZero() {
		t.Fatalf("released row = %+v; want no hold, the same text, not edited", it)
	}
	if _, released, err := s.ReleaseEdit(as(2), a.ID, h.HoldToken); err != nil || released {
		t.Fatalf("a second release: %v, %v; want nothing released", released, err)
	}
	// Its Save is refused too: the hold is gone.
	if _, err := s.SaveEdit(as(2), a.ID, h.HoldToken, "too late", time.Now()); !errors.Is(err, ErrHoldLapsed) {
		t.Fatalf("Save after Cancel: %v, want ErrHoldLapsed", err)
	}
}

// TestAnAbandonedEditRunsOutAndTheRowRevertsWithItsOriginalText: past its
// hold the row is queued again (not Held, HoldLapsed), with the text it had; a
// Save under the lapsed hold is refused and writes nothing; the dispatcher can
// claim it again, and the claim clears the lapsed hold.
func TestAnAbandonedEditRunsOutAndTheRowRevertsWithItsOriginalText(t *testing.T) {
	s, _ := pgStore(t)
	shortHold(s, 50*time.Millisecond)
	a := mustEnqueue(t, s, 2, mirrorItem("original"))
	h, err := s.Hold(as(2), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	// CONTROL: held, the claim is refused.
	if _, ok, err := s.Claim(context.Background(), a.ID, time.Now(), claimFor("pod-a", time.Now())); err != nil || ok {
		t.Fatalf("CONTROL: a claim while held: %v, %v; want refused", ok, err)
	}
	time.Sleep(150 * time.Millisecond)
	got, err := s.Get(as(2), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Held || !got.HoldLapsed || got.State != Queued || got.Body != "original" {
		t.Fatalf("after the hold ran out: held %t lapsed %t state %s body %q; want lapsed, queued, the original",
			got.Held, got.HoldLapsed, got.State, got.Body)
	}
	if _, err := s.SaveEdit(as(2), a.ID, h.HoldToken, "edited too late", time.Now()); !errors.Is(err, ErrHoldLapsed) {
		t.Fatalf("Save after the hold ran out: %v, want ErrHoldLapsed", err)
	}
	if got, _ := s.Get(as(2), a.ID); got.Body != "original" {
		t.Fatalf("a refused Save wrote %q", got.Body)
	}
	c, ok, err := s.Claim(context.Background(), a.ID, time.Now(), claimFor("pod-a", time.Now()))
	if err != nil || !ok {
		t.Fatalf("a claim after the hold ran out: %v, %v; want claimed", ok, err)
	}
	if c.Body != "original" || c.HoldLapsed || c.HoldToken != "" {
		t.Fatalf("claimed row = body %q lapsed %t token %q; want the original, the hold cleared", c.Body, c.HoldLapsed, c.HoldToken)
	}
}

// TestASaveAfterTheHoldRanOutAndTheMessageWasSentIsRefused: the hold runs out,
// the dispatcher sends the message with its original text, and the late Save
// is ErrEditTooLate with the row as it is (sent), its text unchanged.
func TestASaveAfterTheHoldRanOutAndTheMessageWasSentIsRefused(t *testing.T) {
	s, _ := pgStore(t)
	shortHold(s, 50*time.Millisecond)
	a := mustEnqueue(t, s, 2, mirrorItem("sent as it was"))
	h, err := s.Hold(as(2), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if _, ok, err := s.Claim(context.Background(), a.ID, time.Now(), claimFor("pod-a", time.Now())); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := s.Finish(context.Background(), a.ID, "pod-a", Outcome{Sent: true, Detail: "Sent."}, time.Now()); err != nil {
		t.Fatal(err)
	}
	cur, err := s.SaveEdit(as(2), a.ID, h.HoldToken, "the edit", time.Now())
	if !errors.Is(err, ErrEditTooLate) || cur.State != Sent || cur.Body != "sent as it was" {
		t.Fatalf("Save after it was sent: %v, state %s body %q; want ErrEditTooLate, sent, the original", err, cur.State, cur.Body)
	}
}

// TestEditIsRefusedOnARowBeingSentFinishedOrRouted: Edit on a row being sent
// is ErrEditSending, on a finished one ErrNotEditable, on a routed one
// ErrRoutedNotEditable — each an ErrNotEditable — and on another user's,
// ErrNotFound. None holds the row.
func TestEditIsRefusedOnARowBeingSentFinishedOrRouted(t *testing.T) {
	s, pool := pgStore(t)
	sending := mustEnqueue(t, s, 2, mirrorItem("being sent"))
	if _, ok, err := s.Claim(context.Background(), sending.ID, time.Now(), claimFor("pod-a", time.Now())); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if cur, err := s.Hold(as(2), sending.ID); !errors.Is(err, ErrEditSending) || !errors.Is(err, ErrNotEditable) || cur.Held {
		t.Fatalf("Edit while sending: %v (held %t); want ErrEditSending", err, cur.Held)
	}
	cancelled := mustEnqueue(t, s, 2, mirrorItem("cancelled"))
	if _, err := s.Cancel(as(2), cancelled.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Hold(as(2), cancelled.ID); !errors.Is(err, ErrNotEditable) || errors.Is(err, ErrEditSending) {
		t.Fatalf("Edit of a cancelled message: %v; want ErrNotEditable", err)
	}
	routed := mustEnqueue(t, s, 2, mirrorItem("the chief's words"))
	if _, err := pool.Exec(authzAll(), `UPDATE send_queue SET route_id = 'rt_000000000000000000000001' WHERE id = $1`, routed.ID); err != nil {
		t.Fatal(err)
	}
	if cur, err := s.Hold(as(2), routed.ID); !errors.Is(err, ErrRoutedNotEditable) || cur.Held {
		t.Fatalf("Edit of a routed message: %v (held %t); want ErrRoutedNotEditable", err, cur.Held)
	}
	mine := mustEnqueue(t, s, 2, mirrorItem("user 2's"))
	if _, err := s.Hold(as(3), mine.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("user 3 edits user 2's message: %v, want ErrNotFound", err)
	}
	if got, _ := s.Get(as(2), mine.ID); got.Held {
		t.Fatal("another user's Edit held the row")
	}
	// CONTROL: the owner's own Edit of the same row holds it.
	if got, err := s.Hold(as(2), mine.ID); err != nil || !got.Held {
		t.Fatalf("CONTROL: the owner's Edit: %v, held %t", err, got.Held)
	}
}

// TestASecondEditTakesTheRowOver: a second Edit (another device) holds the row
// under a new token; the first editor's Save is then refused, and the second's
// is saved.
func TestASecondEditTakesTheRowOver(t *testing.T) {
	s, _ := pgStore(t)
	a := mustEnqueue(t, s, 2, mirrorItem("contested"))
	first, err := s.Hold(as(2), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Hold(as(2), a.ID)
	if err != nil || second.HoldToken == first.HoldToken {
		t.Fatalf("second Edit: %v, token %q (first %q)", err, second.HoldToken, first.HoldToken)
	}
	if _, err := s.SaveEdit(as(2), a.ID, first.HoldToken, "from the first device", time.Now()); !errors.Is(err, ErrHoldLapsed) {
		t.Fatalf("the first device's Save: %v, want ErrHoldLapsed", err)
	}
	if got, err := s.SaveEdit(as(2), a.ID, second.HoldToken, "from the second device", time.Now()); err != nil || got.Body != "from the second device" {
		t.Fatalf("the second device's Save: %v, %q", err, got.Body)
	}
}

// TestTheClaimSkipsAHeldRow: a held row is not claimed and is its target's
// head, Held (so Decide holds the whole window behind it); released, it is
// claimed. This is the SQL guard on its own: Claim is called directly, with no
// Decide in front of it.
func TestTheClaimSkipsAHeldRow(t *testing.T) {
	s, _ := pgStore(t)
	a := mustEnqueue(t, s, 2, mirrorItem("held"))
	mustEnqueue(t, s, 2, mirrorItem("behind it"))
	h, err := s.Hold(as(2), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.Claim(context.Background(), a.ID, time.Now(), claimFor("pod-a", time.Now())); err != nil || ok {
		t.Fatalf("a claim of a held row: %v, %v; want refused", ok, err)
	}
	heads, err := s.Heads(context.Background())
	if err != nil || len(heads) != 1 || heads[0].ID != a.ID || !heads[0].Held {
		t.Fatalf("heads = %+v, %v; want the held row, Held", heads, err)
	}
	if d := Decide(heads[0], idleSince(time.Now().Add(-time.Hour)), time.Now(), idle); d.Send || d.Why != HeldWhy {
		t.Fatalf("Decide on a held head = %+v; want held, saying why", d)
	}
	if _, released, err := s.ReleaseEdit(as(2), a.ID, h.HoldToken); err != nil || !released {
		t.Fatal(released, err)
	}
	if c, ok, err := s.Claim(context.Background(), a.ID, time.Now(), claimFor("pod-a", time.Now())); err != nil || !ok || c.Body != "held" {
		t.Fatalf("CONTROL: a claim after the release: %v, %v, %q", ok, err, c.Body)
	}
}

// TestAHoldAndAClaimRaceAndExactlyOneWins: a hold and a claim of the same row
// are released together, many times. Exactly one wins each time: a claimed row
// was not held (the hold was refused: ErrEditSending), and a held row was not
// claimed. Then, under a live hold, a claimer hammers the row while Save
// replaces its text: the claim never succeeds while the hold is live, and what
// it finally claims is the WHOLE new text, never the old one after a Save or a
// mix.
func TestAHoldAndAClaimRaceAndExactlyOneWins(t *testing.T) {
	s, _ := pgStore(t)
	sysCtx := context.Background()
	holds, claims := 0, 0
	for i := range 40 {
		it := mustEnqueue(t, s, 2, mirrorItem("race"))
		start := make(chan struct{})
		var wg sync.WaitGroup
		var held Item
		var holdErr error
		var claimed bool
		var claimErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			held, holdErr = s.Hold(as(2), it.ID)
		}()
		go func() {
			defer wg.Done()
			<-start
			_, claimed, claimErr = s.Claim(sysCtx, it.ID, time.Now(), claimFor("pod-a", time.Now()))
		}()
		close(start)
		wg.Wait()
		if claimErr != nil {
			t.Fatal(claimErr)
		}
		switch {
		case holdErr == nil && !claimed:
			holds++
			if !held.Held {
				t.Fatalf("round %d: the hold won but the row is not held", i)
			}
			if _, _, err := s.ReleaseEdit(as(2), it.ID, held.HoldToken); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Cancel(as(2), it.ID, time.Now()); err != nil {
				t.Fatal(err)
			}
		case claimed && errors.Is(holdErr, ErrEditSending):
			claims++
			if err := s.Finish(sysCtx, it.ID, "pod-a", Outcome{Detail: "test"}, time.Now()); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("round %d: hold %v (held %t) and claim %t both won or both lost", i, holdErr, held.Held, claimed)
		}
	}
	t.Logf("40 races: the hold won %d, the claim won %d", holds, claims)

	// Save under a live hold, with a claimer hammering the row.
	it := mustEnqueue(t, s, 2, mirrorItem("old text"))
	h, err := s.Hold(as(2), it.ID)
	if err != nil {
		t.Fatal(err)
	}
	var whileHeld atomic.Int32
	var holding atomic.Bool
	holding.Store(true)
	got := make(chan Item, 1)
	go func() {
		for {
			c, ok, err := s.Claim(sysCtx, it.ID, time.Now(), claimFor("pod-a", time.Now()))
			if err != nil {
				t.Error(err)
				got <- Item{}
				return
			}
			if ok {
				if holding.Load() {
					whileHeld.Add(1)
				}
				got <- c
				return
			}
		}
	}()
	time.Sleep(30 * time.Millisecond)
	// From here the claimer may win at any moment: the Save below releases the
	// hold in the same statement that writes the text.
	holding.Store(false)
	if _, err := s.SaveEdit(as(2), it.ID, h.HoldToken, "new text, whole", time.Now()); err != nil {
		t.Fatal(err)
	}
	c := <-got
	if whileHeld.Load() != 0 {
		t.Fatal("a row was claimed while its edit hold was live")
	}
	if c.Body != "new text, whole" {
		t.Fatalf("claimed %q after the Save; want the whole new text", c.Body)
	}
}

// authzAll is the system scope, for a test's direct writes.
func authzAll() context.Context { return sys(context.Background()) }
