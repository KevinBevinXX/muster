package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/gotrue"
	"github.com/ZacxDev/muster/internal/gotrue/gotruetest"
	"github.com/ZacxDev/muster/internal/users"
)

// The gotrue-mode session tier (accounts PR 4), driven against the in-memory
// GoTrue (internal/gotrue/gotruetest) on its controllable clock. The e2e suite
// drives the same path against the real GoTrue image.

var gtSecret = []byte(strings.Repeat("g", 40))

const gtHook = "gt-hook-token-for-tests-only-0123456789"

// memUsers is an in-memory UserStore. RedeemClaim holds one mutex for its
// whole body, which serialises it the way the Postgres row lock does; the
// concurrency test uses the real store instead.
type memUsers struct {
	mu       sync.Mutex
	rows     map[int64]*users.User
	invites  map[string]*memInvite
	bindFail error
	// signOutErr makes RecordSignOutEverywhere fail.
	signOutErr error
	// lookupErr makes BySub fail as a database outage would.
	lookupErr error
	// lookupFailures makes only the next N BySub calls fail (a blip).
	lookupFailures int
	// stallNext, if set, is called ONCE by the next BySub after it has read
	// its row and before it returns it (outside the lock): a read in flight
	// across whatever the test does meanwhile.
	stallNext func()
}

type memInvite struct {
	claim    int64
	expires  time.Time
	redeemed bool
}

func newMemUsers() *memUsers {
	return &memUsers{rows: map[int64]*users.User{
		1: {ID: 1, DisplayName: "operator"},
	}, invites: map[string]*memInvite{}}
}

func (m *memUsers) add(u users.User) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := u
	m.rows[u.ID] = &cp
}

func (m *memUsers) BySub(_ context.Context, sub string) (users.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lookupErr != nil {
		return users.User{}, m.lookupErr
	}
	if m.lookupFailures > 0 {
		m.lookupFailures--
		return users.User{}, errors.New("simulated database blip")
	}
	for _, u := range m.rows {
		if u.GotrueSub == sub && sub != "" {
			row, stall := *u, m.stallNext
			m.stallNext = nil
			if stall != nil {
				m.mu.Unlock()
				stall()
				m.mu.Lock()
			}
			return row, nil
		}
	}
	return users.User{}, users.ErrNotFound
}

func (m *memUsers) IDByEmail(_ context.Context, email string) (int64, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lookupErr != nil {
		return 0, false, m.lookupErr
	}
	for _, u := range m.rows {
		if u.Email != "" && strings.EqualFold(u.Email, strings.TrimSpace(email)) {
			return u.ID, true, nil
		}
	}
	return 0, false, nil
}

func (m *memUsers) DisabledAmong(_ context.Context, ids []int64) (map[int64]bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lookupErr != nil {
		return nil, m.lookupErr
	}
	out := map[int64]bool{}
	for _, id := range ids {
		if u, ok := m.rows[id]; ok {
			out[id] = u.DisabledAt != nil
		}
	}
	return out, nil
}

func (m *memUsers) ByID(_ context.Context, id int64) (users.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if u, ok := m.rows[id]; ok {
		return *u, nil
	}
	return users.User{}, users.ErrNotFound
}

func (m *memUsers) RecordSignOutEverywhere(_ context.Context, id int64) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.rows[id]
	if !ok {
		return time.Time{}, users.ErrNotFound
	}
	if m.signOutErr != nil {
		return time.Time{}, m.signOutErr
	}
	now := time.Now()
	if u.SignedOutAt == nil || now.After(*u.SignedOutAt) {
		u.SignedOutAt = &now
	}
	return *u.SignedOutAt, nil
}

func (m *memUsers) SetDisabled(_ context.Context, id int64, disabled bool) (users.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.rows[id]
	if !ok {
		return users.User{}, users.ErrNotFound
	}
	if disabled {
		now := time.Now()
		u.DisabledAt = &now
	} else {
		u.DisabledAt = nil
	}
	return *u, nil
}

func (m *memUsers) ClaimInviteLive(_ context.Context, h []byte) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inv, ok := m.invites[string(h)]
	return ok && !inv.redeemed && time.Now().Before(inv.expires) && m.rows[inv.claim].GotrueSub == "", nil
}

func (m *memUsers) CreateClaimInvite(_ context.Context, h []byte, claim, _ int64, exp time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.invites[string(h)] = &memInvite{claim: claim, expires: exp}
	return nil
}

func (m *memUsers) RedeemClaim(ctx context.Context, h []byte, email string,
	create func(context.Context) (string, error), undo func(string)) (users.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inv, ok := m.invites[string(h)]
	if !ok || inv.redeemed || !time.Now().Before(inv.expires) || m.rows[inv.claim].GotrueSub != "" {
		return users.User{}, users.ErrInvalidInvite
	}
	sub, err := create(ctx)
	if err != nil {
		return users.User{}, err
	}
	if m.bindFail != nil {
		undo(sub)
		return users.User{}, m.bindFail
	}
	u := m.rows[inv.claim]
	u.GotrueSub, u.Email = sub, email
	inv.redeemed = true
	return *u, nil
}

type gtFixture struct {
	s       *Server
	fake    *gotruetest.Fake
	store   *memUsers
	edge    http.Handler // the browser listener
	machine http.Handler // the machine-token listener
	seen    []authz.Principal
	mu      sync.Mutex
}

func newGoTrueFixture(t *testing.T) *gtFixture {
	t.Helper()
	return newGoTrueFixtureWith(t, nil)
}

// newGoTrueFixtureWith is newGoTrueFixture with ext wired (nil = none) before
// the handler is built.
func newGoTrueFixtureWith(t *testing.T, ext *Extensions) *gtFixture {
	t.Helper()
	fake := gotruetest.New(t, gtSecret)
	f := &gtFixture{fake: fake, store: newMemUsers()}
	f.s = New(nil, AuthConfig{Mode: AuthModeGoTrue, HookToken: gtHook, PublicURL: "https://muster.example.com"},
		log.New(io.Discard, "", 0))
	if ext != nil {
		f.s.UseExtensions(*ext)
	}
	f.s.UseGoTrue(GoTrueAuth{Client: fake.Client(), Verifier: fake.Verifier(), Users: f.store})
	f.s.principalProbe = func(r *http.Request) {
		p, _ := authz.From(r.Context())
		f.mu.Lock()
		f.seen = append(f.seen, p)
		f.mu.Unlock()
	}
	h := f.s.Handler()
	f.edge, f.machine = BrowserListener(h), h
	return f
}

// member adds a GoTrue user bound to a new muster row and returns
// (muster id, gotrue sub).
// member's role argument documents the caller's intent; the principal's role
// comes from roleOf, which with no users store wired is admin for user 1 and
// member for everyone else.
func (f *gtFixture) member(email string, id int64, _ authz.Role) string {
	sub := f.fake.AddUser(email, "correct horse battery")
	f.store.add(users.User{ID: id, GotrueSub: sub, Email: email})
	return sub
}

func (f *gtFixture) principals() []authz.Principal {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]authz.Principal(nil), f.seen...)
}

type reqOpt func(*http.Request)

func withCookies(at, rt string) reqOpt {
	return func(r *http.Request) {
		if at != "" {
			r.AddCookie(&http.Cookie{Name: accessCookieName, Value: at})
		}
		if rt != "" {
			r.AddCookie(&http.Cookie{Name: refreshCookieName, Value: rt})
		}
	}
}

func asNavigation(r *http.Request) { r.Header.Set("Sec-Fetch-Dest", "document") }
func asXHR(r *http.Request)        { r.Header.Set("HX-Request", "true") }
func fromIP(ip string) reqOpt {
	return func(r *http.Request) { r.Header.Set("CF-Connecting-IP", ip) }
}

func do(h http.Handler, method, target string, body url.Values, opts ...reqOpt) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != nil {
		rd = strings.NewReader(body.Encode())
	}
	r := httptest.NewRequest(method, target, rd)
	if body != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if method == http.MethodPost {
		// muster's own forms are same-origin; a test that wants a cross-site
		// POST overrides this with an option.
		r.Header.Set("Origin", "http://"+r.Host)
	}
	for _, o := range opts {
		o(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func setCookies(w *httptest.ResponseRecorder) map[string]*http.Cookie {
	out := map[string]*http.Cookie{}
	for _, c := range w.Result().Cookies() {
		out[c.Name] = c
	}
	return out
}

func TestAGoTrueSessionActsAsItsUserWithTheUsersRole(t *testing.T) {
	f := newGoTrueFixture(t)
	sub := f.member("m@example.com", 7, authz.RoleMember)
	at, rt, _ := f.fake.Mint(sub)
	w := do(f.edge, "GET", "/account", nil, withCookies(at, rt))
	if w.Code != http.StatusOK {
		t.Fatalf("a valid session: %d %s", w.Code, w.Body)
	}
	ps := f.principals()
	if len(ps) != 1 || ps[0].UserID != 7 || ps[0].Role != authz.RoleMember || ps[0].Via != authz.ViaSession {
		t.Fatalf("principal = %+v, want user 7 / member / session", ps)
	}
	if !strings.Contains(w.Body.String(), "m@example.com") {
		t.Fatalf("the account page does not name the signed-in user:\n%s", w.Body)
	}
	if len(setCookies(w)) != 0 {
		t.Fatalf("a request with a live access token set cookies: %v", setCookies(w))
	}
}

// TestTamperedAccessTokensAreRefusedAndNotRefreshed: alg, signature, issuer,
// audience and a revoked session each END the session — even with a perfectly
// good refresh cookie beside them, nothing is refreshed.
func TestTamperedAccessTokensAreRefusedAndNotRefreshed(t *testing.T) {
	f := newGoTrueFixture(t)
	sub := f.member("m@example.com", 7, authz.RoleMember)
	good, rt, _ := f.fake.Mint(sub)
	parts := strings.Split(good, ".")
	claims := gotrue.Claims{}
	pb, _ := base64.RawURLEncoding.DecodeString(parts[1])
	_ = json.Unmarshal(pb, &claims)

	resign := func(mut func(*gotrue.Claims), secret []byte) string {
		c := claims
		mut(&c)
		tok, err := gotrue.Sign(secret, c)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	noneHdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	cases := map[string]string{
		"alg none":       noneHdr + "." + parts[1] + ".",
		"alg none + sig": noneHdr + "." + parts[1] + "." + parts[2],
		"bad signature":  parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString([]byte("not-the-signature-at-all-xxxxxxx")),
		"other secret":   resign(func(*gotrue.Claims) {}, []byte(strings.Repeat("x", 40))),
		"wrong issuer":   resign(func(c *gotrue.Claims) { c.Issuer = "evil" }, gtSecret),
		"wrong audience": resign(func(c *gotrue.Claims) { c.Audience = gotrue.Audience{"service_role"} }, gtSecret),
		"other subject":  resign(func(c *gotrue.Claims) { c.Subject = "00000000-0000-4000-a000-000000000000" }, gtSecret),
	}
	for name, tok := range cases {
		t.Run(name, func(t *testing.T) {
			before := f.fake.CountCalls("POST", "/token")
			w := do(f.edge, "GET", "/account", nil, withCookies(tok, rt), asXHR)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("%s: %d, want 401", name, w.Code)
			}
			if n := f.fake.CountCalls("POST", "/token") - before; n != 0 {
				t.Fatalf("%s: a refused access token triggered %d refresh call(s)", name, n)
			}
			if c := setCookies(w)[accessCookieName]; c == nil || c.MaxAge >= 0 {
				t.Fatalf("%s: the refused cookie was not cleared", name)
			}
		})
	}
	// Positive control: the untampered token is admitted by the same fixture.
	if w := do(f.edge, "GET", "/account", nil, withCookies(good, rt)); w.Code != http.StatusOK {
		t.Fatalf("positive control: the good token was refused: %d", w.Code)
	}
}

func TestAnExpiredAccessTokenIsRefreshedServerSide(t *testing.T) {
	f := newGoTrueFixture(t)
	sub := f.member("m@example.com", 7, authz.RoleMember)
	f.fake.SetAccessTTL(60 * time.Second)
	at, rt, _ := f.fake.Mint(sub)
	// Past exp + 30 s skew.
	f.fake.Advance(95 * time.Second)

	w := do(f.edge, "GET", "/account", nil, withCookies(at, rt))
	if w.Code != http.StatusOK {
		t.Fatalf("expired access + live refresh: %d %s", w.Code, w.Body)
	}
	if n := f.fake.CountCalls("POST", "/token"); n != 1 {
		t.Fatalf("refresh calls = %d, want 1", n)
	}
	ck := setCookies(w)
	nat, nrt := ck[accessCookieName], ck[refreshCookieName]
	if nat == nil || nrt == nil || nat.Value == at || nrt.Value == rt {
		t.Fatalf("new cookies were not set: %v", ck)
	}
	for _, c := range []*http.Cookie{nat, nrt} {
		if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" {
			t.Fatalf("cookie %s lacks HttpOnly/Secure/Lax/Path=/: %+v", c.Name, c)
		}
	}
	if _, err := f.fake.Verifier().Verify(nat.Value); err != nil {
		t.Fatalf("the refreshed access token does not verify: %v", err)
	}
	// The new pair works on its own, with no further refresh.
	if w := do(f.edge, "GET", "/account", nil, withCookies(nat.Value, nrt.Value)); w.Code != http.StatusOK {
		t.Fatalf("the refreshed pair: %d", w.Code)
	}
	if n := f.fake.CountCalls("POST", "/token"); n != 1 {
		t.Fatalf("a fresh access token was refreshed again (%d calls)", n)
	}
	// Just INSIDE the skew the old token is still accepted without a refresh
	// (the other side of the boundary measured above).
	at2, rt2, _ := f.fake.Mint(sub)
	f.fake.Advance(85 * time.Second)
	if w := do(f.edge, "GET", "/account", nil, withCookies(at2, rt2)); w.Code != http.StatusOK || len(setCookies(w)) != 0 {
		t.Fatalf("within skew: %d, cookies %v", w.Code, setCookies(w))
	}
}

func TestARefreshFailureSendsANavigationToLoginAndAnAPICallA401(t *testing.T) {
	f := newGoTrueFixture(t)
	sub := f.member("m@example.com", 7, authz.RoleMember)
	f.fake.SetAccessTTL(60 * time.Second)
	at, _, _ := f.fake.Mint(sub)
	f.fake.Advance(2 * time.Minute)

	nav := do(f.edge, "GET", "/account?tab=x", nil, withCookies(at, "unknown-refresh-token"), asNavigation)
	if nav.Code != http.StatusSeeOther || !strings.HasPrefix(nav.Header().Get("Location"), "/login?next=") {
		t.Fatalf("navigation: %d Location=%q, want 303 to /login?next=", nav.Code, nav.Header().Get("Location"))
	}
	if c := setCookies(nav)[refreshCookieName]; c == nil || c.MaxAge >= 0 {
		t.Fatal("a failed refresh did not clear the refresh cookie")
	}
	api := do(f.edge, "GET", "/account", nil, withCookies(at, "unknown-refresh-token"), asXHR)
	if api.Code != http.StatusUnauthorized {
		t.Fatalf("XHR: %d, want 401", api.Code)
	}
	// No cookies at all is the same refusal.
	if w := do(f.edge, "GET", "/account", nil, asXHR); w.Code != http.StatusUnauthorized {
		t.Fatalf("no cookies: %d", w.Code)
	}
}

// TestConcurrentRequestsShareOneRefresh: a page load's parallel requests all
// carry the same expired pair; exactly ONE refresh reaches GoTrue and every
// request is handed the same new pair.
func TestConcurrentRequestsShareOneRefresh(t *testing.T) {
	f := newGoTrueFixture(t)
	sub := f.member("m@example.com", 7, authz.RoleMember)
	f.fake.SetAccessTTL(60 * time.Second)
	at, rt, _ := f.fake.Mint(sub)
	f.fake.Advance(2 * time.Minute)

	const n = 8
	var wg sync.WaitGroup
	got := make([]string, n)
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := do(f.edge, "GET", "/account", nil, withCookies(at, rt))
			codes[i] = w.Code
			if c := setCookies(w)[refreshCookieName]; c != nil {
				got[i] = c.Value
			}
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if codes[i] != http.StatusOK || got[i] == "" || got[i] != got[0] {
			t.Fatalf("request %d: code %d, refresh cookie %q (first %q)", i, codes[i], got[i], got[0])
		}
	}
	if c := f.fake.CountCalls("POST", "/token"); c != 1 {
		t.Fatalf("%d concurrent requests made %d refresh calls, want 1", n, c)
	}
}

func TestADisabledUserIsRefusedOnTheNextRequest(t *testing.T) {
	f := newGoTrueFixture(t)
	sub := f.member("m@example.com", 7, authz.RoleMember)
	at, rt, _ := f.fake.Mint(sub)
	if w := do(f.edge, "GET", "/account", nil, withCookies(at, rt)); w.Code != http.StatusOK {
		t.Fatalf("before disable: %d", w.Code) // also warms the 30 s user cache
	}
	if err := f.s.DisableUser(context.Background(), 7, true); err != nil {
		t.Fatal(err)
	}
	if w := do(f.edge, "GET", "/account", nil, withCookies(at, rt), asXHR); w.Code != http.StatusUnauthorized {
		t.Fatalf("the next request after a disable: %d, want 401", w.Code)
	}
	if u, _ := f.fake.User(sub); !u.Banned {
		t.Fatal("disabling did not ban the GoTrue user")
	}
	// Let the cache take the DISABLED row, then re-enable: the very next
	// request must see it (DisableUser drops the cache in both directions).
	f.fake.SetAccessTTL(time.Hour)
	at2, rt2, _ := f.fake.Mint(sub)
	f.fake.Advance(userCacheTTL + time.Second)
	if w := do(f.edge, "GET", "/account", nil, withCookies(at2, rt2), asXHR); w.Code != http.StatusUnauthorized {
		t.Fatalf("disabled, cache refreshed: %d", w.Code)
	}
	if err := f.s.DisableUser(context.Background(), 7, false); err != nil {
		t.Fatal(err)
	}
	if w := do(f.edge, "GET", "/account", nil, withCookies(at2, rt2)); w.Code != http.StatusOK {
		t.Fatalf("the request right after a re-enable: %d, want 200 (stale cache)", w.Code)
	}
	w := do(f.edge, "POST", "/login", url.Values{"email": {"m@example.com"}, "password": {"correct horse battery"}})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("after re-enable, sign-in: %d %s", w.Code, w.Body)
	}
}

func TestLogoutRevokesTheSession(t *testing.T) {
	f := newGoTrueFixture(t)
	f.member("m@example.com", 7, authz.RoleMember)
	login := func() (string, string) {
		w := do(f.edge, "POST", "/login", url.Values{"email": {"m@example.com"}, "password": {"correct horse battery"}})
		ck := setCookies(w)
		if w.Code != http.StatusSeeOther || ck[accessCookieName] == nil || ck[refreshCookieName] == nil {
			t.Fatalf("sign-in: %d %v", w.Code, ck)
		}
		return ck[accessCookieName].Value, ck[refreshCookieName].Value
	}
	at, rt := login()
	c, err := f.fake.Verifier().Verify(at)
	if err != nil {
		t.Fatal(err)
	}
	w := do(f.edge, "POST", "/logout", url.Values{}, withCookies(at, rt))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != loginPath {
		t.Fatalf("logout: %d %q", w.Code, w.Header().Get("Location"))
	}
	if ck := setCookies(w); ck[accessCookieName] == nil || ck[accessCookieName].MaxAge >= 0 {
		t.Fatal("logout did not clear the cookies")
	}
	if !f.fake.SessionRevoked(c.SessionID) {
		t.Fatal("logout did not revoke the GoTrue session")
	}
	// The still-unexpired access token is refused by muster itself.
	if w := do(f.edge, "GET", "/account", nil, withCookies(at, ""), asXHR); w.Code != http.StatusUnauthorized {
		t.Fatalf("the signed-out access token still works: %d", w.Code)
	}
	// And the refresh token is dead in GoTrue.
	if w := do(f.edge, "GET", "/account", nil, withCookies("", rt), asXHR); w.Code != http.StatusUnauthorized {
		t.Fatalf("the signed-out refresh token still works: %d", w.Code)
	}

	// Everywhere: two sessions, one signs out globally, both die.
	a1, r1 := login()
	a2, r2 := login()
	if w := do(f.edge, "POST", "/logout", url.Values{"scope": {"global"}}, withCookies(a1, r1)); w.Code != http.StatusSeeOther {
		t.Fatalf("global logout: %d", w.Code)
	}
	if w := do(f.edge, "GET", "/account", nil, withCookies(a2, r2), asXHR); w.Code != http.StatusUnauthorized {
		t.Fatalf("the OTHER session survived sign-out-everywhere: %d", w.Code)
	}
	// Positive control: signing in again afterwards works.
	a3, r3 := login()
	f.fake.Advance(2 * time.Second)
	if w := do(f.edge, "GET", "/account", nil, withCookies(a3, r3)); w.Code != http.StatusOK {
		t.Fatalf("a new session after sign-out-everywhere: %d", w.Code)
	}
}

// TestGoTrueSessionsAreRefusedOffTheBrowserListener: on the machine-token
// listener no browser session is served, minted or claimed — and machine
// tokens still are.
func TestGoTrueSessionsAreRefusedOffTheBrowserListener(t *testing.T) {
	f := newGoTrueFixture(t)
	sub := f.member("m@example.com", 7, authz.RoleMember)
	at, rt, _ := f.fake.Mint(sub)

	if w := do(f.machine, "GET", "/account", nil, withCookies(at, rt), asXHR); w.Code != http.StatusForbidden ||
		!strings.Contains(w.Body.String(), OffBrowserListenerRefusal) {
		t.Fatalf("XHR with a valid session on the LAN listener: %d %s", w.Code, w.Body)
	}
	if w := do(f.machine, "GET", "/account", nil, withCookies(at, rt), asNavigation); w.Code != http.StatusForbidden ||
		!strings.Contains(w.Body.String(), "https://muster.example.com") {
		t.Fatalf("navigation on the LAN listener: %d %s", w.Code, w.Body)
	}
	if w := do(f.machine, "GET", "/login", nil); w.Code != http.StatusForbidden {
		t.Fatalf("GET /login on the LAN listener: %d", w.Code)
	}
	before := len(f.fake.Calls())
	if w := do(f.machine, "POST", "/login", url.Values{"email": {"m@example.com"}, "password": {"correct horse battery"}}); w.Code != http.StatusForbidden || len(setCookies(w)) != 0 {
		t.Fatalf("POST /login on the LAN listener: %d cookies %v", w.Code, setCookies(w))
	}
	if w := do(f.machine, "POST", ClaimPath, url.Values{"token": {"sometoken"}}); w.Code != http.StatusForbidden {
		t.Fatalf("claim on the LAN listener: %d", w.Code)
	}
	if n := len(f.fake.Calls()) - before; n != 0 {
		t.Fatalf("the LAN listener reached GoTrue %d time(s)", n)
	}
	// Machine tokens still work on the LAN listener.
	n := len(f.principals())
	r := httptest.NewRequest("GET", "/api/tags", nil)
	r.Header.Set("X-Muster-Token", gtHook)
	func() {
		defer func() { _ = recover() }() // the handler has no stores; admission is what is measured
		f.machine.ServeHTTP(httptest.NewRecorder(), r)
	}()
	ps := f.principals()
	if len(ps) != n+1 || ps[n].Via != authz.ViaHook {
		t.Fatalf("the hook token was not admitted on the LAN listener: %+v", ps[n:])
	}
	// Positive control: the same cookies on the browser listener are admitted.
	if w := do(f.edge, "GET", "/account", nil, withCookies(at, rt)); w.Code != http.StatusOK {
		t.Fatalf("positive control: browser listener refused the session: %d", w.Code)
	}
}

func TestClientIPIsTrustedFromHeadersOnlyOnTheBrowserListener(t *testing.T) {
	mk := func(edge bool, hdr map[string]string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = "192.0.2.10:5555"
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		if edge {
			r = r.WithContext(context.WithValue(r.Context(), browserListenerKey{}, true))
		}
		return r
	}
	for _, tc := range []struct {
		edge bool
		hdr  map[string]string
		want string
	}{
		{false, map[string]string{"CF-Connecting-IP": "198.51.100.1", "X-Forwarded-For": "198.51.100.2"}, "192.0.2.10"},
		{true, map[string]string{"CF-Connecting-IP": "198.51.100.1", "X-Forwarded-For": "198.51.100.2"}, "198.51.100.1"},
		{true, map[string]string{"X-Forwarded-For": "198.51.100.2, 203.0.113.9"}, "198.51.100.2"},
		{true, map[string]string{"CF-Connecting-IP": "not-an-ip", "X-Forwarded-For": "also bad"}, "192.0.2.10"},
		{true, nil, "192.0.2.10"},
	} {
		if got := clientIP(mk(tc.edge, tc.hdr)); got != tc.want {
			t.Errorf("edge=%v %v: clientIP = %s, want %s", tc.edge, tc.hdr, got, tc.want)
		}
	}
}

func TestSignInRefusesAGoTrueUserWithNoMusterAccount(t *testing.T) {
	f := newGoTrueFixture(t)
	f.fake.AddUser("stranger@example.com", "correct horse battery")
	w := do(f.edge, "POST", "/login", url.Values{"email": {"stranger@example.com"}, "password": {"correct horse battery"}})
	if w.Code != http.StatusForbidden || len(setCookies(w)) != 0 {
		t.Fatalf("a GoTrue user with no users row: %d cookies %v", w.Code, setCookies(w))
	}
}

func TestPasswordModeIgnoresGoTrueCookies(t *testing.T) {
	fake := gotruetest.New(t, gtSecret)
	store := newMemUsers()
	sub := fake.AddUser("m@example.com", "correct horse battery")
	store.add(users.User{ID: 7, GotrueSub: sub})
	s := New(nil, AuthConfig{HookToken: gtHook, UIPassword: strings.Repeat("p", 20)}, log.New(io.Discard, "", 0))
	s.UseGoTrue(GoTrueAuth{Client: fake.Client(), Verifier: fake.Verifier(), Users: store})
	h := BrowserListener(s.Handler())
	at, rt, _ := fake.Mint(sub)
	if w := do(h, "GET", "/account", nil, withCookies(at, rt), asXHR); w.Code != http.StatusUnauthorized {
		t.Fatalf("password mode admitted a GoTrue cookie: %d", w.Code)
	}
	// And its own login is the password form, as before.
	if w := do(h, "GET", "/login", nil); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Operator password") {
		t.Fatalf("password-mode login page changed: %d", w.Code)
	}
}

// TestGoTrueModeClosesAnUnarmedHookTier: the enforce-when-set opening admits
// anyone as user 1; in gotrue mode it answers 503 instead (the boot refusal in
// cmd/muster-server is the first wall, this the second).
func TestGoTrueModeClosesAnUnarmedHookTier(t *testing.T) {
	probe := func(cfg AuthConfig) (int, bool) {
		s := New(nil, cfg, log.New(io.Discard, "", 0))
		admitted := false
		s.principalProbe = func(*http.Request) { admitted = true; panic(errProbeAbort) }
		w := httptest.NewRecorder()
		func() {
			defer func() { _ = recover() }()
			s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/api/tags", nil))
		}()
		return w.Code, admitted
	}
	if code, admitted := probe(AuthConfig{Mode: AuthModeGoTrue}); admitted || code != http.StatusServiceUnavailable {
		t.Fatalf("gotrue mode, no hook token: admitted=%v code=%d, want refused 503", admitted, code)
	}
	// Control: password mode keeps the documented back-compat opening.
	if _, admitted := probe(AuthConfig{UIPassword: strings.Repeat("p", 20)}); !admitted {
		t.Fatal("control: password mode with no hook token no longer admits (back-compat changed)")
	}
}

func TestDisableUserReportsAGoTrueFailureAfterTheRowChanged(t *testing.T) {
	f := newGoTrueFixture(t)
	f.store.add(users.User{ID: 9, GotrueSub: "00000000-0000-4000-a000-000000000009"})
	err := f.s.DisableUser(context.Background(), 9, true)
	if err == nil || !strings.Contains(err.Error(), "disabled_at is updated") {
		t.Fatalf("a ban against a missing GoTrue user must say the row changed anyway: %v", err)
	}
	if u, _ := f.store.ByID(context.Background(), 9); !u.Disabled() {
		t.Fatal("the row was not disabled")
	}
	if !errors.Is(f.s.DisableUser(context.Background(), 404, true), users.ErrNotFound) {
		t.Fatal("disabling a missing user must report ErrNotFound")
	}
}

// TestTheGoTrueFormsRefuseACrossSitePost: sign-in, claim and sign-out each
// refuse a POST whose Origin is another site (login CSRF would sign the
// victim's browser into the attacker's account), before reaching GoTrue.
func TestTheGoTrueFormsRefuseACrossSitePost(t *testing.T) {
	f := newGoTrueFixture(t)
	f.member("m@example.com", 7, authz.RoleMember)
	tok := mintClaim(t, f.store)
	evil := func(r *http.Request) { r.Header.Set("Origin", "https://evil.example.net") }
	for _, target := range []string{"/login", ClaimPath, "/logout"} {
		w := do(f.edge, "POST", target, url.Values{"token": {tok}, "email": {"m@example.com"}, "password": {"correct horse battery"}}, evil)
		if w.Code != http.StatusForbidden {
			t.Errorf("cross-site POST %s: %d, want 403", target, w.Code)
		}
	}
	if n := len(f.fake.Calls()); n != 0 {
		t.Fatalf("cross-site posts reached GoTrue %d time(s)", n)
	}
	// Positive control: the same sign-in, same-origin, succeeds.
	if w := do(f.edge, "POST", "/login", url.Values{"email": {"m@example.com"}, "password": {"correct horse battery"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("positive control: %d", w.Code)
	}
}

// TestTheAuthPagesLetTheirOwnFormsCarryAnOrigin: securityHeaders sets
// no-referrer everywhere, under which a browser sends `Origin: null` on a form
// POST — and the gotrue forms refuse that. The auth documents must override it
// to same-origin, through the REAL middleware chain.
func TestTheAuthPagesLetTheirOwnFormsCarryAnOrigin(t *testing.T) {
	f := newGoTrueFixture(t)
	for _, target := range []string{"/login", ClaimPath} {
		w := do(f.edge, "GET", target, nil)
		if got := w.Header().Get("Referrer-Policy"); got != "same-origin" {
			t.Errorf("GET %s: Referrer-Policy %q, want same-origin", target, got)
		}
	}
	// Control: an ordinary response keeps the global no-referrer.
	if got := do(f.edge, "GET", "/health", nil).Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("control: /health Referrer-Policy %q, want no-referrer", got)
	}
	// And a POST whose Origin is the literal "null" is refused (what a form
	// under no-referrer would have sent).
	null := func(r *http.Request) { r.Header.Set("Origin", "null") }
	if w := do(f.edge, "POST", "/login", url.Values{"email": {"a@example.com"}, "password": {"x"}}, null); w.Code != http.StatusForbidden {
		t.Fatalf("Origin: null POST /login: %d, want 403", w.Code)
	}
}
