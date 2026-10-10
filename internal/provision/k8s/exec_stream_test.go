package k8s_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/apimachinery/pkg/util/httpstream/spdy"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/provision/k8s"
	"github.com/ZacxDev/muster/internal/provision/provisiontest"
)

// ---------------------------------------------------------------------------
// AN IN-PROCESS pods/exec ENDPOINT THAT SPEAKS THE APISERVER'S SPDY PROTOCOL.
//
// 🔴 THIS IS WHAT TURNS "THE EXEC STREAM IS UNCOVERED" INTO A TESTED PATH, AND
// ITS LIMIT IS STATED HERE SO NOBODY READS IT AS MORE. It is client-go's own
// remotecommand client talking to apimachinery's own SPDY upgrader, negotiating
// the v4 channel protocol a kubelet speaks, so the request the driver builds —
// method, path, every query parameter — and the stream it drives are both real.
// What it is NOT is an apiserver: no authorisation, no kubelet, no container
// runtime, no real pty. A real exec against a real cluster is still owed, and is
// the deploy's check, not this file's.
// ---------------------------------------------------------------------------

// execServer records what reached it and plays the container's side.
type execServer struct {
	t *testing.T

	mu      sync.Mutex
	method  string
	path    string
	query   map[string][]string
	sizes   []provision.TermSize
	gotSize chan struct{}

	// forbid answers every request with an apiserver 403 Status instead.
	forbid bool
	// hold keeps the stream open after stdin ends, so only a cancelled
	// context can end it.
	hold bool
	// closed is closed when the server sees the connection go away.
	closed chan struct{}
}

func newExecServer(t *testing.T) *execServer {
	return &execServer{t: t, gotSize: make(chan struct{}, 16), closed: make(chan struct{})}
}

func (s *execServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.method, s.path, s.query = r.Method, r.URL.Path, r.URL.Query()
	s.mu.Unlock()
	if s.forbid {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(metav1.Status{
			TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
			Status:   metav1.StatusFailure,
			Reason:   metav1.StatusReasonForbidden,
			Code:     http.StatusForbidden,
			Message: `pods "p" is forbidden: User "system:serviceaccount:muster:muster" cannot create ` +
				`resource "pods/exec" in API group "" in the namespace "x"`,
		})
		return
	}
	if _, err := httpstream.Handshake(r, w, []string{"v4.channel.k8s.io"}); err != nil {
		s.t.Errorf("handshake: %v", err)
		return
	}
	q := r.URL.Query()
	want := 1 // error
	for _, p := range []string{"stdin", "stdout", "stderr"} {
		if q.Get(p) == "true" || q.Get(p) == "1" {
			want++
		}
	}
	tty := q.Get("tty") == "true" || q.Get("tty") == "1"
	if tty {
		want++ // resize
	}
	streams := make(chan httpstream.Stream, want)
	conn := spdy.NewResponseUpgrader().UpgradeResponse(w, r, func(st httpstream.Stream, _ <-chan struct{}) error {
		streams <- st
		return nil
	})
	if conn == nil {
		s.t.Error("upgrade failed")
		return
	}
	defer conn.Close()
	byType := map[string]httpstream.Stream{}
	for len(byType) < want {
		select {
		case st := <-streams:
			byType[st.Headers().Get(corev1.StreamType)] = st
		case <-time.After(5 * time.Second):
			s.t.Errorf("only %d of %d streams arrived: %v", len(byType), want, keys(byType))
			return
		}
	}
	if rs := byType[corev1.StreamTypeResize]; rs != nil {
		go func() {
			dec := json.NewDecoder(rs)
			for {
				var sz struct{ Width, Height uint16 }
				if err := dec.Decode(&sz); err != nil {
					return
				}
				s.mu.Lock()
				s.sizes = append(s.sizes, provision.TermSize{Cols: sz.Width, Rows: sz.Height})
				s.mu.Unlock()
				s.gotSize <- struct{}{}
			}
		}()
	}
	out := byType[corev1.StreamTypeStdout]
	if in := byType[corev1.StreamTypeStdin]; in != nil && out != nil {
		// The container's side of a terminal: echo what was typed.
		_, _ = io.Copy(out, in)
	} else if out != nil {
		_, _ = out.Write([]byte("ran: " + strings.Join(q["command"], " ")))
	}
	if s.hold {
		<-conn.CloseChan()
		close(s.closed)
		return
	}
	errStream := byType[corev1.StreamTypeError]
	_ = json.NewEncoder(errStream).Encode(metav1.Status{Status: metav1.StatusSuccess})
	for _, st := range byType {
		_ = st.Close()
	}
}

func keys(m map[string]httpstream.Stream) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// execRig is a driver whose exec stream goes to an execServer, over a fake
// clientset holding one created instance with one pod.
func execRig(t *testing.T, m nsMode, srv *execServer) (*k8s.Driver, provision.Ref, string) {
	t.Helper()
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	d, cs := newDriver(t, m, func(c *k8s.Config) { c.RESTConfig = &rest.Config{Host: ts.URL} })
	spec := provisiontest.MinimalSpec("termy")
	ctx := context.Background()
	if err := d.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	seedPod(t, cs, m.ns("termy"), "termy")
	return d, spec.Ref, m.ns("termy")
}

func seedPod(t *testing.T, cs *fake.Clientset, ns, instance string) {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instance + "-5f7c9",
			Namespace: ns,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "muster-agent",
				"app.kubernetes.io/instance":   instance,
				"app.kubernetes.io/managed-by": "muster",
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if _, err := cs.CoreV1().Pods(ns).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed pod: %v", err)
	}
}

// TestExecTTYStreamsThroughTheExecSubresource: the request names the pod, the
// container, the command and a TTY; typed bytes reach the container and its
// output comes back; every resize arrives in order; ending stdin ends the call.
func TestExecTTYStreamsThroughTheExecSubresource(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) {
		srv := newExecServer(t)
		d, ref, ns := execRig(t, m, srv)

		inR, inW := io.Pipe()
		var out syncBuffer
		resize := make(chan provision.TermSize, 4)
		resize <- provision.TermSize{Cols: 45, Rows: 29}
		cmd := []string{"tmux", "attach-session", "-r", "-t", "=cc"}

		done := make(chan error, 1)
		go func() { done <- d.ExecTTY(context.Background(), ref, cmd, inR, &out, resize) }()

		waitSize(t, srv)
		resize <- provision.TermSize{Cols: 80, Rows: 24}
		waitSize(t, srv)
		if _, err := inW.Write([]byte("hello\x1b[A")); err != nil {
			t.Fatalf("write stdin: %v", err)
		}
		waitFor(t, func() bool { return out.String() == "hello\x1b[A" }, func() string { return "stdout " + out.String() })
		_ = inW.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("ExecTTY after stdin EOF: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("ExecTTY did not return after stdin ended and the container exited")
		}

		srv.mu.Lock()
		defer srv.mu.Unlock()
		if srv.method != "POST" {
			t.Errorf("method %q, want POST (PodExecRBACPrerequisite is derived from it)", srv.method)
		}
		if want := "/api/v1/namespaces/" + ns + "/pods/termy-5f7c9/exec"; srv.path != want {
			t.Errorf("path %q, want %q", srv.path, want)
		}
		for k, want := range map[string]string{"tty": "true", "stdin": "true", "stdout": "true", "container": "agent"} {
			if got := strings.Join(srv.query[k], ","); got != want {
				t.Errorf("query %s=%q, want %q", k, got, want)
			}
		}
		if got := strings.Join(srv.query["stderr"], ","); got == "true" {
			t.Errorf("stderr=%q with a TTY; the apiserver refuses both", got)
		}
		if got := strings.Join(srv.query["command"], " "); got != strings.Join(cmd, " ") {
			t.Errorf("command %q, want %q", got, strings.Join(cmd, " "))
		}
		want := []provision.TermSize{{Cols: 45, Rows: 29}, {Cols: 80, Rows: 24}}
		if len(srv.sizes) != len(want) || srv.sizes[0] != want[0] || srv.sizes[1] != want[1] {
			t.Errorf("sizes %v, want %v", srv.sizes, want)
		}
	})
}

// TestExecTTYAsksForATerminal: the request itself asks for a TTY, with no
// stderr stream (a TTY merges it) — checked with no resize queue, so nothing
// about sizes stands in front of the assertion.
func TestExecTTYAsksForATerminal(t *testing.T) {
	srv := newExecServer(t)
	d, ref, _ := execRig(t, nsModes[0], srv)
	inR, inW := io.Pipe()
	var out syncBuffer
	done := make(chan error, 1)
	go func() { done <- d.ExecTTY(context.Background(), ref, []string{"true"}, inR, &out, nil) }()
	_, _ = inW.Write([]byte("x"))
	_ = inW.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ExecTTY did not return")
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if got := strings.Join(srv.query["tty"], ","); got != "true" {
		t.Errorf("query tty=%q, want \"true\"", got)
	}
	if got := strings.Join(srv.query["stderr"], ","); got == "true" {
		t.Errorf("query stderr=%q with a TTY", got)
	}
}

// TestExecTTYEndsWhenTheContextIsCancelled: a container that never exits and a
// stdin that never ends — only the context can end the call, and it must, and
// the connection must actually close (a stream left open is a tmux client left
// attached in the pod).
func TestExecTTYEndsWhenTheContextIsCancelled(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) {
		srv := newExecServer(t)
		srv.hold = true
		d, ref, _ := execRig(t, m, srv)
		inR, inW := io.Pipe()
		defer inW.Close()
		ctx, cancel := context.WithCancel(context.Background())
		resize := make(chan provision.TermSize, 1)
		resize <- provision.TermSize{Cols: 10, Rows: 5}
		done := make(chan error, 1)
		go func() { done <- d.ExecTTY(ctx, ref, []string{"sleep", "infinity"}, inR, io.Discard, resize) }()
		waitSize(t, srv)
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("ExecTTY did not return after its context was cancelled")
		}
		select {
		case <-srv.closed:
		case <-time.After(5 * time.Second):
			t.Fatal("the exec connection stayed open after the context was cancelled")
		}
	})
}

// TestExecTTYForbiddenNamesThePodsExecRule: a 403 from the upgrade is the
// refusal with the RBAC rule in it, not "forbidden" alone.
func TestExecTTYForbiddenNamesThePodsExecRule(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) {
		srv := newExecServer(t)
		srv.forbid = true
		d, ref, _ := execRig(t, m, srv)
		inR, inW := io.Pipe()
		defer inW.Close()
		err := d.ExecTTY(context.Background(), ref, []string{"true"}, inR, io.Discard, nil)
		if !errors.Is(err, provision.ErrUnsupported) {
			t.Fatalf("a 403 must be ErrUnsupported (a refusal, not an outage), got %v", err)
		}
		for _, want := range []string{`resources ["pods/exec"]`, `verbs ["create"]`, "forbidden"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal must contain %q, got %q", want, err)
			}
		}
	})
}

// TestExecStillStreamsAfterTheURLMovedToRESTConfig: the non-TTY Exec shares the
// request builder, so it is exercised once through the same server.
func TestExecStillStreamsAfterTheURLMovedToRESTConfig(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) {
		srv := newExecServer(t)
		d, ref, _ := execRig(t, m, srv)
		var out, errb bytes.Buffer
		if err := d.Exec(context.Background(), ref, []string{"echo", "hi"}, nil, &out, &errb); err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if out.String() != "ran: echo hi" {
			t.Fatalf("stdout %q", out.String())
		}
		if got := strings.Join(srv.query["tty"], ","); got == "true" {
			t.Error("a plain Exec must not ask for a TTY")
		}
	})
}

// TestTheExecRBACPrerequisiteIsTheVerbTheDriverSends derives the verb from the
// method the driver actually sends, as the apiserver does for a subresource.
func TestTheExecRBACPrerequisiteIsTheVerbTheDriverSends(t *testing.T) {
	srv := newExecServer(t)
	d, ref, _ := execRig(t, nsModes[0], srv)
	if err := d.Exec(context.Background(), ref, []string{"true"}, nil, io.Discard, nil); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	verbFor := map[string]string{"POST": "create", "GET": "get"}
	srv.mu.Lock()
	got := verbFor[srv.method]
	srv.mu.Unlock()
	if got == "" {
		t.Fatalf("the driver sent %q, which this derivation does not map", srv.method)
	}
	if len(k8s.PodExecRBACPrerequisite) != 1 || k8s.PodExecRBACPrerequisite[0] != got {
		t.Fatalf("PodExecRBACPrerequisite = %v, but the driver's %s is authorised as %q",
			k8s.PodExecRBACPrerequisite, srv.method, got)
	}
}

func waitSize(t *testing.T, s *execServer) {
	t.Helper()
	select {
	case <-s.gotSize:
	case <-time.After(5 * time.Second):
		t.Fatal("no terminal size reached the exec server")
	}
}

func waitFor(t *testing.T, ok func() bool, what func() string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out; %s", what())
}

// syncBuffer is a bytes.Buffer safe to read while the stream writes it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}
