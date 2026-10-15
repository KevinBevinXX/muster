package agentgateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// toolTurnRuntime answers request 1 of a tool turn with one function_call and
// request 2 with whatever second does. first, when set, replaces request 1's answer.
type toolTurnRuntime struct {
	mu       sync.Mutex
	requests int
	first    func(http.ResponseWriter)
	second   func(http.ResponseWriter)
}

func (f *toolTurnRuntime) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	f.requests++
	n := f.requests
	f.mu.Unlock()
	if n == 1 && f.first != nil {
		f.first(w)
		return
	}
	if n == 1 {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":["+
			"{\"type\":\"function_call\",\"call_id\":\"c1\",\"name\":\"probe\",\"arguments\":\"{}\"}]}}\n\n")
		return
	}
	f.second(w)
}

func typedRefusal(status int, typ string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"error":{"type":%q,"message":"refused"}}`, typ)
	}
}

// TestAToolTurnThatFailsAfterItsFirstRequestIsNeverUnsent pins finding 1 of
// round 1 at the gateway: a tool turn whose request 1 was answered with a
// function_call that THIS process dispatched, and whose request 2 then failed in
// a shape that on a first request means "nothing was sent" — a typed 409 busy, a
// typed 503 not_ready, a dial error after the runtime went away, a 404 — must NOT
// read as unsent, or a deliverer re-queues it and runs the tools again. The
// control: the same typed 409 on request 1 IS unsent.
func TestAToolTurnThatFailsAfterItsFirstRequestIsNeverUnsent(t *testing.T) {
	for _, c := range []struct {
		name   string
		second func(http.ResponseWriter)
		close  bool // close the runtime between request 1 and 2 (a dial error)
	}{
		{name: "typed 409 busy", second: typedRefusal(http.StatusConflict, "busy")},
		{name: "typed 503 not_ready", second: typedRefusal(http.StatusServiceUnavailable, "not_ready")},
		{name: "typed 401", second: typedRefusal(http.StatusUnauthorized, "unauthorized")},
		{name: "404", second: func(w http.ResponseWriter) { http.NotFound(w, nil) }},
		{name: "dial error", close: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			rt := &toolTurnRuntime{second: c.second}
			srv := httptest.NewServer(rt)
			defer srv.Close()
			g, err := New(Config{Driver: &fixedResolver{ep: endpointOf(t, srv.URL)}, Runtime: HooksSHA256(),
				Model: testSentinel, Client: srv.Client()})
			if err != nil {
				t.Fatal(err)
			}
			dispatched := 0
			dispatch := func(string, string) string {
				dispatched++
				if c.close {
					srv.Close()
				}
				return `{"ok":true}`
			}
			_, err = g.ChatWithTools(context.Background(), fixtureAgent(), "s", "", "hi",
				[]agents.ToolDef{{Type: "function", Name: "probe"}}, dispatch, nil)
			if err == nil || dispatched != 1 {
				t.Fatalf("err=%v dispatched=%d; want a failure after one dispatched call", err, dispatched)
			}
			if c.close {
				var op *net.OpError
				if !errors.As(err, &op) || op.Op != "dial" {
					t.Fatalf("the closed runtime did not produce a dial error: %v", err)
				}
			}
			if got := Unsent(err); got != "" {
				t.Fatalf("request 2 failed (%v) after request 1's tool call ran: Unsent = %q; want \"\" — "+
					"a tool turn past its first request was sent", err, got)
			}
		})
	}
	// Control: the same typed refusal on request 1 is unsent.
	rt := &toolTurnRuntime{first: typedRefusal(http.StatusConflict, "busy")}
	srv := httptest.NewServer(rt)
	defer srv.Close()
	g, err := New(Config{Driver: &fixedResolver{ep: endpointOf(t, srv.URL)}, Runtime: HooksSHA256(),
		Model: testSentinel, Client: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = g.ChatWithTools(context.Background(), fixtureAgent(), "s", "", "hi", nil, nil, nil)
	if got := Unsent(err); got != UnsentBusy {
		t.Fatalf("a typed 409 on request 1: Unsent(%v) = %q; want %q", err, got, UnsentBusy)
	}
}
