package main

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/provision"
)

// 🔴 TestNothingTheCLIKeepsDrivesTheTurnAPI is the seam between agentspec (what
// ccd's container is handed), ccd's own scrub (what it passes on) and ccd's
// turn API (what it accepts). ccd's environment is set from the claude-code
// chief's spec exactly as muster builds it; ccd takes its turn credentials as
// serve does; then EVERY value the CLI is started with — ccd's environment
// after the take, plus the hook channel ccd adds — and the hooks-sha256
// derivation of each, is presented on every authenticated ccd route and is
// refused 401, and none of them is the gateway runtime's own credential either.
//
// The controls: the instrument check that what the CLI keeps includes the agent's
// muster token, the model token and the hook credential (the scan has the values
// worth worrying about), and muster's own bearer, which IS accepted and reaches
// the runtime under the runtime's credential.
func TestNothingTheCLIKeepsDrivesTheTurnAPI(t *testing.T) {
	spec := chiefSpecWithSidecar(t)
	given := append(append([]provision.EnvVar{}, spec.Env...), spec.Secrets...)
	for _, e := range given {
		t.Setenv(e.Name, e.Value)
	}
	creds := takeTurnCredentials()
	bearer, err := resolveBearer(creds.hooksToken, creds.gatewayBearer)
	if err != nil {
		t.Fatal(err)
	}
	if name := exposedTurnCredential(bearer, os.Getenv); name != "" {
		t.Fatalf("ccd would refuse to start on the spec muster builds: its bearer is derivable from %s", name)
	}
	hooks, err := newHookChannel()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(hooks.dir) })

	// What the CLI is started with.
	kept := map[string]string{}
	for _, e := range given {
		if v := os.Getenv(e.Name); v != "" {
			kept[e.Name] = v
		}
	}
	for _, kv := range hooks.cliEnv() {
		name, v, _ := strings.Cut(kv, "=")
		kept[name] = v
	}
	for _, name := range []string{agentspec.EnvToken, agentspec.EnvClaudeOAuthToken, envHookSecret} {
		if kept[name] == "" {
			t.Fatalf("instrument check: the CLI does not keep %s; the scan would miss the values that matter (kept: %v)",
				name, keysOfMap(kept))
		}
	}
	for _, name := range turnCredentialEnv {
		if _, ok := kept[name]; ok {
			t.Errorf("the CLI keeps %s", name)
		}
	}

	rt := &fakeGatewayRuntime{}
	up := httptest.NewServer(rt)
	t.Cleanup(up.Close)
	srv := newServer(serverConfig{Bearer: bearer, HookSecret: hooks.secret, ConfigDir: t.TempDir()}, nil, newAuthTracker())
	if why := gatewayBearerProblem(creds.upstreamBearer, bearer); why != "" {
		t.Fatalf("ccd would refuse gateway turns: %s", why)
	}
	if srv.gateway, err = newGatewayProxy(up.URL, creds.upstreamBearer); err != nil {
		t.Fatal(err)
	}
	gw := gatewayServer(t, srv)
	post := func(path, cred string, gateway bool) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, gw.URL+path, strings.NewReader(`{"input":"x"}`))
		req.Header.Set("Authorization", "Bearer "+cred)
		if gateway {
			req.Header.Set(headerRuntime, runtimeGateway)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	routes := []struct {
		path    string
		gateway bool
	}{{"/v1/responses", false}, {"/v1/responses", true}, {"/v1/chat/completions", true}}
	sidecarOwn := envOf(spec.Sidecar.Secrets)[agentspec.EnvGatewayBearer]
	for name, v := range kept {
		for what, cred := range map[string]string{name: v, "hooks-sha256(" + name + ")": agentgateway.HooksSHA256().Bearer(v)} {
			if cred == sidecarOwn {
				t.Errorf("%s is the gateway runtime's own credential", what)
			}
			for _, r := range routes {
				if code := post(r.path, cred, r.gateway); code != http.StatusUnauthorized {
					t.Errorf("%s on %s (gateway=%v): %d, want 401", what, r.path, r.gateway, code)
				}
			}
		}
	}
	if rt.count() != 0 {
		t.Errorf("the runtime saw %d requests made with what the CLI keeps; want none", rt.count())
	}
	// Positive control: muster's bearer is accepted on both shapes.
	if code := post("/v1/responses", bearer, false); code == http.StatusUnauthorized {
		t.Fatalf("control: muster's bearer on a plain turn: 401")
	}
	if code := post("/v1/responses", bearer, true); code != http.StatusOK || rt.count() != 1 {
		t.Fatalf("control: muster's bearer on a gateway turn: %d (runtime saw %d); want 200 and one hand-off", code, rt.count())
	}
}

func keysOfMap(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestExposedTurnCredentialNamesWhatTheCLIKeeps(t *testing.T) {
	const tok, oauth, own = "fixture-agent-token-71c2", "sk-ant-oat01-FAKE-fixture", "fixture-own-turn-credential-0d5e"
	env := func(name string) string {
		return map[string]string{envAgentToken: tok, envClaudeOAuthToken: oauth}[name]
	}
	for _, c := range []struct{ name, bearer, want string }{
		{"a spec that predates the turn credential: hooks-sha256 of the agent's token", agentgateway.HooksSHA256().Bearer(tok), envAgentToken},
		{"the agent's token itself", tok, envAgentToken},
		{"the model token is not checked: muster derives no bearer from it", oauth, ""},
		{"a turn credential of its own", own, ""},
	} {
		if got := exposedTurnCredential(c.bearer, env); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	if got := exposedTurnCredential(own, func(string) string { return "" }); got != "" {
		t.Errorf("nothing kept: %q", got)
	}
}

// TestCcdScrubsExactlyWhatMusterStrips: ccd's list of turn credentials is
// agentspec's, the one muster's every exec into the container strips; two lists
// that drifted would leave a credential in one of the two routes.
func TestCcdScrubsExactlyWhatMusterStrips(t *testing.T) {
	if !reflect.DeepEqual(turnCredentialEnv, agentspec.TurnCredentialEnv) {
		t.Fatalf("ccd scrubs %v; muster's execs strip %v", turnCredentialEnv, agentspec.TurnCredentialEnv)
	}
	if got := exposedTurnCredential("", func(string) string { return "" }); got != "" {
		t.Errorf("nothing kept: %q", got)
	}
}

// 🔴 TestServeRefusesATurnCredentialTheCLICouldDerive: the binary, as a pod
// whose spec predates the turn credential runs it — HOOKS_TOKEN and
// MUSTER_HOOK_TOKEN with one value, so the bearer is derivable from what the CLI
// keeps — exits with THIS refusal, naming the variable and not its value. The
// control is the same start with a different MUSTER_HOOK_TOKEN: ccd gets past
// the check and listens.
func TestServeRefusesATurnCredentialTheCLICouldDerive(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// start runs ccd; done yields its exit. ONE Wait, in one goroutine: the
	// cleanup kills and then reads done, never Waits a second time.
	start := func(agentToken string) (<-chan error, *lockedBuf, string) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := l.Addr().String()
		l.Close()
		cmd := exec.Command(exe, "serve")
		cmd.Env = append(os.Environ(), "CCD_TEST_AS=ccd", "HOOKS_TOKEN="+knownHooksToken,
			envAgentToken+"="+agentToken, "CCD_LISTEN="+addr, "CLAUDE_CONFIG_DIR="+t.TempDir(),
			"CCD_WORKSPACE="+t.TempDir(), "TMPDIR="+t.TempDir(), "CCD_SUPERVISE=", "CCD_REPO=", "MUSTER_CONNECT_URL=")
		out := &lockedBuf{}
		cmd.Stdout, cmd.Stderr = out, out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done, exited := make(chan error, 1), make(chan struct{})
		go func() { done <- cmd.Wait(); close(exited) }()
		t.Cleanup(func() {
			_ = cmd.Process.Kill()
			select {
			case <-exited:
			case <-time.After(10 * time.Second):
				t.Errorf("ccd (pid %d) did not exit after SIGKILL", cmd.Process.Pid)
			}
		})
		return done, out, addr
	}

	done, out, _ := start(knownHooksToken)
	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("ccd exited 0; want the refusal:\n%s", out.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("ccd kept running on a bearer the CLI could derive:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "the turn credential is derivable from "+envAgentToken) {
		t.Fatalf("ccd exited, but not with the derivable-credential refusal:\n%s", out.String())
	}
	if strings.Contains(out.String(), knownHooksToken) || strings.Contains(out.String(), pinnedDerivation) {
		t.Errorf("the refusal prints a credential:\n%s", out.String())
	}

	// Control: a muster token the bearer is not derived from — ccd listens.
	_, out, addr := start("fixture-unrelated-agent-token-5a90")
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if resp, err := http.Get("http://" + addr + "/healthz"); err == nil {
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("control: ccd never listened:\n%s", out.String())
		}
	}
	if strings.Contains(out.String(), "derivable") {
		t.Fatalf("control: ccd refused:\n%s", out.String())
	}
}

// lockedBuf is a bytes.Buffer safe to write from a child's output copier while
// the test reads it.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
