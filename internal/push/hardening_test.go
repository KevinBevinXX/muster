package push

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// The push hardening for the Android app (docs/design/android.md, server
// change 21): a send never follows a redirect, and the operator's extra push
// hosts match by exact name only.

// TestAPushSendNeverFollowsARedirect: a push service that answers 307 to a
// second host is not followed — the second host is never reached — and the
// send is a failure, not a delivery and not an expiry.
//
// The send goes through the PRODUCTION client (NewWebpushPusher builds it with
// pushHTTPClient); only its transport is swapped for the test servers' (overTLS). Both servers share
// httptest's one certificate, so a client that followed the redirect WOULD
// reach the second: the positive control is that the second server counts a
// direct POST.
func TestAPushSendNeverFollowsARedirect(t *testing.T) {
	var reached atomic.Int32
	elsewhere := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	defer elsewhere.Close()
	for _, code := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		listed := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, elsewhere.URL+"/bounced", code)
		}))
		p := overTLS(t, NewWebpushPusher(realVAPID(t, "mailto:ops@example.com")), listed)
		res := p.Send(context.Background(), realSubscription(t, listed.URL+"/push/abc"), []byte(`{}`))
		listed.Close()
		if n := reached.Load(); n != 0 {
			t.Fatalf("status %d: the redirect was FOLLOWED: the second host received %d POST(s)", code, n)
		}
		if res.Err == nil || !strings.Contains(res.Err.Error(), "redirect") || res.StatusCode != code || res.Expired {
			t.Fatalf("status %d: send = %+v; want a redirect refusal, the 3xx status, not expired", code, res)
		}
	}
	// Positive control: the same client reaches the second host directly.
	p := overTLS(t, NewWebpushPusher(realVAPID(t, "mailto:ops@example.com")), elsewhere)
	if res := p.Send(context.Background(), realSubscription(t, elsewhere.URL+"/push/abc"), []byte(`{}`)); res.Err != nil || res.StatusCode != http.StatusCreated {
		t.Fatalf("CONTROL: a direct send = %+v", res)
	}
	if reached.Load() != 1 {
		t.Fatalf("CONTROL: the second host counted %d POSTs, want 1", reached.Load())
	}
}

// overTLS points p's own HTTP client at ts's transport (it trusts httptest's
// certificate), keeping everything else about the client — its redirect rule
// above all — as production built it.
func overTLS(t *testing.T, p *WebpushPusher, ts *httptest.Server) *WebpushPusher {
	t.Helper()
	c, ok := p.client.(*http.Client)
	if !ok {
		t.Fatalf("the pusher's client is a %T, not an *http.Client", p.client)
	}
	c.Transport = ts.Client().Transport
	return p
}

// TestARedirectedSendIsSettledAsFailed: through the Service, a redirect is a
// failed send — fail_count moves, the device is neither stamped delivered nor
// pruned.
func TestARedirectedSendIsSettledAsFailed(t *testing.T) {
	listed := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://elsewhere.invalid/x", http.StatusTemporaryRedirect)
	}))
	defer listed.Close()
	d := dev(1, 7, listed.URL+"/push/abc")
	d.Sub = realSubscription(t, d.Sub.Endpoint)
	store := &memStore{devs: []Device{d}}
	svc := NewService(store, overTLS(t, NewWebpushPusher(realVAPID(t, "mailto:ops@example.com")), listed),
		VAPIDConfig{}, log.New(io.Discard, "", 0))
	rep := svc.SendTo(context.Background(), 7, Payload{Type: "notify", Tag: "t"})
	if rep.Failed != 1 || rep.Delivered != 0 || rep.Expired != 0 {
		t.Fatalf("report = %+v; want one failed send", rep)
	}
	if len(store.failed) != 1 || len(store.delivered) != 0 || len(store.pruned) != 0 {
		t.Fatalf("store: failed %v delivered %v pruned %v", store.failed, store.delivered, store.pruned)
	}
}

// TestOperatorPushHostsMatchExactly: an operator host is admitted by its exact
// name — no subdomain, no suffix trick, no port, no http — and only when it is
// configured; the browser hosts are unchanged.
func TestOperatorPushHostsMatchExactly(t *testing.T) {
	hosts, err := ParseExtraHosts(" Ntfy.Example.org,push.example.net ")
	if err != nil {
		t.Fatal(err)
	}
	a := NewAllowlist(hosts)
	for ep, want := range map[string]bool{
		"https://ntfy.example.org/upABC?up=1":           true,
		"https://NTFY.example.org/upABC":                true,
		"https://push.example.net/upABC":                true,
		"https://a.ntfy.example.org/upABC":              false, // no suffix match
		"https://ntfy.example.org.evil.example/upABC":   false,
		"https://xntfy.example.org/upABC":               false,
		"https://example.org/upABC":                     false,
		"https://ntfy.example.org:8443/upABC":           false, // default port only
		"https://ntfy.example.org:443/upABC":            false, // an explicit port, even 443
		"http://ntfy.example.org/upABC":                 false, // https only
		"https://u:p@ntfy.example.org/upABC":            true,  // Check refuses userinfo before the allowlist
		"https://fcm.googleapis.com/fcm/send/x":         true,  // the browser hosts still pass
		"https://kubernetes.default.svc/api/v1/secrets": false,
	} {
		if got := a.Allows(ep); got != want {
			t.Errorf("Allows(%q) = %v, want %v", ep, got, want)
		}
	}
	// Unconfigured, an operator host is refused: nothing changes until the
	// operator sets the variable.
	if AllowedPushService("https://ntfy.example.org/upABC") || (Allowlist{}).Allows("https://ntfy.example.org/upABC") {
		t.Error("an operator host was admitted with no MUSTER_PUSH_EXTRA_HOSTS")
	}
	if got := a.ExtraHosts(); len(got) != 2 || got[0] != "ntfy.example.org" || got[1] != "push.example.net" {
		t.Errorf("ExtraHosts = %v", got)
	}
}

// TestExtraHostsMustBeBareNames: the variable takes exact DNS names only.
func TestExtraHostsMustBeBareNames(t *testing.T) {
	for _, v := range []string{
		"*.example.org", ".example.org", "https://ntfy.example.org", "ntfy.example.org:443",
		"ntfy.example.org/up", "192.0.2.5", "localhost", "ntfy..example.org", "-ntfy.example.org",
		"ntfy_example.org", "ntfy.example.org.", "[::1]",
	} {
		if _, err := ParseExtraHosts(v); !errors.Is(err, ErrBadExtraHost) {
			t.Errorf("ParseExtraHosts(%q) = %v; want ErrBadExtraHost", v, err)
		}
	}
	if h, err := ParseExtraHosts(""); err != nil || len(h) != 0 {
		t.Errorf("empty = %v, %v", h, err)
	}
	if h, err := ParseExtraHosts("ntfy.example.org ntfy.example.org"); err != nil || len(h) != 1 {
		t.Errorf("a repeat = %v, %v", h, err)
	}
}
