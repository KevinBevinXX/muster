package ui

import (
	"strings"
	"testing"
)

// The cut never splits a grapheme cluster: a family emoji (ZWJ sequence), a
// flag (regional-indicator pair), a combining accent and a skin-tone modifier
// each straddling the 60th rune are dropped whole, and a flag pair that ENDS
// at the budget is kept. clampTitle is the cards' clamp too, so this covers
// them as well.
func TestTheTitleClampNeverSplitsAGraphemeCluster(t *testing.T) {
	a59 := strings.Repeat("a", 59)
	tail := strings.Repeat(" more words", 10)
	cases := []struct{ name, in, want string }{
		{"family emoji across 60", a59 + "👨‍👩‍👧" + tail, a59},
		{"flag across 60", a59 + "🇨🇦" + tail, a59},
		{"combining accent at 61", a59 + "é" + tail, a59},
		{"skin-tone modifier at 61", a59 + "👍🏽" + tail, a59},
		{"flag pair ending at 60 is kept", strings.Repeat("a", 58) + "🇨🇦🇫🇷" + tail, strings.Repeat("a", 58) + "🇨🇦"},
		{"plain text cuts at 60", strings.Repeat("b", 61) + tail, strings.Repeat("b", 60)},
	}
	for _, c := range cases {
		got, clamped := clampTitle(c.in)
		if !clamped || got != c.want {
			t.Errorf("%s: clampTitle = %q (clamped=%v), want %q", c.name, got, clamped, c.want)
		}
	}
	// Through the agent page header, where the operator saw it.
	page := detailPage(t, cases[0].in, false)
	if got := agentHeaderTitleText(t, page); got != a59 {
		t.Errorf("the header draws %q, want the title cut before the family emoji", got)
	}
}
