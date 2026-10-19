package mirrorhost

import (
	"context"
	"io"
	"log"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestRefreshNowSendsANewClaudeSessionBeforeItReturns: with the periodic
// refresh an hour away, a Claude session that started since the hello reaches
// muster only through RefreshNow — and it has been SENT by the time RefreshNow
// returns (what the MCP endpoint relies on before forwarding a call). A
// RefreshNow with nothing new sends nothing; one with no connection says so.
func TestRefreshNowSendsANewClaudeSessionBeforeItReturns(t *testing.T) {
	f := newFakeMuster(t)
	b := &windowsBackend{}
	b.set(WindowInfo{Session: "work", ID: "@1", Index: 0, Active: true, Pane: "%1", PanePID: 1, Command: "zsh"})
	c, err := New(Config{URL: f.srv.URL, Token: mirror.TokenPrefix + "t", Host: "laptop", Backend: b,
		AllowHTTP: true, Refresh: time.Hour, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.RefreshNow(context.Background()); err == nil {
		t.Fatal("RefreshNow before any connection reported success")
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = c.RunOnce(ctx) }()
	waitUntil(t, "the hello", func() bool { return len(f.of(mirror.TypeHello)) == 1 })
	waitUntil(t, "the connection to be current", func() bool {
		c.curMu.Lock()
		defer c.curMu.Unlock()
		return c.cur != nil
	})

	const sid = "5a6b7c8d-9e0f-4a1b-8c2d-3e4f5a6b7c8d"
	b.set(WindowInfo{Session: "work", ID: "@1", Index: 0, Active: true, Pane: "%1", PanePID: 1, Command: "claude",
		Claude: &ClaudePane{Pane: "%1", Gen: sid}})
	rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer rcancel()
	if err := c.RefreshNow(rctx); err != nil {
		t.Fatal(err)
	}
	// No wait: it was sent before RefreshNow returned. The fake records a
	// message when its reader gets it, so allow only for that read.
	waitUntil(t, "the sessions message", func() bool { return len(f.of(mirror.TypeSessions)) == 1 })
	got := f.of(mirror.TypeSessions)[0]
	if len(got.Windows) != 1 || got.Windows[0].ClaudeSession != sid {
		t.Fatalf("sent %+v, want the window with session %s", got.Windows, sid)
	}
	if err := c.RefreshNow(rctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(f.of(mirror.TypeSessions)); n != 1 {
		t.Fatalf("an unchanged listing was sent again (%d messages)", n)
	}
}

// gatedWindows blocks Windows until a token is on gate.
type gatedWindows struct {
	*windowsBackend
	gate chan struct{}
}

func (g gatedWindows) Windows(ctx context.Context) ([]WindowInfo, error) {
	select {
	case <-g.gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return g.windowsBackend.Windows(ctx)
}

// TestRefreshNowWaitsForTheRead: RefreshNow does not return while the host's
// read is still running — so "it returned" means "the listing was read and,
// if new, sent".
func TestRefreshNowWaitsForTheRead(t *testing.T) {
	f := newFakeMuster(t)
	b := &windowsBackend{}
	b.set(WindowInfo{Session: "work", ID: "@1", Index: 0, Active: true, Pane: "%1", PanePID: 1, Command: "zsh"})
	g := gatedWindows{windowsBackend: b, gate: make(chan struct{}, 1)}
	g.gate <- struct{}{} // the hello's read
	c, err := New(Config{URL: f.srv.URL, Token: mirror.TokenPrefix + "t", Host: "laptop", Backend: g,
		AllowHTTP: true, Refresh: time.Hour, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = c.RunOnce(ctx) }()
	waitUntil(t, "the connection to be current", func() bool {
		c.curMu.Lock()
		defer c.curMu.Unlock()
		return c.cur != nil
	})
	done := make(chan error, 1)
	go func() { done <- c.RefreshNow(context.Background()) }()
	select {
	case err := <-done:
		t.Fatalf("RefreshNow returned (%v) while the read was still blocked", err)
	case <-time.After(200 * time.Millisecond):
	}
	g.gate <- struct{}{}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RefreshNow did not return after the read")
	}
}
