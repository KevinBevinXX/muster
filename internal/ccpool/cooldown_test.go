package ccpool

import (
	"context"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// THE RATE-LIMIT COOLDOWN AND THE PER-ACCOUNT CAP (M4). This package computes
// the cooldown window and carries it (and the opt-in cap) to the job API's
// worker choice, which refuses for them. Agent dispatch (Choose/Select) only
// ranks a cooling account last and never refuses for it.
// ---------------------------------------------------------------------------

// testCooldown is deliberately NOT DefaultCooldown, so an implementation that
// hard-codes the default instead of reading the configured value shows.
const testCooldown = 40 * time.Minute

// TestTheCooldownWindowEndsExactlyAtItsEnd: alpha's message names no reset
// time, so its window is the configured cooldown. coolingUntil (what States
// hands the job API's worker choice) reports it cooling for the whole window —
// up to one nanosecond before the end — and not at all from the instant the
// window has expired.
func TestTheCooldownWindowEndsExactlyAtItsEnd(t *testing.T) {
	m := Mark{RateLimitedAt: t0, RateLimitedDetail: "You've hit your limit"}
	end := t0.Add(testCooldown)
	for _, tc := range []struct {
		name    string
		now     time.Time
		cooling bool
	}{
		{"the moment it was marked", t0, true},
		{"mid-window", t0.Add(25 * time.Minute), true},
		{"one nanosecond before the window ends", end.Add(-time.Nanosecond), true},
		{"the window just expired", end, false},
		{"long after", t0.Add(3 * time.Hour), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := coolingUntil(m, testCooldown, tc.now)
			if tc.cooling && !got.Equal(end) {
				t.Fatalf("coolingUntil = %s, want %s (still cooling down)", got, end)
			}
			if !tc.cooling && !got.IsZero() {
				t.Fatalf("coolingUntil = %s, want zero (the window is over)", got)
			}
		})
	}
}

// TestChooseStillReturnsACoolingAccountWhenItIsTheOnlyOne: agent dispatch
// never refuses for a cooldown — an agent keeps its account for life, so a
// window of hours is no reason not to create it. alpha alone, mid-window, is
// chosen; so is every account when all of them are cooling down.
func TestChooseStillReturnsACoolingAccountWhenItIsTheOnlyOne(t *testing.T) {
	marks := map[string]Mark{"alpha": {RateLimitedAt: t0, RateLimitedDetail: "You've hit your limit"}}
	if got, err := Choose(t0.Add(time.Minute), testCooldown, []string{"alpha"}, tokens, marks, nil); err != nil || got != "alpha" {
		t.Fatalf("Choose = %q, %v; want alpha (cooling down, but the only account)", got, err)
	}
	all := map[string]Mark{}
	for _, n := range names {
		all[n] = Mark{RateLimitedAt: t0.Add(-time.Minute), RateLimitedDetail: "You've hit your limit"}
	}
	all["beta"] = Mark{RateLimitedAt: t0.Add(-2 * time.Minute), RateLimitedDetail: "You've hit your limit"}
	if got, err := Choose(t0, testCooldown, names, tokens, all, nil); err != nil || got != "beta" {
		t.Fatalf("every account cooling: Choose = %q, %v; want beta (rate-limited longest ago), not a refusal", got, err)
	}
}

// TestACoolingAccountLosesEvenWhenItWouldWinTheRanking: alpha was rate-limited
// LONGER ago than beta, so the least-recently-limited rule alone picks it —
// but alpha hit its weekly limit, which resets on Jan 3, while beta's window
// (no reset time: the configured cooldown) has already ended. A cooling
// account ranks after every account that is not.
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
// configured per-account cap on every account; with no cap configured (the
// default pool) the cap is zero, which is none.
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
	def, err := newPool(t, st).States(context.Background(), opOwner)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if def[n].MaxConcurrentJobs != 0 {
			t.Errorf("default pool: %s MaxConcurrentJobs = %d, want 0 (the cap is opt-in: none unless configured)", n, def[n].MaxConcurrentJobs)
		}
	}
}

// TestSelectNeverRefusesForACooldownAndRanksCoolingAccountsLast: through the
// pool and its clock. With every account cooling, Select still answers (the
// one limited longest ago); with one account rested, that one wins although it
// is last by name; a pin stays the operator's override.
func TestSelectNeverRefusesForACooldownAndRanksCoolingAccountsLast(t *testing.T) {
	now := t0
	marks := map[string]Mark{
		"alpha": {RateLimitedAt: now.Add(-5 * time.Minute), RateLimitedDetail: "You've hit your limit"},
		"beta":  {RateLimitedAt: now.Add(-6 * time.Minute), RateLimitedDetail: "You've hit your limit"},
		"gamma": {RateLimitedAt: now.Add(-7 * time.Minute), RateLimitedDetail: "You've hit your limit"},
	}
	p := newPool(t, &fakeStore{marks: marks})
	p.now = func() time.Time { return now }
	if got, err := p.Select(context.Background(), opOwner, ""); err != nil || got != "gamma" {
		t.Fatalf("every account cooling: Select = %q, %v; want gamma (limited longest ago), not a refusal", got, err)
	}
	// Now gamma is rested (a day-old mark, no reset text) while alpha and beta
	// are cooling — beta on a weekly limit with the OLDEST mark, so the
	// least-recently-limited rule alone would pick beta. Cooling ranks last.
	marks["gamma"] = Mark{RateLimitedAt: now.Add(-24 * time.Hour)}
	marks["beta"] = Mark{RateLimitedAt: now.Add(-48 * time.Hour), RateLimitedDetail: "You've hit your weekly limit · resets Jan 3, 9am (UTC)"}
	if got, err := p.Select(context.Background(), opOwner, ""); err != nil || got != "gamma" {
		t.Fatalf("Select = %q, %v; want gamma (the only account not cooling down)", got, err)
	}
	if got, err := p.Select(context.Background(), opOwner, "alpha"); err != nil || got != "alpha" {
		t.Fatalf("Select(pin alpha) = %q, %v; want alpha", got, err)
	}
}

// TestNewWithLimitsRefusesAKnobThatWouldDisableItsGuard, and accepts zero as no cap.
func TestNewWithLimitsRefusesAKnobThatWouldDisableItsGuard(t *testing.T) {
	src, _ := NewStatic(tokens)
	for _, l := range []Limits{{Cooldown: 0, MaxConcurrentJobs: 1}, {Cooldown: -time.Minute, MaxConcurrentJobs: 1}, {Cooldown: time.Minute, MaxConcurrentJobs: -1}} {
		if _, err := NewWithLimits(src, &fakeStore{}, l); err == nil {
			t.Errorf("NewWithLimits accepted %+v", l)
		}
	}
	// Zero is "no cap", and is accepted; so is a cap.
	for _, l := range []Limits{{Cooldown: time.Minute}, {Cooldown: time.Minute, MaxConcurrentJobs: 1}} {
		if _, err := NewWithLimits(src, &fakeStore{}, l); err != nil {
			t.Fatalf("NewWithLimits(%+v) was refused: %v", l, err)
		}
	}
	if got := DefaultLimits(); got != (Limits{Cooldown: time.Hour}) {
		t.Fatalf("DefaultLimits = %+v, want a 1h cooldown and NO cap", got)
	}
}
