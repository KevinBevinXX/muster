package ui

import (
	"encoding/json"
	"html"
	"reflect"
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
		{ID: 1, Name: "keen-wren", Status: "running", Kind: agents.KindClaudeCode, CCAccount: "alpha"},
		{ID: 2, Name: "slow-heron", Status: "error"},
	}
	mirrors := []MirrorCardView{
		{Host: "Tower", Session: "s1", Window: "@1", Kind: "opencode", Online: true},
		{Host: "Tower", Session: "s1", Window: "@2", Kind: "opencode", Online: true},
		{Host: "laptop", Session: "s2", Kind: "shell", Online: false},
	}
	return list, mirrors
}

// TestTheFacetsAreTheCardsOwnValues: every option is a value some card in the
// list carries — in the card's own data-f-<field> spelling as V, its display
// spelling as L — once per value, sorted by label. Red before: there were no
// facets.
func TestTheFacetsAreTheCardsOwnValues(t *testing.T) {
	list, mirrors := facetFixture()
	got := agentsFacets(list, mirrors)
	want := map[string][]facetOption{
		"host":   {{"cluster", "cluster"}, {"laptop", "laptop"}, {"tower", "Tower"}},
		"type":   {{"agent", "Agent"}, {"mirrored", "Mirrored"}},
		"status": {{"error", "error"}, {"running", "running"}, {"stopped", "stopped"}},
		// slow-heron has no kind: the default, labelled Standard.
		"kind": {{"claude code", "Claude Code"}, {"opencode", "opencode"}, {"shell", "shell"}, {"standard", "Standard"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("facets =\n%v\nwant\n%v", got, want)
	}
	// Each option's V is exactly what some card's attribute says, so selecting
	// it can match: read the attributes off the rendered cards themselves.
	cards := renderNode(t, AgentsCardsWithMirrors(list, mirrors))
	for field, opts := range got {
		for _, o := range opts {
			if !strings.Contains(cards, ` data-f-`+field+`="`+o.V+`"`) {
				t.Errorf("option %s=%q is carried by no card", field, o.V)
			}
		}
	}
	// An empty list offers nothing (and still says so as JSON objects, not null).
	for _, f := range facetFields {
		if opts := agentsFacets(nil, nil)[f]; opts == nil || len(opts) != 0 {
			t.Errorf("an empty list's %s facet = %#v, want an empty list", f, opts)
		}
	}
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

var facetsDiv = regexp.MustCompile(`<div id="agents-facets"([^>]*)>`)

// facetsOf parses the facets element out of markup: its attributes and its
// decoded JSON.
func facetsOf(t *testing.T, markup string) (string, map[string][]facetOption) {
	t.Helper()
	m := facetsDiv.FindAllStringSubmatch(markup, -1)
	if len(m) != 1 {
		t.Fatalf("want exactly one #agents-facets, got %d in:\n%s", len(m), markup)
	}
	data := regexp.MustCompile(`data-facets="([^"]*)"`).FindStringSubmatch(m[0][1])
	if data == nil {
		t.Fatalf("#agents-facets carries no data-facets: %s", m[0][0])
	}
	var f map[string][]facetOption
	if err := json.Unmarshal([]byte(html.UnescapeString(data[1])), &f); err != nil {
		t.Fatalf("data-facets is not JSON: %v (%s)", err, data[1])
	}
	return m[0][1], f
}

// TestTheListPartialCarriesItsFacetsOutOfBand: the /ui/agents response is the
// cards and, OUT OF BAND, the facets derived from those same cards — the bar is
// outside #agents-list, which the response's main swap fills. The facets are
// not an <article>, and the cards are unchanged by them.
func TestTheListPartialCarriesItsFacetsOutOfBand(t *testing.T) {
	list, mirrors := facetFixture()
	var b strings.Builder
	if err := RenderAgentsList(&b, list, mirrors); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	attrs, got := facetsOf(t, out)
	if !strings.Contains(attrs, `hx-swap-oob="true"`) {
		t.Errorf("the list's facets are not swapped out of band: %s", attrs)
	}
	if !reflect.DeepEqual(got, agentsFacets(list, mirrors)) {
		t.Errorf("the partial's facets are not agentsFacets of its own cards: %v", got)
	}
	if !strings.HasPrefix(out, renderNode(t, AgentsCardsWithMirrors(list, mirrors))) {
		t.Error("the cards are not the partial's leading content, unchanged")
	}
	// The shell's copy is the swap target: present, empty, and NOT out of band.
	panel := renderNode(t, AgentsPanel(true))
	shellAttrs, shell := facetsOf(t, panel)
	if strings.Contains(shellAttrs, "hx-swap-oob") {
		t.Errorf("the shell's facets element is itself out of band: %s", shellAttrs)
	}
	if len(shell) != len(facetFields) {
		t.Errorf("the shell's facets = %v, want an empty list per facet", shell)
	}
	if strings.Index(panel, `id="agents-facets"`) > strings.Index(panel, `id="agents-list"`) {
		t.Error("the facets element is inside or after #agents-list")
	}
}

// TestTheBarHasAControlPerFacetOutsideTheList: one select per facet, its first
// option selecting nothing, each a 16px 44px target; the chips' container,
// "Clear all" and the count — all OUTSIDE #agents-list (a refresh never touches
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
	tag("agents-active")
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
		"the hide rules read the one string": "var q = composeQ();\n    var ts = terms(q);",
		"it is remembered as is":             "sessionStorage.setItem(KEY, q)",
		"it is the URL's ?q=":                "syncURL(q);",
		"a ?q= is split into the controls":   "if (composeQ() !== q) distribute(q);",
		"the facets come from the server":    "var d = el('" + agentsFacetsID + "');",
		"the controls are the facet fields":  "var CONTROLS = " + facetFieldsJSON() + ";",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("%s: the filter script no longer contains %q", name, want)
		}
	}
}
