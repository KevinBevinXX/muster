package main

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"
)

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
	app, err := buildApp(context.Background(), cfg, quietLogger())
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	t.Cleanup(app.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addrCh := make(chan net.Addr, 1)
	runErr := make(chan error, 1)
	go func() { runErr <- app.Run(ctx, func(a net.Addr) { addrCh <- a }) }()
	var base string
	select {
	case a := <-addrCh:
		base = "http://" + a.String()
	case err := <-runErr:
		t.Fatalf("the server returned before it was ready: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the server never reported a bound address")
	}

	// Sign in, so /events answers a stream rather than a refusal.
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
	defer resp.Body.Close()
	// 🔴 POSITIVE CONTROL: the stream is really open. A refused /events (401,
	// 503) closes at once and would let a broken shutdown pass this test.
	if ct := resp.Header.Get("Content-Type"); resp.StatusCode != http.StatusOK || ct != "text/event-stream" {
		t.Fatalf("GET /events = %d %q, want 200 text/event-stream", resp.StatusCode, ct)
	}
	br := bufio.NewReader(resp.Body)
	if line, err := br.ReadString('\n'); err != nil || line != ": connected\n" {
		t.Fatalf("first stream line = %q, %v; want the open comment", line, err)
	}

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
