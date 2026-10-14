package api

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// THE COMPOSER'S TWO DOORS, through the real mux: a send from the composer's
// script (a fetch) is answered as it always was, and the SAME send submitted
// by the browser itself — the script did not run — is answered with a redirect
// back to the view and a notice, or with the plain confirm page when the
// sign-in is stale.
// ---------------------------------------------------------------------------

func sendForm(session, text string) url.Values {
	return url.Values{"host": {"ws"}, "session": {session}, "text": {text}}
}

func pastesOf(fm *fakeMirror) []pasteCall {
	_, p := fm.v2.snapshot()
	return p
}

// TestASendTheBrowserSubmitsIsDeliveredAndRedirectedBackWithANotice is the
// no-script path, measured against the script's on the same server.
//
// 🔴 A REGRESSION TEST: before this a navigation POST was answered exactly like
// a fetch — 204 No Content, on which a browser stays on the page it was on and
// shows nothing — and for a stale sign-in with a bare 403 text page. The 303s
// and their Location, and the notice on the page they lead to, all fail there.
func TestASendTheBrowserSubmitsIsDeliveredAndRedirectedBackWithANotice(t *testing.T) {
	r := newStepUpRig(t)
	r.mirror.v2 = &fakeMirrorV2{}
	at, rt, _ := r.fake.Mint(r.owner)

	for path, back := range map[string]string{
		ui.MirrorSendPath:         ui.MirrorViewPath("ws", "main"),
		ui.MirrorTerminalSendPath: ui.MirrorTerminalPath("ws", "main"),
	} {
		before := len(pastesOf(r.mirror))
		w := do(r.edge, http.MethodPost, path, sendForm("main", "typed with no script"), withCookies(at, rt), asNavigation)
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != back+"&notice=sent" {
			t.Fatalf("%s as a navigation = %d → %q; want 303 → %s&notice=sent", path, w.Code, w.Header().Get("Location"), back)
		}
		if got := pastesOf(r.mirror); len(got) != before+1 || got[len(got)-1].text != "typed with no script" {
			t.Fatalf("%s as a navigation delivered %+v", path, got[before:])
		}
		// The page the redirect leads to says so, with no script involved.
		page := do(r.edge, http.MethodGet, w.Header().Get("Location"), nil, withCookies(at, rt), asNavigation).Body.String()
		if !strings.Contains(page, `data-ok=""`) || !strings.Contains(page, ">Sent.</p>") {
			t.Errorf("%s: the page after the redirect does not say the message was sent", path)
		}
		// CONTROL — the script's door is unchanged: 204, no redirect.
		x := do(r.edge, http.MethodPost, path, sendForm("main", "typed with the script"), withCookies(at, rt), asXHR)
		if x.Code != http.StatusNoContent || x.Header().Get("Location") != "" {
			t.Fatalf("%s as a fetch = %d (Location %q); want 204 and no redirect", path, x.Code, x.Header().Get("Location"))
		}
	}

	// A refusal is a redirect too, carrying its own notice — and delivers nothing.
	n := len(pastesOf(r.mirror))
	w := do(r.edge, http.MethodPost, ui.MirrorTerminalSendPath, sendForm("main", "   "), withCookies(at, rt), asNavigation)
	if w.Code != http.StatusSeeOther || !strings.HasSuffix(w.Header().Get("Location"), "&notice=empty") {
		t.Fatalf("an empty send as a navigation = %d → %q; want 303 … notice=empty", w.Code, w.Header().Get("Location"))
	}
	page := do(r.edge, http.MethodGet, w.Header().Get("Location"), nil, withCookies(at, rt), asNavigation).Body.String()
	if !strings.Contains(page, ">"+ui.MirrorComposeEmpty+"</p>") || strings.Contains(page[strings.Index(page, `id="mv-note"`):strings.Index(page, `id="mv-form"`)], "data-ok") {
		t.Error("the page after an empty send does not say so (or says it as good news)")
	}
	r.mirror.v2.pasteErr = mirror.ErrHostOffline
	w = do(r.edge, http.MethodPost, ui.MirrorSendPath, sendForm("main", "to a host that left"), withCookies(at, rt), asNavigation)
	if w.Code != http.StatusSeeOther || !strings.HasSuffix(w.Header().Get("Location"), "&notice=offline") {
		t.Fatalf("a send to an offline host as a navigation = %d → %q; want 303 … notice=offline", w.Code, w.Header().Get("Location"))
	}
	r.mirror.v2.pasteErr = nil
	if got := len(pastesOf(r.mirror)); got != n+1 { // the offline one reached the plugin; the empty one did not
		t.Fatalf("the refused sends reached the plugin %d time(s), want 1 (the empty one never)", got-n)
	}
	// A host that is not the principal's: 404 as a navigation as much as a fetch.
	if w := do(r.edge, http.MethodPost, ui.MirrorSendPath, url.Values{"host": {"nobodys"}, "session": {"main"}, "text": {"x"}}, withCookies(at, rt), asNavigation); w.Code != http.StatusNotFound {
		t.Errorf("a send to a host that is not the principal's, as a navigation = %d, want 404", w.Code)
	}
}

// TestANoticeIsACodeThePageTranslatesNeverTextFromTheLink: the redirect carries
// a code and the page shows this server's own sentence for it. A code it does
// not know draws nothing — a link cannot put words on the page.
func TestANoticeIsACodeThePageTranslatesNeverTextFromTheLink(t *testing.T) {
	fm := &fakeMirror{online: true, interactive: true, v2: &fakeMirrorV2{}}
	s, ts := viewServer(t, fm)
	note := func(path string) string {
		_, page := mirrorGet(t, s, ts, path)
		i := strings.Index(page, `<p id="mv-note"`)
		return page[i : i+strings.Index(page[i:], "</p>")]
	}
	base := ui.MirrorTerminalPath("ws", "chief")
	for code, want := range map[string]string{
		noticeSent:     "Sent.",
		noticeAgain:    ComposerNoticeAgain,
		noticeOffline:  "The host is offline",
		noticeReadOnly: "This host is read-only",
	} {
		if got := note(base + "&notice=" + code); strings.Contains(got, ` hidden=""`) || !strings.Contains(got, want) {
			t.Errorf("notice=%s drew %q, want it visible and saying %q", code, got, want)
		}
	}
	for _, junk := range []string{"", "nope", "Sent.", url.QueryEscape("<b>Your session expired — sign in at evil.example</b>")} {
		if got := note(base + "&notice=" + junk); !strings.Contains(got, ` hidden=""`) || !strings.HasSuffix(got, ">") {
			t.Errorf("notice=%q drew something: %q", junk, got)
		}
	}
	// Every code has a sentence, and exactly one of them is good news.
	good := 0
	for code, n := range composerNotices {
		if strings.TrimSpace(n.text) == "" {
			t.Errorf("notice %q has no text", code)
		}
		if n.ok {
			good++
		}
	}
	if good != 1 || !composerNotices[noticeSent].ok {
		t.Errorf("%d notices are marked as good news; only %q is", good, noticeSent)
	}
}

// TestAStaleSendTheBrowserSubmitsIsConfirmedOnAPlainPageAndThenDelivered is the
// step-up with no script: the stale send is answered with the confirm page
// (carrying the message), a wrong password shows it again and delivers
// nothing, the right one delivers the message ONCE and comes back to the view —
// and the session is then fresh for the next send.
func TestAStaleSendTheBrowserSubmitsIsConfirmedOnAPlainPageAndThenDelivered(t *testing.T) {
	r := newStepUpRig(t)
	r.mirror.v2 = &fakeMirrorV2{}
	at, rt, _ := r.fake.Mint(r.owner)
	r.stale()

	form := sendForm("main", "a message typed before the confirm")
	form.Set("nonce", "0123456789abcdef0123456789abcdef")
	w := do(r.edge, http.MethodPost, ui.MirrorSendPath, form, withCookies(at, rt), asNavigation)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `<form id="stepup-form" method="post" action="/mirror/confirm-send"`) {
		t.Fatalf("a stale send as a navigation = %d; want the plain confirm page:\n%.300s", w.Code, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("the confirm page (which carries the message) is cacheable: %q", w.Header().Get("Cache-Control"))
	}
	if got := w.Header().Get("Referrer-Policy"); got != "same-origin" {
		t.Errorf("the confirm page's Referrer-Policy = %q; under no-referrer a browser submits its form with `Origin: null`, which is refused", got)
	}
	page := w.Body.String()
	for _, want := range []string{`name="kind" value="view"`, `name="host" value="ws"`, `name="session" value="main"`,
		`name="text" value="a message typed before the confirm"`, `name="nonce" value="0123456789abcdef0123456789abcdef"`, "from the last 12 hours."} {
		if !strings.Contains(page, want) {
			t.Errorf("the confirm page does not carry %s", want)
		}
	}
	if len(pastesOf(r.mirror)) != 0 {
		t.Fatal("a stale send was delivered before the confirm")
	}
	// CONTROL: the same stale send from the script is the 403 it always was.
	if x := r.send(at, rt, "from the script"); x.Code != http.StatusForbidden || x.Header().Get("X-Muster-Step-Up") == "" {
		t.Fatalf("a stale send from the script = %d; want 403 + X-Muster-Step-Up", x.Code)
	}

	confirm := url.Values{"kind": {"view"}, "host": {"ws"}, "session": {"main"}, "text": {"a message typed before the confirm"},
		"nonce": {"0123456789abcdef0123456789abcdef"}}
	confirm.Set("password", "not the password")
	w = do(r.edge, http.MethodPost, mirrorConfirmSendPath, confirm, withCookies(at, rt), asNavigation)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), stepUpWrongPassword) || !strings.Contains(w.Body.String(), `name="text" value="a message typed before the confirm"`) {
		t.Fatalf("a wrong password = %d; want the confirm page again, saying so, still carrying the message", w.Code)
	}
	if len(pastesOf(r.mirror)) != 0 {
		t.Fatal("a wrong password delivered the message")
	}
	// (This request may carry cookies of its own — the stale session's access
	// token was refreshed on the way in — so "no cookie" is not the test.) A
	// wrong password did not make the session fresh: the same send is still
	// answered with the confirm page.
	again := do(r.edge, http.MethodPost, ui.MirrorSendPath, form, withCookies(at, rt), asNavigation)
	if again.Code != http.StatusOK || !strings.Contains(again.Body.String(), `id="stepup-form"`) || len(pastesOf(r.mirror)) != 0 {
		t.Fatalf("after a wrong password the session is treated as fresh: %d", again.Code)
	}

	confirm.Set("password", stepUpOwnerPW)
	w = do(r.edge, http.MethodPost, mirrorConfirmSendPath, confirm, withCookies(at, rt), asNavigation)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != ui.MirrorViewPath("ws", "main")+"&notice=sent" {
		t.Fatalf("the right password = %d → %q; want 303 back to the view with notice=sent\n%.300s", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	got := pastesOf(r.mirror)
	if len(got) != 1 || got[0].text != "a message typed before the confirm" || got[0].target != mirror.PasteClaude || got[0].user != 1 {
		t.Fatalf("after the confirm the plugin was sent %+v; want the pending message once, to the Claude pane, as user 1", got)
	}
	fresh := setCookies(w)
	if fresh[accessCookieName] == nil || fresh[refreshCookieName] == nil {
		t.Fatal("the confirm set no new session")
	}
	// That session is fresh: the next send needs no confirm.
	next := do(r.edge, http.MethodPost, ui.MirrorSendPath, sendForm("main", "the next one"),
		withCookies(fresh[accessCookieName].Value, fresh[refreshCookieName].Value), asNavigation)
	if next.Code != http.StatusSeeOther || !strings.HasSuffix(next.Header().Get("Location"), "notice=sent") {
		t.Fatalf("a send on the just-confirmed session = %d → %q", next.Code, next.Header().Get("Location"))
	}
	// The terminal's kind confirms into the ACTIVE pane.
	r.stale()
	confirm.Set("kind", "terminal")
	confirm.Set("nonce", "")
	if w = do(r.edge, http.MethodPost, mirrorConfirmSendPath, confirm, withCookies(at, rt), asNavigation); w.Code != http.StatusSeeOther ||
		w.Header().Get("Location") != ui.MirrorTerminalPath("ws", "main")+"&notice=sent" {
		t.Fatalf("a terminal confirm = %d → %q", w.Code, w.Header().Get("Location"))
	}
	if last := pastesOf(r.mirror); last[len(last)-1].target != mirror.PasteActivePane {
		t.Fatalf("a terminal confirm asked for target %v", last[len(last)-1].target)
	}
}

// TestTheConfirmPagesRouteKeepsEveryGate: the route that completes a pending
// send is a write like any other. It refuses a cross-origin post, another
// user's host (404), a read-only host, an unknown kind, and — the one that
// matters — it delivers NOTHING without the password, whatever else it is
// given.
func TestTheConfirmPagesRouteKeepsEveryGate(t *testing.T) {
	r := newStepUpRig(t)
	r.mirror.v2 = &fakeMirrorV2{}
	at, rt, _ := r.fake.Mint(r.owner)
	form := url.Values{"kind": {"view"}, "host": {"ws"}, "session": {"main"}, "text": {"hi"}, "password": {stepUpOwnerPW}}

	cross := func(req *http.Request) { req.Header.Set("Origin", "https://evil.example") }
	if w := do(r.edge, http.MethodPost, mirrorConfirmSendPath, form, withCookies(at, rt), asNavigation, cross); w.Code != http.StatusForbidden {
		t.Errorf("a cross-origin confirm = %d, want 403", w.Code)
	}
	if w := do(r.edge, http.MethodPost, mirrorConfirmSendPath, form, asNavigation); w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "/login") {
		t.Errorf("a confirm with no session = %d → %q, want a redirect to sign in", w.Code, w.Header().Get("Location"))
	}
	bad := url.Values{}
	for k, v := range form {
		bad[k] = v
	}
	bad.Set("kind", "everything")
	if w := do(r.edge, http.MethodPost, mirrorConfirmSendPath, bad, withCookies(at, rt), asNavigation); w.Code != http.StatusBadRequest {
		t.Errorf("an unknown kind = %d, want 400", w.Code)
	}
	bad.Set("kind", "view")
	bad.Set("host", "somebody-elses")
	if w := do(r.edge, http.MethodPost, mirrorConfirmSendPath, bad, withCookies(at, rt), asNavigation); w.Code != http.StatusNotFound {
		t.Errorf("a host that is not the principal's = %d, want 404", w.Code)
	}
	bad.Set("host", "ws")
	bad.Del("password")
	if w := do(r.edge, http.MethodPost, mirrorConfirmSendPath, bad, withCookies(at, rt), asNavigation); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), stepUpWrongPassword) {
		t.Errorf("a confirm with no password = %d, want the page again", w.Code)
	}
	if n := len(pastesOf(r.mirror)); n != 0 {
		t.Fatalf("a refused confirm delivered %d message(s)", n)
	}
	// A read-only host: nothing is confirmed, nothing delivered.
	r.mirror.interactive = false
	if w := do(r.edge, http.MethodPost, mirrorConfirmSendPath, form, withCookies(at, rt), asNavigation); w.Code != http.StatusSeeOther || !strings.HasSuffix(w.Header().Get("Location"), "notice=read-only") {
		t.Errorf("a confirm for a read-only host = %d → %q", w.Code, w.Header().Get("Location"))
	}
	if n := len(pastesOf(r.mirror)); n != 0 {
		t.Fatalf("a confirm for a read-only host delivered %d message(s)", n)
	}
}

// TestTheSameMessagePostedTwiceIsDeliveredOnce: the nonce. The composer's
// script hands a send whose answer was lost to the browser, which posts it
// again — and the first post may have been pasted. The same nonce and text is
// delivered once and answered as sent both times; a different text under the
// same nonce is a different message; and a delivery that FAILED releases its
// nonce, so trying again works.
func TestTheSameMessagePostedTwiceIsDeliveredOnce(t *testing.T) {
	fm := &fakeMirror{online: true, interactive: true, v2: &fakeMirrorV2{}}
	s, ts := viewServer(t, fm)
	post := func(text, nonce string) int {
		f := sendForm("main", text)
		if nonce != "" {
			f.Set("nonce", nonce)
		}
		code, _, _ := postSend(t, ts.URL, ui.MirrorSendPath, sessionCookieFor(s), "", f)
		return code
	}
	const n1, n2 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	for i := 0; i < 3; i++ {
		if code := post("run the tests", n1); code != http.StatusNoContent {
			t.Fatalf("post #%d of the same message = %d, want 204 each time", i+1, code)
		}
	}
	if got := pastesOf(fm); len(got) != 1 {
		t.Fatalf("the same message posted 3 times was delivered %d time(s)", len(got))
	}
	// A different text under the same nonce, the same text under another nonce,
	// and no nonce at all (an older page): each is its own message.
	post("run the tests again", n1)
	post("run the tests", n2)
	post("run the tests", "")
	post("run the tests", "")
	post("run the tests", "short") // not a usable nonce: no protection, never a refusal
	post("run the tests", "short")
	if got := pastesOf(fm); len(got) != 7 {
		t.Fatalf("after 6 distinct-or-unprotected posts the plugin saw %d deliveries in all, want 7", len(got))
	}
	// A failed delivery is not remembered.
	fm.v2.pasteErr = mirror.ErrHostOffline
	if code := post("when the host is back", n2); code != http.StatusServiceUnavailable {
		t.Fatalf("a send to an offline host = %d", code)
	}
	fm.v2.pasteErr = nil
	before := len(pastesOf(fm))
	if code := post("when the host is back", n2); code != http.StatusNoContent || len(pastesOf(fm)) != before+1 {
		t.Fatalf("retrying a FAILED send with its nonce = %d, delivered %d; want 204 and one delivery", code, len(pastesOf(fm))-before)
	}
}

// TestSendNoncesForgetAndAreBounded: the table behind the nonce is time-bound
// and size-bound, per owner.
func TestSendNoncesForgetAndAreBounded(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	n := &sendNonces{now: func() time.Time { return now }}
	const nonce = "cccccccccccccccccccccccccccccccc"
	claim := func(owner int64, text string) bool {
		repeat, finish := n.begin(owner, nonce, text)
		finish(true)
		return repeat
	}
	if claim(1, "x") || !claim(1, "x") {
		t.Fatal("the second claim of one message was not a repeat")
	}
	if claim(2, "x") {
		t.Fatal("another owner's same nonce and text was taken for a repeat")
	}
	// In flight (begun, not finished) is a repeat too: two posts at once deliver once.
	repeat, finish := n.begin(1, nonce, "in flight")
	if repeat {
		t.Fatal("the first claim was a repeat")
	}
	if r2, _ := n.begin(1, nonce, "in flight"); !r2 {
		t.Fatal("a second post while the first is being delivered was not a repeat")
	}
	finish(false)
	if r3, f3 := n.begin(1, nonce, "in flight"); r3 {
		t.Fatal("a FAILED delivery kept its claim")
	} else {
		f3(true)
	}
	// Past the TTL it is forgotten.
	now = now.Add(sendNonceTTL + time.Second)
	if claim(1, "x") {
		t.Fatal("a message was still remembered past the TTL")
	}
	// The table does not grow without bound.
	for i := 0; i < sendNonceMax+500; i++ {
		now = now.Add(time.Millisecond)
		claim(3, "msg-"+string(rune('a'+i%26))+time.Duration(i).String())
	}
	if len(n.seen) > sendNonceMax {
		t.Fatalf("the nonce table holds %d entries, over its bound of %d", len(n.seen), sendNonceMax)
	}
}

// TestASendThatComesBackAsAGETIsSentBackToItsView: a POST made through a
// sign-in at a forward-auth edge returns to its URL as a GET with no body. The
// composer's action carries the address in its query for this, and the GET
// route sends the browser back to the view it came from, told the message was
// not sent. Another user's host is not found.
func TestASendThatComesBackAsAGETIsSentBackToItsView(t *testing.T) {
	fm := goldHost()
	s, ts := viewServer(t, fm)
	nine := ref("scratch2", "@9")
	// The form's own action, as the page drew it.
	_, page := mirrorGet(t, s, ts, nine.ViewPath())
	open := tagOf(page, "mv-form")
	action := strings.ReplaceAll(open[strings.Index(open, `action="`)+8:], "&amp;", "&")
	action = action[:strings.Index(action, `"`)]
	resp, _ := mirrorGet(t, s, ts, action)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != nine.ViewPath()+"&notice=again" {
		t.Fatalf("GET %s = %d → %q; want 303 → %s&notice=again", action, resp.StatusCode, resp.Header.Get("Location"), nine.ViewPath())
	}
	_, back := mirrorGet(t, s, ts, resp.Header.Get("Location"))
	if !strings.Contains(back, ComposerNoticeAgain) {
		t.Error("the view it is sent back to does not say the message was not sent")
	}
	resp, _ = mirrorGet(t, s, ts, ui.MirrorTerminalSendPath+"?host=ws&session=solo&window=%402")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != ref("solo", "@2").TerminalPath()+"&notice=again" {
		t.Fatalf("the terminal's GET = %d → %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	for _, q := range []string{"?host=nobody&session=main", "?host=ws&session=scratch2&window=9", ""} {
		if resp, _ := mirrorGet(t, s, ts, ui.MirrorSendPath+q); resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s%s = %d, want 404", ui.MirrorSendPath, q, resp.StatusCode)
		}
	}
	if len(pastesOf(fm)) != 0 {
		t.Fatal("a GET delivered a message")
	}
}

// TestAFurtherRedirectKeepsTheNotice: a send's redirect goes back to the
// address the send named, and that page may answer with a redirect of its own
// — an address naming only the session, to its active window; a window with no
// rendered view, to its Terminal. The notice travels with each, or the person
// is told nothing after all; an unknown code is still dropped.
func TestAFurtherRedirectKeepsTheNotice(t *testing.T) {
	s, ts := viewServer(t, goldHost())
	for _, tc := range []struct{ from, wantPrefix string }{
		// Session only → its active window, on each page.
		{ui.MirrorViewPath("ws", "scratch2"), "/mirror/"},
		{ui.MirrorTerminalPath("ws", "scratch2"), "/mirror/terminal?"},
		// No rendered view → the Terminal of the same window.
		{ref("scratch2", "@4").ViewPath(), ref("scratch2", "@4").TerminalPath()},
	} {
		resp, _ := mirrorGet(t, s, ts, tc.from+"&notice=sent")
		loc := resp.Header.Get("Location")
		if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(loc, tc.wantPrefix) || !strings.Contains(loc, "window=") {
			t.Fatalf("fixture: GET %s&notice=sent = %d → %q; want a redirect to %s… naming a window", tc.from, resp.StatusCode, loc, tc.wantPrefix)
		}
		if !strings.HasSuffix(loc, "&notice=sent") {
			t.Errorf("GET %s&notice=sent → %q: the further redirect dropped the notice", tc.from, loc)
		}
		resp, _ = mirrorGet(t, s, ts, tc.from+"&notice=nope")
		if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusSeeOther || strings.Contains(loc, "notice") {
			t.Errorf("GET %s&notice=nope = %d → %q: an unknown code was carried on", tc.from, resp.StatusCode, loc)
		}
	}
}

// TestAViewsDocumentIsNeverStored: a view's page is its markup and its inline
// script together, bound to the routes of the build that rendered it. Each is
// served `Cache-Control: no-store`, so neither the HTTP cache nor the
// back/forward cache can show one build's document against another's server.
func TestAViewsDocumentIsNeverStored(t *testing.T) {
	s, ts := viewServer(t, goldHost())
	for _, p := range []string{ref("scratch2", "@7").ViewPath(), ref("scratch2", "@4").TerminalPath()} {
		resp, _ := mirrorGet(t, s, ts, p)
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("GET %s = %d, Cache-Control %q; want 200 and no-store", p, resp.StatusCode, resp.Header.Get("Cache-Control"))
		}
		// Through the whole middleware chain, which sets no-referrer on
		// everything: a page with a form the browser may submit itself must
		// override it, or that submit carries `Origin: null` and is refused.
		if got := resp.Header.Get("Referrer-Policy"); got != "same-origin" {
			t.Errorf("GET %s: Referrer-Policy = %q, want same-origin", p, got)
		}
	}
}
