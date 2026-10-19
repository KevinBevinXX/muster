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

// gatedBody yields first, then blocks until open is closed, then yields rest
// in small pieces a few milliseconds apart (cmd/muster-server's
// proxy_full_duplex_test.go measured why: released whole, the transport can read
// the tail, EOF included, before ccd's head write takes the body, and a broken
// proxy passes).
type gatedBody struct {
	first, rest []byte
	open        <-chan struct{}
	stage       int
}

func (g *gatedBody) Read(p []byte) (int, error) {
	switch g.stage {
	case 0:
		g.stage = 1
		return copy(p, g.first), nil
	case 1:
		select {
		case <-g.open:
		case <-time.After(30 * time.Second):
			return 0, errors.New("gatedBody: never opened")
		}
		g.stage = 2
		fallthrough
	case 2:
		if len(p) > 4<<10 {
			p = p[:4<<10]
		}
		time.Sleep(10 * time.Millisecond)
		n := copy(p, g.rest)
		g.rest = g.rest[n:]
		if len(g.rest) == 0 {
			g.stage = 3
		}
		return n, nil
	}
	return 0, io.EOF
}

// 🔴 TestAGatewayTurnIsNotTornWhenTheRuntimeAnswersBeforeTheBodyEnds pins
// handleGatewayTurn's EnableFullDuplex — the cause of
// TestAGatewayTurnIsHandedToTheRuntimeBesideCcd's CI flake ("ReverseProxy read
// error during body copy: … use of closed network connection", then the caller's
// "unexpected EOF").
//
// net/http's HTTP/1 server, on writing a response head, consumes what is left
// of the request body and CLOSES it unless the handler enabled full duplex. ccd
// writes the head as soon as the runtime's arrives, while the transport handing
// the body to the runtime may still owe a read of it — at least its trailing EOF
// read, which comes AFTER the body is on the wire (the transport flushes the
// head and writes the body straight to the socket). That read fails ("invalid
// Read on closed Body"), the transport's write loop closes the runtime
// connection, and the reply stream is cut mid-way. In CI it took a scheduling
// gap between those two reads; here the order is FORCED: the runtime answers
// before it reads the body, and the body's tail is released only after that.
func TestAGatewayTurnIsNotTornWhenTheRuntimeAnswersBeforeTheBodyEnds(t *testing.T) {
	answered := make(chan struct{}, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.output_text.delta\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		answered <- struct{}{}
		n, err := io.Copy(io.Discard, r.Body)
		fmt.Fprintf(w, "event: done\ndata: received=%d err=%v\n\n", n, err)
	}))
	t.Cleanup(up.Close)
	bearer, _ := resolveBearer(knownHooksToken, "")
	srv := newServer(serverConfig{Bearer: bearer, ConfigDir: t.TempDir()}, nil, newAuthTracker())
	p, err := newGatewayProxy(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	srv.gateway = p
	gw := gatewayServer(t, srv)

	const size = 64 << 10
	payload := []byte(strings.Repeat("p", size))
	open := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(open) }) })
	go func() {
		select {
		case <-answered:
		case <-time.After(10 * time.Second):
		}
		// Long enough for ccd to have received the runtime's head and begun
		// writing its own; a shorter wait can only make this pass on a broken
		// ccd, never fail on a fixed one.
		time.Sleep(300 * time.Millisecond)
		once.Do(func() { close(open) })
	}()
	req, _ := http.NewRequest(http.MethodPost, gw.URL+"/v1/responses",
		&gatedBody{first: payload[:1024], rest: payload[1024:], open: open})
	req.ContentLength = size
	req.Header.Set(headerRuntime, runtimeGateway)
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("a gateway turn: %v", err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	want := fmt.Sprintf("data: received=%d err=<nil>", size)
	if err != nil || resp.StatusCode != http.StatusOK || !strings.Contains(string(got), want) {
		t.Fatalf("the gateway turn answered %d, %q (err %v); want 200 ending %q: ccd tore the exchange",
			resp.StatusCode, got, err, want)
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
