package ui

import (
	"strings"
	"testing"
)

// TestCardsCarryTheirRankAttributes pins what the filter's ranking reads
// (rankAttrs → agentsFilterScript's reorder): every title the card is called
// by, newline-led, as the card spells it (window.musterRank folds case and
// accents itself) — and nothing a rule could match across two titles.
func TestCardsCarryTheirRankAttributes(t *testing.T) {
	agent := renderNode(t, agentCard(AgentCardView{ID: 3, Name: "docs-sweeper", DisplayName: "Gold  Docs", Status: "running"}))
	mirror := renderNode(t, mirrorCard(MirrorCardView{Host: "tower", Session: "old-qdock", Title: "zsh", Online: true}))
	for _, c := range []struct {
		name, html string
		titles     []string
	}{
		{"agent", agent, []string{"Gold Docs", "docs-sweeper"}},
		{"mirror", mirror, []string{"zsh", "old-qdock"}},
	} {
		if got, want := attrValue(c.html, `data-rank-title="`), "\n"+strings.Join(c.titles, "\n"); got != want {
			t.Errorf("%s: data-rank-title = %q, want %q", c.name, got, want)
		}
		if strings.Contains(c.html, "data-rank-words") {
			t.Errorf("%s: still carries data-rank-words, which nothing reads since the ranking is window.musterRank", c.name)
		}
	}
}
