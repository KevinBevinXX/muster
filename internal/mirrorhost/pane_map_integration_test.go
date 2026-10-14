//go:build tmuxit

package mirrorhost

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeMappedClaude is fakeClaude whose sessions file names staleID — as Claude
// Code's does after a conversation changes id inside one process — and which,
// when liveID is not empty, ALSO writes the pane map record a hook would: its
// own pane, its tmux server's pid, its own pid and start time, and liveID's
// transcript. Everything it records it reads from the process and the pane it
// actually runs in, so the test asserts against real tmux, not a copy of it.
func fakeMappedClaude(t *testing.T, dir, home, mapDir, staleID, liveID string) []string {
	t.Helper()
	script := filepath.Join(dir, "node_modules", "@anthropic-ai", "claude-code", "cli.js")
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `home=$1 map=$2 stale=$3 live=$4
mkdir -p "$home/sessions"
start=$(cut -d' ' -f22 /proc/$$/stat)
printf '{"pid":%s,"sessionId":"%s","cwd":"%s","status":"idle","procStart":"%s"}' "$$" "$stale" "$PWD" "$start" > "$home/sessions/$$.json"
if [ -n "$live" ]; then
  srv=${TMUX#*,}; srv=${srv%%,*}
  mkdir -p "$map"
  slug=$(printf '%s' "$PWD" | sed 's/[^A-Za-z0-9]/-/g')
  printf '{"schema":1,"runtime":"claude","pane_id":"%s","tmux_pid":"%s","session_id":"%s","transcript_path":"%s","claude_pid":"%s","claude_start":"%s","last_activity_ts":"2000-01-01T00:00:00Z"}\n' \
    "$TMUX_PANE" "$srv" "$live" "$home/projects/$slug/$live.jsonl" "$$" "$start" > "$map/claude-p${TMUX_PANE#%}.json"
fi
printf 'FAKE-CLAUDE %s\n' "$stale"
cat
`
	if err := os.WriteFile(script, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return []string{"sh", script, home, mapDir, staleID, liveID}
}

// TestTmuxThePaneMapNamesTheConversationAPaneShows, on a private tmux server:
//
//  1. window A's Claude Code has a sessions file naming sess-stale and a pane
//     map entry naming sess-live → A shows sess-live (the measured live shape);
//  2. window B, a second Claude Code in the SAME cwd with no map entry, shows
//     its own sessions file's session — not A's;
//  3. A's pane is then RESPAWNED with a different Claude Code that writes no map
//     entry: the old entry names a process that is gone, so it is refused and A
//     shows the new process's own session.
func TestTmuxThePaneMapNamesTheConversationAPaneShows(t *testing.T) {
	r := newTmuxRig(t)
	home, cwd, mapDir := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "ledger")
	project := filepath.Join(home, "projects", ProjectSlug(cwd))
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"sess-stale", "sess-live", "sess-b", "sess-respawned"} {
		if err := os.WriteFile(filepath.Join(project, id+".jsonl"), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	a := fakeMappedClaude(t, t.TempDir(), home, mapDir, "sess-stale", "sess-live")
	b := fakeMappedClaude(t, t.TempDir(), home, mapDir, "sess-b", "")
	r.run(append([]string{"new-session", "-d", "-s", "scratch", "-x", "120", "-y", "40", "-c", cwd}, a...)...)
	wa := strings.TrimSpace(r.run("display-message", "-p", "-t", "=scratch:", "#{window_id}"))
	wb := strings.TrimSpace(r.run(append([]string{"new-window", "-d", "-P", "-F", "#{window_id}", "-t", "=scratch:", "-c", cwd}, b...)...))
	waitFor(t, "both fake CLIs to start", func() bool {
		return strings.Contains(r.windowPane(wa), "FAKE-CLAUDE sess-stale") && strings.Contains(r.windowPane(wb), "FAKE-CLAUDE sess-b")
	})

	ctx := context.Background()
	be := Tmux{Socket: r.sock, Claude: ClaudeLocator{Home: home, PaneMapDir: mapDir}}
	off := Tmux{Socket: r.sock, Claude: ClaudeLocator{Home: home}}
	resolve := func(b Tmux, w string) *ClaudePane {
		ws, err := b.Windows(ctx)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := FindWindow(ws, "scratch", w)
		return got.Claude
	}
	if c := resolve(off, wa); c == nil || c.Gen != "sess-stale" {
		t.Fatalf("control: with no pane map window A = %+v, want its sessions file's sess-stale", c)
	}
	ca := resolve(be, wa)
	if ca == nil || ca.Gen != "sess-live" || ca.Source != SourcePaneMap || ca.Transcript != filepath.Join(project, "sess-live.jsonl") {
		t.Fatalf("window A = %+v, want the pane map's sess-live", ca)
	}
	if cb := resolve(be, wb); cb == nil || cb.Gen != "sess-b" || cb.Source != SourceSessions || cb.Pane == ca.Pane {
		t.Fatalf("window B = %+v, want its own sess-b on its own pane", cb)
	}

	// 3: a NEW Claude Code in A's pane; the old map entry is left behind.
	c := fakeMappedClaude(t, t.TempDir(), home, mapDir, "sess-respawned", "")
	r.run(append([]string{"respawn-pane", "-k", "-t", wa, "-c", cwd}, c...)...)
	waitFor(t, "the respawned CLI to start", func() bool { return strings.Contains(r.windowPane(wa), "FAKE-CLAUDE sess-respawned") })
	if _, err := os.Stat(filepath.Join(mapDir, "claude-p"+strings.TrimPrefix(ca.Pane, "%")+".json")); err != nil {
		t.Fatalf("instrument check: the old entry must still be there for this step to mean anything: %v", err)
	}
	if cr := resolve(be, wa); cr == nil || cr.Gen != "sess-respawned" || cr.Source != SourceSessions {
		t.Fatalf("window A after respawn = %+v, want the new process's sess-respawned (the stale entry refused)", cr)
	}
}
