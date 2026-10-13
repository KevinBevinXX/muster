package api

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// THE TERMINAL VIEW'S COMPOSER: a message box for a mirrored session whose pane
// is NOT Claude Code (a shell, another TUI) — which has no rendered view and so,
// before this, no way to be sent a message at all.
//
// 🔴 EVERY EXPECTATION HERE IS A LITERAL (the route, the markup, the messages),
// so this file also compiles and runs against the commit before the feature,
// where the route answers 404 and the page draws no composer — the red half of
// the PR's matrix. The fake host's "chief" session is the non-Claude one
// ("main" is the Claude pane).

const (
	termSendPath  = "/mirror/terminal/send"
	viewSendPath  = "/mirror/view/send"
	chiefTermPage = "/mirror/terminal?host=ws&session=chief"
	emptySendText = "nothing to send: type a message first"
)

// postSend POSTs a composer form as the server's signed-in operator (cookie
// "" = no session) from origin ("" = the server's own).
func postSend(t *testing.T, base, path, cookie, origin string, form url.Values) (int, string, http.Header) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	if origin == "" {
		origin = base
	}
	req.Header.Set("Origin", origin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(body)), resp.Header
}

// tagOf is the opening tag of the element with id in markup, "" when absent.
func tagOf(markup, id string) string {
	i := strings.Index(markup, `id="`+id+`"`)
	if i < 0 {
		return ""
	}
	tag := markup[strings.LastIndex(markup[:i], "<"):]
	return tag[:strings.Index(tag, ">")]
}

var hasText16px = regexp.MustCompile(`class="[^"]*\btext-base\b`)

// TestTheTerminalViewHasAComposerForASessionThatIsNotClaude: on an interactive
// host the Terminal page of a NON-Claude session draws the composer — posting
// to the terminal's own send route, enabled, with a 16px input (iOS zooms on
// anything smaller) — while the page still opens READ-ONLY: the composer does
// not need the Interactive switch. A read-only host draws none at all, and
// neither does an offline one.
func TestTheTerminalViewHasAComposerForASessionThatIsNotClaude(t *testing.T) {
	s, ts := viewServer(t, &fakeMirror{online: true, interactive: true, v2: &fakeMirrorV2{}})
	_, page := mirrorGet(t, s, ts, chiefTermPage)
	if strings.Contains(page, `id="view-rendered"`) {
		t.Fatal("fixture: chief has a rendered view; this test is about a session with none")
	}
	form := tagOf(page, "mv-form")
	if form == "" {
		t.Fatalf("a non-Claude session's Terminal page has no composer")
	}
	for _, want := range []string{`data-send="/mirror/terminal/send"`, `data-host="ws"`, `data-session="chief"`} {
		if !strings.Contains(form, want) {
			t.Errorf("the composer form lacks %s: %s", want, form)
		}
	}
	input, send := tagOf(page, "mv-input"), tagOf(page, "mv-send")
	if input == "" || send == "" || disabledAttr.MatchString(input) || disabledAttr.MatchString(send) {
		t.Errorf("the composer is not enabled: %s / %s", input, send)
	}
	if !strings.Contains(input, `placeholder="Message…"`) {
		t.Errorf("the input's placeholder: %s", input)
	}
	if !hasText16px.MatchString(input) {
		t.Errorf("the input is not 16px (text-base); iOS would zoom the page on focus: %s", input)
	}
	// The page opens read-only and the composer is there regardless.
	if mode := tagOf(page, "term-mode"); !strings.Contains(mode, `aria-checked="false"`) {
		t.Errorf("the page does not open read-only: %s", mode)
	}
	// A send refused as stale needs the dialog to ask through.
	if !strings.Contains(page, `<dialog id="term-confirm"`) {
		t.Error("the page has a composer but no confirm dialog")
	}
	// The Claude session's Terminal page has one too (the same pane).
	if _, main := mirrorGet(t, s, ts, mainPage); !strings.Contains(tagOf(main, "mv-form"), `data-send="/mirror/terminal/send"`) {
		t.Error("the Claude session's Terminal page has no composer")
	}

	for name, fm := range map[string]*fakeMirror{
		"a read-only host": {online: true, interactive: false, v2: &fakeMirrorV2{}},
		"an offline host":  {online: false, interactive: true, v2: &fakeMirrorV2{}},
	} {
		s, ts := viewServer(t, fm)
		_, page := mirrorGet(t, s, ts, chiefTermPage)
		if !strings.Contains(page, `id="term"`) {
			t.Fatalf("%s: fixture: not the terminal page", name)
		}
		for _, id := range []string{"mv-form", "mv-input", "mv-send", "mv-compose"} {
			if tagOf(page, id) != "" {
				t.Errorf("%s: the Terminal page draws #%s", name, id)
			}
		}
	}
}

// TestASendFromTheTerminalViewIsPastedWithoutAnInteractiveAttach: the text is
// handed to the plugin for the session it names, as typed — and NO terminal was
// attached interactively to do it (the composer is not the switch).
func TestASendFromTheTerminalViewIsPastedWithoutAnInteractiveAttach(t *testing.T) {
	fm := &fakeMirror{online: true, interactive: true, v2: &fakeMirrorV2{}}
	s, ts := viewServer(t, fm)
	code, body, _ := postSend(t, ts.URL, termSendPath, sessionCookieFor(s), "", url.Values{"host": {"ws"}, "session": {"chief"}, "text": {"ls -la /srv && !!"}})
	if code != http.StatusNoContent {
		t.Fatalf("a send from the Terminal view: %d %q, want 204", code, body)
	}
	_, pastes := fm.v2.snapshot()
	if len(pastes) != 1 || pastes[0].user != 1 || pastes[0].host != "ws" || pastes[0].session != "chief" || pastes[0].text != "ls -la /srv && !!" {
		t.Fatalf("pasted %+v, want one paste of the text into ws/chief for user 1", pastes)
	}
	if calls, _ := fm.snapshot(); len(calls) != 0 {
		t.Fatalf("a send attached a terminal: %+v", calls)
	}
}

// TestTheTerminalSendIsGatedLikeTheRenderedSend: 🔴 the same gates, on the new
// route, each refusing before the plugin is asked — a read-only host, a stale
// sign-in (with the header the page answers by asking to confirm), another
// origin, no session, a session that does not exist on the host. The last row
// is the control.
func TestTheTerminalSendIsGatedLikeTheRenderedSend(t *testing.T) {
	type tc struct {
		name        string
		interactive bool
		cookie      string // "fresh" | "stale" | ""
		origin      string
		session     string
		status      int
		pasted      bool
	}
	for _, c := range []tc{
		{"a read-only host", false, "fresh", "", "chief", http.StatusConflict, false},
		{"a stale sign-in", true, "stale", "", "chief", http.StatusForbidden, false},
		{"another origin", true, "fresh", "https://evil.example", "chief", http.StatusForbidden, false},
		{"no session", true, "", "", "chief", http.StatusUnauthorized, false},
		{"a session name that is not one", true, "fresh", "", "ch\x07ief", http.StatusNotFound, false},
		{"interactive + fresh + same origin", true, "fresh", "", "chief", http.StatusNoContent, true},
	} {
		fm := &fakeMirror{online: true, interactive: c.interactive, v2: &fakeMirrorV2{}}
		s, ts := viewServer(t, fm)
		cookie := ""
		switch c.cookie {
		case "fresh":
			cookie = sessionCookieFor(s)
		case "stale":
			cookie = staleCookieFor(s)
		}
		code, body, hdr := postSend(t, ts.URL, termSendPath, cookie, c.origin, url.Values{"host": {"ws"}, "session": {c.session}, "text": {"whoami"}})
		_, pastes := fm.v2.snapshot()
		if code != c.status || (len(pastes) == 1) != c.pasted {
			t.Errorf("%s: %d %q, pasted %+v; want %d pasted=%v", c.name, code, body, pastes, c.status, c.pasted)
		}
		if c.cookie == "stale" && (hdr.Get("X-Muster-Step-Up") != "required" || !strings.Contains(body, "confirm it's you")) {
			t.Errorf("%s: the refusal does not ask for a fresh sign-in: %q (X-Muster-Step-Up=%q)", c.name, body, hdr.Get("X-Muster-Step-Up"))
		}
	}
}

// TestAnEmptySendSaysSoOnBothRoutes: nothing typed (or only whitespace) is a
// 400 whose text tells the person what to do — on the Terminal view's route and
// on the rendered view's alike, since they are one handler — and nothing is
// pasted. The control is a real message on each route.
func TestAnEmptySendSaysSoOnBothRoutes(t *testing.T) {
	for _, r := range []struct{ path, session string }{{termSendPath, "chief"}, {viewSendPath, "main"}} {
		fm := &fakeMirror{online: true, interactive: true, v2: &fakeMirrorV2{}}
		s, ts := viewServer(t, fm)
		for _, text := range []string{"", "   ", "\n\t "} {
			code, body, _ := postSend(t, ts.URL, r.path, sessionCookieFor(s), "", url.Values{"host": {"ws"}, "session": {r.session}, "text": {text}})
			if code != http.StatusBadRequest || body != emptySendText {
				t.Errorf("%s with text %q: %d %q, want 400 %q", r.path, text, code, body, emptySendText)
			}
		}
		if _, pastes := fm.v2.snapshot(); len(pastes) != 0 {
			t.Errorf("%s: an empty send was pasted: %+v", r.path, pastes)
		}
		if code, body, _ := postSend(t, ts.URL, r.path, sessionCookieFor(s), "", url.Values{"host": {"ws"}, "session": {r.session}, "text": {"x"}}); code != http.StatusNoContent {
			t.Errorf("control: %s with a message: %d %q", r.path, code, body)
		}
	}
}

// TestAnotherUsersHostCannotBeSentToFromTheTerminalView: user 2 does not own
// "ws". Fresh or not, the send is a 404 — never a 403 that would say "this
// exists, confirm it's you" — and the plugin is not asked. The control is the
// owner's own send, on the same server.
func TestAnotherUsersHostCannotBeSentToFromTheTerminalView(t *testing.T) {
	r := newStepUpRig(t)
	r.mirror.v2 = &fakeMirrorV2{}
	form := url.Values{"host": {"ws"}, "session": {"chief"}, "text": {"id"}}
	oat, ort, _ := r.fake.Mint(r.owner)
	if w := do(r.edge, http.MethodPost, termSendPath, form, withCookies(oat, ort), asXHR); w.Code != http.StatusNoContent {
		t.Fatalf("control: the owner's send: %d %s", w.Code, w.Body)
	}
	other, _ := r.store.ByID(context.Background(), 2)
	at, rt, _ := r.fake.Mint(other.GotrueSub)
	if w := do(r.edge, http.MethodPost, termSendPath, form, withCookies(at, rt), asXHR); w.Code != http.StatusNotFound {
		t.Fatalf("user 2's send to user 1's host: %d %s, want 404", w.Code, w.Body)
	}
	if _, pastes := r.mirror.v2.snapshot(); len(pastes) != 1 || pastes[0].user != 1 {
		t.Fatalf("pasted %+v, want only the owner's", pastes)
	}
}
