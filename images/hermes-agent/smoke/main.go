// Command smoke is the hermes agent image's smoke test, the counterpart of the
// claude-code image's cc-smoke: the REAL image (pinned Hermes behind hermesd),
// a stub OpenAI-compatible model and a fake muster relay, driven through
// muster's OWN client (agentgateway.Gateway.Chat). No real model and no
// Kubernetes: the container runs under the local Docker daemon with pod-like
// restrictions (uid 1000, read-only root, no capabilities, no new privileges)
// and the host's network, so it reaches the stub and the fake on loopback.
//
//	nix build ./images/hermes-agent#image && ./result | docker load
//	go run ./images/hermes-agent/smoke -image hermes-agent:hermes-5fc490e2
//
// It checks, in order: a full turn (with muster's instructions at the end of
// the system prompt); an approval approved through muster's relay, and one
// denied; a gateway SIGKILLed mid-turn (the run left `unknown` is resolved by
// recovery and the session is not parked); an Idempotency-Key replay across a
// gateway restart (the design's UNVERIFIED claim, measured); a SIGTERM stop
// (the gateway's exit 1 not counted as a crash); and a container restart on
// the same volume (the session and the API key survive).
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

const instructionsMarker = "MUSTER_SMOKE_INSTRUCTIONS_MARKER"

type smoke struct {
	image, name, volume string
	listen, apiPort     int
	hooksToken          string
	agentToken          string
	stubURL, musterURL  string
	instructions        string
	stub                *stubModel
	muster              *fakeMuster
	gw                  *agentgateway.Gateway
	failed              int
}

func main() {
	image := flag.String("image", "hermes-agent:hermes-5fc490e2", "the image to test")
	keep := flag.Bool("keep", false, "leave the container and volume for inspection")
	flag.Parse()
	s := &smoke{image: *image, name: "hermes-smoke-" + randHex(4), stub: newStubModel()}
	s.volume = s.name + "-data"
	s.hooksToken, s.agentToken = "smoke-hooks-token-"+randHex(8), "smoke-agent-token-"+randHex(8)
	if !*keep {
		defer s.cleanup()
	}
	code := s.run()
	fmt.Printf("\nsmoke: %d check(s) failed\n", s.failed)
	if code == 0 && s.failed > 0 {
		code = 1
	}
	if !*keep {
		s.cleanup()
	}
	os.Exit(code)
}

func (s *smoke) check(name string, ok bool, detail string, args ...any) {
	mark := "PASS"
	if !ok {
		mark = "FAIL"
		s.failed++
	}
	fmt.Printf("  %s  %s", mark, name)
	if detail != "" {
		fmt.Printf(" — "+detail, args...)
	}
	fmt.Println()
}

func (s *smoke) run() int {
	stubLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fail(err)
	}
	go http.Serve(stubLn, s.stub)
	s.stubURL = "http://" + stubLn.Addr().String() + "/v1"
	s.muster = &fakeMuster{token: s.agentToken, decisions: []string{"approve", "reject"}}
	musterLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fail(err)
	}
	go http.Serve(musterLn, s.muster)
	s.musterURL = "http://" + musterLn.Addr().String()
	if s.listen, err = freePort(); err != nil {
		return fail(err)
	}
	if s.apiPort, err = freePort(); err != nil {
		return fail(err)
	}
	dir, err := os.MkdirTemp("", "hermes-smoke-")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(dir)
	s.instructions = filepath.Join(dir, "instructions.md")
	if err := os.WriteFile(s.instructions, []byte("You are a muster agent. "+instructionsMarker+"\n"), 0o644); err != nil {
		return fail(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		return fail(err)
	}
	s.gw, err = agentgateway.New(agentgateway.Config{
		Driver: staticEndpoint{Host: "127.0.0.1", Port: s.listen}, Runtime: agentgateway.HooksSHA256(), Model: "stub-model",
	})
	if err != nil {
		return fail(err)
	}

	fmt.Printf("smoke: image %s, container %s, muster wire :%d, Hermes API :%d\n", s.image, s.name, s.listen, s.apiPort)
	if out, err := docker("volume", "create", s.volume); err != nil {
		return fail(fmt.Errorf("volume create: %v %s", err, out))
	}
	if err := s.start(); err != nil {
		return fail(err)
	}
	st, err := s.waitReady(3*time.Minute, 1)
	s.check("the gateway starts, recovers and is ready", err == nil, "%+v %v", st, err)
	if err != nil {
		s.dumpLogs()
		return 1
	}

	s.turnChecks()
	s.killChecks()
	s.replayCheck()
	s.stopAndRestartChecks()
	if s.failed > 0 {
		s.dumpLogs()
	}
	return 0
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "smoke:", err)
	return 2
}

func (s *smoke) start() error {
	args := []string{"run", "-d", "--name", s.name, "--network", "host",
		"--read-only", "--tmpfs", "/tmp:rw,mode=1777", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"-v", s.volume + ":/data", "-v", s.instructions + ":/etc/hermesd/instructions.md:ro",
		"-e", "HOOKS_TOKEN=" + s.hooksToken,
		"-e", "MUSTER_HOOK_TOKEN=" + s.agentToken, "-e", "MUSTER_CONNECT_URL=" + s.musterURL,
		"-e", "OPENROUTER_API_KEY=stub-not-a-real-key",
		"-e", "HERMESD_MODEL=stub-model", "-e", "HERMESD_MODEL_BASE_URL=" + s.stubURL,
		"-e", "HERMESD_LISTEN=127.0.0.1:" + strconv.Itoa(s.listen), "-e", "HERMESD_API_PORT=" + strconv.Itoa(s.apiPort),
		"-e", "HERMESD_RELAY_TIMEOUT=20s",
		s.image}
	if out, err := docker(args...); err != nil {
		return fmt.Errorf("docker run: %v %s", err, out)
	}
	return nil
}

type healthz struct {
	OK      bool   `json:"ok"`
	State   string `json:"state"`
	Detail  string `json:"detail"`
	Gateway struct {
		Starts     int `json:"gateway_starts"`
		Recoveries int `json:"recoveries"`
	} `json:"gateway"`
}

func (s *smoke) health() (healthz, error) {
	var h healthz
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", s.listen))
	if err != nil {
		return h, err
	}
	defer resp.Body.Close()
	err = json.NewDecoder(resp.Body).Decode(&h)
	return h, err
}

// waitReady waits until /healthz is `ready` after at least minStarts starts.
func (s *smoke) waitReady(d time.Duration, minStarts int) (healthz, error) {
	deadline := time.Now().Add(d)
	var last healthz
	var lastErr error
	sawRecovering := false
	for time.Now().Before(deadline) {
		last, lastErr = s.health()
		if lastErr == nil && last.State == "recovering" {
			sawRecovering = true
		}
		if lastErr == nil && last.State == "ready" && last.Gateway.Starts >= minStarts && last.Gateway.Recoveries >= minStarts {
			_ = sawRecovering
			return last, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return last, fmt.Errorf("not ready within %s (last error %v)", d, lastErr)
}

var agent = agents.Agent{ID: 1, Name: "hermes-smoke", Kind: "hermes"}

func (s *smoke) chat(session, msg string, timeout time.Duration) (string, error) {
	a := agent
	a.HooksToken = s.hooksToken
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return s.gw.Chat(ctx, a, session, msg, nil)
}

func (s *smoke) turnChecks() {
	fmt.Println("\n== a full turn ==")
	reply, err := s.chat("smoke-main", "hello smoke", 3*time.Minute)
	s.check("muster's client gets the reply", err == nil && strings.Contains(reply, "STUB_REPLY to: hello smoke"), "%q %v", reply, err)
	reqs := s.stub.requests()
	if len(reqs) > 0 {
		sys := systemPrompt(reqs[len(reqs)-1])
		idx := strings.Index(sys, instructionsMarker)
		s.check("muster's instructions are at the END of the system prompt (the run's `instructions`)",
			idx >= 0 && len(sys)-idx < 200, "marker at %d of %d", idx, len(sys))
		s.check("Hermes's own identity is kept", strings.Contains(sys, "Hermes"), "")
	} else {
		s.check("the stub saw the turn", false, "no request offered tools")
	}

	fmt.Println("\n== the agent shell's environment ==")
	// Presence as digits, not NAME=value lines: Hermes redacts secret-shaped
	// `…_TOKEN=…` output before the model sees it.
	reply, err = s.chat("smoke-env", "RUN: for v in HOOKS_TOKEN MUSTER_GATEWAY_BEARER OPENROUTER_API_KEY API_SERVER_KEY MUSTER_HOOK_TOKEN; "+
		"do printenv $v >/dev/null && printf 1 || printf 0; done; echo; echo HOME=$HOME TMPDIR=$TMPDIR PWD=$(pwd); "+
		"git config --global --get-regexp 'url[.].*[.]insteadof'", 3*time.Minute)
	for _, want := range []struct{ what, text string }{
		{"of HOOKS_TOKEN, MUSTER_GATEWAY_BEARER, OPENROUTER_API_KEY, API_SERVER_KEY and MUSTER_HOOK_TOKEN only the last is set", "00001"},
		{"HOME is $HERMES_HOME/home", "HOME=/data/hermes/home"},
		{"TMPDIR is on the volume", "TMPDIR=/data/"},
		{"the working directory is the workspace (terminal.cwd)", "PWD=/data/workspace"},
		{"git reaches GitHub through muster's proxy", "url." + s.musterURL + "/connect/github/github.com/.insteadof https://github.com/"},
	} {
		s.check("the agent's shell: "+want.what, err == nil && strings.Contains(reply, want.text), "")
	}
	if s.failed > 0 {
		fmt.Printf("     (the tool result: %q)\n", reply)
	}

	fmt.Println("\n== an approval, approved through muster's relay ==")
	reply, err = s.chat("smoke-main", "RUN: mkdir -p /data/workspace/smoke-dir && rm -r /data/workspace/smoke-dir && echo removed-ok", 3*time.Minute)
	s.check("the approved command ran", err == nil && strings.Contains(reply, "removed-ok"), "%q %v", reply, err)
	s.muster.mu.Lock()
	filed := append([]map[string]any(nil), s.muster.filed...)
	s.muster.mu.Unlock()
	s.check("the approval was filed with muster, naming the command", len(filed) == 1 && strings.Contains(fmt.Sprint(filed[0]["command"]), "rm -r"),
		"%v", filed)
	out, _ := dockerExec(s.name, "sh", "-c", "test -e /data/workspace/smoke-dir && echo present || echo absent")
	s.check("the directory is gone", strings.TrimSpace(out) == "absent", "%s", out)

	fmt.Println("\n== an approval, denied through muster's relay ==")
	reply, err = s.chat("smoke-main", "RUN: mkdir -p /data/workspace/keep-dir && rm -r /data/workspace/keep-dir && echo should-not-run", 3*time.Minute)
	out, _ = dockerExec(s.name, "sh", "-c", "test -e /data/workspace/keep-dir && echo present || echo absent")
	s.check("the denied command did not run", err == nil && !strings.Contains(reply, `should-not-run\n`) && strings.TrimSpace(out) == "absent",
		"reply %q, err %v, dir %s", reply, err, strings.TrimSpace(out))
	fmt.Printf("     (the tool result the model saw: %q)\n", reply)
}

func systemPrompt(req map[string]any) string {
	msgs, _ := req["messages"].([]any)
	for _, m := range msgs {
		if mm, ok := m.(map[string]any); ok && mm["role"] == "system" {
			return textOf(mm["content"])
		}
	}
	return ""
}

// gatewayPID is the pid of the Hermes gateway in the container (comm=hermes;
// hermesd is comm=hermesd, tini comm=tini).
func (s *smoke) gatewayPID() (string, error) {
	out, err := dockerExec(s.name, "pgrep", "-x", "hermes")
	pids := strings.Fields(out)
	if err != nil || len(pids) != 1 {
		return "", fmt.Errorf("pgrep -x hermes: %q %v", out, err)
	}
	return pids[0], nil
}

func (s *smoke) killChecks() {
	fmt.Println("\n== the gateway SIGKILLed mid-turn ==")
	before := len(s.stub.requests())
	type res struct {
		reply string
		err   error
	}
	done := make(chan res, 1)
	go func() {
		r, err := s.chat("smoke-kill", "SLEEP:20\nslow turn", 4*time.Minute)
		done <- res{r, err}
	}()
	if !s.stub.waitFor(func(r []map[string]any) bool { return len(r) > before }, time.Minute) {
		s.check("the slow turn reached the model", false, "")
		return
	}
	pid, err := s.gatewayPID()
	if err != nil {
		s.check("found the gateway", false, "%v", err)
		return
	}
	_, err = dockerExec(s.name, "kill", "-9", pid)
	s.check("the gateway was SIGKILLed", err == nil, "pid %s %v", pid, err)
	r := <-done
	var rt *agents.RuntimeError
	ok := r.err != nil && errors.As(r.err, &rt) && rt.Type == "interrupted"
	s.check("the in-flight turn ends as a typed interruption, not a success or a hang", ok, "%q %v", r.reply, r.err)
	h, err := s.waitReady(2*time.Minute, 2)
	s.check("the gateway was restarted and recovered (starts 2, recoveries 2)", err == nil, "%+v %v", h, err)
	logs, _ := docker("logs", s.name)
	s.check("recovery resolved the unknown run (resolve-unknown, exact body)", strings.Contains(logs, "resolved unknown run"), "")
	reply, err := s.chat("smoke-kill", "after the kill", 3*time.Minute)
	s.check("the next turn on that session runs (the session is not parked)", err == nil && strings.Contains(reply, "STUB_REPLY to: after the kill"),
		"%q %v", reply, err)
}

// replayCheck measures what the design leaves UNVERIFIED: an Idempotency-Key
// replayed with the same body across a gateway restart answers the ORIGINAL run.
func (s *smoke) replayCheck() {
	fmt.Println("\n== an Idempotency-Key replay across a gateway restart (measured) ==")
	key := "smoke-replay-" + randHex(6)
	body := `{"input":"replay me","session_id":"smoke-replay-session"}`
	post := func(b string) (string, error) {
		return dockerExec(s.name, "sh", "-c", fmt.Sprintf(
			`curl -sS -o - -w ' HTTP=%%{http_code}' -X POST -H "Authorization: Bearer $(cat /data/hermesd/api-server-key)" `+
				`-H 'Content-Type: application/json' -H 'Idempotency-Key: %s' --data '%s' http://127.0.0.1:%d/v1/runs`, key, b, s.apiPort))
	}
	first, err := post(body)
	runID := jsonField(first, "run_id")
	s.check("the first POST is admitted", err == nil && strings.Contains(first, "HTTP=202") && runID != "", "%s %v", first, err)
	time.Sleep(3 * time.Second) // let it settle before the restart
	if pid, err := s.gatewayPID(); err == nil {
		_, _ = dockerExec(s.name, "kill", "-9", pid)
	}
	h, err := s.waitReady(2*time.Minute, 3)
	s.check("the gateway restarted", err == nil, "%+v %v", h, err)
	again, err := post(body)
	s.check("the SAME key and bytes after the restart replay the ORIGINAL run", err == nil &&
		strings.Contains(again, "HTTP=202") && jsonField(again, "run_id") == runID && strings.Contains(again, `"replayed": true`),
		"first %s, replay %s", strings.TrimSpace(first), strings.TrimSpace(again))
	// Control: the check above can fail — a DIFFERENT key with the same body is
	// a new run, not a replay.
	key0 := key
	key = "smoke-replay-" + randHex(6)
	other, err := post(body)
	s.check("control: another key with the same body admits a NEW run", err == nil && strings.Contains(other, "HTTP=202") &&
		jsonField(other, "run_id") != "" && jsonField(other, "run_id") != runID, "%s", strings.TrimSpace(other))
	key = key0
	conflict, err := post(`{"input":"a different body","session_id":"smoke-replay-session"}`)
	s.check("the same key with another body is 409 idempotency_key_conflict", err == nil &&
		strings.Contains(conflict, "HTTP=409") && strings.Contains(conflict, "idempotency_key_conflict"), "%s", strings.TrimSpace(conflict))
}

func jsonField(out, field string) string {
	i := strings.Index(out, "{")
	j := strings.LastIndex(out, "}")
	if i < 0 || j < i {
		return ""
	}
	var m map[string]any
	if json.Unmarshal([]byte(out[i:j+1]), &m) != nil {
		return ""
	}
	v, _ := m[field].(string)
	return v
}

func (s *smoke) stopAndRestartChecks() {
	fmt.Println("\n== SIGTERM, then a restart on the same volume ==")
	keyBefore, _ := dockerExec(s.name, "sha256sum", "/data/hermesd/api-server-key")
	before := len(s.stub.requests())
	type res struct {
		reply string
		err   error
	}
	done := make(chan res, 1)
	go func() {
		r, err := s.chat("smoke-term", "SLEEP:45\na turn in flight at SIGTERM", 4*time.Minute)
		done <- res{r, err}
	}()
	s.stub.waitFor(func(r []map[string]any) bool { return len(r) > before }, time.Minute)
	start := time.Now()
	_, err := docker("stop", "-t", "60", s.name)
	took := time.Since(start)
	r := <-done
	var rt *agents.RuntimeError
	s.check("the turn in flight at SIGTERM ends as a typed interruption", r.err != nil && errors.As(r.err, &rt) && rt.Type == "interrupted",
		"%q %v", r.reply, r.err)
	s.check("the gateway drained (about 30 s) inside the stop's grace", took > 25*time.Second && took < 60*time.Second, "took %s", took.Round(time.Millisecond))
	exit, _ := docker("inspect", "-f", "{{.State.ExitCode}}", s.name)
	logs, _ := docker("logs", s.name)
	s.check("SIGTERM stops hermesd cleanly (exit 0) after the gateway drains", err == nil && strings.TrimSpace(exit) == "0",
		"exit %s in %s", strings.TrimSpace(exit), took.Round(time.Millisecond))
	s.check("the gateway's exit 1 after SIGTERM is recorded as a stop, not a crash", strings.Contains(logs, "stopped (status 1) after SIGTERM"), "")
	if out, err := docker("start", s.name); err != nil {
		s.check("the container restarts", false, "%v %s", err, out)
		return
	}
	h, err := s.waitReady(3*time.Minute, 1)
	s.check("after the restart the gateway recovers and is ready", err == nil, "%+v %v", h, err)
	keyAfter, _ := dockerExec(s.name, "sha256sum", "/data/hermesd/api-server-key")
	s.check("the API key on the volume is the same key", keyBefore != "" && keyBefore == keyAfter, "")
	before = len(s.stub.requests())
	reply, err := s.chat("smoke-main", "remember me", 3*time.Minute)
	reqs := s.stub.requests()
	history := 0
	if len(reqs) > before {
		msgs, _ := reqs[before]["messages"].([]any)
		history = len(msgs)
	}
	s.check("the session survived: the model saw its history", err == nil && strings.Contains(reply, "STUB_REPLY to: remember me") && history > 6,
		"%d messages, %q %v", history, reply, err)
}

func (s *smoke) dumpLogs() {
	logs, _ := docker("logs", "--tail", "120", s.name)
	fmt.Println("\n---- container log (tail) ----\n" + logs)
}

func (s *smoke) cleanup() {
	_, _ = docker("rm", "-f", s.name)
	_, _ = docker("volume", "rm", "-f", s.volume)
}

func docker(args ...string) (string, error) {
	var out bytes.Buffer
	cmd := exec.Command("docker", args...)
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

func dockerExec(name string, args ...string) (string, error) {
	return docker(append([]string{"exec", name}, args...)...)
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

type staticEndpoint provision.Endpoint

func (e staticEndpoint) Endpoint(context.Context, provision.Ref) (provision.Endpoint, error) {
	return provision.Endpoint(e), nil
}
