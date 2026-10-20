package api

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/auth"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/devices"
)

// The device tier's confinement, over the principal walk's fakes: an mdv_
// credential opens /api/device/* and nothing else, nothing else opens
// /api/device/*, and a device cannot mint a pairing code (the design's server
// change 27, and "a token cannot mint a token").

// deviceRoutes is the device tier as routes.golden spells it.
func deviceRoutes() map[string]bool {
	out := map[string]bool{}
	for _, r := range routeVias[authz.ViaDevice] {
		out[r] = true
	}
	return out
}

// TestADeviceCredentialIsRefusedOutsideTheDeviceTier drives EVERY route in
// routes.golden outside the device tier with a valid device credential, on a
// server whose hook tier is UNARMED — so the hook-tier wrappers admit any
// caller, and only admit's shape check (refuseDeviceTokenElsewhere) stands
// between the credential and the handler. Session routes are driven with a
// valid session cookie as well, for the same reason. None may reach a handler.
//
// The positive controls: the same credential on the device tier reaches its
// handlers as the device's owner, and the shape refusal (DeviceTokenElsewhere)
// is what answered at least every hook-tier and session route — so the zero
// above is the check working, not every request failing for another reason.
func TestADeviceCredentialIsRefusedOutsideTheDeviceTier(t *testing.T) {
	s, h, seen := newPrincipalTestServerWith(t, AuthConfig{UIPassword: pPassword})
	tier := deviceRoutes()
	session := map[string]bool{}
	for _, r := range routeVias[authz.ViaSession] {
		session[r] = true
	}
	hookish := map[string]bool{}
	for _, r := range routeVias[authz.ViaHook] {
		hookish[r] = true
	}
	shapeRefused, driven, unarmed := 0, 0, 0
	for _, pattern := range readGolden(t) {
		if tier[pattern] {
			continue
		}
		method, path := concretePath(pattern)
		t.Run(pattern, func(t *testing.T) {
			*seen = nil
			req := httptest.NewRequest(method, path, strings.NewReader(""))
			req.Header.Set("Authorization", "Bearer "+pDeviceToken)
			if session[pattern] {
				req.Header.Set("Cookie", sessionCookieFor(s))
				req.Header.Set("Origin", "http://"+req.Host)
			}
			rec := httptest.NewRecorder()
			aborted := func() (aborted bool) {
				defer func() {
					if r := recover(); r != nil {
						aborted = r == errProbeAbort //nolint:errorlint // identity of the sentinel
					}
				}()
				h.ServeHTTP(rec, req)
				return false
			}()
			driven++
			if aborted || len(*seen) != 0 {
				t.Fatalf("a device credential reached the handler of %s as %v", pattern, *seen)
			}
			if strings.Contains(rec.Body.String(), DeviceTokenElsewhere) {
				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("the shape refusal answered %d; want 401", rec.Code)
				}
				shapeRefused++
			} else if rec.Code == http.StatusServiceUnavailable && strings.Contains(rec.Body.String(), `"`+HookUnarmedField+`":true`) {
				// The fail-closed hook routes (requireArmedHookToken) refuse an
				// unarmed server before any credential is looked at.
				unarmed++
			} else if session[pattern] || hookish[pattern] {
				t.Fatalf("%s was refused (%d %s) by something other than the device shape check — on an unarmed "+
					"hook tier or with a valid session, that check is the only wall", pattern, rec.Code, truncate(rec.Body.String(), 160))
			}
		})
	}
	if want := len(routeVias[authz.ViaSession]) + len(routeVias[authz.ViaHook]) - unarmed; shapeRefused < want || driven < 200 || unarmed > 4 {
		t.Fatalf("CONTROL: the shape refusal answered %d of %d routes; want at least the %d session and hook routes",
			shapeRefused, driven, want)
	}
	// Positive control: the device tier admits it, as user 1 through ViaDevice.
	for pattern := range tier {
		method, path := concretePath(pattern)
		*seen = nil
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+pDeviceToken)
		if aborted, code := serve(t, h, req); !aborted || len(*seen) != 1 || (*seen)[0].Via != authz.ViaDevice {
			t.Fatalf("CONTROL: %s with the device credential: aborted=%v status %d saw %v", pattern, aborted, code, *seen)
		}
	}
}

// TestTheDeviceTierRefusesEveryOtherCredential: a session cookie, the shared
// hook and service tokens, a personal token, a plugin token and a mirror token
// each reach no device-tier handler — in password mode and on gotrue mode's
// browser listener, where the device tier is served.
func TestTheDeviceTierRefusesEveryOtherCredential(t *testing.T) {
	s, h, seen := newPrincipalTestServer(t)
	others := []authz.Via{authz.ViaSession, authz.ViaHook, authz.ViaService, authz.ViaToken, authz.ViaPlugin,
		authz.ViaMirrorHost, authz.ViaRunner, authz.ViaAgent}
	for pattern := range deviceRoutes() {
		method, path := concretePath(pattern)
		for _, via := range others {
			for _, edge := range []bool{false, true} {
				*seen = nil
				req := httptest.NewRequest(method, path, nil)
				credential(s, req, via)
				hh := h
				if edge {
					hh = BrowserListener(h)
				}
				aborted, code := serve(t, hh, req)
				if aborted || len(*seen) != 0 {
					t.Fatalf("%s with a %s credential (browser listener %v) reached the handler as %v", pattern, via, edge, *seen)
				}
				if code != http.StatusUnauthorized {
					t.Fatalf("%s with a %s credential: %d; want 401", pattern, via, code)
				}
			}
		}
	}
}

// TestADeviceCannotMintAPairingCode: a device credential cannot ask for a
// pairing code — alone it is not a session, and beside a valid session cookie
// the request carries a device credential and is refused by its shape — so a
// device credential cannot lead to another. The control is the session alone,
// which is admitted.
func TestADeviceCannotMintAPairingCode(t *testing.T) {
	s, h, seen := newPrincipalTestServer(t)
	method, path := "POST", accountDevicesPairPath
	*seen = nil
	alone := httptest.NewRequest(method, path, nil)
	alone.Header.Set("Authorization", "Bearer "+pDeviceToken)
	alone.Header.Set("Origin", "http://"+alone.Host)
	if aborted, code := serve(t, h, alone); aborted || len(*seen) != 0 {
		t.Fatalf("a device credential alone minted a pairing code (status %d, principal %v)", code, *seen)
	}
	*seen = nil
	both := httptest.NewRequest(method, path, nil)
	credential(s, both, authz.ViaSession)
	both.Header.Set("Authorization", "Bearer "+pDeviceToken)
	if aborted, code := serve(t, h, both); aborted || len(*seen) != 0 || code != http.StatusUnauthorized {
		t.Fatalf("a device credential beside a session minted a pairing code (status %d, principal %v)", code, *seen)
	}
	*seen = nil
	ctl := httptest.NewRequest(method, path, nil)
	credential(s, ctl, authz.ViaSession)
	if aborted, _ := serve(t, h, ctl); !aborted || len(*seen) != 1 || (*seen)[0].Via != authz.ViaSession {
		t.Fatalf("CONTROL: the session alone was not admitted to %s (saw %v)", path, *seen)
	}
}

// codeStore is a devices.Store that mints codes and records the sign-in time
// each was asked for with. Nothing else is reached by these tests.
type codeStore struct {
	devices.Store
	mu      sync.Mutex
	signins []time.Time
}

func (c *codeStore) CreatePairingCode(_ context.Context, signin time.Time) (string, time.Time, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.signins = append(c.signins, signin)
	return "mdp_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", time.Now().Add(devices.PairingCodeTTL), nil
}

// TestPairingNeedsASignInFromTheLastFiveMinutes: a session signed in more than
// stepup.CredentialMaxAge ago is refused a pairing code (reauth_required) and
// the store is never asked; a session signed in just now gets one, recorded
// with THAT sign-in time.
func TestPairingNeedsASignInFromTheLastFiveMinutes(t *testing.T) {
	store := &codeStore{}
	s := New(nil, AuthConfig{UIPassword: pPassword}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Devices: store})
	h := s.Handler()
	post := func(issued time.Time) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", accountDevicesPairPath, nil)
		req.Header.Set("Cookie", sessionCookieName+"="+auth.SignSession(s.auth.uiSessionSecret(), issued, sessionTTL))
		req.Header.Set("Origin", "http://"+req.Host)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	stale := post(time.Now().Add(-6 * time.Minute))
	if stale.Code != http.StatusUnauthorized || !strings.Contains(stale.Body.String(), PasskeyReauthRequired) {
		t.Fatalf("a six-minute-old sign-in: %d %s; want 401 %s", stale.Code, stale.Body.String(), PasskeyReauthRequired)
	}
	if len(store.signins) != 0 {
		t.Fatal("the store was asked for a code on a stale sign-in")
	}
	signedIn := time.Now().Add(-time.Minute).Truncate(time.Second)
	fresh := post(signedIn)
	var out struct {
		Code      string `json:"code"`
		ExpiresIn int    `json:"expires_in"`
	}
	if fresh.Code != http.StatusOK || json.Unmarshal(fresh.Body.Bytes(), &out) != nil || !devices.CodeShapeOK(out.Code) || out.ExpiresIn != 60 {
		t.Fatalf("CONTROL: a one-minute-old sign-in: %d %s", fresh.Code, fresh.Body.String())
	}
	if fresh.Header().Get("Cache-Control") != "no-store" {
		t.Error("the pairing code response is cacheable")
	}
	if len(store.signins) != 1 || !store.signins[0].Equal(signedIn) {
		t.Fatalf("the code was minted with sign-in %v; want the session's %v", store.signins, signedIn)
	}
}
