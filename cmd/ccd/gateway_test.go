package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
)

// fakeGatewayRuntime is the gateway runtime beside ccd: it records each
// request and answers with the responses event stream.
type fakeGatewayRuntime struct {
	mu   sync.Mutex
	seen []*http.Request
	body []string
	hold chan struct{} // when set, the stream pauses after its first event until closed
}

func (f *fakeGatewayRuntime) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.seen = append(f.seen, r.Clone(context.Background()))
	f.body = append(f.body, string(b))
	hold := f.hold
	f.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"from the gateway\"}\n\n")
	w.(http.Flusher).Flush()
	if hold != nil {
		<-hold
	}
	fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\","+
		"\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"from the gateway\"}]}]}}\n\n")
}

func (f *fakeGatewayRuntime) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.seen)
}

// gatewayRig is a ccd (no CLI behind it) with a gateway runtime beside it.
func gatewayRig(t *testing.T) (*fakeGatewayRuntime, *httptest.Server) {
	t.Helper()
	rt := &fakeGatewayRuntime{}
	up := httptest.NewServer(rt)
	t.Cleanup(up.Close)
	bearer, _ := resolveBearer(knownHooksToken, "")
	srv := newServer(serverConfig{Bearer: bearer, ConfigDir: t.TempDir()}, nil, newAuthTracker())
	p, err := newGatewayProxy(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	srv.gateway = p
	return rt, gatewayServer(t, srv)
}

func gatewayRoute(model string) context.Context {
	h := http.Header{}
	h.Set(agents.HeaderRuntime, agents.RuntimeGateway)
	h.Set(agents.HeaderGatewayModel, model)
	return agents.WithTurnRoute(context.Background(), agents.TurnRoute{Runtime: agents.SessionRuntimeGateway, Header: h})
}

// TestAGatewayTurnIsHandedToTheRuntimeBesideCcd: through muster's own client, a
// gateway-runtime turn WITH TOOLS reaches the runtime beside ccd — the bearer it
// arrived with, the session key, the model override and the tools intact,
// ccd's own headers dropped — and its answer comes back. ccd's CLI is never
// touched (this ccd has none).
func TestAGatewayTurnIsHandedToTheRuntimeBesideCcd(t *testing.T) {
	rt, gw := gatewayRig(t)
	tools := []agents.ToolDef{{Type: "function", Name: "fleet_list", Description: "x", Parameters: map[string]any{"type": "object"}}}
	reply, err := mustersGateway(t, gw.URL).ChatWithTools(gatewayRoute("openrouter/vendor-x/model-y"), contractAgent,
		"agent:chief:webchat:7", "chief instructions", "hello", tools, func(string, string) string { return "" }, nil)
	if err != nil || reply != "from the gateway" {
		t.Fatalf("the gateway turn: %q, %v", reply, err)
	}
	if rt.count() != 1 {
		t.Fatalf("the runtime saw %d requests, want 1", rt.count())
	}
	r, body := rt.seen[0], rt.body[0]
	bearer, _ := resolveBearer(knownHooksToken, "")
	for name, want := range map[string]string{
		"Authorization": "Bearer " + bearer, "X-Openclaw-Session-Key": "agent:chief:webchat:7",
		agents.HeaderGatewayModel: "openrouter/vendor-x/model-y", agents.HeaderRuntime: "",
	} {
		if got := r.Header.Get(name); got != want {
			t.Errorf("the runtime got %s = %q, want %q", name, got, want)
		}
	}
	var req struct {
		Instructions string `json:"instructions"`
		Tools        []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if r.URL.Path != "/v1/responses" || json.Unmarshal([]byte(body), &req) != nil || req.Instructions != "chief instructions" ||
		len(req.Tools) != 1 || req.Tools[0].Name != "fleet_list" {
		t.Errorf("the runtime got %s %s; want /v1/responses with the instructions and the tool", r.URL.Path, body)
	}
}

// TestAGatewayTurnIsRefusedBeforeTheRuntimeWithoutTheBearer, and is not_ready
// on a pod with no runtime beside ccd.
func TestAGatewayTurnIsRefusedBeforeTheRuntimeWithoutTheBearer(t *testing.T) {
	rt, gw := gatewayRig(t)
	req, _ := http.NewRequest(http.MethodPost, gw.URL+"/v1/responses", strings.NewReader(`{"input":"x"}`))
	req.Header.Set(headerRuntime, runtimeGateway)
	req.Header.Set("Authorization", "Bearer wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || rt.count() != 0 {
		t.Fatalf("a wrong bearer: %d, the runtime saw %d requests; want 401 and none", resp.StatusCode, rt.count())
	}

	bearer, _ := resolveBearer(knownHooksToken, "")
	plain := gatewayServer(t, newServer(serverConfig{Bearer: bearer}, nil, newAuthTracker()))
	_, err = mustersGateway(t, plain.URL).Chat(gatewayRoute("openrouter/vendor-x/model-y"), contractAgent, "k", "hi", nil)
	var re *agents.RuntimeError
	if !errors.As(err, &re) || re.Type != failNotReady {
		t.Fatalf("a gateway turn to a pod with none beside ccd: %v; want %s", err, failNotReady)
	}
}

// TestTheGatewayStreamIsPassedOnAsItComes: the first event reaches the caller
// while the runtime is still answering.
//
// ⚠ AN INVARIANT GUARD, NOT A GUARD ON FlushInterval: net/http/httputil's
// ReverseProxy flushes a text/event-stream response as it comes whatever
// FlushInterval says (the mutant that removes it survives). It pins the
// observable a caller depends on, so a change of proxy cannot lose it unseen.
func TestTheGatewayStreamIsPassedOnAsItComes(t *testing.T) {
	rt, gw := gatewayRig(t)
	rt.hold = make(chan struct{})
	defer close(rt.hold)
	bearer, _ := resolveBearer(knownHooksToken, "")
	req, _ := http.NewRequest(http.MethodPost, gw.URL+"/v1/responses", strings.NewReader(`{"input":"x","stream":true}`))
	req.Header.Set(headerRuntime, runtimeGateway)
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(resp.Body).ReadString('\n')
		got <- line
	}()
	select {
	case line := <-got:
		if !strings.Contains(line, "response.output_text.delta") {
			t.Fatalf("first line %q", line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the first event did not arrive while the runtime held the rest: ccd buffered the stream")
	}
}

// TestTheChatCompletionsDoorIsTheGatewaysAlone: without the runtime header it is
// the 404 it always was; with it, the request is handed over.
func TestTheChatCompletionsDoorIsTheGatewaysAlone(t *testing.T) {
	rt, gw := gatewayRig(t)
	bearer, _ := resolveBearer(knownHooksToken, "")
	post := func(gateway bool) int {
		req, _ := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+bearer)
		if gateway {
			req.Header.Set(headerRuntime, runtimeGateway)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post(false); code != http.StatusNotFound || rt.count() != 0 {
		t.Errorf("chat completions without the header: %d (runtime saw %d); want 404", code, rt.count())
	}
	if code := post(true); code != http.StatusOK || rt.count() != 1 {
		t.Errorf("chat completions for the gateway: %d (runtime saw %d); want it handed over", code, rt.count())
	}
}

// TestTheGatewayUpstreamIsLoopbackOnly.
func TestTheGatewayUpstreamIsLoopbackOnly(t *testing.T) {
	for _, ok := range []string{"http://127.0.0.1:18791", "http://localhost:18791/", "http://[::1]:18791"} {
		if _, err := newGatewayProxy(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"http://198.51.100.7:18791", "http://gateway.example.test:18791", "https://127.0.0.1:18791",
		"127.0.0.1:18791", "http://127.0.0.1:18791/v1", ""} {
		if _, err := newGatewayProxy(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

// TestTheGatewayHeadersAreMusters: the header and value ccd routes by are the
// ones muster's transport sets.
func TestTheGatewayHeadersAreMusters(t *testing.T) {
	if headerRuntime != agents.HeaderRuntime || runtimeGateway != agents.RuntimeGateway {
		t.Fatalf("ccd reads %s: %s; muster sends %s: %s", headerRuntime, runtimeGateway, agents.HeaderRuntime, agents.RuntimeGateway)
	}
}
