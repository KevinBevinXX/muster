package ui

import (
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// TestAHeldKickoffShowsTheGithubApprovalLineOutsideTheCardLink: a card whose first
// turn is held for its github grant shows a "waiting for your github approval"
// line with a link to Connectors; the line sits OUTSIDE the card's nav anchor (a
// link nested in it is invalid markup whose taps open the card); and a card
// without the flag shows none of it.
func TestAHeldKickoffShowsTheGithubApprovalLineOutsideTheCardLink(t *testing.T) {
	card := func(waiting bool) string {
		return renderString(t, agentCard(AgentCardView{
			ID: 31, Name: "teal-brant", DisplayName: "teal-brant", Status: "running",
			Repo: "example-org/shoal-map", KickoffOwed: true, KickoffAwaitingGrant: waiting,
		}))
	}
	if off := card(false); strings.Contains(off, "data-kickoff-awaiting-grant") || strings.Contains(off, `href="/connectors"`) {
		t.Fatalf("a card that is not waiting renders the line:\n%s", off)
	}
	on := card(true)
	doc, err := html.Parse(strings.NewReader(on))
	if err != nil {
		t.Fatal(err)
	}
	var line *html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && hasAttr(n, "data-kickoff-awaiting-grant") {
			line = n
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	if line == nil {
		t.Fatalf("no waiting line on a held card:\n%s", on)
	}
	for p := line.Parent; p != nil; p = p.Parent {
		if p.Type == html.ElementNode && p.Data == "a" {
			t.Fatalf("the waiting line is inside the card's nav anchor")
		}
	}
	var text strings.Builder
	var link string
	var collect func(*html.Node)
	collect = func(n *html.Node) {
		if n.Type == html.TextNode {
			text.WriteString(n.Data)
		}
		if n.Type == html.ElementNode && n.Data == "a" {
			link = attrOf(n, "href")
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			collect(c)
		}
	}
	collect(line)
	if !strings.Contains(text.String(), "Waiting for your github approval") || link != "/connectors" {
		t.Fatalf("line text %q, link %q; want the github approval wording and /connectors", text.String(), link)
	}
	// The owed badge still says the turn is owed.
	if !strings.Contains(on, "data-kickoff-owed") {
		t.Fatal("a held card lost its kickoff owed badge")
	}
}
