package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/agentprovision"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/api"
	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/termsize"
)

// heldTerminalDriver plays a pod whose terminal stays attached until its
// stream is cancelled, and whose release takes releaseDelay to land.
type heldTerminalDriver struct {
	*provision.Noop
	attached     chan struct{}
	releaseDelay time.Duration

	mu       sync.Mutex
	released int
}

func (d *heldTerminalDriver) Capabilities() provision.Capabilities {
	c := d.Noop.Capabilities()
	c.Exec = true
	return c
}

func (d *heldTerminalDriver) ExecTTY(ctx context.Context, _ provision.Ref, _ []string, _ io.Reader, stdout io.Writer, _ <-chan provision.TermSize) error {
	if _, err := io.WriteString(stdout, "attached\r\n"); err != nil {
		return err
	}
	close(d.attached)
	<-ctx.Done()
	return ctx.Err()
}

func (d *heldTerminalDriver) Exec(ctx context.Context, _ provision.Ref, cmd []string, _ io.Reader, stdout, _ io.Writer) error {
	// The window-size query that precedes an attach is not a release; this
	// pod has no session to size ("unknown" — the attach proceeds as asked).
	if slices.Equal(cmd, agents.TerminalSizeCommand(termsize.TmuxWindowFormat)) {
		return errors.New("can't find session: cc")
	}
	select {
	case <-time.After(d.releaseDelay):
	case <-ctx.Done():
		return ctx.Err()
	}
	d.mu.Lock()
	d.released++
	d.mu.Unlock()
	_, err := io.WriteString(stdout, "released 1\n")
	return err
}

func (d *heldTerminalDriver) releases() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.released
}

// oneAgent serves the one claude-code agent the terminal is opened on. It
// EMBEDS agents.Store, so any other call panics instead of passing.
type oneAgent struct{ agents.Store }

func (oneAgent) GetByName(_ context.Context, name string) (agents.Agent, error) {
	if name != "held-heron" {
		return agents.Agent{}, errors.New("no rows")
	}
	return agents.Agent{ID: 31, Name: name, Kind: agents.KindClaudeCode}, nil
}

// TestShutdownReleasesAnOpenTerminal: SIGTERM with an agent terminal open.
// The terminal's socket is hijacked, so http.Server.Shutdown alone neither
// ends it nor waits for it, and the process would exit before the release that
// ends its tmux client in the pod. When app.shutdown RETURNS, the release must
// have run. releaseDelay (250ms) makes it measurably slower than a shutdown
// that does not wait.
func TestShutdownReleasesAnOpenTerminal(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	d := &heldTerminalDriver{Noop: provision.MustNewNoop(), attached: make(chan struct{}), releaseDelay: 250 * time.Millisecond}
	adapter, err := agentprovision.New(agentprovision.Config{Driver: d, Store: oneAgent{}, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	const password = "shutdown-test-operator-password-0123456789"
	srv := api.New(nil, api.AuthConfig{UIPassword: password}, logger)
	srv.UseExtensions(api.Extensions{Agents: oneAgent{}, Provisioner: adapter})
	a := &app{logger: logger, http: &http.Server{Handler: srv.Handler()}, srv: srv}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	resp, err := client.PostForm(ts.URL+"/login", url.Values{"password": {password}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	base, _ := url.Parse(ts.URL)
	if len(jar.Cookies(base)) == 0 {
		t.Fatalf("instrument check FAILED: login issued no session (status %d)", resp.StatusCode)
	}

	h := http.Header{}
	h.Set("Origin", ts.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/agents/held-heron/terminal/ws",
		&websocket.DialOptions{HTTPHeader: h, HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	select {
	case <-d.attached:
	case <-time.After(5 * time.Second):
		t.Fatal("instrument check FAILED: the terminal never attached")
	}

	a.shutdown()
	if got := d.releases(); got != 1 {
		t.Fatalf("app.shutdown returned with %d terminal release(s) run, want 1: the open terminal's "+
			"tmux client would stay attached in the agent's pod after the redeploy", got)
	}
}

// TestTheTerminalDrainFitsInsideTheHTTPDrain: the two run concurrently, so the
// shutdown's total stays as main.go's arithmetic states only while this holds.
func TestTheTerminalDrainFitsInsideTheHTTPDrain(t *testing.T) {
	if terminalDrainTimeout > shutdownDrainTimeout {
		t.Fatalf("terminalDrainTimeout %s exceeds shutdownDrainTimeout %s", terminalDrainTimeout, shutdownDrainTimeout)
	}
}
