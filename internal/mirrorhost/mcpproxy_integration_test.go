//go:build tmuxit

package mirrorhost

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"io"
)

// TestTmuxABrandNewClaudeConnectsAtOnceAndItsFirstCallIsBound is the startup
// race, against REAL tmux: Claude Code connects its MCP servers as it starts,
// when its sessions file exists but its transcript does not — so no listing can
// show the session yet (ClaudeLocator.resolve: "nothing to show YET").
//
//   - initialize and tools/list are forwarded at once, with no session (they
//     need none). Before the fix they were refused, the connection failed, and
//     Claude Code never retried it.
//   - A call before the first record is a TOOL error naming the pane as
//     received — not a failed request.
//   - Once the session has written its first record, the next call is bound to
//     it by a fresh read of the pane (no periodic refresh in between), and the
//     host's listing is pushed to muster BEFORE the call is forwarded.
func TestTmuxABrandNewClaudeConnectsAtOnceAndItsFirstCallIsBound(t *testing.T) {
	r := newTmuxRig(t)
	home, cwd := t.TempDir(), t.TempDir()
	const sid = "3e5f6071-8293-4a41-9dce-3f4a5b6c7d8e"
	r.run(append([]string{"new-session", "-d", "-s", "fresh", "-x", "120", "-y", "40", "-c", cwd}, fakeClaude(t, t.TempDir(), home, sid)...)...)
	pane := strings.TrimSpace(r.run("display-message", "-p", "-t", "=fresh:", "#{pane_id}"))
	waitFor(t, "the fake CLI to start", func() bool { return strings.Contains(r.windowPane(pane), "FAKE-CLAUDE "+sid) })
	if _, err := os.Stat(filepath.Join(home, "sessions")); err != nil {
		t.Fatalf("fixture: no sessions file yet: %v", err)
	}
	tmuxEnvValue := strings.TrimSpace(r.run("display-message", "-p", "#{socket_path}")) + ",1,0"

	var mu sync.Mutex
	var events []string
	record := func(e string) { mu.Lock(); events = append(events, e); mu.Unlock() }
	up, srv := newUpstream(t, func(w http.ResponseWriter) {
		record("forwarded")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	})
	b := Tmux{Socket: r.sock, Claude: ClaudeLocator{Home: home}}
	p := &MCPProxy{URL: srv.URL, Token: proxyToken, AllowHTTP: true, Secret: proxySecret, Windows: b, Socket: b,
		Refresh: func(context.Context) error { record("refreshed"); return nil }}
	hdr := map[string]string{"Authorization": "Bearer " + proxySecret, MCPPaneHeader: pane, MCPTmuxHeader: tmuxEnvValue}

	// Startup: the session has no transcript.
	for _, body := range []string{`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`} {
		if w := postBody(t, p, hdr, body); w.Code != http.StatusOK {
			t.Fatalf("%s at startup: %d %s", body, w.Code, w.Body)
		}
	}
	if up.n() != 2 {
		t.Fatalf("startup: %d forwards, want initialize and tools/list forwarded", up.n())
	}
	if got := up.calls[0].Header.Get(MCPSessionHeader); got != "" {
		t.Fatalf("startup: forwarded with session %q", got)
	}
	text := toolErrorText(t, postBody(t, p, hdr, proxyCall))
	if !strings.Contains(text, "no Claude Code session runs in pane \\\""+pane+"\\\"") || up.n() != 2 {
		t.Fatalf("a call before the first record: %s (%d forwards)", text, up.n())
	}

	// The first record.
	project := filepath.Join(home, "projects", ProjectSlug(cwd))
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, sid+".jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	events = nil
	mu.Unlock()
	if w := postBody(t, p, hdr, proxyCall); w.Code != http.StatusOK {
		t.Fatalf("the first call: %d %s", w.Code, w.Body)
	}
	if up.n() != 3 || up.calls[2].Header.Get(MCPSessionHeader) != sid {
		t.Fatalf("the first call was forwarded as %q (%d forwards), want %s", up.calls[up.n()-1].Header.Get(MCPSessionHeader), up.n(), sid)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(events, ",") != "refreshed,forwarded" {
		t.Fatalf("order %v: muster must be sent the listing BEFORE the call", events)
	}
}
