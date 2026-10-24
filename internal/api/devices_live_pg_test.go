package api

import (
	"context"
	"html"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
)

// deviceListRe finds Account → Devices' live list and its hx-trigger.
var deviceListRe = regexp.MustCompile(`<div id="device-list"[^>]*hx-get="/account/devices/live"[^>]*hx-trigger="([^"]*)"`)

// pushListOOBRe finds the Notifications section's device list carried out of
// band, with its test button.
var pushListOOBRe = regexp.MustCompile(`(?s)<div id="push-device-list"[^>]*hx-swap-oob="true"[^>]*>.*Send a test notification`)

// TestTheDeviceListNoticesAPhonesPushRegistration: a phone registers its push
// endpoint seconds AFTER it pairs, while the person is looking at Account →
// Devices. The list re-fetches itself (GET /account/devices/live) every 3s
// while something is pending — an unused pairing code, or a freshly paired
// phone with no push — and its response shows the registration and, out of
// band, the Notifications section's "Send a test notification". With nothing
// pending it does not poll (only a visibilitychange refetch remains).
//
// The paths, the interval and the markup are LITERALS, not this package's
// constants, so the test compiles on the code before the fix and fails there
// on its assertions.
func TestTheDeviceListNoticesAPhonesPushRegistration(t *testing.T) {
	r := newDeviceRig(t)
	at, rt := r.login()
	trigger := func(body string) string {
		t.Helper()
		m := deviceListRe.FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("no live device list in:\n%s", truncate(body, 600))
		}
		return html.UnescapeString(m[1])
	}
	const poll = "every 3s"
	account := func() string {
		t.Helper()
		w := do(r.edge, "GET", "/account", nil, withCookies(at, rt), asNavigation)
		if w.Code != http.StatusOK {
			t.Fatalf("/account: %d", w.Code)
		}
		return w.Body.String()
	}
	live := func() string {
		t.Helper()
		w := do(r.edge, "GET", "/account/devices/live", nil, withCookies(at, rt), asXHR)
		if w.Code != http.StatusOK {
			t.Fatalf("/account/devices/live: %d %s", w.Code, truncate(w.Body.String(), 300))
		}
		return w.Body.String()
	}
	sys := authz.System(context.Background())

	// Nothing pending: no polling, but a refetch when the tab comes back.
	if tr := trigger(account()); strings.Contains(tr, "every") || !strings.Contains(tr, "visibilitychange") {
		t.Fatalf("with nothing pending the trigger is %q; want visibilitychange and no polling", tr)
	}

	// A pairing code handed to the app and not yet redeemed: polling.
	if w := do(r.edge, "POST", "/account/devices/pair", nil, withCookies(at, rt)); w.Code != http.StatusOK {
		t.Fatalf("pairing code: %d %s", w.Code, w.Body.String())
	}
	if tr := trigger(live()); !strings.Contains(tr, poll) {
		t.Fatalf("with a pairing code pending the trigger is %q; want %q", tr, poll)
	}
	if _, err := r.pool.Exec(sys, `UPDATE device_pairing_codes SET expires_at = now() - interval '1 second' WHERE owner_id = $1`, r.uid); err != nil {
		t.Fatal(err)
	}
	if tr := trigger(live()); strings.Contains(tr, "every") {
		t.Fatalf("CONTROL: with the code expired the trigger is %q; want no polling", tr)
	}

	// Paired, push not registered yet: the page polls and shows no test button.
	tok, id := r.pair(at, rt, "Pixel pending push")
	page := account()
	if tr := trigger(page); !strings.Contains(tr, poll) {
		t.Fatalf("with a phone waiting for push the trigger is %q; want %q", tr, poll)
	}
	if !strings.Contains(page, "push not registered") || strings.Contains(page, "Send a test notification") {
		t.Fatalf("before the registration the page shows:\n%s", truncate(page, 600))
	}

	// The app registers push. The list's own refetch — no reload — shows it,
	// carries the test button out of band, and stops polling.
	reg := `{"endpoint":"https://` + devNtfyHost + `/upLive1?up=1","p256dh":"BPk","auth":"au"}`
	if w := r.call(r.edge, "PUT", devicePushPath, tok, reg); w.Code != http.StatusOK {
		t.Fatalf("push register: %d %s", w.Code, w.Body.String())
	}
	frag := live()
	if !strings.Contains(frag, `data-device-id="`+strconv.FormatInt(id, 10)+`"`) || !strings.Contains(frag, "push registered") ||
		strings.Contains(frag, "push not registered") {
		t.Fatalf("after the registration the list shows:\n%s", truncate(frag, 600))
	}
	if !pushListOOBRe.MatchString(frag) {
		t.Fatalf("the refetch carries no out-of-band Notifications list with the test button:\n%s", frag)
	}
	if tr := trigger(frag); strings.Contains(tr, "every") {
		t.Fatalf("with every phone registered the trigger is %q; want no polling", tr)
	}

	// A phone that paired long ago and never registered is not waited on.
	_, old := r.pair(at, rt, "Pixel without push")
	if tr := trigger(live()); !strings.Contains(tr, poll) {
		t.Fatalf("CONTROL: a fresh unregistered phone: trigger %q; want %q", tr, poll)
	}
	if _, err := r.pool.Exec(sys, `UPDATE devices SET created_at = now() - interval '11 minutes' WHERE id = $1`, old); err != nil {
		t.Fatal(err)
	}
	if tr := trigger(live()); strings.Contains(tr, "every") {
		t.Fatalf("a phone paired 11 minutes ago with no push still polls: %q", tr)
	}
}
