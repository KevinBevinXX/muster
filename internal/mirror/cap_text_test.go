package mirror_test

import (
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestTheCapRefusalsSayHowToFreeOne pins the per-host cap refusals, which a
// person reads in the view that was refused, WHOLE: each says how to free one —
// a view left open in another tab or window may be what holds it, now that a
// window keeps both of its views open for as long as it stays open.
func TestTheCapRefusalsSayHowToFreeOne(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{mirror.ErrTooManyStreams.Error(), "mirror: this host already has 8 terminals open; close one first — a view left open in another tab or window may hold one: close that window"},
		{mirror.ErrTooManyWatches.Error(), "mirror: this host already has 8 rendered views open; close one first — a view left open in another tab or window may hold one: close that window"},
	} {
		if c.got != c.want {
			t.Errorf("refusal = %q\nwant      %q", c.got, c.want)
		}
	}
}
