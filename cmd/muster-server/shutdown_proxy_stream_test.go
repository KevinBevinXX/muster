package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/metrics"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// The /connect/ half of the shutdown-stream contract (api.EndStreams): a proxied
// GET EVENT STREAM ends when shutdown begins, on either listener, and every other
// proxied exchange — an ordinary response, or a POST that streams its answer as
// text/event-stream — is drained like any request (connectors.endsAtShutdown).
//
// These drive buildApp + Run over a real database (a grant and a credential are
// rows) with a FAKE upstream on loopback. The connector policy is loosened the
// way the e2e build loosens it, and only for the test's own host.

const (
	proxyDrainHostSuffix = ".e2e.test"
	proxyDrainSecret     = "drain-test-credential-0123456789abcdef"
	// streamPadding follows the first event, pushing it past the redactor's
	// hold-back (the longest secret form, less one), so the client can SEE the
	// stream open: the proxy releases the LAST bytes it has only when more arrive.
	streamPadding = 4096
)

// proxyUpstream is the fake platform. GET /stream answers text/event-stream and
// holds it open; GET /slow answers application/json, and POST /turn answers
// text/event-stream (an LLM-style streamed turn) — each of those two sends part
// of its body and waits for release before finishing it. Every wait is bounded.
type proxyUpstream struct {
	srv       *httptest.Server
	host      string // the connector's allowlisted host, port included
	streams   chan struct{}
	slowIn    chan struct{}
	release   chan struct{}
	slowCtxOK atomic.Bool // the /slow request's context was live when released
	// POST /early (proxy_full_duplex_test.go): answers before reading its body.
	earlyFlushed chan struct{}
}

func newProxyUpstream(t *testing.T) *proxyUpstream {
	t.Helper()
	u := &proxyUpstream{
		streams: make(chan struct{}, 8),
		slowIn:  make(chan struct{}, 1),
		release: make(chan struct{}),

		earlyFlushed: make(chan struct{}, 1),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		// The open line FIRST, padding after: the redactor holds back the tail.
		_, _ = io.WriteString(w, ": open\n\n: "+strings.Repeat("x", streamPadding)+"\n\n")
		w.(http.Flusher).Flush()
		u.streams <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-time.After(3 * shutdownDrainTimeout):
		}
	})
	held := func(contentType, head, tail string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", contentType)
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, head)
			w.(http.Flusher).Flush()
			u.slowIn <- struct{}{}
			select {
			case <-u.release:
				u.slowCtxOK.Store(r.Context().Err() == nil)
				_, _ = io.WriteString(w, tail)
			case <-r.Context().Done():
			case <-time.After(3 * shutdownDrainTimeout):
			}
		}
	}
	mux.HandleFunc("GET /slow", held("application/json", slowHead, slowTail))
	mux.HandleFunc("POST /turn", held("text/event-stream", turnHead, turnTail))
	mux.HandleFunc("POST /early", u.answerBeforeReading)
	mux.HandleFunc("/echo", echoUpstream)
	u.srv = httptest.NewServer(mux)
	t.Cleanup(u.srv.Close)
	_, port, _ := net.SplitHostPort(u.srv.Listener.Addr().String())
	u.host = "upstream" + proxyDrainHostSuffix + ":" + port
	return u
}

// The held responses' two halves: the head is relayed before shutdown begins,
// the tail only after the upstream is released. Each head is padded past the
// redactor's hold-back so the proxy really has relayed part of the body.
var (
	slowHead = `{"part":"` + strings.Repeat("a", streamPadding) + `",`
	slowTail = `"rest":"finished"}`
	turnHead = "data: {\"delta\":\"" + strings.Repeat("t", streamPadding) + "\"}\n\n"
	turnTail = "data: {\"delta\":\"end of turn\"}\n\ndata: [DONE]\n\n"
)

// proxyShutdownApp builds the app with a connector store, seeds an agent with an
// APPROVED grant on a connector for the fake upstream, and returns the agent's
// token, the connector's name, and a store over the same database.
func proxyShutdownApp(t *testing.T, connectPort int, u *proxyUpstream) (a *app, token, connector string, store *connectors.PGStore) {
	t.Helper()
	prev := e2eConnectorPolicy
	e2eConnectorPolicy = &connectors.Policy{
		AllowLoopback: true,
		InsecureHTTP:  true,
		Resolve: func(_ context.Context, host string) ([]netip.Addr, error) {
			if strings.HasSuffix(host, proxyDrainHostSuffix) {
				return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
			}
			return nil, errors.New("test resolver: not a test host: " + host)
		},
	}
	t.Cleanup(func() { e2eConnectorPolicy = prev })

	cfg := testConfig()
	cfg.Database = dbtest.DSN(t)
	key := bytes.Repeat([]byte{7}, 32)
	cfg.ConnectorEncKey = key
	cfg.ConnectPort = connectPort
	a = buildForShutdown(t, cfg)

	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), time.Minute)
	defer cancel()
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	connector = "drain" + suffix
	token = "drain-agent-token-" + suffix + "-0123456789"
	store, err := connectors.NewPG(a.pool, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateConnector(ctx, connectors.NewConnector{
		Name: connector, Hosts: []string{u.host}, Scheme: connectors.SchemeBearer, Secret: proxyDrainSecret,
	}); err != nil {
		t.Fatalf("create connector: %v", err)
	}
	t.Cleanup(func() { _ = store.DeleteConnector(dbtest.AsOperator(context.Background()), connector) })
	var agentID int64
	if err := a.pool.QueryRow(ctx, `INSERT INTO agents (name, namespace, hooks_token) VALUES ($1, $2, $3) RETURNING id`,
		"drain-"+suffix, "ns-drain-"+suffix, token).Scan(&agentID); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	t.Cleanup(func() {
		_, _ = a.pool.Exec(dbtest.AsOperator(context.Background()), `DELETE FROM agents WHERE id=$1`, agentID)
	})
	g, _, err := store.RequestGrant(ctx, agentID, connector, "shutdown test")
	if err != nil {
		t.Fatalf("request grant: %v", err)
	}
	if _, err := store.DecideGrant(ctx, g.ID, true, "test-operator", ""); err != nil {
		t.Fatalf("approve grant: %v", err)
	}
	return a, token, connector, store
}

// httpPanics is muster's recovered-handler-panic count.
//
// 🔴 IT IS HOW "ENDED CLEANLY" IS OBSERVED, BECAUSE THE CLIENT CANNOT SEE THE
// DIFFERENCE. A stream ended by an upstream read ERROR makes ReverseProxy panic
// with http.ErrAbortHandler; api's recoverMiddleware catches that, logs a PANIC
// with a stack, counts it here, and lets the response finish — so the client
// reads io.EOF either way. MEASURED: with connectors.endOnShutdown's EOF mapping
// removed, every assertion on the client side stayed green.
func httpPanics() float64 { return testutil.ToFloat64(metrics.Panics.WithLabelValues("http")) }

func proxyGet(base, token, connector, host, path string) (*http.Response, error) {
	return proxyDo(http.MethodGet, base, token, connector, host, path)
}

func proxyDo(method, base, token, connector, host, path string) (*http.Response, error) {
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader(`{"stream":true}`)
	}
	req, err := http.NewRequest(method, base+"/connect/"+connector+"/"+host+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Muster-Token", token)
	return http.DefaultClient.Do(req)
}

// openProxiedStream opens /stream through the proxy at base and returns a
// channel that yields the stream's terminal read error once it ends.
//
// 🔴 POSITIVE CONTROL: the stream is proven open — 200, the event-stream type,
// the upstream entered, AND the first event read through the proxy — before
// any test relies on it. A refused or closed stream would let a broken shutdown
// pass.
func openProxiedStream(t *testing.T, base, token, connector string, u *proxyUpstream) <-chan error {
	t.Helper()
	resp, err := proxyGet(base, token, connector, u.host, "/stream")
	if err != nil {
		t.Fatalf("GET proxied stream at %s: %v", base, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if ct := resp.Header.Get("Content-Type"); resp.StatusCode != http.StatusOK || !strings.HasPrefix(ct, "text/event-stream") {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		t.Fatalf("proxied stream at %s = %d %q (%s), want 200 text/event-stream", base, resp.StatusCode, ct, b)
	}
	select {
	case <-u.streams:
	case <-time.After(10 * time.Second):
		t.Fatal("the upstream never saw the proxied stream")
	}
	br := bufio.NewReader(resp.Body)
	lineCh := make(chan string, 1)
	go func() { l, _ := br.ReadString('\n'); lineCh <- l }()
	select {
	case l := <-lineCh:
		if l != ": open\n" {
			t.Fatalf("first proxied stream line = %.40q, want the open comment", l)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the first event never came through the proxy")
	}
	ended := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, br)
		if err == nil {
			err = io.EOF // io.Copy reports a clean end as nil
		}
		ended <- err
	}()
	return ended
}

// TestShutdownEndsAProxiedEventStreamOnBothListeners pins that a text/event-stream
// relayed by /connect/ — on the main listener AND on the connect listener — ends
// when shutdown begins, so Run returns promptly instead of at the drain deadline,
// and that each stream ends CLEANLY — as EOF, not as a ReverseProxy abort (see
// httpPanics for why that is read from the panic counter).
//
// It is ALSO the proof that the connect listener's streams end: shutdown()
// ends streams by one call before either server drains, and with that call
// removed this test is red (the 10 s deadline) — there is no second mechanism
// left to cover for it.
//
// 🔴 BEFORE THE FIX ONE SUCH STREAM COST EVERY SHUTDOWN THE WHOLE DRAIN BUDGET.
// The proxy ran the upstream request on r.Context(), which Shutdown never
// cancels, and EndStreams reached only package api's own handlers. Red on
// `289502c` (Run returned only after the 10 s deadline), green after.
func TestShutdownEndsAProxiedEventStreamOnBothListeners(t *testing.T) {
	u := newProxyUpstream(t)
	connectPort := freePort(t)
	a, token, connector, store := proxyShutdownApp(t, connectPort, u)
	base, cancel, runErr := runBuiltForShutdown(t, a, nil)
	connectBase := "http://127.0.0.1:" + strconv.Itoa(connectPort)

	onMain := openProxiedStream(t, base, token, connector, u)
	onConnect := openProxiedStream(t, connectBase, token, connector, u)

	panicsBefore := httpPanics()
	start := time.Now()
	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned %v; a cancelled context is a clean shutdown", err)
		}
	case <-time.After(3 * shutdownDrainTimeout):
		t.Fatalf("Run had not returned %s after shutdown began", 3*shutdownDrainTimeout)
	}
	if took, bound := time.Since(start), shutdownDrainTimeout/2; took >= bound {
		t.Fatalf("shutdown with a proxied event stream open on each listener took %s, want under %s "+
			"(shutdownDrainTimeout is %s): a proxied stream held the drain open",
			took.Round(time.Millisecond), bound, shutdownDrainTimeout)
	}
	for name, ended := range map[string]<-chan error{"main": onMain, "connect": onConnect} {
		select {
		case err := <-ended:
			if err != io.EOF {
				t.Errorf("the proxied stream on the %s listener ended with %v, want a clean end (io.EOF)", name, err)
			}
		case <-time.After(10 * time.Second):
			t.Errorf("the proxied stream on the %s listener had not ended after Run returned", name)
		}
	}
	if d := httpPanics() - panicsBefore; d != 0 {
		t.Errorf("ending the proxied streams recorded %v handler panic(s), want 0: a stream ended at shutdown "+
			"must end as EOF, not as a ReverseProxy abort", d)
	}

	// The audit row of each stream says WHY it ended: an operator reading the
	// connector audit must not see a 200 that looks like a stream the upstream
	// closed. The rows are written before Serve returns, so Run's return orders
	// them before this read.
	ctx, cancelRead := context.WithTimeout(dbtest.AsOperator(context.Background()), 30*time.Second)
	defer cancelRead()
	rows, err := store.ListAudit(ctx, 500)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	var details []string
	for _, e := range rows {
		if e.Kind == "proxy" && e.Connector == connector && e.Path == "/stream" {
			details = append(details, e.Detail)
		}
	}
	const wantDetail = "event stream ended at server shutdown"
	if len(details) != 2 || details[0] != wantDetail || details[1] != wantDetail {
		t.Errorf("audit details for the two proxied streams = %q, want two rows of %q", details, wantDetail)
	}
}

// TestShutdownLetsAnOrdinaryProxiedRequestFinish pins the other half: a proxied
// response that is NOT an event stream is drained, not ended. With one in
// flight — its headers and part of its body already relayed — shutdown must hold
// until the upstream finishes, leave the upstream request's context live, and
// deliver the WHOLE body.
//
// 🔴 THE MUTANT IT EXISTS FOR: cancelling every proxied request at shutdown (an
// unconditional end, or an event-stream test that always says yes). That ends
// streams just as promptly, so the test above stays green, while truncating a
// git fetch mid-pack. Red on both, green at head; green at `289502c` too, where
// it is an invariant guard.
//
// ⚠ NO SLEEP DECIDES THE OUTCOME. The upstream blocks until the test releases
// it; every wait is bounded inside shutdownDrainTimeout.
func TestShutdownLetsAnOrdinaryProxiedRequestFinish(t *testing.T) {
	u := newProxyUpstream(t)
	a, token, connector, _ := proxyShutdownApp(t, 0, u)
	assertProxiedExchangeIsDrained(t, a, token, connector, u, http.MethodGet, "/slow", slowHead+slowTail)
}

// TestShutdownLetsAProxiedPOSTEventStreamFinish pins that the METHOD gates the
// end, not the media type alone: a POST answered with text/event-stream — an
// LLM call with stream:true, an MCP tool call — is one finite, unresumable
// answer, and it must be drained like any request.
//
// 🔴 RED ON `c19dc66`, which ended every proxied text/event-stream at
// shutdown: the turn was cut mid-body, and because the end is reported as
// io.EOF the client read a truncated answer as a COMPLETE one. Green after.
func TestShutdownLetsAProxiedPOSTEventStreamFinish(t *testing.T) {
	u := newProxyUpstream(t)
	a, token, connector, _ := proxyShutdownApp(t, 0, u)
	assertProxiedExchangeIsDrained(t, a, token, connector, u, http.MethodPost, "/turn", turnHead+turnTail)
}

// assertProxiedExchangeIsDrained runs the app, puts one held exchange in
// flight with part of its body relayed, begins shutdown, and asserts the drain
// holds, the upstream's context stays live, the WHOLE body arrives, and Run
// returns promptly once it has.
func assertProxiedExchangeIsDrained(t *testing.T, a *app, token, connector string, u *proxyUpstream, method, path, want string) {
	t.Helper()
	what := "the proxied " + method + " " + path
	base, cancel, runErr := runBuiltForShutdown(t, a, nil)

	type answer struct {
		status int
		body   string
		err    error
	}
	headers := make(chan struct{})
	answered := make(chan answer, 1)
	go func() {
		resp, err := proxyDo(method, base, token, connector, u.host, path)
		if err != nil {
			close(headers)
			answered <- answer{err: err}
			return
		}
		defer resp.Body.Close()
		close(headers) // the proxy has relayed the response head: ModifyResponse has run
		b, err := io.ReadAll(resp.Body)
		answered <- answer{status: resp.StatusCode, body: string(b), err: err}
	}()
	select {
	case <-u.slowIn:
	case <-time.After(10 * time.Second):
		t.Fatal(what + " never reached the upstream")
	}
	select {
	case <-headers:
	case <-time.After(10 * time.Second):
		t.Fatal("the proxy never relayed the head of " + what)
	}

	cancel() // shutdown begins with the held response mid-body

	const hold = shutdownDrainTimeout / 5
	select {
	case a := <-answered:
		t.Fatalf("shutdown did not leave %s alone: it ended (%d, %d bytes, err %v) "+
			"before the upstream finished", what, a.status, len(a.body), a.err)
	case err := <-runErr:
		t.Fatalf("Run returned (%v) while %s was in flight; the drain did not wait for it", err, what)
	case <-time.After(hold):
	}

	close(u.release)
	afterRelease := time.Now()
	select {
	case a := <-answered:
		if a.err != nil || a.status != http.StatusOK || a.body != want {
			t.Fatalf("%s answered %d with %d bytes (err %v), want 200 and the whole %d-byte body",
				what, a.status, len(a.body), a.err, len(want))
		}
	case <-time.After(10 * time.Second):
		t.Fatal(what + " never answered after the upstream was released")
	}
	if !u.slowCtxOK.Load() {
		t.Fatal("the upstream request's context was cancelled by the time it was released")
	}
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned %v; a cancelled context is a clean shutdown", err)
		}
	case <-time.After(3 * shutdownDrainTimeout):
		t.Fatalf("Run had not returned %s after shutdown began", 3*shutdownDrainTimeout)
	}
	if took, bound := time.Since(afterRelease), shutdownDrainTimeout/2; took >= bound {
		t.Fatalf("Run returned %s after %s finished, want under %s",
			took.Round(time.Millisecond), what, bound)
	}
}
