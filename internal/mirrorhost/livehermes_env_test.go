//go:build livehermes

package mirrorhost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/hermeshost"
)

// THE LIVE HERMES HARNESS's ENVIRONMENT (`make test-livehermes`,
// docs/design/hermes.md Phase 3): the PINNED Hermes CLI, run for real, in
//
//   - its own HERMES_HOME and HOME under a fresh temporary directory: never
//     the operator's ~/.hermes, and no credential of any kind (the model's
//     api_key is a literal stub string; auth.json and .env are never read or
//     written by this harness);
//   - a PRIVATE tmux server (-S <socket> in that directory, -f /dev/null);
//   - a local, scripted OpenAI-compatible model (stubModel) on 127.0.0.1;
//   - a private TMPDIR, short enough for the gateway's control socket.
//
// 🔴 EVERY PROCESS IT STOPS IS STOPPED BY A RESOLVED PID, AND ONLY AFTER ITS
// OWN ENVIRONMENT PROVES IT IS THIS RUN'S: /proc/<pid>/environ must hold
// HERMES_HOME=<one of this run's homes>, re-read at the moment of the kill.
// No pattern ever reaches kill.

// hermesPin is the Hermes commit muster pins (docs/design/hermes.md, Versions).
const hermesPin = "5fc490e2b34123212d73ef316086f939567a725a"

// hermesInstall is the pinned Hermes on this host: its launcher and the
// directory its Python package lives in.
type hermesInstall struct {
	launcher string // the `hermes` launcher, absolute and resolved
	python   string // its interpreter (HERMES_PYTHON)
	src      string // site-packages: hermes_cli/ and cli.py are here
	locales  string // HERMES_BUNDLED_LOCALES
}

var (
	installOnce sync.Once
	install     hermesInstall
	installErr  error
)

// pinnedHermes finds `hermes` on PATH and proves it is the pin: its Nix
// launcher exports HERMES_REVISION, which must equal hermesPin.
func pinnedHermes(t *testing.T) hermesInstall {
	t.Helper()
	installOnce.Do(func() {
		p, err := exec.LookPath("hermes")
		if err != nil {
			installErr = fmt.Errorf("no hermes on PATH: %w", err)
			return
		}
		if p, err = filepath.EvalSymlinks(p); err != nil {
			installErr = err
			return
		}
		b, err := os.ReadFile(p)
		if err != nil {
			installErr = err
			return
		}
		get := func(name string) string {
			m := regexp.MustCompile(`(?m)^export ` + name + `='([^']*)'`).FindSubmatch(b)
			if m == nil {
				return ""
			}
			return string(m[1])
		}
		if rev := get("HERMES_REVISION"); rev != hermesPin {
			installErr = fmt.Errorf("hermes at %s is revision %q, not the pin %s (move the pin only by the design's rule)", p, rev, hermesPin)
			return
		}
		install = hermesInstall{launcher: p, python: get("HERMES_PYTHON"), locales: get("HERMES_BUNDLED_LOCALES")}
		out, err := exec.Command(install.python, "-c", "import hermes_cli,os;print(os.path.dirname(os.path.dirname(hermes_cli.__file__)))").Output()
		if err != nil {
			installErr = fmt.Errorf("find the pinned source: %w", err)
			return
		}
		install.src = strings.TrimSpace(string(out))
	})
	if installErr != nil {
		t.Fatalf("livehermes: %v", installErr)
	}
	return install
}

// ---------------------------------------------------------------- the model

// stubModel is a scripted OpenAI-compatible chat-completions server. The
// reply is chosen from the newest user message:
//
//	APPROVE      a `terminal` call Hermes's approval catches (rm -r of a
//	             directory that does not exist, in the scratch work dir)
//	SUDO         a `terminal` call of `sudo true` (Hermes asks for a password;
//	             nothing ever answers it)
//	CLARIFYFREE  a `clarify` call, one open question
//	CLARIFY      a `clarify` call, one question with two choices
//	SLOW<n>      a plain reply after n seconds
//	anything     "stub reply #<n>"
//
// After a tool result the turn ends with "tool step finished". A request
// with no tools (a title, a summary) gets a short text.
type stubModel struct {
	srv   *httptest.Server
	n     atomic.Int64
	calls atomic.Int64
}

func newStubModel(t *testing.T) *stubModel {
	t.Helper()
	m := &stubModel{}
	m.srv = httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *stubModel) baseURL() string { return m.srv.URL + "/v1" }

func textOf(c any) string {
	switch v := c.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, p := range v {
			if mp, ok := p.(map[string]any); ok {
				if s, ok := mp["text"].(string); ok {
					b.WriteString(s)
					b.WriteString(" ")
				}
			}
		}
		return b.String()
	}
	return ""
}

var slowRE = regexp.MustCompile(`SLOW(\d+)`)

func (m *stubModel) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(strings.TrimRight(r.URL.Path, "/"), "/models") {
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"stub-model","object":"model","owned_by":"stub","context_length":65536}]}`)
			return
		}
		_, _ = io.WriteString(w, `{}`)
		return
	}
	if !strings.HasSuffix(strings.TrimRight(r.URL.Path, "/"), "/chat/completions") {
		http.NotFound(w, r)
		return
	}
	m.calls.Add(1)
	var body struct {
		Model    string           `json:"model"`
		Stream   bool             `json:"stream"`
		Tools    []any            `json:"tools"`
		Messages []map[string]any `json:"messages"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	var (
		text string
		tool string
		args map[string]any
	)
	last := map[string]any{}
	if len(body.Messages) > 0 {
		last = body.Messages[len(body.Messages)-1]
	}
	user := ""
	for i := len(body.Messages) - 1; i >= 0; i-- {
		if body.Messages[i]["role"] == "user" {
			user = textOf(body.Messages[i]["content"])
			break
		}
	}
	switch {
	case len(body.Tools) == 0:
		text = "Stub summary: scripted stub turns."
	case last["role"] == "tool":
		text = "tool step finished"
	case strings.Contains(user, "APPROVE"):
		tool, args = "terminal", map[string]any{"command": "rm -r ./livehermes-victim-dir"}
	case strings.Contains(user, "SUDO"):
		tool, args = "terminal", map[string]any{"command": "sudo true"}
	case strings.Contains(user, "CLARIFYFREE"):
		tool, args = "clarify", map[string]any{"questions": []any{map[string]any{"question": "What name should the file get?"}}}
	case strings.Contains(user, "CLARIFY"):
		tool, args = "clarify", map[string]any{"questions": []any{map[string]any{"question": "Which colour should it use?", "choices": []string{"red", "blue"}}}}
	default:
		if mm := slowRE.FindStringSubmatch(user); mm != nil {
			n, _ := strconv.Atoi(mm[1])
			select {
			case <-time.After(time.Duration(n) * time.Second):
			case <-r.Context().Done():
				return
			}
			text = "slow reply after " + mm[1] + "s"
		} else {
			text = fmt.Sprintf("stub reply #%d", m.n.Add(1))
		}
	}
	id := fmt.Sprintf("chatcmpl-stub%d", time.Now().UnixNano())
	msg := map[string]any{"role": "assistant", "content": text}
	finish := "stop"
	var tc map[string]any
	if tool != "" {
		a, _ := json.Marshal(args)
		tc = map[string]any{"id": fmt.Sprintf("call_%d", time.Now().UnixNano()), "type": "function", "function": map[string]any{"name": tool, "arguments": string(a)}}
		msg = map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{tc}}
		finish = "tool_calls"
	}
	usage := map[string]any{"prompt_tokens": 100 * len(body.Messages), "completion_tokens": 10, "total_tokens": 100*len(body.Messages) + 10}
	model := body.Model
	if model == "" {
		model = "stub-model"
	}
	if !body.Stream {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "object": "chat.completion", "created": time.Now().Unix(), "model": model,
			"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}}, "usage": usage})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fl, _ := w.(http.Flusher)
	chunk := func(delta map[string]any, fr any) {
		c := map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": fr}}}
		b, _ := json.Marshal(c)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		if fl != nil {
			fl.Flush()
		}
	}
	chunk(map[string]any{"role": "assistant", "content": ""}, nil)
	if tc != nil {
		fn := tc["function"].(map[string]any)
		chunk(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": tc["id"], "type": "function", "function": map[string]any{"name": fn["name"], "arguments": ""}}}}, nil)
		chunk(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": fn["arguments"]}}}}, nil)
	} else {
		chunk(map[string]any{"content": text}, nil)
	}
	chunk(map[string]any{}, finish)
	b, _ := json.Marshal(map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []any{}, "usage": usage})
	_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
	if fl != nil {
		fl.Flush()
	}
}

// ---------------------------------------------------------------- the run

// liveRun is one harness run: a scratch directory, the stub model, a private
// tmux server, the hook binary, and the Hermes homes made under it.
type liveRun struct {
	t       *testing.T
	ins     hermesInstall
	dir     string // scratch root (short: the gateway's socket lives under it)
	tmpdir  string // the CLIs' TMPDIR
	work    string // the CLIs' working directory
	fake    string // the CLIs' HOME
	hookBin string // a muster-mirror built from this tree, for hermes-hook
	inject  string // sitecustomize.py's directory (stateInjection)
	model   *stubModel
	tmux    Tmux
	mu      sync.Mutex
	homes   []string
	windows int
}

// newLiveRun starts a run; everything it made is torn down at the test's end.
func newLiveRun(t *testing.T) *liveRun {
	t.Helper()
	ins := pinnedHermes(t)
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Fatal("livehermes: no tmux on PATH")
	}
	// 🔴 SHORT, AND NOT t.TempDir(): the gateway puts its control socket under
	// HERMES_HOME (or, when that path is over 100 bytes, under TMPDIR), and
	// tmux's socket path is limited to ~107 bytes.
	dir, err := os.MkdirTemp("", "mlh")
	if err != nil {
		t.Fatal(err)
	}
	r := &liveRun{t: t, ins: ins, dir: dir, tmpdir: filepath.Join(dir, "t"), work: filepath.Join(dir, "w"), fake: filepath.Join(dir, "u"),
		inject: filepath.Join(dir, "inj")}
	for _, d := range []string{r.tmpdir, r.work, r.fake, r.inject} {
		must(t, os.MkdirAll(d, 0o700))
	}
	r.tmux = Tmux{SocketPath: filepath.Join(dir, "tmux.sock")}
	r.model = newStubModel(t)
	must(t, os.WriteFile(filepath.Join(r.inject, "sitecustomize.py"), []byte(stateInjection), 0o600))
	r.hookBin = filepath.Join(dir, "muster-mirror")
	build := exec.Command("go", "build", "-o", r.hookBin, "./cmd/muster-mirror")
	build.Dir = moduleRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("livehermes: build muster-mirror for the hook: %v\n%s", err, out)
	}
	t.Cleanup(r.close)
	return r
}

// moduleRoot is this module's directory.
func moduleRoot(t *testing.T) string {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// homeOpts is how a Hermes home is configured.
type homeOpts struct {
	name      string
	display   map[string]any // config.yaml's display: section
	hook      bool           // register muster's hermes-hook (and allowlist it)
	strayHook bool           // also declare a hook that is NOT allowlisted (the consent prompt)
	busyMode  string         // display.busy_input_mode
	profile   string         // a named profile: the home is <HOME>/.hermes/profiles/<profile>
}

// makeHome makes a Hermes home: config.yaml pointing at the stub model, the
// telemetry question answered (no), manual approvals, and o's display.
func (r *liveRun) makeHome(o homeOpts) string {
	t := r.t
	h := filepath.Join(r.dir, "h-"+o.name)
	if o.profile != "" {
		h = filepath.Join(r.fake, ".hermes", "profiles", o.profile)
	}
	must(t, os.MkdirAll(h, 0o700))
	var b strings.Builder
	fmt.Fprintf(&b, "model:\n  provider: custom\n  base_url: %s\n  default: stub-model\n  api_key: stub-not-a-key\n", r.model.baseURL())
	fmt.Fprintf(&b, "models_dev:\n  url: %s/models-dev.json\n", r.model.srv.URL)
	b.WriteString("updates:\n  check: false\napprovals:\n  mode: manual\n  timeout: 300\ntoolsets:\n  - hermes-cli\n")
	b.WriteString("telemetry:\n  shared_metrics:\n    enabled: false\n    send: false\n    offer_version: 2\n")
	disp := map[string]any{}
	for k, v := range o.display {
		disp[k] = v
	}
	if o.busyMode != "" {
		disp["busy_input_mode"] = o.busyMode
	}
	if len(disp) > 0 {
		js, _ := json.Marshal(disp)
		fmt.Fprintf(&b, "display: %s\n", js)
	}
	if o.hook || o.strayHook {
		b.WriteString("hooks:\n")
		cmd := hermeshost.HookCommand(r.hookBin)
		for _, ev := range hermeshost.HookEvents {
			if o.hook {
				fmt.Fprintf(&b, "  %s:\n    - command: %q\n      timeout: 5\n", ev, cmd)
			}
		}
		if o.strayHook {
			fmt.Fprintf(&b, "  on_session_start:\n    - command: %q\n", "/bin/true livehermes-not-allowlisted")
		}
	}
	must(t, os.WriteFile(filepath.Join(h, "config.yaml"), []byte(b.String()), 0o600))
	if o.hook {
		if _, err := hermeshost.NewHome(h).WriteAllowlist(hermeshost.HookCommand(r.hookBin), time.Now()); err != nil {
			t.Fatalf("livehermes: allowlist the hook: %v", err)
		}
	}
	r.mu.Lock()
	r.homes = append(r.homes, h)
	r.mu.Unlock()
	return h
}

// statusDir is where the hook writes for a CLI whose HOME is r.fake.
func (r *liveRun) statusDir() string { return filepath.Join(r.fake, ".muster", "hermes-status") }

// env is a CLI's whole environment for home: nothing inherited but PATH.
func (r *liveRun) env(home string, extra ...string) []string {
	return append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + r.fake,
		"HERMES_HOME=" + home,
		"TMPDIR=" + r.tmpdir,
		"TERM=xterm-256color",
		"LANG=C.UTF-8",
		"PYTHONPATH=" + r.inject,
		"MUSTER_LIVEHERMES_INJECT=" + filepath.Join(home, ".inject"),
	}, extra...)
}

// tmuxRun runs a tmux command on the private server.
func (r *liveRun) tmuxRun(args ...string) (string, error) {
	cmd := exec.Command(r.tmux.bin(), append([]string{"-S", r.tmux.SocketPath, "-f", "/dev/null"}, args...)...)
	cmd.Env = tmuxEnv()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// cli starts `hermes chat` (args after `hermes`) for home in a new tmux
// window of w×h and returns its pane id. It does not wait for anything.
func (r *liveRun) cli(home string, w, h int, args []string, extraEnv ...string) string {
	t := r.t
	r.mu.Lock()
	r.windows++
	name := fmt.Sprintf("w%d", r.windows)
	r.mu.Unlock()
	argv := append([]string{"env", "-i"}, r.env(home, extraEnv...)...)
	argv = append(append(argv, r.ins.launcher), args...)
	var out string
	var err error
	if _, e := r.tmuxRun("has-session", "-t", "=live"); e != nil {
		out, err = r.tmuxRun(append([]string{"new-session", "-d", "-s", "live", "-n", name, "-x", strconv.Itoa(w), "-y", strconv.Itoa(h), "-c", r.work, "-P", "-F", "#{pane_id}", "--"}, argv...)...)
	} else {
		out, err = r.tmuxRun(append([]string{"new-window", "-d", "-t", "=live", "-n", name, "-c", r.work, "-P", "-F", "#{pane_id}", "--"}, argv...)...)
		if err == nil {
			_, err = r.tmuxRun("resize-window", "-t", "=live:"+name, "-x", strconv.Itoa(w), "-y", strconv.Itoa(h))
		}
	}
	if err != nil {
		t.Fatalf("livehermes: start hermes: %v: %s", err, out)
	}
	return strings.TrimSpace(strings.Split(out, "\n")[0])
}

// capture is pane's screen with its styles (capture-pane -p -e).
func (r *liveRun) capture(pane string) string {
	out, err := r.tmuxRun("capture-pane", "-p", "-e", "-t", pane)
	if err != nil {
		r.t.Fatalf("livehermes: capture %s: %v: %s", pane, err, out)
	}
	return out
}

// keys types raw keys into pane — the host's own raw paste (tmuxPane.Send:
// paste-buffer -S, unbracketed), so they arrive as keys, as a person's do.
func (r *liveRun) keys(pane, keys string) {
	if err := (tmuxPane{t: r.tmux, id: pane}).Send(context.Background(), keys); err != nil {
		r.t.Fatalf("livehermes: keys into %s: %v", pane, err)
	}
}

// waitScreen polls pane until ok holds, for up to d; the last capture and
// whether it held.
func (r *liveRun) waitScreen(pane string, d time.Duration, ok func(raw string, s hermeshost.Screen) bool) (string, bool) {
	deadline := time.Now().Add(d)
	var raw string
	for {
		raw = r.capture(pane)
		if ok(raw, hermeshost.ParseScreen(raw)) {
			return raw, true
		}
		if time.Now().After(deadline) {
			return raw, false
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// waitIdle waits for pane to pass the idle check, settled: two captures a
// second apart both pass.
func (r *liveRun) waitIdle(pane string, d time.Duration) bool {
	_, ok := r.waitScreen(pane, d, func(_ string, s hermeshost.Screen) bool { return hermeshost.CheckIdle(s) == nil })
	if !ok {
		return false
	}
	time.Sleep(time.Second)
	return hermeshost.CheckIdle(hermeshost.ParseScreen(r.capture(pane))) == nil
}

// killWindow ends pane's window (its CLI gets SIGHUP).
func (r *liveRun) killWindow(pane string) { _, _ = r.tmuxRun("kill-pane", "-t", pane) }

// injectState asks the CLI of home (through sitecustomize.py) to put itself
// in an overlay state no stub-model turn can reach: secret, connection,
// voice, voice-recording, voice-processing, and model-picker (the /model
// handler, called with a draft in the box).
func (r *liveRun) injectState(home, state string) {
	must(r.t, os.WriteFile(filepath.Join(home, ".inject"), []byte(state), 0o600))
}

// close tears the run down: the tmux server, then every process whose OWN
// environment names one of this run's homes, then the directory.
func (r *liveRun) close() {
	_, _ = r.tmuxRun("kill-server")
	for i := 0; i < 20; i++ {
		left := r.ours()
		if len(left) == 0 {
			break
		}
		sig := syscall.SIGTERM
		if i >= 10 {
			sig = syscall.SIGKILL
		}
		for _, pid := range left {
			// Re-read the environment AT the kill, not from the survey above.
			if r.isOurs(pid) {
				_ = syscall.Kill(pid, sig)
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	if left := r.ours(); len(left) > 0 {
		r.t.Errorf("livehermes: processes of this run survived teardown: %v", left)
	}
	if os.Getenv("MUSTER_LIVEHERMES_KEEP") == "" {
		_ = os.RemoveAll(r.dir)
	} else {
		r.t.Logf("livehermes: kept %s", r.dir)
	}
}

// ours is every process whose environment names one of this run's homes.
func (r *liveRun) ours() []int {
	ents, _ := os.ReadDir("/proc")
	var out []int
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		if r.isOurs(pid) {
			out = append(out, pid)
		}
	}
	return out
}

// isOurs reports whether pid's environment holds HERMES_HOME=<a home of
// this run>, exactly.
func (r *liveRun) isOurs(pid int) bool {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "environ"))
	if err != nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, kv := range bytes.Split(b, []byte{0}) {
		v, ok := bytes.CutPrefix(kv, []byte("HERMES_HOME="))
		if !ok {
			continue
		}
		for _, h := range r.homes {
			if string(v) == h {
				return true
			}
		}
	}
	return false
}

// stateInjection is the sitecustomize.py the CLIs load (PYTHONPATH): in a
// `hermes chat` process only, a daemon thread waits for the harness to write
// a state name into $MUSTER_LIVEHERMES_INJECT, then sets that overlay state
// on the running CLI exactly as Hermes's own callbacks do, and redraws. The
// REAL renderer draws it. Used only for states a stub-model turn cannot
// reach at the pin: secret capture, the connection form and voice.
const stateInjection = `
import os, sys, threading, time

def _livehermes_inject():
    path = os.environ.get("MUSTER_LIVEHERMES_INJECT")
    if not path or "chat" not in sys.argv:
        return
    def find():
        import gc
        mod = sys.modules.get("cli")
        cls = getattr(mod, "HermesCLI", None) if mod else None
        if cls is None:
            return None
        for o in gc.get_objects():
            try:
                if isinstance(o, cls) and getattr(o, "_app", None) is not None:
                    return o
            except Exception:
                pass
        return None
    def loop():
        while True:
            time.sleep(0.2)
            if not os.path.exists(path):
                continue
            try:
                what = open(path).read().strip()
                os.remove(path)
                cli = find()
                if cli is None:
                    open(path + ".err", "w").write("no running HermesCLI")
                    continue
                if what == "secret":
                    import queue
                    cli._secret_state = {"var_name": "LIVEHERMES_TOKEN", "prompt": "Enter the stub token",
                                         "metadata": {}, "response_queue": queue.Queue()}
                    cli._secret_deadline = time.monotonic() + 600
                elif what == "connection":
                    cli._connection_callback({"targets": [{"name": "stub", "label": "Stub connector",
                        "kind": "connector", "state": "pending", "action": "install",
                        "required_env": [{"name": "STUB_URL", "prompt": "Stub URL", "type": "plain"},
                                         {"name": "STUB_KEY", "prompt": "Stub key", "secret": True}]}]})
                elif what == "model-picker":
                    cli._handle_model_switch("/model")
                elif what == "image":
                    from pathlib import Path
                    img = Path(path).parent / "livehermes.png"
                    img.write_bytes(bytes.fromhex("89504e470d0a1a0a0000000d4948445200000001000000010806000000"
                                                  "1f15c4890000000d49444154789c6360000002000154a24f5d0000000049454e44ae426082"))
                    cli._attached_images.append(img)
                    cli._image_counter = getattr(cli, "_image_counter", 0) + 1
                elif what == "voice":
                    cli._voice_mode = True
                elif what == "voice-recording":
                    cli._voice_mode = True
                    cli._voice_recording = True
                elif what == "voice-processing":
                    cli._voice_mode = True
                    cli._voice_processing = True
                cli._app.invalidate()
                open(path + ".ok", "w").write(what)
            except Exception as e:
                open(path + ".err", "w").write(repr(e))
    threading.Thread(target=loop, daemon=True).start()

_livehermes_inject()
`
