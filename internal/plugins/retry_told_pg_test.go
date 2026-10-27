package plugins

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

// TestARetriedDeliveryTellsTheChief (#138 follow-up 3): the turn that delivers
// a message the owner retried carries the retry sentence (its count read from
// the plugin.retried audit rows Requeue writes); the cut-off first turn and a
// fresh message's turn do not.
func TestARetriedDeliveryTellsTheChief(t *testing.T) {
	s, _, gw, d, p, target := deliverFixture(t)
	m, _, _ := s.Submit(plugin(2), p.ID, msg("rt-1"))
	gw.err = context.DeadlineExceeded
	if r := d.Deliver(plugin(2), p.ID, m.ID, target); r.Outcome != OutcomeUnconfirmed {
		t.Fatalf("setup: %+v", r)
	}
	gw.err = nil
	if _, err := s.Requeue(session(2), m.ID); err != nil {
		t.Fatal(err)
	}
	if r := d.Deliver(plugin(2), p.ID, m.ID, target); r.Outcome != OutcomeDelivered {
		t.Fatalf("the retried delivery: %+v", r)
	}
	fresh, _, _ := s.Submit(plugin(2), p.ID, msg("rt-2"))
	if r := d.Deliver(plugin(2), p.ID, fresh.ID, target); r.Outcome != OutcomeDelivered {
		t.Fatalf("a fresh delivery: %+v", r)
	}
	if len(gw.sent) != 3 {
		t.Fatalf("%d turns; want 3", len(gw.sent))
	}
	first := func(turn string) string { return strings.SplitN(turn, "\n", 2)[0] }
	if !strings.Contains(first(gw.sent[1]), "Retry: your user retried this message (retry 1, delivery attempt 2)") ||
		!strings.Contains(first(gw.sent[1]), "message_id "+strconv.FormatInt(m.ID, 10)+" before acting again.") {
		t.Errorf("the retried turn's preface does not say it is a retry:\n%s", first(gw.sent[1]))
	}
	for _, i := range []int{0, 2} {
		if strings.Contains(gw.sent[i], "Retry:") {
			t.Errorf("turn %d (not a retry) carries a retry sentence:\n%s", i, first(gw.sent[i]))
		}
	}
}
