package connectors_test

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/connectors"
)

// countingEnd is a StreamsEnd that counts the registrations still live on it.
// context.AfterFunc uses a context's own AfterFunc method when it has one, so
// every registration the proxy makes passes through here, and every stop
// function it calls takes one off.
//
// ⚠ TWO SHORT CUTS IN THE context PACKAGE BYPASS THAT METHOD, AND EACH WAS
// MEASURED TO: a parent whose Done is nil (context.Background) registers
// nothing at all, and a parent the package recognises as one of its own cancel
// contexts (found through Value) is attached to directly. So the embedded
// context is a cancellable one, and Value answers nothing.
type countingEnd struct {
	context.Context
	live atomic.Int64
}

func newCountingEnd(t *testing.T) *countingEnd {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &countingEnd{Context: ctx}
}

func (c *countingEnd) Value(any) any { return nil }

func (c *countingEnd) AfterFunc(f func()) func() bool {
	c.live.Add(1)
	stop := context.AfterFunc(c.Context, f)
	var once sync.Once
	return func() bool {
		ok := stop()
		once.Do(func() { c.live.Add(-1) })
		return ok
	}
}

// TestAnAbortedEventStreamLeavesNothingRegisteredOnStreamsEnd pins that a
// proxied GET event stream which ends WITHOUT a shutdown — the client goes away
// (the normal end of an EventSource feed) or the upstream resets mid-body —
// unregisters itself from Proxy.StreamsEnd.
//
// 🔴 BOTH ENDS ARE ABORTS, AND AN ABORT SKIPPED A PLAIN CALL. Either one makes
// the body copy fail with an error that is not the shutdown's, so ReverseProxy
// panics with http.ErrAbortHandler and Serve unwinds past anything after
// rp.ServeHTTP. The stop was such a call, so each aborted stream left its
// registration on a context that lives as long as the process, one more per
// EventSource reconnect. Red at `941fd6a` (1 live registration after the
// handler returned, in both cases), green once the stop is deferred.
//
// ⚠ POSITIVE CONTROL: the count is read as 1 while the stream is open, so a
// zero afterwards cannot be an instrument that never saw a registration.
func TestAnAbortedEventStreamLeavesNothingRegisteredOnStreamsEnd(t *testing.T) {
	const pad = 4096 // past the redactor's hold-back, so the first line arrives
	for _, tc := range []struct {
		name string
		// How the stream ends once it is proven open: the client closes its
		// body, or else the upstream (told through reset) aborts its connection.
		clientCloses bool
	}{
		{name: "client disconnects", clientCloses: true},
		{name: "upstream resets", clientCloses: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reset := make(chan struct{})
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, ": open\n\n: "+strings.Repeat("x", pad)+"\n\n")
				w.(http.Flusher).Flush()
				select {
				case <-reset:
					panic(http.ErrAbortHandler) // drop the connection mid-body, no terminating chunk
				case <-r.Context().Done():
				case <-time.After(30 * time.Second):
				}
			}))
			t.Cleanup(up.Close)
			_, port, _ := net.SplitHostPort(up.Listener.Addr().String())
			host := "api.fake.test:" + port

			end := newCountingEnd(t)
			p := &connectors.Proxy{Store: approvedStub{c: connectors.Connector{Name: "svc", Hosts: []string{host},
				Scheme: connectors.SchemeBearer}}, Policy: testPolicy(), StreamsEnd: end}
			served := make(chan struct{})
			front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(served) // runs while an abort panic unwinds, too
				p.Serve(w, r, connectors.Caller{AgentID: 1, AgentName: "a"}, "svc", host, "stream")
			}))
			t.Cleanup(front.Close)

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, front.URL+"/connect/svc/"+host+"/stream", nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("GET proxied stream: %v", err)
			}
			defer resp.Body.Close()
			if ct := resp.Header.Get("Content-Type"); resp.StatusCode != http.StatusOK || ct != "text/event-stream" {
				t.Fatalf("proxied stream = %d %q, want 200 text/event-stream", resp.StatusCode, ct)
			}
			lineCh := make(chan string, 1)
			go func() { l, _ := bufio.NewReader(resp.Body).ReadString('\n'); lineCh <- l }()
			select {
			case l := <-lineCh:
				if l != ": open\n" {
					t.Fatalf("first line = %.40q, want the open comment", l)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the first event never came through the proxy")
			}
			if n := end.live.Load(); n != 1 {
				t.Fatalf("control: %d registration(s) live on StreamsEnd while the stream is open, want 1", n)
			}

			if tc.clientCloses {
				resp.Body.Close()
			} else {
				close(reset)
			}
			select {
			case <-served:
			case <-time.After(10 * time.Second):
				t.Fatal("the proxy handler had not returned 10 s after the stream was aborted")
			}
			if n := end.live.Load(); n != 0 {
				t.Fatalf("%d registration(s) still live on StreamsEnd after the aborted stream's handler returned, want 0: "+
					"each one outlives the stream for the life of the process", n)
			}
		})
	}
}
