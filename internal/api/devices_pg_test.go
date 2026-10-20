package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
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

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/devices"
	"github.com/ZacxDev/muster/internal/gotrue/gotruetest"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/users"
)

// The paired-device flow end to end, over the real stores and a real gotrue
// session on the browser listener: pair, read the config, register push, and
// each revocation a person can trigger from the web.

const (
	devOwnerEmail = "phone-owner@example.com"
	devOwnerPW    = "phone owner's password"
	// devNtfyHost is the operator push host the rig configures. A synthetic
	// name: nothing is ever sent to it (the pusher is recorded).
	devNtfyHost = "ntfy.example.org"
)

// probePusher records every push it is asked to send and answers status.
type probePusher struct {
	mu     sync.Mutex
	status int
	sent   []string // endpoints
	bodies []string
}

func (p *probePusher) Send(_ context.Context, sub push.Subscription, payload []byte) push.SendResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, sub.Endpoint)
	p.bodies = append(p.bodies, string(payload))
	return push.SendResult{StatusCode: p.status}
}

func (p *probePusher) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.sent)
}

type deviceRig struct {
	t      *testing.T
	pool   *pgxpool.Pool
	fake   *gotruetest.Fake
	s      *Server
	edge   http.Handler
	mach   http.Handler
	pusher *probePusher
	uid    int64
}

func newDeviceRig(t *testing.T) *deviceRig {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dbtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(dbtest.AsOperator(ctx), pool, log.New(io.Discard, "", 0)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	fake := gotruetest.New(t, gtSecret)
	// The device's revocation compares the pairing sign-in (GoTrue's amr)
	// with timestamps Postgres stamps, so the fake runs on the wall clock.
	fake.UseRealClock()
	sub := fake.AddUser(devOwnerEmail+strconv.FormatInt(time.Now().UnixNano(), 36), devOwnerPW)
	r := &deviceRig{t: t, pool: pool, fake: fake, pusher: &probePusher{status: http.StatusCreated}}
	sys := authz.System(ctx)
	if err := pool.QueryRow(sys, `INSERT INTO users (role, display_name, gotrue_sub, email) VALUES ('member', 'phone owner', $1, $2)
		RETURNING id`, sub, sub+"@example.com").Scan(&r.uid); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := authz.System(context.Background())
		for _, q := range []string{`DELETE FROM push_subscriptions WHERE owner_id = $1`, `DELETE FROM device_pairing_codes WHERE owner_id = $1`,
			`DELETE FROM devices WHERE owner_id = $1`, `DELETE FROM push_preferences WHERE owner_id = $1`,
			`DELETE FROM audit_log WHERE user_id = $1`, `DELETE FROM users WHERE id = $1`} {
			_, _ = pool.Exec(bg, q, r.uid)
		}
	})
	us := users.NewPG(pool)
	r.s = New(nil, AuthConfig{Mode: AuthModeGoTrue, HookToken: gtHook, PublicURL: "https://muster.example.com"},
		log.New(io.Discard, "", 0))
	_, vapidPub, err := push.GenerateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}
	store := push.NewPG(pool).WithAllowlist(push.NewAllowlist([]string{devNtfyHost}))
	r.s.UseExtensions(Extensions{Users: us, Devices: devices.NewPG(pool),
		Push: push.NewService(store, r.pusher, push.VAPIDConfig{PublicKey: vapidPub}, log.New(io.Discard, "", 0))})
	r.s.UseGoTrue(GoTrueAuth{Client: fake.Client(), Verifier: fake.Verifier(), Users: us})
	h := r.s.Handler()
	r.edge, r.mach = BrowserListener(h), h
	return r
}

// login signs the owner in on the browser listener and returns the cookies.
func (r *deviceRig) login() (string, string) {
	r.t.Helper()
	u, _ := r.fake.User(r.subject())
	w := do(r.edge, "POST", "/login", url.Values{"email": {u.Email}, "password": {devOwnerPW}})
	ck := setCookies(w)
	if w.Code != http.StatusSeeOther || ck[accessCookieName] == nil {
		r.t.Fatalf("sign-in: %d %v", w.Code, ck)
	}
	return ck[accessCookieName].Value, ck[refreshCookieName].Value
}

func (r *deviceRig) subject() string {
	var sub string
	if err := r.pool.QueryRow(authz.System(context.Background()), `SELECT gotrue_sub::text FROM users WHERE id = $1`, r.uid).Scan(&sub); err != nil {
		r.t.Fatal(err)
	}
	return sub
}

// pair asks for a code with the session's cookies and redeems it as the app.
func (r *deviceRig) pair(at, rt, name string) (string, int64) {
	r.t.Helper()
	w := do(r.edge, "POST", accountDevicesPairPath, nil, withCookies(at, rt))
	var code struct {
		Code string `json:"code"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &code) != nil {
		r.t.Fatalf("pairing code: %d %s", w.Code, w.Body.String())
	}
	body, _ := json.Marshal(map[string]string{"code": code.Code, "name": name, "app_version": "0.1.0"})
	w = r.call(r.edge, "POST", devicePairPath, "", string(body))
	var out struct {
		Token  string `json:"token"`
		Device struct {
			ID int64 `json:"id"`
		} `json:"device"`
	}
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &out) != nil || !devices.ShapeOK(out.Token) {
		r.t.Fatalf("redeem: %d %s", w.Code, w.Body.String())
	}
	return out.Token, out.Device.ID
}

// call is one app request: bearer, if set, and a JSON body.
func (r *deviceRig) call(h http.Handler, method, path, bearer, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func (r *deviceRig) config(tok string) int {
	return r.call(r.edge, "GET", deviceConfigPath, tok, "").Code
}

// TestAPhonePairsOnTheBrowserListenerAndSignOutEverywhereCutsItOff: the
// whole web-side flow in gotrue mode. The device credential is admitted on the
// BROWSER listener (and the shared hook token stays refused there); a sign-out
// of one session leaves the phone paired (control); Sign out everywhere cuts
// it off on its next request.
func TestAPhonePairsOnTheBrowserListenerAndSignOutEverywhereCutsItOff(t *testing.T) {
	r := newDeviceRig(t)
	at, rt := r.login()
	tok, _ := r.pair(at, rt, "Pixel")

	w := r.call(r.edge, "GET", deviceConfigPath, tok, "")
	var cfg struct {
		Version string `json:"server_version"`
		Push    struct {
			Served bool     `json:"served"`
			Key    string   `json:"vapid_public_key"`
			Hosts  []string `json:"hosts"`
		} `json:"push"`
		Pairing struct {
			PINKeys *bool `json:"pin_keys_allowed"`
		} `json:"pairing"`
		Features map[string]bool `json:"features"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &cfg) != nil || !cfg.Push.Served || cfg.Push.Key == "" ||
		len(cfg.Push.Hosts) != 1 || cfg.Push.Hosts[0] != devNtfyHost || cfg.Version != BuildVersion ||
		cfg.Pairing.PINKeys == nil || *cfg.Pairing.PINKeys || !cfg.Features["push"] || cfg.Features["actions"] {
		t.Fatalf("config on the browser listener: %d %s", w.Code, w.Body.String())
	}
	if c := r.call(r.mach, "GET", deviceConfigPath, tok, "").Code; c != http.StatusOK {
		t.Fatalf("config on the machine listener: %d", c)
	}
	if c := r.call(r.edge, "GET", deviceConfigPath, gtHook, "").Code; c != http.StatusUnauthorized {
		t.Fatalf("the shared hook token on the device tier, browser listener: %d; want 401", c)
	}
	// The browser listener still refuses the shared tokens on the machine tier.
	if c := r.call(r.edge, "GET", "/api/whoami", gtHook, "").Code; c != http.StatusForbidden {
		t.Fatalf("the shared hook token on /api/whoami, browser listener: %d; want 403", c)
	}
	if c := r.call(r.edge, "GET", "/api/whoami", tok, "").Code; c != http.StatusUnauthorized && c != http.StatusForbidden {
		t.Fatalf("the device credential on /api/whoami: %d; want a refusal", c)
	}

	// A sign-out of ONE session: the phone stays paired.
	if w := do(r.edge, "POST", "/logout", url.Values{}, withCookies(at, rt)); w.Code != http.StatusSeeOther {
		t.Fatalf("logout: %d", w.Code)
	}
	if c := r.config(tok); c != http.StatusOK {
		t.Fatalf("CONTROL: after a sign-out of one session the phone got %d", c)
	}
	// Sign out everywhere: cut off.
	at, rt = r.login()
	if w := do(r.edge, "POST", "/logout", url.Values{"scope": {"global"}}, withCookies(at, rt)); w.Code != http.StatusSeeOther {
		t.Fatalf("global logout: %d", w.Code)
	}
	if c := r.config(tok); c != http.StatusUnauthorized {
		t.Fatalf("after Sign out everywhere the phone got %d; want 401", c)
	}
}

// TestRemovingAPhoneOnTheAccountPageCutsItOffAndDropsItsPush: Account →
// Devices lists the phone with its push state; Remove signs it out on its
// next request and deletes its push subscription. The app's own unpair does
// the same for another phone.
func TestRemovingAPhoneOnTheAccountPageCutsItOffAndDropsItsPush(t *testing.T) {
	r := newDeviceRig(t)
	at, rt := r.login()
	tok, id := r.pair(at, rt, "Pixel of the operator")
	reg := `{"endpoint":"https://` + devNtfyHost + `/upAbc123?up=1","p256dh":"BPk","auth":"au"}`
	if w := r.call(r.edge, "PUT", devicePushPath, tok, reg); w.Code != http.StatusOK {
		t.Fatalf("push register: %d %s", w.Code, w.Body.String())
	}
	page := do(r.edge, "GET", "/account", nil, withCookies(at, rt), asNavigation)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "Pixel of the operator") ||
		!strings.Contains(page.Body.String(), "push registered") || !strings.Contains(page.Body.String(), `data-device-id="`+strconv.FormatInt(id, 10)+`"`) {
		t.Fatalf("Account → Devices: %d\n%s", page.Code, truncate(page.Body.String(), 400))
	}
	if w := do(r.edge, "POST", "/account/devices/"+strconv.FormatInt(id, 10)+"/remove", nil, withCookies(at, rt)); w.Code != http.StatusSeeOther {
		t.Fatalf("remove: %d %s", w.Code, w.Body.String())
	}
	if c := r.config(tok); c != http.StatusUnauthorized {
		t.Fatalf("a removed phone got %d; want 401", c)
	}
	var subs int
	if err := r.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM push_subscriptions WHERE owner_id = $1`, r.uid).Scan(&subs); err != nil || subs != 0 {
		t.Fatalf("push subscriptions after the remove: %d (%v)", subs, err)
	}

	other, _ := r.pair(at, rt, "second phone")
	if w := r.call(r.edge, "DELETE", deviceAPIPrefix, other, ""); w.Code != http.StatusNoContent {
		t.Fatalf("unpair: %d %s", w.Code, w.Body.String())
	}
	if c := r.config(other); c != http.StatusUnauthorized {
		t.Fatalf("an unpaired phone got %d; want 401", c)
	}
}

// TestDisablingOrResettingTheAccountCutsThePhoneOff: the account-level
// revocations, through the real users store, refuse the credential over HTTP.
func TestDisablingOrResettingTheAccountCutsThePhoneOff(t *testing.T) {
	for _, path := range []string{"disable", "reset"} {
		t.Run(path, func(t *testing.T) {
			r := newDeviceRig(t)
			at, rt := r.login()
			tok, _ := r.pair(at, rt, "Pixel")
			if c := r.config(tok); c != http.StatusOK {
				t.Fatalf("CONTROL: before the %s: %d", path, c)
			}
			us := users.NewPG(r.pool)
			var err error
			if path == "disable" {
				_, err = us.SetDisabled(context.Background(), r.uid, true)
			} else {
				_, err = us.ResetCredentials(context.Background(), r.uid)
			}
			if err != nil {
				t.Fatal(err)
			}
			if c := r.config(tok); c != http.StatusUnauthorized {
				t.Fatalf("after the %s: %d; want 401", path, c)
			}
		})
	}
}

// TestPushRegistrationIsCheckedThenProbedThenStored: an endpoint off the
// allowlist is refused WITHOUT being POSTed to; one that fails the test push is
// refused and not stored; one that passes is stored as the device's native
// subscription and replaces its previous endpoint; DELETE removes it.
func TestPushRegistrationIsCheckedThenProbedThenStored(t *testing.T) {
	r := newDeviceRig(t)
	at, rt := r.login()
	tok, id := r.pair(at, rt, "Pixel")
	stored := func() []string {
		rows, err := r.pool.Query(authz.System(context.Background()),
			`SELECT endpoint || '|' || kind || '|' || coalesce(device_id::text, '') FROM push_subscriptions WHERE owner_id = $1 ORDER BY id`, r.uid)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			_ = rows.Scan(&s)
			out = append(out, s)
		}
		return out
	}
	reg := func(ep string) string {
		return `{"endpoint":"` + ep + `","p256dh":"BPk","auth":"au"}`
	}
	for _, ep := range []string{"https://a." + devNtfyHost + "/up1", "https://evil.example/up1", "https://" + devNtfyHost + ":8443/up1"} {
		if w := r.call(r.edge, "PUT", devicePushPath, tok, reg(ep)); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d; want 400", ep, w.Code)
		}
	}
	if n := r.pusher.count(); n != 0 {
		t.Fatalf("an endpoint off the allowlist was POSTed to %d time(s)", n)
	}
	r.pusher.status = http.StatusInternalServerError
	if w := r.call(r.edge, "PUT", devicePushPath, tok, reg("https://"+devNtfyHost+"/upFails")); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a failed test push: %d; want 422", w.Code)
	}
	if got := stored(); len(got) != 0 {
		t.Fatalf("a registration that failed its test push was stored: %v", got)
	}
	r.pusher.status = http.StatusCreated
	first, second := "https://"+devNtfyHost+"/upOne", "https://"+devNtfyHost+"/upTwo"
	for _, ep := range []string{first, second} {
		if w := r.call(r.edge, "PUT", devicePushPath, tok, reg(ep)); w.Code != http.StatusOK {
			t.Fatalf("register %s: %d %s", ep, w.Code, w.Body.String())
		}
	}
	if got, want := stored(), second+"|android|"+strconv.FormatInt(id, 10); len(got) != 1 || got[0] != want {
		t.Fatalf("stored = %v; want only %q", got, want)
	}
	r.pusher.mu.Lock()
	probe := r.pusher.bodies[len(r.pusher.bodies)-1]
	r.pusher.mu.Unlock()
	if probe != string(devicePushProbe) {
		t.Fatalf("the test push carried %q", probe)
	}
	if w := r.call(r.edge, "DELETE", devicePushPath, tok, ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"removed":true`) {
		t.Fatalf("push delete: %d %s", w.Code, w.Body.String())
	}
	if got := stored(); len(got) != 0 {
		t.Fatalf("after the delete: %v", got)
	}
}

// sendCount is how many pushes the rig's pusher was asked to make to ep.
func (r *deviceRig) sendCount(ep string) int {
	r.pusher.mu.Lock()
	defer r.pusher.mu.Unlock()
	n := 0
	for _, e := range r.pusher.sent {
		if e == ep {
			n++
		}
	}
	return n
}

// registered pairs a phone with push registered at ep and returns its
// credential, after checking a notification reaches it (the control).
func (r *deviceRig) registered(t *testing.T, ep string) (string, string, string) {
	t.Helper()
	at, rt := r.login()
	tok, _ := r.pair(at, rt, "Pixel")
	if w := r.call(r.edge, "PUT", devicePushPath, tok, `{"endpoint":"`+ep+`","p256dh":"BPk","auth":"au"}`); w.Code != http.StatusOK {
		t.Fatalf("register: %d %s", w.Code, w.Body.String())
	}
	before := r.sendCount(ep)
	r.s.ext.Push.SendTo(context.Background(), r.uid, push.Payload{Type: "review", Tag: "task-1", Title: "agent"})
	if r.sendCount(ep) != before+1 {
		t.Fatal("CONTROL: a notification did not reach the registered phone")
	}
	return tok, at, rt
}

// TestARevokedPhoneGetsNoPush is the audit's probe: pair, register push, end
// the phone by each account-level path a person can take — Sign out everywhere
// (the real logout), a password reset, a disable — and a notification to the
// owner reaches the phone ZERO times. Before the fix the credential was
// refused but the push row lived on and was sent to.
func TestARevokedPhoneGetsNoPush(t *testing.T) {
	for _, path := range []string{"sign out everywhere", "reset", "disable"} {
		t.Run(path, func(t *testing.T) {
			r := newDeviceRig(t)
			ep := "https://" + devNtfyHost + "/upRevoked" + strings.ReplaceAll(path, " ", "")
			_, at, rt := r.registered(t, ep)
			us := users.NewPG(r.pool)
			switch path {
			case "sign out everywhere":
				if w := do(r.edge, "POST", "/logout", url.Values{"scope": {"global"}}, withCookies(at, rt)); w.Code != http.StatusSeeOther {
					t.Fatalf("global logout: %d %s", w.Code, w.Body.String())
				}
			case "reset":
				if _, err := us.ResetCredentials(context.Background(), r.uid); err != nil {
					t.Fatal(err)
				}
			case "disable":
				if _, err := us.SetDisabled(context.Background(), r.uid, true); err != nil {
					t.Fatal(err)
				}
			}
			before := r.sendCount(ep)
			r.s.ext.Push.SendTo(context.Background(), r.uid, push.Payload{Type: "review", Tag: "task-2", Title: "agent"})
			if got := r.sendCount(ep) - before; got != 0 {
				t.Fatalf("after %s the revoked phone was sent %d push(es)", path, got)
			}
		})
	}
}

// TestAPushRowThatOutlivedItsPhonesRevocationIsNotSent is the second wall:
// with the owner's sign-out recorded directly (no writer, so the phone's rows
// survive), the fan-out still sends the phone nothing — the push store applies
// the device credential's own rule (devices.LiveSQL) to android rows.
func TestAPushRowThatOutlivedItsPhonesRevocationIsNotSent(t *testing.T) {
	r := newDeviceRig(t)
	ep := "https://" + devNtfyHost + "/upOutlived"
	r.registered(t, ep)
	if _, err := r.pool.Exec(authz.System(context.Background()), `UPDATE users SET signed_out_at = now() WHERE id = $1`, r.uid); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := r.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM push_subscriptions WHERE owner_id = $1`, r.uid).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("fixture: %d push rows (%v); the row must have survived", rows, err)
	}
	before := r.sendCount(ep)
	r.s.ext.Push.SendTo(context.Background(), r.uid, push.Payload{Type: "review", Tag: "task-3", Title: "agent"})
	if got := r.sendCount(ep) - before; got != 0 {
		t.Fatalf("a phone signed out everywhere was sent %d push(es) from a surviving row", got)
	}
}

// TestTheDeviceRoutesAreThrottled: per device on the device tier, per address
// before the credential is even looked up, and tighter on PUT /api/device/push
// (it makes an outbound test push). Each answers 429 past its limit, and a
// different device or address is unaffected (control).
func TestTheDeviceRoutesAreThrottled(t *testing.T) {
	r := newDeviceRig(t)
	at, rt := r.login()
	tok, _ := r.pair(at, rt, "Pixel")
	other, _ := r.pair(at, rt, "Pixel 2")
	from := func(ip string) func(*http.Request) {
		return func(q *http.Request) { q.Header.Set("CF-Connecting-IP", ip) }
	}
	call := func(method, path, bearer, body, ip string) int {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+bearer)
		from(ip)(req)
		w := httptest.NewRecorder()
		r.edge.ServeHTTP(w, req)
		return w.Code
	}
	// Per device: each call from a fresh address, so only the device's own
	// counter can refuse.
	for i := 0; i < deviceRoutePerDevice; i++ {
		if c := call("GET", deviceConfigPath, tok, "", "198.51.100."+strconv.Itoa(i%250+1)); c != http.StatusOK {
			t.Fatalf("request %d: %d", i+1, c)
		}
	}
	if c := call("GET", deviceConfigPath, tok, "", "203.0.113.200"); c != http.StatusTooManyRequests {
		t.Fatalf("past the per-device limit: %d; want 429", c)
	}
	if c := call("GET", deviceConfigPath, other, "", "203.0.113.201"); c != http.StatusOK {
		t.Fatalf("CONTROL: another device: %d", c)
	}
	// Per address, before the credential: a bad credential from one address.
	for i := 0; i < deviceRoutePerAddr; i++ {
		if c := call("GET", deviceConfigPath, "mdv_bad", "", "203.0.113.7"); c != http.StatusUnauthorized {
			t.Fatalf("bad credential %d: %d", i+1, c)
		}
	}
	if c := call("GET", deviceConfigPath, other, "", "203.0.113.7"); c != http.StatusTooManyRequests {
		t.Fatalf("past the per-address limit: %d; want 429", c)
	}
	// The push registration: tighter, per device.
	third, _ := r.pair(at, rt, "Pixel 3")
	reg := `{"endpoint":"https://` + devNtfyHost + `/upThrottle","p256dh":"BPk","auth":"au"}`
	for i := 0; i < devicePushPerDevice; i++ {
		if c := call("PUT", devicePushPath, third, reg, "192.0.2."+strconv.Itoa(i+1)); c != http.StatusOK {
			t.Fatalf("push register %d: %d", i+1, c)
		}
	}
	probes := r.sendCount("https://" + devNtfyHost + "/upThrottle")
	if c := call("PUT", devicePushPath, third, reg, "192.0.2.99"); c != http.StatusTooManyRequests {
		t.Fatalf("past the push-registration limit: %d; want 429", c)
	}
	if r.sendCount("https://"+devNtfyHost+"/upThrottle") != probes {
		t.Fatal("a throttled registration still made its outbound test push")
	}
	// …and per address: two more phones from ONE address, each under its own
	// limit, together reach the address's.
	p4, _ := r.pair(at, rt, "Pixel 4")
	p5, _ := r.pair(at, rt, "Pixel 5")
	p6, _ := r.pair(at, rt, "Pixel 6")
	n := 0
	for _, dev := range []string{p4, p5} {
		for i := 0; i < devicePushPerDevice; i++ {
			if c := call("PUT", devicePushPath, dev, reg, "198.18.0.1"); c != http.StatusOK {
				t.Fatalf("push register %d from one address: %d", n+1, c)
			}
			n++
		}
	}
	if n != devicePushPerAddr {
		t.Fatalf("fixture: %d registrations from one address; the limit is %d", n, devicePushPerAddr)
	}
	if c := call("PUT", devicePushPath, p6, reg, "198.18.0.1"); c != http.StatusTooManyRequests {
		t.Fatalf("past the per-address push-registration limit: %d; want 429", c)
	}
}

// TestPairingRecordsTheStepUpKeyAndItsPolicy: a biometric hardware key pairs
// and Account → Devices says so; a software key is refused 403; a PIN key is
// refused 403 with the policy off.
func TestPairingRecordsTheStepUpKeyAndItsPolicy(t *testing.T) {
	r := newDeviceRig(t)
	at, rt := r.login()
	key := testP256SPKI(t)
	redeem := func(typ, level string) int {
		w := do(r.edge, "POST", accountDevicesPairPath, nil, withCookies(at, rt))
		var code struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(w.Body.Bytes(), &code) != nil {
			t.Fatalf("code: %d %s", w.Code, w.Body.String())
		}
		body, _ := json.Marshal(map[string]string{"code": code.Code, "name": "keyed " + typ + " " + level,
			"stepup_pubkey": key, "stepup_key_type": typ, "stepup_key_level": level})
		return r.call(r.edge, "POST", devicePairPath, "", string(body)).Code
	}
	if c := redeem(devices.KeyBiometric, devices.LevelSoftware); c != http.StatusForbidden {
		t.Fatalf("a software key: %d; want 403", c)
	}
	if c := redeem(devices.KeyBiometricOrCredential, devices.LevelStrongBox); c != http.StatusForbidden {
		t.Fatalf("a PIN key with the policy off: %d; want 403", c)
	}
	if c := redeem(devices.KeyBiometric, devices.LevelStrongBox); c != http.StatusCreated {
		t.Fatalf("CONTROL: a biometric StrongBox key: %d", c)
	}
	page := do(r.edge, "GET", "/account", nil, withCookies(at, rt), asNavigation)
	if !strings.Contains(page.Body.String(), "step-up: biometric") {
		t.Fatalf("Account → Devices does not show the key type:\n%s", truncate(page.Body.String(), 400))
	}
}

// testP256SPKI is a fresh EC P-256 public key as base64 SubjectPublicKeyInfo,
// the shape the app sends.
func testP256SPKI(t *testing.T) string {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(der)
}
