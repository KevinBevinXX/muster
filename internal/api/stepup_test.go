package api

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/auth"
	"github.com/ZacxDev/muster/internal/gotrue/gotruetest"
	"github.com/ZacxDev/muster/internal/stepup"
	"github.com/ZacxDev/muster/internal/ui"
	"github.com/ZacxDev/muster/internal/users"
)

// The step-up gate, end to end through the real GoTrue session tier (over the
// fake GoTrue, whose `amr` follows what v2.189.0 and v2.197.0 were measured to do): a mirrored
// terminal attaches INTERACTIVELY only on a session that signed in within
// stepup.MaxAge, and POST /auth/confirm is how a stale one becomes fresh.

const (
	stepUpOwnerPW = "owner password, correct"
	stepUpOtherPW = "the other user's password"
)

type stepUpRig struct {
	*gtFixture
	mirror *fakeMirror
	ts     *httptest.Server
	owner  string // GoTrue sub of user 1, who owns fakeMirror's host "ws"
}

// newStepUpRig is a gotrue-mode server with the mirror plugin played by
// fakeMirror (host "ws", user 1's, interactive), served on a real listener so
// the socket upgrade is the real one. User 1 is owner@, user 2 other@.
func newStepUpRig(t *testing.T) *stepUpRig {
	t.Helper()
	fake := gotruetest.New(t, gtSecret)
	f := &gtFixture{fake: fake, store: newMemUsers()}
	f.s = New(nil, AuthConfig{Mode: AuthModeGoTrue, HookToken: gtHook, PublicURL: "https://muster.example.com"},
		log.New(io.Discard, "", 0))
	fm := &fakeMirror{online: true, interactive: true}
	f.s.UseExtensions(Extensions{
		Notes:           stubNotesStore{},
		Agents:          mirrorAgents{},
		SessionLiveness: stubLiveness{},
		Provisioner:     newFakeTerminal(),
		Mirror:          fm,
	})
	f.s.UseGoTrue(GoTrueAuth{Client: fake.Client(), Verifier: fake.Verifier(), Users: f.store})
	f.edge = BrowserListener(f.s.Handler())
	ts := httptest.NewServer(f.edge)
	t.Cleanup(ts.Close)
	owner := fake.AddUser("owner@example.com", stepUpOwnerPW)
	f.store.add(users.User{ID: 1, GotrueSub: owner, Email: "owner@example.com"})
	other := fake.AddUser("other@example.com", stepUpOtherPW)
	f.store.add(users.User{ID: 2, GotrueSub: other, Email: "other@example.com"})
	return &stepUpRig{gtFixture: f, mirror: fm, ts: ts, owner: owner}
}

// dial opens the mirror terminal socket for ws/main with the session cookies.
func (r *stepUpRig) dial(t *testing.T, at, rt, mode string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	h := http.Header{}
	h.Set("Cookie", (&http.Cookie{Name: accessCookieName, Value: at}).String()+"; "+(&http.Cookie{Name: refreshCookieName, Value: rt}).String())
	h.Set("Origin", r.ts.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	path := ui.MirrorTerminalWSPath("ws", "main")
	if mode != "" {
		path += "&mode=" + mode
	}
	return websocket.Dial(ctx, "ws"+strings.TrimPrefix(r.ts.URL, "http")+path, &websocket.DialOptions{HTTPHeader: h})
}

// mustAttach dials and waits for "ready", returning the mode the server
// attached; it fails the test on any refusal.
func (r *stepUpRig) mustAttach(t *testing.T, at, rt, mode string) string {
	t.Helper()
	c, resp, err := r.dial(t, at, rt, mode)
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("mode=%q: dial refused (%d): %v", mode, code, err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	m, _ := readUntil(t, c, "ready")
	return m["mode"]
}

// mustRefuse dials and asserts the upgrade is refused with code, before any
// attach.
func (r *stepUpRig) mustRefuse(t *testing.T, at, rt, mode string, code int, what string) {
	t.Helper()
	c, resp, err := r.dial(t, at, rt, mode)
	if err == nil {
		c.Close(websocket.StatusNormalClosure, "")
		t.Fatalf("%s: the socket upgraded; want a %d refusal", what, code)
	}
	if resp == nil || resp.StatusCode != code {
		got := 0
		if resp != nil {
			got = resp.StatusCode
		}
		t.Fatalf("%s: refused with %d (%v), want %d", what, got, err, code)
	}
}

func (r *stepUpRig) rwCalls() int {
	calls, _ := r.mirror.snapshot()
	n := 0
	for _, c := range calls {
		if !c.readOnly {
			n++
		}
	}
	return n
}

// TestAStaleSessionCannotAttachAMirrorInteractively is the server-side gate:
// a session that signed in more than stepup.MaxAge ago gets 403 for mode=rw —
// before the upgrade, so the plugin is never asked — and still watches
// read-only. A fresh session (the control) attaches rw.
func TestAStaleSessionCannotAttachAMirrorInteractively(t *testing.T) {
	r := newStepUpRig(t)
	at, rt, _ := r.fake.Mint(r.owner)

	if got := r.mustAttach(t, at, rt, "rw"); got != "rw" {
		t.Fatalf("control: a fresh session attached %q, want rw", got)
	}
	if n := r.rwCalls(); n != 1 {
		t.Fatalf("control: the plugin saw %d rw attaches, want 1", n)
	}

	// Past MaxAge — and STILL inside the access token's lifetime (15 min), so
	// the session itself is perfectly valid. Only the sign-in is old.
	r.fake.Advance(stepup.MaxAge + time.Minute)
	r.mustRefuse(t, at, rt, "rw", http.StatusForbidden, "a stale session's rw attach")
	if n := r.rwCalls(); n != 1 {
		t.Fatalf("a stale rw attach reached the plugin (%d rw attaches)", n)
	}
	if got := r.mustAttach(t, at, rt, ""); got != "ro" {
		t.Fatalf("a stale session must still WATCH: attached %q", got)
	}
}

// TestARefreshDoesNotMakeASessionFresh: the access token is re-minted by the
// refresh grant (new iat), but the sign-in it records (amr) is not — so a
// stale session stays stale across a refresh. This is the guard against
// reading freshness off `iat`.
func TestARefreshDoesNotMakeASessionFresh(t *testing.T) {
	r := newStepUpRig(t)
	at, rt, _ := r.fake.Mint(r.owner)
	// Past the access token's 15 min lifetime: the request must refresh.
	r.fake.Advance(16 * time.Minute)
	w := do(r.edge, http.MethodGet, ui.MirrorTerminalPath("ws", "main"), nil, withCookies(at, rt), asNavigation)
	if w.Code != http.StatusOK {
		t.Fatalf("the page on a refreshed session: %d", w.Code)
	}
	fresh := setCookies(w)[accessCookieName]
	if fresh == nil || fresh.Value == at {
		t.Fatal("fixture: the request did not refresh the access token")
	}
	c, err := r.fake.Verifier().Verify(fresh.Value)
	if err != nil {
		t.Fatal(err)
	}
	old, _ := r.fake.Verifier().Verify(at)
	if c.IssuedAt <= old.IssuedAt {
		t.Fatalf("fixture: the refreshed token's iat %d did not move past %d", c.IssuedAt, old.IssuedAt)
	}
	if !strings.Contains(w.Body.String(), `data-fresh-for="0"`) {
		t.Fatalf("a refreshed-but-stale session was told it is fresh: %s", attrOf(w.Body.String(), "data-fresh-for"))
	}
	r.mustRefuse(t, fresh.Value, setCookies(w)[refreshCookieName].Value, "rw", http.StatusForbidden, "a refreshed stale session's rw attach")
}

// TestASessionWithNoAMRIsNeverFresh: a token GoTrue minted with no `amr` has
// no sign-in time, and unknown is stale — even seconds after signing in.
func TestASessionWithNoAMRIsNeverFresh(t *testing.T) {
	r := newStepUpRig(t)
	_, rt, sid := r.fake.Mint(r.owner)
	r.fake.SetSessionAMR(sid, nil)
	sess, err := r.fake.Client().Refresh(context.Background(), "203.0.113.9", rt)
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := r.fake.Verifier().Verify(sess.AccessToken); len(c.AMR) != 0 {
		t.Fatalf("fixture: the token still carries amr %+v", c.AMR)
	}
	r.mustRefuse(t, sess.AccessToken, sess.RefreshToken, "rw", http.StatusForbidden, "an amr-less session's rw attach")
	if got := r.mustAttach(t, sess.AccessToken, sess.RefreshToken, ""); got != "ro" {
		t.Fatalf("control: the amr-less session's read-only attach = %q", got)
	}
}

// TestAnotherOwnersHostStaysInvisibleWhateverTheFreshness: user 2 does not own
// "ws". Fresh or stale, rw or ro, the socket is a 404 — never a 403 that would
// say "this exists, sign in again".
func TestAnotherOwnersHostStaysInvisibleWhateverTheFreshness(t *testing.T) {
	r := newStepUpRig(t)
	other, _ := r.store.ByID(context.Background(), 2)
	at, rt, _ := r.fake.Mint(other.GotrueSub)
	for _, mode := range []string{"rw", ""} {
		r.mustRefuse(t, at, rt, mode, http.StatusNotFound, "user 2, fresh, mode="+mode)
	}
	r.fake.Advance(stepup.MaxAge + time.Minute)
	r.mustRefuse(t, at, rt, "rw", http.StatusNotFound, "user 2, stale, rw")
	if calls, _ := r.mirror.snapshot(); len(calls) != 0 {
		t.Fatalf("the plugin was asked to attach for a non-owner: %+v", calls)
	}
}

// confirm POSTs the step-up confirm with the session cookies.
func (r *stepUpRig) confirm(at, rt string, body url.Values, opts ...reqOpt) *httptest.ResponseRecorder {
	return do(r.edge, http.MethodPost, stepUpConfirmPath, body, append([]reqOpt{withCookies(at, rt), asXHR}, opts...)...)
}

func errorOf(w *httptest.ResponseRecorder) string {
	var m map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	s, _ := m["error"].(string)
	return s
}

// TestConfirmingThePasswordMakesAStaleSessionFresh is the whole loop: stale →
// rw refused → confirm with the right password → new cookies whose sign-in is
// now → rw attaches. The confirm's answer says how long that lasts.
func TestConfirmingThePasswordMakesAStaleSessionFresh(t *testing.T) {
	r := newStepUpRig(t)
	at, rt, _ := r.fake.Mint(r.owner)
	r.fake.Advance(stepup.MaxAge + time.Minute)
	r.mustRefuse(t, at, rt, "rw", http.StatusForbidden, "stale before confirming")

	w := r.confirm(at, rt, url.Values{"password": {stepUpOwnerPW}})
	if w.Code != http.StatusOK {
		t.Fatalf("confirm with the right password: %d %s", w.Code, w.Body)
	}
	var res stepUpResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || res.FreshFor <= 0 || res.FreshFor > stepup.MaxAge.Milliseconds() {
		t.Fatalf("confirm answered %s; want 0 < fresh_for <= %d", w.Body, stepup.MaxAge.Milliseconds())
	}
	ck := setCookies(w)
	nat, nrt := ck[accessCookieName], ck[refreshCookieName]
	if nat == nil || nrt == nil || nat.Value == at {
		t.Fatalf("confirm did not replace the session cookies: %v", ck)
	}
	c, err := r.fake.Verifier().Verify(nat.Value)
	if err != nil || c.Subject != r.owner {
		t.Fatalf("the new session is %q (%v), want the same user %q", c.Subject, err, r.owner)
	}
	if got := r.mustAttach(t, nat.Value, nrt.Value, "rw"); got != "rw" {
		t.Fatalf("after confirming, the rw attach = %q", got)
	}
}

// TestTheConfirmRefusesTheWrongPasswordAndAnotherUsersPassword: the password is
// checked against the SESSION's user, so another account's real password is
// just a wrong password here. Neither sets a cookie, and the session stays
// stale.
func TestTheConfirmRefusesTheWrongPasswordAndAnotherUsersPassword(t *testing.T) {
	r := newStepUpRig(t)
	at, rt, _ := r.fake.Mint(r.owner)
	first, _ := r.fake.Verifier().Verify(at)
	signedIn, _ := first.AuthenticatedAt()
	r.fake.Advance(stepup.MaxAge + time.Minute)
	for name, body := range map[string]url.Values{
		"wrong password":         {"password": {"not it"}},
		"another user's":         {"password": {stepUpOtherPW}},
		"another user's + email": {"password": {stepUpOtherPW}, "email": {"other@example.com"}},
		"empty":                  {"password": {""}},
	} {
		w := r.confirm(at, rt, body, fromIP("203.0.113."+strconv.Itoa(len(name))))
		if w.Code != http.StatusUnauthorized || errorOf(w) != stepUpWrongPassword {
			t.Fatalf("%s: %d %s, want 401 %q", name, w.Code, w.Body, stepUpWrongPassword)
		}
		// The session wrapper may have REFRESHED the cookies (the backoffs below
		// outlive the access token); what must not happen is a NEW sign-in.
		if ck := setCookies(w)[accessCookieName]; ck != nil {
			c, _ := r.fake.Verifier().Verify(ck.Value)
			if got, _ := c.AuthenticatedAt(); !got.Equal(signedIn) {
				t.Fatalf("%s: a refused confirm issued a session signed in at %v, not the original %v", name, got, signedIn)
			}
		}
		// Each refusal is followed by the backoff it earned; wait it out so the
		// next case is judged on its password, not on the throttle.
		r.fake.Advance(accountBackoffMax)
	}
	// at/rt may have been rotated by the refreshes above; a fresh pair for the
	// SAME stale sign-in is a refresh away.
	if sess, err := r.fake.Client().Refresh(context.Background(), "203.0.113.9", rt); err == nil {
		at, rt = sess.AccessToken, sess.RefreshToken
	}

	// A password in the QUERY STRING is not read (it would be in the access log).
	w := r.confirm(at, rt, nil, func(q *http.Request) { q.URL.RawQuery = "password=" + url.QueryEscape(stepUpOwnerPW) })
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("a query-string password was accepted: %d", w.Code)
	}
}

// TestTheConfirmIsSameOriginAndNeedsASession.
func TestTheConfirmIsSameOriginAndNeedsASession(t *testing.T) {
	r := newStepUpRig(t)
	at, rt, _ := r.fake.Mint(r.owner)
	before := r.fake.CountCalls(http.MethodPost, "/token")
	w := r.confirm(at, rt, url.Values{"password": {stepUpOwnerPW}}, func(q *http.Request) { q.Header.Set("Origin", "https://evil.example") })
	if w.Code != http.StatusForbidden || errorOf(w) != CrossOriginRefusal {
		t.Fatalf("a cross-site confirm: %d %s", w.Code, w.Body)
	}
	if r.fake.CountCalls(http.MethodPost, "/token") != before {
		t.Fatal("a cross-site confirm still asked GoTrue")
	}
	w = do(r.edge, http.MethodPost, stepUpConfirmPath, url.Values{"password": {stepUpOwnerPW}}, asXHR)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("a confirm with no session: %d", w.Code)
	}
	if r.fake.CountCalls(http.MethodPost, "/token") != before {
		t.Fatal("a sessionless confirm asked GoTrue")
	}
	// Control: the same request, same-origin, is judged.
	if w := r.confirm(at, rt, url.Values{"password": {stepUpOwnerPW}}); w.Code != http.StatusOK {
		t.Fatalf("control: a same-origin confirm: %d %s", w.Code, w.Body)
	}
}

// TestTheConfirmIsThrottledLikeSignIn: it shares the sign-in limiter, keyed on
// the session's ACCOUNT. After accountFreeFailures wrong passwords the next
// attempt — even the right password — is 429 WITHOUT asking GoTrue, and /login
// for the same account is throttled by the same record.
func TestTheConfirmIsThrottledLikeSignIn(t *testing.T) {
	r := newStepUpRig(t)
	at, rt, _ := r.fake.Mint(r.owner)
	// The backoff is armed by the failure AFTER the free ones.
	for i := 0; i <= accountFreeFailures; i++ {
		if w := r.confirm(at, rt, url.Values{"password": {"guess " + strconv.Itoa(i)}}, fromIP("198.51.100."+strconv.Itoa(i+1))); w.Code != http.StatusUnauthorized {
			t.Fatalf("guess %d: %d %s", i, w.Code, w.Body)
		}
	}
	calls := r.fake.CountCalls(http.MethodPost, "/token")
	w := r.confirm(at, rt, url.Values{"password": {stepUpOwnerPW}}, fromIP("198.51.100.99"))
	if w.Code != http.StatusTooManyRequests || errorOf(w) != stepUpThrottled {
		t.Fatalf("past the free failures: %d %s, want 429", w.Code, w.Body)
	}
	if r.fake.CountCalls(http.MethodPost, "/token") != calls {
		t.Fatal("a throttled confirm still asked GoTrue")
	}
	lw := do(r.edge, http.MethodPost, "/login", url.Values{"email": {"owner@example.com"}, "password": {stepUpOwnerPW}}, fromIP("198.51.100.98"))
	if lw.Code != http.StatusTooManyRequests {
		t.Fatalf("/login for the same account after confirm failures: %d, want 429 (one limiter)", lw.Code)
	}
	// Control: once the backoff has passed, the right password confirms.
	r.fake.Advance(accountBackoffMax)
	if w := r.confirm(at, rt, url.Values{"password": {stepUpOwnerPW}}, fromIP("198.51.100.97")); w.Code != http.StatusOK {
		t.Fatalf("control: after the backoff the right password: %d %s", w.Code, w.Body)
	}
}

// TestTheTerminalPageCarriesTheFreshnessWindowAndTheConfirmRoute: a mirrored
// session's page names the confirm route and how long the sign-in stays fresh
// (a duration, measured server-side); the agent pod's terminal has no step-up.
func TestTheTerminalPageCarriesTheFreshnessWindowAndTheConfirmRoute(t *testing.T) {
	r := newStepUpRig(t)
	at, rt, _ := r.fake.Mint(r.owner)
	r.fake.Advance(time.Minute)
	w := do(r.edge, http.MethodGet, ui.MirrorTerminalPath("ws", "main"), nil, withCookies(at, rt), asNavigation)
	page := w.Body.String()
	if !strings.Contains(page, `data-step-up="`+stepUpConfirmPath+`"`) || !strings.Contains(page, `<dialog id="term-confirm"`) {
		t.Fatalf("the mirrored page has no step-up wiring")
	}
	ff, _ := strconv.ParseInt(attrOf(page, "data-fresh-for"), 10, 64)
	// amr is whole seconds, so up to one second less than MaxAge minus the
	// minute since sign-in.
	want := (stepup.MaxAge - time.Minute).Milliseconds()
	if ff > want || ff <= want-1000 {
		t.Fatalf("data-fresh-for = %d, want (%d, %d]", ff, want-1000, want)
	}
	if !strings.Contains(page, `data-remember="mirror:ws"`) {
		t.Fatalf("the Interactive choice is not remembered per host: %s", attrOf(page, "data-remember"))
	}
	agentPage := ui.AgentTerminalPage(ui.AgentTerminalView{Name: "x"})
	var b strings.Builder
	_ = agentPage.Render(&b)
	if strings.Contains(b.String(), `data-step-up="`) || strings.Contains(b.String(), `<dialog id="term-confirm"`) {
		t.Fatal("an agent pod's terminal page grew a step-up")
	}
}

// TestPasswordModeConfirmsWithTheOperatorPassword: the same route in password
// mode re-checks MUSTER_UI_PASSWORD and re-issues the session cookie, whose
// issue time is what the gate reads.
func TestPasswordModeConfirmsWithTheOperatorPassword(t *testing.T) {
	fm := &fakeMirror{online: true, interactive: true}
	s, ts := mirrorServer(t, fm, newFakeTerminal())
	old := auth.SignSession(s.auth.uiSessionSecret(), time.Now().Add(-stepup.MaxAge-time.Minute), time.Hour)
	stale := func(r *http.Request) { r.Header.Set("Cookie", sessionCookieName+"="+old) }

	// Stale: rw refused, ro fine.
	h := http.Header{}
	h.Set("Cookie", sessionCookieName+"="+old)
	h.Set("Origin", ts.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + ui.MirrorTerminalWSPath("ws", "main")
	if c, resp, err := websocket.Dial(ctx, wsURL+"&mode=rw", &websocket.DialOptions{HTTPHeader: h}); err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		if c != nil {
			c.Close(websocket.StatusNormalClosure, "")
		}
		t.Fatalf("password mode, stale cookie: rw attach was not 403 (%v)", err)
	}

	w := do(s.Handler(), http.MethodPost, stepUpConfirmPath, url.Values{"password": {"wrong"}}, stale, asXHR)
	if w.Code != http.StatusUnauthorized || len(setCookies(w)) != 0 {
		t.Fatalf("wrong operator password: %d, cookies %v", w.Code, setCookies(w))
	}
	w = do(s.Handler(), http.MethodPost, stepUpConfirmPath, url.Values{"password": {testUIPassword}}, stale, asXHR)
	ck := setCookies(w)[sessionCookieName]
	if w.Code != http.StatusOK || ck == nil {
		t.Fatalf("right operator password: %d %s", w.Code, w.Body)
	}
	h.Set("Cookie", sessionCookieName+"="+ck.Value)
	c, _, err := websocket.Dial(ctx, wsURL+"&mode=rw", &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatalf("after confirming, rw: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	if m, _ := readUntil(t, c, "ready"); m["mode"] != "rw" {
		t.Fatalf("after confirming, attached %q", m["mode"])
	}
}

// attrOf is the first value of attr="…" in markup.
func attrOf(markup, attr string) string {
	i := strings.Index(markup, attr+`="`)
	if i < 0 {
		return ""
	}
	rest := markup[i+len(attr)+2:]
	return rest[:strings.Index(rest, `"`)]
}
