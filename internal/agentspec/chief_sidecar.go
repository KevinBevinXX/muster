package agentspec

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

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

// GatewaySidecarRestartSeconds is how long the sidecar's supervisor waits
// before running the runtime again after it exits (gatewaySidecarCommand).
const GatewaySidecarRestartSeconds = 10

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
	if !installHonoursGatewayPort(cfg.RuntimeConfig.Install) {
		return nil, fmt.Errorf("agentspec: agent %q asks for the gateway runtime beside it, and this deployment's "+
			"install script (MUSTER_AGENT_RUNTIME_INSTALL) never reads $%s: the runtime would serve on its own "+
			"default port, which is %d — ccd's, in the same pod — and either crash-loop the pod or answer muster in "+
			"ccd's place. Update the install script to pass --port \"$%s\" (and bind loopback) before naming "+
			"gateway in MUSTER_CHIEF_SESSION_RUNTIMES", a.Name, GatewaySidecarPortEnv, DefaultGatewayPort,
			GatewaySidecarPortEnv)
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
	//
	// WITHOUT the workspace key: the sidecar mounts no workspace, so a runtime
	// told to use one would try to create it under / as an unprivileged user. Its
	// instructions travel with each turn instead (internal/api's turnInstructions).
	conf := make(map[string]any, len(gw.Config))
	for k, v := range gw.Config {
		if k != ConfigKeyWorkspace {
			conf[k] = v
		}
	}
	if len(conf) > 0 {
		b, err := json.Marshal(conf)
		if err != nil {
			return nil, fmt.Errorf("agentspec: the gateway runtime's config for %q: %w", a.Name, err)
		}
		env = append(env, provision.EnvVar{Name: "MUSTER_CONFIG", Value: string(b)})
	}
	rt := gw.Runtime
	rt.WorkingDir = ""
	rt.Command = gatewaySidecarCommand(rt.Command, GatewaySidecarRestartSeconds)
	return &provision.Sidecar{
		Name:      GatewaySidecarName,
		Runtime:   rt,
		Env:       env,
		Secrets:   gw.Secrets,
		Files:     gw.Files,
		Resources: GatewaySidecarResources,
	}, nil
}

// gatewayPortRef is a shell expansion of [GatewaySidecarPortEnv]: `$NAME` or
// `${NAME…}`, and not a longer name that starts with it.
var gatewayPortRef = regexp.MustCompile(`\$\{?` + GatewaySidecarPortEnv + `\b`)

// installHonoursGatewayPort reports whether the operator's install script reads
// [GatewaySidecarPortEnv] on a line that is not a comment.
//
// 🔴 THE SIDECAR SHARES ccd'S NETWORK NAMESPACE, AND THE RUNTIME'S DEFAULT PORT IS
// ccd's. A script written before the sidecar existed runs the runtime on that
// default (18789, the runtime's DEFAULT_GATEWAY_PORT in the 2026.5.7 image, and
// [DefaultGatewayPort] here). Then either the sidecar loses the bind and crash-
// loops — the pod goes NotReady, its endpoint drops, and the Deployment's Recreate
// takes the Claude Code sessions with it — or, rarer, it wins the bind and
// answers muster in ccd's place. That is what a deployment gets by naming gateway
// in MUSTER_CHIEF_SESSION_RUNTIMES before the runtime ConfigMap is updated, and
// refusing the build names the cause on the agent instead.
//
// ⚠ IT IS A TEXT CHECK, AND IT IS NECESSARY RATHER THAN SUFFICIENT. The script is
// the operator's, opaque and not executable here; what can be checked is that it
// mentions the port at all, as an expansion, outside a comment line. A script
// that reads the variable and then ignores it passes. What this catches is the
// case that actually occurs: a script that predates the variable.
func installHonoursGatewayPort(install []byte) bool {
	for _, line := range strings.Split(string(install), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if gatewayPortRef.MatchString(line) {
			return true
		}
	}
	return false
}

// gatewaySidecarCommand wraps the runtime's command in a supervisor that runs it
// again, after delay seconds, whenever it exits, and forwards TERM/INT to it.
//
// 🔴 THE SIDECAR'S CONTAINER MUST NOT EXIT WHEN THE RUNTIME DOES, BECAUSE EVERY
// CONTAINER IN A POD GATES THE POD'S READINESS. A pod is Ready only when all its
// containers are (the ContainersReady condition); a container that has exited,
// or waits out a CrashLoopBackOff, is not. So a runtime that crashes — a bad
// configuration, a lost bind — would take the pod NotReady and drop the chief's
// Service endpoint, which is ccd's: Claude Code sessions would become unreachable
// over a runtime they do not use. Restarting INSIDE the container keeps the
// container running, so it stays Ready (it has no readiness probe) and the pod's
// readiness is ccd's alone. A broken runtime then shows as gateway turns failing
// at ccd's hand-off, and as this supervisor's line in the sidecar's log.
//
// ⚠ WHY NO PROBE ON THE SIDECAR'S OWN PORT. A readiness or startup probe on it
// would ADD a way for the pod to go NotReady — the opposite of keeping a broken
// sidecar away from ccd's endpoint — and a liveness probe would restart the
// container, which is the same NotReady window this exists to avoid. The case a
// probe would have caught (a runtime serving on ccd's port instead of its own)
// is refused before the pod exists: see installHonoursGatewayPort.
//
// ⚠ NOT COVERED: a kernel OOM kill of the whole container (cgroup v2's
// memory.oom.group, which the kubelet sets) restarts the container itself, so
// that one failure still briefly takes the pod NotReady.
func gatewaySidecarCommand(inner []string, delay int) []string {
	script := fmt.Sprintf(`child=
trap 'kill -TERM "$child" 2>/dev/null; wait "$child"; exit 0' TERM INT
while :; do
  %s &
  child=$!
  wait "$child"
  rc=$?
  echo "muster: the gateway runtime exited ($rc); running it again in %ds (ccd is unaffected)" >&2
  sleep %d &
  child=$!
  wait "$child"
done`, strings.Join(inner, " "), delay, delay)
	return []string{"sh", "-c", script}
}
