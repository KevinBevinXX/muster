package ui

import (
	"strings"
	"testing"
)

// TestEverySwitchKeepsBothViews pins that the Rendered | Terminal switch keeps
// both views (view_swap.go) on ALL FOUR pages that draw it — a mirrored
// window's rendered view and terminal, and a claude-code agent's chat page and
// terminal: the link to the other view is marked data-view-swap with that
// view's name, and the page carries the frame script that acts on it.
func TestEverySwitchKeepsBothViews(t *testing.T) {
	pages := map[string]struct {
		html, link string
	}{
		"mirrored rendered view": {
			renderString(t, MirrorViewPage(MirrorViewView{Host: "host-a", Session: "main", Window: "@3", Interactive: true, StepUp: stepUpFixture()})),
			`id="view-terminal" href="/mirror/terminal?host=host-a&amp;session=main&amp;window=%403" hx-boost="false" data-view-swap="terminal"`,
		},
		"mirrored terminal": {
			renderString(t, AgentTerminalPage(AgentTerminalView{Name: "main", WSPath: "/mirror/terminal/ws?host=host-a&session=main&window=%403",
				RenderedHref: "/mirror/view?host=host-a&session=main&window=%403", StepUp: stepUpFixture()})),
			`id="view-rendered" href="/mirror/view?host=host-a&amp;session=main&amp;window=%403" hx-boost="false" data-view-swap="rendered"`,
		},
		"agent chat": {
			renderString(t, AgentDetailPage(AgentDetailView{Name: "keen-wren", Kind: "claude-code"}, Features{})),
			`id="view-terminal" href="/agents/keen-wren/terminal" hx-boost="false" data-view-swap="terminal"`,
		},
		"agent terminal": {
			renderString(t, AgentTerminalPage(AgentTerminalView{Name: "keen-wren", RenderedHref: AgentPath("keen-wren")})),
			`id="view-rendered" href="/agents/keen-wren" hx-boost="false" data-view-swap="rendered"`,
		},
	}
	for name, p := range pages {
		if !strings.Contains(p.html, p.link) {
			t.Errorf("%s: its switch's link is not %s", name, p.link)
		}
		if n := strings.Count(p.html, "window.musterViews = {"); n != 1 {
			t.Errorf("%s: carries the frame script %d times, want 1", name, n)
		}
	}

	// A page with one view draws no switch, and no frame script.
	for name, html := range map[string]string{
		"a terminal with no rendered view": renderString(t, AgentTerminalPage(AgentTerminalView{Name: "scratch", WSPath: "/mirror/terminal/ws?host=host-a&session=scratch"})),
		"a gateway agent's chat":           renderString(t, AgentDetailPage(AgentDetailView{Name: "docs-sweeper", Kind: "gateway"}, Features{})),
	} {
		if strings.Contains(html, "data-view-swap") || strings.Contains(html, "window.musterViews") {
			t.Errorf("%s carries the switch's machinery", name)
		}
	}
}
