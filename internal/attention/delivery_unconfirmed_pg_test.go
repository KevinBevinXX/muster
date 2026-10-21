package attention

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestAReadUnconfirmedAnswerCannotBePushedAgain (#152 r2): a delivery that gave
// up (settled unconfirmed — after a day queued, or a failed turn) and whose
// asker THEN read the answer through attention_wait is the asker's: MarkWaited
// takes it to waited, so the owner's Retry cannot queue it and the sweep never
// pushes a second copy of an answer the agent already has.
func TestAReadUnconfirmedAnswerCannotBePushedAgain(t *testing.T) {
	s, pool, _ := pgStore(t)
	agent := agentFor(t, pool, 2)
	a := answered(t, s, 2, agent, "left")
	if _, ok, err := s.ClaimDelivery(as(2), a.ID, "p1", time.Minute, 0); err != nil || !ok {
		t.Fatal(err)
	}
	if held, err := s.FinishDelivery(as(2), a.ID, "p1", false, 0, "not delivered within 24h0m0s: the agent is stopped"); err != nil || !held {
		t.Fatal(err)
	}
	if got, _ := s.Get(as(2), a.ID); got.Delivery != DeliveryUnconfirmed {
		t.Fatalf("after the give-up: %q; want unconfirmed", got.Delivery)
	}
	if ok, err := s.MarkWaited(as(2), a.ID); err != nil || !ok {
		t.Fatalf("the asker's read of an unconfirmed answer was not recorded: %v %v", ok, err)
	}
	if got, _ := s.Get(as(2), a.ID); got.Delivery != DeliveryWaited || got.DeliveryDetail != "" {
		t.Fatalf("after the read: %q %q; want waited, no detail", got.Delivery, got.DeliveryDetail)
	}
	if _, err := s.RetryDelivery(as(2), a.ID); !errors.Is(err, ErrNotRetryable) {
		t.Fatalf("an answer its asker read was retried: %v", err)
	}
	due, err := s.DueDeliveries(context.Background(), 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range due {
		if d.ID == a.ID {
			t.Fatal("the sweep would push an answer its asker already read")
		}
	}
}
