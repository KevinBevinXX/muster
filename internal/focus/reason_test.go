package focus

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestReasonIsShortFixedAndNonSecret: every failure a refresh can end in maps
// to its fixed reason, and nothing of the error's own text — a provider's body,
// a key-shaped string, a host — survives into it.
func TestReasonIsShortFixedAndNonSecret(t *testing.T) {
	secret := "sk-or-v1-LEAKED upstream body text"
	for _, c := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("%w: %s", ErrNoKey, secret), ReasonNoKey},
		{ErrRateLimited, ReasonRateLimited},
		{ErrCapped, ReasonRateLimited},
		{&HTTPError{Status: 502, Body: secret}, "summarizer error (HTTP 502)"},
		{fmt.Errorf("wrapped: %w", &HTTPError{Status: 500, Body: secret}), "summarizer error (HTTP 500)"},
		{ErrMalformed, ReasonUnusable},
		{ErrOversized, ReasonUnusable},
		{ErrEmpty, ReasonUnusable},
		{fmt.Errorf("%w: %s", ErrUnreachable, secret), ReasonUnreachable},
		{ErrNoSession, ReasonNoSession},
		{errors.New(secret), ReasonFailed},
	} {
		got := Reason(c.err)
		if got != c.want {
			t.Errorf("Reason(%v) = %q, want %q", c.err, got, c.want)
		}
		if strings.Contains(got, "sk-or") || strings.Contains(got, "upstream") || len(got) > 60 {
			t.Errorf("Reason(%v) = %q carries the error's text", c.err, got)
		}
	}
	if Reason(nil) != "" {
		t.Error("Reason(nil) is not empty")
	}
}

// TestProblemIsTheReasonOnlyWhenMissingOrStale: a failure with no focus, or
// one AFTER the focus was written, is the problem; a failure the focus was
// written after (a later success) is not.
func TestProblemIsTheReasonOnlyWhenMissingOrStale(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	for _, c := range []struct {
		name string
		f    Focus
		want string
	}{
		{"no focus, a failure", Focus{LastError: ReasonNoKey, ErrorAt: now}, ReasonNoKey},
		{"a focus, a later failure", Focus{Title: "t", UpdatedAt: now, LastError: ReasonRateLimited, ErrorAt: now.Add(time.Minute)}, ReasonRateLimited},
		{"a failure, a later focus", Focus{Title: "t", UpdatedAt: now.Add(time.Minute), LastError: ReasonRateLimited, ErrorAt: now}, ""},
		{"a focus, no failure", Focus{Title: "t", UpdatedAt: now}, ""},
		{"nothing", Focus{}, ""},
	} {
		if got := c.f.Problem(); got != c.want {
			t.Errorf("%s: Problem() = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestEveryFailedRefreshRecordsItsReason: the rate cap, the summarizer's
// errors, an unreadable session and a missing key each leave their Reason on
// the row, never the raw error text.
func TestEveryFailedRefreshRecordsItsReason(t *testing.T) {
	ctx := context.Background()
	r, st, src, sm, sub := fixture()
	sm.err = &HTTPError{Status: 503, Body: "sk-or-v1-secret-body"}
	if _, err := r.Refresh(ctx, 1, sub); err == nil {
		t.Fatal("no error")
	}
	if st.errs[sub] != "summarizer error (HTTP 503)" {
		t.Fatalf("recorded %q", st.errs[sub])
	}
	// The cap: exhaust it, then the next subject records rate-limited.
	sm.err = nil
	r.Now = func() time.Time { return time.Unix(2_000_000, 0) }
	for i := 0; i < PerOwnerPerHour; i++ {
		s := AgentSubject(int64(500 + i))
		src.tails[s] = []Entry{{Role: "user", Text: fmt.Sprint(i)}}
		_, _ = r.Refresh(ctx, 1, s)
	}
	capped := AgentSubject(999)
	src.tails[capped] = []Entry{{Role: "user", Text: "late"}}
	if _, err := r.Refresh(ctx, 1, capped); !errors.Is(err, ErrCapped) {
		t.Fatalf("err = %v, want ErrCapped", err)
	}
	if st.errs[capped] != ReasonRateLimited {
		t.Fatalf("capped refresh recorded %q, want %q", st.errs[capped], ReasonRateLimited)
	}
}
