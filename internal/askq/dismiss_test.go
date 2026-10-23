package askq

import "testing"

// TestADismissalWaitsForAnyTyping: an open question no answer is being typed
// into may be dismissed (Dismissing); one being typed, or whose last typing's
// outcome is unknown, is ErrBusy (a host may still be typing it); a resolved
// one ErrClosed; another owner's ErrNotFound.
func TestADismissalWaitsForAnyTyping(t *testing.T) {
	s, c := testStore(t)
	if _, err := s.Open(key(1), twoQs, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Dismissing(key(2)); err != ErrNotFound {
		t.Fatalf("another owner: %v", err)
	}
	if _, err := s.Dismissing(key(1)); err != nil {
		t.Fatalf("a pending question: %v", err)
	}
	_, _, _ = s.BeginTyping(key(1), someAnswers)
	if _, err := s.Dismissing(key(1)); err != ErrBusy {
		t.Fatalf("a question being typed: %v", err)
	}
	_, _ = s.TypingFailed(key(1), Failure{Note: "timeout", Settled: false})
	if _, err := s.Dismissing(key(1)); err != ErrBusy {
		t.Fatalf("a question whose typing's outcome is unknown: %v", err)
	}
	c.add(MaxDrive)
	if _, err := s.Dismissing(key(1)); err != nil {
		t.Fatalf("after MaxDrive: %v", err)
	}
	if _, ok := s.Resolve(1, key(1).Source, key(1).Scope, key(1).Session, key(1).ToolUseID, nil); !ok {
		t.Fatal("resolve")
	}
	if _, err := s.Dismissing(key(1)); err != ErrClosed {
		t.Fatalf("a resolved question: %v", err)
	}
}
