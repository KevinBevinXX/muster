//go:build tmuxit

package mirrorhost

// THE IDENTITY COLOUR AND THE SHELL COUNT against REAL tmux on a private socket
// (newTmuxRig, -f /dev/null): never the operator's server.

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/cctasks"
	"github.com/ZacxDev/muster/internal/mirror"
)

// TestTmuxReadsTheIdentityColourOption is the design's measured table, as a
// test (tmux resolves a user option window, then session, then global):
//
//   - a GLOBAL conditional on the session name tints alpha and beta;
//   - a SESSION literal tints gamma, an uppercase one (#D… collides with the
//     pane id under E:) is recovered from the raw field;
//   - a WINDOW override beats alpha's session colour for that window only;
//   - a session the conditional does not name has no colour (negative
//     control), and a value holding a tab or newline is no colour — while its
//     window is still listed.
func TestTmuxReadsTheIdentityColourOption(t *testing.T) {
	r := newTmuxRig(t)
	for _, s := range []string{"alpha", "beta", "gamma", "delta", "plain", "broken"} {
		r.session(s, "marker-"+s)
	}
	second := r.window("alpha", "alpha-second")
	r.run("set", "-g", "@muster-color", "#{?#{==:#{session_name},alpha},##aabbcc,#{?#{==:#{session_name},beta},##ddeeff,}}")
	r.run("set", "-t", "gamma", "@muster-color", "#123abc")
	r.run("set", "-t", "delta", "@muster-color", "#D79921")
	r.run("set", "-w", "-t", second, "@muster-color", "#456def")
	r.run("set", "-t", "broken", "@muster-color", "#aa\nbb\tcc")

	ws, err := (Tmux{Socket: r.sock, Claude: ClaudeLocator{Proc: t.TempDir(), Home: t.TempDir()}}).Windows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, w := range ws {
		key := w.Session
		if w.ID == second {
			key = "alpha:second"
		}
		got[key] = w.Color
	}
	want := map[string]string{"alpha": "#aabbcc", "alpha:second": "#456def", "beta": "#ddeeff", "gamma": "#123abc",
		"delta": "#d79921", "plain": "", "broken": ""}
	if !maps.Equal(got, want) {
		t.Fatalf("colours = %v, want %v", got, want)
	}
}

// TestTmuxCountsAFakeClaudesBackgroundShell: a pane runs a fake `claude` (a
// script by that name) that writes its sessions file with status "shell" and
// starts `sleep` with stdout on a task output file. The listing must count
// exactly 1 shell (positive control); with the sleep killed, the process tree
// counts 0 (negative control) — and the card says "1+", never 0, because the
// CLI still says shell.
func TestTmuxCountsAFakeClaudesBackgroundShell(t *testing.T) {
	r := newTmuxRig(t)
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config")
	cwd := filepath.Join(dir, "work")
	tasks := filepath.Join(dir, "tmp", "claude-1000", ProjectSlug(cwd), "5e55-it01", "tasks")
	for _, d := range []string{filepath.Join(cfg, "sessions"), filepath.Join(cfg, "projects", ProjectSlug(cwd)), cwd, tasks} {
		must(t, os.MkdirAll(d, 0o755))
	}
	must(t, os.WriteFile(filepath.Join(cfg, "projects", ProjectSlug(cwd), "5e55-it01.jsonl"), []byte("{}\n"), 0o644))
	bin := filepath.Join(dir, "bin")
	must(t, os.MkdirAll(bin, 0o755))
	script := "#!/bin/sh\n" +
		"printf '{\"pid\":%d,\"sessionId\":\"5e55-it01\",\"cwd\":\"%s\",\"status\":\"shell\",\"startedAt\":1}' $$ \"$PWD\" > '" + cfg + "/sessions/'$$.json\n" +
		"sleep 600 > '" + tasks + "/b0000000a.output' 2>&1 &\n" +
		"echo $! > '" + dir + "/sleep.pid'\n" +
		"while read -r line; do :; done\n"
	must(t, os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o755))
	r.run("new-session", "-d", "-s", "work", "-c", cwd, filepath.Join(bin, "claude"))

	// 🔴 WAIT FOR THE FAKE CLI TO BE UP BEFORE THE FIRST READ. new-session
	// returns once tmux has forked the pane, not once the script has written its
	// sessions file and started its sleep; a read before that sees a pane that
	// is not (yet) a Claude window and fails the instrument check — a race CI
	// lost once. Ready is observable: sleep.pid and the sessions file exist.
	waitFakeClaude(t, dir, filepath.Join(cfg, "sessions"))
	tm := Tmux{Socket: r.sock, Claude: ClaudeLocator{Home: cfg}}
	read := func() (cctasks.Shells, map[string]mirror.WindowWork) {
		ws, err := tm.Windows(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(ws) != 1 || ws[0].Claude == nil {
			t.Fatalf("instrument check: want one Claude window, got %+v", ws)
		}
		return ws[0].Claude.Shells, (&workCounter{}).windowWork(ws)
	}
	var sleepPID int
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, _ := os.ReadFile(filepath.Join(dir, "sleep.pid"))
		sleepPID, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		if sh, _ := read(); sleepPID > 0 && sh.N == 1 {
			break
		}
		if time.Now().After(deadline) {
			sh, work := read()
			t.Fatalf("the fake CLI's background sleep was never counted: shells %+v, work %v", sh, work)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, work := read(); !maps.Equal(work, map[string]mirror.WindowWork{work0(work): {Shells: 1}}) {
		t.Fatalf("with the sleep running: work %v, want exactly 1 shell", work)
	}
	// Kill the sleep — the one this test started, checked by its argv.
	cmd, _ := os.ReadFile(filepath.Join("/proc", strconv.Itoa(sleepPID), "cmdline"))
	if !strings.HasPrefix(string(cmd), "sleep\x00600") {
		t.Fatalf("pid %d is %q, not the fixture's sleep", sleepPID, cmd)
	}
	must(t, syscall.Kill(sleepPID, syscall.SIGTERM))
	deadline = time.Now().Add(10 * time.Second)
	for {
		sh, work := read()
		if sh.N == 0 {
			if !maps.Equal(work, map[string]mirror.WindowWork{work0(work): {Shells: 1, ShellsAtLeast: true}}) {
				t.Fatalf("with the sleep gone and the CLI still saying shell: work %v, want 1+", work)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the killed sleep is still counted: %+v", sh)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitFakeClaude waits, bounded, until a fake claude script is up: it wrote
// dir/sleep.pid and a sessions file under sessions. It fails with what is
// missing when it is not up within 10 s.
func waitFakeClaude(t *testing.T, dir, sessions string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, errPID := os.Stat(filepath.Join(dir, "sleep.pid"))
		files, _ := filepath.Glob(filepath.Join(sessions, "*.json"))
		if errPID == nil && len(files) > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the fake claude did not come up in 10s: sleep.pid: %v; sessions files: %v", errPID, files)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// work0 is the one key of a one-entry map ("" for none).
func work0(m map[string]mirror.WindowWork) string {
	for k := range m {
		return k
	}
	return ""
}
