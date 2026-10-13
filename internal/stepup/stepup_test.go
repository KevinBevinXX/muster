package stepup

import (
	"context"
	"testing"
	"time"
)

// TestFreshIsWithinMaxAgeOfTheSignInAndUnknownIsStale: the boundary is
// inclusive (exactly MaxAge is fresh), one second past is stale, and a context
// with no sign-in time is stale — the points measured are both sides of the
// boundary, the middle, and absence.
func TestFreshIsWithinMaxAgeOfTheSignInAndUnknownIsStale(t *testing.T) {
	signedIn := time.Unix(1_800_000_000, 0)
	ctx := WithAuthTime(context.Background(), signedIn)
	for _, tc := range []struct {
		after time.Duration
		want  bool
	}{
		{0, true}, {2*time.Minute + 17*time.Second, true}, {MaxAge, true}, {MaxAge + time.Second, false}, {time.Hour, false},
	} {
		if got := Fresh(ctx, signedIn.Add(tc.after)); got != tc.want {
			t.Errorf("signed in %v ago: Fresh = %v, want %v", tc.after, got, tc.want)
		}
	}
	if Fresh(context.Background(), signedIn) {
		t.Error("a context with no sign-in time is fresh")
	}
	if Fresh(WithAuthTime(context.Background(), time.Time{}), signedIn) {
		t.Error("a zero sign-in time is fresh")
	}
	if got := FreshUntil(ctx); !got.Equal(signedIn.Add(MaxAge)) {
		t.Errorf("FreshUntil = %v", got)
	}
	if !FreshUntil(context.Background()).IsZero() {
		t.Error("FreshUntil of no sign-in is not zero")
	}
}
