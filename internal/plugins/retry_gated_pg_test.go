package plugins

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestAMessageAGatedTaskAlreadyCitesIsNotRetried (#138 follow-up 2): the cut-off
// turn may already have run task_create_gated citing the message — recorded
// only as the ActionGatedTask audit row carrying its message_id. A retry would
// hand the chief the message again and it would file a SECOND gated task. So a
// message a gated task cites is not listed and its requeue is refused, saying
// why. A gated task citing a DIFFERENT message, or none, does not count (control).
func TestAMessageAGatedTaskAlreadyCitesIsNotRetried(t *testing.T) {
	s, _, gw, d, p, chief := deliverFixture(t)
	cut := func(ext string) Message {
		t.Helper()
		m, _, _ := s.Submit(plugin(2), p.ID, msg(ext))
		gw.err = context.DeadlineExceeded
		defer func() { gw.err = nil }()
		if r := d.Deliver(plugin(2), p.ID, m.ID, chief); r.Outcome != OutcomeUnconfirmed {
			t.Fatalf("setup %s: %+v; want unconfirmed", ext, r)
		}
		return m
	}
	free, cited := cut("gt-free"), cut("gt-cited")
	listed := func(id int64) bool {
		t.Helper()
		ms, err := s.Retryable(session(2), 50)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range ms {
			if m.ID == id {
				return true
			}
		}
		return false
	}
	// The chief files a gated task citing CITED (under the owner's principal,
	// as its tool call runs) — and one citing no message at all.
	if err := s.ReserveGatedTask(plugin(2), chief.ID, &cited.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ReserveGatedTask(plugin(2), chief.ID, nil); err != nil {
		t.Fatal(err)
	}
	if !listed(free.ID) {
		t.Fatal("CONTROL: a message no gated task cites is not retryable (a task citing another message, or none, counted)")
	}
	if listed(cited.ID) {
		t.Error("a message a gated task cites is listed with a Retry button")
	}
	if _, err := s.Requeue(session(2), cited.ID); !errors.Is(err, ErrNotRetryable) || !strings.Contains(err.Error(), "already filed a gated task citing it") {
		t.Errorf("requeue: %v; want ErrNotRetryable saying a gated task cites it", err)
	}
	if got, _ := s.Message(plugin(2), cited.ID); got.Status != StatusUnconfirmed {
		t.Errorf("the refused requeue moved the message to %s", got.Status)
	}
	// And the control message is still retryable for real.
	if _, err := s.Requeue(session(2), free.ID); err != nil {
		t.Errorf("requeue of the uncited message: %v", err)
	}
}
