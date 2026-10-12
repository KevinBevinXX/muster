package main

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	trickle    = 4 << 10
	trickleGap = 10 * time.Millisecond
)

// earlyHead is what POST /early sends before it reads a byte of its request
// body. Padded past the redactor's hold-back, so the proxy has relayed part of
// it — and therefore written the response head — before the body is done.
var earlyHead = strings.Repeat("h", streamPadding)

// answerBeforeReading answers the way a full-duplex upstream may (an HTTP/2
// server, a streaming API that starts its reply while the upload is still
// arriving): head and part of the body FIRST, then it reads the whole request
// body and reports how much arrived.
func (u *proxyUpstream) answerBeforeReading(w http.ResponseWriter, r *http.Request) {
	if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
		http.Error(w, "fake upstream: full duplex: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, earlyHead)
	w.(http.Flusher).Flush()
	select {
	case u.earlyFlushed <- struct{}{}:
	default:
	}
	n, err := io.Copy(io.Discard, r.Body)
	_, _ = fmt.Fprintf(w, "|received=%d|err=%v", n, err)
}

// echoUpstream reads the whole request body and then describes it, so a
// client can tell exactly what reached the upstream.
func echoUpstream(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "text/plain")
	_, _ = fmt.Fprintf(w, "method=%s len=%d sha=%x err=%v expect=%q", r.Method, len(b), sha256.Sum256(b), err, r.Header.Get("Expect"))
}

// gatedBody yields first, then blocks until open is closed, then yields rest
// in pieces of at most trickle bytes, trickleGap apart.
//
// ⚠ THE TRICKLE IS WHAT MAKES THE RED DETERMINISTIC. Released in one piece,
// the tail could be read whole by the transport, EOF included, before the
// proxy's head write took the body — measured green on a broken proxy in 1 run
// of 6. Released in pieces, the transport is between reads many times while
// the head write waits for the body, and the head write wins one of them.
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
			return 0, fmt.Errorf("gatedBody: never opened")
		}
		g.stage = 2
		fallthrough
	case 2:
		if len(p) > trickle {
			p = p[:trickle]
		}
		time.Sleep(trickleGap)
		n := copy(p, g.rest)
		g.rest = g.rest[n:]
		if len(g.rest) == 0 {
			g.stage = 3
		}
		return n, nil
	}
	return 0, io.EOF
}

// TestAProxiedPOSTIsNotTornWhenTheUpstreamAnswersBeforeTheBodyEnds pins the
// connector proxy's EnableFullDuplex.
//
// 🔴 WITHOUT IT A PROXIED POST WAS TORN AND DELIVERED AS A CLEAN, SHORT 200.
// net/http's HTTP/1 server, on writing a response head, consumes whatever is
// left of the request body and CLOSES it — unless the handler enabled full
// duplex. The proxy's handler writes the head as soon as the upstream's
// arrives, while the upstream transport may still be reading that same body to
// forward it. The transport's next read then fails ("invalid Read on closed
// Body"), its write loop closes the upstream connection, ReverseProxy aborts,
// and api's recoverMiddleware lets the response finish: the client reads a
// truncated body with no error, and the upstream a truncated request.
//
// The body here is GATED to force that order deterministically: its tail is
// released only after the upstream has answered, so the proxy's head write
// is waiting on the body while the transport still owes a read. Red without
// the call, green with it.
func TestAProxiedPOSTIsNotTornWhenTheUpstreamAnswersBeforeTheBodyEnds(t *testing.T) {
	u := newProxyUpstream(t)
	a, token, connector, _ := proxyShutdownApp(t, 0, u)
	base, _, _ := runBuiltForShutdown(t, a, nil)

	const size = 64 << 10
	payload := []byte(strings.Repeat("p", size))
	open := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(open) }) })
	go func() {
		select {
		case <-u.earlyFlushed:
		case <-time.After(10 * time.Second):
		}
		// Long enough for the proxy to have received the upstream's head and
		// begun writing its own; a shorter wait can only make this test pass
		// on a broken proxy, never fail on a fixed one.
		time.Sleep(300 * time.Millisecond)
		once.Do(func() { close(open) })
	}()
	req, err := http.NewRequest(http.MethodPost, base+"/connect/"+connector+"/"+u.host+"/early",
		&gatedBody{first: payload[:1024], rest: payload[1024:], open: open})
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = size
	req.Header.Set("X-Muster-Token", token)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST through the proxy: %v", err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	want := earlyHead + fmt.Sprintf("|received=%d|err=<nil>", size)
	if err != nil || resp.StatusCode != http.StatusOK || string(got) != want {
		tail := string(got)
		if len(tail) > 80 {
			tail = "…" + tail[len(tail)-80:]
		}
		t.Fatalf("proxied POST answered %d with %d bytes ending %q (err %v), want 200 and %d bytes ending %q: "+
			"the proxy tore the exchange", resp.StatusCode, len(got), tail, err, len(want), want[len(want)-30:])
	}
}

// TestFullDuplexLeavesEveryOrdinaryProxiedShapeWorking is the behaviour check
// for the proxy's EnableFullDuplex: it changes how the server treats the
// request body of EVERY proxied HTTP/1 request, so each ordinary shape must
// still deliver its whole body upstream and its whole answer back.
//
// ⚠ AN INVARIANT GUARD, NOT REGRESSION COVERAGE: every row was measured green
// with the call removed as well. What it pins is that the call broke none of
// them; the regression is the gated test above.
func TestFullDuplexLeavesEveryOrdinaryProxiedShapeWorking(t *testing.T) {
	u := newProxyUpstream(t)
	a, token, connector, _ := proxyShutdownApp(t, 0, u)
	base, _, _ := runBuiltForShutdown(t, a, nil)
	url := base + "/connect/" + connector + "/" + u.host + "/echo"

	plain := &http.Client{Timeout: 30 * time.Second}
	expecting := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{ExpectContinueTimeout: 5 * time.Second}}
	t.Cleanup(expecting.CloseIdleConnections)

	for _, c := range []struct {
		name    string
		method  string
		size    int  // body bytes; -1 none
		chunked bool // send with no Content-Length
		expect  bool // Expect: 100-continue
	}{
		{"GET, no body", http.MethodGet, -1, false, false},
		{"POST, empty body", http.MethodPost, 0, false, false},
		{"POST, small body", http.MethodPost, 15, false, false},
		{"POST, large body (over the server's 256 KiB discard limit)", http.MethodPost, 1 << 20, false, false},
		{"POST, chunked body", http.MethodPost, 100 << 10, true, false},
		{"POST, Expect: 100-continue", http.MethodPost, 64 << 10, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var payload []byte
			var body io.Reader
			if c.size >= 0 {
				payload = []byte(strings.Repeat("b", c.size))
				body = strings.NewReader(string(payload))
				if c.chunked {
					body = io.MultiReader(body) // hides the length: sent chunked
				}
			}
			req, err := http.NewRequest(c.method, url, body)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("X-Muster-Token", token)
			client := plain
			var got100 atomic.Bool
			if c.expect {
				req.Header.Set("Expect", "100-continue")
				client = expecting
				req = req.WithContext(httptrace.WithClientTrace(req.Context(),
					&httptrace.ClientTrace{Got100Continue: func() { got100.Store(true) }}))
			}
			if c.chunked && req.ContentLength != 0 {
				t.Fatalf("control: the chunked row has ContentLength %d; it would not be sent chunked", req.ContentLength)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("%s through the proxy: %v", c.method, err)
			}
			defer resp.Body.Close()
			got, err := io.ReadAll(resp.Body)
			want := fmt.Sprintf("method=%s len=%d sha=%x err=<nil> expect=", c.method, len(payload), sha256.Sum256(payload))
			if err != nil || resp.StatusCode != http.StatusOK || !strings.HasPrefix(string(got), want) {
				t.Fatalf("answered %d %q (err %v), want 200 starting %q", resp.StatusCode, got, err, want)
			}
			if c.expect && !got100.Load() {
				t.Fatal("control: the client never received 100 Continue, so the row did not exercise it")
			}
		})
	}
}
