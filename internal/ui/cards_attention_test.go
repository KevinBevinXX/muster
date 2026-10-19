package ui

import (
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// badgeNodes is every element carrying data-attention-badge in markup, with
// the data-card-slot of each one's nearest slotted ancestor ("" for none).
func attentionBadgeNodes(t *testing.T, markup string) (slots []string, hrefs []string) {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(markup))
	if err != nil {
		t.Fatal(err)
	}
	var walk func(n *html.Node, slot string)
	walk = func(n *html.Node, slot string) {
		if n.Type == html.ElementNode {
			if v, ok := nodeAttr(n, "data-card-slot"); ok {
				slot = v
			}
			if _, ok := nodeAttr(n, "data-attention-badge"); ok {
				if n.Data != "a" {
					t.Fatalf("the badge is a <%s>, want a link", n.Data)
				}
				for p := n.Parent; p != nil; p = p.Parent {
					if p.Type == html.ElementNode && p.Data == "a" {
						t.Fatalf("the badge is inside another link (the card's nav anchor?)")
					}
				}
				href, _ := nodeAttr(n, "href")
				slots, hrefs = append(slots, slot), append(hrefs, href)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, slot)
		}
	}
	walk(doc, "")
	return slots, hrefs
}

// TestTheAttentionBadgeIsAControlThatOpensTheQueueItem: on BOTH kinds of card
// the badge is a link to the item on the queue page, in the control cluster —
// never inside the card's nav anchor, so tapping it cannot open the card —
// showing the count and the kind's icon; the title rows widen their
// reservation for it. No items, no badge, and the old reservation.
func TestTheAttentionBadgeIsAControlThatOpensTheQueueItem(t *testing.T) {
	at := &CardAttention{Count: 3, Kind: "question", Href: "/attention#session-0a1b"}
	a := realisticAgent()
	a.Attention = at
	m := realisticMirror()
	m.Attention = at
	for name, card := range map[string]string{"agent": renderNode(t, agentCard(a)), "mirror": renderNode(t, mirrorCard(m))} {
		slots, hrefs := attentionBadgeNodes(t, card)
		if len(slots) != 1 || slots[0] != "controls" || hrefs[0] != "/attention#session-0a1b" {
			t.Errorf("%s: badges in slots %v linking %v, want one in controls linking the item", name, slots, hrefs)
		}
		for _, want := range []string{`data-kind="question"`, `data-count="3"`, AttentionKindIcons["question"].Icon, `hx-boost="false"`,
			"3 waiting on you; the most urgent is a session asking a question"} {
			if !strings.Contains(card, want) {
				t.Errorf("%s: the badge lacks %q:\n%s", name, want, card)
			}
		}
		if !strings.Contains(card, `data-card-slot="body" class="flex flex-col gap-3 p-4 pr-44"`) {
			t.Errorf("%s: the title rows do not keep clear of the wider cluster:\n%s", name, card)
		}
	}
	a.Attention, m.Attention = &CardAttention{}, nil
	for name, card := range map[string]string{"agent": renderNode(t, agentCard(a)), "mirror": renderNode(t, mirrorCard(m))} {
		if slots, _ := attentionBadgeNodes(t, card); len(slots) != 0 || !strings.Contains(card, "pr-28") || strings.Contains(card, "pr-44") {
			t.Errorf("%s: with no items: badges %v", name, slots)
		}
	}
}

// TestAMirroredCardShowsAndIsFoundByItsRepo: the repo chip, the `repo:` field
// and the free-text haystack — and none of them for a host that did not say
// (an older muster-mirror). The card's NAME is unchanged by it.
func TestAMirroredCardShowsAndIsFoundByItsRepo(t *testing.T) {
	m := MirrorCardView{Host: "lapbox", Session: "fleet", Window: "@5", Title: "fleet:5", Claude: true, Online: true, Repo: "Fixture-Repo"}
	card := renderNode(t, mirrorCard(m))
	if !strings.Contains(card, `<span data-mirror-repo=""><span class="inline-flex`) || !strings.Contains(card, `repo</span>Fixture-Repo</span>`) {
		t.Errorf("no repo chip:\n%s", card)
	}
	if got := fieldsOf(t, card)["repo"]; got != "fixture-repo" {
		t.Errorf("data-f-repo = %q, want fixture-repo", got)
	}
	if hs := haystackOf(t, card); !strings.Contains(hs, "fixture-repo") {
		t.Errorf("the haystack %q lacks the repo", hs)
	}
	if !strings.Contains(card, `data-mirror-name="">fleet:5</span>`) {
		t.Errorf("the card's name changed:\n%s", card)
	}
	m.Repo = ""
	old := renderNode(t, mirrorCard(m))
	if strings.Contains(old, "data-mirror-repo") || fieldsOf(t, old)["repo"] != "" {
		t.Errorf("a card with no repo drew one:\n%s", old)
	}
}
