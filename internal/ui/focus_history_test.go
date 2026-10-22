package ui

import (
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"
	g "maragu.dev/gomponents"
)

// THE TAB COMPONENT, THE HISTORY LIST, AND WHERE EACH PAGE DRAWS THEM.

func parseFragment(t *testing.T, markup string) *html.Node {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(markup))
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// allAttr is every element under n carrying attribute key.
func allAttr(n *html.Node, key string) []*html.Node {
	var out []*html.Node
	walkNodes(n, func(c *html.Node) bool {
		if c.Type == html.ElementNode {
			if _, ok := nodeAttr(c, key); ok {
				out = append(out, c)
			}
		}
		return true
	})
	return out
}

func attrOf(n *html.Node, key string) string { v, _ := nodeAttr(n, key); return v }

// byID is the element with id under n.
func byID(n *html.Node, id string) *html.Node {
	var out *html.Node
	walkNodes(n, func(c *html.Node) bool {
		if out == nil && c.Type == html.ElementNode && attrOf(c, "id") == id {
			out = c
		}
		return out == nil
	})
	return out
}

// TestTabsAreTheARIATabsPattern: one tablist; each tab a button with
// role=tab, aria-controls naming its panel and the panel aria-labelledby the
// tab; exactly one selected, tabindex 0 on it and -1 on the rest (roving);
// every panel but the selected one hidden; the storage key on the list.
func TestTabsAreTheARIATabsPattern(t *testing.T) {
	set := Tabs("t", "store-key", "Things",
		Tab{ID: "a", Label: "Alpha", Panel: g.Text("panel a")},
		Tab{ID: "b", Label: "Beta", Panel: g.Text("panel b")},
		Tab{ID: "c", Label: "Gamma", Panel: g.Text("panel c")},
	)
	doc := parseFragment(t, renderString(t, g.Group{set.List, set.Panels}))
	list := byID(doc, "t")
	if list == nil || attrOf(list, "role") != "tablist" || attrOf(list, "aria-label") != "Things" || attrOf(list, "data-tabs-key") != "store-key" {
		t.Fatalf("tablist: %v", list)
	}
	tabs := allAttr(list, "data-tab")
	if len(tabs) != 3 {
		t.Fatalf("%d tabs, want 3", len(tabs))
	}
	selected := 0
	for i, tab := range tabs {
		if tab.Data != "button" || attrOf(tab, "role") != "tab" || attrOf(tab, "type") != "button" {
			t.Errorf("tab %d is not a role=tab button", i)
		}
		panel := byID(doc, attrOf(tab, "aria-controls"))
		if panel == nil || attrOf(panel, "role") != "tabpanel" || attrOf(panel, "aria-labelledby") != attrOf(tab, "id") {
			t.Errorf("tab %d's panel is not tied to it both ways", i)
			continue
		}
		_, hidden := nodeAttr(panel, "hidden")
		on := attrOf(tab, "aria-selected") == "true"
		if on {
			selected++
		}
		wantIdx := "-1"
		if on {
			wantIdx = "0"
		}
		if attrOf(tab, "tabindex") != wantIdx {
			t.Errorf("tab %d: tabindex %q with aria-selected=%v (roving tabindex)", i, attrOf(tab, "tabindex"), on)
		}
		if hidden == on {
			t.Errorf("tab %d: panel hidden=%v while selected=%v", i, hidden, on)
		}
	}
	if selected != 1 || attrOf(tabs[0], "aria-selected") != "true" {
		t.Errorf("%d tabs selected (want exactly the first)", selected)
	}
	// The script: arrow keys, Home/End, persisted in try/catch.
	script := renderString(t, tabsScript())
	for _, want := range []string{"'ArrowRight'", "'ArrowLeft'", "'Home'", "'End'", "localStorage.getItem", "localStorage.setItem", "try {", "window.muTabs"} {
		if !strings.Contains(script, want) {
			t.Errorf("tabsScript lacks %q", want)
		}
	}
	if strings.Count(script, "try {") < 2 {
		t.Error("each storage access must be in its own try")
	}
}

// TestFocusHistoryRowsRenderEachPageShape: a first page with no rows is the
// empty state; a page with a next one ends in the sentinel naming it; the last
// page ends in the end mark; text is escaped.
func TestFocusHistoryRowsRenderEachPageShape(t *testing.T) {
	at := time.Now().Add(-5 * time.Minute)
	entries := []FocusHistoryEntry{{Title: `<b>Kernel</b> "upgrade"`, Description: "Draining <i>first</i>.", At: at}, {Title: "Older", At: at.Add(-time.Hour)}}
	next := FocusHistoryPath("agent:12", "1700000000000000.9")
	for name, c := range map[string]struct {
		v                FocusHistoryPageView
		items            int
		empty, more, end bool
	}{
		"empty first page": {FocusHistoryPageView{First: true}, 0, true, false, false},
		"empty later page": {FocusHistoryPageView{}, 0, false, false, true},
		"page with more":   {FocusHistoryPageView{Entries: entries, Next: next, First: true}, 2, false, true, false},
		"last page":        {FocusHistoryPageView{Entries: entries}, 2, false, false, true},
	} {
		t.Run(name, func(t *testing.T) {
			var b strings.Builder
			if err := RenderFocusHistoryPage(&b, c.v); err != nil {
				t.Fatal(err)
			}
			doc := parseFragment(t, b.String())
			if n := len(allAttr(doc, "data-focus-history-item")); n != c.items {
				t.Errorf("%d items, want %d", n, c.items)
			}
			if got := len(allAttr(doc, "data-focus-history-empty")) == 1; got != c.empty {
				t.Errorf("empty state %v, want %v", got, c.empty)
			}
			mores := allAttr(doc, "data-focus-history-more")
			if got := len(mores) == 1; got != c.more {
				t.Errorf("sentinel %v, want %v", got, c.more)
			}
			if c.more && attrOf(mores[0], "data-href") != next {
				t.Errorf("sentinel loads %q, want %q", attrOf(mores[0], "data-href"), next)
			}
			if got := len(allAttr(doc, "data-focus-history-end")) == 1; got != c.end {
				t.Errorf("end mark %v, want %v", got, c.end)
			}
			if c.items > 0 {
				first := allAttr(doc, "data-focus-history-item")[0]
				// The title AND the description go through the title rule
				// (internal/textplain), which removes HTML tags as markup and keeps
				// the rest as text; neither is ever written as markup (below).
				if got := focusNodeText(findAttr(first, "data-focus-history-title")); got != `Kernel "upgrade"` {
					t.Errorf("title %q (the title's plain text expected)", got)
				}
				if got := focusNodeText(findAttr(first, "data-focus-history-description")); got != "Draining first." {
					t.Errorf("description %q (its plain text expected)", got)
				}
				if findAttr(first, "data-ts") == nil {
					t.Error("an entry has no relative time")
				}
				if strings.Contains(b.String(), "<b>Kernel") || strings.Contains(b.String(), "<i>first") {
					t.Error("focus text was written as markup")
				}
			}
		})
	}
	if got := FocusHistoryPath("mirror:0b2c-x", ""); got != "/ui/focus/history?subject=mirror%3A0b2c-x" {
		t.Errorf("FocusHistoryPath = %q", got)
	}
}

// TestTheRenderedViewTabsYourMessagesWithFocusHistory: the index sheet has
// the two tabs, the history tab loads the session's history (or says there is
// none when the host reports no Claude session), the choice is stored per
// window, and the sheet keeps its fixed height.
func TestTheRenderedViewTabsYourMessagesWithFocusHistory(t *testing.T) {
	path := FocusHistoryPath("mirror:0b2c3d4e-sid", "")
	page := renderString(t, MirrorViewPage(MirrorViewView{Host: "host-a", Session: "main", Window: "@2", FocusHistory: path}))
	doc := parseFragment(t, page)
	sheet := byID(doc, "chat-index")
	if sheet == nil {
		t.Fatal("no index sheet")
	}
	list := findAttr(sheet, "data-tabs")
	if list == nil || attrOf(list, "role") != "tablist" {
		t.Fatal("the index sheet has no tablist")
	}
	var labels []string
	for _, tab := range allAttr(list, "data-tab") {
		labels = append(labels, focusNodeText(tab))
	}
	if strings.Join(labels, "|") != "Your messages|Focus history" {
		t.Errorf("tabs %q", labels)
	}
	if key := attrOf(list, "data-tabs-key"); key != "mirror-view:host=host-a&session=main&window=%402" {
		t.Errorf("the tab choice is stored under %q, want the window's address", key)
	}
	msgs, hist := byID(doc, "chat-index-tabs-panel-messages"), byID(doc, "chat-index-tabs-panel-focus")
	if msgs == nil || hist == nil || byID(msgs, "chat-index-list") == nil {
		t.Fatal("the panels are missing, or the message list is not in the first")
	}
	more := findAttr(hist, "data-focus-history-more")
	if more == nil || attrOf(more, "data-href") != path {
		t.Errorf("the history tab does not load %q", path)
	}
	if !strings.Contains(page, "window.muTabs") || !strings.Contains(page, "window.muFocusHistory") {
		t.Error("the page lacks the tabs or history script")
	}
	// The class TOKEN, not a substring: "max-h-[70dvh]" contains "h-[70dvh]"
	// and lets the sheet grow.
	if !nodeHasClass(findAttr(sheet, "data-chat-index-close").NextSibling, "h-[70dvh]") {
		t.Error("the sheet is not a fixed height (a page arriving would move it)")
	}
	// No Claude session reported: the history tab says there is none, and
	// fetches nothing.
	none := parseFragment(t, renderString(t, MirrorViewPage(MirrorViewView{Host: "host-a", Session: "main"})))
	h := byID(none, "chat-index-tabs-panel-focus")
	if findAttr(h, "data-focus-history-more") != nil || findAttr(h, "data-focus-history-empty") == nil {
		t.Error("a window with no Claude session should show the empty history, and load nothing")
	}
}

// TestTheAgentPageHasAFocusHistoryDialog: with a focus store, the header has
// the control and the page the dialog loading the agent's history; without
// one, neither.
func TestTheAgentPageHasAFocusHistoryDialog(t *testing.T) {
	v := sampleAgentDetailView()
	v.FocusHistory = FocusHistoryPath("agent:42", "")
	var b strings.Builder
	if err := RenderAgentDetail(&b, v, featuresAllOn); err != nil {
		t.Fatal(err)
	}
	doc := parseFragment(t, b.String())
	btn, dlg := byID(doc, "focus-history-open"), byID(doc, "focus-history")
	if btn == nil || dlg == nil || dlg.Data != "dialog" {
		t.Fatalf("button %v, dialog %v", btn, dlg)
	}
	if attrOf(btn, "aria-controls") != "focus-history" || attrOf(btn, "aria-haspopup") != "dialog" || attrOf(btn, "aria-expanded") != "false" {
		t.Error("the control is not tied to its dialog")
	}
	if !insideTag(btn, "header") {
		t.Error("the control is not in the page header")
	}
	if more := findAttr(dlg, "data-focus-history-more"); more == nil || attrOf(more, "data-href") != v.FocusHistory {
		t.Error("the dialog does not load the agent's history")
	}
	if !strings.Contains(b.String(), "window.muFocusHistory") {
		t.Error("no history script")
	}
	b.Reset()
	v.FocusHistory = ""
	if err := RenderAgentDetail(&b, v, featuresAllOn); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "focus-history") {
		t.Error("an agent page with no focus store draws focus history")
	}
}

// escapePayload is markup, an event handler, both quotes and an ampersand:
// what a summarizer could be talked into writing into a focus.
const escapePayload = `<img src=x onerror="alert(1)">"'&`

// escapeTitlePayload is the same attack for a TITLE (and a focus description,
// drawn through the same rule). A title is drawn through
// the title rule (internal/textplain), which removes real HTML elements such as
// <img> as markup, so escapePayload would reach the escaper as just "'& and
// this test would stop testing escaping there. <svg> is not on the rule's list
// of elements it removes, so it arrives whole and must still be escaped.
const escapeTitlePayload = `<svg onload="alert(1)">"'&`

// TestFocusTextIsEscapedEverywhereItIsDrawn: the payload in a focus title and
// description is TEXT in a history row, in the card's focus title, in its
// aria-label, and a URL carrying it is an attribute value in data-href — never
// markup, never an attribute break-out. Each place is read back through a
// parser (so the text round-trips exactly) and searched raw (so no tag leaks).
func TestFocusTextIsEscapedEverywhereItIsDrawn(t *testing.T) {
	href := FocusHistoryRoute + "?subject=" + escapePayload
	var b strings.Builder
	if err := RenderFocusHistoryPage(&b, FocusHistoryPageView{First: true, Next: href,
		Entries: []FocusHistoryEntry{{Title: escapeTitlePayload, Description: escapeTitlePayload, At: time.Now()}}}); err != nil {
		t.Fatal(err)
	}
	rows := b.String()
	a := realisticAgent()
	a.Focus = &FocusView{Title: escapeTitlePayload, Description: escapeTitlePayload, UpdatedAt: time.Now()}
	card := renderNode(t, agentCard(a))
	for name, markup := range map[string]string{"history rows": rows, "card": card} {
		if strings.Contains(markup, "<img") || strings.Contains(markup, `onerror="alert`) ||
			strings.Contains(markup, "<svg onload") || strings.Contains(markup, `onload="alert`) {
			t.Errorf("%s: the payload is markup:\n%s", name, markup)
		}
	}
	doc := parseFragment(t, rows)
	item := findAttr(doc, "data-focus-history-item")
	if got := focusNodeText(findAttr(item, "data-focus-history-title")); got != escapeTitlePayload {
		t.Errorf("history title reads %q", got)
	}
	if got := focusNodeText(findAttr(item, "data-focus-history-description")); got != escapeTitlePayload {
		t.Errorf("history description reads %q", got)
	}
	if got := attrOf(findAttr(doc, "data-focus-history-more"), "data-href"); got != href {
		t.Errorf("data-href reads %q, want %q", got, href)
	}
	art := parseArticle(t, card)
	trigger := findAttr(art, "data-card-focus")
	if got := focusNodeText(trigger); !strings.HasPrefix(got, escapeTitlePayload) {
		t.Errorf("card focus title reads %q", got)
	}
	if got := attrOf(trigger, "aria-label"); got != "Focus: "+escapeTitlePayload+" — show details" {
		t.Errorf("aria-label reads %q", got)
	}
	for _, h := range []string{"onerror", "onload"} {
		if n := len(allAttr(art, h)); n != 0 {
			t.Errorf("%d elements carry an %s attribute", n, h)
		}
	}
}
