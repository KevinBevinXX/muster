package agentgateway

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// 🔴 TestTheGatewaySendsAnAgentsOwnTurnCredential: the bearer muster sends a
// turn with is the agent's own GatewaySecret when the row has one — the value its
// claude-code spec ships ccd — and the hooks-sha256 derivation only when it has
// none (every other kind, and a claude-code row whose pod predates the secret).
// Sender and spec builder share agents.TurnBearer; this pins the sender's side.
func TestTheGatewaySendsAnAgentsOwnTurnCredential(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	gw, err := New(Config{Driver: &fixedResolver{ep: endpointOf(t, srv.URL)}, Runtime: HooksSHA256(),
		Model: testSentinel, Client: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	const secret = "fixture-own-turn-credential-91af"
	own := fixtureAgent()
	own.GatewaySecret = secret
	legacy := fixtureAgent()
	for _, ag := range []struct {
		name string
		want string
	}{{"a row with its own secret", "Bearer " + secret},
		{"a row without one", "Bearer " + HooksSHA256().Bearer(legacy.HooksToken)}} {
		a := legacy
		if ag.name == "a row with its own secret" {
			a = own
		}
		if _, err := gw.Chat(context.Background(), a, "s", "hi", nil); err != nil {
			t.Fatalf("%s: %v", ag.name, err)
		}
		mu.Lock()
		got := seen[len(seen)-1]
		mu.Unlock()
		if got != ag.want {
			t.Errorf("%s: sent a bearer other than %s", ag.name, map[bool]string{true: "its own secret",
				false: "the hooks-sha256 derivation"}[a.GatewaySecret != ""])
		}
	}
}
