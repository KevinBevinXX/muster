package ui

import (
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"
)

// THE FOCUS AS THE CARD'S TITLE (operator ask): with a focus, the focus title
// heads the card and the card's own name moves under it as a subtitle; with
// none — or only a recorded failure — the card is headed by its name, as
// before, with no subtitle.

// parseArticle is markup's one <article>.
func parseArticle(t *testing.T, markup string) *html.Node {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(markup))
	if err != nil {
		t.Fatal(err)
	}
	var art *html.Node
	walkNodes(doc, func(n *html.Node) bool {
		if n.Type == html.ElementNode && n.Data == "article" {
			art = n
			return false
		}
		return true
	})
	if art == nil {
		t.Fatal("instrument check FAILED: no <article>")
	}
	return art
}

// walkNodes visits n's subtree in document order; f returning false stops
// the descent below that node.
func walkNodes(n *html.Node, f func(*html.Node) bool) {
	if !f(n) {
		return
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walkNodes(c, f)
	}
}

// findAttr is the first element under n carrying attribute key.
func findAttr(n *html.Node, key string) *html.Node {
	var out *html.Node
	walkNodes(n, func(c *html.Node) bool {
		if out != nil {
			return false
		}
		if c.Type == html.ElementNode {
			if _, ok := nodeAttr(c, key); ok {
				out = c
				return false
			}
		}
		return true
	})
	return out
}

func focusNodeText(n *html.Node) string {
	var b strings.Builder
	walkNodes(n, func(c *html.Node) bool {
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
		}
		return true
	})
	return strings.Join(strings.Fields(b.String()), " ")
}

func nodeHasClass(n *html.Node, cls string) bool {
	v, _ := nodeAttr(n, "class")
	for _, c := range strings.Fields(v) {
		if c == cls {
			return true
		}
	}
	return false
}

// insideTag reports whether n has an ancestor element named tag.
func insideTag(n *html.Node, tag string) bool {
	for p := n.Parent; p != nil; p = p.Parent {
		if p.Type == html.ElementNode && p.Data == tag {
			return true
		}
	}
	return false
}

func TestAFocusTitleHeadsTheCardAndTheNameBecomesItsSubtitle(t *testing.T) {
	f := &FocusView{Title: "Bisecting the flaky tunnel test", Description: "The backoff resets too early.", UpdatedAt: fixedNow.Add(-time.Minute)}
	agent, mirror := realisticAgent(), realisticMirror()
	agent.Focus, mirror.Focus = f, f
	for name, c := range map[string]struct {
		markup, subtitle string
	}{
		"agent":  {renderNode(t, agentCard(agent)), "Worker seven"},
		"mirror": {renderNode(t, mirrorCard(mirror)), "mango:1"}, // the label alone: a card draws no raw session name
	} {
		t.Run(name, func(t *testing.T) {
			art := parseArticle(t, c.markup)
			trigger := findAttr(art, "data-card-focus")
			if trigger == nil || trigger.Data != "button" {
				t.Fatalf("no focus title button: %v", trigger)
			}
			if got := focusNodeText(trigger); !strings.HasPrefix(got, f.Title) {
				t.Errorf("the focus title reads %q, want it to start %q", got, f.Title)
			}
			// It heads the card: the first slot after the controls, outside the
			// nav anchor (a button may not sit in an <a>, and must not navigate).
			if insideTag(trigger, "a") {
				t.Error("the focus title is inside the card's nav anchor")
			}
			if got := slotTree(t, c.markup); strings.Join(got, ",") != "controls,focus,body,body>header,footer" {
				t.Errorf("slots %v, want the focus slot between the controls and the body", got)
			}
			// It looks and acts like what it is: a popover trigger with a
			// chevron, a hover surface and a 44px target.
			for _, attr := range []string{"aria-haspopup", "aria-expanded", "aria-controls", "popovertarget"} {
				if _, ok := nodeAttr(trigger, attr); !ok {
					t.Errorf("the trigger has no %s", attr)
				}
			}
			pop, _ := nodeAttr(trigger, "popovertarget")
			if ctl, _ := nodeAttr(trigger, "aria-controls"); ctl != pop {
				t.Errorf("aria-controls %q is not the popover %q", ctl, pop)
			}
			if exp, _ := nodeAttr(trigger, "aria-expanded"); exp != "false" {
				t.Errorf("aria-expanded=%q before any open, want false", exp)
			}
			for _, cls := range []string{"min-h-[44px]", "hover:bg-s2", "focus-visible:ring-2", "press"} {
				if !nodeHasClass(trigger, cls) {
					t.Errorf("the trigger lacks %s", cls)
				}
			}
			if findAttr(trigger, "aria-hidden") == nil || !strings.Contains(c.markup, `group-aria-expanded/focus:rotate-180`) {
				t.Error("the trigger has no chevron")
			}
			// The card's own name is the subtitle: inside the nav anchor, marked,
			// and drawn smaller than a title.
			sub := findAttr(art, "data-card-subtitle")
			if sub == nil || !insideTag(sub, "a") {
				t.Fatalf("no subtitle inside the nav anchor")
			}
			if got := focusNodeText(sub); got != c.subtitle {
				t.Errorf("subtitle %q, want %q", got, c.subtitle)
			}
			if strings.Count(c.markup, cardTitleClass) != 0 {
				t.Error("a card headed by its focus still draws its name in the title class")
			}
			if !strings.Contains(c.markup, cardSubtitleClass) {
				t.Error("the subtitle is not drawn in the subtitle class")
			}
		})
	}
}

func TestACardWithoutAFocusKeepsItsTitleAndHasNoSubtitle(t *testing.T) {
	missing := &FocusView{Problem: "no OpenRouter key", ProblemAt: fixedNow}
	for name, c := range map[string]struct {
		markup, title string
	}{
		"agent, none":     {renderNode(t, agentCard(realisticAgent())), "Worker seven"},
		"agent, empty":    {renderNode(t, agentCard(func() AgentCardView { a := realisticAgent(); a.Focus = &FocusView{}; return a }())), "Worker seven"},
		"agent, missing":  {renderNode(t, agentCard(func() AgentCardView { a := realisticAgent(); a.Focus = missing; return a }())), "Worker seven"},
		"mirror, none":    {renderNode(t, mirrorCard(realisticMirror())), "mango:1"},
		"mirror, missing": {renderNode(t, mirrorCard(func() MirrorCardView { m := realisticMirror(); m.Focus = missing; return m }())), "mango:1"},
	} {
		t.Run(name, func(t *testing.T) {
			art := parseArticle(t, c.markup)
			if findAttr(art, "data-card-subtitle") != nil {
				t.Error("a card with no focus title has a subtitle")
			}
			if got := slotTree(t, c.markup); strings.Join(got, ",") != "controls,body,body>header,footer" {
				t.Errorf("slots %v: a card with no focus title must keep today's shape", got)
			}
			if !strings.Contains(c.markup, cardTitleClass) {
				t.Error("the card's name is not drawn as its title")
			}
			if !strings.Contains(c.markup, "flex flex-col gap-3 p-4 pr-28") {
				t.Error("the nav anchor lost its own top padding")
			}
			// Positive control for the title text itself.
			var title *html.Node
			walkNodes(art, func(n *html.Node) bool {
				if title == nil && n.Type == html.ElementNode && nodeHasClass(n, "font-semibold") && insideTag(n, "a") {
					title = n
				}
				return title == nil
			})
			if title == nil || focusNodeText(title) != c.title {
				t.Errorf("title %q, want %q", focusNodeText(title), c.title)
			}
		})
	}
	// The failure stays readable: the footer chip, as before (TestAMissingOrStaleFocusSaysWhy).
	m := renderNode(t, agentCard(func() AgentCardView { a := realisticAgent(); a.Focus = missing; return a }()))
	if footer := m[strings.Index(m, `data-card-slot="footer"`):]; !strings.Contains(footer, "No focus: no OpenRouter key") {
		t.Error("a missing focus no longer says why in the footer")
	}
}

// TestAStaleFocusTitleSaysSoOnTheCard: a focus whose latest refresh failed
// heads the card marked "stale", and its popover says why.
func TestAStaleFocusTitleSaysSoOnTheCard(t *testing.T) {
	a := realisticAgent()
	a.Focus = &FocusView{Title: "Porting the importer", UpdatedAt: fixedNow.Add(-time.Hour), Problem: "rate-limited", ProblemAt: fixedNow}
	art := parseArticle(t, renderNode(t, agentCard(a)))
	trigger := findAttr(art, "data-card-focus")
	if _, ok := nodeAttr(trigger, "data-focus-stale"); !ok || findAttr(trigger, "data-focus-stale-mark") == nil {
		t.Fatal("a stale focus title is not marked stale")
	}
	if got := focusNodeText(findAttr(art, "data-focus-problem")); !strings.HasPrefix(got, "Last refresh failed: rate-limited") {
		t.Errorf("popover problem %q", got)
	}
	a.Focus.Problem = ""
	if findAttr(parseArticle(t, renderNode(t, agentCard(a))), "data-focus-stale-mark") != nil {
		t.Error("a current focus is marked stale")
	}
}

// TestRankingStillUsesTheNameUnderAFocusTitle: the filter ranks on
// data-rank-title (agents_filter.go), which must stay the card's NAMES — the
// focus title is a heading, not a name a reader types.
func TestRankingStillUsesTheNameUnderAFocusTitle(t *testing.T) {
	f := &FocusView{Title: "Zebra migration", Description: "Moving zebras."}
	for name, pair := range map[string][2]string{
		"agent": {renderNode(t, agentCard(realisticAgent())), renderNode(t, agentCard(func() AgentCardView { a := realisticAgent(); a.Focus = f; return a }()))},
		"mirror": {renderNode(t, mirrorCard(realisticMirror())),
			renderNode(t, mirrorCard(func() MirrorCardView { m := realisticMirror(); m.Focus = f; return m }()))},
	} {
		without, _ := nodeAttr(parseArticle(t, pair[0]), "data-rank-title")
		with, _ := nodeAttr(parseArticle(t, pair[1]), "data-rank-title")
		if without == "" {
			t.Fatalf("%s: instrument check FAILED: no data-rank-title", name)
		}
		if with != without {
			t.Errorf("%s: data-rank-title %q with a focus, %q without: the focus changed what ranks", name, with, without)
		}
		if strings.Contains(strings.ToLower(with), "zebra") {
			t.Errorf("%s: the focus title is ranked as a name: %q", name, with)
		}
		filter, _ := nodeAttr(parseArticle(t, pair[1]), "data-filter")
		if !strings.Contains(filter, "zebra migration") {
			t.Errorf("%s: the filter no longer matches the focus text: %q", name, filter)
		}
	}
}
