package api

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// EVERY WINDOW IS A CARD, through the real mux: a per-window host's windows are
// each listed, linked and addressed by (session, tmux window id); a URL naming
// only the session — every link from before windows were addressable — lands
// on the session's ACTIVE window, as an explicit address; and each page's
// socket and composer name the window on screen.
// ---------------------------------------------------------------------------

// goldHost is the per-window fixture: session scratch2 ("Gold") has three
// windows and its ACTIVE one is index 8 / id @7 (Claude Code) — not the first
// listed, not the lowest id; session solo has one window, a shell.
func goldHost() *fakeMirror {
	return &fakeMirror{online: true, interactive: true, v2: &fakeMirrorV2{}, windows: []mirror.Window{
		{Session: "scratch2", ID: "@4", Index: 1, Label: "Gold:1", Command: "zsh"},
		{Session: "scratch2", ID: "@7", Index: 8, Active: true, Label: "Gold:8", Claude: true, Command: "claude"},
		{Session: "scratch2", ID: "@9", Index: 11, Label: "Gold:11", Claude: true, Command: "claude"},
		{Session: "solo", ID: "@2", Index: 3, Active: true, Command: "opencode"},
	}}
}

func ref(session, window string) ui.MirrorRef {
	return ui.MirrorRef{Host: "ws", Session: session, Window: window}
}

// cardFor is the mirrored card whose link is href, in the list markup.
func cardFor(t *testing.T, markup, href string) string {
	t.Helper()
	marker := `href="` + amp(href) + `"`
	i := strings.Index(markup, marker)
	if i < 0 {
		t.Fatalf("no mirrored card links to %s in:\n%s", href, markup)
	}
	rest := markup[strings.LastIndex(markup[:i], "<article"):]
	if j := strings.Index(rest, "</article>"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// TestAPerWindowHostsWindowsAreEachACard: four windows, four cards — where one
// card per session showed two. Each is titled by its own label (or
// "<session>:<index>" with none), says what runs in it, opens the rendered view
// when that is Claude Code and the terminal otherwise, and its link carries the
// window's ID. The raw session name stays visible beside a codename.
func TestAPerWindowHostsWindowsAreEachACard(t *testing.T) {
	s, ts := viewServer(t, goldHost())
	_, list := mirrorGet(t, s, ts, "/ui/agents")
	if n := strings.Count(list, "data-mirror-card"); n != 4 {
		t.Fatalf("a host with 4 windows drew %d mirrored cards", n)
	}
	for _, tc := range []struct {
		href, title, small, kind string
	}{
		{ref("scratch2", "@4").TerminalPath(), "Gold:1", "scratch2", "shell"},
		{ref("scratch2", "@7").ViewPath(), "Gold:8", "scratch2", "Claude Code"},
		{ref("scratch2", "@9").ViewPath(), "Gold:11", "scratch2", "Claude Code"},
		{ref("solo", "@2").TerminalPath(), "solo:3", "", "opencode"},
	} {
		card := cardFor(t, list, tc.href)
		if tc.small != "" {
			if !strings.Contains(card, `data-mirror-label="">`+tc.title+`<`) || !strings.Contains(card, `>`+tc.small+`</span>`) {
				t.Errorf("%s: the card is not titled %q with %q beside it:\n%s", tc.href, tc.title, tc.small, card)
			}
		} else if !strings.Contains(card, `data-mirror-name="">`+tc.title+`<`) || strings.Contains(card, "data-mirror-label") {
			t.Errorf("%s: a window with no codename should be titled %q and nothing else:\n%s", tc.href, tc.title, card)
		}
		if !strings.Contains(card, ">"+tc.kind+"</span>") {
			t.Errorf("%s: the card does not say %q runs there:\n%s", tc.href, tc.kind, card)
		}
		if !strings.Contains(tc.href, "window=%40") {
			t.Fatalf("fixture: %s carries no window id", tc.href)
		}
	}
	// Two windows of one session are two DIFFERENT elements.
	a, b := cardFor(t, list, ref("scratch2", "@7").ViewPath()), cardFor(t, list, ref("scratch2", "@9").ViewPath())
	if idOf(a) == "" || idOf(a) == idOf(b) {
		t.Fatalf("two windows of one session share an element id: %q / %q", idOf(a), idOf(b))
	}
}

func idOf(card string) string {
	i := strings.Index(card, ` id="`)
	if i < 0 {
		return ""
	}
	rest := card[i+5:]
	return rest[:strings.Index(rest, `"`)]
}

// TestAnAddressWithNoWindowLandsOnTheSessionsActiveWindow: the URLs that
// existed before windows did — /mirror/{view,terminal}?host=&session= — still
// work, and mean the session's ACTIVE window: each answers with a redirect to
// that window's explicit address (so the page, its socket and its composer
// then name ONE window). A session whose active window is not Claude Code goes
// to its terminal, as it always did, with the window named too.
func TestAnAddressWithNoWindowLandsOnTheSessionsActiveWindow(t *testing.T) {
	s, ts := viewServer(t, goldHost())
	for from, to := range map[string]string{
		ui.MirrorTerminalPath("ws", "scratch2"): ref("scratch2", "@7").TerminalPath(),
		ui.MirrorViewPath("ws", "scratch2"):     ref("scratch2", "@7").ViewPath(),
		ui.MirrorTerminalPath("ws", "solo"):     ref("solo", "@2").TerminalPath(),
		ui.MirrorViewPath("ws", "solo"):         ref("solo", "@2").TerminalPath(), // not Claude Code
		ref("scratch2", "@4").ViewPath():        ref("scratch2", "@4").TerminalPath(),
	} {
		resp, _ := mirrorGet(t, s, ts, from)
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != to {
			t.Errorf("GET %s = %d → %q, want 303 → %q", from, resp.StatusCode, resp.Header.Get("Location"), to)
		}
	}
	// An explicit address is served, not redirected.
	for _, p := range []string{ref("scratch2", "@7").ViewPath(), ref("scratch2", "@9").TerminalPath(), ref("solo", "@2").TerminalPath()} {
		if resp, _ := mirrorGet(t, s, ts, p); resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", p, resp.StatusCode)
		}
	}
	// A per-session host (no windows) keeps its old URLs AS THEY ARE: no redirect.
	s2, ts2 := viewServer(t, &fakeMirror{online: true, interactive: true, v2: &fakeMirrorV2{}})
	for _, p := range []string{ui.MirrorTerminalPath("ws", "main"), ui.MirrorViewPath("ws", "main")} {
		if resp, _ := mirrorGet(t, s2, ts2, p); resp.StatusCode != http.StatusOK {
			t.Errorf("a per-session host: GET %s = %d, want 200 with no redirect", p, resp.StatusCode)
		}
	}
}

// TestAWindowsPagesSocketAndComposerAllNameThatWindow: the page of a window
// that is NOT the session's active one. Its title is that window's; its socket
// URL, the Rendered|Terminal switch and its composer all carry that window's
// id — and what reaches the plugin, for the socket and for a send, is that id.
func TestAWindowsPagesSocketAndComposerAllNameThatWindow(t *testing.T) {
	fm := goldHost()
	s, ts := viewServer(t, fm)
	nine := ref("scratch2", "@9")

	_, term := mirrorGet(t, s, ts, nine.TerminalPath())
	if !strings.Contains(term, `data-ws="`+amp(nine.TerminalWSPath())+`"`) {
		t.Errorf("the terminal page's socket is not window @9's:\n%s", tagOf(term, "term"))
	}
	if form := tagOf(term, "mv-form"); !strings.Contains(form, `data-window="@9"`) || !strings.Contains(form, `data-session="scratch2"`) {
		t.Errorf("the terminal page's composer does not name window @9: %s", form)
	}
	if !strings.Contains(term, `id="view-rendered" href="`+amp(nine.ViewPath())+`"`) {
		t.Error("the terminal page's Rendered switch does not lead to window @9's view")
	}
	if !strings.Contains(term, "Gold:11") || strings.Contains(term, "Gold:8") {
		t.Error("the terminal page is not titled by its own window's label")
	}

	_, view := mirrorGet(t, s, ts, nine.ViewPath())
	if !strings.Contains(view, `data-ws="`+amp(nine.ViewWSPath())+`"`) {
		t.Errorf("the rendered view's socket is not window @9's: %s", tagOf(view, "mv-log"))
	}
	if form := tagOf(view, "mv-form"); !strings.Contains(form, `data-window="@9"`) {
		t.Errorf("the rendered view's composer does not name window @9: %s", form)
	}
	if !strings.Contains(view, `id="view-terminal" href="`+amp(nine.TerminalPath())+`"`) {
		t.Error("the rendered view's Terminal switch does not lead to window @9's terminal")
	}

	asked := func() []string {
		fm.mu.Lock()
		defer fm.mu.Unlock()
		return append([]string(nil), fm.asked...)
	}
	// The terminal socket.
	c, _, err := dialTerm(t, s, ts, nine.TerminalWSPath()+"&mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	readUntil(t, c, "ready")
	c.CloseNow()
	// A send from each composer.
	for _, path := range []string{ui.MirrorTerminalSendPath, ui.MirrorSendPath} {
		code, body, _ := postSend(t, ts.URL, path, sessionCookieFor(s), "", url.Values{"host": {"ws"}, "session": {"scratch2"}, "window": {"@9"}, "text": {"hi"}})
		if code != http.StatusNoContent {
			t.Fatalf("%s: %d %q", path, code, body)
		}
	}
	if got := asked(); len(got) != 3 || got[0] != "@9" || got[1] != "@9" || got[2] != "@9" {
		t.Fatalf("the plugin was asked for windows %q; the socket and both sends must each name @9", got)
	}
	// A send that names only the session: the plugin resolves the active window.
	if code, body, _ := postSend(t, ts.URL, ui.MirrorSendPath, sessionCookieFor(s), "", url.Values{"host": {"ws"}, "session": {"scratch2"}, "text": {"hi"}}); code != http.StatusNoContent {
		t.Fatalf("a send naming only the session: %d %q", code, body)
	}
	if got := asked(); got[3] != "" {
		t.Fatalf("a send naming only the session passed the plugin window %q; resolving it is the plugin's", got[3])
	}
}

// TestAWindowParameterThatIsNotAWindowIDIsNotFound: `window` is a tmux window
// id or it is nothing. An index, a name or junk is a 404 on every route that
// takes one — never an address that silently means "the active window" — and a
// well-formed id the host does not list gets the plugin's own refusal (409).
func TestAWindowParameterThatIsNotAWindowIDIsNotFound(t *testing.T) {
	fm := goldHost()
	s, ts := viewServer(t, fm)
	for _, bad := range []string{"8", "@", "@x", "Gold:8", "@7;kill-server", " @7"} {
		q := url.Values{"host": {"ws"}, "session": {"scratch2"}, "window": {bad}}.Encode()
		for _, p := range []string{"/mirror/terminal?", "/mirror/view?"} {
			if resp, _ := mirrorGet(t, s, ts, p+q); resp.StatusCode != http.StatusNotFound {
				t.Errorf("GET %s%s = %d, want 404", p, q, resp.StatusCode)
			}
		}
		for _, p := range []string{"/mirror/terminal/ws?", "/mirror/view/ws?"} {
			if _, resp, err := dialTerm(t, s, ts, p+q); err == nil || resp == nil || resp.StatusCode != http.StatusNotFound {
				t.Errorf("dial %s%s did not 404 (%v)", p, q, err)
			}
		}
		for _, p := range []string{ui.MirrorTerminalSendPath, ui.MirrorSendPath} {
			if code, _, _ := postSend(t, ts.URL, p, sessionCookieFor(s), "", url.Values{"host": {"ws"}, "session": {"scratch2"}, "window": {bad}, "text": {"hi"}}); code != http.StatusNotFound {
				t.Errorf("POST %s window=%q = %d, want 404", p, bad, code)
			}
		}
	}
	fm.mu.Lock()
	reached := len(fm.asked)
	fm.mu.Unlock()
	if reached != 0 {
		t.Fatalf("a malformed window reached the plugin %d time(s)", reached)
	}
	// Well-formed, not listed: the plugin's refusal, as a 409.
	fm.v2.pasteErr = mirror.ErrWindowGone
	code, body, _ := postSend(t, ts.URL, ui.MirrorTerminalSendPath, sessionCookieFor(s), "", url.Values{"host": {"ws"}, "session": {"scratch2"}, "window": {"@99"}, "text": {"hi"}})
	if code != http.StatusConflict || !strings.Contains(body, "no longer lists this tmux window") {
		t.Fatalf("a send to a window the host no longer lists: %d %q, want 409 saying so", code, body)
	}
}
