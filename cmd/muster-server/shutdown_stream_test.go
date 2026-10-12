package main

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"
)

// runForShutdown assembles the app as main() does and runs it on a real port.
// wrap, when non-nil, replaces the handler before the listener opens — it is how
// a test adds a route of its own in front of the real one. cancel begins the
// shutdown; runErr yields Run's result.
func runForShutdown(t *testing.T, cfg config, wrap func(http.Handler) http.Handler) (base string, cancel context.CancelFunc, runErr chan error) {
	t.Helper()
	app, err := buildApp(context.Background(), cfg, quietLogger())
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	t.Cleanup(app.Close)
	if wrap != nil {
		app.http.Handler = wrap(app.http.Handler)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	addrCh := make(chan net.Addr, 1)
	runErr = make(chan error, 1)
	go func() { runErr <- app.Run(ctx, func(a net.Addr) { addrCh <- a }) }()
	select {
	case a := <-addrCh:
		base = "http://" + a.String()
	case err := <-runErr:
		t.Fatalf("the server returned before it was ready: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the server never reported a bound address")
	}
	return base, cancel, runErr
}

// openEventStream signs in and opens /events, returning the response once the
// stream is PROVEN open.
//
// 🔴 POSITIVE CONTROL: a refused /events (401, 503) closes at once and would let
// a broken shutdown pass any test built on this.
func openEventStream(t *testing.T, base string, cfg config) *http.Response {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	login, err := http.NewRequest(http.MethodPost, base+"/login",
		strings.NewReader(url.Values{"password": {cfg.UIPassword}}.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	login.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	login.Header.Set("Origin", base)
	lr, err := client.Do(login)
	if err != nil {
		t.Fatalf("POST /login: %v", err)
	}
	lr.Body.Close()
	if lr.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /login = %d, want 303 (signed in)", lr.StatusCode)
	}

	resp, err := client.Get(base + "/events")
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if ct := resp.Header.Get("Content-Type"); resp.StatusCode != http.StatusOK || ct != "text/event-stream" {
		t.Fatalf("GET /events = %d %q, want 200 text/event-stream", resp.StatusCode, ct)
	}
	if line, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil || line != ": connected\n" {
		t.Fatalf("first stream line = %q, %v; want the open comment", line, err)
	}
	return resp
}

// TestShutdownDoesNotWaitOutAnOpenEventStream pins that a shutdown with an
// /events stream open ends the stream and returns promptly, rather than waiting
// out shutdownDrainTimeout.
//
// 🔴 EVERY PAGE OPENS /events, SO THIS WAS EVERY SHUTDOWN WITH A TAB OPEN.
// http.Server.Shutdown waits for active connections and an SSE stream is never
// idle, so before api.EndStreams was registered with RegisterOnShutdown the
// process sat out the whole drain budget and exited on the deadline. Red at
// `a75b09b` (Run returns only after the 10 s deadline), green after.
//
// It drives buildApp + Run on a real port — the same path main() takes — so it
// also fails if the RegisterOnShutdown wiring is removed while the api side
// stays correct.
//
// ⚠ THE BOUND IS HALF THE DRAIN BUDGET, NOT A TIGHT TIMING. A correct shutdown
// takes milliseconds and the broken one takes the full budget; anything in
// between under a loaded -race run is still a pass, which is the point.
func TestShutdownDoesNotWaitOutAnOpenEventStream(t *testing.T) {
	cfg := testConfig()
	base, cancel, runErr := runForShutdown(t, cfg, nil)
	openEventStream(t, base, cfg)

	start := time.Now()
	cancel()
	bound := shutdownDrainTimeout / 2
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned %v; a cancelled context is a clean shutdown", err)
		}
	case <-time.After(3 * shutdownDrainTimeout):
		t.Fatalf("Run had not returned %s after shutdown began", 3*shutdownDrainTimeout)
	}
	if took := time.Since(start); took >= bound {
		t.Fatalf("shutdown with an open /events stream took %s, want under %s "+
			"(shutdownDrainTimeout is %s): the stream held the drain open",
			took.Round(time.Millisecond), bound, shutdownDrainTimeout)
	}
}

// TestShutdownEndsStreamsButLetsAnOrdinaryRequestFinish pins the other half of
// api.EndStreams' claim: ONLY streams end. With /events open and an ordinary
// request in flight, shutdown must leave that request's context alone, wait for
// it, and return only after it has answered.
//
// 🔴 WITHOUT THIS THE "ONLY STREAMS" HALF WAS PINNED BY NOTHING. Replacing the
// registration with a BaseContext cancelled on shutdown ends streams just as
// promptly — every other test stayed green — while cancelling EVERY in-flight
// request, which turns the drain into an abort. Red on exactly that mutant
// ("its context was cancelled"), green at head.
//
// ⚠ NO SLEEP DECIDES THE OUTCOME. The slow route blocks until the test releases
// it, so load can only lengthen the run. Every wait is bounded, and the bounds
// sit well inside shutdownDrainTimeout so a stream that never ends (Run returns
// only on the deadline) fails here too rather than hanging.
func TestShutdownEndsStreamsButLetsAnOrdinaryRequestFinish(t *testing.T) {
	const slowPath = "/-/test/slow"
	entered := make(chan struct{})
	release := make(chan struct{})
	cfg := testConfig()
	base, cancel, runErr := runForShutdown(t, cfg, func(inner http.Handler) http.Handler {
		mux := http.NewServeMux()
		mux.HandleFunc("GET "+slowPath, func(w http.ResponseWriter, r *http.Request) {
			close(entered)
			select {
			case <-release:
				_, _ = io.WriteString(w, "finished")
			case <-r.Context().Done():
				http.Error(w, "cancelled", http.StatusServiceUnavailable)
			case <-time.After(3 * shutdownDrainTimeout):
				http.Error(w, "never released", http.StatusGatewayTimeout)
			}
		})
		mux.Handle("/", inner)
		return mux
	})
	openEventStream(t, base, cfg)

	type answer struct {
		status int
		body   string
		err    error
	}
	answered := make(chan answer, 1)
	go func() {
		resp, err := http.Get(base + slowPath)
		if err != nil {
			answered <- answer{err: err}
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		answered <- answer{status: resp.StatusCode, body: string(b)}
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the slow request never reached its handler")
	}

	cancel() // shutdown begins: one stream open, one ordinary request in flight

	// While the request is in flight the drain must HOLD: no answer (so its
	// context was not cancelled) and no return from Run.
	const hold = shutdownDrainTimeout / 5
	select {
	case a := <-answered:
		t.Fatalf("shutdown did not leave an ordinary in-flight request alone: it answered %d %q (err %v) "+
			"before it was released — its context was cancelled", a.status, a.body, a.err)
	case err := <-runErr:
		t.Fatalf("Run returned (%v) while an ordinary request was still in flight; the drain did not wait for it", err)
	case <-time.After(hold):
	}

	close(release)
	afterRelease := time.Now()
	select {
	case a := <-answered:
		if a.err != nil || a.status != http.StatusOK || a.body != "finished" {
			t.Fatalf("the in-flight request answered %d %q (err %v), want 200 \"finished\"", a.status, a.body, a.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the in-flight request never answered after it was released")
	}
	// hold + bound < shutdownDrainTimeout: Run returning inside it means the
	// drain completed because the request finished, not because the deadline hit.
	bound := shutdownDrainTimeout / 2
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned %v; a cancelled context is a clean shutdown", err)
		}
	case <-time.After(3 * shutdownDrainTimeout):
		t.Fatalf("Run had not returned %s after shutdown began", 3*shutdownDrainTimeout)
	}
	if took := time.Since(afterRelease); took >= bound {
		t.Fatalf("Run returned %s after the last ordinary request finished, want under %s: "+
			"something else (an open stream) held the drain to its deadline",
			took.Round(time.Millisecond), bound)
	}
}
