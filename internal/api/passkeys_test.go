package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/gotrue"
	"github.com/ZacxDev/muster/internal/gotrue/gotruetest"
)

// postJSON POSTs body as JSON, same-origin unless an option says otherwise.
func postJSON(h http.Handler, target string, body any, opts ...reqOpt) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "http://"+r.Host)
	for _, o := range opts {
		o(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func fromOrigin(o string) reqOpt { return func(r *http.Request) { r.Header.Set("Origin", o) } }

func jsonBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("not JSON (%d): %s", rec.Code, truncate(rec.Body.String(), 300))
	}
	return m
}

// passkeyWorld is the invite world with passkeys on in its GoTrue.
func newPasskeyWorld(t *testing.T) *invWorld {
	w := newInviteWorld(t)
	w.fake.EnablePasskeys()
	return w
}

// passkeySignIn runs the sign-in ceremony through muster: options, then
// verify with an assertion for credID carrying flags.
func (w *invWorld) passkeySignIn(credID string, flags byte, opts ...reqOpt) *httptest.ResponseRecorder {
	w.t.Helper()
	o := postJSON(w.edge, passkeySignInOptionsPath, map[string]any{}, opts...)
	if o.Code != http.StatusOK {
		w.t.Fatalf("sign-in options: %d %s", o.Code, o.Body)
	}
	ch, _ := jsonBody(w.t, o)["challenge_id"].(string)
	return postJSON(w.edge, passkeySignInVerifyPath, map[string]any{
		"challenge_id": ch, "credential": gotruetest.Assertion(credID, flags), "next": "/tasks"}, opts...)
}

// addPasskey runs the registration ceremony through muster as the session
// in cookies, and returns the verify answer.
func (w *invWorld) addPasskey(credID string, flags byte, cookies reqOpt) *httptest.ResponseRecorder {
	w.t.Helper()
	o := postJSON(w.edge, accountPasskeysPath+"/options", map[string]any{}, cookies)
	if o.Code != http.StatusOK {
		return o
	}
	ch, _ := jsonBody(w.t, o)["challenge_id"].(string)
	return postJSON(w.edge, accountPasskeysPath+"/verify", map[string]any{
		"challenge_id": ch, "credential": gotruetest.Attestation(credID, flags)}, cookies)
}

// TestAPasskeySignInLandsWithBothCookies is the positive path: the options
// muster hands the browser ask for user verification, a verified assertion
// signs the member in with the same two cookies a password does, and that
// session works.
func TestAPasskeySignInLandsWithBothCookies(t *testing.T) {
	w := newPasskeyWorld(t)
	_, sub, _, _ := w.joinMember("pk-in@invite.test")
	w.fake.AddPasskey(sub, "cred-in")
	o := postJSON(w.edge, passkeySignInOptionsPath, map[string]any{})
	opts, _ := jsonBody(t, o)["options"].(map[string]any)
	if o.Code != 200 || opts["userVerification"] != "required" {
		t.Fatalf("sign-in options %d: userVerification=%v, want required", o.Code, opts["userVerification"])
	}
	rec := w.passkeySignIn("cred-in", gotruetest.FlagUPUV)
	if rec.Code != http.StatusOK || jsonBody(t, rec)["redirect"] != "/tasks" {
		t.Fatalf("passkey sign-in: %d %s", rec.Code, rec.Body)
	}
	ck := setCookies(rec)
	if ck[accessCookieName] == nil || ck[refreshCookieName] == nil || !ck[accessCookieName].HttpOnly || !ck[accessCookieName].Secure {
		t.Fatalf("cookies: %+v", ck)
	}
	if r := do(w.edge, "GET", "/account", nil, withCookies(ck[accessCookieName].Value, ck[refreshCookieName].Value)); r.Code != 200 ||
		!strings.Contains(r.Body.String(), "pk-in@invite.test") {
		t.Fatalf("the passkey session: %d", r.Code)
	}
	c, err := w.fake.Verifier().Verify(ck[accessCookieName].Value)
	if err != nil || len(c.AMR) != 1 || c.AMR[0].Method != "passkey" {
		t.Fatalf("the session's amr: %+v %v", c.AMR, err)
	}
}

// TestAPasskeyWithoutUserVerificationIsRefused: an assertion (or a
// registration) whose authenticator did not verify the person is refused BY
// MUSTER and never reaches GoTrue, which would accept it (its UV is
// "preferred"). The same request with UV set succeeds — the control that
// the refusal is about the flag and nothing else in the fixture.
func TestAPasskeyWithoutUserVerificationIsRefused(t *testing.T) {
	w := newPasskeyWorld(t)
	_, sub, at, rt := w.joinMember("pk-uv@invite.test")
	w.fake.AddPasskey(sub, "cred-uv")
	verifies := w.fake.CountCalls("POST", "/passkeys/authentication/verify")
	rec := w.passkeySignIn("cred-uv", gotruetest.FlagUP)
	if rec.Code != http.StatusUnauthorized || jsonBody(t, rec)["error"] != PasskeyNoUVRefusal {
		t.Fatalf("an assertion without UV: %d %s", rec.Code, rec.Body)
	}
	if len(setCookies(rec)) != 0 {
		t.Fatal("an assertion without UV set cookies")
	}
	if n := w.fake.CountCalls("POST", "/passkeys/authentication/verify") - verifies; n != 0 {
		t.Fatalf("an assertion without UV was forwarded to GoTrue %d time(s)", n)
	}
	if rec := w.passkeySignIn("cred-uv", gotruetest.FlagUPUV); rec.Code != http.StatusOK {
		t.Fatalf("CONTROL: the same assertion WITH UV: %d %s", rec.Code, rec.Body)
	}

	cookies := withCookies(at, rt)
	regs := w.fake.CountCalls("POST", "/passkeys/registration/verify")
	if rec := w.addPasskey("new-no-uv", gotruetest.FlagUP, cookies); rec.Code != http.StatusBadRequest || jsonBody(t, rec)["error"] != PasskeyNoUVRefusal {
		t.Fatalf("a registration without UV: %d %s", rec.Code, rec.Body)
	}
	if n := w.fake.CountCalls("POST", "/passkeys/registration/verify") - regs; n != 0 || len(w.fake.Passkeys(sub)) != 1 {
		t.Fatalf("a registration without UV reached GoTrue (%d calls, %d passkeys)", n, len(w.fake.Passkeys(sub)))
	}
	if rec := w.addPasskey("new-uv", gotruetest.FlagUPUV, cookies); rec.Code != http.StatusOK || len(w.fake.Passkeys(sub)) != 2 {
		t.Fatalf("CONTROL: the same registration WITH UV: %d %s", rec.Code, rec.Body)
	}
}

// TestADisabledUsersPasskeyIsRefused, at both layers: a disable (GoTrue ban)
// makes GoTrue refuse the assertion; with the ban absent (a disable whose ban
// did not apply) muster's own admission check refuses the session GoTrue
// issued. Neither sets a cookie.
func TestADisabledUsersPasskeyIsRefused(t *testing.T) {
	w := newPasskeyWorld(t)
	uid, sub, _, _ := w.joinMember("pk-dis@invite.test")
	w.fake.AddPasskey(sub, "cred-dis")
	if rec := w.passkeySignIn("cred-dis", gotruetest.FlagUPUV); rec.Code != http.StatusOK {
		t.Fatalf("CONTROL: before the disable: %d %s", rec.Code, rec.Body)
	}
	id := strconv.FormatInt(uid, 10)
	if r := do(w.edge, "POST", "/admin/users/"+id+"/disable", url.Values{}, w.asAdmin()); r.Code != http.StatusSeeOther {
		t.Fatalf("disable: %d %s", r.Code, truncate(r.Body.String(), 300))
	}
	rec := w.passkeySignIn("cred-dis", gotruetest.FlagUPUV)
	if rec.Code != http.StatusUnauthorized || len(setCookies(rec)) != 0 {
		t.Fatalf("a disabled (banned) user's passkey: %d %s", rec.Code, rec.Body)
	}
	// The ban lifted in GoTrue only: muster's disabled_at alone must refuse.
	if err := w.fake.Client().AdminSetBanned(context.Background(), "127.0.0.1", sub, false); err != nil {
		t.Fatal(err)
	}
	w.s.gt.forgetUser(uid)
	rec = w.passkeySignIn("cred-dis", gotruetest.FlagUPUV)
	if rec.Code != http.StatusForbidden || jsonBody(t, rec)["error"] != "This account is disabled." || len(setCookies(rec)) != 0 {
		t.Fatalf("a disabled-in-muster user's passkey: %d %s", rec.Code, rec.Body)
	}
}

// TestAPasskeyForALoginWithNoMusterAccountIsRefused: a GoTrue user muster has
// no row for gets no cookies, and the session GoTrue issued is ended there.
func TestAPasskeyForALoginWithNoMusterAccountIsRefused(t *testing.T) {
	w := newPasskeyWorld(t)
	stray := w.fake.AddUser("stray@invite.test", "stray-password-long")
	w.fake.AddPasskey(stray, "cred-stray")
	logouts := w.fake.CountCalls("POST", "/logout")
	rec := w.passkeySignIn("cred-stray", gotruetest.FlagUPUV)
	if rec.Code != http.StatusForbidden || jsonBody(t, rec)["error"] != "This login has no muster account." || len(setCookies(rec)) != 0 {
		t.Fatalf("a passkey of a login with no muster row: %d %s", rec.Code, rec.Body)
	}
	if w.fake.CountCalls("POST", "/logout")-logouts != 1 {
		t.Fatal("the session GoTrue issued for a login muster refused was not ended")
	}
}

// TestPasskeyRoutesRefuseCrossOriginWrites: every passkey POST, sign-in and
// account alike, refuses a cross-site Origin — and the same request
// same-origin is not refused for that reason (the control).
func TestPasskeyRoutesRefuseCrossOriginWrites(t *testing.T) {
	w := newPasskeyWorld(t)
	_, sub, at, rt := w.joinMember("pk-xo@invite.test")
	pid := w.fake.AddPasskey(sub, "cred-xo")
	cookies := withCookies(at, rt)
	for _, path := range []string{passkeySignInOptionsPath, passkeySignInVerifyPath, accountPasskeysPath + "/options",
		accountPasskeysPath + "/verify", accountPasskeysPath + "/" + pid + "/delete"} {
		rec := postJSON(w.edge, path, map[string]any{}, cookies, fromOrigin("https://evil.example"))
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), CrossOriginRefusal) {
			t.Errorf("cross-origin %s: %d %s", path, rec.Code, truncate(rec.Body.String(), 200))
		}
		if rec := postJSON(w.edge, path, map[string]any{}, cookies); strings.Contains(rec.Body.String(), CrossOriginRefusal) {
			t.Errorf("CONTROL: same-origin %s was refused as cross-origin", path)
		}
	}
	// The cross-origin delete ran BEFORE the same-origin one, and only the
	// latter may have removed the passkey.
	if len(w.fake.Passkeys(sub)) != 0 {
		t.Error("CONTROL: the same-origin delete did not remove the passkey")
	}
}

// TestPasskeyRoutesAreBrowserListenerOnly: on the machine-token listener every
// passkey route is a 403, whatever credential comes with it.
func TestPasskeyRoutesAreBrowserListenerOnly(t *testing.T) {
	w := newPasskeyWorld(t)
	_, _, at, rt := w.joinMember("pk-ml@invite.test")
	machine := w.s.Handler() // NOT wrapped in BrowserListener
	for _, path := range []string{passkeySignInOptionsPath, passkeySignInVerifyPath, accountPasskeysPath + "/options",
		accountPasskeysPath + "/verify", accountPasskeysPath + "/x/delete"} {
		if rec := postJSON(machine, path, map[string]any{}, withCookies(at, rt)); rec.Code != http.StatusForbidden {
			t.Errorf("machine listener %s: %d %s", path, rec.Code, truncate(rec.Body.String(), 200))
		}
	}
	if rec := do(machine, "GET", accountPasskeysPath, nil, withCookies(at, rt)); rec.Code != http.StatusForbidden {
		t.Errorf("machine listener GET %s: %d", accountPasskeysPath, rec.Code)
	}
	if rec := postJSON(w.edge, passkeySignInOptionsPath, map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("CONTROL: the browser listener serves sign-in options: %d", rec.Code)
	}
}

// TestSignOutEverywhereEndsAPasskeySession: a passkey session is an ordinary
// GoTrue session to muster — sign-out-everywhere (and the per-user cutoff
// behind it) ends it.
func TestSignOutEverywhereEndsAPasskeySession(t *testing.T) {
	w := newPasskeyWorld(t)
	_, sub, _, _ := w.joinMember("pk-out@invite.test")
	w.fake.AddPasskey(sub, "cred-out")
	ck := setCookies(w.passkeySignIn("cred-out", gotruetest.FlagUPUV))
	held := withCookies(ck[accessCookieName].Value, ck[refreshCookieName].Value)
	if r := do(w.edge, "GET", "/account", nil, held, asXHR); r.Code != 200 {
		t.Fatalf("CONTROL: the passkey session before sign-out: %d", r.Code)
	}
	if r := do(w.edge, "POST", "/logout", url.Values{"scope": {"global"}}, held); r.Code != http.StatusSeeOther {
		t.Fatalf("sign out everywhere: %d", r.Code)
	}
	if r := do(w.edge, "GET", "/account", nil, held, asXHR); r.Code != http.StatusUnauthorized {
		t.Fatalf("the passkey session after sign-out-everywhere: %d, want 401", r.Code)
	}
	c, _ := w.fake.Verifier().Verify(ck[accessCookieName].Value)
	if !w.fake.SessionRevoked(c.SessionID) {
		t.Fatal("GoTrue still holds the passkey session")
	}
}

// TestAStaleSessionCannotAddAPasskey: adding a passkey needs a sign-in within
// stepup.MaxAge (the session's amr timestamp). Measured at two points: 4
// minutes after sign-in it is allowed, 6 minutes after it is refused with
// reauth_required — and GoTrue's registration API is not called. The
// password, asked again, mints a fresh session that may add one.
func TestAStaleSessionCannotAddAPasskey(t *testing.T) {
	w := newPasskeyWorld(t)
	_, sub, at, rt := w.joinMember("pk-stale@invite.test")
	cookies := withCookies(at, rt)
	w.fake.Advance(4 * time.Minute)
	if rec := postJSON(w.edge, accountPasskeysPath+"/options", map[string]any{}, cookies); rec.Code != http.StatusOK {
		t.Fatalf("4 minutes after sign-in: %d %s", rec.Code, rec.Body)
	}
	w.fake.Advance(2 * time.Minute) // 6 minutes
	regCalls := func() int {
		return w.fake.CountCalls("POST", "/passkeys/registration/options") + w.fake.CountCalls("POST", "/passkeys/registration/verify")
	}
	before := regCalls()
	if rec := postJSON(w.edge, accountPasskeysPath+"/options", map[string]any{}, cookies); rec.Code != http.StatusUnauthorized ||
		jsonBody(t, rec)["error"] != PasskeyReauthRequired {
		t.Fatalf("6 minutes after sign-in, options: %d %s", rec.Code, rec.Body)
	}
	rec := postJSON(w.edge, accountPasskeysPath+"/verify", map[string]any{
		"challenge_id": w.fake.NewChallenge(sub), "credential": gotruetest.Attestation("stale-cred", gotruetest.FlagUPUV)}, cookies)
	if rec.Code != http.StatusUnauthorized || jsonBody(t, rec)["error"] != PasskeyReauthRequired {
		t.Fatalf("6 minutes after sign-in, verify: %d %s", rec.Code, rec.Body)
	}
	if regCalls() != before || len(w.fake.Passkeys(sub)) != 0 {
		t.Fatal("a stale session reached GoTrue's registration API")
	}
	// The re-ask is the shared step-up confirm (POST /auth/confirm).
	if rec := do(w.edge, "POST", stepUpConfirmPath, url.Values{"password": {"not-the-password-x"}}, cookies, fromIP("198.51.100.40")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("confirm with a wrong password: %d %s", rec.Code, rec.Body)
	}
	rec = do(w.edge, "POST", stepUpConfirmPath, url.Values{"password": {goodPW}}, cookies, fromIP("198.51.100.40"))
	ck := setCookies(rec)
	if rec.Code != http.StatusOK || ck[accessCookieName] == nil {
		t.Fatalf("reauth with the password: %d %s", rec.Code, rec.Body)
	}
	fresh := withCookies(ck[accessCookieName].Value, ck[refreshCookieName].Value)
	if rec := w.addPasskey("after-reauth", gotruetest.FlagUPUV, fresh); rec.Code != http.StatusOK || len(w.fake.Passkeys(sub)) != 1 {
		t.Fatalf("adding a passkey after reauth: %d %s", rec.Code, rec.Body)
	}
}

// TestAResetDeletesEveryPasskey: a password reset removes every passkey the
// user has, so a passkey on a lost device cannot sign the thief back in.
func TestAResetDeletesEveryPasskey(t *testing.T) {
	w := newPasskeyWorld(t)
	_, sub, _, _ := w.joinMember("pk-reset@invite.test")
	w.fake.AddPasskey(sub, "lost-phone")
	w.fake.AddPasskey(sub, "old-laptop")
	if rec := w.passkeySignIn("lost-phone", gotruetest.FlagUPUV); rec.Code != http.StatusOK {
		t.Fatalf("CONTROL: the passkey works before the reset: %d", rec.Code)
	}
	r := do(w.edge, "POST", ResetPath, url.Values{"token": {w.fake.RecoveryHash(sub)}, "password": {"a-new-long-password"}, "password2": {"a-new-long-password"}})
	if r.Code != http.StatusSeeOther {
		t.Fatalf("reset: %d %s", r.Code, truncate(r.Body.String(), 300))
	}
	if n := len(w.fake.Passkeys(sub)); n != 0 {
		t.Fatalf("after the reset the user still has %d passkeys", n)
	}
	if rec := w.passkeySignIn("lost-phone", gotruetest.FlagUPUV); rec.Code != http.StatusUnauthorized || len(setCookies(rec)) != 0 {
		t.Fatalf("the pre-reset passkey after the reset: %d %s", rec.Code, rec.Body)
	}
}

// TestAResetFailsClosedWhenPasskeysCannotBeRemoved: if GoTrue fails while the
// reset is removing passkeys, the password is NOT set (the old one still signs
// in) and the passkeys that were not reached are still there to be removed by
// the next link — never a reset that reports success with a passkey left.
func TestAResetFailsClosedWhenPasskeysCannotBeRemoved(t *testing.T) {
	w := newPasskeyWorld(t)
	_, sub, _, _ := w.joinMember("pk-resetfail@invite.test")
	w.fake.AddPasskey(sub, "kept-1")
	w.fake.AddPasskey(sub, "kept-2")
	w.fake.FailAdminPasskeys(1) // the list call fails
	r := do(w.edge, "POST", ResetPath, url.Values{"token": {w.fake.RecoveryHash(sub)}, "password": {"a-new-long-password"}, "password2": {"a-new-long-password"}})
	if r.Code != http.StatusBadGateway || !strings.Contains(r.Body.String(), "The password could not be set") {
		t.Fatalf("reset with GoTrue failing: %d %s", r.Code, truncate(r.Body.String(), 300))
	}
	if gu, _ := w.fake.User(sub); gu.Password != goodPW {
		t.Fatal("the password was set although passkeys could not be removed")
	}
	if n := len(w.fake.Passkeys(sub)); n != 2 {
		t.Fatalf("CONTROL: the failure removed %d passkeys before it", 2-n)
	}
}

// TestThePasskeyListShowsTheSyncedFlag: the backup-state flag muster read at
// registration is shown, synced and device-bound alike, and removing a
// passkey forgets its record.
func TestThePasskeyListShowsTheSyncedFlag(t *testing.T) {
	w := newPasskeyWorld(t)
	_, sub, at, rt := w.joinMember("pk-list@invite.test")
	cookies := withCookies(at, rt)
	rec := w.addPasskey("synced-one", gotruetest.FlagUPUVBEBS, cookies)
	if rec.Code != 200 {
		t.Fatalf("add synced: %d %s", rec.Code, rec.Body)
	}
	syncedID, _ := jsonBody(t, rec)["id"].(string)
	rec = w.addPasskey("device-one", gotruetest.FlagUPUV, cookies)
	if rec.Code != 200 {
		t.Fatalf("add device-bound: %d %s", rec.Code, rec.Body)
	}
	deviceID, _ := jsonBody(t, rec)["id"].(string)
	page := do(w.edge, "GET", accountPasskeysPath, nil, cookies).Body.String()
	// Each flag is read off ITS passkey's row: a count of one of each would
	// pass with the two swapped.
	row := func(id string) string {
		i := strings.Index(page, `data-passkey-id="`+id+`"`)
		if i < 0 {
			return ""
		}
		j := strings.Index(page[i:], "</li>")
		return page[i : i+j]
	}
	if !strings.Contains(row(syncedID), `data-passkey-synced="true"`) || !strings.Contains(row(deviceID), `data-passkey-synced="false"`) ||
		strings.Count(page, "data-passkey-id=") != 2 || !strings.Contains(page, "never used") {
		t.Fatalf("the list: %s", truncate(page, 2000))
	}
	if r := do(w.edge, "POST", accountPasskeysPath+"/"+deviceID+"/delete", url.Values{}, cookies); r.Code != http.StatusSeeOther {
		t.Fatalf("remove: %d", r.Code)
	}
	gu, _ := w.fake.User(sub)
	recorded, _ := gu.AppMetadata[passkeyMetaKey].(map[string]any)
	if _, still := recorded[deviceID]; still || len(recorded) != 1 || len(w.fake.Passkeys(sub)) != 1 {
		t.Fatalf("after removing one: record %v, %d passkeys", recorded, len(w.fake.Passkeys(sub)))
	}
}

// TestTheAdminCanRemoveAUsersPasskeys: the admin page's button removes them
// all, audited.
func TestTheAdminCanRemoveAUsersPasskeys(t *testing.T) {
	w := newPasskeyWorld(t)
	uid, sub, _, _ := w.joinMember("pk-admin@invite.test")
	w.fake.AddPasskey(sub, "a1")
	w.fake.AddPasskey(sub, "a2")
	id := strconv.FormatInt(uid, 10)
	if page := do(w.edge, "GET", "/admin/users", nil, w.asAdmin()).Body.String(); !strings.Contains(page, `/admin/users/`+id+`/passkeys/delete`) {
		t.Fatal("no Remove passkeys button for the member")
	}
	rec := do(w.edge, "POST", "/admin/users/"+id+"/passkeys/delete", url.Values{}, w.asAdmin())
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Removed 2 passkeys of pk-admin@invite.test") {
		t.Fatalf("remove: %d %s", rec.Code, truncate(rec.Body.String(), 400))
	}
	if len(w.fake.Passkeys(sub)) != 0 || len(w.audit("user.passkeys_removed")) != 1 {
		t.Fatalf("after remove: %d passkeys, audit %+v", len(w.fake.Passkeys(sub)), w.audit("user.passkeys_removed"))
	}
}

// TestAnInviteLandsOnThePasskeyOffer: with passkeys on, a redeemed invite lands
// on /account/passkeys?welcome=1, which offers one; with them off, on the board.
func TestAnInviteLandsOnThePasskeyOffer(t *testing.T) {
	w := newPasskeyWorld(t)
	rec := do(w.edge, "POST", InvitePath, redeemForm(w.createInvite(""), "pk-welcome@invite.test", "", goodPW, goodPW))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != accountPasskeysPath+"?welcome=1" {
		t.Fatalf("invite with passkeys on: %d → %q", rec.Code, rec.Header().Get("Location"))
	}
	ck := setCookies(rec)
	page := do(w.edge, "GET", accountPasskeysPath+"?welcome=1", nil, withCookies(ck[accessCookieName].Value, ck[refreshCookieName].Value)).Body.String()
	if !strings.Contains(page, `id="passkey-welcome"`) || !strings.Contains(page, "Add a passkey on this device") || !strings.Contains(page, `id="passkey-skip"`) {
		t.Fatalf("the welcome page: %s", truncate(page, 1500))
	}
	off := newInviteWorld(t) // passkeys off
	rec = do(off.edge, "POST", InvitePath, redeemForm(off.createInvite(""), "pk-nowelcome@invite.test", "", goodPW, goodPW))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("invite with passkeys off: %d → %q", rec.Code, rec.Header().Get("Location"))
	}
}

// TestPasskeysOffServesNothing: with GOTRUE_PASSKEY_ENABLED off, no passkey
// route does anything and the sign-in page has no passkey button — and with
// it on, the button is there (the control that the check reads the setting).
func TestPasskeysOffServesNothing(t *testing.T) {
	w := newInviteWorld(t)
	_, _, at, rt := w.joinMember("pk-off@invite.test")
	if rec := postJSON(w.edge, passkeySignInOptionsPath, map[string]any{}); rec.Code != http.StatusNotFound {
		t.Errorf("sign-in options with passkeys off: %d", rec.Code)
	}
	if rec := do(w.edge, "GET", accountPasskeysPath, nil, withCookies(at, rt)); rec.Code != http.StatusNotFound {
		t.Errorf("account page with passkeys off: %d", rec.Code)
	}
	if page := do(w.edge, "GET", "/login", nil).Body.String(); strings.Contains(page, "passkey-signin") {
		t.Error("the sign-in page offers passkeys while they are off")
	}
	w.fake.EnablePasskeys()
	w.s.gt.pk = passkeyGate{} // drop the cached "off"
	if page := do(w.edge, "GET", "/login", nil).Body.String(); !strings.Contains(page, `id="passkey-signin"`) {
		t.Error("CONTROL: the sign-in page has no passkey button with passkeys on")
	}
}

// TestPasskeySignInIsThrottledPerAddress: failed passkey sign-ins count
// against the client address (signinLimiter's address side); another address
// is not held.
func TestPasskeySignInIsThrottledPerAddress(t *testing.T) {
	w := newPasskeyWorld(t)
	_, sub, _, _ := w.joinMember("pk-rl@invite.test")
	w.fake.AddPasskey(sub, "cred-rl")
	from := fromIP("203.0.113.66")
	for i := 0; i < loginFailureLimit; i++ {
		if rec := w.passkeySignIn("cred-rl", gotruetest.FlagUP, from); rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d: %d", i+1, rec.Code)
		}
	}
	if rec := w.passkeySignIn("cred-rl", gotruetest.FlagUPUV, from); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after %d failures from one address: %d, want 429", loginFailureLimit, rec.Code)
	}
	if rec := w.passkeySignIn("cred-rl", gotruetest.FlagUPUV, fromIP("203.0.113.67")); rec.Code != http.StatusOK {
		t.Fatalf("CONTROL: another address: %d", rec.Code)
	}
}

// TestTheHandlerForwardsTheCanonicalCredential: what reaches GoTrue is muster's
// re-encoding of the credential it checked — not the browser's bytes. A field
// the browser added does not travel, and the authenticatorData GoTrue gets is
// the one whose flags muster read.
func TestTheHandlerForwardsTheCanonicalCredential(t *testing.T) {
	w := newPasskeyWorld(t)
	_, sub, _, _ := w.joinMember("pk-canon@invite.test")
	w.fake.AddPasskey(sub, "cred-canon")
	o := postJSON(w.edge, passkeySignInOptionsPath, map[string]any{})
	ch, _ := jsonBody(t, o)["challenge_id"].(string)
	var cred map[string]any
	_ = json.Unmarshal(gotruetest.Assertion("cred-canon", gotruetest.FlagUPUV), &cred)
	cred["smuggled"] = "x"
	rec := postJSON(w.edge, passkeySignInVerifyPath, map[string]any{"challenge_id": ch, "credential": cred})
	if rec.Code != http.StatusOK {
		t.Fatalf("sign-in: %d %s", rec.Code, rec.Body)
	}
	fwd := w.fake.LastCredential()
	_, canonical, err := gotrue.CanonicalAssertion(gotruetest.Assertion("cred-canon", gotruetest.FlagUPUV))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fwd, canonical) {
		t.Fatalf("GoTrue got\n  %s\nwant muster's canonical\n  %s", fwd, canonical)
	}
}
