package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/provision"
)

// chiefSpecWithSidecar is the claude-code chief's spec as muster builds it, with
// the gateway runtime beside ccd and the REAL credential scheme ccd uses.
// Synthetic fixture values only.
func chiefSpecWithSidecar(t *testing.T) provision.Spec {
	t.Helper()
	cfg := agentspec.Config{
		ImageRepo:  "ghcr.io/example-org/agent-runtime",
		APIBaseURL: "http://muster.example.test:8105",
		ClaudeCode: &agentspec.ClaudeCodeConfig{
			Image:    "ghcr.io/example-org/claude-code-agent:0a1b2c3",
			Accounts: map[string]string{"acct": "sk-ant-oat01-FAKE-fixture"},
		},
		RuntimeConfig: agentspec.RuntimeConfig{
			Template:     []byte(`{"example":true}`),
			Install:      []byte("set -eu\nexec /example/entrypoint --port \"$MUSTER_GATEWAY_PORT\"\n"),
			DeriveBearer: agentgateway.HooksSHA256().Bearer,
		},
		OpenRouterAPIKey: "sk-or-v1-FAKE-fixture",
	}
	a := agents.Agent{ID: 7, Name: "chief", Kind: agents.KindClaudeCode, CCAccount: "acct", HooksToken: knownHooksToken,
		GatewaySecret: "fixture-turn-credential-chief-4be1"}
	spec, err := agentspec.Build(a, cfg, agentspec.Options{GatewaySidecar: &agentspec.GatewaySidecar{}})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Sidecar == nil {
		t.Fatal("no sidecar")
	}
	return spec
}

func envOf(vars ...[]provision.EnvVar) map[string]string {
	m := map[string]string{}
	for _, l := range vars {
		for _, e := range l {
			m[e.Name] = e.Value
		}
	}
	return m
}

// 🔴 TestTheGatewayRuntimesCredentialsAreRefusedOnEveryCcdRoute is the seam
// between agentspec (what each container is handed) and ccd (what it accepts).
// ccd is configured from the INSTANCE container's environment exactly as
// serve() reads it; then every value the SIDECAR container holds — and the
// bearer a runtime would derive from each — is presented as a bearer on every
// authenticated ccd route, and every one is refused before anything runs. The
// positive control is muster's own credential: it is accepted, and the turn
// reaches the runtime under the runtime's own credential.
func TestTheGatewayRuntimesCredentialsAreRefusedOnEveryCcdRoute(t *testing.T) {
	spec := chiefSpecWithSidecar(t)
	ccdEnv := envOf(spec.Env, spec.Secrets)
	bearer, err := resolveBearer(ccdEnv["HOOKS_TOKEN"], ccdEnv["MUSTER_GATEWAY_BEARER"])
	if err != nil {
		t.Fatal(err)
	}
	rt := &fakeGatewayRuntime{}
	up := httptest.NewServer(rt)
	t.Cleanup(up.Close)
	srv := newServer(serverConfig{Bearer: bearer, ConfigDir: t.TempDir()}, nil, newAuthTracker())
	if why := gatewayBearerProblem(ccdEnv[envGatewayUpstreamBearer], bearer); why != "" {
		t.Errorf("ccd would refuse gateway turns: %s", why)
		srv.gatewayOff = why
	} else if srv.gateway, err = newGatewayProxy(up.URL, ccdEnv[envGatewayUpstreamBearer]); err != nil {
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
			return -1
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	routes := []struct {
		path    string
		gateway bool
	}{{"/v1/responses", false}, {"/v1/responses", true}, {"/v1/chat/completions", true}}

	held := envOf(spec.Sidecar.Env, spec.Sidecar.Secrets)
	creds := map[string]string{}
	for name, v := range held {
		if v == "" {
			continue
		}
		creds["the sidecar's "+name] = v
		creds["a bearer derived from the sidecar's "+name] = agentgateway.HooksSHA256().Bearer(v)
	}
	if len(creds) < 6 {
		t.Fatalf("the sidecar holds %d values; the scan would prove little", len(creds))
	}
	for what, cred := range creds {
		for _, r := range routes {
			if code := post(r.path, cred, r.gateway); code != http.StatusUnauthorized {
				t.Errorf("%s on %s (gateway=%v): %d, want 401", what, r.path, r.gateway, code)
			}
		}
	}
	if rt.count() != 0 {
		t.Errorf("the runtime saw %d requests made with its own credentials; want none", rt.count())
	}

	// Positive control: muster's credential is accepted, and the runtime is
	// reached with ITS credential — the one the sidecar serves on.
	if code := post("/v1/responses", bearer, true); code != http.StatusOK || rt.count() != 1 {
		t.Fatalf("muster's bearer on a gateway turn: %d (runtime saw %d); want 200 and one hand-off", code, rt.count())
	}
	if got, want := rt.seen[0].Header.Get("Authorization"), "Bearer "+held[agentspec.EnvGatewayBearer]; got != want || held[agentspec.EnvGatewayBearer] == "" {
		t.Errorf("the runtime got Authorization %q; want the sidecar's own credential %q", got, want)
	}
}

// TestCcdRefusesGatewayTurnsWithoutTheRuntimesOwnCredential: a pod whose spec
// predates the runtime's own credential (none set), or one that would hand the
// runtime ccd's own bearer, refuses gateway turns — naming the fix — rather than
// forwarding ccd's bearer to the runtime.
func TestCcdRefusesGatewayTurnsWithoutTheRuntimesOwnCredential(t *testing.T) {
	bearer, _ := resolveBearer(knownHooksToken, "")
	for _, c := range []struct{ upstream, want string }{
		{"", "Stop → Start"},
		{bearer, "equals ccd's own bearer"},
	} {
		why := gatewayBearerProblem(c.upstream, bearer)
		if !strings.Contains(why, c.want) {
			t.Errorf("upstream credential %q: %q, want it to say %q", c.upstream, why, c.want)
		}
		srv := newServer(serverConfig{Bearer: bearer, ConfigDir: t.TempDir()}, nil, newAuthTracker())
		srv.gatewayOff = why
		_, err := mustersGateway(t, gatewayServer(t, srv).URL).Chat(gatewayRoute("openrouter/vendor-x/model-y"), contractAgent, "k", "hi", nil)
		var re *agents.RuntimeError
		if !errors.As(err, &re) || re.Type != failNotReady || !strings.Contains(err.Error(), c.want) {
			t.Errorf("upstream credential %q: a gateway turn: %v; want %s naming %q", c.upstream, err, failNotReady, c.want)
		}
	}
	if why := gatewayBearerProblem(fixtureRuntimeCredential, bearer); why != "" {
		t.Errorf("a credential of the runtime's own was refused: %s", why)
	}
	if _, err := newGatewayProxy("http://127.0.0.1:18791", ""); err == nil {
		t.Error("a proxy with no runtime credential was built")
	}
}
