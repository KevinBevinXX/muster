package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
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
	// The victim's own second session, expired at the same time — refreshed
	// at the end, INSIDE the minute the junk token's quota was spent in.
	at2, rt2, _ := f.fake.Mint(sub)
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
	// Per-REFRESH-TOKEN cap: one refresh token replayed with a real expired
	// access token reaches GoTrue at most refreshPerTokenLimit times. The
	// token is marked as issued by this process so the per-SUBJECT cap
	// (TestAJunkRefreshFloodIsBoundedAndSparesTheVictim) cannot be what stops
	// it — this measures the per-token cap alone.
	markIssued(f, "planted-junk-refresh-token")
	markIssued(f, rt2) // the victim's real token, as muster would have issued it
	before := f.fake.CountCalls("POST", "/token")
	for i := 0; i < refreshPerTokenLimit+15; i++ {
		do(f.edge, "GET", "/account", nil, withCookies(at, "planted-junk-refresh-token"), asXHR)
	}
	if n := f.fake.CountCalls("POST", "/token") - before; n > refreshPerTokenLimit || n == 0 {
		t.Fatalf("one refresh token drove %d refresh calls in a minute, want 1..%d", n, refreshPerTokenLimit)
	}
	if w := do(f.edge, "GET", "/account", nil, withCookies(at2, rt2)); w.Code != http.StatusOK {
		t.Fatalf("the victim's own refresh after someone burned a junk token's quota: %d", w.Code)
	}
}

func markIssued(f *gtFixture, rt string) {
	sum := sha256.Sum256([]byte(rt))
	f.s.gt.issued.add(hex.EncodeToString(sum[:]), f.fake.Now(), issuedWindow)
}

// TestAJunkRefreshFloodIsBoundedAndSparesTheVictim: a signed expired access
// token with a NEW junk refresh token on every request (a per-token cap sees
// each as fresh) reaches GoTrue at most failedRefreshLimit times per subject
// per minute — and the victim's own browser, holding a refresh token muster
// issued, keeps refreshing through the flood.
func TestAJunkRefreshFloodIsBoundedAndSparesTheVictim(t *testing.T) {
	f := newGoTrueFixture(t)
	f.member("v@example.com", 7, authz.RoleMember)
	f.fake.SetAccessTTL(60 * time.Second)
	w := do(f.edge, "POST", "/login", url.Values{"email": {"v@example.com"}, "password": {"correct horse battery"}})
	ck := setCookies(w)
	if w.Code != http.StatusSeeOther || ck[accessCookieName] == nil {
		t.Fatalf("victim sign-in: %d", w.Code)
	}
	vat, vrt := ck[accessCookieName].Value, ck[refreshCookieName].Value
	f.fake.Advance(2 * time.Minute) // the victim's access token (and the attacker's copy) expires
	before := f.fake.CountCalls("POST", "/token")
	for i := 0; i < 200; i++ {
		w := do(f.edge, "GET", "/account", nil, withCookies(vat, "junk-"+strconv.Itoa(i)), asXHR)
		if i >= failedRefreshLimit && w.Code != http.StatusServiceUnavailable {
			t.Fatalf("junk refresh %d past the cap: %d, want 503 without calling GoTrue", i, w.Code)
		}
	}
	if n := f.fake.CountCalls("POST", "/token") - before; n > failedRefreshLimit || n == 0 {
		t.Fatalf("200 junk refreshes for one subject reached GoTrue %d times, want 1..%d", n, failedRefreshLimit)
	}
	if w := do(f.edge, "GET", "/account", nil, withCookies(vat, vrt)); w.Code != http.StatusOK {
		t.Fatalf("the victim's own refresh during the flood: %d, want 200", w.Code)
	}
	// The per-token map is bounded however many junk tokens arrive.
	for i := 0; i < 3*boundedWindowCap; i++ {
		f.s.gt.refreshPerToken.take("k"+strconv.Itoa(i), f.fake.Now(), time.Minute, refreshPerTokenLimit)
	}
	if n := f.s.gt.refreshPerToken.size(); n > boundedWindowCap {
		t.Fatalf("the per-token map grew to %d (cap %d)", n, boundedWindowCap)
	}
}

// TestChurnCannotEvictAVictimsBackoff is the auditor's churn probe: a victim
// in backoff, then more than signinLimiterCap sign-ins at made-up emails. The
// victim must still be blocked; a table full of protected records refuses a
// NEW key rather than evict one; and a stale ticket cannot settle a newer
// record of the same account.
func TestChurnCannotEvictAVictimsBackoff(t *testing.T) {
	var l signinLimiter
	now := time.Unix(946684800, 0)
	ipN := 0
	fail := func(acct string) bool {
		ipN++
		tk, ok := l.reserve(acct, "ip"+strconv.Itoa(ipN), now)
		if ok {
			l.settle(tk, outcomeFailed)
		}
		return ok
	}
	for i := 0; i <= accountFreeFailures; i++ {
		if !fail("victim@example.com") {
			t.Fatalf("victim failure %d refused early", i+1)
		}
	}
	if fail("victim@example.com") {
		t.Fatal("control: the victim is not in backoff")
	}
	for i := 0; i < signinLimiterCap+1; i++ {
		fail("made-up-" + strconv.Itoa(i) + "@example.com")
	}
	if fail("victim@example.com") {
		t.Fatal("churn through more than the cap evicted the victim's backoff")
	}

	// A table full of PROTECTED records refuses a new key (fail closed).
	var full signinLimiter
	for i := 0; i < signinLimiterCap; i++ {
		acct := "p" + strconv.Itoa(i)
		for j := 0; j <= accountFreeFailures; j++ {
			ipN++
			tk, _ := full.reserve(acct, "ip"+strconv.Itoa(ipN), now)
			full.settle(tk, outcomeFailed)
		}
	}
	if _, ok := full.reserve("brand-new@example.com", "fresh-ip", now); ok {
		t.Fatal("a full table of records in backoff evicted one to admit a new key")
	}
	if _, ok := full.reserve("brand-new@example.com", "fresh-ip", now.Add(accountBackoffMax+time.Second)); !ok {
		t.Fatal("control: once the backoffs expired, a new key was still refused")
	}

	// Generation: a ticket for a record that is gone must not settle its
	// replacement.
	var g signinLimiter
	stale, _ := g.reserve("x@example.com", "a", now)
	g.mu.Lock()
	g.accOrder.Remove(g.accounts["x@example.com"])
	delete(g.accounts, "x@example.com")
	g.mu.Unlock()
	if _, ok := g.reserve("x@example.com", "b", now); !ok {
		t.Fatal("control: the recreated record refused its first attempt")
	}
	g.settle(stale, outcomeUnjudged)
	if _, ok := g.reserve("x@example.com", "c", now); ok {
		t.Fatal("a stale ticket cleared the in-flight flag of the account's NEW record")
	}
}

// TestARefreshForAnotherSubjectIsRefused: a muster_rt that belongs to someone
// else, beside the victim's expired access token, must not sign the victim's
// browser in as that someone.
func TestARefreshForAnotherSubjectIsRefused(t *testing.T) {
	f := newGoTrueFixture(t)
	victim := f.member("v@example.com", 7, authz.RoleMember)
	planter := f.member("p@example.com", 8, authz.RoleMember)
	f.fake.SetAccessTTL(60 * time.Second)
	vat, _, _ := f.fake.Mint(victim)
	_, prt, _ := f.fake.Mint(planter)
	f.fake.Advance(2 * time.Minute)
	f.seen = nil
	w := do(f.edge, "GET", "/account", nil, withCookies(vat, prt), asXHR)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("a refresh token of another subject: %d, want 401", w.Code)
	}
	if c := setCookies(w)[accessCookieName]; c == nil || c.MaxAge >= 0 {
		t.Fatal("the mismatched pair was not cleared")
	}
	if len(f.principals()) != 0 {
		t.Fatalf("a principal was admitted: %+v", f.principals())
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

// TestParallelSignInsAtOneAccountAreBounded is the audit's probe as a test:
// 200 PARALLEL wrong-password sign-ins at one account, each from its own
// forwarded address, against a GoTrue that holds every /token call open.
// The reservation (count up front, one in flight per account) bounds how many
// reach GoTrue to the backoff schedule's free attempts plus the one that
// starts the backoff. Check-then-record let 136-179 of 200 through.
func TestParallelSignInsAtOneAccountAreBounded(t *testing.T) {
	f := newGoTrueFixture(t)
	f.member("victim@example.com", 7, authz.RoleMember)
	f.fake.SetTokenDelay(20 * time.Millisecond)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			ip := "198.51." + strconv.Itoa(i/250) + "." + strconv.Itoa(i%250+1)
			do(f.edge, "POST", "/login", url.Values{"email": {"victim@example.com"}, "password": {"wrong password!!"}}, fromIP(ip))
		}(i)
	}
	close(start)
	wg.Wait()
	if n := f.fake.CountCalls("POST", "/token"); n > accountFreeFailures+1 || n == 0 {
		t.Fatalf("200 parallel sign-ins at one account reached GoTrue %d times, want 1..%d", n, accountFreeFailures+1)
	}

	// One in flight per account: a burst of 10 at a FRESH account (inside its
	// free failures, so the backoff is not what stops them) reaches GoTrue once
	// while that one call is held open.
	b := newGoTrueFixture(t)
	b.member("burst@example.com", 9, authz.RoleMember)
	b.fake.SetTokenDelay(300 * time.Millisecond)
	var wg3 sync.WaitGroup
	start3 := make(chan struct{})
	for i := 0; i < 10; i++ {
		wg3.Add(1)
		go func(i int) {
			defer wg3.Done()
			<-start3
			do(b.edge, "POST", "/login", url.Values{"email": {"burst@example.com"}, "password": {"wrong password!!"}},
				fromIP("203.0.113."+strconv.Itoa(i+1)))
		}(i)
	}
	close(start3)
	wg3.Wait()
	if n := b.fake.CountCalls("POST", "/token"); n != 1 {
		t.Fatalf("a burst of 10 at one fresh account reached GoTrue %d times, want 1 (one in flight per account)", n)
	}

	// The per-address window is reserved the same way: 200 parallel attempts
	// from ONE address at 200 different accounts.
	g := newGoTrueFixture(t)
	g.fake.SetTokenDelay(20 * time.Millisecond)
	var wg2 sync.WaitGroup
	start2 := make(chan struct{})
	for i := 0; i < 200; i++ {
		wg2.Add(1)
		go func(i int) {
			defer wg2.Done()
			<-start2
			do(g.edge, "POST", "/login", url.Values{"email": {"spray" + strconv.Itoa(i) + "@example.com"}, "password": {"x"}}, fromIP("192.0.2.9"))
		}(i)
	}
	close(start2)
	wg2.Wait()
	if n := g.fake.CountCalls("POST", "/token"); n > loginFailureLimit || n == 0 {
		t.Fatalf("200 parallel sign-ins from one address reached GoTrue %d times, want 1..%d", n, loginFailureLimit)
	}
}

// TestTheSignInLimiterIsBoundedAndRefundsUnjudgedAttempts: the limiter's maps
// never exceed signinLimiterCap (oldest evicted), a success or an unjudged
// attempt gives the address its slot back, and only a failure keeps it.
func TestTheSignInLimiterIsBoundedAndRefundsUnjudgedAttempts(t *testing.T) {
	var l signinLimiter
	now := time.Unix(946684800, 0)
	for i := 0; i < signinLimiterCap+500; i++ {
		tk, ok := l.reserve("acct"+strconv.Itoa(i)+"@example.com", "ip"+strconv.Itoa(i), now)
		if !ok {
			t.Fatalf("reserve %d refused", i)
		}
		l.settle(tk, outcomeFailed)
	}
	if a, ips := l.sizes(); a > signinLimiterCap || ips > signinLimiterCap {
		t.Fatalf("limiter grew past its cap: %d accounts, %d addresses (cap %d)", a, ips, signinLimiterCap)
	}
	// One address: failures keep their slot, unjudged/success give it back.
	var m signinLimiter
	for i := 0; i < loginFailureLimit*3; i++ {
		tk, ok := m.reserve("u"+strconv.Itoa(i), "203.0.113.1", now)
		if !ok {
			t.Fatalf("an address with only refunded attempts was refused at %d", i)
		}
		m.settle(tk, []signinOutcome{outcomeSucceeded, outcomeUnjudged}[i%2])
	}
	for i := 0; i < loginFailureLimit; i++ {
		tk, _ := m.reserve("f"+strconv.Itoa(i), "203.0.113.1", now)
		m.settle(tk, outcomeFailed)
	}
	if _, ok := m.reserve("another", "203.0.113.1", now); ok {
		t.Fatal("an address with loginFailureLimit failures was not limited")
	}
	// One account: a second reservation while one is in flight is refused.
	var a signinLimiter
	tk, _ := a.reserve("x@example.com", "198.51.100.1", now)
	if _, ok := a.reserve("x@example.com", "198.51.100.2", now); ok {
		t.Fatal("two attempts at one account were in flight at once")
	}
	a.settle(tk, outcomeUnjudged)
	if _, ok := a.reserve("x@example.com", "198.51.100.2", now); !ok {
		t.Fatal("after the in-flight attempt settled, the account was still refused")
	}
}
