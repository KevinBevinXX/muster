//go:build tmuxit

package main

// The real-tmux suite: ccd driving a REAL tmux server whose pane runs the fake
// CLI (fakeclaude_test.go), with the image's REAL settings template wired to the
// REAL `ccd hook` subcommand, and the turn sent through muster's own client.
//
//	go test -tags tmuxit -race -count=1 -run TestTmux ./cmd/ccd/
//
// 🔴 IT NEVER SKIPS. A missing tmux is a FAILURE, because a suite that skips on
// the runner that lacks its tool reports green having tested nothing. The build
// tag keeps it out of `go test ./...` (and out of the nix sandbox, which has no
// tmux); the CI job that runs it installs tmux and asserts it is there.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type tmuxRig struct {
	srv     *server
	gwURL   string
	cfg, ws string
	tmux    tmuxTerminal
	pane    paneRef // the CLI's pane
}

func startTmuxRig(t *testing.T, trustWorkspace bool) *tmuxRig {
	t.Helper()
	tmuxBin, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal("tmux is REQUIRED by the tmuxit suite and is not on PATH; install it (this suite never skips)")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	cfg := filepath.Join(root, "claude")
	ws := filepath.Join(root, "workspace")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, as := range map[string]string{"ccd": "ccd", "claude": "claude"} {
		shim := fmt.Sprintf("#!/bin/sh\nCCD_TEST_AS=%s exec %q \"$@\"\n", as, exe)
		if err := os.WriteFile(filepath.Join(bin, name), []byte(shim), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	seedWS := ws
	if !trustWorkspace {
		seedWS = filepath.Join(root, "some-other-workspace")
	}
	seedInto(t, cfg, seedWS)
	if err := os.MkdirAll(ws, 0o700); err != nil {
		t.Fatal(err)
	}

	sock := fmt.Sprintf("ccd-it-%d-%d", os.Getpid(), time.Now().UnixNano())
	term := tmuxTerminal{bin: tmuxBin, socket: sock, target: "cc", enterDelay: 150 * time.Millisecond}
	bearer, _ := resolveBearer(knownHooksToken, "")
	srv := newServer(serverConfig{Bearer: bearer, ConfigDir: cfg, SubmitTimeout: 10 * time.Second,
		TurnTimeout: 30 * time.Second}, term, newAuthTracker())

	hl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: srv.hookHandler()}
	go hs.Serve(hl)
	t.Cleanup(func() { hs.Close() })
	gw := gatewayServer(t, srv)

	cmd := exec.Command(tmuxBin, "-L", sock, "-f", "/dev/null", "new-session", "-d", "-P", "-F", serverFormat+" #{pane_id}",
		"-s", "cc", "-x", "200", "-y", "50", "-c", ws, filepath.Join(bin, "claude"))
	// The environment goes on the tmux CLIENT that starts this private server, so
	// it becomes the server's global environment. (`new-session -e` was not
	// applied to PATH for the pane's command on tmux 3.7c, which left the
	// `ccd` shim unresolvable from the hooks.) SHELL=/bin/sh because tmux runs
	// the pane command through $SHELL, and a shell that reads rc files can
	// rewrite PATH.
	cmd.Env = append(os.Environ(),
		"PATH="+bin+":"+os.Getenv("PATH"),
		"CLAUDE_CONFIG_DIR="+cfg,
		"CCD_HOOK_URL=http://"+hl.Addr().String(),
		"SHELL=/bin/sh")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("tmux new-session: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command(tmuxBin, "-L", sock, "kill-server").Run() })
	// This rig starts the CLI itself (no supervisor), so it names the pane the way
	// the supervisor does in production: by the server identity and pane id
	// new-session printed.
	f := strings.Fields(string(out))
	if len(f) != 3 || !strings.HasPrefix(f[2], "%") {
		t.Fatalf("new-session printed %q, not a server identity and pane id", out)
	}
	pane := paneRef{Server: f[0] + " " + f[1], Pane: f[2]}
	srv.inputPane = func() (paneRef, bool) { return pane, true }
	return &tmuxRig{srv: srv, gwURL: gw.URL, cfg: cfg, ws: ws, tmux: term, pane: pane}
}

func (r *tmuxRig) started() bool {
	r.srv.mu.Lock()
	defer r.srv.mu.Unlock()
	return r.srv.sessionStarted
}

func (r *tmuxRig) waitStarted(t *testing.T) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if r.started() {
			return
		}
	}
	pane, _ := exec.Command(r.tmux.bin, "-L", r.tmux.socket, "capture-pane", "-p", "-t", "cc").CombinedOutput()
	hooklog, _ := os.ReadFile(filepath.Join(r.cfg, "fake-hooks.log"))
	t.Fatalf("no SessionStart hook reached ccd within 15s; the pane shows:\n%s\nhook log:\n%s", pane, hooklog)
}

// transcriptHas reports whether some user record's content is exactly s.
func (r *tmuxRig) transcriptHas(t *testing.T, s string) bool {
	t.Helper()
	for _, v := range r.userRecords(t) {
		if v == s {
			return true
		}
	}
	return false
}

// userRecords returns promptId -> prompt content from the transcript the fake wrote.
func (r *tmuxRig) userRecords(t *testing.T) map[string]string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(r.cfg, "projects", "*", "*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("transcripts: %v", files)
	}
	f, err := os.Open(files[0])
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := map[string]string{}
	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var rec struct {
			Type     string `json:"type"`
			PromptID string `json:"promptId"`
			Message  struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) != nil || rec.Type != "user" {
			continue
		}
		var s string
		if json.Unmarshal(rec.Message.Content, &s) == nil {
			key := rec.PromptID
			if key == "" { // a local command or shell record: no prompt id
				n++
				key = fmt.Sprintf("local-%d", n)
			}
			out[key] = s
		}
	}
	return out
}

func TestTmuxPastedBytesArriveExactlyAndTheReplyComesBack(t *testing.T) {
	rig := startTmuxRig(t, true)
	rig.waitStarted(t)
	gw := mustersGateway(t, rig.gwURL)

	prompts := []string{
		"single line",
		"para one\n\npara two with \"quotes\" and $HOME and ${PATH} and `backticks` and $(echo no)",
		"trailing newline kept inside\nthe paste\n\n\nthree blank lines above; 'single' \\backslash\\ ✓ é 日本",
	}
	for _, p := range prompts {
		reply, err := gw.Chat(context.Background(), contractAgent, "s", p, nil)
		if err != nil {
			t.Fatalf("prompt %q: %v", p, err)
		}
		sum := sha256.Sum256([]byte(p))
		want := fmt.Sprintf("received %d bytes\n\nsha256 %s", len(p), hex.EncodeToString(sum[:]))
		if reply != want {
			t.Fatalf("prompt %q:\nreply %q\nwant  %q", p, reply, want)
		}
	}
	// A leading `!` would run in a shell in the real CLI (and here); ccd prefixes
	// one space, so it reaches the model as text.
	for _, p := range []string{"!echo this must not run in a shell", "!ls"} {
		reply, err := gw.Chat(context.Background(), contractAgent, "s", p, nil)
		if err != nil {
			t.Fatalf("prompt %q: %v", p, err)
		}
		sum := sha256.Sum256([]byte(" " + p))
		if want := fmt.Sprintf("received %d bytes\n\nsha256 %s", len(p)+1, hex.EncodeToString(sum[:])); reply != want {
			t.Fatalf("prompt %q: reply %q, want %q", p, reply, want)
		}
		prompts = append(prompts, " "+p)
	}
	// A leading `/` is let through byte-exact. Text that names no command is an
	// ordinary turn (as on the real CLI)...
	{
		p := "/etc/hosts is broken"
		reply, err := gw.Chat(context.Background(), contractAgent, "s", p, nil)
		sum := sha256.Sum256([]byte(p))
		if want := fmt.Sprintf("received %d bytes\n\nsha256 %s", len(p), hex.EncodeToString(sum[:])); err != nil || reply != want {
			t.Fatalf("prompt %q: reply %q err %v, want %q", p, reply, err, want)
		}
		prompts = append(prompts, p)
	}
	// ...and a command runs in the CLI, reaching it byte-exact, with a typed
	// local_command error to the caller (there is no model turn to reply).
	if _, err := gw.Chat(context.Background(), contractAgent, "s", "/compact", nil); err == nil ||
		!strings.Contains(err.Error(), `"type":"local_command"`) {
		t.Fatalf("/compact: err %v, want local_command", err)
	}
	if !rig.transcriptHas(t, "<local-command>/compact</local-command>") {
		t.Fatal("the CLI did not receive /compact byte-exact as a command")
	}
	// The other end of the same claim: the transcript holds each prompt verbatim.
	got := rig.userRecords(t)
	seen := map[string]bool{}
	for _, v := range got {
		seen[v] = true
	}
	for _, p := range prompts {
		if !seen[p] {
			t.Fatalf("transcript does not hold %q verbatim; it holds %q", p, got)
		}
	}
}

func TestTmuxAFailedTurnIsAnErrorThroughMustersClient(t *testing.T) {
	rig := startTmuxRig(t, true)
	rig.waitStarted(t)
	reply, err := mustersGateway(t, rig.gwURL).Chat(context.Background(), contractAgent, "s", "please FAIL-AUTH now", nil)
	if err == nil || reply != "" || !strings.Contains(err.Error(), `"type":"auth_failed"`) {
		t.Fatalf("reply %q err %v", reply, err)
	}
	// And the session is usable again afterwards.
	if _, err := mustersGateway(t, rig.gwURL).Chat(context.Background(), contractAgent, "s", "after", nil); err != nil {
		t.Fatalf("turn after a failure: %v", err)
	}
}

// Without the trust seed for the EXACT working directory, the (fake) CLI sits on
// its trust screen and fires no hooks: ccd must report not_ready rather than
// paste a prompt into the dialog.
func TestTmuxAnUntrustedWorkspaceIsNotReady(t *testing.T) {
	rig := startTmuxRig(t, false)
	time.Sleep(2 * time.Second)
	if rig.started() {
		t.Fatal("SessionStart fired in an untrusted workspace")
	}
	_, err := mustersGateway(t, rig.gwURL).Chat(context.Background(), contractAgent, "s", "hi", nil)
	if err == nil || !strings.Contains(err.Error(), `"type":"not_ready"`) {
		t.Fatalf("err = %v", err)
	}
	if err := rig.tmux.Alive(context.Background(), rig.pane); err != nil {
		t.Fatalf("control: the tmux session itself should be alive: %v", err)
	}
}

// serveRig is `ccd serve` running as its own process, configured only through
// the environment the image's entrypoint sets, creating and SUPERVISING the tmux
// session itself (CCD_SUPERVISE=1) — the production wiring end to end.
type serveRig struct {
	tmuxBin, sock string
	gwAddr        string
	root, cfg, ws string
	logs          *syncBuffer
}

// startServe runs `ccd serve`; extra is more NAME=value environment.
func startServe(t *testing.T, extra ...string) *serveRig {
	t.Helper()
	return startServeAt(t, "workspace", nil, extra...)
}

// startServeAt is startServe with the workspace at <root>/<wsRel>, and env
// computed from the rig's root (nil: none) appended to extra.
func startServeAt(t *testing.T, wsRel string, env func(root string) []string, extra ...string) *serveRig {
	t.Helper()
	tmuxBin, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal("tmux is REQUIRED by the tmuxit suite and is not on PATH; install it (this suite never skips)")
	}
	exe, _ := os.Executable()
	root := t.TempDir()
	bin, cfg, ws := filepath.Join(root, "bin"), filepath.Join(root, "claude"), filepath.Join(root, wsRel)
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, as := range map[string]string{"ccd": "ccd", "claude": "claude"} {
		shim := fmt.Sprintf("#!/bin/sh\nCCD_TEST_AS=%s exec %q \"$@\"\n", as, exe)
		if err := os.WriteFile(filepath.Join(bin, name), []byte(shim), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	seedInto(t, cfg, ws)
	free := func() string {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		return l.Addr().String()
	}
	gwAddr, hookAddr := free(), free()
	sock := fmt.Sprintf("ccd-it-bin-%d-%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() { _ = exec.Command(tmuxBin, "-L", sock, "kill-server").Run() })

	cmd := exec.Command(filepath.Join(bin, "ccd"), "serve")
	cmd.Dir = ws
	cmd.Env = append(os.Environ(),
		"PATH="+bin+":"+os.Getenv("PATH"), "SHELL=/bin/sh",
		"HOOKS_TOKEN="+knownHooksToken, "CLAUDE_CONFIG_DIR="+cfg, "CCD_WORKSPACE="+ws,
		"CCD_LISTEN="+gwAddr, "CCD_HOOK_LISTEN="+hookAddr, "CCD_HOOK_URL=http://"+hookAddr,
		"CCD_TMUX_SOCKET="+sock, "CCD_TMUX_CONF=/dev/null",
		"CCD_SUPERVISE=1", "CCD_CLAUDE_BIN="+filepath.Join(bin, "claude"))
	cmd.Env = append(cmd.Env, extra...)
	if env != nil {
		cmd.Env = append(cmd.Env, env(root)...)
	}
	logs := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = logs, logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return &serveRig{tmuxBin: tmuxBin, sock: sock, gwAddr: gwAddr, root: root, cfg: cfg, ws: ws, logs: logs}
}

func (r *serveRig) waitHealth(t *testing.T, label string, ok func(health) bool) health {
	t.Helper()
	var h health
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		resp, err := http.Get("http://" + r.gwAddr + "/healthz")
		if err != nil {
			continue
		}
		h = health{}
		_ = json.NewDecoder(resp.Body).Decode(&h)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK && ok(h) {
			return h
		}
	}
	t.Fatalf("%s: healthz never got there; last %+v sup=%+v; ccd log:\n%s", label, h, h.Supervisor, r.logs.String())
	return h
}

// primaryReady: the primary can take a turn — its CLI has sent SessionStart AND
// the supervisor has recorded it running (its pane is the input pane). /healthz
// reports the two separately and either can come first: tmux new-session runs
// the CLI before it returns the pane, so `session: started` while the
// supervisor is still `starting` is a real state, in which a turn is 503
// not_ready. Waiting on Session alone made the first chat race the start.
func primaryReady(h health) bool {
	return h.Session == sessionStartedState && h.Supervisor != nil && h.Supervisor.CLI == cliRunning
}

// slowNewSession is a startServeAt env func: it puts a `tmux` first on ccd's
// PATH that sleeps d after each successful new-session, after the pane id is
// printed — so every CLI (the primary's and each lane's) is running, and
// hooking, for d before ccd learns its pane. The window startedRef,
// supervisePrimary and lane.waitStarted must survive.
func slowNewSession(t *testing.T, d time.Duration) func(root string) []string {
	t.Helper()
	return func(root string) []string {
		real, err := exec.LookPath("tmux")
		if err != nil {
			t.Fatal(err)
		}
		script := fmt.Sprintf("#!/bin/sh\n%q \"$@\"\nrc=$?\nif [ $rc -eq 0 ]; then\n  for a in \"$@\"; do\n    if [ \"$a\" = new-session ]; then sleep %g; break; fi\n  done\nfi\nexit $rc\n", real, d.Seconds())
		if err := os.WriteFile(filepath.Join(root, "bin", "tmux"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		return nil
	}
}

// chat sends p through muster's client and requires the fake CLI's reply for
// exactly those bytes.
func (r *serveRig) chat(t *testing.T, p string) {
	t.Helper()
	reply, err := mustersGateway(t, "http://"+r.gwAddr).Chat(context.Background(), contractAgent, "s", p, nil)
	sum := sha256.Sum256([]byte(p))
	if want := fmt.Sprintf("received %d bytes\n\nsha256 %s", len(p), hex.EncodeToString(sum[:])); err != nil || reply != want {
		t.Fatalf("prompt %q: reply %q err %v; ccd log:\n%s", p, reply, err, r.logs.String())
	}
}

func (r *serveRig) tmux(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command(r.tmuxBin, append([]string{"-L", r.sock}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("tmux %q: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// waitFile reports whether path appears within d.
func waitFile(path string, d time.Duration) bool {
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}

// The WHOLE BINARY, supervising:
//
//   - health is 200 with no credential proof (there is no probe) and reports a
//     FRESH supervised start;
//   - a turn round-trips through muster's client;
//   - 🔴 `/exit` typed in the pane restarts the CLI IN THE POD with --continue:
//     the same session id, ONE transcript file holding the prompts from before
//     and after, and a turn works again.
func TestTmuxTheServeBinarySupervisesTheSessionAndResumesAfterExit(t *testing.T) {
	r := startServe(t)
	h := r.waitHealth(t, "first start", primaryReady)
	if h.Terminal != "ok" || h.Auth != authUnknown || h.Supervisor == nil ||
		h.Supervisor.CLI != cliRunning || h.Supervisor.Mode != modeFresh || h.Supervisor.Starts != 1 {
		t.Fatalf("after the first start: %+v sup=%+v", h, h.Supervisor)
	}
	r.chat(t, "before the exit")

	// The operator types /exit in the attached terminal.
	r.tmux(t, "send-keys", "-t", "cc", "-l", "/exit")
	r.tmux(t, "send-keys", "-t", "cc", "Enter")
	h = r.waitHealth(t, "restart after /exit", func(h health) bool {
		return primaryReady(h) && h.Supervisor.Starts == 2
	})
	if h.Supervisor.Mode != modeContinue || h.Supervisor.LastExit == nil || *h.Supervisor.LastExit != 0 {
		t.Fatalf("after /exit: sup=%+v", h.Supervisor)
	}
	r.chat(t, "after the exit")

	files, _ := filepath.Glob(filepath.Join(r.cfg, "projects", "*", "*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("want ONE transcript (the conversation resumed, not forked), have %v", files)
	}
	b, _ := os.ReadFile(files[0])
	for _, p := range []string{`"content":"before the exit"`, `"content":"after the exit"`} {
		if !strings.Contains(string(b), p) {
			t.Fatalf("the one transcript lacks %s", p)
		}
	}
}

// 🔴 F1: AN OPERATOR'S SPLIT PANE NEVER RECEIVES A TURN. The operator splits the
// window and leaves a SHELL pane focused — the session's active pane. A turn
// whose text is a shell command must reach the CLI (the fake's reply hashes the
// exact bytes) and must not run in the shell (no marker file).
//
// Positive control, built WITHOUT ccd: the same command sent to the session name
// (what the code before this fix pasted into) does create a marker — the shell
// pane is live and would have run it.
func TestTmuxATurnGoesToTheCLIPaneNotTheFocusedShellPane(t *testing.T) {
	r := startServe(t)
	r.waitHealth(t, "first start", primaryReady)
	cliPane := r.tmux(t, "display-message", "-p", "-t", "cc", "#{pane_id}")

	shellPane := r.tmux(t, "split-window", "-t", "cc", "-P", "-F", "#{pane_id}", "-c", r.root, "/bin/sh")
	if active := r.tmux(t, "display-message", "-p", "-t", "cc", "#{pane_id}"); active != shellPane || active == cliPane {
		t.Fatalf("setup: the session's active pane is %s, want the new shell pane %s (CLI pane %s)", active, shellPane, cliPane)
	}

	marker := filepath.Join(r.root, "turn-ran-in-the-shell")
	p := "touch " + marker
	reply, err := mustersGateway(t, "http://"+r.gwAddr).Chat(context.Background(), contractAgent, "s", p, nil)
	if waitFile(marker, time.Second) {
		t.Fatalf("the turn ran in the operator's focused shell pane %s: %s exists (turn err %v)", shellPane, marker, err)
	}
	sum := sha256.Sum256([]byte(p))
	if want := fmt.Sprintf("received %d bytes\n\nsha256 %s", len(p), hex.EncodeToString(sum[:])); err != nil || reply != want {
		t.Fatalf("the CLI did not receive the turn's bytes: reply %q err %v", reply, err)
	}

	control := filepath.Join(r.root, "control-shell-is-live")
	r.tmux(t, "send-keys", "-t", "cc", "-l", "touch "+control)
	r.tmux(t, "send-keys", "-t", "cc", "Enter")
	if !waitFile(control, 5*time.Second) {
		t.Fatal("control: a command sent to the SESSION name did not run in the focused shell pane, so this test could not see the bug")
	}
}

// recreateWithShell starts a new tmux server on sock with a session `cc` running a
// shell in dir, and returns its pane id. A new-session racing the old server's
// exit can fail ("server exited unexpectedly", measured on 3.7c), so it retries
// until the old server is gone.
func recreateWithShell(t *testing.T, tmuxBin, sock, dir string) string {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		out, err := exec.Command(tmuxBin, "-L", sock, "-f", "/dev/null", "new-session", "-d", "-P", "-F", "#{pane_id}",
			"-s", "cc", "-c", dir, "/bin/sh").CombinedOutput()
		if err == nil {
			return strings.TrimSpace(string(out))
		}
		if time.Now().After(deadline) {
			t.Fatalf("recreate the tmux server: %v: %s", err, out)
		}
	}
}

// 🔴 R2-F1: A RECREATED tmux SERVER NEVER RECEIVES A TURN, AND HEALTH SAYS SO.
// The tmux server ccd started goes away (`kill-server`) and someone recreates a
// session named `cc` on the same socket with a SHELL in it. The new server numbers
// its panes from %0 again, so the shell pane carries the very id ccd recorded for
// its CLI. A turn whose text is a shell command must not run there (no marker),
// must be refused as not_ready, and /healthz must stop answering 200 and report
// the supervisor's CLI as lost — never `running`.
//
// Positive controls, built WITHOUT ccd: the recreated pane really does carry the
// recorded id (otherwise this test could not see the bug), and a command sent to
// it does run (the shell is live).
func TestTmuxARecreatedServerWithARecycledPaneIDNeverReceivesATurn(t *testing.T) {
	r := startServe(t)
	r.waitHealth(t, "first start", primaryReady)
	cliPane := r.tmux(t, "display-message", "-p", "-t", "cc", "#{pane_id}")

	r.tmux(t, "kill-server")
	shellPane := recreateWithShell(t, r.tmuxBin, r.sock, r.root)
	if shellPane != cliPane {
		t.Fatalf("control: the recreated server's shell pane is %s, not the recycled id %s — this test "+
			"could not see the bug", shellPane, cliPane)
	}

	marker := filepath.Join(r.root, "turn-ran-in-the-recreated-shell")
	_, err := mustersGateway(t, "http://"+r.gwAddr).Chat(context.Background(), contractAgent, "s", "touch "+marker, nil)
	if waitFile(marker, 2*time.Second) {
		t.Fatalf("the turn ran in the recreated server's shell pane %s: %s exists (turn err %v)", shellPane, marker, err)
	}
	if err == nil || !strings.Contains(err.Error(), `"type":"not_ready"`) {
		t.Fatalf("turn into a recreated server: err %v, want not_ready", err)
	}

	var code int
	var h health
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		resp, err := http.Get("http://" + r.gwAddr + "/healthz")
		if err != nil {
			continue
		}
		h = health{}
		_ = json.NewDecoder(resp.Body).Decode(&h)
		resp.Body.Close()
		code = resp.StatusCode
		if code != http.StatusOK && h.Supervisor != nil && h.Supervisor.CLI == "terminal_lost" {
			break
		}
	}
	if code != http.StatusServiceUnavailable || h.Supervisor == nil || h.Supervisor.CLI != "terminal_lost" {
		t.Fatalf("healthz after the server was recreated: %d %+v sup=%+v, want 503 with cli \"terminal_lost\"; ccd log:\n%s",
			code, h, h.Supervisor, r.logs.String())
	}

	control := filepath.Join(r.root, "control-recreated-shell-is-live")
	r.tmux(t, "send-keys", "-t", shellPane, "-l", "touch "+control)
	r.tmux(t, "send-keys", "-t", shellPane, "Enter")
	if !waitFile(control, 5*time.Second) {
		t.Fatal("control: a command sent to the recycled pane id did not run in the shell, so this test could not see the bug")
	}
}

// 🔴 F2: A SMUGGLED PASTE TERMINATOR RUNS NOTHING. "\x1b[201~!touch X" ends the
// bracketed paste at once and leaves `!touch X` TYPED at an empty prompt — shell
// mode. ccd refuses it (400 invalid_input) and no marker appears.
//
// Positive control, built WITHOUT ccd's sanitiser: the same bytes pasted straight
// into the CLI's pane by tmux do run the command, so the fake (like the real CLI)
// would have executed it.
func TestTmuxASmuggledPasteTerminatorIsRefusedAndRunsNothing(t *testing.T) {
	r := startServe(t)
	r.waitHealth(t, "first start", primaryReady)
	cliPane := r.tmux(t, "display-message", "-p", "-t", "cc", "#{pane_id}")

	marker := filepath.Join(r.root, "smuggled")
	_, err := mustersGateway(t, "http://"+r.gwAddr).Chat(context.Background(), contractAgent, "s",
		"\x1b[201~!touch "+marker, nil)
	if waitFile(marker, 2*time.Second) {
		t.Fatalf("the smuggled `!` ran in shell mode: %s exists (turn err %v)", marker, err)
	}
	if err == nil || !strings.Contains(err.Error(), `"type":"invalid_input"`) {
		t.Fatalf("err %v, want invalid_input", err)
	}
	r.chat(t, "the session is still usable")

	control := filepath.Join(r.root, "control-smuggle-runs")
	load := exec.Command(r.tmuxBin, "-L", r.sock, "load-buffer", "-b", "ctl", "-")
	load.Stdin = strings.NewReader("\x1b[201~!touch " + control)
	if out, err := load.CombinedOutput(); err != nil {
		t.Fatalf("load-buffer: %v: %s", err, out)
	}
	r.tmux(t, "paste-buffer", "-p", "-d", "-b", "ctl", "-t", cliPane)
	time.Sleep(150 * time.Millisecond)
	r.tmux(t, "send-keys", "-t", cliPane, "Enter")
	if waitFile(control, 5*time.Second) {
		return
	}
	// ⚠ A tmux that escapes ESC inside a bracketed paste ITSELF (measured: 3.7c
	// delivers it as the two characters "^[") closes this hole on its own, and
	// on it the marker half above cannot fail. tmux 3.3a (the image's) and 3.4
	// (CI's) deliver ESC raw — measured — so CI sets CCD_TMUXIT_RAW_ESC=1 and
	// this control must fire there.
	ver, _ := exec.Command(r.tmuxBin, "-V").Output()
	if os.Getenv("CCD_TMUXIT_RAW_ESC") == "1" {
		t.Fatalf("control: the smuggle shape pasted directly did not run on %s, and CCD_TMUXIT_RAW_ESC=1 "+
			"says this tmux delivers ESC raw — this test could not see the bug", bytes.TrimSpace(ver))
	}
	files, _ := filepath.Glob(filepath.Join(r.cfg, "projects", "*", "*.jsonl"))
	var tr []byte
	if len(files) == 1 {
		tr, _ = os.ReadFile(files[0])
	}
	if !bytes.Contains(tr, []byte(`"content":"^[[201~!touch `+control)) {
		t.Fatalf("control: the smuggle shape pasted directly neither ran nor arrived escaped as \"^[\"; "+
			"this test could not see the bug on %s", bytes.TrimSpace(ver))
	}
	t.Logf("⚠ %s escapes ESC inside a bracketed paste itself, so the no-marker assertion above is "+
		"VACUOUS on this host; it is live where CCD_TMUXIT_RAW_ESC=1 (CI). The refusal half "+
		"(invalid_input) was asserted here regardless.", bytes.TrimSpace(ver))
}

// 🔴 F3: A CLI KILLED BY A SIGNAL IS RECORDED AS ONE. tmux reports a signal
// death with an EMPTY pane_dead_status for ever; reading that as status 0 calls a
// SIGKILL a clean exit. The supervisor records 128+9 and names the signal.
func TestTmuxASignalKilledCLIIsRecordedAsSignal9(t *testing.T) {
	r := startServe(t)
	r.waitHealth(t, "first start", primaryReady)
	pid := r.tmux(t, "display-message", "-p", "-t", "cc", "#{pane_pid}")
	if out, err := exec.Command("kill", "-9", pid).CombinedOutput(); err != nil {
		t.Fatalf("kill -9 %s: %v: %s", pid, err, out)
	}
	h := r.waitHealth(t, "restart after SIGKILL", func(h health) bool {
		return primaryReady(h) && h.Supervisor.Starts == 2
	})
	if h.Supervisor.LastExit == nil || *h.Supervisor.LastExit != 137 || !strings.Contains(h.Supervisor.Detail, "signal 9") {
		t.Fatalf("after kill -9: sup=%+v (last exit %v), want 137 and signal 9 named", h.Supervisor, derefInt(h.Supervisor.LastExit))
	}
	r.chat(t, "after the kill")
}

func derefInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// tmuxTerminal.Paste refuses a pane whose process has exited (errPaneNotLive)
// instead of pasting into it, and a pane id that does not exist; the server
// turns that into not_ready. Control: the same terminal pastes into a live pane.
func TestTmuxPasteRefusesADeadOrMissingPane(t *testing.T) {
	tmuxBin, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal("tmux is REQUIRED by the tmuxit suite and is not on PATH; install it (this suite never skips)")
	}
	sock := fmt.Sprintf("ccd-it-dead-%d-%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() { _ = exec.Command(tmuxBin, "-L", sock, "kill-server").Run() })
	term := tmuxTerminal{bin: tmuxBin, socket: sock, conf: "/dev/null", target: "cc"}
	ctx := context.Background()
	live, err := term.Start(ctx, t.TempDir(), []string{"sleep", "60"})
	if err != nil {
		t.Fatal(err)
	}
	if err := term.Paste(ctx, live, "x"); err != nil {
		t.Fatalf("control: paste into a live pane: %v", err)
	}
	deadID, err := term.output(ctx, "split-window", "-t", "cc", "-P", "-F", "#{pane_id}", "true")
	if err != nil {
		t.Fatal(err)
	}
	dead := paneRef{Server: live.Server, Pane: strings.TrimSpace(deadID)}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if e, err := term.Exited(ctx, dead); err == nil && e.Dead {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pane %v never died", dead)
		}
	}
	for _, p := range []paneRef{dead, {Server: live.Server, Pane: "%999"}} {
		if err := term.Paste(ctx, p, "x"); !errors.Is(err, errPaneNotLive) {
			t.Fatalf("paste into %v: %v, want errPaneNotLive", p, err)
		}
	}
}

// 🔴 R2-F1 (terminal half): tmuxTerminal ITSELF refuses a pane id that a
// recreated server has recycled — Paste, Enter, Respawn, Exited and Alive — with no
// supervisor poll in front of it to have noticed first. The pane ccd started runs
// `sleep`; the server is killed and recreated with a SHELL whose pane gets the same
// id. Nothing may be typed into that shell (no marker) and Respawn must not start
// anything in it.
//
// Controls, on the recreated server: its pane carries the recycled id, the shell
// in it runs what is sent to it, and the SESSION check (zero ref) passes — so only
// the server identity tells the two apart.
func TestTmuxTheTerminalRefusesAPaneIDRecycledByARecreatedServer(t *testing.T) {
	tmuxBin, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal("tmux is REQUIRED by the tmuxit suite and is not on PATH; install it (this suite never skips)")
	}
	sock := fmt.Sprintf("ccd-it-recycle-%d-%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() { _ = exec.Command(tmuxBin, "-L", sock, "kill-server").Run() })
	term := tmuxTerminal{bin: tmuxBin, socket: sock, conf: "/dev/null", target: "cc"}
	ctx := context.Background()
	dir := t.TempDir()
	ref, err := term.Start(ctx, dir, []string{"sleep", "60"})
	if err != nil {
		t.Fatal(err)
	}
	if err := term.Alive(ctx, ref); err != nil {
		t.Fatalf("control: the pane ccd started is alive: %v", err)
	}
	if err := exec.Command(tmuxBin, "-L", sock, "kill-server").Run(); err != nil {
		t.Fatal(err)
	}
	shellPane := recreateWithShell(t, tmuxBin, sock, dir)
	if shellPane != ref.Pane {
		t.Fatalf("control: the recreated shell pane is %s, not the recycled id %s — this test could not see the bug", shellPane, ref.Pane)
	}
	if err := term.Alive(ctx, paneRef{}); err != nil {
		t.Fatalf("control: the recreated session `cc` should pass the session check: %v", err)
	}

	marker := filepath.Join(dir, "typed-into-the-recreated-shell")
	if err := term.Paste(ctx, ref, "touch "+marker); !errors.Is(err, errPaneNotLive) || !errors.Is(err, errTerminalLost) {
		t.Fatalf("Paste into the recycled id: %v, want errPaneNotLive and errTerminalLost", err)
	}
	if err := term.Enter(ctx, ref); !errors.Is(err, errTerminalLost) {
		t.Fatalf("Enter into the recycled id: %v, want errTerminalLost", err)
	}
	if _, err := term.Exited(ctx, ref); !errors.Is(err, errTerminalLost) {
		t.Fatalf("Exited on the recycled id: %v, want errTerminalLost", err)
	}
	if err := term.Alive(ctx, ref); !errors.Is(err, errTerminalLost) {
		t.Fatalf("Alive for the recycled id: %v, want errTerminalLost", err)
	}
	respawned := filepath.Join(dir, "respawned-in-the-recreated-server")
	if err := term.Respawn(ctx, ref, dir, []string{"touch", respawned}); !errors.Is(err, errTerminalLost) {
		t.Fatalf("Respawn on the recycled id: %v, want errTerminalLost", err)
	}
	// Enter the shell's own line, so anything typed above would have run by now.
	if out, err := exec.Command(tmuxBin, "-L", sock, "send-keys", "-t", shellPane, "Enter").CombinedOutput(); err != nil {
		t.Fatalf("send-keys: %v: %s", err, out)
	}
	if waitFile(marker, time.Second) || waitFile(respawned, 10*time.Millisecond) {
		t.Fatal("the terminal acted on the recreated server's pane")
	}

	control := filepath.Join(dir, "control-recycled-shell-runs")
	if out, err := exec.Command(tmuxBin, "-L", sock, "send-keys", "-t", shellPane, "-l", "touch "+control).CombinedOutput(); err != nil {
		t.Fatalf("send-keys: %v: %s", err, out)
	}
	if out, err := exec.Command(tmuxBin, "-L", sock, "send-keys", "-t", shellPane, "Enter").CombinedOutput(); err != nil {
		t.Fatalf("send-keys: %v: %s", err, out)
	}
	if !waitFile(control, 5*time.Second) {
		t.Fatal("control: the recreated shell did not run a command sent to it — this test could not see the bug")
	}
}

// 🔴 A CLI THAT DIES AT ONCE, IN A REAL tmux PANE: the supervisor restarts it with
// backoff, then reports crash_loop and /healthz turns 503. Also proves the
// remain-on-exit ordering: a process that exits immediately still leaves a dead
// pane to read its status from.
func TestTmuxACrashingCLIEndsInCrashLoopAndFailsHealth(t *testing.T) {
	tmuxBin, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal("tmux is REQUIRED by the tmuxit suite and is not on PATH; install it (this suite never skips)")
	}
	exe, _ := os.Executable()
	root := t.TempDir()
	shim := filepath.Join(root, "claude")
	if err := os.WriteFile(shim, []byte(fmt.Sprintf("#!/bin/sh\nCCD_TEST_AS=claude CCD_FAKE_CRASH=1 exec %q \"$@\"\n", exe)), 0o755); err != nil {
		t.Fatal(err)
	}
	sock := fmt.Sprintf("ccd-it-crash-%d-%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() { _ = exec.Command(tmuxBin, "-L", sock, "kill-server").Run() })
	term := tmuxTerminal{bin: tmuxBin, socket: sock, conf: "/dev/null", target: "cc"}
	srv := newServer(serverConfig{Bearer: "x", ConfigDir: root}, term, newAuthTracker())
	sup := newSupervisor(term, shim, root, root)
	sup.poll, sup.backoffMin = 20*time.Millisecond, 10*time.Millisecond
	srv.sup = sup
	gw := gatewayServer(t, srv)

	done := make(chan error, 1)
	go func() { done <- sup.run(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("no crash loop within 20s: %+v", sup.state())
	}
	st := sup.state()
	if st.CLI != cliCrashLoop || st.Starts != 5 || st.LastExit == nil || *st.LastExit != 3 {
		t.Fatalf("state %+v", st)
	}
	resp, err := http.Get(gw.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("healthz %d during a crash loop, want 503", resp.StatusCode)
	}
	// Control: the tmux session itself is alive (the dead pane is kept), so the
	// 503 is the crash loop's, not a dead terminal's.
	if err := term.Alive(context.Background(), sup.ref()); err != nil {
		t.Fatalf("control: %v", err)
	}
}

// syncBuffer is a strings.Builder safe to write from a child's two streams.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
