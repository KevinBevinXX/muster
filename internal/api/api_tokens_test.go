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

// TestAPersonalTokenActsAsItsOwnerNotTheMachineUser: the same route, three
// credentials, three identities. A token is its owner (user 7, a member — the
// role is READ, via roleOf); the hook secret is the machine user, which
// MUSTER_MACHINE_USER_ID moves.
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
	moved := tokenTestServer(t, AuthConfig{HookToken: tokTestHook, MachineUserID: 5}, f)
	if code, body := whoami(t, moved, tokTestHook, ""); code != 200 || body["userId"] != float64(5) {
		t.Fatalf("MachineUserID 5: the hook token acts as %v (%d); want 5", body["userId"], code)
	}
	if code, body := whoami(t, moved, tok, ""); body["userId"] != float64(7) {
		t.Fatalf("MachineUserID must not move a PERSONAL token: %d %v", code, body)
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

// TestAKnownBadTokenIsAnsweredWithoutASecondLookup: unknown and revoked are
// permanent, so the second presentation is answered from the bad-hash table.
func TestAKnownBadTokenIsAnsweredWithoutASecondLookup(t *testing.T) {
	f := newFakeAPITokens()
	h := tokenTestServer(t, AuthConfig{HookToken: tokTestHook}, f)
	unknown, _ := apitokens.New()
	for i := 0; i < 3; i++ {
		if code, _ := whoami(t, h, unknown, ""); code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i, code)
		}
	}
	if n := f.lookups(); n != 1 {
		t.Fatalf("one unknown token presented 3 times cost %d lookups; want 1", n)
	}
}

// TestAValidTokenIsNeverRefusedByTheLimiter: one address floods well-formed
// junk until its failures are answered 429 — and its valid token is still
// admitted, then the junk is still 429 (the limiter really was engaged).
func TestAValidTokenIsNeverRefusedByTheLimiter(t *testing.T) {
	f := newFakeAPITokens()
	tok := f.add(t, 7)
	h := tokenTestServer(t, AuthConfig{HookToken: tokTestHook}, f)
	const addr = "192.0.2.9:4000"
	saw429 := 0
	for i := 0; i < apiTokenFailureBudget+10; i++ {
		junk, _ := apitokens.New()
		code, _ := whoami(t, h, junk, addr)
		switch {
		case i < apiTokenFailureBudget && code != http.StatusUnauthorized:
			t.Fatalf("failure %d (within budget): %d; want 401", i, code)
		case code == http.StatusTooManyRequests:
			saw429++
		}
	}
	if saw429 != 10 {
		t.Fatalf("CONTROL: %d of the 10 over-budget failures were 429; the limiter never engaged", saw429)
	}
	if code, body := whoami(t, h, tok, addr); code != 200 || body["userId"] != float64(7) {
		t.Fatalf("a VALID token from the flooded address: %d %v; a valid token must never be refused", code, body)
	}
	junk, _ := apitokens.New()
	if code, _ := whoami(t, h, junk, addr); code != http.StatusTooManyRequests {
		t.Fatalf("after the valid token, junk from the address: %d; want 429 still", code)
	}
	if code, _ := whoami(t, h, junk, "198.51.100.1:1"); code != http.StatusUnauthorized {
		t.Fatalf("another address: %d; want 401 (the budget is per address)", code)
	}
}

// TestADisabledOwnersTokenIsRefusedAndNothingIsRemembered: refused while
// disabled, admitted the moment it is re-enabled, and a lookup FAILURE is a
// 503 that is not remembered either.
func TestADisabledOwnersTokenIsRefusedAndNothingIsRemembered(t *testing.T) {
	f := newFakeAPITokens()
	tok := f.add(t, 7)
	h := tokenTestServer(t, AuthConfig{HookToken: tokTestHook}, f)
	f.disabled[7] = true
	if code, _ := whoami(t, h, tok, ""); code != http.StatusUnauthorized {
		t.Fatalf("disabled owner: %d; want 401", code)
	}
	f.disabled[7] = false
	if code, _ := whoami(t, h, tok, ""); code != 200 {
		t.Fatalf("re-enabled owner: %d; want 200 (a disabled refusal must not be cached)", code)
	}
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

// TestTheTokenAPITakesOnlyAPersonalToken: the hook secret mints nothing.
func TestTheTokenAPITakesOnlyAPersonalToken(t *testing.T) {
	f := newFakeAPITokens()
	h := tokenTestServer(t, AuthConfig{HookToken: tokTestHook}, f)
	for _, c := range []struct{ method, path string }{{"GET", "/api/tokens"}, {"POST", "/api/tokens"}, {"DELETE", "/api/tokens/1"}} {
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
	bTok, _, err := apitokens.NewPG(pool).Create(asUser(b), "b")
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

	// 3. Mint over the API with the token: 201, the secret once; the list never
	// carries one.
	rec = do("POST", apiTokensPath, `{"name":"cli-`+suffix+`"}`, pageTok, 0)
	var created struct {
		ID    int64  `json:"id"`
		Token string `json:"token"`
		Name  string `json:"name"`
	}
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &created) != nil || !apitokens.ShapeOK(created.Token) {
		t.Fatalf("POST /api/tokens: %d %s", rec.Code, rec.Body)
	}
	listed := do("GET", apiTokensPath, "", pageTok, 0).Body.String()
	if strings.Contains(listed, created.Token) || strings.Contains(listed, pageTok) || !strings.Contains(listed, "cli-"+suffix) {
		t.Fatalf("GET /api/tokens: %s", truncate(listed, 400))
	}

	// 4. Disable A: refused at once; re-enable: admitted at once.
	if _, err := pool.Exec(sys, `UPDATE users SET disabled_at = now() WHERE id = $1`, a); err != nil {
		t.Fatal(err)
	}
	if rec := do("GET", "/api/tasks", "", created.Token, 0); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a disabled user's token: %d", rec.Code)
	}
	if _, err := pool.Exec(sys, `UPDATE users SET disabled_at = NULL WHERE id = $1`, a); err != nil {
		t.Fatal(err)
	}
	if rec := do("GET", "/api/tasks", "", created.Token, 0); rec.Code != 200 {
		t.Fatalf("re-enabled: %d", rec.Code)
	}

	// 5. Revoke over the API (a token revoking ITSELF): the next request is 401.
	if rec := do("DELETE", apiTokensPath+"/"+strconv.FormatInt(created.ID, 10), "", created.Token, 0); rec.Code != 200 {
		t.Fatalf("self-revoke: %d %s", rec.Code, rec.Body)
	}
	if rec := do("GET", "/api/tasks", "", created.Token, 0); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a revoked token, the very next request: %d", rec.Code)
	}
	// B cannot revoke A's page token (404, like a ghost), which keeps working.
	var pageID int64
	if err := pool.QueryRow(sys, `SELECT id FROM api_tokens WHERE token_hash = $1`, apitokens.Hash(pageTok)).Scan(&pageID); err != nil {
		t.Fatal(err)
	}
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

	// 6. The audit trail: A's two creates and two revokes, with the door each
	// came through, and no secret anywhere in them.
	var trail string
	if err := pool.QueryRow(sys, `SELECT string_agg(action || '/' || via, ',' ORDER BY id) FROM audit_log WHERE user_id = $1`, a).Scan(&trail); err != nil {
		t.Fatal(err)
	}
	if want := "api_token.create/session,api_token.create/api-token,api_token.revoke/api-token,api_token.revoke/session"; trail != want {
		t.Fatalf("A's audit trail = %q; want %q", trail, want)
	}
	var all string
	_ = pool.QueryRow(sys, `SELECT coalesce(string_agg(a::text, ' '), '') FROM audit_log a WHERE user_id = ANY($1)`, []int64{a, b}).Scan(&all)
	for _, secret := range []string{pageTok, created.Token, bTok} {
		if strings.Contains(all, strings.TrimPrefix(secret, apitokens.Prefix)) {
			t.Fatal("an audit row carries a token")
		}
	}
}
