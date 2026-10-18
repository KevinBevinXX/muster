package askq

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func gatePayload(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/pretooluse-askuserquestion.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type registration struct {
	auth string
	body map[string]any
}

// fakeMuster answers POST /mirror/ask with status (201 by default), after
// delay, recording each registration.
func fakeMuster(t *testing.T, status int, delay time.Duration) (*httptest.Server, func() []registration) {
	t.Helper()
	var mu sync.Mutex
	var got []registration
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/mirror/ask" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		got = append(got, registration{r.Header.Get("Authorization"), body})
		mu.Unlock()
		time.Sleep(delay)
		if status == 0 {
			status = http.StatusCreated
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []registration { mu.Lock(); defer mu.Unlock(); return append([]registration(nil), got...) }
}

func testHook(base string, timeout time.Duration) Hook {
	return Hook{Base: base, Path: "/mirror/ask", Auth: func(r *http.Request) { r.Header.Set("Authorization", "Bearer mmt_test") },
		Client: &http.Client{Timeout: timeout}}
}

// TestTheHookRegistersTheRealCallAndReturns: the captured PreToolUse payload is
// registered — its session, tool_use id and input, under an id the hook chose
// — on the host's token.
func TestTheHookRegistersTheRealCallAndReturns(t *testing.T) {
	srv, got := fakeMuster(t, 0, 0)
	if !testHook(srv.URL, time.Second).Run(context.Background(), gatePayload(t)) {
		t.Fatal("not registered")
	}
	regs := got()
	if len(regs) != 1 {
		t.Fatalf("%d registrations", len(regs))
	}
	r := regs[0]
	if r.auth != "Bearer mmt_test" || r.body["sessionId"] != "8c1d1892-a9c0-4d90-bfdf-3c56848ac6fa" || r.body["toolUseId"] != "toolu_01RY27FfKTitvDGupDC74z2s" {
		t.Fatalf("registration %+v", r)
	}
	if id, _ := r.body["id"].(string); !ValidID(id) {
		t.Fatalf("the hook's own id %q", r.body["id"])
	}
	in, _ := json.Marshal(r.body["toolInput"])
	if qs, err := ParseInput(in); err != nil || len(qs) != 2 {
		t.Fatalf("the input sent: %s (%v)", in, err)
	}
}

// TestTheHookNeverHoldsTheCall: muster slow or down, the hook returns within
// its client's timeout — the picker waits on the hook, and nothing waits on
// muster's answer.
func TestTheHookNeverHoldsTheCall(t *testing.T) {
	slow, _ := fakeMuster(t, 0, 5*time.Second)
	start := time.Now()
	if testHook(slow.URL, 200*time.Millisecond).Run(context.Background(), gatePayload(t)) {
		t.Fatal("a registration that timed out was taken for one that landed")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the hook held the call %s", d)
	}
	down := httptest.NewServer(http.NotFoundHandler())
	base := down.URL
	down.Close()
	if testHook(base, time.Second).Run(context.Background(), gatePayload(t)) {
		t.Fatal("muster down was taken for a registration")
	}
	refused, _ := fakeMuster(t, http.StatusConflict, 0)
	if testHook(refused.URL, time.Second).Run(context.Background(), gatePayload(t)) {
		t.Fatal("a refused registration was taken for one")
	}
}

func TestTheHookIgnoresOtherTools(t *testing.T) {
	srv, got := fakeMuster(t, 0, 0)
	p := strings.Replace(string(gatePayload(t)), `"tool_name":"AskUserQuestion"`, `"tool_name":"Bash"`, 1)
	if testHook(srv.URL, time.Second).Run(context.Background(), []byte(p)) || len(got()) != 0 {
		t.Fatal("a Bash call was registered")
	}
	p = strings.Replace(string(gatePayload(t)), `"hook_event_name":"PreToolUse"`, `"hook_event_name":"PostToolUse"`, 1)
	if testHook(srv.URL, time.Second).Run(context.Background(), []byte(p)) || len(got()) != 0 {
		t.Fatal("a PostToolUse payload was registered")
	}
}
