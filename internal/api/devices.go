package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/devices"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/stepup"
)

// PAIRED DEVICES: the native Android app's server side, Phase 1
// (docs/design/android.md, "The device credential" and "Server changes").
//
// THREE DOORS, AND WHICH CREDENTIAL OPENS WHICH IS THE DESIGN:
//
//   - The SESSION tier (a signed-in browser, same-origin writes):
//     POST /account/devices/pair mints a one-time pairing code, and needs a
//     sign-in from the last five minutes (stepup.CredentialMaxAge, the rule for
//     adding a passkey: the credential it leads to outlives the session).
//     POST /account/devices/{id}/remove and the Account → Devices section.
//   - The PUBLIC pairing route, POST /api/device/pair: the app redeems the code
//     for the device credential. The code is the authority, so no credential
//     is asked for; it is throttled per client address.
//   - The DEVICE tier, /api/device/*: a DEVICE credential (mdv_…) through
//     requireDeviceToken and nothing else. It acts as the device's owner
//     (authz.ViaDevice). 🔴 THE SHARED HOOK AND SERVICE TOKENS, A PERSONAL
//     TOKEN, A PLUGIN TOKEN AND A SESSION COOKIE ARE ALL REFUSED HERE, and it
//     is served on the browser listener in gotrue mode (the phone reaches
//     muster through the public edge) without opening that listener to the
//     shared tokens: requireDeviceToken never compares one.
//
// 🔴 AND THE CONVERSE: AN mdv_ CREDENTIAL IS REFUSED ON EVERY OTHER ROUTE.
// admit refuses a request carrying a device-shaped credential unless the
// wrapper that admitted it is the device door (refuseDeviceTokenElsewhere,
// TestADeviceCredentialIsRefusedOutsideTheDeviceTier). A device cannot ask for
// a pairing code — that route is the session tier — so a device credential
// cannot mint another.
//
// 🔴 REVOCATION IS READ ON EVERY REQUEST (devices.PGStore.Authenticate):
// removing the device, disabling the account, the session cutoff, and Sign out
// everywhere (gotrue mode; the design's recommendation) each refuse the
// credential on its next request.

const (
	accountDevicesPairPath = "/account/devices/pair"
	deviceAPIPrefix        = "/api/device"
	devicePairPath         = deviceAPIPrefix + "/pair"
	deviceConfigPath       = deviceAPIPrefix + "/config"
	devicePushPath         = deviceAPIPrefix + "/push"

	// maxDeviceBody bounds every device-tier JSON body (a pairing request with
	// its step-up key, or a push subscription, is well under 2 KB).
	maxDeviceBody = 8 << 10

	// devicePairBurst per devicePairWindow is how many pairing attempts one
	// client address may make. A code is 256 bits, so this bounds database
	// work, not guessing.
	devicePairBurst  = 10
	devicePairWindow = time.Minute

	// deviceProbeTimeout bounds the test push a registration must pass.
	deviceProbeTimeout = 10 * time.Second
)

// DeviceTokenRefusal is the 401 body for every refused credential on the
// device tier: another kind of credential, a malformed one, an unknown or
// removed device, or a revoked one (disabled owner, cutoff, sign-out) — so a
// refusal says nothing about which.
const DeviceTokenRefusal = "this route takes a paired device credential (" + devices.TokenPrefix + "…)"

// DeviceTokenElsewhere is the 401 body when a device credential is presented on
// any route outside the device tier.
const DeviceTokenElsewhere = "a device credential (" + devices.TokenPrefix + "…) opens " + deviceAPIPrefix + "/* only"

// DevicePairRefusal is the 401 body for a pairing code that cannot be used:
// unknown, used, expired, or minted by a session that has since been ended.
const DevicePairRefusal = "this pairing code cannot be used: pair again from a signed-in session"

// DevicesNotServed is the 503 body when the server has no device store.
const DevicesNotServed = "this server does not serve paired devices (no database)"

func (s *Server) registerDeviceRoutes(mux Mux) {
	// The session tier.
	mux.HandleFunc("POST "+accountDevicesPairPath, s.requireOperatorWrite(s.handleDevicePairCode))
	mux.HandleFunc("POST /account/devices/{id}/remove", s.requireOperatorWrite(s.handleDeviceRemove))
	// The public pairing route.
	mux.HandleFunc("POST "+devicePairPath, s.handleDevicePair)
	// The device tier.
	mux.HandleFunc("DELETE "+deviceAPIPrefix, s.requireDeviceToken(s.handleDeviceUnpair))
	mux.HandleFunc("GET "+deviceConfigPath, s.requireDeviceToken(s.handleDeviceConfig))
	mux.HandleFunc("PUT "+devicePushPath, s.requireDeviceToken(s.handleDevicePushPut))
	mux.HandleFunc("DELETE "+devicePushPath, s.requireDeviceToken(s.handleDevicePushDelete))
}

// --- the device door ---------------------------------------------------------------

type deviceCtxKey struct{}

func deviceFromContext(ctx context.Context) devices.Device {
	d, _ := ctx.Value(deviceCtxKey{}).(devices.Device)
	return d
}

// requireDeviceToken admits a DEVICE credential and nothing else, acting as the
// device's owner through authz.ViaDevice. See the file header.
//
// ⚠ NO FAILURE LIMITER, AS FOR admitAPIToken: a 256-bit credential cannot be
// guessed, and the shape check refuses junk before any database read.
func (s *Server) requireDeviceToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.ext.Devices == nil {
			s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": DevicesNotServed})
			return
		}
		tok := hookTokenFromRequest(r)
		if !devices.ShapeOK(tok) {
			s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": DeviceTokenRefusal})
			return
		}
		d, err := s.ext.Devices.Authenticate(r.Context(), tok)
		switch {
		case err == nil:
		case errors.Is(err, devices.ErrBadToken), errors.Is(err, devices.ErrUserDisabled),
			errors.Is(err, devices.ErrSessionEnded), errors.Is(err, devices.ErrSignedOut):
			s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": DeviceTokenRefusal})
			return
		default:
			// The lookup FAILED; nothing was judged. 503 so the app keeps a good
			// credential.
			s.logger.Printf("devices: credential lookup failed: %v", err)
			s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "could not check the device credential"})
			return
		}
		ctx := context.WithValue(r.Context(), deviceCtxKey{}, d)
		s.admit(w, r.WithContext(ctx), next, s.principalFor(r.Context(), d.OwnerID, authz.ViaDevice))
	}
}

// refuseDeviceTokenElsewhere is admit's half of "mdv_ opens /api/device/* only":
// true (and the request answered) when a request admitted by any door but the
// device door carries a device-shaped credential.
func (s *Server) refuseDeviceTokenElsewhere(w http.ResponseWriter, r *http.Request, via authz.Via) bool {
	if via == authz.ViaDevice || !devices.LooksLike(hookTokenFromRequest(r)) {
		return false
	}
	s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": DeviceTokenElsewhere})
	return true
}

// --- the session tier ----------------------------------------------------------------

// handleDevicePairCode mints a pairing code for the signed-in user. The page
// (in the app's WebView) hands it to the app over the bridge.
func (s *Server) handleDevicePairCode(w http.ResponseWriter, r *http.Request) {
	if s.ext.Devices == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": DevicesNotServed})
		return
	}
	// A sign-in from the last five minutes, as for adding a passkey: the
	// credential this leads to outlives the session.
	if !stepup.FreshWithin(r.Context(), s.stepUpNow(), stepup.CredentialMaxAge) {
		s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": PasskeyReauthRequired,
			"detail": "pairing a phone needs a sign-in from the last " + stepup.HumanDuration(stepup.CredentialMaxAge)})
		return
	}
	signin, _ := stepup.AuthTime(r.Context())
	code, expires, err := s.ext.Devices.CreatePairingCode(r.Context(), signin)
	if err != nil {
		s.logger.Printf("devices: pairing code for user %d: %v", requestOwner(r.Context()), err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the pairing code could not be created"})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, map[string]any{
		"code": code, "expires_at": expires.UTC().Format(time.RFC3339),
		"expires_in": int(devices.PairingCodeTTL / time.Second),
	})
}

func (s *Server) handleDeviceRemove(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 || s.ext.Devices == nil {
		http.Error(w, "no such device", http.StatusNotFound)
		return
	}
	found, err := s.ext.Devices.Remove(r.Context(), id)
	if err != nil {
		s.logger.Printf("devices: remove %d: %v", id, err)
		http.Error(w, "the device could not be removed", http.StatusInternalServerError)
		return
	}
	if !found {
		http.Error(w, "no such device", http.StatusNotFound)
		return
	}
	http.Redirect(w, r, "/account?devices=removed#devices", http.StatusSeeOther)
}

// --- the public pairing route ---------------------------------------------------------

// addrRateLimit is a fixed window per client address.
type addrRateLimit struct {
	mu   sync.Mutex
	wins map[string]*ownerWindow
}

// allow reports whether addr may make one more attempt at now, and records it.
// The map is cleared when it grows past a bound, so it cannot be grown without
// limit by spoofed addresses (the window is short; losing it costs a reset).
func (l *addrRateLimit) allow(addr string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.wins == nil || len(l.wins) > 10000 {
		l.wins = map[string]*ownerWindow{}
	}
	w := l.wins[addr]
	if w == nil || now.Sub(w.start) >= devicePairWindow {
		w = &ownerWindow{start: now}
		l.wins[addr] = w
	}
	if w.count >= devicePairBurst {
		return false
	}
	w.count++
	return true
}

func (s *Server) handleDevicePair(w http.ResponseWriter, r *http.Request) {
	if s.ext.Devices == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": DevicesNotServed})
		return
	}
	if !s.devicePairLimit.allow(clientIP(r), s.now()) {
		w.Header().Set("Retry-After", strconv.Itoa(int(devicePairWindow/time.Second)))
		s.writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "too many pairing attempts; wait a minute"})
		return
	}
	var body struct {
		Code         string `json:"code"`
		Name         string `json:"name"`
		AppVersion   string `json:"app_version"`
		StepUpPubkey string `json:"stepup_pubkey"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxDeviceBody)).Decode(&body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": `the body is {"code","name","app_version","stepup_pubkey" (base64 SubjectPublicKeyInfo, EC P-256; optional)}`})
		return
	}
	tok, d, err := s.ext.Devices.Redeem(r.Context(), body.Code, devices.PairIn{
		Name: body.Name, AppVersion: body.AppVersion, StepUpKey: body.StepUpPubkey})
	switch {
	case err == nil:
	case errors.Is(err, devices.ErrInvalid):
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": strings.TrimPrefix(err.Error(), "devices: ")})
		return
	case errors.Is(err, devices.ErrTooMany):
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": strings.TrimPrefix(err.Error(), "devices: ")})
		return
	case errors.Is(err, devices.ErrBadCode), errors.Is(err, devices.ErrUserDisabled),
		errors.Is(err, devices.ErrSessionEnded), errors.Is(err, devices.ErrSignedOut):
		s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": DevicePairRefusal})
		return
	default:
		s.logger.Printf("devices: pair: %v", err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the device could not be paired"})
		return
	}
	s.logger.Printf("devices: user %d paired device %d (step-up key: %v)", d.OwnerID, d.ID, d.HasStepUpKey)
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusCreated, map[string]any{
		"token":  tok,
		"device": map[string]any{"id": d.ID, "name": d.Name, "stepup_key": d.HasStepUpKey},
	})
}

// --- the device tier ------------------------------------------------------------------

// handleDeviceUnpair is the app removing its own pairing: the same delete as
// Account → Devices' Remove, under the device's owner.
func (s *Server) handleDeviceUnpair(w http.ResponseWriter, r *http.Request) {
	d := deviceFromContext(r.Context())
	if _, err := s.ext.Devices.Remove(r.Context(), d.ID); err != nil {
		s.logger.Printf("devices: unpair %d: %v", d.ID, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the device could not be unpaired"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDeviceConfig is what a paired device needs to register for push: the
// VAPID public key it registers with (so its distributor can tell muster's
// pushes from others) and the operator's push hosts.
func (s *Server) handleDeviceConfig(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"push": map[string]any{"served": false, "vapid_public_key": "", "hosts": []string{}}}
	if s.pushServed() {
		out["push"] = map[string]any{"served": true, "vapid_public_key": s.ext.Push.PublicKey(), "hosts": s.ext.Push.Allowlist().ExtraHosts()}
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, out)
}

// devicePushProbe is the payload of the test push a registration must pass.
var devicePushProbe = []byte(`{"v":2,"type":"test"}`)

// handleDevicePushPut registers (or replaces) the device's UnifiedPush
// endpoint. It is kept only after muster has sent it a VAPID test push and got
// a 2xx (UnifiedPush's guidance): a registration whose endpoint does not work
// is refused here rather than failing silently later.
//
// 🔴 THE ALLOWLIST IS CHECKED BEFORE THE TEST PUSH. The probe POSTs to the
// endpoint, so an endpoint muster would refuse to store must not be POSTed to
// first.
func (s *Server) handleDevicePushPut(w http.ResponseWriter, r *http.Request) {
	if !s.pushServed() {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": pushUnconfigured})
		return
	}
	var body struct {
		Endpoint string `json:"endpoint"`
		P256dh   string `json:"p256dh"`
		Auth     string `json:"auth"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxDeviceBody)).Decode(&body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": `the body is {"endpoint","p256dh","auth"}`})
		return
	}
	var sub push.Subscription
	sub.Endpoint, sub.Keys.P256dh, sub.Keys.Auth = body.Endpoint, body.P256dh, body.Auth
	if err := sub.Check(); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if !s.ext.Push.Allowlist().Allows(sub.Endpoint) {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": push.ErrPushServiceNotAllowed.Error()})
		return
	}
	d := deviceFromContext(r.Context())
	ctx, cancel := context.WithTimeout(r.Context(), deviceProbeTimeout)
	res := s.ext.Push.Probe(ctx, sub, devicePushProbe)
	cancel()
	if res.Err != nil || res.StatusCode < 200 || res.StatusCode >= 300 {
		why := fmt.Sprintf("status %d", res.StatusCode)
		if res.Err != nil {
			why = push.Redact(res.Err, sub.Endpoint)
		}
		s.logger.Printf("devices: device %d's push endpoint %s refused the test push: %s", d.ID, push.Short(sub.Endpoint), why)
		s.writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "the push endpoint did not accept a test push (" + why + ")"})
		return
	}
	err := s.ext.Push.Store().ClaimNative(r.Context(), d.OwnerID, d.ID, sub, "muster app: "+d.Name)
	switch {
	case err == nil:
	case errors.Is(err, push.ErrBadSubscription), errors.Is(err, push.ErrPushServiceNotAllowed):
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	case errors.Is(err, push.ErrTooManyDevices):
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	default:
		s.logger.Printf("devices: device %d push register %s: %s", d.ID, push.Short(sub.Endpoint), push.Redact(err, sub.Endpoint))
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the push registration could not be saved"})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDevicePushDelete(w http.ResponseWriter, r *http.Request) {
	if !s.pushServed() {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": pushUnconfigured})
		return
	}
	d := deviceFromContext(r.Context())
	removed, err := s.ext.Push.Store().ForgetNative(r.Context(), d.OwnerID, d.ID)
	if err != nil {
		s.logger.Printf("devices: device %d push remove: %v", d.ID, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the push registration could not be removed"})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"removed": removed})
}

// --- Account → Devices ---------------------------------------------------------------

// AppBridgeName is the JavaScript object the Android app's WebView injects
// (WebViewCompat.addWebMessageListener) on muster's own origin only. The page
// posts to it the pairing code and the "open the app's notification settings"
// request; it never receives a credential from it. The app repository must use
// the same name.
const AppBridgeName = "musterApp"

// accountDevicesSection renders Account → Devices: the caller's paired phones
// (row-level security), each with Remove, and — only inside the app's WebView,
// where AppBridgeName exists — "Pair this phone" and the app's notification
// settings.
func (s *Server) accountDevicesSection(r *http.Request) string {
	var b strings.Builder
	b.WriteString(`<section id="devices" data-testid="paired-devices" class="flex flex-col gap-3 rounded-2xl border border-line bg-s1/70 px-5 py-6 ring-1 ring-line">
<h2 class="text-base font-semibold text-fg">Devices</h2>
`)
	if s.ext.Devices == nil {
		b.WriteString(`<p class="text-sm text-fg2">` + html.EscapeString(DevicesNotServed) + `</p></section>`)
		return b.String()
	}
	if r.URL.Query().Get("devices") == "removed" {
		b.WriteString(`<p role="status" class="text-sm font-semibold text-fg">Device removed. It is signed out.</p>`)
	}
	list, err := s.ext.Devices.List(r.Context())
	if err != nil {
		s.logger.Printf("devices: list: %v", err)
		b.WriteString(problemNode("The device list could not be read."))
		b.WriteString(`</section>`)
		return b.String()
	}
	if len(list) == 0 {
		b.WriteString(`<p class="text-sm text-fg2">No phone is paired. Open muster in the Android app to pair one.</p>`)
	} else {
		b.WriteString(`<ul class="flex flex-col gap-2">`)
		for _, d := range list {
			name := d.Name
			if name == "" {
				name = "(unnamed phone)"
			}
			seen := "never"
			if d.LastSeenAt != nil {
				seen = d.LastSeenAt.UTC().Format("2006-01-02 15:04 UTC")
			}
			pushState := "push not registered"
			if d.PushRegistered {
				pushState = "push registered"
			}
			version := ""
			if d.AppVersion != "" {
				version = ` · app ` + html.EscapeString(d.AppVersion)
			}
			b.WriteString(`<li data-testid="paired-device" data-device-id="` + strconv.FormatInt(d.ID, 10) + `" class="flex flex-col gap-1 rounded-md bg-bg px-3 py-2 ring-1 ring-inset ring-edge">
<span class="break-all text-sm font-semibold text-fg">` + html.EscapeString(truncate(name, 80)) + `</span>
<span class="text-xs text-fg2">paired ` + d.CreatedAt.UTC().Format("2006-01-02 15:04 UTC") + ` · last seen ` + html.EscapeString(seen) +
				version + ` · ` + pushState + `</span>
<form method="POST" action="/account/devices/` + strconv.FormatInt(d.ID, 10) + `/remove">
<button type="submit" class="press min-h-[44px] w-full rounded-md bg-bg px-3 py-2 text-sm font-semibold text-fg ring-1 ring-inset ring-edge">Remove</button></form>
</li>`)
		}
		b.WriteString(`</ul>`)
	}
	b.WriteString(`<div id="app-bridge" class="hidden flex-col gap-2">
<button type="button" id="pair-this-phone" class="press min-h-[44px] w-full rounded-md bg-accent px-3 py-2 text-base font-semibold text-on-accent">Pair this phone</button>
<button type="button" id="app-notification-settings" class="press min-h-[44px] w-full rounded-md bg-bg px-3 py-2 text-base font-semibold text-fg ring-1 ring-inset ring-edge">Notification settings</button>
<p id="pair-status" role="status" class="text-sm text-fg2"></p>
</div>
<script>` + appBridgeJS + `</script>
</section>`)
	return b.String()
}

// appBridgeJS shows the app-only controls when the page is inside the app's
// WebView (AppBridgeName exists) and drives them. The code goes to the app and
// nowhere else; the request is same-origin with the session cookie.
const appBridgeJS = `(function () {
  var bridge = window.` + AppBridgeName + `;
  var box = document.getElementById('app-bridge');
  if (!bridge || typeof bridge.postMessage !== 'function' || !box) return;
  box.classList.remove('hidden'); box.classList.add('flex');
  var status = document.getElementById('pair-status');
  document.getElementById('app-notification-settings').addEventListener('click', function () {
    bridge.postMessage(JSON.stringify({ type: 'open-notification-settings' }));
  });
  document.getElementById('pair-this-phone').addEventListener('click', async function () {
    status.textContent = 'Pairing…';
    try {
      var res = await fetch('` + accountDevicesPairPath + `', { method: 'POST', credentials: 'same-origin' });
      var data = await res.json().catch(function () { return {}; });
      if (res.status === 401 && data.error === '` + PasskeyReauthRequired + `') {
        status.textContent = 'Pairing needs a sign-in from the last five minutes. Sign out, sign in again, then tap Pair this phone.';
        return;
      }
      if (!res.ok || !data.code) { status.textContent = data.error || ('Pairing failed (' + res.status + ').'); return; }
      bridge.postMessage(JSON.stringify({ type: 'pair', code: data.code, expires_at: data.expires_at }));
      status.textContent = 'The app is finishing the pairing.';
    } catch (e) { status.textContent = 'Pairing failed: ' + e.message; }
  });
})();`
