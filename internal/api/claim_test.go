package api

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/gotrue/gotruetest"
	"github.com/ZacxDev/muster/internal/users"
)

func claimForm(token, email, pw, pw2 string) url.Values {
	return url.Values{"token": {token}, "email": {email}, "password": {pw}, "password2": {pw2}}
}

// mintClaim mints a claim link against store and returns its token (the
// fragment), checking the link's shape on the way.
func mintClaim(t *testing.T, store ClaimMinter) string {
	t.Helper()
	link, err := NewAdminClaimLink(context.Background(), store, "https://muster.example.com", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	prefix := "https://muster.example.com" + ClaimPath + "#"
	if !strings.HasPrefix(link, prefix) || len(link) <= len(prefix) {
		t.Fatalf("link %q is not <public>%s#<token>", link, ClaimPath)
	}
	return strings.TrimPrefix(link, prefix)
}

func TestAClaimInGoTrueModeBindsUserOneAndSignsIn(t *testing.T) {
	f := newGoTrueFixture(t)
	tok := mintClaim(t, f.store)
	if w := do(f.edge, "GET", ClaimPath, nil); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Claim the admin account") {
		t.Fatalf("claim page: %d", w.Code)
	}
	w := do(f.edge, "POST", ClaimPath, claimForm(tok, "op@example.com", "a-strong-password", "a-strong-password"))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("claim: %d %s", w.Code, w.Body)
	}
	ck := setCookies(w)
	if ck[accessCookieName] == nil || ck[refreshCookieName] == nil {
		t.Fatalf("a claim in gotrue mode did not sign in: %v", ck)
	}
	u, _ := f.store.ByID(context.Background(), 1)
	if u.GotrueSub == "" || u.Email != "op@example.com" {
		t.Fatalf("user 1 not bound: %+v", u)
	}
	// The session acts as user 1, admin.
	f.seen = nil
	if w := do(f.edge, "GET", "/account", nil, withCookies(ck[accessCookieName].Value, ck[refreshCookieName].Value)); w.Code != http.StatusOK {
		t.Fatalf("the claimed session: %d", w.Code)
	}
	if ps := f.principals(); len(ps) != 1 || ps[0].UserID != 1 || ps[0].Role != authz.RoleAdmin {
		t.Fatalf("principal %+v, want user 1 admin", ps)
	}
	// Single use: the same link is now the uniform invalid page.
	if w := do(f.edge, "POST", ClaimPath, claimForm(tok, "other@example.com", "a-strong-password", "a-strong-password")); w.Code != http.StatusNotFound {
		t.Fatalf("second redemption: %d, want 404", w.Code)
	}
}

func TestAClaimInPasswordModeBindsButDoesNotSignIn(t *testing.T) {
	fake := gotruetest.New(t, gtSecret)
	store := newMemUsers()
	s := New(nil, AuthConfig{HookToken: gtHook, UIPassword: strings.Repeat("p", 20)}, log.New(io.Discard, "", 0))
	s.UseGoTrue(GoTrueAuth{Client: fake.Client(), Verifier: fake.Verifier(), Users: store})
	h := BrowserListener(s.Handler())
	tok := mintClaim(t, store)
	w := do(h, "POST", ClaimPath, claimForm(tok, "op@example.com", "a-strong-password", "a-strong-password"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Admin account claimed") {
		t.Fatalf("password-mode claim: %d %s", w.Code, w.Body)
	}
	if len(setCookies(w)) != 0 {
		t.Fatalf("password mode set GoTrue cookies: %v", setCookies(w))
	}
	if u, _ := store.ByID(context.Background(), 1); u.GotrueSub == "" {
		t.Fatal("user 1 not bound")
	}
	// It verified the login by actually signing in once.
	if fake.CountCalls("POST", "/token") != 1 {
		t.Fatalf("the claim did not prove the new login (%d password grants)", fake.CountCalls("POST", "/token"))
	}
}

func TestTheClaimFormRefusesBeforeReachingGoTrue(t *testing.T) {
	f := newGoTrueFixture(t)
	tok := mintClaim(t, f.store)
	for name, form := range map[string]url.Values{
		"11-char password": claimForm(tok, "op@example.com", "elevenchars", "elevenchars"),
		"mismatch":         claimForm(tok, "op@example.com", "a-strong-password", "a-strong-passwore"),
		"bad email":        claimForm(tok, "not an email", "a-strong-password", "a-strong-password"),
		"named email":      claimForm(tok, "Op <op@example.com>", "a-strong-password", "a-strong-password"),
	} {
		if w := do(f.edge, "POST", ClaimPath, form); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", name, w.Code)
		}
	}
	if n := len(f.fake.Calls()); n != 0 {
		t.Fatalf("refused forms reached GoTrue %d time(s)", n)
	}
	// Positive control: a good form on the same link works.
	if w := do(f.edge, "POST", ClaimPath, claimForm(tok, "op@example.com", "a-strong-password", "a-strong-password")); w.Code != http.StatusSeeOther {
		t.Fatalf("positive control: %d %s", w.Code, w.Body)
	}
}

func TestAnUnknownClaimTokenGetsTheUniformInvalidPage(t *testing.T) {
	f := newGoTrueFixture(t)
	for name, tok := range map[string]string{"unknown": "no-such-token", "empty": ""} {
		w := do(f.edge, "POST", ClaimPath, claimForm(tok, "op@example.com", "a-strong-password", "a-strong-password"))
		if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "invalid or expired") {
			t.Fatalf("%s token: %d", name, w.Code)
		}
	}
	if f.fake.CountCalls("POST", "/admin/users") != 0 {
		t.Fatal("an unknown token created a GoTrue user")
	}
}

func TestClaimCompensatesWhenTheBindingFails(t *testing.T) {
	f := newGoTrueFixture(t)
	tok := mintClaim(t, f.store)
	f.store.bindFail = errors.New("simulated commit failure")
	w := do(f.edge, "POST", ClaimPath, claimForm(tok, "op@example.com", "a-strong-password", "a-strong-password"))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("failed binding: %d", w.Code)
	}
	if c := f.fake.CountCalls("POST", "/admin/users"); c != 1 {
		t.Fatalf("create calls = %d, want 1 (the compensation must follow a real create)", c)
	}
	if n := f.fake.UserCount(); n != 0 {
		t.Fatalf("the GoTrue user created for a failed binding was left behind (%d users)", n)
	}
}

// TestTheClaimLinkIsSingleUseUnderConcurrency races N redemptions of ONE link
// against real Postgres (the row lock is the mechanism under test): exactly one
// succeeds and GoTrue gets exactly one user.
func TestTheClaimLinkIsSingleUseUnderConcurrency(t *testing.T) {
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	reset := func() {
		sys := authz.System(context.Background())
		if _, err := pool.Exec(sys, `DELETE FROM invites WHERE claim_user_id = 1`); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(sys, `UPDATE users SET gotrue_sub = NULL, email = NULL WHERE id = 1`); err != nil {
			t.Fatal(err)
		}
	}
	reset()
	t.Cleanup(reset)

	fake := gotruetest.New(t, gtSecret)
	// Hold each GoTrue create open, so that without the row lock every racer
	// would pass the "unredeemed" check before any commits (measured: with
	// no delay a lock-free mutant was caught only 2 runs in 3).
	fake.SetCreateDelay(100 * time.Millisecond)
	store := users.NewPG(pool)
	s := New(nil, AuthConfig{Mode: AuthModeGoTrue, HookToken: gtHook}, log.New(io.Discard, "", 0))
	s.UseGoTrue(GoTrueAuth{Client: fake.Client(), Verifier: fake.Verifier(), Users: store})
	h := BrowserListener(s.Handler())
	tok := mintClaim(t, store)

	const n = 8
	codes := make([]int, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			email := "op" + string(rune('a'+i)) + "@example.com"
			w := do(h, "POST", ClaimPath, claimForm(tok, email, "a-strong-password", "a-strong-password"),
				fromIP("198.51.100."+string(rune('1'+i))))
			codes[i] = w.Code
		}(i)
	}
	close(start)
	wg.Wait()
	ok, invalid := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusSeeOther:
			ok++
		case http.StatusNotFound:
			invalid++
		}
	}
	if ok != 1 || invalid != n-1 {
		t.Fatalf("codes %v: %d succeeded and %d were refused, want 1 and %d", codes, ok, invalid, n-1)
	}
	if c := fake.CountCalls("POST", "/admin/users"); c != 1 || fake.UserCount() != 1 {
		t.Fatalf("GoTrue got %d create call(s) and has %d user(s), want 1 and 1", c, fake.UserCount())
	}
	u, err := store.ByID(ctx, 1)
	if err != nil || u.GotrueSub == "" {
		t.Fatalf("user 1 not bound: %+v %v", u, err)
	}
	if _, ok := fake.User(u.GotrueSub); !ok {
		t.Fatal("user 1 is bound to a subject GoTrue does not have")
	}
}
