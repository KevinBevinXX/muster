package api

import (
	"bytes"
	"context"
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
	"github.com/ZacxDev/muster/internal/gotrue/gotruetest"
)

// TestATransientFailureKeepsTheSession: a GoTrue 502 or 429 on refresh, or a
// database error resolving the user, is muster being unable to answer — not
// a verdict on the cookies. They are kept, the caller gets 503, and the failed
// refresh is NOT cached: the very next request refreshes for real.
func TestATransientFailureKeepsTheSession(t *testing.T) {
	for _, status := range []int{http.StatusBadGateway, http.StatusTooManyRequests} {
		f := newGoTrueFixture(t)
		sub := f.member("m@example.com", 7, authz.RoleMember)
		f.fake.SetAccessTTL(60 * time.Second)
		at, rt, _ := f.fake.Mint(sub)
		f.fake.Advance(2 * time.Minute)

		f.fake.FailToken(status, 1)
		w := do(f.edge, "GET", "/account", nil, withCookies(at, rt), asXHR)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("GoTrue %d on refresh: %d, want 503", status, w.Code)
		}
		if ck := setCookies(w); len(ck) != 0 {
			t.Fatalf("GoTrue %d on refresh changed cookies: %v", status, ck)
		}
		nav := do(f.edge, "GET", "/account", nil, withCookies(at, rt), asNavigation)
		if nav.Code != http.StatusOK {
			// The failure was not cached: this navigation refreshed for real.
			t.Fatalf("the request after a %d: %d, want 200 (a failed refresh must not be cached)", status, nav.Code)
		}
		if setCookies(nav)[accessCookieName] == nil {
			t.Fatal("the recovered refresh set no new cookies")
		}
	}
	// A database error resolving the user.
	f := newGoTrueFixture(t)
	sub := f.member("m@example.com", 7, authz.RoleMember)
	at, rt, _ := f.fake.Mint(sub)
	f.store.lookupErr = errors.New("conn refused")
	w := do(f.edge, "GET", "/account", nil, withCookies(at, rt), asNavigation)
	if w.Code != http.StatusServiceUnavailable || len(setCookies(w)) != 0 {
		t.Fatalf("a database error: %d cookies %v, want 503 and none touched", w.Code, setCookies(w))
	}
	// Control: an AUTH failure (unknown subject) still clears them.
	f.store.lookupErr = nil
	stranger := f.fake.AddUser("x@example.com", "correct horse battery")
	sat, srt, _ := f.fake.Mint(stranger)
	if c := setCookies(do(f.edge, "GET", "/account", nil, withCookies(sat, srt), asXHR))[accessCookieName]; c == nil || c.MaxAge >= 0 {
		t.Fatal("control: an unknown subject must clear the cookies")
	}
}

// TestARefreshNeedsASignedExpiredAccessToken: a request carrying only a
// muster_rt — real or invented — never reaches GoTrue; and one subject's
// replayed expired token is capped per minute.
func TestARefreshNeedsASignedExpiredAccessToken(t *testing.T) {
	f := newGoTrueFixture(t)
	sub := f.member("m@example.com", 7, authz.RoleMember)
	f.fake.SetAccessTTL(60 * time.Second)
	at, rt, _ := f.fake.Mint(sub)
	f.fake.Advance(2 * time.Minute)
	for i := 0; i < 50; i++ {
		if w := do(f.edge, "GET", "/account", nil, withCookies("", rt), asXHR); w.Code != http.StatusUnauthorized {
			t.Fatalf("rt-only: %d, want 401", w.Code)
		}
	}
	if n := f.fake.CountCalls("POST", "/token"); n != 0 {
		t.Fatalf("rt-only requests made %d GoTrue refresh calls, want 0", n)
	}
	// Control: the same rt WITH its signed (expired) access token refreshes.
	if w := do(f.edge, "GET", "/account", nil, withCookies(at, rt)); w.Code != http.StatusOK {
		t.Fatalf("control: a signed expired token + rt: %d", w.Code)
	}
	// Per-subject cap: an expired token replayed with junk refresh tokens.
	before := f.fake.CountCalls("POST", "/token")
	for i := 0; i < refreshPerSubjectLimit+10; i++ {
		do(f.edge, "GET", "/account", nil, withCookies(at, "junk-"+string(rune('a'+i%26))+strings.Repeat("x", i)), asXHR)
	}
	if n := f.fake.CountCalls("POST", "/token") - before; n > refreshPerSubjectLimit {
		t.Fatalf("one subject drove %d refresh calls in a minute, cap is %d", n, refreshPerSubjectLimit)
	}
}

// TestSignInBackoffIsPerAccountWithNoGlobalLockout: failures on one account
// make THAT account wait, doubling; another account (even from the same
// address) is unaffected; one address spraying accounts hits the per-address
// limit; and no amount of failure elsewhere locks out a fresh account+address.
func TestSignInBackoffIsPerAccountWithNoGlobalLockout(t *testing.T) {
	f := newGoTrueFixture(t)
	f.member("victim@example.com", 7, authz.RoleMember)
	f.member("other@example.com", 8, authz.RoleMember)
	login := func(email, pw, ip string) int {
		return do(f.edge, "POST", "/login", url.Values{"email": {email}, "password": {pw}}, fromIP(ip)).Code
	}
	const good = "correct horse battery"
	// Free failures, from different addresses (an address rotator).
	for i := 0; i < accountFreeFailures; i++ {
		if c := login("Victim@Example.com ", "wrong password!!", "198.51.100."+string(rune('1'+i))); c != http.StatusUnauthorized {
			t.Fatalf("free failure %d: %d", i+1, c)
		}
	}
	// The next failure starts the backoff (base delay).
	if c := login("victim@example.com", "wrong password!!", "203.0.113.50"); c != http.StatusUnauthorized {
		t.Fatalf("first counted failure: %d", c)
	}
	before := f.fake.CountCalls("POST", "/token")
	if c := login("victim@example.com", good, "203.0.113.51"); c != http.StatusTooManyRequests {
		t.Fatalf("inside the backoff, even the right password from a NEW address: %d, want 429", c)
	}
	if f.fake.CountCalls("POST", "/token") != before {
		t.Fatal("a throttled attempt reached GoTrue")
	}
	// Another account, same address: unaffected.
	if c := login("other@example.com", good, "203.0.113.51"); c != http.StatusSeeOther {
		t.Fatalf("another account: %d, want 303", c)
	}
	// Doubling: after the base wait one more failure, then the next wait is 2x.
	f.fake.Advance(accountBackoffBase + time.Millisecond)
	if c := login("victim@example.com", "wrong password!!", "203.0.113.52"); c != http.StatusUnauthorized {
		t.Fatalf("after the base wait: %d", c)
	}
	f.fake.Advance(accountBackoffBase + time.Millisecond)
	if c := login("victim@example.com", good, "203.0.113.53"); c != http.StatusTooManyRequests {
		t.Fatalf("the second wait must be longer than the first: %d, want 429", c)
	}
	f.fake.Advance(accountBackoffBase + time.Millisecond)
	if c := login("victim@example.com", good, "203.0.113.53"); c != http.StatusSeeOther {
		t.Fatalf("after 2x base: %d, want 303", c)
	}
	// Success cleared it: a single failure now is free again, so the right
	// password straight after it is not held back.
	if c := login("victim@example.com", "wrong password!!", "203.0.113.53"); c != http.StatusUnauthorized {
		t.Fatalf("after success, a failure: %d", c)
	}
	if c := login("victim@example.com", good, "203.0.113.53"); c != http.StatusSeeOther {
		t.Fatalf("after success the count must restart: %d, want 303", c)
	}
	// One address spraying many accounts hits the per-address limit.
	for i := 0; i < loginFailureLimit; i++ {
		login("nobody"+string(rune('a'+i))+"@example.com", "wrong password!!", "192.0.2.77")
	}
	if c := login("other@example.com", good, "192.0.2.77"); c != http.StatusTooManyRequests {
		t.Fatalf("a spraying address: %d, want 429", c)
	}
	// No global lever: hundreds of failures on other accounts and addresses do
	// not stop a fresh account from a fresh address.
	for i := 0; i < 200; i++ {
		login("spray"+strings.Repeat("x", i)+"@example.com", "wrong password!!", "192.0.2."+string(rune('0'+i%10)))
	}
	if c := login("other@example.com", good, "203.0.113.200"); c != http.StatusSeeOther {
		t.Fatalf("after a flood elsewhere, an unrelated sign-in: %d, want 303 (no global lockout)", c)
	}
}

// TestTheClaimTokenNeverReachesTheLog: the whole claim — the page, the
// redemption, and an old-style path request — leaves no trace of the token in
// muster's log.
func TestTheClaimTokenNeverReachesTheLog(t *testing.T) {
	var mu sync.Mutex
	var buf bytes.Buffer
	logger := log.New(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}), "", 0)
	fake := gotruetest.New(t, gtSecret)
	store := newMemUsers()
	s := New(nil, AuthConfig{Mode: AuthModeGoTrue, HookToken: gtHook}, logger)
	s.UseGoTrue(GoTrueAuth{Client: fake.Client(), Verifier: fake.Verifier(), Users: store})
	h := BrowserListener(s.Handler())
	tok := mintClaim(t, store)
	do(h, "GET", ClaimPath, nil)
	do(h, "GET", "/auth/claim/"+tok, nil) // an old-style link pasted by hand
	if w := do(h, "POST", ClaimPath, claimForm(tok, "op@example.com", "a-strong-password", "a-strong-password")); w.Code != http.StatusSeeOther {
		t.Fatalf("claim: %d", w.Code)
	}
	mu.Lock()
	out := buf.String()
	mu.Unlock()
	if !strings.Contains(out, "POST /auth/claim") || !strings.Contains(out, "/auth/claim/[redacted]") {
		t.Fatalf("positive control: the access log did not record the claim requests:\n%s", out)
	}
	if strings.Contains(out, tok) {
		t.Fatalf("the claim token is in the log:\n%s", out)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// TestSafeReturnToRefusesEveryOffOriginShape: every way a ?next= can leave the
// origin collapses to "/", including the bytes browsers STRIP before parsing
// (a tab turns "/\t/evil" into "//evil") and the percent-encoded forms.
func TestSafeReturnToRefusesEveryOffOriginShape(t *testing.T) {
	for _, bad := range []string{
		"", "x", "//evil.example", "/\\evil.example", "/\t/evil.example/x", "/\n/evil.example", "/\r/evil.example",
		"/ /evil", "/\x00/evil", "/\x7f/evil", "/%2F%2Fevil.example", "/%2f/evil.example", "/%5Cevil.example",
		"/%09/evil.example", "/%0A/evil.example", "https://evil.example/", "http:evil.example", "\t//evil.example",
	} {
		if got := safeReturnTo(bad); got != "/" {
			t.Errorf("safeReturnTo(%q) = %q, want /", bad, got)
		}
	}
	for _, good := range []string{"/", "/tasks", "/tasks?tab=x&next=%2Fy", "/agents/keen-wren", "/a/b#frag"} {
		if got := safeReturnTo(good); got != good {
			t.Errorf("safeReturnTo(%q) = %q, want it kept", good, got)
		}
	}
	// End to end: a signed-in password-mode visit to /login?next=<tab trick>
	// redirects home.
	s := New(nil, AuthConfig{UIPassword: strings.Repeat("p", 20)}, log.New(io.Discard, "", 0))
	r, _ := http.NewRequest("GET", "/login?next="+url.QueryEscape("/\t/evil.example/x"), nil)
	r.Header.Set("Cookie", sessionCookieFor(s))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if loc := w.Header().Get("Location"); w.Code != http.StatusSeeOther || loc != "/" {
		t.Fatalf("tab trick through /login: %d Location %q, want 303 /", w.Code, loc)
	}
}

// TestTrackRefusesARequestThatResolvedJustBeforeItsSessionEnded: the window
// between "the session resolved" and "its stream registered" is closed by
// checking disable/revocation under the same lock the sweep takes.
func TestTrackRefusesARequestThatResolvedJustBeforeItsSessionEnded(t *testing.T) {
	f := newGoTrueFixture(t)
	sub := f.member("m@example.com", 7, authz.RoleMember)
	at, _, _ := f.fake.Mint(sub)
	c, err := f.fake.Verifier().Verify(at)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, done, ok := f.s.gt.track(ctx, 7, c); !ok {
		t.Fatal("control: a live session must track")
	} else {
		done()
	}
	f.s.gt.setDisabled(7, true)
	if _, _, ok := f.s.gt.track(ctx, 7, c); ok {
		t.Fatal("a disabled user's request registered after the sweep")
	}
	f.s.gt.setDisabled(7, false)
	f.s.gt.revoke(7, c, "local")
	if _, _, ok := f.s.gt.track(ctx, 7, c); ok {
		t.Fatal("a signed-out session's request registered after the sweep")
	}
}

// TestADisabledUserIsRefusedOnceTheCacheTurnsOver: a disable made OUTSIDE
// this process (the CLI, another replica) reaches only the database. The
// request path's own disabled_at check is what refuses it, within the 30 s
// user cache.
func TestADisabledUserIsRefusedOnceTheCacheTurnsOver(t *testing.T) {
	f := newGoTrueFixture(t)
	sub := f.member("m@example.com", 7, authz.RoleMember)
	f.fake.SetAccessTTL(time.Hour)
	at, rt, _ := f.fake.Mint(sub)
	if w := do(f.edge, "GET", "/account", nil, withCookies(at, rt)); w.Code != http.StatusOK {
		t.Fatalf("before: %d", w.Code)
	}
	if _, err := f.store.SetDisabled(context.Background(), 7, true); err != nil { // the row only
		t.Fatal(err)
	}
	f.fake.Advance(userCacheTTL + time.Second)
	if w := do(f.edge, "GET", "/account", nil, withCookies(at, rt), asXHR); w.Code != http.StatusUnauthorized {
		t.Fatalf("a row-only disable after the cache window: %d, want 401", w.Code)
	}
}
