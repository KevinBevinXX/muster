package askq

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// The audit's follow-ups (PR #130): a question is answered only while its hook
// is still waiting (scenarios A, B, C), one owner cannot fill the store, and
// the store's expiry runs on its own.

// TestAnAnswerAfterTheHookStoppedIsRefused (audit scenario B — the hook was
// SIGKILLed, or lost muster): with no poll for PollGrace the question is
// withdrawn as STOPPED and an answer is refused, so muster never says "sent to
// Claude" for answers nobody will collect. The control: within the grace the
// same answer is taken.
func TestAnAnswerAfterTheHookStoppedIsRefused(t *testing.T) {
	for _, c := range []struct {
		silent time.Duration
		taken  bool
	}{{PollGrace - time.Second, true}, {PollGrace + time.Second, false}} {
		s, clk := testStore(t)
		if _, err := s.Open(key(1), twoQs, time.Hour, ""); err != nil {
			t.Fatal(err)
		}
		clk.add(c.silent)
		e, err := s.Answer(key(1), someAnswers)
		if c.taken {
			if err != nil || e.State != Answered {
				t.Fatalf("silent %s: %+v %v, want answered", c.silent, e, err)
			}
			continue
		}
		if !errors.Is(err, ErrNotPending) || e.State != Withdrawn || e.Reason != ReasonStopped {
			t.Fatalf("silent %s: %+v %v, want refused as stopped", c.silent, e, err)
		}
	}
}

func eventually(t *testing.T, ok func() bool) {
	t.Helper()
	for i := 0; i < 1000; i++ {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition never held")
}

// TestAPollInFlightKeepsTheQuestionAlive: a long-poll that outlasts PollGrace
// is a hook still waiting; a poll whose connection closed is not.
func TestAPollInFlightKeepsTheQuestionAlive(t *testing.T) {
	s, clk := testStore(t)
	e, _ := s.Open(key(1), twoQs, time.Hour, "")
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = s.Poll(context.Background(), e.ID, 1, "mirror", "laptop", 10*time.Second)
	}()
	eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.byID[e.ID].polling == 1 })
	clk.add(2 * PollGrace)
	if got, err := s.Answer(key(1), someAnswers); err != nil || got.State != Answered {
		t.Fatalf("an answer while a poll is in flight: %+v %v", got, err)
	}
	<-done
	// An abandoned poll (its context ended) is no sign of life.
	s2, clk2 := testStore(t)
	e2, _ := s2.Open(key(1), twoQs, time.Hour, "")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		eventually(t, func() bool { s2.mu.Lock(); defer s2.mu.Unlock(); return s2.byID[e2.ID].polling == 1 })
		clk2.add(PollGrace - time.Second)
		cancel()
	}()
	_, _ = s2.Poll(ctx, e2.ID, 1, "mirror", "laptop", 10*time.Second)
	clk2.add(2 * time.Second)
	if _, err := s2.Answer(key(1), someAnswers); !errors.Is(err, ErrNotPending) {
		t.Fatalf("an abandoned poll kept the question alive: %v", err)
	}
}

// TestAWithdrawReturnsAnAnswerThatArrivedFirst (audit scenario A, the
// server's half): the hook's give-up reads what its withdraw returns, so
// answers given just before are still delivered — and the withdraw does not
// undo them.
func TestAWithdrawReturnsAnAnswerThatArrivedFirst(t *testing.T) {
	s, _ := testStore(t)
	e, _ := s.Open(key(1), twoQs, time.Hour, "")
	if _, err := s.Answer(key(1), someAnswers); err != nil {
		t.Fatal(err)
	}
	got, err := s.Withdraw(e.ID, 1, "mirror", "laptop")
	if err != nil || got.State != Answered || got.Answers["Which pets?"] != "Cat, Dog" {
		t.Fatalf("withdraw after an answer: %+v %v", got, err)
	}
}

// TestTheHooksOwnIDIsIdempotentAndWithdrawable (audit scenario C, the
// server's half): the id the hook chose names the question — a retried
// registration is the same entry, another call cannot take the id, and the
// hook can withdraw it without ever having read the registration's answer.
func TestTheHooksOwnIDIsIdempotentAndWithdrawable(t *testing.T) {
	s, _ := testStore(t)
	id := NewID()
	a, err := s.Open(key(1), twoQs, time.Hour, id)
	if err != nil || a.ID != id {
		t.Fatalf("open: %+v %v", a, err)
	}
	if b, err := s.Open(key(1), twoQs, time.Hour, id); err != nil || b.ID != id {
		t.Fatalf("retry: %+v %v", b, err)
	}
	other := key(1)
	other.ToolUseID = "toolu_other"
	if _, err := s.Open(other, twoQs, time.Hour, id); !errors.Is(err, ErrIDTaken) {
		t.Fatalf("another call took the id: %v", err)
	}
	if _, err := s.Open(key(1), twoQs, time.Hour, "NOT-HEX"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a malformed id: %v", err)
	}
	if got, err := s.Withdraw(id, 1, "mirror", "laptop"); err != nil || got.State != Withdrawn || got.Reason != ReasonGaveUp {
		t.Fatalf("withdraw by the hook's id: %+v %v", got, err)
	}
}

// TestOneOwnerCannotFillTheStore (audit 🟡2): owner 1 asking and answering
// past every bound never makes the store refuse owner 2, and owner 1 holds at
// most MaxEntriesPerOwner entries.
func TestOneOwnerCannotFillTheStore(t *testing.T) {
	s, _ := testStore(t)
	for i := 0; i < MaxEntries+500; i++ {
		k := key(1)
		k.ToolUseID = fmt.Sprintf("toolu_%d", i)
		if _, err := s.Open(k, twoQs, time.Hour, ""); err != nil {
			t.Fatalf("owner 1's question %d: %v", i, err)
		}
		if _, err := s.Answer(k, someAnswers); err != nil {
			t.Fatal(err)
		}
		if i%499 == 0 || i == MaxEntries+499 {
			k2 := key(2)
			k2.ToolUseID = fmt.Sprintf("toolu_two_%d", i)
			if _, err := s.Open(k2, twoQs, time.Hour, ""); err != nil {
				t.Fatalf("after owner 1's %d questions, owner 2 is refused: %v", i+1, err)
			}
		}
	}
	s.mu.Lock()
	n := 0
	for _, e := range s.byID {
		if e.Key.Owner == 1 {
			n++
		}
	}
	s.mu.Unlock()
	if n > MaxEntriesPerOwner {
		t.Fatalf("owner 1 holds %d entries, past MaxEntriesPerOwner %d", n, MaxEntriesPerOwner)
	}
}

// TestTheGlobalBoundEvictsResolvedNotPending: many owners filling the store
// with resolved entries make room for a new question; a store of PENDING
// questions only is refused (they are never evicted).
func TestTheGlobalBoundEvictsResolvedNotPending(t *testing.T) {
	s, _ := testStore(t)
	for i := 0; i < MaxEntries; i++ {
		k := key(int64(100 + i%200))
		k.ToolUseID = fmt.Sprintf("toolu_%d", i)
		if _, err := s.Open(k, twoQs, time.Hour, ""); err != nil {
			t.Fatalf("question %d: %v", i, err)
		}
		_, _ = s.Answer(k, someAnswers)
	}
	if _, err := s.Open(key(1), twoQs, time.Hour, ""); err != nil {
		t.Fatalf("a full store of resolved questions refused a new one: %v", err)
	}
}

// TestTheSweepFiresOnExpiry (audit 🟢3): a question whose hook died is
// withdrawn by the periodic sweep — with no read asking — and the change
// callback hears it (which is what drops the window's "waiting").
func TestTheSweepFiresOnExpiry(t *testing.T) {
	s, clk := testStore(t)
	var mu sync.Mutex
	var seen []Entry
	s.OnChange(func(e Entry) { mu.Lock(); seen = append(seen, e); mu.Unlock() })
	if _, err := s.Open(key(1), twoQs, time.Hour, ""); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { s.RunSweep(ctx, 5*time.Millisecond); close(stopped) }()
	clk.add(PollGrace + time.Second)
	eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seen) == 2 && seen[1].State == Withdrawn && seen[1].Reason == ReasonStopped
	})
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("RunSweep did not stop with its context")
	}
}
