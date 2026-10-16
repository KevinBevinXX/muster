package ui

import (
	"strings"
	"testing"
	"time"
)

// The focus chip on BOTH card kinds: present with a focus, absent without, a
// native popover (the ⋯ menu's pattern) in the footer, and found by the filter.

func TestBothCardKindsShowTheirFocusInAPopover(t *testing.T) {
	f := &FocusView{Title: "Reconciling invoices", Description: "Matching payouts to the ledger.", UpdatedAt: time.Now().Add(-3 * time.Minute)}
	for name, html := range map[string]string{
		"agent":  renderString(t, agentCard(AgentCardView{ID: 7, Name: "otter", Status: "running", Focus: f})),
		"mirror": renderString(t, mirrorCard(MirrorCardView{Host: "tower", Session: "work", Window: "@3", Claude: true, Online: true, Focus: f})),
	} {
		t.Run(name, func(t *testing.T) {
			for _, want := range []string{
				`data-card-focus`, `popovertarget=`, `popover="auto"`, `data-floating-menu`, `data-focus-popover`,
				`>Reconciling invoices</span>`, `Matching payouts to the ledger.`, `data-focus-updated`, `data-ts=`,
				`min-h-[44px]`,
			} {
				if !strings.Contains(html, want) {
					t.Errorf("missing %q", want)
				}
			}
			// The chip and its popover are in the FOOTER, outside the nav anchor.
			footer := html[strings.Index(html, `data-card-slot="footer"`):]
			if !strings.Contains(footer, "data-card-focus") || !strings.Contains(footer, "data-focus-popover") {
				t.Error("the focus chip is not in the card footer")
			}
			// The filter finds the card by its focus.
			if !strings.Contains(html, "reconciling invoices") || !strings.Contains(html, "matching payouts to the ledger.") {
				t.Error("data-filter does not carry the focus text")
			}
		})
	}
}

func TestACardWithNoFocusShowsNoChip(t *testing.T) {
	for _, html := range []string{
		renderString(t, agentCard(AgentCardView{ID: 7, Name: "otter", Status: "running"})),
		renderString(t, mirrorCard(MirrorCardView{Host: "tower", Session: "work", Window: "@3", Online: true})),
		renderString(t, agentCard(AgentCardView{ID: 8, Name: "seal", Status: "running", Focus: &FocusView{}})),
	} {
		if strings.Contains(html, "data-card-focus") || strings.Contains(html, "data-focus-popover") {
			t.Fatalf("a card with no focus drew a chip: %s", html)
		}
	}
}

// TestFocusPopoverIDsAreUniquePerCard: two cards' popovers cannot share an id
// (popovertarget resolves by id; a shared one would open the wrong card's).
func TestFocusPopoverIDsAreUniquePerCard(t *testing.T) {
	f := &FocusView{Title: "t"}
	a := renderString(t, agentCard(AgentCardView{ID: 7, Name: "a", Focus: f}))
	b := renderString(t, agentCard(AgentCardView{ID: 8, Name: "b", Focus: f}))
	m := renderString(t, mirrorCard(MirrorCardView{Host: "h", Session: "s", Window: "@1", Focus: f}))
	ids := map[string]bool{}
	for _, html := range []string{a, b, m} {
		start := strings.Index(html, `data-focus-popover`)
		seg := html[:start]
		j := strings.LastIndex(seg, `id="`)
		id := seg[j+4 : j+4+strings.Index(seg[j+4:], `"`)]
		if ids[id] {
			t.Fatalf("two cards share the focus popover id %q", id)
		}
		ids[id] = true
	}
}
