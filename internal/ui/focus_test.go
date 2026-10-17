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

// TestAMissingOrStaleFocusSaysWhy: no focus but a recorded failure draws an
// "unavailable" chip whose popover gives the reason; a focus whose latest
// refresh failed keeps its text and adds the failure — on both card kinds.
func TestAMissingOrStaleFocusSaysWhy(t *testing.T) {
	at := time.Now().Add(-2 * time.Minute)
	missing := &FocusView{Problem: "no OpenRouter key", ProblemAt: at}
	stale := &FocusView{Title: "Porting the importer", UpdatedAt: at.Add(-time.Hour), Problem: "rate-limited", ProblemAt: at}
	for name, c := range map[string]struct {
		html string
		want []string
		not  []string
	}{
		"agent, missing": {renderString(t, agentCard(AgentCardView{ID: 7, Name: "otter", Status: "running", Focus: missing})),
			[]string{"data-focus-stale", `aria-label="Focus unavailable: no OpenRouter key"`, ">unavailable</span>", "No focus: no OpenRouter key", "data-focus-problem"},
			[]string{"data-focus-title", "data-focus-updated"}},
		"mirror, missing": {renderString(t, mirrorCard(MirrorCardView{Host: "tower", Session: "work", Window: "@3", Claude: true, Online: true, Focus: missing})),
			[]string{"data-focus-stale", "No focus: no OpenRouter key"}, []string{"data-focus-title"}},
		"agent, stale": {renderString(t, agentCard(AgentCardView{ID: 7, Name: "otter", Status: "running", Focus: stale})),
			[]string{"data-focus-stale", "Porting the importer", "Last refresh failed: rate-limited", "data-focus-updated"}, []string{"No focus:"}},
		"mirror, stale": {renderString(t, mirrorCard(MirrorCardView{Host: "tower", Session: "work", Window: "@3", Claude: true, Online: true, Focus: stale})),
			[]string{"data-focus-stale", "Last refresh failed: rate-limited"}, []string{"No focus:"}},
	} {
		for _, w := range c.want {
			if !strings.Contains(c.html, w) {
				t.Errorf("%s: missing %q", name, w)
			}
		}
		for _, n := range c.not {
			if strings.Contains(c.html, n) {
				t.Errorf("%s: carries %q", name, n)
			}
		}
	}
	current := renderString(t, agentCard(AgentCardView{ID: 7, Name: "otter", Focus: &FocusView{Title: "ok", UpdatedAt: at}}))
	if strings.Contains(current, "data-focus-stale") || strings.Contains(current, "data-focus-problem") {
		t.Fatal("a current focus is drawn as stale")
	}
}

// TestFocusTextIsEscapedInTheCardAndPopover: a title and description carrying
// markup and quotes are drawn as text — on both card kinds.
func TestFocusTextIsEscapedInTheCardAndPopover(t *testing.T) {
	f := &FocusView{Title: `<script>alert(1)</script> "t"`, Description: `<img src=x onerror="x()"> "d"`, UpdatedAt: time.Now()}
	for _, html := range []string{
		renderString(t, agentCard(AgentCardView{ID: 7, Name: "otter", Focus: f})),
		renderString(t, mirrorCard(MirrorCardView{Host: "h", Session: "s", Window: "@1", Focus: f})),
	} {
		for _, raw := range []string{"<script>", "<img", `onerror="x()"`} {
			if strings.Contains(html, raw) {
				t.Fatalf("unescaped %q in:\n%s", raw, html)
			}
		}
		for _, want := range []string{"&lt;script&gt;alert(1)&lt;/script&gt; &#34;t&#34;", "&lt;img src=x onerror=&#34;x()&#34;&gt; &#34;d&#34;"} {
			if !strings.Contains(html, want) {
				t.Fatalf("missing escaped %q in:\n%s", want, html)
			}
		}
	}
}
