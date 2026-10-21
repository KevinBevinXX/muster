//go:build livehermes

package mirrorhost

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/hermeshost"
	"github.com/ZacxDev/muster/internal/mirror"
)

// THE GATEWAY VIEWER AGAINST THE PIN (docs/design/hermes.md, Phase 4): the
// real gateway a real `hermes chat` started, in this run's scratch home.
//
//   - identify names the pin, through gateway.sock and — for a home whose
//     path does not fit sun_path — through gateway.sock.path;
//   - an approval is listed on attach, and answered from the viewer: "deny"
//     and the command does not run, "once" and it runs — each with an effect
//     that can fail (the victim directory) — while the CLI's modal stays
//     drawn (the stale-modal card's words);
//   - nothing the person typed reaches what the viewer hands out (the
//     conversation and pending[].text are in every resume);
//   - a sudo prompt is not an approval, and the viewer never answers it.
func TestLiveHermesGateway(t *testing.T) {
	r := newLiveRun(t)
	ctx := func() context.Context {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		t.Cleanup(cancel)
		return c
	}

	t.Run("identify, in the home", func(t *testing.T) {
		home := r.makeHome(homeOpts{name: "gwi", hook: true})
		pane := r.cli(home, 120, 40, []string{"chat"})
		if !r.waitIdle(pane, 90*time.Second) {
			t.Fatalf("not idle:\n%s", showScreen(r.capture(pane), 12))
		}
		g, err := hermeshost.NewHome(home).Identify(ctx())
		if err != nil {
			t.Fatal(err)
		}
		if g.CodeSHA != hermeshost.Pin || g.PID <= 0 || g.InstanceID == "" {
			t.Fatalf("identify = %+v", g)
		}
		if _, err := os.Lstat(filepath.Join(home, "gateway.sock")); err != nil {
			t.Fatalf("the gateway did not listen in the home: %v", err)
		}
		t.Logf("gateway pid %d, code_sha %s", g.PID, g.CodeSHA)
		r.killWindow(pane)
	})

	t.Run("identify, through the pointer", func(t *testing.T) {
		home := r.makeHome(homeOpts{name: "gwp-" + strings.Repeat("p", 100), hook: true})
		if len(filepath.Join(home, "gateway.sock")) <= 100 {
			t.Fatalf("the home is too short to need a pointer: %s", home)
		}
		pane := r.cli(home, 120, 40, []string{"chat"})
		if !r.waitIdle(pane, 90*time.Second) {
			t.Fatalf("not idle:\n%s", showScreen(r.capture(pane), 12))
		}
		if _, err := os.Lstat(filepath.Join(home, "gateway.sock.path")); err != nil {
			t.Fatalf("the gateway wrote no pointer: %v", err)
		}
		g, err := hermeshost.NewHome(home).Identify(ctx())
		if err != nil {
			t.Fatal(err)
		}
		if g.CodeSHA != hermeshost.Pin {
			t.Fatalf("identify = %+v", g)
		}
		r.killWindow(pane)
	})

	for _, choice := range []string{"deny", "once"} {
		t.Run("an approval answered "+choice+" from the viewer", func(t *testing.T) {
			home := r.makeHome(homeOpts{name: "gwa" + choice, hook: true})
			victim := filepath.Join(r.work, "livehermes-victim-dir")
			must(t, os.MkdirAll(victim, 0o700))
			pane := r.cli(home, 120, 40, []string{"chat"})
			if !r.waitIdle(pane, 90*time.Second) {
				t.Fatalf("not idle:\n%s", showScreen(r.capture(pane), 12))
			}
			const canary = "LIVEHERMES-TYPED-CANARY-7f3a"
			typed(r, pane, "APPROVE please "+canary)
			if !shows(r, pane, "Dangerous", 60*time.Second) {
				t.Fatalf("no approval modal:\n%s", showScreen(r.capture(pane), 20))
			}
			sid := waitStatusKind(t, r, "approval", 30*time.Second)
			v, ps, err := hermeshost.NewHome(home).Attach(ctx(), sid)
			if err != nil {
				t.Fatal(err)
			}
			defer v.Close()
			if len(ps) != 1 || ps[0].Command != "rm -r ./livehermes-victim-dir" || !ps[0].Offers("once") || !ps[0].Offers("deny") {
				t.Fatalf("prompts = %+v", ps)
			}
			t.Logf("attached: session %s, approval %+v", v.Session, ps[0])
			if err := v.Respond(ctx(), ps[0], choice); err != nil {
				t.Fatal(err)
			}
			if st := waitStatus(t, r, sid, "idle", 60*time.Second); st == "" {
				t.Fatal("the turn did not finish after the answer")
			}
			_, statErr := os.Stat(victim)
			switch choice {
			case "deny":
				if statErr != nil {
					t.Fatalf("denied, but the command ran: %v", statErr)
				}
			case "once":
				if statErr == nil {
					t.Fatal("approved once, but the command did not run")
				}
			}
			// The CLI's modal stays drawn after a viewer answered it (S1): the
			// card says so ("answered here; the terminal will show a stale
			// prompt until a key is pressed there").
			time.Sleep(2 * time.Second)
			if raw := r.capture(pane); !strings.Contains(raw, "Dangerous") {
				t.Fatalf("the CLI's modal was cleared after a viewer's answer — the card's stale-prompt words are now wrong:\n%s", showScreen(raw, 20))
			}
			if strings.Contains(fmt.Sprintf("%+v", ps), canary) {
				t.Fatal("what the person typed reached the viewer's output")
			}
			r.keys(pane, "x")
			r.killWindow(pane)
		})
	}

	t.Run("a sudo prompt is not an approval, and is never answered", func(t *testing.T) {
		home := r.makeHome(homeOpts{name: "gws", hook: true})
		pane := r.cli(home, 120, 40, []string{"chat"})
		if !r.waitIdle(pane, 90*time.Second) {
			t.Fatalf("not idle:\n%s", showScreen(r.capture(pane), 12))
		}
		typed(r, pane, "SUDO please")
		if !shows(r, pane, "Sudo", 60*time.Second) {
			t.Fatalf("no sudo modal:\n%s", showScreen(r.capture(pane), 20))
		}
		sid := waitStatusKind(t, r, "sudo", 30*time.Second)
		v, ps, err := hermeshost.NewHome(home).Attach(ctx(), sid)
		if err != nil {
			t.Fatal(err)
		}
		defer v.Close()
		if len(ps) != 0 {
			t.Fatalf("a sudo prompt was listed as an approval: %+v", ps)
		}
		time.Sleep(3 * time.Second)
		if raw := r.capture(pane); !strings.Contains(raw, "Sudo") {
			t.Fatalf("the sudo modal went away with only the viewer attached:\n%s", showScreen(raw, 20))
		}
		r.killWindow(pane)
	})
}

// TestLiveHermesApprovalThroughTheHost is Phase 4's host half against the
// pin: the REAL listing (Tmux with the home's HermesSource) and the host's
// asks (hermes_asks.go) attach a viewer to the window waiting on an approval,
// list its command with once and deny, and answer it — "deny", and the
// command does not run (an effect that can fail); once the window stops
// waiting, the viewer is detached.
func TestLiveHermesApprovalThroughTheHost(t *testing.T) {
	r := newLiveRun(t)
	home := r.makeHome(homeOpts{name: "gwh", hook: true})
	tm := Tmux{SocketPath: r.tmux.SocketPath, Hermes: NewHermesSource(home, r.statusDir())}
	victim := filepath.Join(r.work, "livehermes-victim-dir")
	must(t, os.MkdirAll(victim, 0o700))
	pane := r.cli(home, 120, 40, []string{"chat"})
	if !r.waitIdle(pane, 90*time.Second) {
		t.Fatalf("not idle:\n%s", showScreen(r.capture(pane), 12))
	}
	typed(r, pane, "APPROVE please")
	if !shows(r, pane, "Dangerous", 60*time.Second) {
		t.Fatalf("no approval modal:\n%s", showScreen(r.capture(pane), 20))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	m := newHermesAsks(ctx, tm, func() {}, t.Logf)
	m.terminal = tm.Hermes.State().PendingTerminalCommands
	defer m.close()
	list := func() *AgentPane {
		ws, err := tm.Windows(ctx)
		if err != nil {
			t.Fatal(err)
		}
		m.attachPending(ws)
		for _, w := range ws {
			if w.Pane == pane && w.Agent != nil {
				return w.Agent
			}
		}
		t.Fatal("the pane is not listed as Hermes")
		return nil
	}
	var ap *AgentPane
	deadline := time.Now().Add(60 * time.Second)
	for {
		ap = list()
		if ap.Pending != nil && ap.Pending.PromptID != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the approval was never listed: %+v (pending %+v)", ap, ap.Pending)
		}
		time.Sleep(300 * time.Millisecond)
	}
	p := ap.Pending
	if p.Kind != "approval" || p.Summary != "terminal: rm -r ./livehermes-victim-dir" || p.Choices != "once deny" || p.Detail != "recursive delete" {
		t.Fatalf("the listed approval: %+v", *p)
	}
	if err := m.answer(ctx, ap.Gen, p.PromptID, "deny"); err != nil {
		t.Fatal(err)
	}
	for time.Now().Before(deadline) && list().Status == "waiting" {
		time.Sleep(300 * time.Millisecond)
	}
	if st := waitStatus(t, r, ap.Gen, "idle", 60*time.Second); st == "" {
		t.Fatal("the turn did not finish after the answer")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("denied through the host, but the command ran: %v", err)
	}
	list()
	m.mu.Lock()
	n := len(m.sessions)
	m.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d viewer(s) still attached after the window stopped waiting", n)
	}
	r.keys(pane, "x")
	r.killWindow(pane)
}

// TestLiveHermesClarifyThroughTheHost is Phase 4c against the pin: a window
// waiting on a clarify lists its question and choices, read by the host from
// the real state.db (the assistant row that called the tool, no tool row
// yet), with nothing to answer from muster; the CLI's own picker stays up.
func TestLiveHermesClarifyThroughTheHost(t *testing.T) {
	r := newLiveRun(t)
	home := r.makeHome(homeOpts{name: "gwc", hook: true})
	src := NewHermesSource(home, r.statusDir())
	tm := Tmux{SocketPath: r.tmux.SocketPath, Hermes: src}
	pane := r.cli(home, 120, 40, []string{"chat"})
	if !r.waitIdle(pane, 90*time.Second) {
		t.Fatalf("not idle:\n%s", showScreen(r.capture(pane), 12))
	}
	typed(r, pane, "CLARIFY please")
	if !shows(r, pane, "needs your input", 60*time.Second) {
		t.Fatalf("no clarify picker:\n%s", showScreen(r.capture(pane), 20))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	m := newHermesAsks(ctx, nil, func() {}, t.Logf)
	m.clarify = src.State().PendingClarify
	defer m.close()
	var p *mirror.Pending
	deadline := time.Now().Add(60 * time.Second)
	for {
		ws, err := tm.Windows(ctx)
		if err != nil {
			t.Fatal(err)
		}
		m.attachPending(ws)
		for _, w := range ws {
			if w.Pane == pane && w.Agent != nil {
				p = w.Agent.Pending
			}
		}
		if p != nil && p.Summary != HermesClarifyText {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the clarify was never listed: %+v", p)
		}
		time.Sleep(300 * time.Millisecond)
	}
	want := mirror.Pending{Kind: "clarify", Summary: "Which colour should it use?", Tool: "clarify", Detail: "red · blue — answer in the terminal"}
	if *p != want {
		t.Fatalf("%+v\nwant %+v", *p, want)
	}
	if raw := r.capture(pane); !strings.Contains(raw, "needs your input") {
		t.Fatalf("the CLI's picker went away:\n%s", showScreen(raw, 20))
	}
	r.killWindow(pane)
}

// liveStatus is a status file as the harness reads it.
type liveStatus struct {
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
	Kind      string `json:"kind"`
	Since     int64  `json:"since"`
}

// statusFiles is every status file of the run, newest first by since.
func statusFiles(r *liveRun) []liveStatus {
	ents, _ := os.ReadDir(r.statusDir())
	var out []liveStatus
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(r.statusDir(), e.Name()))
		if err != nil {
			continue
		}
		var s liveStatus
		if json.Unmarshal(b, &s) == nil {
			out = append(out, s)
		}
	}
	return out
}

// waitStatusKind waits for a session waiting on kind, and returns its id.
func waitStatusKind(t *testing.T, r *liveRun, kind string, d time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		for _, s := range statusFiles(r) {
			if s.Status == "waiting" && s.Kind == kind {
				return s.SessionID
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("no status file waiting on %s: %+v", kind, statusFiles(r))
	return ""
}

// waitStatus waits for session's status file to read status; "" on timeout.
func waitStatus(t *testing.T, r *liveRun, session, status string, d time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		for _, s := range statusFiles(r) {
			if s.SessionID == session && s.Status == status {
				return s.Status
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return ""
}
