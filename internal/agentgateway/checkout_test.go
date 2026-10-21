package agentgateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// TestCheckoutReadsTheRepoStateFromHealthz: Gateway.Checkout GETs the agent's
// /healthz and returns its `repo` — including from a 503 (an unhealthy session
// still reports its checkout) — answers "unknown" with no error for a pod with
// no repository, and errors on anything it cannot read.
func TestCheckoutReadsTheRepoStateFromHealthz(t *testing.T) {
	for _, c := range []struct {
		name    string
		status  int
		body    string
		want    agents.CheckoutStatus
		wantErr string
	}{
		{"awaiting grant", 200, `{"ok":true,"repo":{"repo":"example-org/shoal","dir":"/data/workspace","state":"awaiting_grant","detail":"approve it"}}`,
			agents.CheckoutStatus{State: agents.CheckoutAwaitingGrant, Detail: "approve it"}, ""},
		{"ready", 200, `{"ok":true,"repo":{"state":"ready"}}`, agents.CheckoutStatus{State: agents.CheckoutReady}, ""},
		{"unhealthy but cloning", 503, `{"ok":false,"repo":{"state":"cloning"}}`, agents.CheckoutStatus{State: agents.CheckoutCloning}, ""},
		{"no repository", 200, `{"ok":true,"terminal":"ok"}`, agents.CheckoutStatus{}, ""},
		{"not json", 200, `<html>`, agents.CheckoutStatus{}, "/healthz body"},
		{"not found", 404, `nope`, agents.CheckoutStatus{}, "answered 404"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var paths []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.Method+" "+r.URL.Path)
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()
			gw := newGateway(t, srv)
			target, err := gw.Resolve(context.Background(), agents.Agent{ID: 3, Name: "shoal-agent", HooksToken: "tok-shoal"})
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			got, err := gw.Checkout(context.Background(), target)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, c.wantErr)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("Checkout = %+v, %v; want %+v", got, err, c.want)
			}
			if len(paths) != 1 || paths[0] != "GET /healthz" {
				t.Fatalf("requests = %v, want one GET /healthz", paths)
			}
		})
	}
	// 🔴 A redirect is answered as what it is, never followed: the target of it
	// says "ready" and must not be read.
	var followed bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followed = true
		_, _ = w.Write([]byte(`{"ok":true,"repo":{"state":"ready"}}`))
	}))
	defer elsewhere.Close()
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/healthz", http.StatusFound)
	}))
	defer redirecting.Close()
	gw := newGateway(t, redirecting)
	target, err := gw.Resolve(context.Background(), agents.Agent{ID: 4, Name: "shoal-agent", HooksToken: "tok-shoal"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got, err := gw.Checkout(context.Background(), target); err == nil || !strings.Contains(err.Error(), "answered 302") || followed {
		t.Fatalf("a redirecting /healthz: Checkout = %+v, %v (followed=%t); want an error naming the 302, not followed", got, err, followed)
	}

	// A Target that was never resolved names nothing and is refused, not dialled.
	if _, err := (&Gateway{client: http.DefaultClient}).Checkout(context.Background(), Target{}); err == nil {
		t.Fatal("a zero Target was read")
	}
}
