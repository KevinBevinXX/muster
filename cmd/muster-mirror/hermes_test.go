package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/hermeshost"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
)

// TestMain lets a test run THIS binary as `muster-mirror …` in a child
// process (hookProcess), so the hook's real stdout and exit code are what is
// checked, not a writer a test passed in.
func TestMain(m *testing.M) {
	if os.Getenv("MUSTER_MIRROR_TEST_AS_MAIN") == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

// hookProcess runs `muster-mirror hermes-hook` as a child process with env and
// stdin, and returns its exit code, stdout and stderr.
func hookProcess(t *testing.T, env []string, stdin string) (int, string, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "hermes-hook")
	cmd.Env = append([]string{"MUSTER_MIRROR_TEST_AS_MAIN=1", "PATH=" + os.Getenv("PATH")}, env...)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return code, out.String(), errb.String()
}

func hookPayload(event, session string) string {
	b, _ := json.Marshal(map[string]any{"hook_event_name": event, "session_id": session, "cwd": "/w",
		"extra": map[string]any{"kind": "approval", "request_id": "0123456789abcdef0123456789abcdef", "prompt": "rm -rf /"}})
	return string(b)
}

// 🔴 THE HOOK's CONTRACT, measured on the real process: for a status it
// writes, one it ignores, a payload that is not JSON, and a status directory
// it cannot write — stdout is EMPTY and the exit code is 0, every time.
func TestTheHermesHookPrintsNothingAndExitsZero(t *testing.T) {
	home := t.TempDir()
	id := "local-" + strings.Repeat("a", 64)
	statusDir := filepath.Join(home, ".muster", "hermes-status")
	cases := []struct {
		name, stdin string
		prep        func()
		wrote       bool
	}{
		{"a status it writes", hookPayload("pre_llm_call", id), func() {}, true},
		{"an event it ignores", hookPayload("pre_tool_call", id), func() {}, false},
		{"an invalid session id", hookPayload("pre_llm_call", "../../x"), func() {}, false},
		{"not JSON", "{{{", func() {}, false},
		{"an unwritable directory", hookPayload("on_session_end", id), func() {
			_ = os.RemoveAll(statusDir)
			must(t, os.MkdirAll(filepath.Dir(statusDir), 0o700))
			must(t, os.WriteFile(statusDir, []byte("a file, not a directory"), 0o600))
		}, false},
	}
	for _, c := range cases {
		_ = os.RemoveAll(statusDir)
		c.prep()
		code, out, _ := hookProcess(t, []string{"HOME=" + home}, c.stdin)
		if code != 0 || out != "" {
			t.Errorf("%s: exit %d, stdout %q — want 0 and nothing", c.name, code, out)
		}
		_, err := os.Stat(filepath.Join(statusDir, id+".json"))
		if (err == nil) != c.wrote {
			t.Errorf("%s: status file written = %v, want %v", c.name, err == nil, c.wrote)
		}
	}
}

// 🔴 The hook never reads TMUX_PANE (nor the tty): the gateway's TMUX_PANE is
// whichever pane started it. Two runs that differ only in TMUX_PANE (and in
// TMUX, and in having a terminal at all) write the same file, but its time.
func TestTheHermesHookIgnoresTMUXPANE(t *testing.T) {
	home := t.TempDir()
	id := "local-" + strings.Repeat("b", 64)
	read := func() hermeshost.StatusFile {
		f, err := hermeshost.StatusDir(filepath.Join(home, ".muster", "hermes-status")).Read(id)
		if err != nil {
			t.Fatal(err)
		}
		f.Since = 0
		return f
	}
	if code, out, errs := hookProcess(t, []string{"HOME=" + home, "TMUX_PANE=%1", "TMUX=/tmp/a,1,0"}, hookPayload("on_human_input_request", id)); code != 0 || out != "" {
		t.Fatalf("exit %d out %q err %s", code, out, errs)
	}
	a := read()
	time.Sleep(5 * time.Millisecond)
	if code, out, _ := hookProcess(t, []string{"HOME=" + home, "TMUX_PANE=%99"}, hookPayload("on_human_input_request", id)); code != 0 || out != "" {
		t.Fatalf("exit %d out %q", code, out)
	}
	b := read()
	if a != b {
		t.Fatalf("the status differs with TMUX_PANE:\n%+v\n%+v", a, b)
	}
	if a.Status != mirror.StatusWaiting || a.Kind != "approval" || a.WriterPID != os.Getpid() {
		t.Fatalf("status = %+v (writer must be the hook's parent, this test: %d)", a, os.Getpid())
	}
}

// The hook writes where the daemon recorded (-write-hook-config), not its
// default.
func TestTheHermesHookWritesWhereTheDaemonRecorded(t *testing.T) {
	home := t.TempDir()
	rec := filepath.Join(home, "rec.json")
	dir := filepath.Join(home, "elsewhere")
	must(t, writeHookConfig(rec, hookConfig{URL: "https://m.example", HermesStatusDir: dir}))
	id := "local-" + strings.Repeat("c", 64)
	var errb bytes.Buffer
	getenv := func(k string) string {
		return map[string]string{envHookConfig: rec, "HOME": home}[k]
	}
	if code := runHermesHook(getenv, strings.NewReader(hookPayload("on_session_end", id)), &errb, time.Now); code != 0 {
		t.Fatal(code)
	}
	if _, err := hermeshost.StatusDir(dir).Read(id); err != nil {
		t.Fatalf("not written to the recorded directory: %v (%s)", err, errb.String())
	}
}

func TestTheHermesHomeReachesTheTmuxBackend(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	prev, prevC := newTmuxBackend, runContext
	t.Cleanup(func() { newTmuxBackend, runContext = prev, prevC })
	var got *mirrorhost.HermesSource
	newTmuxBackend = func(_, _, _ string, h *mirrorhost.HermesSource) mirrorhost.Backend { got = h; return listBackend{} }
	rc, _, errs := runWith(t, map[string]string{envHermesHome: "/fx/hermes", envHermesStatusDir: "/fx/status"}, "-list")
	if rc != 0 {
		t.Fatalf("rc %d %s", rc, errs)
	}
	if got == nil || got.Home.String() != "/fx/hermes" || string(got.Status) != "/fx/status" {
		t.Fatalf("the backend's Hermes source = %+v", got)
	}
	rc, _, _ = runWith(t, map[string]string{envHermesHome: "/fx/hermes"}, "-list", "-hermes-home", "/fx/flag")
	if rc != 0 || got.Home.String() != "/fx/flag" {
		t.Fatalf("the flag did not win: %v", got.Home)
	}
	rc, _, _ = runWith(t, map[string]string{"HERMES_HOME": "/fx/hh"}, "-list")
	if rc != 0 || got.Home.String() != "/fx/hh" {
		t.Fatalf("$HERMES_HOME is not the default: %v", got.Home)
	}
}

func TestHermesAllowlistWritesThePairs(t *testing.T) {
	home := t.TempDir()
	cmd := "/nix/store/x-muster-mirror/bin/muster-mirror hermes-hook"
	rc, out, errs := runWith(t, nil, "hermes-allowlist", "-hermes-home", home, "-command", cmd)
	if rc != 0 || !strings.Contains(out, "allowlisted 5 hook events") {
		t.Fatalf("rc %d out %q err %q", rc, out, errs)
	}
	c, err := hermeshost.NewHome(home).CheckAllowlist(cmd)
	if err != nil || !c.OK() {
		t.Fatalf("%+v %v", c, err)
	}
	rc, out, _ = runWith(t, nil, "hermes-allowlist", "-hermes-home", home, "-command", cmd)
	if rc != 0 || !strings.Contains(out, "already allowlisted") {
		t.Fatalf("second run: %d %q", rc, out)
	}
	if rc, _, errs := runWith(t, nil, "hermes-allowlist", "-hermes-home", home, "-command", "sh -c evil"); rc != 1 || errs == "" {
		t.Fatalf("a bad command: rc %d", rc)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
