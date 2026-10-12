package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/apitokens"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/users"
)

// fakeAPITokens is an in-memory personal-token store that COUNTS lookups, so a
// test can tell "refused without a lookup" from "looked up and refused".
type fakeAPITokens struct {
	apitokens.Store
	mu       sync.Mutex
	calls    int
	byTok    map[string]apitokens.Token
	disabled map[int64]bool
	err      error
}

func newFakeAPITokens() *fakeAPITokens {
	return &fakeAPITokens{byTok: map[string]apitokens.Token{}, disabled: map[int64]bool{}}
}

func (f *fakeAPITokens) add(t *testing.T, uid int64) string {
	t.Helper()
	tok, err := apitokens.New()
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byTok[tok] = apitokens.Token{ID: int64(len(f.byTok) + 1), UserID: uid, Name: "t"}
	return tok
}

func (f *fakeAPITokens) Authenticate(ctx context.Context, tok string) (apitokens.Token, error) {
	if p, ok := authz.From(ctx); !ok || !p.All() {
		return apitokens.Token{}, errors.New("fake: Authenticate must run under authz.System")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return apitokens.Token{}, f.err
	}
	t, ok := f.byTok[tok]
	if !ok {
		return apitokens.Token{}, apitokens.ErrBadToken
	}
	if f.disabled[t.UserID] {
		return apitokens.Token{}, apitokens.ErrUserDisabled
	}
	return t, nil
}

func (f *fakeAPITokens) lookups() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

const tokTestHook = "api-token-test-hook-token-0123456789abc"

func tokenTestServer(t *testing.T, cfg AuthConfig, f *fakeAPITokens) http.Handler {
	t.Helper()
	s := New(nil, cfg, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{APITokens: f, Users: stubUsers{}})
	mux := http.NewServeMux()
	s.registerAll(mux)
	return mux
}

func whoami(t *testing.T, h http.Handler, bearer, remote string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/whoami", nil)
	if remote != "" {
		r.RemoteAddr = remote
	}
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

// TestAPersonalTokenActsAsItsOwnerNotTheMachineUser: the same route, two
// credentials, two identities. A token is its owner (user 7, a member — the
// role is READ, via roleOf); the hook secret is the machine user, user 1.
func TestAPersonalTokenActsAsItsOwnerNotTheMachineUser(t *testing.T) {
	f := newFakeAPITokens()
	tok := f.add(t, 7)
	h := tokenTestServer(t, AuthConfig{HookToken: tokTestHook}, f)
	code, body := whoami(t, h, tok, "")
	if code != 200 || body["userId"] != float64(7) || body["role"] != "member" || body["via"] != "api-token" {
		t.Fatalf("whoami with user 7's token = %d %v", code, body)
	}
	if tk, _ := body["token"].(map[string]any); tk == nil || tk["id"] == nil {
		t.Fatalf("whoami does not name the token it was admitted with: %v", body)
	}
	if code, body := whoami(t, h, tokTestHook, ""); code != 200 || body["userId"] != float64(1) || body["via"] != "hook" {
		t.Fatalf("whoami with the hook token = %d %v", code, body)
	}
}

// TestAMalformedPersonalTokenNeverReachesTheStore — and the control: a
// well-shaped unknown one does.
func TestAMalformedPersonalTokenNeverReachesTheStore(t *testing.T) {
	f := newFakeAPITokens()
	h := tokenTestServer(t, AuthConfig{HookToken: tokTestHook}, f)
	for _, bad := range []string{"mst_", "mst_short", "mst_" + strings.Repeat("!", 43), "mst_" + strings.Repeat("A", 44)} {
		if code, _ := whoami(t, h, bad, ""); code != http.StatusUnauthorized {
			t.Errorf("%q: %d; want 401", bad, code)
		}
	}
	if n := f.lookups(); n != 0 {
		t.Fatalf("malformed tokens cost %d store lookup(s); want 0", n)
	}
	unknown, _ := apitokens.New()
	if code, _ := whoami(t, h, unknown, ""); code != http.StatusUnauthorized || f.lookups() != 1 {
		t.Fatalf("CONTROL: a well-formed unknown token: %d after %d lookups; want 401 after 1", code, f.lookups())
	}
}

// TestADisabledOwnersTokenIsRefusedAndAnOutageIsNotRemembered: a disabled
// owner's token is refused; a lookup FAILURE is a 503 that the next request
// does not inherit. (Re-enabling does NOT bring a token back — the disable
// revoked it; that is TestDisablingAUserRevokesItsTokensForGood, over the
// real stores. This fake only models the disabled flag.)
func TestADisabledOwnersTokenIsRefusedAndAnOutageIsNotRemembered(t *testing.T) {
	f := newFakeAPITokens()
	tok := f.add(t, 7)
	h := tokenTestServer(t, AuthConfig{HookToken: tokTestHook}, f)
	f.disabled[7] = true
	if code, _ := whoami(t, h, tok, ""); code != http.StatusUnauthorized {
		t.Fatalf("disabled owner: %d; want 401", code)
	}
	f.disabled[7] = false
	f.err = errors.New("db down")
	if code, _ := whoami(t, h, tok, ""); code != http.StatusServiceUnavailable {
		t.Fatalf("lookup failure: %d; want 503", code)
	}
	f.err = nil
	if code, _ := whoami(t, h, tok, ""); code != 200 {
		t.Fatalf("after the outage: %d; want 200", code)
	}
}

// TestEveryHookTierWrapperTakesATokenEvenUnarmed: a personal token needs no
// MUSTER_HOOK_TOKEN. On an UNARMED password-mode server a bad mst_ value is a
// 401, not the open tier's anonymous admission as user 1; in gotrue mode,
// where the unarmed hook tier is 503, a valid token is still admitted.
func TestEveryHookTierWrapperTakesATokenEvenUnarmed(t *testing.T) {
	f := newFakeAPITokens()
	tok := f.add(t, 7)
	bad, _ := apitokens.New()
	open := tokenTestServer(t, AuthConfig{UIPassword: "a-long-operator-password"}, f)
	if code, body := whoami(t, open, "", ""); code != 200 || body["userId"] != float64(1) {
		t.Fatalf("CONTROL: the unarmed tier with no credential: %d %v; want the open admission as user 1", code, body)
	}
	if code, body := whoami(t, open, bad, ""); code != http.StatusUnauthorized {
		t.Fatalf("an unknown personal token on the open tier: %d %v; want 401", code, body)
	}
	if code, body := whoami(t, open, tok, ""); code != 200 || body["userId"] != float64(7) {
		t.Fatalf("a valid token on the open tier: %d %v", code, body)
	}
	gt := tokenTestServer(t, AuthConfig{Mode: AuthModeGoTrue}, f)
	if code, _ := whoami(t, gt, "", ""); code != http.StatusServiceUnavailable {
		t.Fatalf("CONTROL: gotrue mode, unarmed, no credential: %d; want 503", code)
	}
	if code, body := whoami(t, gt, tok, ""); code != 200 || body["userId"] != float64(7) {
		t.Fatalf("gotrue mode, unarmed: a valid personal token: %d %v", code, body)
	}
}

// mintCountingTokens is a fake whose Create COUNTS, so a test can see that no
// route minted anything.
type mintCountingTokens struct {
	*fakeAPITokens
	mints int
}

func (m *mintCountingTokens) Create(context.Context, string, int64) (string, apitokens.Token, error) {
	m.mints++
	tok, _ := apitokens.New()
	return tok, apitokens.Token{ID: 99, UserID: 7}, nil
}

// TestATokenCannotMintAToken (review round 1): a personal token must not be
// able to mint another. A leaked token that could would mint a non-expiring
// child that survives revoking the parent. Minting is browser-session only
// (/account/tokens); POST /api/tokens does not exist, and nothing is created.
func TestATokenCannotMintAToken(t *testing.T) {
	f := &mintCountingTokens{fakeAPITokens: newFakeAPITokens()}
	tok := f.add(t, 7)
	s := New(nil, AuthConfig{HookToken: tokTestHook}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{APITokens: f, Users: stubUsers{}})
	mux := http.NewServeMux()
	s.registerAll(mux)
	r := httptest.NewRequest(http.MethodPost, "/api/tokens", strings.NewReader(`{"name":"child"}`))
	r.Header.Set("Authorization", "Bearer "+tok)
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code >= 200 && rec.Code < 300 || f.mints != 0 {
		t.Fatalf("POST /api/tokens with a personal token: %d %s, %d mint(s); want no route and nothing minted",
			rec.Code, truncate(rec.Body.String(), 200), f.mints)
	}
	// Control: the same token IS admitted on the token API (GET lists), so the
	// refusal above is about minting, not about the credential.
	if code, body := whoami(t, mux, tok, ""); code != 200 || body["userId"] != float64(7) {
		t.Fatalf("CONTROL: the token is not admitted at all: %d %v", code, body)
	}
}

// TestTheTokenAPITakesOnlyAPersonalToken: the hook secret manages no tokens.
func TestTheTokenAPITakesOnlyAPersonalToken(t *testing.T) {
	f := newFakeAPITokens()
	h := tokenTestServer(t, AuthConfig{HookToken: tokTestHook}, f)
	for _, c := range []struct{ method, path string }{{"GET", "/api/tokens"}, {"DELETE", "/api/tokens/1"}} {
		r := httptest.NewRequest(c.method, c.path, strings.NewReader(`{}`))
		r.Header.Set("Authorization", "Bearer "+tokTestHook)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with the hook token: %d; want 401", c.method, c.path, rec.Code)
		}
	}
	if f.lookups() != 0 {
		t.Fatalf("the hook token was looked up as a personal token %d time(s)", f.lookups())
	}
}

// --- the whole loop, over the real stores ----------------------------------------

var mintedRE = regexp.MustCompile(`value="(mst_[A-Za-z0-9_-]{43})"`)

// TestPersonalTokenLifecycleOverHTTP drives the real server over real
// Postgres: mint on the page (shown once), use it as its owner on the machine
// tier, mint another over the API, disable/re-enable, revoke (immediate 401),
// and the audit trail.
func TestPersonalTokenLifecycleOverHTTP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dbtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	sys := authz.System(ctx)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	var a, b int64
	for _, id := range []*int64{&a, &b} {
		if err := pool.QueryRow(sys, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`, "tok"+suffix).Scan(id); err != nil {
			t.Fatal(err)
		}
	}
	ns := notes.NewPG(pool)
	asUser := func(uid int64) context.Context {
		return authz.WithPrincipal(ctx, authz.Principal{UserID: uid, Role: authz.RoleMember, Via: authz.ViaSession})
	}
	aTask, err := ns.Create(asUser(a), notes.Note{Title: "a-secret-" + suffix, Status: notes.StatusOpen})
	if err != nil {
		t.Fatal(err)
	}
	bTask, err := ns.Create(asUser(b), notes.Note{Title: "b-own-" + suffix, Status: notes.StatusOpen})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ids := []int64{a, b}
		for _, q := range []string{`DELETE FROM notes WHERE owner_id = ANY($1)`, `DELETE FROM api_tokens WHERE user_id = ANY($1)`,
			`DELETE FROM audit_log WHERE user_id = ANY($1)`, `DELETE FROM users WHERE id = ANY($1)`} {
			_, _ = pool.Exec(authz.System(context.Background()), q, ids)
		}
	})

	srv := New(nil, AuthConfig{HookToken: tokTestHook, UIPassword: "token-lifecycle-operator-pw"}, log.New(io.Discard, "", 0))
	srv.UseExtensions(Extensions{Notes: ns, Users: users.NewPG(pool), APITokens: apitokens.NewPG(pool)})
	const userHeader = "X-Tok-Test-User"
	srv.sessionUserOverride = func(r *http.Request) int64 {
		v, _ := strconv.ParseInt(r.Header.Get(userHeader), 10, 64)
		return v
	}
	h := srv.Handler()
	do := func(method, path, body, bearer string, session int64) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
		r.Host = "muster.test"
		if session > 0 {
			r.Header.Set("Cookie", sessionCookieFor(srv))
			r.Header.Set("Origin", "http://muster.test")
			r.Header.Set(userHeader, strconv.FormatInt(session, 10))
			if body != "" {
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			}
		}
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
			if body != "" {
				r.Header.Set("Content-Type", "application/json")
			}
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}

	// 1. Mint on the page: shown in THIS response, never again.
	rec := do("POST", accountTokensPath, url.Values{"name": {"page-" + suffix}}.Encode(), "", a)
	m := mintedRE.FindStringSubmatch(rec.Body.String())
	if rec.Code != 200 || m == nil {
		t.Fatalf("minting on the page: %d, no token in the body: %s", rec.Code, truncate(rec.Body.String(), 400))
	}
	pageTok := m[1]
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("the page that shows a token is cacheable: %q", rec.Header().Get("Cache-Control"))
	}
	if page := do("GET", accountTokensPath, "", "", a).Body.String(); strings.Contains(page, pageTok) || !strings.Contains(page, "page-"+suffix) {
		t.Fatalf("the token list re-shows the token, or does not list it by name: %s", truncate(page, 400))
	}
	if page := do("GET", accountTokensPath, "", "", b).Body.String(); strings.Contains(page, "page-"+suffix) {
		t.Fatalf("B's token page lists A's token")
	}

	// 2. It acts as A on the machine tier — A's task, not B's — and B's own
	// token acts as B.
	if rec := do("GET", "/api/whoami", "", pageTok, 0); !strings.Contains(rec.Body.String(), `"userId":`+strconv.FormatInt(a, 10)) {
		t.Fatalf("whoami with A's token: %d %s", rec.Code, rec.Body)
	}
	list := do("GET", "/api/tasks", "", pageTok, 0).Body.String()
	if !strings.Contains(list, aTask.Title) || strings.Contains(list, bTask.Title) {
		t.Fatalf("A's token's task list: %s", truncate(list, 400))
	}
	bTok, _, err := apitokens.NewPG(pool).Create(asUser(b), "b", 0)
	if err != nil {
		t.Fatal(err)
	}
	list = do("GET", "/api/tasks", "", bTok, 0).Body.String()
	if !strings.Contains(list, bTask.Title) || strings.Contains(list, aTask.Title) {
		t.Fatalf("B's token's task list: %s", truncate(list, 400))
	}
	if rec := do("GET", "/api/tasks/"+strconv.FormatInt(aTask.ID, 10), "", bTok, 0); rec.Code != http.StatusNotFound {
		t.Fatalf("B's token reading A's task: %d %s", rec.Code, truncate(rec.Body.String(), 200))
	}

	// mint mints a token for user uid on the PAGE — the only place a token is
	// minted — and returns it.
	mint := func(uid int64, name string) string {
		t.Helper()
		rec := do("POST", accountTokensPath, url.Values{"name": {name}}.Encode(), "", uid)
		m := mintedRE.FindStringSubmatch(rec.Body.String())
		if rec.Code != 200 || m == nil {
			t.Fatalf("minting %q on the page: %d", name, rec.Code)
		}
		return m[1]
	}
	countTokens := func(uid int64) int {
		var n int
		if err := pool.QueryRow(sys, `SELECT count(*) FROM api_tokens WHERE user_id = $1`, uid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// 3. A token cannot mint a token (review round 1): no create route on the
	// token API, and nothing is written. The list never carries a secret.
	before := countTokens(a)
	if rec := do("POST", apiTokensPath, `{"name":"child"}`, pageTok, 0); rec.Code >= 200 && rec.Code < 300 {
		t.Fatalf("POST /api/tokens with a token: %d %s; minting must be browser-session only", rec.Code, truncate(rec.Body.String(), 200))
	}
	if after := countTokens(a); after != before {
		t.Fatalf("A's token count moved %d -> %d through the API", before, after)
	}
	second := mint(a, "second-"+suffix)
	listed := do("GET", apiTokensPath, "", pageTok, 0).Body.String()
	if strings.Contains(listed, second) || strings.Contains(listed, pageTok) || !strings.Contains(listed, "second-"+suffix) {
		t.Fatalf("GET /api/tokens: %s", truncate(listed, 400))
	}
	var secondID, pageID int64
	for tok, id := range map[string]*int64{second: &secondID, pageTok: &pageID} {
		if err := pool.QueryRow(sys, `SELECT id FROM api_tokens WHERE token_hash = $1`, apitokens.Hash(tok)).Scan(id); err != nil {
			t.Fatal(err)
		}
	}

	// 4. Revoke over the API (a token revoking ITSELF): the next request is 401.
	if rec := do("DELETE", apiTokensPath+"/"+strconv.FormatInt(secondID, 10), "", second, 0); rec.Code != 200 {
		t.Fatalf("self-revoke: %d %s", rec.Code, rec.Body)
	}
	if rec := do("GET", "/api/tasks", "", second, 0); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a revoked token, the very next request: %d", rec.Code)
	}
	// B cannot revoke A's page token (404, like a ghost), which keeps working.
	if rec := do("DELETE", apiTokensPath+"/"+strconv.FormatInt(pageID, 10), "", bTok, 0); rec.Code != http.StatusNotFound {
		t.Fatalf("B revoking A's token: %d", rec.Code)
	}
	if rec := do("POST", accountTokensPath+"/"+strconv.FormatInt(pageID, 10)+"/revoke", "x=1", "", b); rec.Code != http.StatusNotFound {
		t.Fatalf("B's session revoking A's token: %d", rec.Code)
	}
	if rec := do("GET", "/api/whoami", "", pageTok, 0); rec.Code != 200 {
		t.Fatalf("A's token after B's attempts: %d", rec.Code)
	}
	// Revoke on the page: 303 back, and the token is dead.
	if rec := do("POST", accountTokensPath+"/"+strconv.FormatInt(pageID, 10)+"/revoke", "x=1", "", a); rec.Code != http.StatusSeeOther {
		t.Fatalf("revoking on the page: %d %s", rec.Code, truncate(rec.Body.String(), 200))
	}
	if rec := do("GET", "/api/whoami", "", pageTok, 0); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the page-revoked token: %d", rec.Code)
	}

	// 5. Disable A through the REAL disable writer: refused at once — and
	// re-enabling does NOT bring the token back (review round 1: the disable
	// revoked it). A token minted after the re-enable works.
	third := mint(a, "third-"+suffix)
	if rec := do("GET", "/api/tasks", "", third, 0); rec.Code != 200 {
		t.Fatalf("CONTROL: a fresh token before the disable: %d", rec.Code)
	}
	us := users.NewPG(pool)
	if _, err := us.SetDisabled(ctx, a, true); err != nil {
		t.Fatal(err)
	}
	if rec := do("GET", "/api/tasks", "", third, 0); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a disabled user's token: %d", rec.Code)
	}
	if _, err := us.SetDisabled(ctx, a, false); err != nil {
		t.Fatal(err)
	}
	if rec := do("GET", "/api/tasks", "", third, 0); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a token of a disabled-then-RE-ENABLED user: %d; the disable must have revoked it for good", rec.Code)
	}
	fourth := mint(a, "fourth-"+suffix)
	if rec := do("GET", "/api/tasks", "", fourth, 0); rec.Code != 200 {
		t.Fatalf("a token minted after the re-enable: %d", rec.Code)
	}

	// 6. The audit trail: A's mints and its two explicit revokes, with the door
	// each came through, and no secret anywhere in them.
	var trail string
	if err := pool.QueryRow(sys, `SELECT string_agg(action || '/' || via, ',' ORDER BY id) FROM audit_log WHERE user_id = $1`, a).Scan(&trail); err != nil {
		t.Fatal(err)
	}
	if want := "api_token.create/session,api_token.create/session,api_token.revoke/api-token,api_token.revoke/session," +
		"api_token.create/session,api_token.create/session"; trail != want {
		t.Fatalf("A's audit trail = %q; want %q", trail, want)
	}
	var all string
	_ = pool.QueryRow(sys, `SELECT coalesce(string_agg(a::text, ' '), '') FROM audit_log a WHERE user_id = ANY($1)`, []int64{a, b}).Scan(&all)
	for _, secret := range []string{pageTok, second, third, fourth, bTok} {
		if strings.Contains(all, strings.TrimPrefix(secret, apitokens.Prefix)) {
			t.Fatal("an audit row carries a token")
		}
	}
}

// TestAPasswordResetRevokesTheUsersAPITokens (review round 3): a password
// reset is what someone does after a credential leaked, and a personal token
// is a credential — so the reset revokes all of the user's tokens, and the
// page the reset lands on says so. Literal strings, not the constants, so the
// claim on the page is pinned as the user reads it.
func TestAPasswordResetRevokesTheUsersAPITokens(t *testing.T) {
	w := newInviteWorld(t)
	tok := w.createInvite("")
	do(w.edge, "POST", InvitePath, redeemForm(tok, "tokreset@invite.test", "", goodPW, goodPW))
	sys := authz.System(context.Background())
	var uid int64
	if err := w.pool.QueryRow(sys, `SELECT id FROM users WHERE email = 'tokreset@invite.test'`).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = w.pool.Exec(authz.System(context.Background()), `DELETE FROM api_tokens WHERE user_id = $1`, uid)
	})
	ts := apitokens.NewPG(w.pool)
	asUser := authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: authz.RoleMember, Via: authz.ViaSession})
	secret, _, err := ts.Create(asUser, "before-reset", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Authenticate(sys, secret); err != nil {
		t.Fatalf("CONTROL: the token before the reset: %v", err)
	}
	rec := do(w.edge, "POST", "/admin/users/"+strconv.FormatInt(uid, 10)+"/reset-link", url.Values{}, w.asAdmin())
	m := regexp.MustCompile(regexp.QuoteMeta(invitePublic+ResetPath+"#") + `([A-Za-z0-9_-]+)`).FindStringSubmatch(rec.Body.String())
	if rec.Code != 200 || m == nil {
		t.Fatalf("reset link: %d", rec.Code)
	}
	const newPW = "a-brand-new-password-for-tokens"
	r := do(w.edge, "POST", ResetPath, url.Values{"token": {m[1]}, "password": {newPW}, "password2": {newPW}})
	if r.Code != http.StatusSeeOther {
		t.Fatalf("reset: %d %s", r.Code, truncate(r.Body.String(), 300))
	}
	if _, err := ts.Authenticate(sys, secret); !errors.Is(err, apitokens.ErrBadToken) {
		t.Fatalf("the user's API token after a password reset: %v; want it revoked", err)
	}
	if loc := r.Header().Get("Location"); loc != "/account?reset=1" {
		t.Fatalf("a successful reset lands on %q; want the account page that says the tokens were revoked", loc)
	}
	ck := setCookies(r)
	page := do(w.edge, "GET", "/account?reset=1", nil, withCookies(ck[accessCookieName].Value, ck[refreshCookieName].Value))
	if page.Code != 200 || !strings.Contains(page.Body.String(), "Your API tokens were revoked") {
		t.Fatalf("the reset's landing page: %d, does not say the API tokens were revoked\n%s", page.Code, truncate(page.Body.String(), 600))
	}
}

// TestSignOutEverywhereSaysItKeepsAPITokens (review round 3): "Sign out
// everywhere" ends GoTrue sessions and NOT personal tokens, and the page says
// so beside the button — and, as the control, a token survives that sign-out.
func TestSignOutEverywhereSaysItKeepsAPITokens(t *testing.T) {
	w := newInviteWorld(t)
	page := do(w.edge, "GET", "/account", nil, w.asAdmin())
	body := page.Body.String()
	i, j := strings.Index(body, "Sign out everywhere"), strings.Index(body, "API tokens are not affected")
	if page.Code != 200 || i < 0 || j < 0 || j < i {
		t.Fatalf("/account: %d; want the note \"API tokens are not affected\" after the Sign out everywhere button\n%s",
			page.Code, truncate(body, 800))
	}
	t.Cleanup(func() {
		_, _ = w.pool.Exec(authz.System(context.Background()), `DELETE FROM api_tokens WHERE user_id = 1`)
	})
	ts := apitokens.NewPG(w.pool)
	secret, _, err := ts.Create(authz.Operator(context.Background()), "kept", 0)
	if err != nil {
		t.Fatal(err)
	}
	r := do(w.edge, "POST", "/logout", url.Values{"scope": {"global"}}, w.asAdmin())
	if r.Code != http.StatusSeeOther {
		t.Fatalf("sign out everywhere: %d", r.Code)
	}
	if _, err := ts.Authenticate(authz.System(context.Background()), secret); err != nil {
		t.Fatalf("a token after Sign out everywhere: %v; the page says tokens are not affected", err)
	}
}

// refusingTokens is a store whose Create returns a preset error (and whose
// List is empty), for the mint page's refusal texts.
type refusingTokens struct {
	apitokens.Store
	err error
}

func (r refusingTokens) Create(context.Context, string, int64) (string, apitokens.Token, error) {
	return "", apitokens.Token{}, r.err
}
func (refusingTokens) List(context.Context) ([]apitokens.Token, error) { return nil, nil }

// TestTheMintPageSaysWhyItRefused (review round 4): the two refusals the store
// can give a mint each reach the person as a status and a sentence — a
// disabled account (403) and a session a reset or sign-out ended (401) — and
// neither renders a token.
func TestTheMintPageSaysWhyItRefused(t *testing.T) {
	for _, c := range []struct {
		err  error
		code int
		want string
	}{
		{apitokens.ErrUserDisabled, http.StatusForbidden, "This account is disabled"},
		{apitokens.ErrSessionEnded, http.StatusUnauthorized, "Sign in again to create API tokens"},
	} {
		s := New(nil, AuthConfig{HookToken: tokTestHook, UIPassword: "mint-refusal-operator-pw"}, log.New(io.Discard, "", 0))
		s.UseExtensions(Extensions{APITokens: refusingTokens{err: c.err}, Users: stubUsers{}})
		mux := http.NewServeMux()
		s.registerAll(mux)
		r := httptest.NewRequest(http.MethodPost, accountTokensPath, strings.NewReader("name=x"))
		r.Host = "muster.test"
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Cookie", sessionCookieFor(s))
		r.Header.Set("Origin", "http://muster.test")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, r)
		if rec.Code != c.code || !strings.Contains(rec.Body.String(), c.want) || strings.Contains(rec.Body.String(), "minted-token") {
			t.Errorf("mint refused with %v: %d; want %d and %q, and no token\n%s", c.err, rec.Code, c.code, c.want, truncate(rec.Body.String(), 400))
		}
	}
}

// TestAResetWhoseSignInFailsStillSaysTheTokensWereRevoked (review round 4):
// the password is set and the tokens are revoked BEFORE the sign-in, so when
// the sign-in then fails (GoTrue down) the notice must still say the tokens
// are gone — the person would otherwise find their scripts 401 with no word.
func TestAResetWhoseSignInFailsStillSaysTheTokensWereRevoked(t *testing.T) {
	w := newInviteWorld(t)
	uid, sub, _, _ := w.joinMember("tokfail@invite.test")
	t.Cleanup(func() {
		_, _ = w.pool.Exec(authz.System(context.Background()), `DELETE FROM api_tokens WHERE user_id = $1`, uid)
	})
	ts := apitokens.NewPG(w.pool)
	secret, _, err := ts.Create(authz.WithPrincipal(context.Background(),
		authz.Principal{UserID: uid, Role: authz.RoleMember, Via: authz.ViaSession}), "before", 0)
	if err != nil {
		t.Fatal(err)
	}
	w.fake.FailToken(http.StatusInternalServerError, 10)
	r := do(w.edge, "POST", ResetPath, url.Values{"token": {w.fake.RecoveryHash(sub)}, "password": {"a-new-long-password"}, "password2": {"a-new-long-password"}})
	body := r.Body.String()
	if r.Code != http.StatusBadGateway || !strings.Contains(body, "Your password is set, but sign-in failed") ||
		!strings.Contains(body, "Your API tokens were revoked") {
		t.Fatalf("a reset whose sign-in fails: %d; want 502 saying the password is set AND the API tokens were revoked\n%s", r.Code, truncate(body, 500))
	}
	if _, err := ts.Authenticate(authz.System(context.Background()), secret); !errors.Is(err, apitokens.ErrBadToken) {
		t.Fatalf("CONTROL: the token after that reset: %v; the notice says it was revoked", err)
	}
}

// iatCapturingTokens records the session issued-at the mint handler passes.
type iatCapturingTokens struct {
	apitokens.Store
	got *[]int64
}

func (c iatCapturingTokens) Create(_ context.Context, _ string, iat int64) (string, apitokens.Token, error) {
	*c.got = append(*c.got, iat)
	tok, _ := apitokens.New()
	return tok, apitokens.Token{ID: 1}, nil
}
func (iatCapturingTokens) List(context.Context) ([]apitokens.Token, error) { return nil, nil }

// TestAGoTrueMintCarriesTheSessionsIssuedAt (review round 4): the store's
// cutoff check is only as good as the iat the handler hands it — zero would
// skip it — so a real gotrue session's mint must arrive with that session's
// own, non-zero issued-at.
func TestAGoTrueMintCarriesTheSessionsIssuedAt(t *testing.T) {
	w := newInviteWorld(t)
	var got []int64
	w.s.UseExtensions(Extensions{Users: w.store, APITokens: iatCapturingTokens{got: &got}})
	edge := BrowserListener(w.s.Handler())
	c, err := w.fake.Verifier().Verify(w.adminAT)
	if err != nil || c.IssuedAt <= 0 {
		t.Fatalf("CONTROL: the admin's access token: %+v %v", c, err)
	}
	rec := do(edge, "POST", accountTokensPath, url.Values{"name": {"x"}}, w.asAdmin())
	if rec.Code != 200 || len(got) != 1 || got[0] != c.IssuedAt {
		t.Fatalf("mint as a gotrue session: %d, store saw issued-at %v; want exactly the session's %d", rec.Code, got, c.IssuedAt)
	}
}
