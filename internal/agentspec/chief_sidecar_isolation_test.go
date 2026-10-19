package agentspec

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/provision"
)

// ccdAcceptsFrom is every value that authenticates to ccd, or derives one that
// does, given the environment the INSTANCE's container (ccd's) is built with.
//
// It mirrors cmd/ccd's resolveBearer, which reads HOOKS_TOKEN (and derives the
// bearer with agentgateway.HooksSHA256) and MUSTER_GATEWAY_BEARER: so the bearer
// itself, and the raw token it is derived from under EITHER name the instance
// carries it. A holder of the raw token holds the bearer, because the derivation
// is public.
func ccdAcceptsFrom(spec provision.Spec) map[string]string {
	out := map[string]string{}
	all := append(append([]provision.EnvVar{}, spec.Env...), spec.Secrets...)
	for _, e := range all {
		switch e.Name {
		case EnvGatewayToken, EnvToken:
			if e.Value != "" {
				out[e.Name+" (raw; ccd derives its bearer from it)"] = e.Value
				out["ccd's bearer, derived from "+e.Name] = agentgateway.HooksSHA256().Bearer(e.Value)
			}
		case EnvGatewayBearer:
			if e.Value != "" {
				out["ccd's bearer ("+e.Name+")"] = e.Value
			}
		}
	}
	return out
}

// sidecarValues is every place a credential could reach the sidecar's process:
// its environment, its confidential environment, its files and its command line.
func sidecarValues(sc *provision.Sidecar) map[string]string {
	out := map[string]string{}
	for _, e := range sc.Env {
		out["env "+e.Name] = e.Value
	}
	for _, e := range sc.Secrets {
		out["secret "+e.Name] = e.Value
	}
	for _, f := range sc.Files {
		out["file "+f.Path] = string(f.Content)
	}
	out["command"] = strings.Join(append(append([]string{}, sc.Runtime.Command...), sc.Runtime.Args...), " ")
	return out
}

// TestTheGatewaySidecarHoldsNoCredentialCcdAccepts: the gateway runtime beside
// ccd shares ccd's loopback, so whatever it holds that ccd accepts lets a
// prompt-injected gateway session drive the Claude Code chief. Built with the
// REAL derivation ccd uses (agentgateway.HooksSHA256), the sidecar's env,
// secrets, files and command contain none of: ccd's bearer, or the raw hooks
// token it is derived from. And ccd is handed the sidecar's own credential to
// forward with, which is not its own.
func TestTheGatewaySidecarHoldsNoCredentialCcdAccepts(t *testing.T) {
	cfg := sidecarConfig()
	cfg.RuntimeConfig.DeriveBearer = agentgateway.HooksSHA256().Bearer
	a := ccAgent()
	spec := mustBuild(t, a, cfg, Options{GatewaySidecar: &GatewaySidecar{}})
	if spec.Sidecar == nil {
		t.Fatal("no sidecar")
	}
	accepts := ccdAcceptsFrom(spec)
	// Positive control: the instance does hold the token ccd derives from, so
	// the scan below has something to find.
	if len(accepts) == 0 || accepts["ccd's bearer, derived from "+EnvGatewayToken] != agentgateway.HooksSHA256().Bearer(a.HooksToken) {
		t.Fatalf("the instance carries no credential ccd accepts (%d found); the scan would prove nothing", len(accepts))
	}
	for where, v := range sidecarValues(spec.Sidecar) {
		for what, cred := range accepts {
			if strings.Contains(v, cred) {
				t.Errorf("the sidecar's %s holds %s", where, what)
			}
		}
	}

	upstream := secretMap(spec.Secrets)[EnvCCDGatewayUpstreamBearer]
	own := secretMap(spec.Sidecar.Secrets)[EnvGatewayBearer]
	if upstream == "" || upstream != own {
		t.Errorf("ccd forwards with %q; the sidecar serves on %q: they must be the same, non-empty", upstream, own)
	}
	for what, cred := range accepts {
		if upstream == cred {
			t.Errorf("ccd's forwarding credential is %s", what)
		}
	}
	// The sidecar's credential is its own, but still per-agent: another agent's
	// chief gets another one.
	b := ccAgent()
	b.HooksToken = "fixture-other-agent-token-51d0"
	other := mustBuild(t, b, cfg, Options{GatewaySidecar: &GatewaySidecar{}})
	if secretMap(other.Sidecar.Secrets)[EnvGatewayBearer] == own {
		t.Error("two agents' sidecars share one credential")
	}
}

func secretMap(env []provision.EnvVar) map[string]string {
	m := map[string]string{}
	for _, e := range env {
		m[e.Name] = e.Value
	}
	return m
}

// TestCheckSidecarIsolationRefusesEveryPlaceACredentialCanReachTheSidecar: the
// build-time guard, reached directly with specs no earlier check rejects. A
// value of the instance's own Secrets, or ccd's bearer, in the sidecar's env,
// secrets, a file or its command is refused, naming the place and not the
// value; the sidecar's own forwarding credential is the one exception.
func TestCheckSidecarIsolationRefusesEveryPlaceACredentialCanReachTheSidecar(t *testing.T) {
	const (
		token   = "fixture-instance-token-a41e"
		ccdCred = "fixture-ccd-ccdCred-0b7d"
		own     = "fixture-sidecar-own-credential-93c5"
	)
	base := func() provision.Spec {
		return provision.Spec{
			Ref: provision.Ref{Name: "chief", ID: 1},
			Secrets: []provision.EnvVar{
				{Name: EnvGatewayToken, Value: token},
				{Name: EnvCCDGatewayUpstreamBearer, Value: own},
			},
			Sidecar: &provision.Sidecar{
				Name:    GatewaySidecarName,
				Runtime: provision.Runtime{Command: []string{"sh", "-c", "run"}},
				Env:     []provision.EnvVar{{Name: "HOME", Value: "/home/node"}},
				Secrets: []provision.EnvVar{{Name: EnvGatewayBearer, Value: own}},
				Files:   []provision.File{{Path: "/muster/runtime/config", Content: []byte("{}")}},
			},
		}
	}
	if err := checkSidecarIsolation(base(), ccdCred); err != nil {
		t.Fatalf("a clean sidecar holding only its own credential was refused: %v", err)
	}
	for _, c := range []struct {
		name, where string
		leak        func(*provision.Sidecar)
	}{
		{"instance token in env", "env LEAK", func(s *provision.Sidecar) { s.Env = append(s.Env, provision.EnvVar{Name: "LEAK", Value: token}) }},
		{"ccd ccdCred in a secret", "secret LEAK", func(s *provision.Sidecar) {
			s.Secrets = append(s.Secrets, provision.EnvVar{Name: "LEAK", Value: ccdCred})
		}},
		{"instance token inside a file", "file /x", func(s *provision.Sidecar) {
			s.Files = append(s.Files, provision.File{Path: "/x", Content: []byte(`{"t":"` + token + `"}`)})
		}},
		{"ccd ccdCred on the command line", "command", func(s *provision.Sidecar) { s.Runtime.Args = []string{"--token=" + ccdCred} }},
	} {
		spec := base()
		c.leak(spec.Sidecar)
		err := checkSidecarIsolation(spec, ccdCred)
		if err == nil || !strings.Contains(err.Error(), c.where) {
			t.Errorf("%s: %v; want a refusal naming %q", c.name, err, c.where)
			continue
		}
		if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), ccdCred) {
			t.Errorf("%s: the refusal prints the credential: %v", c.name, err)
		}
	}
}
