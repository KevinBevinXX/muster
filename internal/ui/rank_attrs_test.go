package ui

import (
	"strings"
	"testing"
)

// TestCardsCarryTheirRankAttributes pins what the filter's rank rules read
// (rankAttrs): every title as a newline-led entry, lower-cased, and every
// suffix of a title that starts a word — and nothing a rule could match across
// two titles or inside a word. One agent card and one mirrored card, rendered.
func TestCardsCarryTheirRankAttributes(t *testing.T) {
	agent := renderNode(t, agentCard(AgentCardView{ID: 3, Name: "docs-sweeper", DisplayName: "Gold Docs", Status: "running"}))
	mirror := renderNode(t, mirrorCard(MirrorCardView{Host: "tower", Session: "old-qdock", Title: "zsh", Online: true}))
	for _, c := range []struct {
		name, html  string
		title       []string // the data-rank-title entries, in order
		words, nots []string // entries that must / must not be in data-rank-words
	}{
		{"agent", agent, []string{"gold docs", "docs-sweeper"}, []string{"gold docs", "docs", "docs-sweeper", "sweeper"}, []string{"weeper", "ocs"}},
		{"mirror", mirror, []string{"zsh", "old-qdock"}, []string{"old-qdock", "qdock"}, []string{"dock", "ld-qdock"}},
	} {
		title := attrValue(c.html, `data-rank-title="`)
		words := attrValue(c.html, `data-rank-words="`)
		if want := "\n" + strings.Join(c.title, "\n"); title != want {
			t.Errorf("%s: data-rank-title = %q, want %q", c.name, title, want)
		}
		got := map[string]bool{}
		for _, w := range strings.Split(words, "\n") {
			got[w] = true
		}
		for _, w := range c.words {
			if !got[w] {
				t.Errorf("%s: data-rank-words lacks the word start %q (%q)", c.name, w, words)
			}
		}
		for _, w := range c.nots {
			if got[w] {
				t.Errorf("%s: data-rank-words has %q, which does not start a word", c.name, w)
			}
		}
	}
}
