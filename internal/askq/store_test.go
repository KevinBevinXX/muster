package askq

import (
	"context"
	"errors"
	"fmt"
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
	c := &clock{t: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
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

var (
	someAnswers  = map[string]string{"Which color?": "Red", "Which pets?": "Cat, Dog"}
	otherAnswers = map[string]string{"Which color?": "Blue", "Which pets?": "Cat"}
)

func resolve(s *Store, owner int64, answers map[string]string) (Entry, bool) {
	k := key(owner)
	return s.Resolve(k.Owner, k.Source, k.Scope, k.Session, k.ToolUseID, answers)
}

func TestOpenIsIdempotentWhileOpen(t *testing.T) {
	s, _ := testStore(t)
	a, err := s.Open(key(1), twoQs, "")
	if err != nil || a.State != Pending {
		t.Fatalf("open: %+v %v", a, err)
	}
	if b, err := s.Open(key(1), twoQs, ""); err != nil || b.ID != a.ID {
		t.Fatalf("a retried Open is a new entry: %v %v", a.ID, b.ID)
	}
	if got := s.Pending(1); len(got) != 1 || got[0].ID != a.ID {
		t.Fatalf("Pending(1) = %+v", got)
	}
	if got := s.Pending(2); len(got) != 0 {
		t.Fatalf("Pending(2) = %+v", got)
	}
}

// TestTheHooksOwnIDIsIdempotent: the id the hook chose names the question; a
// retried registration is the same entry, another call cannot take the id.
func TestTheHooksOwnIDIsIdempotent(t *testing.T) {
	s, _ := testStore(t)
	id := NewID()
	if a, err := s.Open(key(1), twoQs, id); err != nil || a.ID != id {
		t.Fatalf("open: %+v %v", a, err)
	}
	if b, err := s.Open(key(1), twoQs, id); err != nil || b.ID != id {
		t.Fatalf("retry: %+v %v", b, err)
	}
	other := key(1)
	other.ToolUseID = "toolu_other"
	if _, err := s.Open(other, twoQs, id); !errors.Is(err, ErrIDTaken) {
		t.Fatalf("another call took the id: %v", err)
	}
	if _, err := s.Open(key(1), twoQs, "NOT-HEX"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a malformed id: %v", err)
	}
}

// TestBeginTypingIsTheOwnersAloneAndOnceOnly pins the typing gate: another
// owner's question is not found; answers must fit; a second, different answer
// while one is being typed is refused (ErrBusy); the SAME again is `again`
// (never typed twice).
func TestBeginTypingIsTheOwnersAloneAndOnceOnly(t *testing.T) {
	s, _ := testStore(t)
	if _, err := s.Open(key(1), twoQs, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.BeginTyping(key(2), someAnswers); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another owner: %v, want ErrNotFound", err)
	}
	if _, _, err := s.BeginTyping(key(1), map[string]string{"Which color?": "Red"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a partial answer: %v", err)
	}
	e, again, err := s.BeginTyping(key(1), someAnswers)
	if err != nil || again || e.State != Typing {
		t.Fatalf("begin: %+v again=%v %v", e, again, err)
	}
	if _, again, err := s.BeginTyping(key(1), someAnswers); err != nil || !again {
		t.Fatalf("the same answers again (a double submit): again=%v %v", again, err)
	}
	if _, _, err := s.BeginTyping(key(1), otherAnswers); !errors.Is(err, ErrBusy) {
		t.Fatalf("other answers while typing: %v, want ErrBusy", err)
	}
	// Still open while typing: the attention queue keeps it.
	if got := s.Pending(1); len(got) != 1 {
		t.Fatalf("a question being typed left Pending: %+v", got)
	}
}

// TestTheCLIsRecordSaysWhoWon: the result muster typed is "answered here"; any
// other result is the terminal's — whoever got there first, the CLI's record
// decides.
func TestTheCLIsRecordSaysWhoWon(t *testing.T) {
	// Muster's answer landed.
	s, _ := testStore(t)
	_, _ = s.Open(key(1), twoQs, "")
	_, _, _ = s.BeginTyping(key(1), someAnswers)
	if e, ok := resolve(s, 1, someAnswers); !ok || e.State != Answered || e.By != ByMuster {
		t.Fatalf("muster's answer recorded: %+v %v", e, ok)
	}
	// The terminal got there first, though muster was typing.
	s, _ = testStore(t)
	_, _ = s.Open(key(1), twoQs, "")
	_, _, _ = s.BeginTyping(key(1), someAnswers)
	if e, ok := resolve(s, 1, otherAnswers); !ok || e.State != Answered || e.By != ByTerminal || e.Answers["Which color?"] != "Blue" {
		t.Fatalf("the terminal's answer recorded: %+v %v", e, ok)
	}
	// Answered only in the terminal.
	s, _ = testStore(t)
	_, _ = s.Open(key(1), twoQs, "")
	if e, ok := resolve(s, 1, otherAnswers); !ok || e.By != ByTerminal {
		t.Fatalf("a terminal-only answer: %+v %v", e, ok)
	}
	// Then: begin typing a resolved question is refused, resolving again is a
	// no-op, and it leaves Pending.
	if _, _, err := s.BeginTyping(key(1), someAnswers); !errors.Is(err, ErrClosed) {
		t.Fatalf("typing into an answered question: %v, want ErrClosed", err)
	}
	if _, ok := resolve(s, 1, someAnswers); ok {
		t.Fatal("a resolved question resolved again")
	}
	if got := s.Pending(1); len(got) != 0 {
		t.Fatalf("an answered question is still pending: %+v", got)
	}
	// Dismissed in the terminal: no answers.
	s, _ = testStore(t)
	_, _ = s.Open(key(1), twoQs, "")
	if e, ok := resolve(s, 1, nil); !ok || e.State != Declined {
		t.Fatalf("a dismissed picker: %+v %v", e, ok)
	}
	// ...and a dismissed question is not typed into either.
	if _, _, err := s.BeginTyping(key(1), someAnswers); !errors.Is(err, ErrClosed) {
		t.Fatalf("typing into a dismissed question: %v, want ErrClosed", err)
	}
	// A result for a call the store never asked about changes nothing.
	if _, ok := s.Resolve(1, "mirror", "laptop", "s-1", "toolu_unknown", someAnswers); ok {
		t.Fatal("an unknown call resolved")
	}
}

func TestTypingFailedLeavesItToTheTerminal(t *testing.T) {
	s, _ := testStore(t)
	_, _ = s.Open(key(1), twoQs, "")
	_, _, _ = s.BeginTyping(key(1), someAnswers)
	e, err := s.TypingFailed(key(1), "no picker")
	if err != nil || e.State != Failed || e.Note != "no picker" {
		t.Fatalf("failed: %+v %v", e, err)
	}
	// Still open: the terminal answers it, or muster may try again.
	if e, ok := resolve(s, 1, otherAnswers); !ok || e.By != ByTerminal {
		t.Fatalf("the terminal's answer after a failed typing: %+v %v", e, ok)
	}
	// A failure reported after the CLI resolved it does not undo that.
	s, _ = testStore(t)
	_, _ = s.Open(key(1), twoQs, "")
	_, _, _ = s.BeginTyping(key(1), someAnswers)
	_, _ = resolve(s, 1, otherAnswers)
	if e, _ := s.TypingFailed(key(1), "late"); e.State != Answered {
		t.Fatalf("a late failure overwrote the result: %+v", e)
	}
}

func TestAnOpenQuestionExpires(t *testing.T) {
	s, c := testStore(t)
	e, _ := s.Open(key(1), twoQs, "")
	c.add(MaxOpen - time.Minute)
	if got, _ := s.Get(e.ID); got.State != Pending {
		t.Fatalf("expired early: %s", got.State)
	}
	c.add(2 * time.Minute)
	if got, _ := s.Get(e.ID); got.State != Expired {
		t.Fatalf("state %s past MaxOpen, want expired", got.State)
	}
	if len(s.Pending(1)) != 0 {
		t.Fatal("an expired question is still pending")
	}
}

func TestPendingIsBoundedPerOwner(t *testing.T) {
	s, _ := testStore(t)
	for i := 0; i < MaxPendingPerOwner; i++ {
		k := key(1)
		k.ToolUseID = fmt.Sprintf("toolu_%d", i)
		if _, err := s.Open(k, twoQs, ""); err != nil {
			t.Fatalf("%d: %v", i, err)
		}
	}
	k := key(1)
	k.ToolUseID = "toolu_over"
	if _, err := s.Open(k, twoQs, ""); !errors.Is(err, ErrFull) {
		t.Fatalf("past the bound: %v, want ErrFull", err)
	}
	if _, err := s.Open(key(2), twoQs, ""); err != nil {
		t.Fatalf("another owner is not bounded by owner 1's questions: %v", err)
	}
}

// TestOneOwnerCannotFillTheStore: owner 1 asking and resolving past every
// bound never makes the store refuse owner 2.
func TestOneOwnerCannotFillTheStore(t *testing.T) {
	s, _ := testStore(t)
	for i := 0; i < MaxEntries+500; i++ {
		k := key(1)
		k.ToolUseID = fmt.Sprintf("toolu_%d", i)
		if _, err := s.Open(k, twoQs, ""); err != nil {
			t.Fatalf("owner 1's question %d: %v", i, err)
		}
		_, _ = s.Resolve(k.Owner, k.Source, k.Scope, k.Session, k.ToolUseID, someAnswers)
		if i%499 == 0 {
			k2 := key(2)
			k2.ToolUseID = fmt.Sprintf("toolu_two_%d", i)
			if _, err := s.Open(k2, twoQs, ""); err != nil {
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

func TestOnChangeSeesEveryTransition(t *testing.T) {
	s, c := testStore(t)
	var mu sync.Mutex
	var seen []State
	s.OnChange(func(e Entry) { mu.Lock(); seen = append(seen, e.State); mu.Unlock() })
	_, _ = s.Open(key(1), twoQs, "")
	_, _, _ = s.BeginTyping(key(1), someAnswers)
	_, _ = s.TypingFailed(key(1), "x")
	_, _ = resolve(s, 1, otherAnswers)
	k := key(1)
	k.ToolUseID = "toolu_2"
	_, _ = s.Open(k, twoQs, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.RunSweep(ctx, 5*time.Millisecond)
	c.add(MaxOpen + time.Minute)
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(seen)
		last := State("")
		if n > 0 {
			last = seen[n-1]
		}
		mu.Unlock()
		if n == 6 && last == Expired {
			break
		}
		if time.Now().After(deadline) {
			mu.Lock()
			t.Fatalf("changes %v, want [pending typing failed answered pending expired]", seen)
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []State{Pending, Typing, Failed, Answered, Pending, Expired}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("changes %v, want %v", seen, want)
		}
	}
}
