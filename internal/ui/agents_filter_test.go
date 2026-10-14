package ui

import (
	"regexp"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

var dataFilter = regexp.MustCompile(` data-filter="([^"]*)"`)

func haystackOf(t *testing.T, card string) string {
	t.Helper()
	m := dataFilter.FindStringSubmatch(card)
	if m == nil {
		t.Fatalf("the card carries no data-filter:\n%s", card)
	}
	return m[1]
}

// TestEveryCardCanBeFoundByWhatItIs: each card carries, lower-cased, what the
// filter matches it by. A mirrored window: its label ("Gold:8"), its raw tmux
// session name, its host, what runs in it, and "mirrored". An agent: its
// display name, its slug, its repository, its kind, its account, its status.
// The values are pairwise distinct, so a card found by the wrong field fails.
func TestEveryCardCanBeFoundByWhatItIs(t *testing.T) {
	win := haystackOf(t, renderNode(t, mirrorCard(MirrorCardView{Host: "Tower", Session: "Scratch2", Window: "@7", Label: "Gold:8",
		Kind: "opencode", Online: true, Interactive: true})))
	for _, want := range []string{"gold:8", "scratch2", "tower", "opencode", "mirrored", "interactive"} {
		if !strings.Contains(win, want) {
			t.Errorf("a mirrored window's haystack %q lacks %q", win, want)
		}
	}
	if win != strings.ToLower(win) || strings.Contains(win, "offline") {
		t.Errorf("the haystack is not lower-case, or calls an online card offline: %q", win)
	}
	// A Claude Code window is found by "claude code" whatever its command is, an
	// offline one by "offline", and a window with no codename by "<session>:<index>".
	cc := haystackOf(t, renderNode(t, mirrorCard(MirrorCardView{Host: "tower", Session: "solo", Window: "@2", Title: "solo:3", Kind: "node", Claude: true})))
	for _, want := range []string{"claude code", "solo:3", "offline"} {
		if !strings.Contains(cc, want) {
			t.Errorf("the haystack %q lacks %q", cc, want)
		}
	}
	if strings.Contains(cc, "node") {
		t.Errorf("a Claude Code window is filed under its command: %q", cc)
	}

	agent := haystackOf(t, renderNode(t, agentCard(AgentCardView{ID: 7, Name: "keen-wren", DisplayName: "#12 · Fix The Chip", Repo: "Acme/Widgets",
		Kind: agents.KindClaudeCode, CCAccount: "Alpha", Status: "running"})))
	for _, want := range []string{"#12 · fix the chip", "keen-wren", "acme/widgets", "claude code", "alpha", "running", "agent"} {
		if !strings.Contains(agent, want) {
			t.Errorf("an agent's haystack %q lacks %q", agent, want)
		}
	}
	if strings.Contains(agent, "mirrored") {
		t.Errorf("an agent is filed as mirrored: %q", agent)
	}
}

// TestTheFilterBoxIsOutsideTheListItFilters: the search box, its clear button
// and its count are in the Agents panel but NOT inside #agents-list — the list
// re-renders itself (an htmx morph) on every nudge, and an input inside it
// would lose its focus and caret each time. And the box is phone-shaped: a 16px
// input (iOS zooms the page on focusing a smaller one) and 44px targets.
func TestTheFilterBoxIsOutsideTheListItFilters(t *testing.T) {
	panel := renderNode(t, AgentsPanel(true))
	list := strings.Index(panel, `id="agents-list"`)
	for _, id := range []string{"agents-search", "agents-search-clear", "agents-count", "agents-nomatch"} {
		i := strings.Index(panel, `id="`+id+`"`)
		if i < 0 {
			t.Fatalf("the Agents panel has no #%s", id)
		}
		if i > list {
			t.Errorf("#%s is drawn after #agents-list opens — inside or below what a refresh re-renders", id)
		}
	}
	tag := func(id string) string {
		i := strings.Index(panel, `id="`+id+`"`)
		start := strings.LastIndex(panel[:i], "<")
		return panel[start : i+strings.Index(panel[i:], ">")]
	}
	input := tag("agents-search")
	for _, want := range []string{" text-base ", "min-h-[44px]", `aria-controls="agents-list"`, `type="text"`} {
		if !strings.Contains(input, want) {
			t.Errorf("the filter input lacks %q: %s", want, input)
		}
	}
	if strings.Contains(input, "text-sm") || strings.Contains(input, "text-xs") {
		t.Errorf("the filter input is smaller than 16px: %s", input)
	}
	// The clear button is hidden by ATTRIBUTE until there is something to
	// clear, so it must carry no display utility that would beat it.
	clear := tag("agents-search-clear")
	if !strings.Contains(clear, ` hidden=""`) {
		t.Errorf("the clear button is not hidden while the box is empty: %s", clear)
	}
	for _, display := range []string{"inline-flex", " flex ", " grid ", " block "} {
		if strings.Contains(clear, display) {
			t.Errorf("the clear button carries %q, which beats its hidden attribute: %s", display, clear)
		}
	}
	if !strings.Contains(clear, "h-11") || !strings.Contains(clear, "w-11") {
		t.Errorf("the clear button is not a 44px target: %s", clear)
	}
}

// TestTheFilterHidesWithOneRulePerWordAndCountsWithTheSameSelector pins the two
// decisions the script is written around, where a rewrite would lose them:
// the hiding is a stylesheet rule over data-filter (so the list's own refresh
// cannot undo it), and the count is taken with the SAME selector fragment (so
// what is counted is what is shown).
func TestTheFilterHidesWithOneRulePerWordAndCountsWithTheSameSelector(t *testing.T) {
	page := renderString(t, Page("agents", featuresAllOn))
	script := scriptWith(t, page, "__musterAgentsFilter")
	for name, want := range map[string]string{
		"the rule hides a card lacking the word": `'#agents-list > article:not(' + has(w) + '){display:none !important}'`,
		"the count uses the same selector":       `list.querySelectorAll(':scope > article' + ws.map(has).join(''))`,
		"the words are lower-cased and split":    `toLowerCase().split(/\s+/).filter(Boolean)`,
		"it re-counts when the list settles":     `if (e.target && e.target.id === 'agents-list') apply();`,
		"it remembers the text for the session":  `sessionStorage.setItem(KEY, input.value)`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("%s: the filter script no longer contains %s", name, want)
		}
	}
	if strings.Count(page, "__musterAgentsFilter = true") != 1 {
		t.Error("the filter script is not rendered exactly once on the shell")
	}
	// It never marks a card itself: nothing for a morph to undo.
	if strings.Contains(script, ".hidden = true") || strings.Contains(script, "setAttribute('hidden'") {
		t.Error("the filter script hides cards one by one; a list refresh would un-hide them")
	}
}
