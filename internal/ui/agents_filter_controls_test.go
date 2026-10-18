package ui

import (
	"html"
	"regexp"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// ---------------------------------------------------------------------------
// THE FILTER'S CONTROLS: host tabs built from the cards, and the search box.
// ---------------------------------------------------------------------------

// facetFixture is a list whose every field value is pairwise distinct across
// fields and cards, with one DUPLICATE per field (two "tower" windows, two
// running cards, two opencode panes). Nothing in it spells "cluster" except the
// pod agents' host, which is the literal podHost.
func facetFixture() ([]AgentCardView, []MirrorCardView) {
	list := []AgentCardView{
		{ID: 1, Name: "keen-wren", Status: "running", Kind: agents.KindClaudeCode, CCAccount: "Alpha"},
		{ID: 2, Name: "slow-heron", Status: "error"},
	}
	mirrors := []MirrorCardView{
		{Host: "Tower", Session: "s1", Window: "@1", Kind: "opencode", Online: true},
		{Host: "Tower", Session: "s1", Window: "@2", Kind: "opencode", Online: true},
		{Host: "laptop", Session: "s2", Kind: "shell", Online: false},
	}
	return list, mirrors
}

// TestTheCardsCarryTheirHostLabel: the script builds a host TAB off the cards
// — its value from data-f-host, what it says from data-l-host — so a host that
// lower-casing changed carries its spelling as the card writes it, and one it
// did not change carries none. Only the host is labelled now: the type, status
// and kind dropdowns that read data-l-type/-kind are gone, and a label nothing
// reads is dead weight on every card.
func TestTheCardsCarryTheirHostLabel(t *testing.T) {
	list, mirrors := facetFixture()
	cards := renderNode(t, AgentsCardsWithMirrors(list, mirrors))
	attr := func(card, name string) (string, bool) {
		m := regexp.MustCompile(` ` + regexp.QuoteMeta(name) + `="([^"]*)"`).FindStringSubmatch(card)
		if m == nil {
			return "", false
		}
		return html.UnescapeString(m[1]), true
	}
	tower := articleWith(t, cards, `data-mirror-card`)
	if v, _ := attr(tower, "data-f-host"); v != "tower" {
		t.Errorf("data-f-host = %q, want tower", v)
	}
	if l, _ := attr(tower, "data-l-host"); l != "Tower" {
		t.Errorf("data-l-host = %q, want Tower", l)
	}
	// A pod agent is on host "cluster", already lower-case: no label.
	agent := articleWith(t, cards, `id="agent-1"`)
	if v, _ := attr(agent, "data-f-host"); v != podHost {
		t.Errorf("a pod agent's data-f-host = %q, want %q", v, podHost)
	}
	if l, ok := attr(agent, "data-l-host"); ok {
		t.Errorf("an already lower-case host carries data-l-host=%q", l)
	}
	// Its kind changed under lower-casing ("Claude Code") and is still a field,
	// but is not labelled any more.
	if v, _ := attr(agent, "data-f-kind"); v != "claude code" {
		t.Fatalf("instrument check: the agent's data-f-kind = %q, want claude code", v)
	}
	for _, f := range []string{"data-l-kind", "data-l-type", "data-l-status", "data-l-account", "data-l-name", "data-l-session"} {
		if strings.Contains(cards, f) {
			t.Errorf("a card carries %s, which nothing reads:\n%s", f, cards)
		}
	}
	// The list's partial is the cards and the recent strip, and nothing else
	// (no out-of-band tabs: the script builds them from the cards).
	var b strings.Builder
	if err := RenderAgentsList(&b, list, mirrors, nil); err != nil {
		t.Fatal(err)
	}
	if b.String() != cards+renderNode(t, recentStrip(recentItems(list, mirrors, nil), true)) {
		t.Error("the /ui/agents partial is not exactly the cards and the strip")
	}
}

// articleWith is the <article> in markup that contains marker.
func articleWith(t *testing.T, markup, marker string) string {
	t.Helper()
	i := strings.Index(markup, marker)
	if i < 0 {
		t.Fatalf("no card with %q in:\n%s", marker, markup)
	}
	start := strings.LastIndex(markup[:i], "<article")
	rest := markup[start:]
	if j := strings.Index(rest, "</article>"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// TestEveryLabelledFieldIsARegisteredField: a label for a field the registry
// does not know would never be written.
func TestEveryLabelledFieldIsARegisteredField(t *testing.T) {
	known := map[string]bool{}
	for _, f := range cardFieldRegistry {
		known[f.Key] = true
	}
	for _, f := range labelledFields {
		if !known[f] {
			t.Errorf("labelled field %q is not in cardFieldRegistry", f)
		}
	}
}

// TestTheBarHasHostTabsAndNoDropdowns: the four facet <select>s are gone; in
// their place, ABOVE the box and OUTSIDE #agents-list (a refresh never touches
// it), is a tablist whose one server-drawn tab is "All" — selected, in the
// roving tabindex, controlling the list, and drawn with the app's tab look
// (tabClass, which the script's clones inherit). The strip scrolls sideways
// inside itself and is one 44px row however many tabs it holds.
func TestTheBarHasHostTabsAndNoDropdowns(t *testing.T) {
	panel := renderNode(t, AgentsPanel(true))
	list := strings.Index(panel, `id="agents-list"`)
	tag := func(id string) string {
		i := strings.Index(panel, `id="`+id+`"`)
		if i < 0 {
			t.Fatalf("the Agents panel has no #%s", id)
		}
		if i > list {
			t.Errorf("#%s is inside or after #agents-list", id)
		}
		start := strings.LastIndex(panel[:i], "<")
		return panel[start : i+strings.Index(panel[i:], ">")]
	}
	for _, gone := range []string{"<select", `id="agents-f-`, "agents-filter-controls", "agents-filter-clear", "data-filter-field"} {
		if strings.Contains(panel, gone) {
			t.Errorf("the Agents panel still has %q", gone)
		}
	}
	strip := tag("agents-hosts")
	for _, want := range []string{`role="tablist"`, `aria-label="Hosts"`, "overflow-x-auto", "flex-nowrap", "min-h-[44px]"} {
		if !strings.Contains(strip, want) {
			t.Errorf("the host strip lacks %q: %s", want, strip)
		}
	}
	all := tag("agents-host-all")
	for _, want := range []string{`role="tab"`, `data-host=""`, `aria-controls="agents-list"`, `aria-selected="true"`, `tabindex="0"`, `class="` + tabClass + `"`} {
		if !strings.Contains(all, want) {
			t.Errorf("the All tab lacks %q: %s", want, all)
		}
	}
	if strings.Index(panel, `id="agents-hosts"`) > strings.Index(panel, `id="agents-search"`) {
		t.Error("the host tabs are below the search box, not above it")
	}
	i := strings.Index(panel, `id="agents-host-all"`)
	rest := panel[i:]
	rest = rest[:strings.Index(rest, "</button>")]
	if !strings.Contains(rest, `<span data-tab-label="">All</span>`) || !strings.Contains(rest, `data-tab-count=""`) {
		t.Errorf("the All tab has no label/count slots for its clones: %s", rest)
	}
	tag("agents-count")
	if count := tag("agents-count"); strings.Contains(count, "text-xs") {
		t.Errorf("the count is still the small muted print: %s", count)
	}
}

// TestTheHostTabIsAnExactTermAndedWithTheBox pins the shape of the script's
// host filter, so a rewrite cannot quietly make the tab a substring match (a
// host "mac" showing "macbook"), a filter of its own instead of one more ANDed
// term, part of ?q=, or built from held arrivals. Behaviour: e2e/tests/host-tabs.spec.ts.
func TestTheHostTabIsAnExactTermAndedWithTheBox(t *testing.T) {
	script := scriptWith(t, renderString(t, Page("agents", featuresAllOn)), "__musterAgentsFilter")
	for name, want := range map[string]string{
		"the tab is one more EXACT term, ANDed":   "var all_ = host ? ts.concat([{ attr: 'data-f-host', value: host, exact: true }]) : ts;",
		"every term hides":                        "style.textContent = ['#agents-list > article[data-held]{display:none !important}'].concat(all_.map(",
		"the box alone is ?q= and the session":    "var q = input.value;",
		"tabs come from the DRAWN cards":          "var drawn = list.querySelectorAll(':scope > article:not([data-held])');",
		"the choice is stored under tabs' prefix": "var HOST_KEY = 'muster:tabs:agents-host';",
		"an absent host SHOWS All":                "var on = h && seen[h] ? h : '';",
		"the hold reads the filter":               "var h = heldOrder.filter(function (id) { return inLive[id] && shows(byId(id)); });",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("%s: the filter script no longer contains %q", name, want)
		}
	}
	if strings.Contains(script, "syncURL(q + ") || strings.Contains(script, "host=' + ") {
		t.Error("the host tab is written into ?q=")
	}
}
