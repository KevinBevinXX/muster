package api

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/push"
)

// memPush is push.Store in memory, with row-level security played honestly:
// the request methods see only the principal's devices.
type memPush struct {
	mu      sync.Mutex
	devs    []push.Device
	nextID  int64
	forgot  []string // "owner:hash" ForgetDevice was called with
	claimed []int64  // owners Claim was called for
}

func (m *memPush) uid(ctx context.Context) int64 { return authz.MustFrom(ctx).UserID }

func (m *memPush) Claim(_ context.Context, owner int64, sub push.Subscription, ua string) (int64, error) {
	if err := sub.Check(); err != nil {
		return 0, err
	}
	if !push.AllowedPushService(sub.Endpoint) {
		return 0, push.ErrPushServiceNotAllowed
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.claimed = append(m.claimed, owner)
	for i, d := range m.devs {
		if d.Sub.Endpoint == sub.Endpoint {
			prev := d.OwnerID
			m.devs[i].OwnerID, m.devs[i].Sub, m.devs[i].UserAgent = owner, sub, ua
			if prev == owner {
				prev = 0
			}
			return prev, nil
		}
	}
	m.nextID++
	m.devs = append(m.devs, push.Device{ID: m.nextID, OwnerID: owner, Sub: sub, UserAgent: ua})
	return 0, nil
}

func (m *memPush) Devices(ctx context.Context) ([]push.Device, error) {
	return m.ForOwner(ctx, m.uid(ctx))
}

func (m *memPush) del(match func(push.Device) bool) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, d := range m.devs {
		if match(d) {
			m.devs = append(m.devs[:i], m.devs[i+1:]...)
			return true
		}
	}
	return false
}

func (m *memPush) Remove(ctx context.Context, id int64) (bool, error) {
	uid := m.uid(ctx)
	return m.del(func(d push.Device) bool { return d.ID == id && d.OwnerID == uid }), nil
}

func (m *memPush) Unsubscribe(ctx context.Context, ep string) (bool, error) {
	uid := m.uid(ctx)
	return m.del(func(d push.Device) bool { return d.Sub.Endpoint == ep && d.OwnerID == uid }), nil
}

func (m *memPush) ForOwner(_ context.Context, owner int64) ([]push.Device, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []push.Device
	for _, d := range m.devs {
		if d.OwnerID == owner {
			out = append(out, d)
		}
	}
	return out, nil
}

func (m *memPush) MarkDelivered(context.Context, push.Device) error { return nil }
func (m *memPush) MarkFailed(context.Context, push.Device) error    { return nil }
func (m *memPush) Prune(_ context.Context, d push.Device) (bool, error) {
	return m.del(func(x push.Device) bool { return x.ID == d.ID && x.OwnerID == d.OwnerID }), nil
}

func (m *memPush) ForgetDevice(_ context.Context, owner int64, hash string) (bool, error) {
	m.mu.Lock()
	m.forgot = append(m.forgot, strconv.FormatInt(owner, 10)+":"+hash)
	m.mu.Unlock()
	return m.del(func(d push.Device) bool { return d.OwnerID == owner && push.EndpointHash(d.Sub.Endpoint) == hash }), nil
}

func (m *memPush) owners() map[string]int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]int64{}
	for _, d := range m.devs {
		out[d.Sub.Endpoint] = d.OwnerID
	}
	return out
}

// recPusher records the endpoints it was asked to reach.
type recPusher struct {
	mu   sync.Mutex
	sent []string
}

func (p *recPusher) Send(_ context.Context, sub push.Subscription, _ []byte) push.SendResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, sub.Endpoint)
	return push.SendResult{StatusCode: http.StatusCreated}
}

const testVAPIDPublic = "test-vapid-public-key-not-a-real-key"

func pushServer(t *testing.T, store push.Store, pusher push.Pusher, uid int64) (*Server, http.Handler) {
	t.Helper()
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	ext := Extensions{}
	if store != nil {
		ext.Push = push.NewService(store, pusher, push.VAPIDConfig{PublicKey: testVAPIDPublic}, log.New(io.Discard, "", 0))
	}
	s.UseExtensions(ext)
	s.sessionUserOverride = func(*http.Request) int64 { return uid }
	return s, s.Handler()
}

// pushDo is a same-origin session request; body is sent as JSON when it starts
// with '{', as a form otherwise.
func pushDo(t *testing.T, h http.Handler, method, path, body string, opts ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = "muster.test"
	if strings.HasPrefix(body, "{") {
		req.Header.Set("Content-Type", "application/json")
	} else if body != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Origin", "http://muster.test")
	req.Header.Set("Cookie", sessionCookieFor(&Server{auth: AuthConfig{UIPassword: testUIPassword}}))
	for _, o := range opts {
		o(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const fcmEndpoint = "https://fcm.googleapis.com/fcm/send/route-test-device"

func subscribeBody(endpoint string) string {
	return `{"endpoint":"` + endpoint + `","owner_id":1,"ownerId":1,"keys":{"p256dh":"k","auth":"a"}}`
}

// TestPushIsOffWithoutKeys: with no VAPID keys the key route answers 503, the
// subscribe route refuses, the shell draws no Enable button and the account
// page says push is not configured. With keys, the same server shape answers
// the key and draws the button — so each absence is a fact about the keys.
func TestPushIsOffWithoutKeys(t *testing.T) {
	_, off := pushServer(t, nil, nil, 7)
	if rec := pushDo(t, off, "GET", pushKeyPath, ""); rec.Code != http.StatusServiceUnavailable ||
		!strings.Contains(rec.Body.String(), "MUSTER_VAPID_") {
		t.Errorf("key route with no keys: %d %s", rec.Code, rec.Body)
	}
	if rec := pushDo(t, off, "POST", pushSubscribePath, subscribeBody(fcmEndpoint)); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("subscribe with no keys: %d", rec.Code)
	}
	if page := pushDo(t, off, "GET", "/tasks", "").Body.String(); strings.Contains(page, `id="enable-push"`) {
		t.Error("the shell draws the Enable button with no VAPID keys")
	}
	if page := pushDo(t, off, "GET", "/account", "").Body.String(); !strings.Contains(page, pushUnconfigured) {
		t.Error("the account page does not say push is unconfigured")
	}

	_, on := pushServer(t, &memPush{}, &recPusher{}, 7)
	rec := pushDo(t, on, "GET", pushKeyPath, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), testVAPIDPublic) {
		t.Errorf("key route with keys: %d %s", rec.Code, rec.Body)
	}
	if page := pushDo(t, on, "GET", "/tasks", "").Body.String(); !strings.Contains(page, `id="enable-push"`) {
		t.Error("the shell does not draw the Enable button WITH VAPID keys (the positive control)")
	}
}

// TestSubscribeOwnsTheDeviceByThePrincipal: the device is the SESSION user's
// whatever the body claims; the browser gets its device cookie; a
// cross-origin POST and a non-push-service endpoint store nothing.
func TestSubscribeOwnsTheDeviceByThePrincipal(t *testing.T) {
	store := &memPush{}
	_, h := pushServer(t, store, &recPusher{}, 7)

	rec := pushDo(t, h, "POST", pushSubscribePath, subscribeBody(fcmEndpoint))
	if rec.Code != http.StatusOK {
		t.Fatalf("subscribe: %d %s", rec.Code, rec.Body)
	}
	if got := store.owners()[fcmEndpoint]; got != 7 {
		t.Fatalf("the device is owned by user %d; the session is user 7 (the body said 1)", got)
	}
	ck := setCookies(rec)[pushDeviceCookie]
	if ck == nil || ck.Value != push.EndpointHash(fcmEndpoint) || !ck.HttpOnly {
		t.Fatalf("device cookie = %+v", ck)
	}

	evil := pushDo(t, h, "POST", pushSubscribePath, subscribeBody("https://fcm.googleapis.com/fcm/send/forged"),
		func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") })
	if evil.Code != http.StatusForbidden || len(store.owners()) != 1 {
		t.Errorf("a cross-origin subscribe: %d, %d devices", evil.Code, len(store.owners()))
	}
	ssrf := pushDo(t, h, "POST", pushSubscribePath, subscribeBody("https://kubernetes.default.svc/api"))
	if ssrf.Code != http.StatusBadRequest || len(store.owners()) != 1 {
		t.Errorf("a non-push-service endpoint: %d, %d devices", ssrf.Code, len(store.owners()))
	}
	if junk := pushDo(t, h, "POST", pushSubscribePath, `{"endpoint":`); junk.Code != http.StatusBadRequest {
		t.Errorf("a malformed body: %d", junk.Code)
	}
}

// TestSendTestReachesOnlyTheCallersDevices: "send test" on the account page
// delivers to the caller's devices and reports the count; another user's
// device is never asked.
func TestSendTestReachesOnlyTheCallersDevices(t *testing.T) {
	store := &memPush{}
	pusher := &recPusher{}
	mine, theirs := fcmEndpoint, "https://fcm.googleapis.com/fcm/send/someone-else"
	for owner, ep := range map[int64]string{7: mine, 8: theirs} {
		var sub push.Subscription
		sub.Endpoint, sub.Keys.P256dh, sub.Keys.Auth = ep, "k", "a"
		if _, err := store.Claim(context.Background(), owner, sub, "ua"); err != nil {
			t.Fatal(err)
		}
	}
	_, h := pushServer(t, store, pusher, 7)
	rec := pushDo(t, h, "POST", accountPushTestPath, "")
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "ok=1&of=1") {
		t.Fatalf("send test: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if len(pusher.sent) != 1 || pusher.sent[0] != mine {
		t.Fatalf("send test asked the pusher for %v; want only the caller's device", pusher.sent)
	}
	page := pushDo(t, h, "GET", "/account?push=test&ok=1&of=1", "").Body.String()
	if !strings.Contains(page, "1 of 1 device(s)") || strings.Contains(page, "someone-else") {
		t.Errorf("account page after the test:\n%s", page)
	}
	if strings.Contains(page, "route-test-device") {
		t.Error("the account page prints the device's endpoint path (a capability)")
	}
}

// TestRemoveIsTheCallersDevicesOnly: removing another user's device id is a
// 404 and leaves it; removing one's own works.
func TestRemoveIsTheCallersDevicesOnly(t *testing.T) {
	store := &memPush{}
	var sub push.Subscription
	sub.Endpoint, sub.Keys.P256dh, sub.Keys.Auth = fcmEndpoint, "k", "a"
	if _, err := store.Claim(context.Background(), 8, sub, "ua"); err != nil {
		t.Fatal(err)
	}
	_, as7 := pushServer(t, store, &recPusher{}, 7)
	if rec := pushDo(t, as7, "POST", "/account/push/1/remove", ""); rec.Code != http.StatusNotFound || len(store.owners()) != 1 {
		t.Fatalf("user 7 removing user 8's device: %d, %d left", rec.Code, len(store.owners()))
	}
	_, as8 := pushServer(t, store, &recPusher{}, 8)
	if rec := pushDo(t, as8, "POST", "/account/push/1/remove", ""); rec.Code != http.StatusSeeOther || len(store.owners()) != 0 {
		t.Fatalf("the owner's remove: %d, %d left", rec.Code, len(store.owners()))
	}
}

// TestLogoutUnsubscribesThisBrowser: signing out (password mode and gotrue
// mode) removes THIS browser's device — named by its device cookie — for the
// signing-out user, and clears the cookie. Without the cookie nothing is
// forgotten.
func TestLogoutUnsubscribesThisBrowser(t *testing.T) {
	hash := push.EndpointHash(fcmEndpoint)
	withDevice := func(r *http.Request) { r.AddCookie(&http.Cookie{Name: pushDeviceCookie, Value: hash}) }

	// Password mode.
	store := &memPush{}
	var sub push.Subscription
	sub.Endpoint, sub.Keys.P256dh, sub.Keys.Auth = fcmEndpoint, "k", "a"
	if _, err := store.Claim(context.Background(), 7, sub, "ua"); err != nil {
		t.Fatal(err)
	}
	_, h := pushServer(t, store, &recPusher{}, 7)
	if rec := pushDo(t, h, "POST", "/logout", ""); rec.Code != http.StatusSeeOther || len(store.owners()) != 1 {
		t.Fatalf("a logout with no device cookie forgot a device: %d, %v", rec.Code, store.owners())
	}
	rec := pushDo(t, h, "POST", "/logout", "", withDevice)
	if len(store.owners()) != 0 {
		t.Fatalf("password-mode logout left the browser subscribed: %v (forget calls %v)", store.owners(), store.forgot)
	}
	if ck := setCookies(rec)[pushDeviceCookie]; ck == nil || ck.MaxAge >= 0 {
		t.Error("logout did not clear the device cookie")
	}

	// GoTrue mode.
	f := newGoTrueFixture(t)
	gstore := &memPush{}
	if _, err := gstore.Claim(context.Background(), 7, sub, "ua"); err != nil {
		t.Fatal(err)
	}
	f.s.UseExtensions(Extensions{Push: push.NewService(gstore, &recPusher{}, push.VAPIDConfig{PublicKey: "k"}, nil)})
	gsub := f.member("m@example.com", 7, authz.RoleMember)
	at, rt, _ := f.fake.Mint(gsub)
	edge := BrowserListener(f.s.Handler())
	w := do(edge, "POST", "/logout", url.Values{}, withCookies(at, rt), withDevice)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("gotrue logout: %d", w.Code)
	}
	if len(gstore.owners()) != 0 {
		t.Fatalf("gotrue logout left the browser subscribed: %v", gstore.owners())
	}
}
