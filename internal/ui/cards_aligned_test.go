package ui

import (
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"
)

// ---------------------------------------------------------------------------
// ONE CARD LAYOUT FOR BOTH KINDS OF CARD.
// ---------------------------------------------------------------------------

// slotTree is a card's structural slots (data-card-slot) as "parent>slot"
// paths in document order — "controls", "body", "body>header", "footer". The
// article itself is the root, so a slot that moves INTO another (the footer
// into the nav anchor, say) changes its path, not just its position.
func slotTree(t *testing.T, markup string) []string {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(markup))
	if err != nil {
		t.Fatal(err)
	}
	var articles []*html.Node
	var find func(*html.Node)
	find = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "article" {
			articles = append(articles, n)
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			find(c)
		}
	}
	find(doc)
	if len(articles) != 1 {
		t.Fatalf("instrument check FAILED: want one <article>, got %d", len(articles))
	}
	var out []string
	var walk func(n *html.Node, path string)
	walk = func(n *html.Node, path string) {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			p := path
			if c.Type == html.ElementNode {
				if v, ok := nodeAttr(c, "data-card-slot"); ok {
					if p != "" {
						p += ">"
					}
					p += v
					out = append(out, p)
				}
			}
			walk(c, p)
		}
	}
	walk(articles[0], "")
	return out
}

func nodeAttr(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

// realisticAgent / realisticMirror are a card of each kind as the list really
// draws them: an agent with a repo, a kind and both footer times; a window
// with a kind, a label and a reported activity. Their times are distinct.
func realisticAgent() AgentCardView {
	return AgentCardView{ID: 7, Name: "worker-7", DisplayName: "Worker seven", Repo: "acme/widgets", Status: "running",
		CreatedAt: fixedNow.Add(-3 * time.Hour), LastActiveAt: fixedNow.Add(-4 * time.Minute)}
}

func realisticMirror() MirrorCardView {
	return MirrorCardView{Host: "tower", Session: "work", Window: "@3", Label: "mango:1", Kind: "opencode",
		Online: true, Interactive: true, ActiveAt: fixedNow.Add(-9 * time.Minute)}
}

// TestAgentAndMirrorCardsRenderTheSameSlots: the operator report "the two
// kinds of card look like two different apps". An agent card and a mirrored
// card are built from the same structural slots, in the same order and the
// same nesting: a control cluster, a nav body holding the header row, and a
// footer. Red before: a mirrored card had no slots and no footer at all.
func TestAgentAndMirrorCardsRenderTheSameSlots(t *testing.T) {
	want := []string{"controls", "body", "body>header", "footer"}
	agent := slotTree(t, renderNode(t, agentCard(realisticAgent())))
	mirror := slotTree(t, renderNode(t, mirrorCard(realisticMirror())))
	if strings.Join(agent, ",") != strings.Join(want, ",") {
		t.Errorf("agent card slots = %v, want %v", agent, want)
	}
	if strings.Join(mirror, ",") != strings.Join(agent, ",") {
		t.Errorf("mirrored card slots = %v, agent card slots = %v: the two cards are laid out differently", mirror, agent)
	}
	// An offline window is still the same card, not a third layout.
	off := realisticMirror()
	off.Online, off.ActiveAt = false, time.Time{}
	if got := slotTree(t, renderNode(t, mirrorCard(off))); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("an offline mirrored card's slots = %v, want %v", got, want)
	}
}

// TestBothCardsPrintActivityWithTheSameComponent: the footer's "active … ago"
// is ONE component (cardActivity) on both kinds, each fed its own time.
func TestBothCardsPrintActivityWithTheSameComponent(t *testing.T) {
	a, m := realisticAgent(), realisticMirror()
	for name, tc := range map[string]struct {
		markup string
		at     time.Time
	}{
		"agent":    {renderNode(t, agentCard(a)), a.LastActiveAt},
		"mirrored": {renderNode(t, mirrorCard(m)), m.ActiveAt},
	} {
		want := renderNode(t, cardActivity(tc.at))
		if !strings.Contains(want, "data-card-active") {
			t.Fatal("instrument check FAILED: cardActivity renders no data-card-active")
		}
		if !strings.Contains(tc.markup, want) {
			t.Errorf("%s card does not print cardActivity(%v):\n%s", name, tc.at, tc.markup)
		}
	}
}

// TestNoCardRendersAPreview: operator decision — no chat or log preview on
// ANY card, so no card carries the preview's <pre>, its lazy slot, or a fetch
// of /ui/agents/{id}/recent. The agent is RUNNING, which is the case the
// preview used to load for.
func TestNoCardRendersAPreview(t *testing.T) {
	a := realisticAgent()
	for name, markup := range map[string]string{
		"agent":    renderNode(t, agentCard(a)),
		"mirrored": renderNode(t, mirrorCard(realisticMirror())),
		"list":     renderNode(t, AgentsCardsWithMirrors([]AgentCardView{a}, []MirrorCardView{realisticMirror()})),
		"panel":    renderNode(t, AgentsPanel(true)),
	} {
		for _, bad := range []string{"<pre", "agent-recent-", "/recent", "animate-pulse"} {
			if strings.Contains(markup, bad) {
				t.Errorf("%s: still carries the preview (%q)", name, bad)
			}
		}
	}
}

// TestAMirroredCardHasNoInteractiveBadge: the `mode: interactive` chip is
// gone from the card. What the host announced is still a FILTER field
// (mode:interactive), so the attribute stays.
func TestAMirroredCardHasNoInteractiveBadge(t *testing.T) {
	markup := renderNode(t, mirrorCard(realisticMirror()))
	if strings.Contains(markup, "data-mirror-interactive") || strings.Contains(markup, `<span class="text-muted">mode</span>`) {
		t.Errorf("the card still badges interactive:\n%s", markup)
	}
	if !strings.Contains(markup, `data-f-mode="interactive"`) {
		t.Errorf("the mode filter field went with the badge:\n%s", markup)
	}
}

// TestTheMirroredMarkIsAnIconWithAnAccessibleTooltip: "Mirrored · <host>" is
// no longer text in the title row. It is an icon button OUTSIDE the nav anchor
// (so a tap opens the tooltip rather than the window) whose accessible name
// and title say it, and whose popover — the tap tooltip — says it too. The
// host is still VISIBLE, in the footer.
func TestTheMirroredMarkIsAnIconWithAnAccessibleTooltip(t *testing.T) {
	markup := renderNode(t, mirrorCard(realisticMirror()))
	doc, err := html.Parse(strings.NewReader(markup))
	if err != nil {
		t.Fatal(err)
	}
	var mark, tip, anchor, hostEl *html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if _, ok := nodeAttr(n, "data-mirror-badge"); ok {
				mark = n
			}
			if n.Data == "a" {
				anchor = n
			}
			if _, ok := nodeAttr(n, "data-mirror-host"); ok {
				hostEl = n
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	if mark == nil || anchor == nil {
		t.Fatalf("no mark or no anchor:\n%s", markup)
	}
	if mark.Data != "button" {
		t.Errorf("the mark is a <%s>, want a <button> a tap can open", mark.Data)
	}
	for p := mark.Parent; p != nil; p = p.Parent {
		if p == anchor {
			t.Fatal("the mark is inside the card's nav anchor: a tap navigates instead of showing the tooltip")
		}
	}
	for _, k := range []string{"aria-label", "title"} {
		if v, _ := nodeAttr(mark, k); v != "Mirrored · tower" {
			t.Errorf("mark %s = %q, want %q", k, v, "Mirrored · tower")
		}
	}
	if text := nodeText(mark); strings.TrimSpace(text) != "" {
		t.Errorf("the mark carries visible text %q; it is an icon", text)
	}
	target, _ := nodeAttr(mark, "popovertarget")
	var findTip func(*html.Node)
	findTip = func(n *html.Node) {
		if v, ok := nodeAttr(n, "id"); ok && n.Type == html.ElementNode && v == target && target != "" {
			tip = n
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			findTip(c)
		}
	}
	findTip(doc)
	if tip == nil {
		t.Fatalf("the mark's popovertarget %q names no element", target)
	}
	if v, _ := nodeAttr(tip, "popover"); v != "auto" {
		t.Errorf("tooltip popover = %q, want auto (outside tap and Escape close it)", v)
	}
	if v, _ := nodeAttr(tip, "role"); v != "tooltip" {
		t.Errorf("tooltip role = %q", v)
	}
	if !strings.Contains(nodeText(tip), "Mirrored · tower") {
		t.Errorf("the tap tooltip does not name the host: %q", nodeText(tip))
	}
	if strings.Contains(nodeText(anchor), "Mirrored") {
		t.Errorf("the title row still spells the badge: %q", nodeText(anchor))
	}
	if hostEl == nil || nodeText(hostEl) != "tower" {
		t.Errorf("the host is not visible on the card")
	}
	// Two cards' tooltips are two elements: the id is the card's.
	other := realisticMirror()
	other.Window = "@4"
	if strings.Contains(renderNode(t, mirrorCard(other)), `id="`+target+`"`) {
		t.Errorf("two windows' tooltips share the id %q", target)
	}
}

func nodeText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}
