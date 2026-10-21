package api

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
)

// TestASignOutEverywhereThatDidNotFinishSaysSo: Sign out everywhere also signs
// out the person's paired phones, through the durable record
// (RecordSignOutEverywhere). When that record fails, or the session cannot be
// resolved at all, the person is SHOWN that it did not finish — never
// redirected to the sign-in page as though it had. The controls: the same
// global sign-out with the record working redirects, and a plain sign-out of
// one session with no session redirects as before.
func TestASignOutEverywhereThatDidNotFinishSaysSo(t *testing.T) {
	signedIn := func() (*gtFixture, string, string) {
		f := newGoTrueFixture(t)
		sub := f.member("phone-owner@example.com", 7, authz.RoleMember)
		at, rt, _ := f.fake.Mint(sub)
		return f, at, rt
	}

	f, at, rt := signedIn()
	if w := do(f.edge, "POST", "/logout", url.Values{"scope": {"global"}}, withCookies(at, rt)); w.Code != http.StatusSeeOther {
		t.Fatalf("CONTROL: a global sign-out that was recorded: %d; want 303", w.Code)
	}

	f, at, rt = signedIn()
	f.store.mu.Lock()
	f.store.signOutErr = errors.New("database unavailable")
	f.store.mu.Unlock()
	w := do(f.edge, "POST", "/logout", url.Values{"scope": {"global"}}, withCookies(at, rt), asNavigation)
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "Sign out everywhere did not finish") ||
		!strings.Contains(w.Body.String(), "could not record the sign-out") {
		t.Fatalf("an unrecorded global sign-out: %d %s; want 500 saying it could not be recorded", w.Code, truncate(w.Body.String(), 300))
	}

	f, _, _ = signedIn()
	w = do(f.edge, "POST", "/logout", url.Values{"scope": {"global"}}, asNavigation)
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "were NOT signed out") {
		t.Fatalf("a global sign-out with no session: %d %s; want 500 saying nothing was signed out", w.Code, truncate(w.Body.String(), 300))
	}
	if w := do(f.edge, "POST", "/logout", url.Values{}); w.Code != http.StatusSeeOther {
		t.Fatalf("CONTROL: a plain sign-out with no session: %d; want 303", w.Code)
	}
}
