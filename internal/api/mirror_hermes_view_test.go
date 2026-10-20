package api

import (
	"html"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// A HERMES WINDOW's RENDERED VIEW (docs/design/hermes.md, Phase 2): its card
// opens the rendered view when the host names its session, the page draws
// the conversation socket like a Claude Code window's, and its composer is
// DISABLED, saying why, with no send queue — whatever the host allows.

const hermesSID = "local-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func hermesWindows() []mirror.Window {
	return []mirror.Window{
		{Session: "main", ID: "@1", Index: 0, Active: true, Runtime: mirror.RuntimeHermes, AgentSession: hermesSID, Status: mirror.StatusIdle, StatusSince: 7},
		// A Hermes window whose breadcrumb is stale: no session, no view.
		{Session: "main", ID: "@2", Index: 1, Runtime: mirror.RuntimeHermes},
	}
}

func TestAHermesWindowsCardOpensItsReadOnlyRenderedView(t *testing.T) {
	fm := &fakeMirror{online: true, interactive: true, v2: &fakeMirrorV2{}, windows: hermesWindows(), statusReport: true}
	s, ts := viewServer(t, fm)
	_, list := mirrorGet(t, s, ts, "/ui/agents")
	view := ui.MirrorRef{Host: "ws", Session: "main", Window: "@1"}
	stale := ui.MirrorRef{Host: "ws", Session: "main", Window: "@2"}
	if !strings.Contains(list, `href="`+amp(view.ViewPath())+`"`) {
		t.Fatalf("the Hermes card with a session does not open its rendered view:\n%s", list)
	}
	if !strings.Contains(list, `href="`+amp(stale.TerminalPath())+`"`) {
		t.Fatalf("the Hermes card with no session does not open its terminal")
	}

	resp, page := mirrorGet(t, s, ts, view.ViewPath())
	if resp.StatusCode != 200 {
		t.Fatalf("the Hermes view page = %d", resp.StatusCode)
	}
	for _, want := range []string{`id="mv-log"`, `data-ws="` + amp(view.ViewWSPath()) + `"`, "Hermes is working…",
		html.EscapeString(ui.MirrorHermesNoSendNote)} {
		if !strings.Contains(page, want) {
			t.Errorf("the Hermes view lacks %q", want)
		}
	}
	// The composer's input is disabled — the attribute, not a class variant.
	i := strings.Index(page, `id="mv-input"`)
	tag := page[strings.LastIndex(page[:i], "<"):]
	tag = tag[:strings.Index(tag, ">")]
	if !disabledAttr.MatchString(tag) {
		t.Fatalf("the Hermes view's composer is enabled: %s", tag)
	}
	if strings.Contains(page, ui.SendQueuePath) || strings.Contains(page, "Claude is working") {
		t.Fatal("the Hermes view offers a send queue, or calls the agent Claude")
	}

	// A stale one has no view: the page goes to its terminal.
	resp, _ = mirrorGet(t, s, ts, stale.ViewPath())
	if resp.StatusCode != 303 || resp.Header.Get("Location") != stale.TerminalPath() {
		t.Fatalf("the stale Hermes window's view = %d → %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

// A Claude Code window keeps its enabled composer and its queue: the Hermes
// branch changed nothing for it.
func TestAClaudeWindowsViewIsUnchangedByHermes(t *testing.T) {
	fm := &fakeMirror{online: true, interactive: true, v2: &fakeMirrorV2{}, statusReport: true,
		windows: []mirror.Window{{Session: "main", ID: "@1", Index: 0, Active: true, Claude: true, ClaudeSession: "c1"}}}
	s, ts := viewServer(t, fm)
	_, page := mirrorGet(t, s, ts, ui.MirrorRef{Host: "ws", Session: "main", Window: "@1"}.ViewPath())
	i := strings.Index(page, `id="mv-input"`)
	tag := page[strings.LastIndex(page[:i], "<"):]
	tag = tag[:strings.Index(tag, ">")]
	if disabledAttr.MatchString(tag) || !strings.Contains(page, "Claude is working") || strings.Contains(page, "Hermes") {
		t.Fatalf("a Claude Code view changed: %s", tag)
	}
}
