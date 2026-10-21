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

// TestLiveHermesLabelApprovalIsDenyOnly is audit #212 round 2's live
// reproduction: the pinned Hermes's SECOND approval gate (_run_approval_gate,
// tools/approval.py:1035) asks about a write to ~/.ssh/config with a
// "command" that is only a LABEL, `<write to …/.ssh/config>`, and no mask, no
// right-to-left text. Approving it once writes content muster never showed (a
// ProxyCommand runs on the next ssh). Only a terminal-command approval whose
// command IS what will run may be offered "once": this card is deny only.
func TestLiveHermesLabelApprovalIsDenyOnly(t *testing.T) {
	r := newLiveRun(t)
	home := r.makeHome(homeOpts{name: "gwl", hook: true})
	tm := Tmux{SocketPath: r.tmux.SocketPath, Hermes: NewHermesSource(home, r.statusDir())}
	pane := r.cli(home, 120, 40, []string{"chat"})
	if !r.waitIdle(pane, 90*time.Second) {
		t.Fatalf("not idle:\n%s", showScreen(r.capture(pane), 12))
	}
	typed(r, pane, "WRITESSH please")
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
			t.Fatalf("the approval was never listed: %+v\n%s", ap, showScreen(r.capture(pane), 20))
		}
		time.Sleep(300 * time.Millisecond)
	}
	p := ap.Pending
	t.Logf("the card: %q (%s), choices %q", p.Summary, p.Detail, p.Choices)
	if !strings.Contains(p.Summary, "<write to ") {
		t.Fatalf("not the SSH-config write's approval: %+v", *p)
	}
	if p.Choices != "deny" {
		t.Fatalf("a label approval is offered %q: %+v", p.Choices, *p)
	}
	if err := m.answer(ctx, ap.Gen, p.PromptID, "once"); err == nil {
		t.Fatal("once was answered for a label approval")
	}
	if err := m.answer(ctx, ap.Gen, p.PromptID, "deny"); err != nil {
		t.Fatal(err)
	}
	if st := waitStatus(t, r, ap.Gen, "idle", 60*time.Second); st == "" {
		t.Fatal("the turn did not finish after the deny")
	}
	if b, err := os.ReadFile(filepath.Join(r.fake, ".ssh", "config")); err == nil && strings.Contains(string(b), "ProxyCommand") {
		t.Fatal("denied, but the SSH config was written")
	}
	r.keys(pane, "x")
	r.killWindow(pane)
}
