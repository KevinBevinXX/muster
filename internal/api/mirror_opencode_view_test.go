package api

import (
	"html"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// AN OPENCODE WINDOW's RENDERED VIEW (docs/design/opencode.md): its card opens
// the rendered view when the host names the session its pane title matched,
// the page draws the conversation socket like a Claude Code window's, and its
// composer is DISABLED, saying why, with no send queue — on an interactive
// host too: muster views opencode and sends nothing to it.
//
// 🔴 REGRESSION TEST. On origin/main an opencode window is not on the wire: its
// card opens the Terminal, and /mirror/view redirects there.
func TestAnOpenCodeWindowsCardOpensItsReadOnlyRenderedView(t *testing.T) {
	windows := []mirror.Window{
		{Session: "main", ID: "@1", Index: 0, Active: true, Command: "opencode", Runtime: mirror.RuntimeOpenCode, AgentSession: "ses_0fixture1a2b",
			Status: mirror.StatusIdle, StatusSince: 7},
		// An opencode window whose title matched no session: no view.
		{Session: "main", ID: "@2", Index: 1, Command: "opencode", Runtime: mirror.RuntimeOpenCode},
	}
	fm := &fakeMirror{online: true, interactive: true, v2: &fakeMirrorV2{}, windows: windows, statusReport: true}
	s, ts := viewServer(t, fm)
	_, list := mirrorGet(t, s, ts, "/ui/agents")
	view := ui.MirrorRef{Host: "ws", Session: "main", Window: "@1"}
	none := ui.MirrorRef{Host: "ws", Session: "main", Window: "@2"}
	if !strings.Contains(list, `href="`+amp(view.ViewPath())+`"`) {
		t.Fatalf("the opencode card with a session does not open its rendered view:\n%s", list)
	}
	if !strings.Contains(list, `href="`+amp(none.TerminalPath())+`"`) {
		t.Fatal("the opencode card with no session does not open its terminal")
	}
	if n := strings.Count(list, `data-f-kind="opencode"`); n != 2 {
		t.Errorf("%d cards filter as kind:opencode, want 2", n)
	}

	resp, page := mirrorGet(t, s, ts, view.ViewPath())
	if resp.StatusCode != 200 {
		t.Fatalf("the opencode view page = %d", resp.StatusCode)
	}
	for _, want := range []string{`id="mv-log"`, `data-ws="` + amp(view.ViewWSPath()) + `"`, "opencode is working…",
		// The note, pinned literally (ui.MirrorViewOnlyNote spells it).
		html.EscapeString("muster shows this opencode session read-only: it found the session by the pane's title, and it does not send to opencode yet. Type in the terminal.")} {
		if !strings.Contains(page, want) {
			t.Errorf("the opencode view lacks %q", want)
		}
	}
	i := strings.Index(page, `id="mv-input"`)
	tag := page[strings.LastIndex(page[:i], "<"):]
	tag = tag[:strings.Index(tag, ">")]
	if !disabledAttr.MatchString(tag) {
		t.Fatalf("the opencode view's composer is enabled: %s", tag)
	}
	if strings.Contains(page, ui.SendQueuePath) || strings.Contains(page, "Claude is working") {
		t.Fatal("the opencode view offers a send queue, or calls the agent Claude")
	}

	resp, _ = mirrorGet(t, s, ts, none.ViewPath())
	if resp.StatusCode != 303 || resp.Header.Get("Location") != none.TerminalPath() {
		t.Fatalf("the unmatched opencode window's view = %d → %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}
