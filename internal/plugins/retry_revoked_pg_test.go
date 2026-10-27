package plugins

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestARevokedPluginsMessageIsNeitherRetriedNorDelivered (#138 follow-up 5):
// revoking a plugin stops what it sent from reaching the target. Submit and
// QueuedForOwner already refused; Retry (the list and the requeue) and a
// Deliver that names a message directly did not. An unconfirmed message of a
// revoked plugin is not listed, its requeue is refused saying why, and a
// queued one is not handed to the target — nothing is sent, and the message is
// left as it was.
func TestARevokedPluginsMessageIsNeitherRetriedNorDelivered(t *testing.T) {
	s, _, gw, d, p, target := deliverFixture(t)
	cut, _, _ := s.Submit(plugin(2), p.ID, msg("rv-cut"))
	gw.err = context.DeadlineExceeded // the turn was cut off: unconfirmed
	if r := d.Deliver(plugin(2), p.ID, cut.ID, target); r.Outcome != OutcomeUnconfirmed {
		t.Fatalf("setup: %+v; want unconfirmed", r)
	}
	gw.err = nil
	queued, _, _ := s.Submit(plugin(2), p.ID, msg("rv-queued"))
	listed := func() bool {
		t.Helper()
		ms, err := s.Retryable(session(2), 50)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range ms {
			if m.ID == cut.ID {
				return true
			}
		}
		return false
	}
	if !listed() {
		t.Fatal("CONTROL: the unconfirmed message of a live plugin is not retryable")
	}

	if found, err := s.Revoke(session(2), p.ID); err != nil || !found {
		t.Fatalf("revoke: %v %v", found, err)
	}
	if listed() {
		t.Error("a revoked plugin's unconfirmed message is listed with a Retry button")
	}
	if _, err := s.Requeue(session(2), cut.ID); !errors.Is(err, ErrNotRetryable) || !strings.Contains(err.Error(), "its plugin has been revoked") {
		t.Errorf("requeue: %v; want ErrNotRetryable saying the plugin was revoked", err)
	}
	if got, _ := s.Message(plugin(2), cut.ID); got.Status != StatusUnconfirmed {
		t.Errorf("the refused requeue moved the message to %s", got.Status)
	}

	sent := len(gw.sent)
	before, _ := s.Message(plugin(2), queued.ID)
	if r := d.Deliver(plugin(2), p.ID, queued.ID, target); r.Outcome != OutcomePending || r.Reason != "plugin_revoked" {
		t.Errorf("deliver: %+v; want pending/plugin_revoked", r)
	}
	if len(gw.sent) != sent {
		t.Errorf("a revoked plugin's message was sent to the target (%d turns, want %d)", len(gw.sent), sent)
	}
	if got, _ := s.Message(plugin(2), queued.ID); got.Status != StatusQueued || got.Attempts != before.Attempts {
		t.Errorf("the queued message became %s with %d attempts; want queued, %d", got.Status, got.Attempts, before.Attempts)
	}
}
