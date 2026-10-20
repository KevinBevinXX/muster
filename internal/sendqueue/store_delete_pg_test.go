package sendqueue

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestDeleteRemovesOnlyAFinishedMessageOfTheCaller: Delete removes the
// caller's sent, failed and cancelled messages; it refuses a queued one and
// one being sent (ErrNotDeletable, the row unchanged), and another user's
// finished message is not found and stays.
func TestDeleteRemovesOnlyAFinishedMessageOfTheCaller(t *testing.T) {
	s, _ := pgStore(t)
	ctx := context.Background()
	finish := func(body string, o Outcome) Item {
		it := mustEnqueue(t, s, 2, mirrorItem(body))
		if _, ok, err := s.Claim(ctx, it.ID, time.Now(), claimFor("t", time.Now())); err != nil || !ok {
			t.Fatal(ok, err)
		}
		if err := s.Finish(ctx, it.ID, "t", o, time.Now()); err != nil {
			t.Fatal(err)
		}
		return it
	}
	sent := finish("sent", Outcome{Sent: true, Detail: "Sent."})
	failed := finish("failed", Outcome{Detail: "Not sent."})
	cancelled := mustEnqueue(t, s, 2, mirrorItem("cancelled"))
	if _, err := s.Cancel(as(2), cancelled.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	sending := mustEnqueue(t, s, 2, mirrorItem("sending"))
	if _, ok, err := s.Claim(ctx, sending.ID, time.Now(), claimFor("live", time.Now())); err != nil || !ok {
		t.Fatal(ok, err)
	}
	queued := mustEnqueue(t, s, 2, mirrorItem("queued"))
	theirs := mustEnqueue(t, s, 3, mirrorItem("user 3's"))
	if _, err := s.Cancel(as(3), theirs.ID, time.Now()); err != nil {
		t.Fatal(err)
	}

	if err := s.Delete(as(2), theirs.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("user 2 deletes user 3's message: %v, want ErrNotFound", err)
	}
	if got, err := s.Get(as(3), theirs.ID); err != nil || got.State != Cancelled {
		t.Fatalf("user 3's message after user 2's Delete: %+v %v", got, err)
	}
	for _, it := range []Item{queued, sending} {
		before, _ := s.Get(as(2), it.ID)
		if err := s.Delete(as(2), it.ID); !errors.Is(err, ErrNotDeletable) {
			t.Fatalf("Delete of %q (%s): %v, want ErrNotDeletable", it.Body, before.State, err)
		}
		if after, err := s.Get(as(2), it.ID); err != nil || after.State != before.State {
			t.Fatalf("a refused Delete moved %q: %s -> %+v %v", it.Body, before.State, after.State, err)
		}
	}
	for _, it := range []Item{sent, failed, cancelled} {
		if err := s.Delete(as(2), it.ID); err != nil {
			t.Fatalf("Delete of %q: %v", it.Body, err)
		}
		if _, err := s.Get(as(2), it.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%q is still there after its Delete: %v", it.Body, err)
		}
	}
	if err := s.Delete(as(2), sent.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a second Delete: %v, want ErrNotFound", err)
	}
}

// TestFinishedIsExactlySentFailedCancelled pins the one list: every state
// the schema allows (0019's send_queue_state_check), and an unknown one.
func TestFinishedIsExactlySentFailedCancelled(t *testing.T) {
	for st, want := range map[State]bool{Queued: false, Sending: false, Sent: true, Failed: true, Cancelled: true, "expired": false, "": false} {
		if got := st.Finished(); got != want {
			t.Errorf("%q.Finished() = %v, want %v", st, got, want)
		}
	}
}

// TestDeleteCarriesATurnOnlyToTheOwnersOpenRowsOfTheTarget: deleting a sent
// message raises the after_since of the same owner's open rows for the same
// target to its own; another target's rows, and another owner's rows for the
// same address, are untouched; a failed message that never reached the host
// carries nothing.
func TestDeleteCarriesATurnOnlyToTheOwnersOpenRowsOfTheTarget(t *testing.T) {
	s, _ := pgStore(t)
	ctx := context.Background()
	sent := mustEnqueue(t, s, 2, mirrorItem("sent"))
	if _, ok, err := s.Claim(ctx, sent.ID, time.Now(), Claim{By: "t", Lease: time.Minute, Since: 5000}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := s.Finish(ctx, sent.ID, "t", Outcome{Sent: true}, time.Now()); err != nil {
		t.Fatal(err)
	}
	next := mustEnqueue(t, s, 2, mirrorItem("next"))
	elsewhere := mirrorItem("another window")
	elsewhere.Target.Window = "@8"
	other := mustEnqueue(t, s, 2, elsewhere)
	stranger := mustEnqueue(t, s, 3, mirrorItem("user 3's"))
	if err := s.Delete(as(2), sent.ID); err != nil {
		t.Fatal(err)
	}
	after := func(uid, id int64) int64 {
		it, err := s.Get(as(uid), id)
		if err != nil {
			t.Fatal(err)
		}
		return it.AfterSince
	}
	if got := after(2, next.ID); got != 5000 {
		t.Fatalf("the next message waits behind %d; want the deleted one's 5000", got)
	}
	if got := after(2, other.ID); got != 0 {
		t.Fatalf("another window's message was moved to %d", got)
	}
	if got := after(3, stranger.ID); got != 0 {
		t.Fatalf("another owner's message was moved to %d", got)
	}
	// A failed message that never reached the host is no turn.
	failed := mustEnqueue(t, s, 2, mirrorItem("never reached"))
	if _, err := s.pool.Exec(sys(ctx), `UPDATE send_queue SET state = 'failed', finished_at = now(), claimed_at = now(),
		after_since = 9000, may_have_delivered = false WHERE id = $1`, failed.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(as(2), failed.ID); err != nil {
		t.Fatal(err)
	}
	if got := after(2, next.ID); got != 5000 {
		t.Fatalf("a failed, never-delivered message carried its turn: %d", got)
	}
}
