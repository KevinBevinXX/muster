package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/gotrue/gotruetest"
	"github.com/ZacxDev/muster/internal/ui"
)

// "Confirm it's you" WITH A PASSKEY, and the 12-hour window, end to end through
// the real GoTrue session tier (over the fake GoTrue, which does no
// cryptography — the signature is exercised by e2e/tests/mirror-stepup.spec.ts
// against the real image; what is pinned here is everything muster decides).
//
// 🔴 EVERY EXPECTATION IN THIS FILE IS A LITERAL — the routes, the refusals,
// the twelve hours — never a constant read back from the code under test, so
// the file also compiles and RUNS against the commit before the feature (where
// each test below fails: the routes 404 and the window is five minutes). That
// is the red half of the matrix in the PR.

const (
	pkConfirmOptions = "/auth/confirm/passkey/options"
	pkConfirmVerify  = "/auth/confirm/passkey/verify"
	// The window the operator chose, restated here on purpose.
	twelveHours = 12 * time.Hour
	// What the dialog says to a passkey that verified for somebody else.
	otherAccountRefusal = "That passkey belongs to a different account. Use a passkey for the account you are signed in to, or your password."
)

// newPasskeyStepUpRig is the step-up rig with passkeys on in its GoTrue, the
// owner (user 1) holding passkey "owner-cred" and user 2 holding "other-cred".
func newPasskeyStepUpRig(t *testing.T) *stepUpRig {
	t.Helper()
	r := newStepUpRig(t)
	r.mirror.v2 = &fakeMirrorV2{} // "main" is a Claude pane: the rendered view's send has somewhere to go
	r.fake.EnablePasskeys()
	r.fake.AddPasskey(r.owner, "owner-cred")
	other, _ := r.store.ByID(context.Background(), 2)
	r.fake.AddPasskey(other.GotrueSub, "other-cred")
	return r
}

// passkeyConfirm runs the confirm ceremony as the session in (at, rt): options,
// then verify with an assertion for credID carrying flags.
func (r *stepUpRig) passkeyConfirm(t *testing.T, at, rt, credID string, flags byte, opts ...reqOpt) *httptest.ResponseRecorder {
	t.Helper()
	cookies := append([]reqOpt{withCookies(at, rt)}, opts...)
	o := postJSON(r.edge, pkConfirmOptions, map[string]any{}, cookies...)
	if o.Code != http.StatusOK {
		t.Fatalf("confirm options: %d %s", o.Code, o.Body)
	}
	body := jsonBody(t, o)
	ch, _ := body["challenge_id"].(string)
	if body["available"] != true || ch == "" {
		t.Fatalf("confirm options offered no passkey challenge: %s", o.Body)
	}
	return postJSON(r.edge, pkConfirmVerify, map[string]any{
		"challenge_id": ch, "credential": gotruetest.Assertion(credID, flags)}, cookies...)
}

// send POSTs the rendered view's composer as the session in (at, rt).
func (r *stepUpRig) send(at, rt, text string) *httptest.ResponseRecorder {
	return do(r.edge, http.MethodPost, "/mirror/view/send", url.Values{"host": {"ws"}, "session": {"main"}, "text": {text}}, withCookies(at, rt), asXHR)
}

// stale ages every session by one minute past twelve hours.
func (r *stepUpRig) stale() { r.fake.Advance(twelveHours + time.Minute) }

// TestConfirmingWithAPasskeyMakesAStaleSessionFresh is the whole loop: stale →
// rw refused → the options ask the browser for user verification → a verified
// assertion for the SESSION's user → new cookies for the same user whose
// sign-in is now → rw attaches. The answer says how long that lasts.
func TestConfirmingWithAPasskeyMakesAStaleSessionFresh(t *testing.T) {
	r := newPasskeyStepUpRig(t)
	at, rt, _ := r.fake.Mint(r.owner)
	r.stale()
	r.mustRefuse(t, at, rt, "rw", http.StatusForbidden, "stale before confirming")

	o := postJSON(r.edge, pkConfirmOptions, map[string]any{}, withCookies(at, rt))
	if o.Code != http.StatusOK {
		t.Fatalf("options: %d %s", o.Code, o.Body)
	}
	opts, _ := jsonBody(t, o)["options"].(map[string]any)
	if opts["userVerification"] != "required" {
		t.Fatalf("the confirm options do not REQUIRE user verification: %v", opts["userVerification"])
	}

	w := r.passkeyConfirm(t, at, rt, "owner-cred", gotruetest.FlagUPUV)
	if w.Code != http.StatusOK {
		t.Fatalf("confirm with the owner's passkey: %d %s", w.Code, w.Body)
	}
	var res struct {
		FreshFor int64 `json:"fresh_for"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || res.FreshFor <= twelveHours.Milliseconds()-2000 || res.FreshFor > twelveHours.Milliseconds() {
		t.Fatalf("confirm answered %s; want fresh_for within 2s of %d", w.Body, twelveHours.Milliseconds())
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
	if signedIn, ok := c.AuthenticatedAt(); !ok || !signedIn.Equal(time.Unix(r.fake.Now().Unix(), 0)) {
		t.Fatalf("the new session signed in at %v, want now (%v)", signedIn, r.fake.Now())
	}
	if got := r.mustAttach(t, nat.Value, nrt.Value, "rw"); got != "rw" {
		t.Fatalf("after confirming, the rw attach = %q", got)
	}
	if w := r.send(nat.Value, nrt.Value, "after the passkey"); w.Code != http.StatusNoContent {
		t.Fatalf("after confirming, a send: %d %s", w.Code, w.Body)
	}
}

// TestAPasskeyConfirmWithoutUserVerificationIsRefused: 🔴 the UV guard. An
// assertion that only proved PRESENCE (UV=0) — which GoTrue itself would accept
// — is refused by muster with its own refusal, before GoTrue is asked to verify
// it, sets no session, and leaves the session stale. The control (same
// credential, UV=1) confirms.
func TestAPasskeyConfirmWithoutUserVerificationIsRefused(t *testing.T) {
	r := newPasskeyStepUpRig(t)
	at, rt, _ := r.fake.Mint(r.owner)
	r.stale()
	verifies := func() int { return r.fake.CountCalls(http.MethodPost, "/passkeys/authentication/verify") }
	before := verifies()
	w := r.passkeyConfirm(t, at, rt, "owner-cred", gotruetest.FlagUP)
	if w.Code != http.StatusUnauthorized || !strings.Contains(errorOf(w), "did not verify you") {
		t.Fatalf("a UV=0 confirm: %d %s, want 401 naming the missing verification", w.Code, w.Body)
	}
	if verifies() != before {
		t.Fatal("a UV=0 assertion was forwarded to GoTrue")
	}
	if ck := setCookies(w)[accessCookieName]; ck != nil {
		if c, _ := r.fake.Verifier().Verify(ck.Value); c.Subject != "" {
			if signedIn, _ := c.AuthenticatedAt(); r.fake.Now().Sub(signedIn) < time.Hour {
				t.Fatalf("a UV=0 confirm issued a freshly signed-in session (%v)", signedIn)
			}
		}
	}
	r.mustRefuse(t, at, rt, "rw", http.StatusForbidden, "still stale after a UV=0 confirm")

	// Control: the same passkey WITH user verification.
	if w := r.passkeyConfirm(t, at, rt, "owner-cred", gotruetest.FlagUPUV, fromIP("203.0.113.71")); w.Code != http.StatusOK {
		t.Fatalf("control: the same passkey with UV: %d %s", w.Code, w.Body)
	}
}

// TestAPasskeyConfirmForAnotherUserIsRefused: 🔴 the user binding. User 2's
// passkey, perfectly valid and user-verified, presented on user 1's session:
// GoTrue (usernameless) verifies it and mints USER 2 a session — and muster
// refuses, sets NO cookie (user 1's browser must never become user 2), ends the
// session it was just handed, and leaves user 1 stale.
func TestAPasskeyConfirmForAnotherUserIsRefused(t *testing.T) {
	r := newPasskeyStepUpRig(t)
	at, rt, _ := r.fake.Mint(r.owner)
	r.stale()
	logouts := func() int { return r.fake.CountCalls(http.MethodPost, "/logout") }
	before := logouts()
	w := r.passkeyConfirm(t, at, rt, "other-cred", gotruetest.FlagUPUV)
	if w.Code != http.StatusForbidden || errorOf(w) != otherAccountRefusal {
		t.Fatalf("another user's passkey: %d %s, want 403 %q", w.Code, w.Body, otherAccountRefusal)
	}
	other, _ := r.store.ByID(context.Background(), 2)
	// The session wrapper may have REFRESHED user 1's own cookies (the access
	// token is twelve hours old); what must not be set is anybody's NEW sign-in,
	// and never user 2's session.
	if ck := setCookies(w)[accessCookieName]; ck != nil {
		c, err := r.fake.Verifier().Verify(ck.Value)
		if err != nil || c.Subject != r.owner || c.Subject == other.GotrueSub {
			t.Fatalf("user 1's browser was handed a session for %q (%v), want its own user %q", c.Subject, err, r.owner)
		}
		if signedIn, _ := c.AuthenticatedAt(); r.fake.Now().Sub(signedIn) < time.Hour {
			t.Fatalf("a refused confirm issued a freshly signed-in session (%v)", signedIn)
		}
	}
	if got := logouts(); got != before+1 {
		t.Fatalf("GoTrue /logout calls = %d, want %d: the session minted for the other user was not ended", got, before+1)
	}
	r.mustRefuse(t, at, rt, "rw", http.StatusForbidden, "still stale after another user's passkey")
	if w := r.send(at, rt, "must not be sent"); w.Code != http.StatusForbidden {
		t.Fatalf("a send after another user's passkey: %d, want 403", w.Code)
	}

	// Control: the owner's own passkey on the same session confirms.
	if w := r.passkeyConfirm(t, at, rt, "owner-cred", gotruetest.FlagUPUV, fromIP("203.0.113.72")); w.Code != http.StatusOK {
		t.Fatalf("control: the owner's own passkey: %d %s", w.Code, w.Body)
	}
}

// TestAConfirmOnOneSessionLeavesAnotherStale: 🔴 freshness is the SESSION's —
// one browser on one device — not the user's. The same user is signed in twice
// (a laptop and a phone); both go stale; a confirm on the laptop, by passkey
// and then again by password, makes the LAPTOP's new cookies fresh and leaves
// the phone refused — for the terminal's rw attach and for a send alike.
func TestAConfirmOnOneSessionLeavesAnotherStale(t *testing.T) {
	for _, how := range []string{"passkey", "password"} {
		r := newPasskeyStepUpRig(t)
		laptopAT, laptopRT, _ := r.fake.Mint(r.owner)
		phoneAT, phoneRT, _ := r.fake.Mint(r.owner)
		r.stale()
		r.mustRefuse(t, laptopAT, laptopRT, "rw", http.StatusForbidden, how+": laptop stale before confirming")
		r.mustRefuse(t, phoneAT, phoneRT, "rw", http.StatusForbidden, how+": phone stale before confirming")

		var w *httptest.ResponseRecorder
		if how == "passkey" {
			w = r.passkeyConfirm(t, laptopAT, laptopRT, "owner-cred", gotruetest.FlagUPUV)
		} else {
			w = r.confirm(laptopAT, laptopRT, url.Values{"password": {stepUpOwnerPW}})
		}
		if w.Code != http.StatusOK {
			t.Fatalf("%s: the laptop's confirm: %d %s", how, w.Code, w.Body)
		}
		ck := setCookies(w)
		if got := r.mustAttach(t, ck[accessCookieName].Value, ck[refreshCookieName].Value, "rw"); got != "rw" {
			t.Fatalf("%s: control: the laptop, confirmed, attached %q", how, got)
		}

		// The phone presents ITS OWN session: same user, still signed in twelve
		// hours ago.
		r.mustRefuse(t, phoneAT, phoneRT, "rw", http.StatusForbidden, how+": the phone after the laptop confirmed")
		if w := r.send(phoneAT, phoneRT, "from the phone"); w.Code != http.StatusForbidden || w.Header().Get("X-Muster-Step-Up") != "required" {
			t.Fatalf("%s: the phone's send after the laptop confirmed: %d, want 403 + X-Muster-Step-Up", how, w.Code)
		}
		if got := r.mustAttach(t, phoneAT, phoneRT, ""); got != "ro" {
			t.Fatalf("%s: the phone must still WATCH: attached %q", how, got)
		}
	}
}

// TestThePasskeyConfirmSpendsNoChallengeItCannotUse: GoTrue's passkey budget.
// Rendering the page asks GoTrue for no challenge; an account with NO passkey
// is told so (available:false) and mints none; an account with one mints
// exactly one per options call.
func TestThePasskeyConfirmSpendsNoChallengeItCannotUse(t *testing.T) {
	r := newStepUpRig(t)
	r.fake.EnablePasskeys()
	challenges := func() int { return r.fake.CountCalls(http.MethodPost, "/passkeys/authentication/options") }
	at, rt, _ := r.fake.Mint(r.owner)
	r.stale()

	page := do(r.edge, http.MethodGet, ui.MirrorTerminalPath("ws", "main"), nil, withCookies(at, rt), asNavigation)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `data-passkey="/auth/confirm/passkey"`) {
		t.Fatalf("the terminal page (%d) does not name the passkey confirm route", page.Code)
	}
	if n := challenges(); n != 0 {
		t.Fatalf("rendering the page minted %d passkey challenge(s)", n)
	}

	o := postJSON(r.edge, pkConfirmOptions, map[string]any{}, withCookies(at, rt))
	if o.Code != http.StatusOK || jsonBody(t, o)["available"] != false || jsonBody(t, o)["challenge_id"] != nil {
		t.Fatalf("options for an account with no passkey: %d %s, want available:false and no challenge", o.Code, o.Body)
	}
	if n := challenges(); n != 0 {
		t.Fatalf("an account with no passkey minted %d challenge(s)", n)
	}

	r.fake.AddPasskey(r.owner, "owner-cred")
	o = postJSON(r.edge, pkConfirmOptions, map[string]any{}, withCookies(at, rt))
	if o.Code != http.StatusOK || jsonBody(t, o)["available"] != true {
		t.Fatalf("control: options for an account with a passkey: %d %s", o.Code, o.Body)
	}
	if n := challenges(); n != 1 {
		t.Fatalf("control: one options call minted %d challenges, want 1", n)
	}
}

// TestThePasskeyConfirmIsSameOriginAndNeedsASession, and answers 404 with
// passkeys off in GoTrue — on both of its routes.
func TestThePasskeyConfirmIsSameOriginAndNeedsASession(t *testing.T) {
	r := newPasskeyStepUpRig(t)
	at, rt, _ := r.fake.Mint(r.owner)
	asked := func() int {
		return r.fake.CountCalls(http.MethodPost, "/passkeys/authentication/options") + r.fake.CountCalls(http.MethodPost, "/passkeys/authentication/verify")
	}
	before := asked()
	body := map[string]any{"challenge_id": r.fake.NewChallenge(""), "credential": gotruetest.Assertion("owner-cred", gotruetest.FlagUPUV)}
	for _, path := range []string{pkConfirmOptions, pkConfirmVerify} {
		if w := postJSON(r.edge, path, body, withCookies(at, rt), fromOrigin("https://evil.example")); w.Code != http.StatusForbidden || errorOf(w) != CrossOriginRefusal {
			t.Fatalf("%s cross-site: %d %s", path, w.Code, w.Body)
		}
		if w := postJSON(r.edge, path, body, asXHR); w.Code != http.StatusUnauthorized {
			t.Fatalf("%s with no session: %d %s", path, w.Code, w.Body)
		}
	}
	if asked() != before {
		t.Fatal("a refused passkey confirm still asked GoTrue")
	}
	// Control: the same body, same-origin, with the session, confirms.
	if w := postJSON(r.edge, pkConfirmVerify, body, withCookies(at, rt)); w.Code != http.StatusOK {
		t.Fatalf("control: same-origin verify: %d %s", w.Code, w.Body)
	}

	off := newStepUpRig(t) // passkeys never enabled in this GoTrue
	oat, ort, _ := off.fake.Mint(off.owner)
	for _, path := range []string{pkConfirmOptions, pkConfirmVerify} {
		if w := postJSON(off.edge, path, body, withCookies(oat, ort)); w.Code != http.StatusNotFound {
			t.Fatalf("%s with passkeys off: %d %s, want 404", path, w.Code, w.Body)
		}
	}
	page := do(off.edge, http.MethodGet, ui.MirrorTerminalPath("ws", "main"), nil, withCookies(oat, ort), asNavigation)
	if !strings.Contains(page.Body.String(), `id="term-confirm-password"`) {
		t.Fatal("control: with passkeys off the dialog lost its password field")
	}
}

// TestThePasskeyConfirmIsThrottledUnderTheAccount: it shares the sign-in
// limiter with the password confirm and /login, keyed on the session's ACCOUNT.
// After the free failures (each a UV=0 assertion, from a different address so
// the address side is not what trips) the next attempt — even a good passkey —
// is 429 without asking GoTrue, and the password confirm for the same account
// is held by the same record.
func TestThePasskeyConfirmIsThrottledUnderTheAccount(t *testing.T) {
	r := newPasskeyStepUpRig(t)
	at, rt, _ := r.fake.Mint(r.owner)
	for i := 0; i <= accountFreeFailures; i++ {
		if w := r.passkeyConfirm(t, at, rt, "owner-cred", gotruetest.FlagUP, fromIP("198.51.100."+strconv.Itoa(i+1))); w.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d: %d %s", i, w.Code, w.Body)
		}
	}
	verifies := r.fake.CountCalls(http.MethodPost, "/passkeys/authentication/verify")
	w := r.passkeyConfirm(t, at, rt, "owner-cred", gotruetest.FlagUPUV, fromIP("198.51.100.99"))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("past the free failures: %d %s, want 429", w.Code, w.Body)
	}
	if r.fake.CountCalls(http.MethodPost, "/passkeys/authentication/verify") != verifies {
		t.Fatal("a throttled passkey confirm still asked GoTrue to verify")
	}
	if pw := r.confirm(at, rt, url.Values{"password": {stepUpOwnerPW}}, fromIP("198.51.100.98")); pw.Code != http.StatusTooManyRequests {
		t.Fatalf("the password confirm for the same account: %d, want 429 (one limiter)", pw.Code)
	}
	// Control: once the backoff has passed, the passkey confirms.
	r.fake.Advance(accountBackoffMax)
	if w := r.passkeyConfirm(t, at, rt, "owner-cred", gotruetest.FlagUPUV, fromIP("198.51.100.97")); w.Code != http.StatusOK {
		t.Fatalf("control: after the backoff: %d %s", w.Code, w.Body)
	}
}

// TestTheGateStaysOpenForTwelveHoursAndNoLonger: the window, at the gate, on an
// INJECTED clock (the fake GoTrue's, which muster's verifier and step-up clock
// share — nothing sleeps). Measured at four points: six minutes (stale under
// the old five-minute window), six hours and one minute short of twelve are
// FRESH — rw attaches, a send is pasted; one minute past twelve is STALE — both
// refused — while the session itself still works.
func TestTheGateStaysOpenForTwelveHoursAndNoLonger(t *testing.T) {
	r := newStepUpRig(t)
	r.mirror.v2 = &fakeMirrorV2{}
	at, rt, _ := r.fake.Mint(r.owner)
	sent := 0
	for _, since := range []time.Duration{6 * time.Minute, 6 * time.Hour, twelveHours - time.Minute} {
		r.fake.Advance(since - r.sinceSignIn(t, at))
		if got := r.mustAttach(t, at, rt, "rw"); got != "rw" {
			t.Fatalf("%v after sign-in: attached %q, want rw", since, got)
		}
		w := r.send(at, rt, "at "+since.String())
		sent++
		if _, pastes := r.mirror.v2.snapshot(); w.Code != http.StatusNoContent || len(pastes) != sent {
			t.Fatalf("%v after sign-in: send %d %s, %d pasted (want %d)", since, w.Code, w.Body, len(pastes), sent)
		}
	}
	r.fake.Advance(2 * time.Minute) // twelve hours and one minute
	r.mustRefuse(t, at, rt, "rw", http.StatusForbidden, "12h01m after sign-in")
	w := r.send(at, rt, "too late")
	if _, pastes := r.mirror.v2.snapshot(); w.Code != http.StatusForbidden || w.Header().Get("X-Muster-Step-Up") != "required" || len(pastes) != sent {
		t.Fatalf("12h01m after sign-in: send %d, %d pasted; want 403 + X-Muster-Step-Up and nothing more pasted", w.Code, len(pastes))
	}
	if !strings.Contains(w.Body.String(), "from the last 12 hours") {
		t.Fatalf("the refusal does not name the window: %q", w.Body)
	}
	if got := r.mustAttach(t, at, rt, ""); got != "ro" {
		t.Fatalf("control: the stale session still watches: %q", got)
	}
}

// sinceSignIn is how long ago (on the fake's clock) at's session signed in.
func (r *stepUpRig) sinceSignIn(t *testing.T, at string) time.Duration {
	t.Helper()
	parts := strings.Split(at, ".")
	var c struct {
		AMR []struct {
			Timestamp int64 `json:"timestamp"`
		} `json:"amr"`
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || json.Unmarshal(raw, &c) != nil || len(c.AMR) == 0 {
		t.Fatalf("no amr in the access token: %v", err)
	}
	return r.fake.Now().Sub(time.Unix(c.AMR[0].Timestamp, 0))
}

// TestTheDialogNamesTwelveHoursAndOffersBothWays: the page's confirm dialog
// says the window in words (and no longer "5 minutes"), keeps the password
// field, and carries the passkey button — hidden until the page has asked
// whether the account has one.
func TestTheDialogNamesTwelveHoursAndOffersBothWays(t *testing.T) {
	r := newPasskeyStepUpRig(t)
	at, rt, _ := r.fake.Mint(r.owner)
	for name, path := range map[string]string{"terminal": ui.MirrorTerminalPath("ws", "main")} {
		page := do(r.edge, http.MethodGet, path, nil, withCookies(at, rt), asNavigation).Body.String()
		i := strings.Index(page, `<dialog id="term-confirm"`)
		if i < 0 {
			t.Fatalf("%s: no confirm dialog", name)
		}
		dlg := page[i : i+strings.Index(page[i:], "</dialog>")]
		if !strings.Contains(dlg, "Typing into this session needs a sign-in from the last 12 hours.") {
			t.Errorf("%s: the dialog does not name the 12 hour window", name)
		}
		if strings.Contains(page, "5 minutes") {
			t.Errorf("%s: the page still says \"5 minutes\"", name)
		}
		for _, want := range []string{`id="term-confirm-password"`, `id="term-confirm-passkey"`, `>Confirm with passkey<`, `data-confirm="/auth/confirm"`} {
			if !strings.Contains(dlg, want) {
				t.Errorf("%s: the dialog lacks %s", name, want)
			}
		}
		box := dlg[strings.Index(dlg, `id="term-confirm-passkey-box"`):]
		if tag := box[:strings.Index(box, ">")]; !strings.Contains(tag, " hidden") {
			t.Errorf("%s: the passkey button is not drawn hidden: %s", name, tag)
		}
	}
}
