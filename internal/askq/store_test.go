package askq

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func testStore(t *testing.T) (*Store, *clock) {
	t.Helper()
	c := &clock{t: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
	s := New()
	s.SetNow(c.now)
	return s, c
}

var twoQs = []Question{
	{Question: "Which color?", Options: []Option{{Label: "Red"}, {Label: "Blue"}}},
	{Question: "Which pets?", Options: []Option{{Label: "Cat"}, {Label: "Dog"}}, MultiSelect: true},
}

func key(owner int64) Key {
	return Key{Owner: owner, Source: "mirror", Scope: "laptop", Session: "s-1", ToolUseID: "toolu_1"}
}

var someAnswers = map[string]string{"Which color?": "Red", "Which pets?": "Cat, Dog"}

func TestOpenIsIdempotentWhilePending(t *testing.T) {
	s, _ := testStore(t)
	a, err := s.Open(key(1), twoQs, time.Minute, "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Open(key(1), twoQs, time.Minute, "")
	if err != nil || b.ID != a.ID {
		t.Fatalf("a retried Open is a new entry: %v %v", a.ID, b.ID)
	}
	if !s.Waiting(1, "mirror", "laptop", "s-1") {
		t.Fatal("a pending question does not mark its session waiting")
	}
	if s.Waiting(2, "mirror", "laptop", "s-1") || s.Waiting(1, "mirror", "other-host", "s-1") || s.Waiting(1, "mirror", "laptop", "s-2") {
		t.Fatal("waiting leaked to another owner, host or session")
	}
}

// TestAnswerIsTheOwnersAloneAndReplaySafe pins the store's authz and idempotency.
func TestAnswerIsTheOwnersAloneAndReplaySafe(t *testing.T) {
	s, _ := testStore(t)
	if _, err := s.Open(key(1), twoQs, time.Minute, ""); err != nil {
		t.Fatal(err)
	}
	// Another owner, same everything else: not found.
	if _, err := s.Answer(key(2), someAnswers); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another owner's answer: %v, want ErrNotFound", err)
	}
	// Answers that do not fit the questions: refused, still pending.
	if _, err := s.Answer(key(1), map[string]string{"Which color?": "Red"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a partial answer: %v", err)
	}
	e, err := s.Answer(key(1), someAnswers)
	if err != nil || e.State != Answered {
		t.Fatalf("answer: %+v %v", e, err)
	}
	if _, err := s.Answer(key(1), someAnswers); err != nil {
		t.Fatalf("the same answers again (a double submit) must succeed: %v", err)
	}
	if _, err := s.Answer(key(1), map[string]string{"Which color?": "Blue", "Which pets?": "Cat"}); !errors.Is(err, ErrAnswered) {
		t.Fatalf("different answers after the first: %v, want ErrAnswered", err)
	}
	if s.Waiting(1, "mirror", "laptop", "s-1") {
		t.Fatal("an answered question still marks its session waiting")
	}
}

func TestPollWakesOnTheAnswerAndChecksTheHolder(t *testing.T) {
	s, _ := testStore(t)
	e, _ := s.Open(key(1), twoQs, time.Minute, "")
	if _, err := s.Poll(context.Background(), e.ID, 2, "mirror", "laptop", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another owner polled: %v", err)
	}
	if _, err := s.Poll(context.Background(), e.ID, 1, "mirror", "other-host", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another host polled: %v", err)
	}
	got := make(chan Entry, 1)
	go func() {
		p, _ := s.Poll(context.Background(), e.ID, 1, "mirror", "laptop", 10*time.Second)
		got <- p
	}()
	time.Sleep(50 * time.Millisecond)
	if _, err := s.Answer(key(1), someAnswers); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-got:
		if p.State != Answered || p.Answers["Which pets?"] != "Cat, Dog" {
			t.Fatalf("poll woke with %+v", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the long-poll did not wake on the answer")
	}
}

func TestWithdrawSendsItBackToTheTerminal(t *testing.T) {
	s, _ := testStore(t)
	e, _ := s.Open(key(1), twoQs, time.Minute, "")
	if _, err := s.Withdraw(e.ID, 2, "mirror", "laptop"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another owner withdrew: %v", err)
	}
	if _, err := s.Withdraw(e.ID, 1, "mirror", "laptop"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Answer(key(1), someAnswers); !errors.Is(err, ErrNotPending) {
		t.Fatalf("answering a withdrawn question: %v, want ErrNotPending", err)
	}
	// The same call asked again is a new, pending entry.
	again, err := s.Open(key(1), twoQs, time.Minute, "")
	if err != nil || again.ID == e.ID || again.State != Pending {
		t.Fatalf("re-asked: %+v %v", again, err)
	}
}

// TestAHookThatStopsPollingIsWithdrawn: a hook killed without its DELETE
// (the CLI's own timeout, a Ctrl-C) must not leave the session "waiting".
func TestAHookThatStopsPollingIsWithdrawn(t *testing.T) {
	s, c := testStore(t)
	e, _ := s.Open(key(1), twoQs, time.Hour, "")
	c.add(PollGrace - time.Second)
	if _, err := s.Poll(context.Background(), e.ID, 1, "mirror", "laptop", 0); err != nil {
		t.Fatal(err)
	}
	c.add(PollGrace - time.Second)
	if !s.Waiting(1, "mirror", "laptop", "s-1") {
		t.Fatal("a hook that polled within PollGrace was taken for dead")
	}
	c.add(2 * time.Second)
	if s.Waiting(1, "mirror", "laptop", "s-1") {
		t.Fatal("a hook silent past PollGrace still marks its session waiting")
	}
	if got, _ := s.Get(e.ID); got.State != Withdrawn {
		t.Fatalf("state %s, want withdrawn", got.State)
	}
}

func TestTheDeadlineWithdraws(t *testing.T) {
	s, c := testStore(t)
	e, _ := s.Open(key(1), twoQs, 30*time.Second, "")
	c.add(31 * time.Second)
	if got, _ := s.Get(e.ID); got.State != Withdrawn {
		t.Fatalf("state %s past the deadline, want withdrawn", got.State)
	}
	// A hold past MaxHold is capped.
	f, _ := s.Open(Key{Owner: 1, Source: "mirror", Scope: "laptop", Session: "s-1", ToolUseID: "toolu_2"}, twoQs, 10*time.Hour, "")
	if d := f.Deadline.Sub(f.Created); d != MaxHold {
		t.Fatalf("hold %s, want MaxHold", d)
	}
}

func TestPendingIsBoundedPerOwner(t *testing.T) {
	s, _ := testStore(t)
	for i := 0; i < MaxPendingPerOwner; i++ {
		k := key(1)
		k.ToolUseID = "toolu_" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		if _, err := s.Open(k, twoQs, time.Minute, ""); err != nil {
			t.Fatalf("%d: %v", i, err)
		}
	}
	k := key(1)
	k.ToolUseID = "toolu_over"
	if _, err := s.Open(k, twoQs, time.Minute, ""); !errors.Is(err, ErrFull) {
		t.Fatalf("past the bound: %v, want ErrFull", err)
	}
	if _, err := s.Open(key(2), twoQs, time.Minute, ""); err != nil {
		t.Fatalf("another owner is not bounded by owner 1's questions: %v", err)
	}
	if n := len(s.Pending(1)); n != MaxPendingPerOwner {
		t.Fatalf("Pending(1) = %d", n)
	}
}

func TestOnChangeSeesEveryTransition(t *testing.T) {
	s, _ := testStore(t)
	var mu sync.Mutex
	var seen []State
	s.OnChange(func(e Entry) { mu.Lock(); seen = append(seen, e.State); mu.Unlock() })
	e, _ := s.Open(key(1), twoQs, time.Minute, "")
	_, _ = s.Answer(key(1), someAnswers)
	_, _ = s.Withdraw(e.ID, 1, "mirror", "laptop") // already answered: no change
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != Pending || seen[1] != Answered {
		t.Fatalf("changes %v, want [pending answered]", seen)
	}
}
