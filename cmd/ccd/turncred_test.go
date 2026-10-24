package main

import (
	"bytes"
	"net"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/provision"
)

// chiefSpec is the claude-code chief's spec as muster builds it, with the REAL
// credential scheme ccd uses. Synthetic fixture values only.
func chiefSpec(t *testing.T) provision.Spec {
	t.Helper()
	cfg := agentspec.Config{
		ImageRepo:  "ghcr.io/example-org/agent-runtime",
		APIBaseURL: "http://muster.example.test:8105",
		ClaudeCode: &agentspec.ClaudeCodeConfig{
			Image:    "ghcr.io/example-org/claude-code-agent:0a1b2c3",
			Accounts: map[string]string{"acct": "sk-ant-oat01-FAKE-fixture"},
		},
	}
	a := agents.Agent{ID: 7, Name: "chief", Kind: agents.KindClaudeCode, CCAccount: "acct", HooksToken: knownHooksToken,
		GatewaySecret: "fixture-turn-credential-chief-4be1"}
	spec, err := agentspec.Build(a, cfg, agentspec.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

// 🔴 TestNothingTheCLIKeepsDrivesTheTurnAPI is the seam between agentspec (what
// ccd's container is handed), ccd's own scrub (what it passes on) and ccd's
// turn API (what it accepts). ccd's environment is set from the claude-code
// chief's spec exactly as muster builds it — plus the retired
// CCD_GATEWAY_UPSTREAM_BEARER an older spec carried — ccd takes its turn
// credentials as serve does; then EVERY value the CLI is started with — ccd's
// environment after the take, plus the hook channel ccd adds — and the
// hooks-sha256 derivation of each, is presented on /v1/responses and is
// refused 401.
//
// The controls: the instrument check that what the CLI keeps includes the agent's
// muster token, the model token and the hook credential (the scan has the values
// worth worrying about), and muster's own bearer, which IS accepted.
func TestNothingTheCLIKeepsDrivesTheTurnAPI(t *testing.T) {
	spec := chiefSpec(t)
	given := append(append([]provision.EnvVar{}, spec.Env...), spec.Secrets...)
	given = append(given, provision.EnvVar{Name: envGatewayUpstreamBearer, Value: "fixture-retired-upstream-credential-5a1"})
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

	srv := newServer(serverConfig{Bearer: bearer, HookSecret: hooks.secret, ConfigDir: t.TempDir()}, nil, newAuthTracker())
	gw := gatewayServer(t, srv)
	post := func(cred string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, gw.URL+"/v1/responses", strings.NewReader(`{"input":"x"}`))
		req.Header.Set("Authorization", "Bearer "+cred)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for name, v := range kept {
		for what, cred := range map[string]string{name: v, "hooks-sha256(" + name + ")": agentgateway.HooksSHA256().Bearer(v)} {
			if code := post(cred); code != http.StatusUnauthorized {
				t.Errorf("%s on /v1/responses: %d, want 401", what, code)
			}
		}
	}
	// Positive control: muster's bearer is accepted.
	if code := post(bearer); code == http.StatusUnauthorized {
		t.Fatalf("control: muster's bearer on a turn: 401")
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
