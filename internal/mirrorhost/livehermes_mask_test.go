//go:build livehermes

package mirrorhost

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLiveHermesMaskedApprovalIsDenyOnly runs a command through the REAL
// pinned redactor (audit #212): the gateway masks `X-Api-Key:a;rm` into
// `X-Api-Key:*** -r…`, swallowing the ';', so the card's command is not the
// one that would run — the host lists it deny only, and refuses "once".
func TestLiveHermesMaskedApprovalIsDenyOnly(t *testing.T) {
	r := newLiveRun(t)
	home := r.makeHome(homeOpts{name: "gwm", hook: true})
	tm := Tmux{SocketPath: r.tmux.SocketPath, Hermes: NewHermesSource(home, r.statusDir())}
	victim := filepath.Join(r.work, "livehermes-victim-dir")
	must(t, os.MkdirAll(victim, 0o700))
	pane := r.cli(home, 120, 40, []string{"chat"})
	if !r.waitIdle(pane, 90*time.Second) {
		t.Fatalf("not idle:\n%s", showScreen(r.capture(pane), 12))
	}
	typed(r, pane, "APPROVEMASK please")
	if !shows(r, pane, "Dangerous", 60*time.Second) {
		t.Fatalf("no approval modal:\n%s", showScreen(r.capture(pane), 20))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	m := newHermesAsks(ctx, tm, func() {}, t.Logf)
	m.terminal = tm.Hermes.State().PendingTerminalCommands
	defer m.close()
	var ap *AgentPane
	deadline := time.Now().Add(60 * time.Second)
	for {
		ws, err := tm.Windows(ctx)
		if err != nil {
			t.Fatal(err)
		}
		m.attachPending(ws)
		for _, w := range ws {
			if w.Pane == pane && w.Agent != nil {
				ap = w.Agent
			}
		}
		if ap != nil && ap.Pending != nil && ap.Pending.PromptID != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the approval was never listed: %+v", ap)
		}
		time.Sleep(300 * time.Millisecond)
	}
	p := ap.Pending
	t.Logf("the card: %q (%s), choices %q", p.Summary, p.Detail, p.Choices)
	if !strings.Contains(p.Summary, "***") || p.Choices != "deny" || p.Detail != HermesNotWholeText {
		t.Fatalf("a gateway-masked command is offered: %+v", *p)
	}
	if err := m.answer(ctx, ap.Gen, p.PromptID, "once"); err == nil {
		t.Fatal("once was answered for a masked command")
	}
	if err := m.answer(ctx, ap.Gen, p.PromptID, "deny"); err != nil {
		t.Fatal(err)
	}
	if st := waitStatus(t, r, ap.Gen, "idle", 60*time.Second); st == "" {
		t.Fatal("the turn did not finish after the deny")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("denied, but the command ran: %v", err)
	}
	r.keys(pane, "x")
	r.killWindow(pane)
}
