package agentspec

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

// THE GATEWAY RUNTIME BESIDE ccd IN A CLAUDE-CODE CHIEF'S POD.
//
// A claude-code chief hosts chat sessions on two runtimes (muster's
// chat_sessions.runtime, migration 0030): Claude Code (ccd, the pod's own
// process) and the model-gateway runtime, which runs here as a SIDECAR
// (provision.Sidecar) — a second container in the same pod, reached by ccd on
// loopback. muster sends a gateway session's turn to ccd's port as usual, with
// X-Muster-Runtime: gateway, and ccd hands it to the sidecar (cmd/ccd/gateway.go).
//
// 🔴 THE SIDECAR IS BUILT BY THE GATEWAY KIND'S OWN Build, NOT BY A SECOND COPY
// OF IT. Its image, its command (the operator's install script), its runtime
// configuration files, its derived gateway credential, its OpenRouter key and its
// MUSTER_CONFIG model are exactly what a gateway-kind agent with this row's
// identity would get; only what a SIDECAR cannot have is changed:
//
//   - no workspace, repository, seed files or Init (it has no volume and runs no
//     init container) — Build is asked for none, and a non-empty Init is refused;
//   - WorkingDir is cleared: Build's is the gateway kind's workspace, which a
//     sidecar does not mount, and a container runtime creates a missing working
//     directory as ROOT, which the uid-1000 process then cannot write under;
//   - HOME is set to the image's own user's home, where the runtime keeps its
//     state (and where the install script writes its configuration) in the
//     container's own writable layer — never on the chief's volume, which the
//     Claude Code session can read (provision.Sidecar);
//   - GatewaySidecarPortEnv tells the operator's install script which LOOPBACK
//     port to serve on: 18789 is ccd's.
//
// The chief's INSTRUCTIONS are not placed here: the runtime reads its standing
// files from its own home, and muster sends a gateway session's turn the chief's
// prose as the request's instructions (internal/api's chatTurn), which the
// runtime adds to its system prompt.
//
// MEASURED against the image a deployment runs (tag 2026.5.7), in a container as
// uid 1000, all capabilities dropped, no-new-privileges, HOME=/home/node, the
// operator's install script with `--bind loopback --port "$MUSTER_GATEWAY_PORT"`:
// ready on 127.0.0.1:18791 in 3s, about 380MiB resident idle.
const (
	// GatewaySidecarName is the sidecar container's name.
	GatewaySidecarName = "gateway"
	// GatewaySidecarPort is the loopback port it serves on.
	GatewaySidecarPort = 18791
	// GatewaySidecarPortEnv carries GatewaySidecarPort to the operator's install
	// script, which must bind the runtime to loopback on it.
	GatewaySidecarPortEnv = "MUSTER_GATEWAY_PORT"
	// GatewaySidecarHome is the runtime image's own user's home.
	GatewaySidecarHome = "/home/node"
	// EnvCCDGatewayUpstream tells ccd where the sidecar is.
	EnvCCDGatewayUpstream = "CCD_GATEWAY_UPSTREAM"
)

// GatewaySidecarResources is the sidecar's budget: measured ~380MiB idle.
var GatewaySidecarResources = provision.Resources{CPURequest: "100m", MemoryRequest: "512Mi", MemoryLimit: "1536Mi"}

// GatewaySidecar asks buildClaudeCode (via [Options.GatewaySidecar]) for the
// gateway runtime beside ccd. Only a claude-code CHIEF is given one (the caller
// decides; agentprovision's buildClaudeCodeSpec).
type GatewaySidecar struct{}

// gatewaySidecarUpstream is ccd's view of it.
func gatewaySidecarUpstream() string {
	return "http://127.0.0.1:" + strconv.Itoa(GatewaySidecarPort)
}

// buildGatewaySidecar builds the sidecar for a (claude-code) agent row.
func buildGatewaySidecar(a agents.Agent, cfg Config) (*provision.Sidecar, error) {
	if !cfg.RuntimeConfig.Configured() {
		return nil, fmt.Errorf("agentspec: agent %q asks for the gateway runtime beside it, and this deployment "+
			"has no runtime bundle for it (MUSTER_AGENT_GATEWAY, MUSTER_AGENT_RUNTIME_CONFIG, MUSTER_AGENT_RUNTIME_INSTALL)", a.Name)
	}
	ga := a
	ga.Kind, ga.CCAccount, ga.Repo, ga.RepoBranch = agents.KindGateway, "", "", ""
	gcfg := cfg
	gcfg.WorkspacePersist = false
	gw, err := Build(ga, gcfg, Options{})
	if err != nil {
		return nil, fmt.Errorf("agentspec: the gateway runtime beside agent %q: %w", a.Name, err)
	}
	if len(gw.Init) > 0 {
		return nil, fmt.Errorf("agentspec: the gateway runtime beside agent %q would need %d init step(s), and a "+
			"sidecar runs none", a.Name, len(gw.Init))
	}
	env := append([]provision.EnvVar{}, gw.Env...)
	env = append(env,
		provision.EnvVar{Name: "HOME", Value: GatewaySidecarHome},
		provision.EnvVar{Name: GatewaySidecarPortEnv, Value: strconv.Itoa(GatewaySidecarPort)})
	// MUSTER_CONFIG is what the k8s driver renders Spec.Config as for the
	// instance's own container; a sidecar has no Config of its own, so the same
	// variable is set here, from the same value.
	if len(gw.Config) > 0 {
		b, err := json.Marshal(gw.Config)
		if err != nil {
			return nil, fmt.Errorf("agentspec: the gateway runtime's config for %q: %w", a.Name, err)
		}
		env = append(env, provision.EnvVar{Name: "MUSTER_CONFIG", Value: string(b)})
	}
	rt := gw.Runtime
	rt.WorkingDir = ""
	return &provision.Sidecar{
		Name:      GatewaySidecarName,
		Runtime:   rt,
		Env:       env,
		Secrets:   gw.Secrets,
		Files:     gw.Files,
		Resources: GatewaySidecarResources,
	}, nil
}
