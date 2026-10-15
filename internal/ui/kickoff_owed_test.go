package ui

import (
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// ---------------------------------------------------------------------------
// THE OWED-KICKOFF BADGE: BESIDE THE STATUS, NEVER INSTEAD OF IT, AND NEVER
// NESTED INSIDE THE POLLING SPAN.
// ---------------------------------------------------------------------------

// cardFor renders one agent card with the given status and owed flag.
func cardFor(t *testing.T, status string, owed bool) string {
	t.Helper()
	return renderString(t, agentCard(AgentCardView{
		ID: 7, Name: "agent-7", DisplayName: "agent-7", Status: status, KickoffOwed: owed,
	}))
}

// badgeNodes returns EVERY element carrying data-kickoff-owed.
//
// 🔴 IT LOOKS FOR THE ATTRIBUTE, NOT FOR THE WORDS "kickoff owed". A substring
// guard on the label is walkable by any other element that happens to spell the
// same phrase — a tooltip, a mirrored window's title, a future
// filter chip — and the attribute is a handle only this badge has.
//
// 🔴 AND IT RETURNS ALL OF THEM RATHER THAN THE FIRST, WHICH IS A HARNESS FIX FOR
// A MUTANT THAT SURVIVED. The first draft returned one node, and the placement
// mutation — rendering the badge inside cardStatusIcon's polling span — survived
// the whole suite: with a badge in BOTH places, the single-node lookup found the
// safe sibling and reported correct placement while a doomed copy sat in the
// swap target. Collecting every node is what lets the placement guard below
// assert about all of them, and lets the count assertion see a duplicate at all.
func badgeNodes(t *testing.T, markup string) []*html.Node {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(markup))
	if err != nil {
		t.Fatalf("parse card: %v", err)
	}
	var found []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			for _, a := range n.Attr {
				if a.Key == "data-kickoff-owed" {
					found = append(found, n)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return found
}

// badgeNode returns the single owed badge, failing if there is not exactly one.
func badgeNode(t *testing.T, markup string) *html.Node {
	t.Helper()
	ns := badgeNodes(t, markup)
	switch len(ns) {
	case 0:
		return nil
	case 1:
		return ns[0]
	default:
		t.Fatalf("the card renders %d owed-kickoff badges; exactly one is expected. A "+
			"duplicate is how a copy in a doomed position (inside the polling status "+
			"span) hides behind a correctly-placed one.", len(ns))
		return nil
	}
}

// TestAnOwedKickoffIsBadgedBesideARunningStatus is the render half of the
// regression, and `running` is deliberately the status it asserts.
//
// 🔴 `running` IS THE DEFECT, SO IT IS THE FIXTURE. agents.ComputeStatus refines
// a live ready instance to `running` without reading the kickoff columns, so the
// card read entirely healthy over an agent that was never told what to do. A
// fixture using `pending` or `error` would render the badge too and prove nothing
// about the state anybody was fooled by.
//
// 🔴 AND IT ASSERTS THE STATUS DOT IS STILL THERE. "Beside" is the operator's
// decision — an additive signal, not a replacement — and a badge that had
// displaced the status indicator would satisfy a presence-only test.
func TestAnOwedKickoffIsBadgedBesideARunningStatus(t *testing.T) {
	card := cardFor(t, "running", true)

	badge := badgeNode(t, card)
	if badge == nil {
		t.Fatalf("an agent with KickoffOwed=true renders NO owed-kickoff badge.\n"+
			"    This is the measured defect: the status reads `running` (ComputeStatus "+
			"refines a live ready instance and reads neither pending_note nor "+
			"kickoff_error), so the card was entirely healthy over an agent whose first "+
			"turn never happened.\ncard:\n%s", card)
	}
	if got := strings.TrimSpace(textOf(renderToString(t, badge))); got != "kickoff owed" {
		t.Errorf("the badge reads %q, want %q", got, "kickoff owed")
	}

	// The status indicator must still be rendered: the badge is ADDITIVE.
	if !strings.Contains(card, `aria-label="Status: running"`) {
		t.Errorf("the `running` status indicator is gone from a card carrying the owed "+
			"badge. The badge sits BESIDE the status, never instead of it.\ncard:\n%s", card)
	}

	// An accessible name, because the badge's only other affordance is colour.
	var hasAria bool
	for _, a := range badge.Attr {
		if a.Key == "aria-label" && strings.TrimSpace(a.Val) != "" {
			hasAria = true
		}
	}
	if !hasAria {
		t.Error("the owed-kickoff badge has no aria-label, so its meaning is carried by " +
			"an amber pill and nothing else")
	}
}

// TestACardWithNothingOwedRendersNoBadge is the control that stops the test above
// passing over an unconditional badge.
//
// It sweeps every status, because a badge wired to the wrong field (`KickedOff`,
// say, or the status string) would be absent for one value and present for the
// rest — and a single-status control cannot tell that from correct behaviour.
func TestACardWithNothingOwedRendersNoBadge(t *testing.T) {
	for _, st := range []string{"pending", "provisioning", "running", "stopped", "error"} {
		card := cardFor(t, st, false)
		if badgeNode(t, card) != nil {
			t.Errorf("status %q with KickoffOwed=false still renders the owed-kickoff "+
				"badge, so the badge is unconditional and reports nothing", st)
		}
		if !strings.Contains(card, `aria-label="Status: `+st+`"`) {
			t.Errorf("status %q lost its status indicator", st)
		}
	}
}

// TestTheOwedBadgeIsNotInsideThePollingStatusSpan pins the one placement that
// LOOKS right and is erased ten seconds later.
//
// 🔴 cardStatusIcon's SPAN REPLACES ITS OWN innerHTML EVERY 10 SECONDS. It carries
// hx-get=/ui/agents/{id}/status, hx-target=this and hx-swap=innerHTML, and that
// endpoint writes ui.RenderStatusIcon — the bare dot, nothing else. A badge nested
// inside it renders correctly on first paint, passes every assertion above, and
// then vanishes on the first poll tick, which is indistinguishable from "the
// kickoff was delivered". Only a structural check can see this; the markup is
// identical either way at render time.
//
// ⚠ INVARIANT GUARD, NOT REGRESSION COVERAGE. The badge was never inside that
// span.
//
// 🔴 ITS FIRST DRAFT WAS UNREACHABLE AND THE MUTANT SURVIVED THE WHOLE SUITE,
// WHICH IS WHY IT CHECKS EVERY BADGE NODE RATHER THAN ONE. Rendering the badge
// inside cardStatusIcon's Span while leaving the sibling in place produced TWO
// badges; the old single-node lookup found the sibling, walked its safe ancestry
// and passed, with a doomed copy in the swap target. Re-measured after the fix:
// the ADDITIVE mutant now dies on the duplicate count, and the MOVE mutant (the
// sibling deleted, the badge inside the span) dies on this test's own hx-swap
// message. Both are recorded because the second is the realistic edit and the
// first is the one that walked the guard.
func TestTheOwedBadgeIsNotInsideThePollingStatusSpan(t *testing.T) {
	card := cardFor(t, "running", true)
	badges := badgeNodes(t, card)
	if len(badges) == 0 {
		t.Fatal("no owed badge to place-check; see TestAnOwedKickoffIsBadgedBesideARunningStatus")
	}
	if len(badges) != 1 {
		t.Errorf("the card renders %d owed-kickoff badges, want 1. A second copy is how "+
			"one in a doomed position hides behind a correctly-placed one.", len(badges))
	}
	for i, badge := range badges {
		for p := badge.Parent; p != nil; p = p.Parent {
			for _, a := range p.Attr {
				if a.Key == "hx-swap" && strings.Contains(a.Val, "innerHTML") {
					t.Fatalf("owed-kickoff badge #%d is nested inside an element whose "+
						"hx-swap is %q, so the first poll of /ui/agents/{id}/status "+
						"REPLACES it with the bare status icon.\n"+
						"    The badge survives only as a SIBLING of cardStatusIcon. It "+
						"would render correctly once and then disappear, which reads as "+
						"\"the kickoff was delivered\".", i, a.Val)
				}
			}
		}
	}
}

// renderToString re-renders a parsed node so its text can be extracted with the
// package's own textOf helper.
func renderToString(t *testing.T, n *html.Node) string {
	t.Helper()
	var b strings.Builder
	if err := html.Render(&b, n); err != nil {
		t.Fatalf("render node: %v", err)
	}
	return b.String()
}
