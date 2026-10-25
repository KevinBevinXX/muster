package mirror

import (
	"io"
	"log"
	"strings"
	"testing"
	"time"
)

// TestTheClaudeGuardsHoldBacksOffAndSaysWhy: a refusal by the host's prompt
// guard that the status cannot explain (a dialog on screen, text in the box)
// holds the window — so the send queue does not re-send at every 2-second
// pass — with the host's words, for 4 s, then 8 s, 16 s, and 30 s from then
// on. A new status (the prompt answered) and a paste that landed each end the
// hold and its backoff.
func TestTheClaudeGuardsHoldBacksOffAndSaysWhy(t *testing.T) {
	h := NewHub(log.New(io.Discard, "", 0))
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return now }
	const owner, host, session, window = int64(1), "on", "work", "@2"
	h.observe(owner, host, []Window{{Session: session, ID: window, Claude: true, Status: StatusIdle, StatusSince: 1000}}, now)
	held := func() string { return h.turn(owner, host, session, window).guardHeld(now) }
	if got := held(); got != "" {
		t.Fatalf("held before any refusal: %q", got)
	}

	for i, want := range []time.Duration{4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second} {
		h.guardHold(owner, host, session, window, ClaudeBoxText)
		got := held()
		if !strings.Contains(got, "input box already has text") || strings.Contains(got, "Not sent:") {
			t.Fatalf("refusal %d: held %q; want the host's reason, as a hold", i+1, got)
		}
		if until := h.turn(owner, host, session, window).guardUntil; until.Sub(now) != want {
			t.Fatalf("refusal %d: held for %s; want %s", i+1, until.Sub(now), want)
		}
		now = now.Add(want - time.Millisecond)
		if held() == "" {
			t.Fatalf("refusal %d: released before its %s were up", i+1, want)
		}
		now = now.Add(time.Millisecond)
		if got := held(); got != "" {
			t.Fatalf("refusal %d: still held at %s: %q", i+1, want, got)
		}
	}

	// A new status starts it over: the next refusal holds for 4 s again.
	h.observe(owner, host, []Window{{Session: session, ID: window, Claude: true, Status: StatusIdle, StatusSince: 2000}}, now)
	h.guardHold(owner, host, session, window, ClaudeBoxText)
	if d := h.turn(owner, host, session, window).guardUntil.Sub(now); d != 4*time.Second {
		t.Fatalf("after a new status the hold is %s; want 4s", d)
	}
	// A paste that landed ends it.
	h.endPaste(owner, host, session, window, 2000, true)
	if got := held(); got != "" {
		t.Fatalf("still held after a paste landed: %q", got)
	}
}
