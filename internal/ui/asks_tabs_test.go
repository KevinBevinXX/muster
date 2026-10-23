package ui

import (
	"regexp"
	"strings"
	"testing"

	g "maragu.dev/gomponents"
)

// TestTheSidebarCallsTheQueueAsks: the queue's sidebar entry reads "Asks"
// (operator decision "Rename to Asks"), still at /attention.
func TestTheSidebarCallsTheQueueAsks(t *testing.T) {
	link := renderString(t, attentionLink(Features{Attention: true}, false, false))
	m := regexp.MustCompile(`<a id="sidebar-attention" href="([^"]*)"[^>]*>.*?<span>([^<]*)</span>`).FindStringSubmatch(link)
	if m == nil || m[1] != "/attention" || m[2] != "Asks" {
		t.Fatalf("the sidebar entry: %q; want href /attention labelled Asks", m)
	}
}

// TestTabsSelectedDrawsTheNamedTab: TabsSelected draws the named tab selected
// (aria-selected, the roving tabindex 0, its panel shown, every other hidden)
// — the first when the name matches none, which is also Tabs.
func TestTabsSelectedDrawsTheNamedTab(t *testing.T) {
	tabs := []Tab{{ID: "one", Label: "One", Panel: g.Text("p1")}, {ID: "two", Label: "Two", Panel: g.Text("p2")}, {ID: "three", Label: "Three", Panel: g.Text("p3")}}
	state := func(set TabSet) string {
		html := renderString(t, g.Group{set.List, set.Panels})
		var out []string
		for _, id := range []string{"one", "two", "three"} {
			tab := regexp.MustCompile(`<button[^>]* id="x-tab-` + id + `"[^>]*>`).FindString(html)
			panel := regexp.MustCompile(`<div id="x-panel-` + id + `"[^>]*>`).FindString(html)
			if tab == "" || panel == "" {
				t.Fatalf("no tab or panel %q in %s", id, html)
			}
			on := strings.Contains(tab, `aria-selected="true"`) && strings.Contains(tab, `tabindex="0"`) && !strings.Contains(panel, " hidden")
			off := strings.Contains(tab, `aria-selected="false"`) && strings.Contains(tab, `tabindex="-1"`) && strings.Contains(panel, " hidden")
			switch {
			case on:
				out = append(out, id+"=on")
			case off:
				out = append(out, id+"=off")
			default:
				t.Fatalf("tab %q is half-selected: %s / %s", id, tab, panel)
			}
		}
		return strings.Join(out, " ")
	}
	for _, c := range []struct{ sel, want string }{
		{"two", "one=off two=on three=off"},
		{"three", "one=off two=off three=on"},
		{"one", "one=on two=off three=off"},
		{"", "one=on two=off three=off"},
		{"nope", "one=on two=off three=off"},
	} {
		if got := state(TabsSelected("x", "", "X", c.sel, tabs...)); got != c.want {
			t.Errorf("TabsSelected(%q): %s; want %s", c.sel, got, c.want)
		}
	}
	if got := state(Tabs("x", "", "X", tabs...)); got != "one=on two=off three=off" {
		t.Errorf("Tabs: %s; want the first selected", got)
	}
}
