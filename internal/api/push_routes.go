package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/metrics"
	"github.com/ZacxDev/muster/internal/push"
)

// muster-native Web Push: the browser's subscribe flow (pushScript in
// internal/ui) and the account page's Notifications section.
//
// 🔴 THESE ROUTES USED TO BE THE ROUTER'S. The client has called
// /api/push/vapid-public-key and /api/push/subscribe since the carve, and
// muster answered 404 — the subscription table and the VAPID key lived with
// the permission router, which a standalone deployment does not have. They are
// muster's now (routes.partition.tsv), served from muster's own keypair and
// its own owner-scoped table (migration 0013).
//
// 🔴 EVERY ONE IS THE SESSION TIER, AND EVERY WRITE IS SAME-ORIGIN
// (requireOperatorWrite). A subscription is created FOR the signed-in user —
// the owner comes from the principal, never from the body — so no machine
// credential may create one, and a forged cross-origin POST could otherwise
// register an attacker's endpoint on the victim's account and receive their
// task titles.

const (
	pushKeyPath          = "/api/push/vapid-public-key"
	pushSubscribePath    = "/api/push/subscribe"
	pushUnsubscribePath  = "/api/push/unsubscribe"
	accountPushTestPath  = "/account/push/test"
	accountPushPrefsPath = "/account/push/preferences"

	// pushDeviceCookie names the browser's own subscription (the sha256 of its
	// endpoint), so logout can remove THIS device's row and no other. HttpOnly:
	// no script needs it. It is not a credential: it can only name a row its
	// holder's own session already owns.
	pushDeviceCookie = "muster_push_device"

	// maxSubscriptionBody bounds the subscribe body (a PushSubscription's JSON
	// is a few hundred bytes).
	maxSubscriptionBody = 8 << 10

	// pushTestTimeout bounds the account page's synchronous "send test".
	pushTestTimeout = 15 * time.Second
)

// PushService is muster's own Web Push (internal/push). Nil: no VAPID keys are
// configured — the key route answers 503, the subscribe routes refuse, and the
// shell draws no Enable button (Features.Push).
type PushService interface {
	PublicKey() string
	Store() push.Store
	SendTo(ctx context.Context, owner int64, p push.Payload) push.Report
	// The paired-device half (devices.go): the ONE allowlist (the store's),
	// for /api/device/config's push hosts and the check a registration passes
	// before its test push, and that test push.
	Allowlist() push.Allowlist
	Probe(ctx context.Context, sub push.Subscription, payload []byte) push.SendResult
}

func (s *Server) registerPushRoutes(mux Mux) {
	mux.HandleFunc("GET "+pushKeyPath, s.requireSession(s.handlePushKey))
	mux.HandleFunc("POST "+pushSubscribePath, s.requireOperatorWrite(s.handlePushSubscribe))
	mux.HandleFunc("POST "+pushUnsubscribePath, s.requireOperatorWrite(s.handlePushUnsubscribe))
	mux.HandleFunc("POST "+accountPushTestPath, s.requireOperatorWrite(s.handlePushTest))
	mux.HandleFunc("POST /account/push/{id}/remove", s.requireOperatorWrite(s.handlePushRemove))
	mux.HandleFunc("POST "+accountPushPrefsPath, s.requireOperatorWrite(s.handlePushPreferences))
}

// handlePushPreferences saves the caller's event toggles: every event NOT
// checked in the form is muted.
func (s *Server) handlePushPreferences(w http.ResponseWriter, r *http.Request) {
	if !s.pushServed() {
		http.Redirect(w, r, "/account?push=off#notifications", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	on := r.PostForm["on"]
	var muted []string
	for _, e := range push.Events {
		if !slices.Contains(on, e.Name) {
			muted = append(muted, e.Name)
		}
	}
	if err := s.ext.Push.Store().SetMuted(r.Context(), muted); err != nil {
		s.logger.Printf("push: save preferences: %v", err)
		http.Error(w, "the preferences could not be saved", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/account?push=saved#notifications", http.StatusSeeOther)
}

// pushServed reports whether this server can hold and send to subscriptions.
// It is the ONE predicate behind Features.Push and every handler's 503.
func (s *Server) pushServed() bool { return s.ext.Push != nil }

const pushUnconfigured = "Push notifications are not configured on this server (MUSTER_VAPID_* is unset)."

func (s *Server) handlePushKey(w http.ResponseWriter, r *http.Request) {
	if !s.pushServed() {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": pushUnconfigured})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, map[string]any{"key": s.ext.Push.PublicKey()})
}

func (s *Server) handlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	if !s.pushServed() {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": pushUnconfigured})
		return
	}
	var sub push.Subscription
	if err := json.NewDecoder(io.LimitReader(r.Body, maxSubscriptionBody)).Decode(&sub); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the body is not a PushSubscription"})
		return
	}
	owner := requestOwner(r.Context())
	prev, err := s.ext.Push.Store().Claim(r.Context(), owner, sub, r.UserAgent())
	switch {
	case errors.Is(err, push.ErrBadSubscription), errors.Is(err, push.ErrPushServiceNotAllowed):
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	case errors.Is(err, push.ErrTooManyDevices), errors.Is(err, push.ErrNativeEndpoint):
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	case err != nil:
		s.logger.Printf("push: subscribe %s for owner=%d: %s", push.Short(sub.Endpoint), owner, push.Redact(err, sub.Endpoint))
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the subscription could not be saved"})
		return
	}
	if prev != 0 {
		// The shared-device case: this browser was another user's device.
		s.logger.Printf("push: device %s moved from owner=%d to owner=%d (a different user signed in on it)",
			push.Short(sub.Endpoint), prev, owner)
	}
	s.setPushDeviceCookie(w, push.EndpointHash(sub.Endpoint))
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handlePushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	if !s.pushServed() {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": pushUnconfigured})
		return
	}
	var body struct {
		Endpoint string `json:"endpoint"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxSubscriptionBody)).Decode(&body); err != nil || body.Endpoint == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the body names no endpoint"})
		return
	}
	// Row-level security: only the caller's own row can match.
	removed, err := s.ext.Push.Store().Unsubscribe(r.Context(), body.Endpoint)
	if err != nil {
		s.logger.Printf("push: unsubscribe %s: %s", push.Short(body.Endpoint), push.Redact(err, body.Endpoint))
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the subscription could not be removed"})
		return
	}
	s.clearPushDeviceCookie(w)
	s.writeJSON(w, http.StatusOK, map[string]any{"removed": removed})
}

// handlePushTest sends one test notification to every device the caller holds
// and reports how many accepted it.
func (s *Server) handlePushTest(w http.ResponseWriter, r *http.Request) {
	if !s.pushServed() {
		http.Redirect(w, r, "/account?push=off#notifications", http.StatusSeeOther)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), pushTestTimeout)
	defer cancel()
	rep := s.sendPush(ctx, requestOwner(r.Context()), push.Payload{
		Type:  "notify",
		Title: "muster test notification",
		Body:  "Notifications reach this device.",
		Tag:   "muster-test",
		Data:  map[string]string{"url": "/account#notifications"},
	})
	http.Redirect(w, r, fmt.Sprintf("/account?push=test&ok=%d&of=%d#notifications", rep.Delivered, rep.Devices),
		http.StatusSeeOther)
}

func (s *Server) handlePushRemove(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 || !s.pushServed() {
		http.Error(w, "no such device", http.StatusNotFound)
		return
	}
	found, err := s.ext.Push.Store().Remove(r.Context(), id)
	if err != nil {
		s.logger.Printf("push: remove device %d: %v", id, err)
		http.Error(w, "the device could not be removed", http.StatusInternalServerError)
		return
	}
	if !found {
		http.Error(w, "no such device", http.StatusNotFound)
		return
	}
	http.Redirect(w, r, "/account?push=removed#notifications", http.StatusSeeOther)
}

// Per-owner rate cap. "Every event on by default" (operator decision) is the
// amplification risk: the router this replaces once sent 14–17k pushes a day
// to one device. The debounce coalesces bursts; this caps what is left, per
// owner, so one user's runaway producer cannot buzz their phone without bound
// (and cannot spend another user's budget).
const (
	pushOwnerBurst  = 30
	pushOwnerWindow = 10 * time.Minute
)

// ownerRateLimit is a fixed window per owner.
type ownerRateLimit struct {
	mu   sync.Mutex
	wins map[int64]*ownerWindow
}

type ownerWindow struct {
	start  time.Time
	count  int
	warned bool
}

// allow reports whether owner may be sent one more notification at now, and
// records it. first is true the first time owner is refused in a window, so
// the refusal is logged once rather than per notification.
func (l *ownerRateLimit) allow(owner int64, now time.Time) (ok, first bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.wins == nil {
		l.wins = map[int64]*ownerWindow{}
	}
	w := l.wins[owner]
	if w == nil || now.Sub(w.start) >= pushOwnerWindow {
		w = &ownerWindow{start: now}
		l.wins[owner] = w
	}
	if w.count >= pushOwnerBurst {
		first = !w.warned
		w.warned = true
		return false, first
	}
	w.count++
	return true, false
}

// pushAllowed applies owner's preferences and the per-owner rate cap to one
// notification of event. A muted event is not sent; past the cap nothing is
// sent until the window rolls. "resolved" (which shows nothing and only clears
// what an earlier push showed) never counts against the cap — throttling the
// clear would strand exactly the notification the cap let through.
func (s *Server) pushAllowed(ctx context.Context, owner int64, event string) bool {
	muted, err := s.ext.Push.Store().Muted(ctx, owner)
	if err != nil {
		// Fail OPEN: preferences are a convenience, and a database blip must not
		// silence a notification the user did not ask to silence.
		s.logger.Printf("push: read preferences for owner=%d: %v", owner, err)
	}
	if slices.Contains(muted, event) {
		metrics.PushSuppressed.WithLabelValues("muted").Inc()
		return false
	}
	if event == push.EventResolved {
		return true
	}
	ok, first := s.pushLimit.allow(owner, s.now())
	if !ok {
		metrics.PushSuppressed.WithLabelValues("throttled").Inc()
		if first {
			s.logger.Printf("push: owner=%d reached the cap of %d notifications per %s; dropping until the window rolls",
				owner, pushOwnerBurst, pushOwnerWindow)
		}
	}
	return ok
}

// sendPush is the ONE caller of PushService.SendTo. Everything that delivers a
// notification comes through here, so "who receives it" is decided in one
// place: owner, and only owner.
func (s *Server) sendPush(ctx context.Context, owner int64, p push.Payload) push.Report {
	if !s.pushServed() || owner <= 0 {
		return push.Report{}
	}
	return s.ext.Push.SendTo(ctx, owner, p)
}

// forgetPushDevice removes the signing-out browser's own subscription (named by
// its device cookie) from uid's devices, and clears the cookie. Called by both
// logout paths: a device left subscribed after its user signed out would keep
// buzzing with their task titles for whoever picks it up next.
func (s *Server) forgetPushDevice(w http.ResponseWriter, r *http.Request, uid int64) {
	hash := cookieValue(r, pushDeviceCookie)
	if hash == "" {
		return
	}
	s.clearPushDeviceCookie(w)
	if !s.pushServed() || uid <= 0 {
		return
	}
	ok, err := s.ext.Push.Store().ForgetDevice(r.Context(), uid, hash)
	if err != nil {
		s.logger.Printf("push: forget device at logout for owner=%d: %v", uid, err)
		return
	}
	if ok {
		s.logger.Printf("push: device …#%s unsubscribed at logout (owner=%d)", hash[:min(8, len(hash))], uid)
	}
}

func (s *Server) setPushDeviceCookie(w http.ResponseWriter, hash string) {
	http.SetCookie(w, &http.Cookie{
		Name: pushDeviceCookie, Value: hash, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.auth.SecureCookies || s.auth.goTrueMode(),
		MaxAge: int((400 * 24 * time.Hour) / time.Second),
	})
}

func (s *Server) clearPushDeviceCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: pushDeviceCookie, Value: "", Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.auth.SecureCookies || s.auth.goTrueMode(), MaxAge: -1,
	})
}

// accountNotificationsSection renders the account page's Notifications
// section: the caller's devices (row-level security), "send test", remove.
func (s *Server) accountNotificationsSection(r *http.Request) string {
	var b strings.Builder
	b.WriteString(`<section id="notifications" data-testid="push-devices" class="flex flex-col gap-3 rounded-2xl border border-line bg-s1/70 px-5 py-6 ring-1 ring-line">
<h2 class="text-base font-semibold text-fg">Notifications</h2>
`)
	if !s.pushServed() {
		b.WriteString(`<p class="text-sm text-fg2">` + html.EscapeString(pushUnconfigured) + `</p></section>`)
		return b.String()
	}
	q := r.URL.Query()
	switch q.Get("push") {
	case "test":
		b.WriteString(`<p role="status" class="text-sm font-semibold text-fg">Test sent: ` +
			html.EscapeString(q.Get("ok")) + ` of ` + html.EscapeString(q.Get("of")) + ` device(s) accepted it.</p>`)
	case "removed":
		b.WriteString(`<p role="status" class="text-sm font-semibold text-fg">Device removed.</p>`)
	case "saved":
		b.WriteString(`<p role="status" class="text-sm font-semibold text-fg">Notification settings saved.</p>`)
	}
	b.WriteString(s.accountPushDeviceList(r, false))
	b.WriteString(s.pushPreferencesForm(r))
	b.WriteString(`</section>`)
	return b.String()
}

// accountPushDeviceList is the Notifications section's devices and its "Send a
// test notification" button. Account → Devices' live list re-renders it out of
// band (oob), so a phone's push registration shows here without a reload.
func (s *Server) accountPushDeviceList(r *http.Request, oob bool) string {
	var b strings.Builder
	b.WriteString(`<div id="push-device-list" class="flex flex-col gap-3"`)
	if oob {
		b.WriteString(` hx-swap-oob="true"`)
	}
	b.WriteString(`>`)
	devs, err := s.ext.Push.Store().Devices(r.Context())
	if err != nil {
		s.logger.Printf("push: list devices: %v", err)
		b.WriteString(problemNode("The device list could not be read."))
		b.WriteString(`</div>`)
		return b.String()
	}
	if len(devs) == 0 {
		b.WriteString(`<p class="text-sm text-fg2">No device receives notifications yet. Open muster on a phone or browser and tap <span class="font-semibold text-fg">Enable</span> in the header.</p>`)
	} else {
		b.WriteString(`<ul class="flex flex-col gap-2">`)
		for _, d := range devs {
			last := "never"
			if d.LastOKAt != nil {
				last = d.LastOKAt.UTC().Format("2006-01-02 15:04 UTC")
			}
			label := d.UserAgent
			if label == "" {
				label = "unknown browser"
			}
			b.WriteString(`<li data-testid="push-device" class="flex flex-col gap-1 rounded-md bg-bg px-3 py-2 ring-1 ring-inset ring-edge">
<span class="text-sm text-fg">` + html.EscapeString(truncate(label, 80)) + `</span>
<span class="text-xs text-fg2">` + html.EscapeString(push.Short(d.Sub.Endpoint)) + ` · last delivered: ` + html.EscapeString(last) +
				func() string {
					if d.FailCount > 0 {
						return ` · ` + strconv.Itoa(d.FailCount) + ` failed`
					}
					return ""
				}() + `</span>
<form method="POST" action="/account/push/` + strconv.FormatInt(d.ID, 10) + `/remove">
<button type="submit" class="press min-h-[44px] text-sm text-accent underline">Remove</button></form>
</li>`)
		}
		b.WriteString(`</ul>
<form method="POST" action="` + accountPushTestPath + `">
<button type="submit" class="press min-h-[44px] w-full rounded-md bg-bg px-3 py-2 text-base font-semibold text-fg ring-1 ring-inset ring-edge">Send a test notification</button></form>`)
	}
	b.WriteString(`</div>`)
	return b.String()
}

// pushPreferencesForm is the caller's per-event toggles. Every event is on
// unless the caller muted it.
func (s *Server) pushPreferencesForm(r *http.Request) string {
	muted, err := s.ext.Push.Store().MyMuted(r.Context())
	if err != nil {
		s.logger.Printf("push: read preferences: %v", err)
		return problemNode("The notification settings could not be read.")
	}
	var b strings.Builder
	b.WriteString(`<form method="POST" action="` + accountPushPrefsPath + `" data-testid="push-preferences" class="flex flex-col gap-2">
<p class="text-sm font-medium text-fg">Notify me when</p>`)
	for _, e := range push.Events {
		checked := ""
		if !slices.Contains(muted, e.Name) {
			checked = " checked"
		}
		b.WriteString(`<label class="flex min-h-[44px] items-center gap-2 text-sm text-fg"><input type="checkbox" name="on" value="` +
			e.Name + `"` + checked + `> ` + html.EscapeString(e.Label) + `</label>`)
	}
	b.WriteString(`<button type="submit" class="press min-h-[44px] w-full rounded-md bg-bg px-3 py-2 text-base font-semibold text-fg ring-1 ring-inset ring-edge">Save notification settings</button></form>`)
	return b.String()
}
