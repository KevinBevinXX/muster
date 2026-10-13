package stepup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// withWindow sets the process's window for one test and restores the default.
// (No test in this package runs in parallel: the window is process-wide.)
func withWindow(t *testing.T, d time.Duration) {
	t.Helper()
	if err := SetWindow(d); err != nil {
		t.Fatalf("SetWindow(%v): %v", d, err)
	}
	t.Cleanup(func() { window.Store(0) })
}

// TestFreshIsWithinTheWindowOfTheSignInAndUnknownIsStale: the boundary is
// inclusive (exactly Window is fresh), one second past is stale, and a context
// with no sign-in time is stale. The clock is INJECTED (the `now` argument) —
// nothing sleeps — and the points measured are the start, the middle, both
// sides of the boundary, far past it, and absence. Run at the default window
// and at a configured one, so a gate that ignored the configured value (or a
// literal 5 minutes left behind) fails one of them.
func TestFreshIsWithinTheWindowOfTheSignInAndUnknownIsStale(t *testing.T) {
	signedIn := time.Unix(1_800_000_000, 0)
	ctx := WithAuthTime(context.Background(), signedIn)
	for _, w := range []time.Duration{0, 37 * time.Minute} {
		name := "default"
		if w != 0 {
			withWindow(t, w)
			name = w.String()
		}
		win := Window()
		for _, tc := range []struct {
			after time.Duration
			want  bool
		}{
			{0, true}, {win / 2, true}, {win - time.Second, true}, {win, true},
			{win + time.Second, false}, {win + time.Hour, false}, {3 * win, false},
		} {
			if got := Fresh(ctx, signedIn.Add(tc.after)); got != tc.want {
				t.Errorf("window %s, signed in %v ago: Fresh = %v, want %v", name, tc.after, got, tc.want)
			}
		}
		if got := FreshUntil(ctx); !got.Equal(signedIn.Add(win)) {
			t.Errorf("window %s: FreshUntil = %v, want %v", name, got, signedIn.Add(win))
		}
	}
	if Fresh(context.Background(), signedIn) {
		t.Error("a context with no sign-in time is fresh")
	}
	if Fresh(WithAuthTime(context.Background(), time.Time{}), signedIn) {
		t.Error("a zero sign-in time is fresh")
	}
	if !FreshUntil(context.Background()).IsZero() {
		t.Error("FreshUntil of no sign-in is not zero")
	}
}

// TestTheDefaultWindowIsTwelveHours pins the default to the LITERAL the
// operator chose, and that it is what Window answers when nothing configured
// one. Six hours is fresh and thirteen is not: a default left at (or reverted
// to) five minutes fails the first, an unbounded one the second.
func TestTheDefaultWindowIsTwelveHours(t *testing.T) {
	if DefaultWindow != 12*time.Hour {
		t.Fatalf("DefaultWindow = %v, want 12h", DefaultWindow)
	}
	if got := Window(); got != 12*time.Hour {
		t.Fatalf("Window() with nothing configured = %v, want 12h", got)
	}
	signedIn := time.Unix(1_800_000_000, 0)
	ctx := WithAuthTime(context.Background(), signedIn)
	if !Fresh(ctx, signedIn.Add(6*time.Hour)) {
		t.Error("6 hours after sign-in is stale under the default window")
	}
	if Fresh(ctx, signedIn.Add(13*time.Hour)) {
		t.Error("13 hours after sign-in is fresh under the default window")
	}
}

// TestTheWindowIsBounded: ParseWindow (the environment variable's reader) and
// SetWindow both refuse zero, a negative duration, anything under MinWindow and
// anything over MaxWindow — and a refused SetWindow leaves the window as it
// was. The accepted points are both bounds and a middle.
func TestTheWindowIsBounded(t *testing.T) {
	if MaxWindow != 24*time.Hour || MinWindow != time.Minute {
		t.Fatalf("bounds = [%v, %v], want [1m, 24h]", MinWindow, MaxWindow)
	}
	for _, v := range []string{"0", "0s", "-1h", "-5m", "59s", "24h1s", "25h", "720h", "12", "twelve hours", "12 h", "1d"} {
		if d, err := ParseWindow(v); err == nil {
			t.Errorf("ParseWindow(%q) = %v, want an error", v, d)
		}
	}
	for v, want := range map[string]time.Duration{
		"": DefaultWindow, "  ": DefaultWindow, "1m": time.Minute, "45m": 45 * time.Minute,
		"12h": 12 * time.Hour, " 6h30m ": 6*time.Hour + 30*time.Minute, "24h": 24 * time.Hour,
	} {
		if d, err := ParseWindow(v); err != nil || d != want {
			t.Errorf("ParseWindow(%q) = %v, %v; want %v", v, d, err, want)
		}
	}

	withWindow(t, 2*time.Hour)
	for _, d := range []time.Duration{0, -time.Hour, MinWindow - time.Second, MaxWindow + time.Second, 1000 * time.Hour} {
		if err := SetWindow(d); err == nil {
			t.Errorf("SetWindow(%v) was accepted", d)
		}
		if got := Window(); got != 2*time.Hour {
			t.Fatalf("a refused SetWindow(%v) changed the window to %v", d, got)
		}
	}
	for _, d := range []time.Duration{MinWindow, 7 * time.Hour, MaxWindow} {
		if err := SetWindow(d); err != nil || Window() != d {
			t.Errorf("SetWindow(%v): %v, Window() = %v", d, err, Window())
		}
	}
}

// TestEveryMessageNamesTheConfiguredWindow: the refusal a person reads is built
// from the window, not from a second copy of it — at the default it says "12
// hours", at a configured 45 minutes it says "45 minutes", and it never says
// the old "5 minutes" at either. errors.Is still finds ErrStale through a
// wrapper.
func TestEveryMessageNamesTheConfiguredWindow(t *testing.T) {
	if got := ErrStale.Error(); !strings.HasSuffix(got, "the last 12 hours") {
		t.Errorf("default refusal = %q, want it to end \"the last 12 hours\"", got)
	}
	withWindow(t, 45*time.Minute)
	if got := ErrStale.Error(); !strings.HasSuffix(got, "the last 45 minutes") || strings.Contains(got, "12 hours") {
		t.Errorf("refusal under a 45m window = %q", got)
	}
	if got := WindowText(); got != "45 minutes" {
		t.Errorf("WindowText = %q", got)
	}
	if !errors.Is(fmt.Errorf("wrapped: %w", ErrStale), ErrStale) {
		t.Error("errors.Is does not find ErrStale through a wrapper")
	}
	for d, want := range map[time.Duration]string{
		12 * time.Hour: "12 hours", time.Hour: "1 hour", 5 * time.Minute: "5 minutes", time.Minute: "1 minute",
		90 * time.Minute: "1 hour 30 minutes", 24 * time.Hour: "24 hours", 61 * time.Second: "1 minute 1 second",
		2*time.Hour + 5*time.Second: "2 hours 5 seconds",
	} {
		if got := HumanDuration(d); got != want {
			t.Errorf("HumanDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

// TestTheCredentialWindowDidNotLengthen: adding a passkey is gated on its own
// FIXED five minutes, whatever the typing window is configured to. Six minutes
// after sign-in is fresh for typing and stale for a credential change.
func TestTheCredentialWindowDidNotLengthen(t *testing.T) {
	if CredentialMaxAge != 5*time.Minute {
		t.Fatalf("CredentialMaxAge = %v, want 5m", CredentialMaxAge)
	}
	signedIn := time.Unix(1_800_000_000, 0)
	ctx := WithAuthTime(context.Background(), signedIn)
	for _, w := range []time.Duration{0, MaxWindow} {
		if w != 0 {
			withWindow(t, w)
		}
		at := signedIn.Add(6 * time.Minute)
		if !Fresh(ctx, at) {
			t.Errorf("window %v: 6 minutes after sign-in is stale for typing", Window())
		}
		if FreshWithin(ctx, at, CredentialMaxAge) {
			t.Errorf("window %v: 6 minutes after sign-in is fresh for a credential change", Window())
		}
		if !FreshWithin(ctx, signedIn.Add(4*time.Minute), CredentialMaxAge) {
			t.Errorf("window %v: 4 minutes after sign-in is stale for a credential change", Window())
		}
	}
}
