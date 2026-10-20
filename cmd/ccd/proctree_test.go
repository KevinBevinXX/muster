package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeProc writes a /proc fixture: pid -> (parent, program).
func fakeProc(t *testing.T, procs map[int]struct {
	ppid int
	exe  string
}) string {
	t.Helper()
	root := t.TempDir()
	for pid, p := range procs {
		dir := filepath.Join(root, strconv.Itoa(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		// A comm holding ") (" and spaces, as a real one may.
		stat := fmt.Sprintf("%d (we) (ird name) S %d 1 1 0 -1\n", pid, p.ppid)
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(p.exe, filepath.Join(dir, "exe")); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

type proc = struct {
	ppid int
	exe  string
}

// The tree a chief pod has: tmux(10) -> primary CLI(20) -> its hook's sh(30) ->
// `ccd hook`(31); and a nested CLI the primary's Bash tool started:
// 20 -> bash(40) -> nested CLI(41) -> sh(50) -> `ccd hook`(51).
var chiefTree = map[int]proc{
	10: {1, "/usr/bin/tmux"},
	20: {10, "/usr/bin/node"},
	30: {20, "/usr/bin/dash"},
	31: {30, "/usr/local/bin/ccd"},
	40: {20, "/usr/bin/bash"},
	41: {40, "/usr/bin/node"},
	50: {41, "/usr/bin/dash"},
	51: {50, "/usr/local/bin/ccd"},
	// A hook whose shell exec'd it: its parent IS the CLI.
	60: {20, "/usr/local/bin/ccd"},
	// A process outside the pane's tree.
	70: {1, "/usr/local/bin/ccd"},
}

// 🔴 ONLY THE PRIMARY'S OWN PROCESS FIRED ITS HOOK: the nested CLI's hook has
// the primary as an ancestor too, and is told apart by the nearer process
// running the same program.
func TestHookFiredByTellsThePrimaryFromANestedCLI(t *testing.T) {
	root := fakeProc(t, chiefTree)
	for _, c := range []struct {
		cli, hook int
		want      bool
	}{
		{20, 31, true},  // the primary's own hook, through its shell
		{20, 60, true},  // the primary's own hook, the shell exec'd
		{20, 51, false}, // a nested CLI's hook
		{41, 51, true},  // ...which IS the nested CLI's
		{20, 70, false}, // not under the pane at all
		{20, 999, false},
		{999, 31, false},
		{0, 31, false},
	} {
		if got := hookFiredBy(root, c.cli, c.hook); got != c.want {
			t.Errorf("hookFiredBy(cli %d, hook %d) = %v, want %v", c.cli, c.hook, got, c.want)
		}
	}
	// A pid cycle in a broken tree ends the walk.
	loop := fakeProc(t, map[int]proc{20: {10, "/usr/bin/node"}, 80: {81, "/usr/local/bin/ccd"}, 81: {82, "/x"}, 82: {81, "/y"}})
	if hookFiredBy(loop, 20, 80) {
		t.Error("a cycle reached the CLI")
	}
}

// 🔴 A NESTED CLI'S SessionStart IS NOT RECORDED AS THE PRIMARY'S — through the
// hook listener, with the hook's pid as `ccd hook` sends it. The primary's own
// is; one with no pid, or before the pane's process is known, is not.
func TestOnlyThePrimaryProcessSessionStartIsRecorded(t *testing.T) {
	old := procRoot
	procRoot = fakeProc(t, chiefTree)
	t.Cleanup(func() { procRoot = old })
	cfg := t.TempDir()
	primary := newServer(serverConfig{ConfigDir: cfg}, nil, newAuthTracker())
	primary.recordsPrimary = true
	pane := 20
	primary.primaryPID = func(context.Context) (int, error) {
		if pane == 0 {
			return 0, fmt.Errorf("no pane yet")
		}
		return pane, nil
	}
	hp := hooks(t, primary)
	record := func() string {
		b, _ := os.ReadFile(filepath.Join(cfg, primaryRecordFile))
		return strings.TrimSpace(string(b))
	}
	start := func(q, sid string) {
		t.Helper()
		hp.mustPost(t, "SessionStart"+q, `{"hook_event_name":"SessionStart","session_id":"`+sid+`"}`)
	}
	const own, nested, other = "0f0e0d0c-0b0a-4908-8706-050403020100", "11111111-2222-4333-8444-555555555555", "22222222-3333-4444-8555-666666666666"
	start("?pid=31", own)
	if got := record(); got != own {
		t.Fatalf("the primary's own SessionStart: recorded %q, want %q", got, own)
	}
	start("?pid=51", nested)
	if got := record(); got != own {
		t.Fatalf("a nested CLI's SessionStart replaced the record: %q, want %q", got, own)
	}
	for _, q := range []string{"", "?pid=", "?pid=x", "?pid=70"} {
		start(q, other)
		if got := record(); got != own {
			t.Fatalf("SessionStart%s: recorded %q, want the record kept at %q", q, got, own)
		}
	}
	pane = 0
	start("?pid=31", other)
	if got := record(); got != own {
		t.Fatalf("with no pane process known: recorded %q", got)
	}
	// The hook still REACHES the primary: only the record is gated.
	if !primary.started() {
		t.Fatal("an unrecorded SessionStart no longer reached the primary's server")
	}
}

// Every argv is handed to tmux behind `env`, which execs it, so the pane's
// process is the CLI and not a `sh -c` around it (⚠ an INVARIANT pin: on this
// host's /bin/sh, bash, which execs a lone `-c` command, the end-to-end test
// passes without it), and with CLAUDE_CODE_ENTRYPOINT unset (the end-to-end
// check is TestTmuxLaneTwoSessionsRunInTheirOwnCLIsOnTheirOwnModels).
func TestEveryArgvIsExecdWithoutAnInheritedEntrypoint(t *testing.T) {
	if got := strings.Join(execArgv([]string{"claude"}), " "); got != "env -u CLAUDE_CODE_ENTRYPOINT claude" {
		t.Errorf("lone argv: %q", got)
	}
	if got := strings.Join(execArgv([]string{"env", "CCD_LANE=x", "claude", "--resume", "id"}), " "); got != "env -u CLAUDE_CODE_ENTRYPOINT env CCD_LANE=x claude --resume id" {
		t.Errorf("argv: %q", got)
	}
}

// `ccd hook` names its own pid, beside its lane.
func TestRunHookSendsItsPID(t *testing.T) {
	var got []string
	unixHookTarget(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.Query().Get("pid")+"|"+r.URL.Query().Get("lane"))
	}))
	t.Setenv("CCD_LANE", "")
	runHook([]string{"SessionStart"}, strings.NewReader(`{}`), io.Discard)
	t.Setenv("CCD_LANE", laneID("k"))
	runHook([]string{"SessionStart"}, strings.NewReader(`{}`), io.Discard)
	me := strconv.Itoa(os.Getpid())
	if want := me + "|," + me + "|" + laneID("k"); strings.Join(got, ",") != want {
		t.Fatalf("hook queries %v, want %s", got, want)
	}
}

// 🔴 THE PRIMARY'S HOOK PATH AS main.go WIRES IT (server.supervisePrimary): the
// CLI's SessionStart reaches ccd while the supervisor's first Start has not
// returned the pane yet, and is still recorded as the primary's — the hook
// waits for that start rather than reading "no pane yet". Red when
// supervisePrimary reads the pane with sup.ref() instead of sup.cliPID.
func TestThePrimarysSessionStartIsRecordedWhileItsFirstStartIsPending(t *testing.T) {
	old := procRoot
	procRoot = fakeProc(t, chiefTree)
	t.Cleanup(func() { procRoot = old })
	cfg := t.TempDir()
	pane := &gatedPane{release: make(chan struct{})}
	sup := newSupervisor(pane, "claude", cfg, "/data/workspace")
	sup.poll = 10 * time.Millisecond
	srv := newServer(serverConfig{ConfigDir: cfg}, pane, newAuthTracker())
	// tmuxTerminal.PanePID's contract: the zero ref is "no pane yet".
	srv.supervisePrimary(sup, func(_ context.Context, ref paneRef) (int, error) {
		if ref != fakeRef {
			return 0, fmt.Errorf("no pane yet")
		}
		return 20, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan struct{})
	go func() { defer close(ran); _ = sup.run(ctx) }()
	t.Cleanup(func() { cancel(); <-ran })
	hp := hooks(t, srv)

	const own = "0f0e0d0c-0b0a-4908-8706-050403020100"
	done := make(chan error, 1)
	go func() {
		_, err := hp.post("SessionStart?pid=31", `{"hook_event_name":"SessionStart","session_id":"`+own+`"}`)
		done <- err
	}()
	// The hook is in flight before the start lands.
	time.Sleep(200 * time.Millisecond)
	if st := sup.state(); st.CLI != cliStarting {
		t.Fatalf("precondition: the supervisor is %q, want %q", st.CLI, cliStarting)
	}
	close(pane.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the hook never answered")
	}
	b, _ := os.ReadFile(filepath.Join(cfg, primaryRecordFile))
	if got := strings.TrimSpace(string(b)); got != own {
		t.Fatalf("the primary's SessionStart during its first start: recorded %q, want %q", got, own)
	}
}
