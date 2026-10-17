package ui

import (
	"html"
	"regexp"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// ---------------------------------------------------------------------------
// THE FILTER'S CONTROLS: one select per facet, options from the cards.
// ---------------------------------------------------------------------------

// facetFixture is a list whose every facet value is pairwise distinct across
// fields and cards, with one DUPLICATE per field (two "tower" windows, two
// running cards, two opencode panes) so a facet that lists a value per card
// rather than per value is red. Nothing in it spells "cluster" except the pod
// agents' host, which is the literal podHost.
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

// TestTheCardsCarryTheirOptionsLabels: the script reads a select's options off
// the cards — the value from data-f-<field>, what the option says from
// data-l-<field> — so a facet value that lower-casing changed carries its
// spelling as the card writes it, and one it did not change carries none. Only
// facet fields get a label (nothing reads one elsewhere). Red before: no card
// carried a data-l-* attribute.
func TestTheCardsCarryTheirOptionsLabels(t *testing.T) {
	list, mirrors := facetFixture()
	cards := renderNode(t, AgentsCardsWithMirrors(list, mirrors))
	attr := func(card, name string) (string, bool) {
		m := regexp.MustCompile(` ` + regexp.QuoteMeta(name) + `="([^"]*)"`).FindStringSubmatch(card)
		if m == nil {
			return "", false
		}
		return html.UnescapeString(m[1]), true
	}
	for _, c := range []struct {
		marker, field, value, label string
	}{
		{`id="agent-1"`, "kind", "claude code", "Claude Code"},
		{`id="agent-1"`, "type", "agent", "Agent"},
		{`id="agent-2"`, "kind", "standard", "Standard"},
		{`data-mirror-card`, "host", "tower", "Tower"},
		{`data-mirror-card`, "type", "mirrored", "Mirrored"},
	} {
		card := articleWith(t, cards, c.marker)
		if v, _ := attr(card, "data-f-"+c.field); v != c.value {
			t.Errorf("%s: data-f-%s = %q, want %q", c.marker, c.field, v, c.value)
		}
		if l, _ := attr(card, "data-l-"+c.field); l != c.label {
			t.Errorf("%s: data-l-%s = %q, want %q", c.marker, c.field, l, c.label)
		}
	}
	// A value lower-casing left alone carries no label: the option says the value.
	laptop := articleWith(t, cards, `data-f-host="laptop"`)
	if l, ok := attr(laptop, "data-l-host"); ok {
		t.Errorf("an already lower-case host carries data-l-host=%q", l)
	}
	if v, _ := attr(laptop, "data-f-kind"); v != "shell" {
		t.Fatalf("instrument check: the laptop card's data-f-kind = %q, want shell", v)
	}
	// A non-facet field is never labelled, whatever its case (account "Alpha").
	if strings.Contains(cards, "data-l-account") || strings.Contains(cards, "data-l-name") || strings.Contains(cards, "data-l-session") {
		t.Errorf("a non-facet field carries a label:\n%s", cards)
	}
	if !strings.Contains(cards, `data-f-account="alpha"`) {
		t.Fatalf("instrument check: the account field is not on the card at all:\n%s", cards)
	}
	// The list's partial is the cards and the recent strip, and nothing else
	// (no out-of-band options).
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

// TestEveryFacetIsARegisteredField: a facet the registry does not know would be
// a select whose token the script treats as free text.
func TestEveryFacetIsARegisteredField(t *testing.T) {
	known := map[string]bool{}
	for _, f := range cardFieldRegistry {
		known[f.Key] = true
	}
	for _, f := range facetFields {
		if !known[f] {
			t.Errorf("facet %q is not in cardFieldRegistry", f)
		}
	}
}

// TestTheBarHasAControlPerFacetOutsideTheList: one select per facet, its first
// option selecting nothing, each a 16px 44px target; "Clear all" and the
// count — all OUTSIDE #agents-list (a refresh never touches
// them). "Clear all" is hidden by ATTRIBUTE, so it carries no display utility.
func TestTheBarHasAControlPerFacetOutsideTheList(t *testing.T) {
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
	for _, f := range facetFields {
		s := tag("agents-f-" + f)
		for _, want := range []string{"<select", `data-filter-field="` + f + `"`, " text-base ", "min-h-[44px]", `aria-label="Filter by ` + f + `"`} {
			if !strings.Contains(s, want) {
				t.Errorf("the %s control lacks %q: %s", f, want, s)
			}
		}
		i := strings.Index(panel, `id="agents-f-`+f+`"`)
		first := regexp.MustCompile(`<option value="">([^<]*)</option>`).FindStringSubmatch(panel[i:])
		if first == nil || first[1] != facetAny[f] {
			t.Errorf("the %s control's first option is not %q", f, facetAny[f])
		}
	}
	tag("agents-count")
	clear := tag("agents-filter-clear")
	if !strings.Contains(clear, ` hidden=""`) || !strings.Contains(clear, "min-h-[44px]") {
		t.Errorf("Clear all is not a hidden 44px target: %s", clear)
	}
	for _, display := range []string{"inline-flex", " flex ", " grid ", " block "} {
		if strings.Contains(clear, display) {
			t.Errorf("Clear all carries %q, which beats its hidden attribute: %s", display, clear)
		}
	}
	if count := tag("agents-count"); strings.Contains(count, "text-xs") {
		t.Errorf("the count is still the small muted print: %s", count)
	}
}

// TestTheControlsWriteTheSameTokenString pins where the script's ONE string
// comes from and goes, so a rewrite cannot quietly give the controls a filter
// of their own: the hide rules are built from composeQ, sessionStorage and the
// URL are written with it, and a ?q= is split back into the controls.
func TestTheControlsWriteTheSameTokenString(t *testing.T) {
	script := scriptWith(t, renderString(t, Page("agents", featuresAllOn)), "__musterAgentsFilter")
	for name, want := range map[string]string{
		"the hide rules read the one string":      "var q = composeQ();\n    var ts = terms(q);",
		"it is remembered as is":                  "sessionStorage.setItem(KEY, q)",
		"it is the URL's ?q=":                     "syncURL(q);",
		"a link's ?q= is split into the controls": "} else distribute(q);",
		"the options come from the drawn cards":   "list.querySelectorAll(':scope > article[data-f-' + f + ']:not([data-held])')",
		"a select writes an EXACT token":          "return /[\\s\"\\\\]/.test(v) ? f + '=\"' + v.replace(/[\"\\\\]/g, '\\\\$&') + '\"' : f + '=' + v;",
		"the controls are the facet fields":       "var CONTROLS = " + facetFieldsJSON() + ";",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("%s: the filter script no longer contains %q", name, want)
		}
	}
}
