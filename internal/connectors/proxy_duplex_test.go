package connectors_test

import (
	"bytes"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/connectors"
)

// syncBuffer is a log sink safe for the server goroutines that write to it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// echoProxy is a Proxy, with a log it records, in front of an upstream that
// reads the whole request body before answering with its length.
func echoProxy(t *testing.T) (p *connectors.Proxy, host string, logs *syncBuffer) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		_, _ = io.WriteString(w, r.Method+" len="+strconv.Itoa(len(b))+" err="+errString(err))
	}))
	t.Cleanup(up.Close)
	_, port, _ := net.SplitHostPort(up.Listener.Addr().String())
	host = "api.fake.test:" + port
	logs = &syncBuffer{}
	p = &connectors.Proxy{Store: approvedStub{c: connectors.Connector{Name: "svc", Hosts: []string{host},
		Scheme: connectors.SchemeBearer}}, Policy: testPolicy(), Logger: log.New(logs, "", 0)}
	return p, host, logs
}

func errString(err error) string {
	if err == nil {
		return "nil"
	}
	return err.Error()
}

const fullDuplexUnavailable = "full duplex unavailable"

// TestFullDuplexOverHTTP2IsANoOpThatSucceeds: on an HTTP/2 inbound request the
// server is always full duplex, EnableFullDuplex answers nil, and the proxy
// logs nothing and forwards the whole body.
//
// ⚠ AN INVARIANT GUARD. muster's own listeners serve plain HTTP/1 (no TLS, no
// h2c), so this path is reached only behind something that speaks HTTP/2 to
// the pod directly; it pins that the error handling does not fire there.
func TestFullDuplexOverHTTP2IsANoOpThatSucceeds(t *testing.T) {
	p, host, logs := echoProxy(t)
	front := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.Serve(w, r, connectors.Caller{AgentID: 1, AgentName: "a"}, "svc", host, "x")
	}))
	front.EnableHTTP2 = true
	front.StartTLS()
	t.Cleanup(front.Close)
	client := front.Client()
	client.Timeout = 30 * time.Second

	body := strings.Repeat("b", 300<<10)
	resp, err := client.Post(front.URL+"/x", "application/octet-stream", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST over HTTP/2: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.ProtoMajor != 2 {
		t.Fatalf("control: the inbound request was %s, not HTTP/2", resp.Proto)
	}
	if want := "POST len=" + strconv.Itoa(len(body)) + " err=nil"; resp.StatusCode != http.StatusOK || string(got) != want {
		t.Fatalf("answered %d %q, want 200 %q", resp.StatusCode, got, want)
	}
	if strings.Contains(logs.String(), fullDuplexUnavailable) {
		t.Fatalf("the proxy logged that full duplex was unavailable over HTTP/2:\n%s", logs)
	}
}

// TestAWriterHidingTheServersIsLoggedNotIgnored pins the other branch: a
// ResponseWriter that does not reach the server's (here a recorder) makes
// EnableFullDuplex fail, and the proxy SAYS so — the tearing race is open for
// that request, and nothing else would show it. The request is still served.
func TestAWriterHidingTheServersIsLoggedNotIgnored(t *testing.T) {
	p, host, logs := echoProxy(t)
	r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("hello"))
	w := httptest.NewRecorder()
	p.Serve(w, r, connectors.Caller{AgentID: 1, AgentName: "a"}, "svc", host, "x")
	if w.Code != http.StatusOK || w.Body.String() != "POST len=5 err=nil" {
		t.Fatalf("answered %d %q, want 200 %q", w.Code, w.Body.String(), "POST len=5 err=nil")
	}
	if !strings.Contains(logs.String(), fullDuplexUnavailable) {
		t.Fatalf("no %q log line for a writer that hides the server's; logs:\n%s", fullDuplexUnavailable, logs)
	}
}
