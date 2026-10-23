package ui

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var recentLink = regexp.MustCompile(`<a href="([^"]+)" hx-boost="false" data-recent-entry="(agent|mirror)"`)

// TestTheStripShowsTheNewestSixThatAreThereAndNotPinned: eight eligible refs,
// interleaved with a pinned agent, a pinned window and two that are gone; the
// strip draws exactly the first six eligible, in the refs' (newest-first)
// order. Eight, not six, so a missing cap is red; the excluded ones sit
// BEFORE the sixth, so a filter applied after the cap is red too.
func TestTheStripShowsTheNewestSixThatAreThereAndNotPinned(t *testing.T) {
	var list []AgentCardView
	for i := int64(1); i <= 6; i++ {
		list = append(list, AgentCardView{ID: i, Name: "ag-" + strconv.FormatInt(i, 10), Status: "running", Pinned: i == 2})
	}
	mirrors := []MirrorCardView{
		{Host: "tower", Session: "w", Window: "@1", Online: true},
		{Host: "tower", Session: "w", Window: "@2", Online: true, Pinned: true},
		{Host: "tower", Session: "w", Window: "@3", Online: false},
		{Host: "tower", Session: "w", Window: "@4", Online: true, Claude: true},
	}
	w := func(id string) RecentRef {
		return RecentRef{Mirror: MirrorRef{Host: "tower", Session: "w", Window: id}}
	}
	a := func(id int64) RecentRef { return RecentRef{AgentID: id} }
	refs := []RecentRef{a(1), a(2), w("@1"), a(77), w("@2"), w("@9"), a(3), w("@3"), a(4), w("@4"), a(5), a(6)}
	got := []string{}
	for _, m := range recentLink.FindAllStringSubmatch(renderNode(t, recentStrip(recentItems(list, mirrors, refs), true)), -1) {
		got = append(got, m[1])
	}
	want := []string{"/agents/ag-1", amp(MirrorRef{"tower", "w", "@1"}.TerminalPath()), "/agents/ag-3",
		amp(MirrorRef{"tower", "w", "@3"}.TerminalPath()), "/agents/ag-4", amp(MirrorRef{"tower", "w", "@4"}.ViewPath())}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("strip =\n%v\nwant\n%v", got, want)
	}
}

func amp(s string) string { return strings.ReplaceAll(s, "&", "&amp;") }

// TestTheStripIsAboveTheListAndOutsideIt: the shell draws it empty and hidden
// (the out-of-band target), before #agents-list opens — the filter's
// `#agents-list > article` rule assumes every child of the list is a card.
// The partial sends it out of band; an entry is a 44px tap target.
func TestTheStripIsAboveTheListAndOutsideIt(t *testing.T) {
	panel := renderNode(t, AgentsPanel(true))
	i, l := strings.Index(panel, `id="agents-recent"`), strings.Index(panel, `id="agents-list"`)
	if i < 0 || i > l {
		t.Fatalf("the strip is not above #agents-list (strip at %d, list at %d)", i, l)
	}
	shell := panel[strings.LastIndex(panel[:i], "<"):]
	shell = shell[:strings.Index(shell, ">")]
	if !strings.Contains(shell, ` hidden=""`) || strings.Contains(shell, "hx-swap-oob") {
		t.Errorf("the shell's strip is not the empty, hidden swap target: %s", shell)
	}
	part := listPartial(t, []AgentCardView{{ID: 1, Name: "ag-1", Status: "running"}}, nil, []RecentRef{{AgentID: 1}})
	// MORPHED, not replaced (`true`): a replace made the strip's sideways
	// scroller a new element at scrollLeft 0 on every list render
	// (agents_scroll.go; e2e/tests/host-icons.spec.ts measures the offset).
	if !strings.Contains(part, `<nav id="agents-recent" aria-label="Recently viewed" hx-swap-oob="morph" class=`) {
		t.Errorf("the partial does not send the strip out of band, shown:\n%s", part)
	}
	if !strings.Contains(part, `data-recent-entry="agent"`) || !strings.Contains(part, "min-h-[44px]") {
		t.Errorf("the entry is not a 44px link")
	}
	// Nothing to show: the target is still sent (so a stale strip is cleared), hidden.
	empty := listPartial(t, nil, nil, []RecentRef{{AgentID: 1}})
	if !strings.Contains(empty, `<nav id="agents-recent" aria-label="Recently viewed" hidden="" hx-swap-oob="morph"`) {
		t.Errorf("an empty strip is not sent hidden:\n%s", empty)
	}
}

// listPartial is the /ui/agents partial (RenderAgentsList) as a string.
func listPartial(t *testing.T, list []AgentCardView, mirrors []MirrorCardView, recent []RecentRef) string {
	t.Helper()
	var b strings.Builder
	if err := RenderAgentsList(&b, list, mirrors, recent, 0); err != nil {
		t.Fatal(err)
	}
	return b.String()
}
