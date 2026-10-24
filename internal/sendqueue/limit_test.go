package sendqueue

import (
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// THE HOLD RULES (limit.go), as a pure function of the head, the target's state
// and the clock: Decide's own, so the composer's "Held — …" and the
// dispatcher's restraint are the same decision.

// limitErr is a usage limit resetting at resetsAt (zero: none named);
// rejected: it was its prompt's only reply.
func limitErr(resetsAt time.Time, rejected bool) *mirror.TurnError {
	e := &mirror.TurnError{Kind: mirror.TurnErrorLimit, Rejected: rejected, Quick: 1}
	if !resetsAt.IsZero() {
		e.ResetsAt = resetsAt.UnixMilli()
	}
	return e
}

func TestDecideHoldsOnAUsageLimitUntilItsResetPlusTheMargin(t *testing.T) {
	now := time.Date(2026, 10, 12, 21, 0, 0, 0, time.UTC)
	reset := now.Add(90 * time.Minute)
	st := idleSince(now.Add(-time.Hour))
	st.TurnError = limitErr(reset, true)
	h := Head{Item: Item{State: Queued}}
	for _, c := range []struct {
		at   time.Time
		send bool
	}{
		{now, false},
		{reset.Add(-time.Second), false},
		{reset, false}, // the reset minute itself: not yet
		{reset.Add(LimitMargin - time.Second), false}, // inside the margin
		{reset.Add(LimitMargin), true},                // past it
		{reset.Add(24 * time.Hour), true},
	} {
		d := Decide(h, st, c.at, idle)
		if d.Send != c.send {
			t.Fatalf("at %s (reset %s): Decide = %+v; want send %t", c.at, reset, d, c.send)
		}
		if !c.send && (!d.Held || !d.ResumeAt.Equal(reset) || d.Why != LimitHeldText) {
			t.Fatalf("at %s: Decide = %+v; want held, resuming at the reset %s, saying %q", c.at, d, reset, LimitHeldText)
		}
	}
}

// TestTheHeldWordsCarryNoTime: the time a held queue resumes is ResumeAt,
// drawn in the reader's zone; the words never carry the server's rendering of
// it, which would disagree with the reader's.
func TestTheHeldWordsCarryNoTime(t *testing.T) {
	for _, s := range []string{LimitHeldText, LimitHeldNoReset, BreakerHeldText(&mirror.TurnError{Kind: "server_error", Quick: 3})} {
		if strings.ContainsAny(strings.ReplaceAll(s, "last 3", ""), "0123456789:") {
			t.Fatalf("%q carries a time", s)
		}
	}
}

// TestALimitWithNoResetTimeHoldsUntilARealAnswer: however long it lasts; the
// positive control is the same head once the host reports no turn error (a
// real answer came).
func TestALimitWithNoResetTimeHoldsUntilARealAnswer(t *testing.T) {
	now := time.Date(2026, 10, 12, 21, 0, 0, 0, time.UTC)
	st := idleSince(now.Add(-time.Hour))
	st.TurnError = limitErr(time.Time{}, true)
	h := Head{Item: Item{State: Queued}}
	for _, after := range []time.Duration{0, time.Hour, 7 * 24 * time.Hour} {
		if d := Decide(h, st, now.Add(after), idle); d.Send || !d.Held || d.Why != LimitHeldNoReset || !d.ResumeAt.IsZero() {
			t.Fatalf("%s later: Decide = %+v; want held, saying %q", after, d, LimitHeldNoReset)
		}
	}
	st.TurnError = nil
	if d := Decide(h, st, now, idle); !d.Send {
		t.Fatalf("CONTROL, a real answer: Decide = %+v; want send", d)
	}
}

// TestTheBreakerBacksOff1_2_5Then15Minutes is the back-off schedule on an
// injected clock: BreakerAfter quick failures in a row hold for 1 minute from
// when muster saw the failure, each further one for 2, 5, then 15 — and 15
// again after that; one second before each the queue is held, saying when it
// retries; at it, it sends.
func TestTheBreakerBacksOff1_2_5Then15Minutes(t *testing.T) {
	seen := time.Date(2026, 10, 12, 21, 0, 0, 0, time.UTC)
	h := Head{Item: Item{State: Queued}}
	for _, c := range []struct {
		quick int
		hold  time.Duration
	}{
		{BreakerAfter, time.Minute},
		{BreakerAfter + 1, 2 * time.Minute},
		{BreakerAfter + 2, 5 * time.Minute},
		{BreakerAfter + 3, 15 * time.Minute},
		{BreakerAfter + 4, 15 * time.Minute},
		{BreakerAfter + 9, 15 * time.Minute},
	} {
		e := &mirror.TurnError{Kind: "authentication_failed", Quick: c.quick}
		st := AgentState{Idle: true, Since: 1, Observed: seen, TurnError: e}
		before := Decide(h, st, seen.Add(c.hold-time.Second), 0)
		if before.Send || !before.Held || !before.ResumeAt.Equal(seen.Add(c.hold)) || before.Why != BreakerHeldText(e) {
			t.Fatalf("quick %d, a second before %s: Decide = %+v; want held until %s", c.quick, c.hold, before, seen.Add(c.hold))
		}
		if at := Decide(h, st, seen.Add(c.hold), 0); !at.Send {
			t.Fatalf("quick %d, at %s: Decide = %+v; want send (the back-off has run)", c.quick, c.hold, at)
		}
	}
}

func TestTheCircuitBreakerHoldsOnQuickFailuresInARow(t *testing.T) {
	now := time.Date(2026, 10, 12, 21, 0, 0, 0, time.UTC)
	h := Head{Item: Item{State: Queued}}
	for _, c := range []struct {
		name string
		e    *mirror.TurnError
		held bool
	}{
		{"no error (a real answer resets it)", nil, false},
		{"one quick failure", &mirror.TurnError{Kind: "authentication_failed", Quick: 1}, false},
		{"BreakerAfter quick failures", &mirror.TurnError{Kind: "authentication_failed", Quick: BreakerAfter}, true},
		{"more", &mirror.TurnError{Kind: "server_error", Quick: BreakerAfter + 3}, true},
		{"an error that was not quick", &mirror.TurnError{Kind: "server_error"}, false},
		// A limit that has RESET is not held by the breaker: those quick turns
		// were the limit's.
		{"a reset limit's quick turns", &mirror.TurnError{Kind: mirror.TurnErrorLimit, ResetsAt: now.Add(-time.Hour).UnixMilli(), Quick: 5}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := AgentState{Idle: true, Since: 1, Observed: now.Add(-30 * time.Second), TurnError: c.e}
			d := Decide(h, st, now, idle/2)
			if d.Send == c.held || d.Held != c.held {
				t.Fatalf("Decide = %+v; want held %t", d, c.held)
			}
			if c.held && d.Why != BreakerHeldText(c.e) {
				t.Fatalf("Why = %q; want %q", d.Why, BreakerHeldText(c.e))
			}
		})
	}
}

// TestTheHoldComesAfterTheTurnChecks: a limit does not hide that the window is
// working, or that the previous message's turn has not ended — those say what
// is true now.
func TestTheHoldComesAfterTheTurnChecks(t *testing.T) {
	now := time.Date(2026, 10, 12, 21, 0, 0, 0, time.UTC)
	e := limitErr(now.Add(time.Hour), true)
	busy := AgentState{Why: "Claude Code is working.", TurnError: e}
	if d := Decide(Head{Item: Item{State: Queued}}, busy, now, idle); d.Held || d.Why != busy.Why {
		t.Fatalf("busy: %+v", d)
	}
	st := idleSince(now.Add(-time.Hour))
	st.TurnError = e
	if d := Decide(Head{Item: Item{State: Queued}, TurnAfter: st.Since}, st, now, idle); d.Held || !strings.Contains(d.Why, "previous message") {
		t.Fatalf("turn not ended: %+v", d)
	}
}

// TestBounced is the one rule for "a usage limit refused this sent message
// before Claude Code worked on it", each condition's own case against the case
// that passes.
func TestBounced(t *testing.T) {
	ok := Item{State: Sent, Body: "please run the migration", AfterSince: 1000}
	refused := limitErr(time.Time{}, true)
	refused.Prompt, refused.PrevAt = mirror.PromptDigest(ok.Body), 1000
	okSt := AgentState{Idle: true, Since: 2000, Pasted: 1000, TurnError: refused, Session: "s1"}
	if !Bounced(ok, "s1", okSt) {
		t.Fatal("CONTROL: the bounced case is not Bounced")
	}
	if Bounced(ok, "", okSt) {
		t.Fatal("Bounced with no record of the session it was pasted into")
	}
	unknown := okSt
	unknown.Session = ""
	if Bounced(ok, "", unknown) {
		t.Fatal("Bounced with neither session known: unknown is never a match")
	}
	// Any kind the CLI refuses with counts, not only a limit (the breaker's
	// probes: "Not logged in", an API error).
	other := *refused
	other.Kind = "authentication_failed"
	if !Bounced(ok, "s1", AgentState{Idle: true, Since: 2000, Pasted: 1000, TurnError: &other, Session: "s1"}) {
		t.Fatal("CONTROL: a refusal of another kind is not Bounced")
	}
	for name, f := range map[string]func(*Item, *AgentState){
		"not sent (failed)":              func(it *Item, _ *AgentState) { it.State = Failed },
		"not idle":                       func(_ *Item, st *AgentState) { st.Idle = false },
		"no status newer than its paste": func(_ *Item, st *AgentState) { st.Since = 1000 },
		"a paste through muster since":   func(_ *Item, st *AgentState) { st.Pasted = 1500 },
		"no turn error (a real answer)":  func(_ *Item, st *AgentState) { st.TurnError = nil },
		"processed first (mid-turn)":     func(_ *Item, st *AgentState) { st.TurnError.Rejected = false },
		"another prompt was refused":     func(_ *Item, st *AgentState) { st.TurnError.Prompt = mirror.PromptDigest("typed in the terminal") },
		"a record after its paste":       func(_ *Item, st *AgentState) { st.TurnError.PrevAt = 1001 },
		"another Claude session":         func(_ *Item, st *AgentState) { st.Session = "s2" },
		"the window's session unknown":   func(_ *Item, st *AgentState) { st.Session = "" },
	} {
		it, st := ok, okSt
		e := *okSt.TurnError
		st.TurnError = &e
		f(&it, &st)
		if Bounced(it, "s1", st) {
			t.Errorf("%s: Bounced; want not", name)
		}
	}
}

// TestTheHostsLimitHoldsAWindowWhoseOwnTurnSaysNothing: a usage limit is the
// account's. A window whose own latest turn carries no TurnError (another
// window hit the limit, or this one's report was cut by the list's bound) is
// held on its host's limit, until its reset plus the margin; with no reset,
// until the host stops reporting it. A window's own non-limit error does not
// hide the host's limit. CONTROL: no host limit, the same window sends.
func TestTheHostsLimitHoldsAWindowWhoseOwnTurnSaysNothing(t *testing.T) {
	now := time.Date(2026, 10, 12, 21, 0, 0, 0, time.UTC)
	reset := now.Add(time.Hour)
	h := Head{Item: Item{State: Queued}}
	st := idleSince(now.Add(-time.Hour))
	if d := Decide(h, st, now, idle); !d.Send {
		t.Fatalf("CONTROL, no host limit: %+v", d)
	}
	st.HostLimit = &mirror.HostLimit{At: 1, ResetsAt: reset.UnixMilli()}
	if d := Decide(h, st, now, idle); d.Send || !d.Held || d.Why != LimitHeldText || !d.ResumeAt.Equal(reset) {
		t.Fatalf("on a limited host: %+v; want held until %s", d, reset)
	}
	if d := Decide(h, st, reset.Add(LimitMargin), idle); !d.Send {
		t.Fatalf("past its reset: %+v; want send", d)
	}
	st.TurnError = &mirror.TurnError{Kind: "server_error"}
	if d := Decide(h, st, now, idle); d.Send || d.Why != LimitHeldText {
		t.Fatalf("its own error another kind: %+v; want held on the host's limit", d)
	}
	st.TurnError, st.HostLimit = nil, &mirror.HostLimit{At: 1}
	if d := Decide(h, st, now.Add(48*time.Hour), idle); d.Send || d.Why != LimitHeldNoReset {
		t.Fatalf("a host limit with no reset: %+v; want held", d)
	}
}

// TestTheNewerLimitHolds (F2): window A's own latest turn is YESTERDAY's
// limit, long reset, while its host reports a live limit another window hit
// just now: the newer limit, by its record's time, holds A. CONTROL: with the
// host's limit older than A's own, A's own (reset) decides, and it sends.
func TestTheNewerLimitHolds(t *testing.T) {
	now := time.Date(2026, 10, 12, 21, 0, 0, 0, time.UTC)
	yesterday := now.Add(-24 * time.Hour)
	h := Head{Item: Item{State: Queued}}
	st := idleSince(now.Add(-time.Hour))
	st.TurnError = &mirror.TurnError{Kind: mirror.TurnErrorLimit, At: yesterday.UnixMilli(), ResetsAt: yesterday.Add(time.Hour).UnixMilli()}
	if d := Decide(h, st, now, idle); !d.Send {
		t.Fatalf("CONTROL, A's own limit reset yesterday, no host limit: %+v; want send", d)
	}
	reset := now.Add(time.Hour)
	st.HostLimit = &mirror.HostLimit{At: now.Add(-time.Minute).UnixMilli(), ResetsAt: reset.UnixMilli()}
	if d := Decide(h, st, now, idle); d.Send || !d.Held || !d.ResumeAt.Equal(reset) {
		t.Fatalf("A with yesterday's limit, the host's live: %+v; want held until %s", d, reset)
	}
	st.HostLimit.At = yesterday.Add(-time.Hour).UnixMilli()
	if d := Decide(h, st, now, idle); !d.Send {
		t.Fatalf("CONTROL, the host's limit older than A's own: %+v; want A's own (reset) to decide", d)
	}
}
