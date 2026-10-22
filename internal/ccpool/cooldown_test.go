package ccpool

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// THE RATE-LIMIT COOLDOWN AND THE PER-ACCOUNT CAP (M4). A rate-limited account
// is NOT selectable until its cooldown ends; before this, it was only ranked
// lower and was still chosen when it was the best left.
// ---------------------------------------------------------------------------

// testCooldown is deliberately NOT DefaultCooldown, so an implementation that
// hard-codes the default instead of reading the configured value shows.
const testCooldown = 40 * time.Minute

// TestARateLimitedAccountIsSkippedDuringItsWindowAndSelectableAfter: alpha is
// the ONLY account, so before M4 it was chosen whatever its mark said. Its
// message names no reset time, so its window is the configured cooldown. It is
// refused for the whole window — up to one nanosecond before the end — and
// chosen again the instant the window has expired.
func TestARateLimitedAccountIsSkippedDuringItsWindowAndSelectableAfter(t *testing.T) {
	marks := map[string]Mark{"alpha": {RateLimitedAt: t0, RateLimitedDetail: "You've hit your limit"}}
	for _, tc := range []struct {
		name       string
		now        time.Time
		selectable bool
	}{
		{"the moment it was marked", t0, false},
		{"mid-window", t0.Add(25 * time.Minute), false},
		{"one nanosecond before the window ends", t0.Add(testCooldown - time.Nanosecond), false},
		{"the window just expired", t0.Add(testCooldown), true},
		{"long after", t0.Add(3 * time.Hour), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Choose(tc.now, testCooldown, []string{"alpha"}, tokens, marks, nil)
			if tc.selectable {
				if err != nil || got != "alpha" {
					t.Fatalf("Choose = %q, %v; want alpha (its cooldown has ended)", got, err)
				}
				return
			}
			if !errors.Is(err, ErrNoUsableAccount) {
				t.Fatalf("Choose = %q, %v; want ErrNoUsableAccount (alpha is cooling down until %s)",
					got, err, t0.Add(testCooldown).Format(time.RFC3339))
			}
			want := `alpha (rate-limited, cooling down until 2000-01-01T12:40:00Z)`
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("the refusal %q does not name the account and when it is usable again (%q)", err, want)
			}
		})
	}
}

// TestACoolingAccountLosesEvenWhenItWouldWinTheRanking: alpha was rate-limited
// LONGER ago than beta, so the least-recently-limited rule picks it — but
// alpha hit its weekly limit, which resets on Jan 3, while beta's window (no
// reset time: the configured cooldown) has already ended.
func TestACoolingAccountLosesEvenWhenItWouldWinTheRanking(t *testing.T) {
	marks := map[string]Mark{
		"alpha": {RateLimitedAt: t0.Add(-2 * time.Hour), RateLimitedDetail: "You've hit your weekly limit · resets Jan 3, 9am (UTC)"},
		"beta":  {RateLimitedAt: t0.Add(-time.Hour), RateLimitedDetail: "You've hit your limit"},
	}
	got, err := Choose(t0, testCooldown, []string{"alpha", "beta"}, tokens, marks, nil)
	if err != nil || got != "beta" {
		t.Fatalf("Choose = %q, %v; want beta (alpha cools down until Jan 3)", got, err)
	}
	// The control: once alpha's reset has passed, the ranking picks it again.
	got, err = Choose(time.Date(2000, 1, 3, 9, 0, 0, 0, time.UTC), testCooldown, []string{"alpha", "beta"}, tokens, marks, nil)
	if err != nil || got != "alpha" {
		t.Fatalf("after alpha's reset Choose = %q, %v; want alpha (rate-limited longest ago)", got, err)
	}
}

// TestCooldownUntilReadsTheResetTimeTheMessageCarries: the CLI states when the
// limit resets ("· resets 9:45pm (UTC)", "· resets Oct 9, 9am (UTC)"); the
// cooldown ends then. A message with no reset this can read — no zone, an
// unknown zone, a nonsense clock, a date further ahead than a weekly limit —
// falls back to the configured cooldown from the mark's time.
func TestCooldownUntilReadsTheResetTimeTheMessageCarries(t *testing.T) {
	dec30 := time.Date(2000, 12, 30, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		at     time.Time
		detail string
		want   time.Time
	}{
		{"no reset text", t0, "You've hit your limit", t0.Add(testCooldown)},
		{"empty detail", t0, "", t0.Add(testCooldown)},
		{"hour and minute, later today", t0, "You've hit your session limit · resets 9:45pm (UTC)",
			time.Date(2000, 1, 1, 21, 45, 0, 0, time.UTC)},
		{"hour only, later today", t0, "You've hit your session limit · resets 9pm (UTC)",
			time.Date(2000, 1, 1, 21, 0, 0, 0, time.UTC)},
		{"a clock time already past today is tomorrow", t0, "resets 9am (UTC)",
			time.Date(2000, 1, 2, 9, 0, 0, 0, time.UTC)},
		{"12am is midnight", t0, "resets 12am (UTC)", time.Date(2000, 1, 2, 0, 0, 0, 0, time.UTC)},
		{"12:30pm is half past noon", t0, "resets 12:30pm (UTC)", time.Date(2000, 1, 1, 12, 30, 0, 0, time.UTC)},
		{"a named zone", t0, "resets 5pm (America/New_York)", time.Date(2000, 1, 1, 22, 0, 0, 0, time.UTC)},
		{"a weekly limit's date", t0, "You've hit your weekly limit · resets Jan 3, 9am (UTC)",
			time.Date(2000, 1, 3, 9, 0, 0, 0, time.UTC)},
		{"a date across the new year", dec30, "resets Jan 2, 9am (UTC)", time.Date(2001, 1, 2, 9, 0, 0, 0, time.UTC)},
		{"a date further ahead than a weekly limit is not believed", t0, "resets Oct 9, 9am (UTC)", t0.Add(testCooldown)},
		{"no zone", t0, "resets 9pm", t0.Add(testCooldown)},
		{"an unknown zone", t0, "resets 9pm (Not/AZone)", t0.Add(testCooldown)},
		{"a nonsense clock", t0, "resets 25pm (UTC)", t0.Add(testCooldown)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CooldownUntil(Mark{RateLimitedAt: tc.at, RateLimitedDetail: tc.detail}, testCooldown)
			if !got.Equal(tc.want) {
				t.Fatalf("CooldownUntil(%q at %s) = %s, want %s", tc.detail, tc.at.Format(time.RFC3339),
					got.UTC().Format(time.RFC3339), tc.want.Format(time.RFC3339))
			}
		})
	}
	if got := CooldownUntil(Mark{}, testCooldown); !got.IsZero() {
		t.Fatalf("an account never rate-limited cools down until %s, want never (zero)", got)
	}
}

// TestStatesReportsTheCooldownAndTheCap: States is what the job API's worker
// choice reads, so it carries CoolingUntil — set only while the window is still
// open at the pool's clock, and zero the instant it has expired — and the
// configured per-account cap on every account.
func TestStatesReportsTheCooldownAndTheCap(t *testing.T) {
	now := t0
	st := &fakeStore{marks: map[string]Mark{
		"alpha": {RateLimitedAt: now.Add(-10 * time.Minute)},
		"beta":  {RateLimitedAt: now.Add(-testCooldown)}, // the window just expired
	}}
	src, err := NewStatic(tokens)
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewWithLimits(src, st, Limits{Cooldown: testCooldown, MaxConcurrentJobs: 3})
	if err != nil {
		t.Fatal(err)
	}
	p.now = func() time.Time { return now }
	got, err := p.States(context.Background(), opOwner)
	if err != nil {
		t.Fatal(err)
	}
	if want := now.Add(30 * time.Minute); !got["alpha"].CoolingUntil.Equal(want) {
		t.Errorf("alpha CoolingUntil = %s, want %s", got["alpha"].CoolingUntil, want)
	}
	for _, n := range []string{"beta", "gamma"} {
		if !got[n].CoolingUntil.IsZero() {
			t.Errorf("%s CoolingUntil = %s, want zero (not cooling down)", n, got[n].CoolingUntil)
		}
	}
	for _, n := range names {
		if got[n].MaxConcurrentJobs != 3 {
			t.Errorf("%s MaxConcurrentJobs = %d, want 3 (the configured cap)", n, got[n].MaxConcurrentJobs)
		}
	}
}

// TestSelectRefusesWhileEveryAccountIsCoolingDownButAPinStillWins: through the
// pool and its clock. A pin stays the operator's override.
func TestSelectRefusesWhileEveryAccountIsCoolingDownButAPinStillWins(t *testing.T) {
	now := t0
	marks := map[string]Mark{}
	for _, n := range names {
		marks[n] = Mark{RateLimitedAt: now.Add(-5 * time.Minute), RateLimitedDetail: "You've hit your limit"}
	}
	p := newPool(t, &fakeStore{marks: marks})
	p.now = func() time.Time { return now }
	if got, err := p.Select(context.Background(), opOwner, ""); !errors.Is(err, ErrNoUsableAccount) {
		t.Fatalf("Select = %q, %v; want ErrNoUsableAccount (every account is cooling down)", got, err)
	}
	if got, err := p.Select(context.Background(), opOwner, "beta"); err != nil || got != "beta" {
		t.Fatalf("Select(pin beta) = %q, %v; want beta", got, err)
	}
	p.now = func() time.Time { return now.Add(DefaultCooldown) }
	if got, err := p.Select(context.Background(), opOwner, ""); err != nil || got != "alpha" {
		t.Fatalf("after the default cooldown Select = %q, %v; want alpha", got, err)
	}
}

// TestNewWithLimitsRefusesAKnobThatWouldDisableItsGuard.
func TestNewWithLimitsRefusesAKnobThatWouldDisableItsGuard(t *testing.T) {
	src, _ := NewStatic(tokens)
	for _, l := range []Limits{{Cooldown: 0, MaxConcurrentJobs: 1}, {Cooldown: -time.Minute, MaxConcurrentJobs: 1}, {Cooldown: time.Minute, MaxConcurrentJobs: 0}} {
		if _, err := NewWithLimits(src, &fakeStore{}, l); err == nil {
			t.Errorf("NewWithLimits accepted %+v", l)
		}
	}
	if _, err := NewWithLimits(src, &fakeStore{}, Limits{Cooldown: time.Minute, MaxConcurrentJobs: 1}); err != nil {
		t.Fatalf("the control was refused: %v", err)
	}
}
