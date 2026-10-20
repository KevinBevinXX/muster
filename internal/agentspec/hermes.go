package agentspec

import (
	"fmt"
	"strings"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

// The hermes kind's profile: the Hermes pod (cmd/hermesd + Hermes at one pinned
// upstream sha, images/hermes-agent; docs/design/hermes.md, "The hermes agent
// kind").
//
// Every value below is a property of THAT IMAGE and of hermesd, not chosen here:
//
//   - port 18789, named provision.DefaultPortName: hermesd's listen port
//     (HERMESD_LISTEN's default), serving muster's /v1/responses wire;
//   - health "/" on that port: hermesd answers 200 unless the gateway latched (a
//     crash loop, or a .env it refuses), so a pod waiting for its checkout,
//     starting or recovering is never restarted for it;
//   - uid/gid 1000: the image's `User = "1000:1000"`;
//   - the volume at /data: HERMES_HOME=/data/hermes (the image's), hermesd's own
//     state at /data/hermesd, the workspace /data/workspace — all on the PVC, so
//     sessions, run records and the API key survive a restart;
//   - the termination grace: over Hermes's 30 s SIGTERM drain (measured by the
//     image smoke test: 34 s idle, 53 s on a loaded host);
//   - resources: Phase 0's soak (RSS 275 → 330 MiB over 790 runs) plus the
//     agent's own shell — the claude-code kind's budget.
//
// 🔴 THE POD IS THE SANDBOX, EXACTLY THE claude-code PROFILE: the same Security
// (no ServiceAccount token, non-root, restricted) and the same network
// confinement, unconditionally. Hermes's terminal tool runs in the pod (the
// `local` backend); its approvals are a policy aid, not a boundary against the
// agent.
const (
	// HermesInstructionsPath is where a hermes agent's [Options.Instructions] is
	// placed: OUTSIDE every volume, read-only, as for claude-code. hermesd reads
	// it on every run and sends it as that run's `instructions`
	// (HERMESD_INSTRUCTIONS's default, cmd/hermesd). Never SOUL.md, never the
	// workspace's .hermes.md (which would suppress the repository's AGENTS.md),
	// never a config prompt (ignored for API-server runs) — Phase 0, S9.
	HermesInstructionsPath = "/etc/hermesd/instructions.md"
	// HermesTerminationGraceSeconds is the pod's terminationGracePeriodSeconds.
	HermesTerminationGraceSeconds = 60
	// DefaultHermesStorage is the volume size when the deployment names none.
	DefaultHermesStorage = "10Gi"
)

// The variables that configure hermesd, as cmd/hermesd reads them;
// TestHermesdReadsTheVariablesAgentspecWrites pins the spellings on both sides.
const (
	// EnvHermesdModel is the OpenRouter model id Hermes runs.
	EnvHermesdModel = "HERMESD_MODEL"
	// EnvHermesdRepo, EnvHermesdRepoBranch and EnvHermesdWorkspace hand hermesd
	// the agent's repository, cloned before Hermes starts, and the working
	// directory (terminal.cwd) it is cloned into.
	EnvHermesdRepo       = "HERMESD_REPO"
	EnvHermesdRepoBranch = "HERMESD_REPO_BRANCH"
	EnvHermesdWorkspace  = "HERMESD_WORKSPACE"
	// EnvTerminalHomeMode makes the agent shell's HOME $HERMES_HOME/home rather
	// than leaving it to Hermes's container detection (S8). hermesd sets it on
	// the gateway too; the spec states it so the pod's own environment says so.
	EnvTerminalHomeMode = "TERMINAL_HOME_MODE"
)

// HermesResources is the hermes kind's compute budget.
var HermesResources = provision.Resources{CPURequest: "250m", MemoryRequest: "512Mi", MemoryLimit: "3Gi"}

// HermesConfig enables the hermes kind. A nil *HermesConfig on [Config] means
// the kind is NOT enabled, and Build refuses a hermes row.
type HermesConfig struct {
	// Image is the FULL image reference, tag or digest included. Required.
	Image string
	// StorageSize is the /data volume's size. Empty means [DefaultHermesStorage].
	StorageSize string
}

// buildHermes is Build for a hermes row. Pure, like Build.
//
// 🔴 EVERY MISSING INPUT IS A REFUSAL, NAMING IT, for buildClaudeCode's reason:
// a pod with no image, no hooks token or no provider credential is still a
// valid-looking Deployment, and would read on the card as a slow agent.
//
// ⚠ Options: Instructions is honoured (placed at [HermesInstructionsPath]);
// SeedFiles, ExtraEnv, CairnEligible and GatewaySidecar are refused — a caller
// that asked for them would otherwise get an agent silently without them.
func buildHermes(a agents.Agent, cfg Config, opts Options) (provision.Spec, error) {
	h := cfg.Hermes
	if h == nil {
		return provision.Spec{}, fmt.Errorf("agentspec: agent %q is kind %s, which this deployment has not "+
			"enabled (MUSTER_AGENT_KINDS)", a.Name, agents.KindHermes)
	}
	if strings.TrimSpace(h.Image) == "" {
		return provision.Spec{}, fmt.Errorf("agentspec: HermesConfig.Image is required")
	}
	if strings.TrimSpace(cfg.APIBaseURL) == "" {
		return provision.Spec{}, fmt.Errorf("agentspec: Config.APIBaseURL is required (an instance with no base URL cannot reach muster)")
	}
	if len(opts.SeedFiles) > 0 || len(opts.ExtraEnv) > 0 || opts.CairnEligible || opts.GatewaySidecar != nil {
		return provision.Spec{}, fmt.Errorf("agentspec: agent %q is kind %s, which takes no seed files, extra "+
			"environment, subsystem-store credential or gateway sidecar", a.Name, agents.KindHermes)
	}
	if a.CCAccount != "" {
		return provision.Spec{}, fmt.Errorf("agentspec: agent %q is kind %s and names a Claude account; "+
			"only a %s agent may", a.Name, agents.KindHermes, agents.KindClaudeCode)
	}
	if a.HooksToken == "" {
		return provision.Spec{}, fmt.Errorf("agentspec: agent %q has no hooks token; hermesd refuses to start "+
			"without one (it derives the bearer muster sends from it)", a.Name)
	}
	// 🔴 THE PROVIDER CREDENTIAL IS REQUIRED: a Hermes pod without it boots, and
	// fails every turn at the model call. It is the OpenRouter key the gateway
	// kind uses (operator decision), resolved per owner by agentprovision.
	if cfg.OpenRouterAPIKey == "" {
		return provision.Spec{}, fmt.Errorf("agentspec: agent %q is kind %s, which runs on OpenRouter, and there is "+
			"no OpenRouter key to give it (MUSTER_AGENT_OPENROUTER_KEY, or its owner's own key)", a.Name, agents.KindHermes)
	}
	model, err := hermesModel(a, cfg)
	if err != nil {
		return provision.Spec{}, err
	}
	if cfg.ConnectURL != "" && (cfg.ConnectPort < 1 || cfg.ConnectPort > 65535) {
		return provision.Spec{}, fmt.Errorf("agentspec: Config.ConnectURL is set but Config.ConnectPort (%d) is not a "+
			"port; a hermes agent's network policy could not allow it to reach the connector surface", cfg.ConnectPort)
	}
	repo, err := podRepo(a)
	if err != nil {
		return provision.Spec{}, err
	}
	ref := provision.Ref{Name: a.Name, ID: a.ID}
	if err := ref.Validate(); err != nil {
		return provision.Spec{}, fmt.Errorf("agentspec: agent %d has an unusable name: %w", a.ID, err)
	}
	size := h.StorageSize
	if size == "" {
		size = DefaultHermesStorage
	}
	labels := buildLabels(a, cfg)
	labels[LabelKind] = agents.KindHermes

	env := []provision.EnvVar{
		{Name: EnvAPIURL, Value: cfg.APIBaseURL},
		{Name: EnvGitTerminalPrompt, Value: "0"},
		{Name: EnvTerminalHomeMode, Value: "profile"},
		{Name: EnvHermesdModel, Value: model},
	}
	if cfg.ClaudeCodeMCPOffered() {
		// The connector surface: git routing, the approval relay and muster's MCP
		// server (rendered into Hermes's config by hermesd).
		env = append(env, provision.EnvVar{Name: EnvConnectURL, Value: cfg.ConnectURL})
	}
	if repo.URL != "" {
		env = append(env, provision.EnvVar{Name: EnvHermesdRepo, Value: repo.URL})
		if repo.Branch != "" {
			env = append(env, provision.EnvVar{Name: EnvHermesdRepoBranch, Value: repo.Branch})
		}
		env = append(env, provision.EnvVar{Name: EnvHermesdWorkspace, Value: repo.Path})
	}
	spec := provision.Spec{
		Ref: ref,
		// No WorkingDir, as for claude-code: a runtime creates a missing working
		// directory as root on a fresh volume. hermesd's seed creates the layout
		// as uid 1000.
		Runtime: provision.Runtime{Image: h.Image},
		Env:     env,
		// 🔴 THE ONLY PLACE A HERMES AGENT'S OPENROUTER KEY IS WRITTEN: its own
		// Secret, consumed by envFrom, as CLAUDE_CODE_OAUTH_TOKEN is for
		// claude-code. hermesd passes it to the gateway only, and Hermes strips
		// it from the agent's shell (S8).
		Secrets: []provision.EnvVar{
			{Name: EnvToken, Value: a.HooksToken},
			{Name: EnvGatewayToken, Value: a.HooksToken},
			{Name: EnvOpenRouterKey, Value: cfg.OpenRouterAPIKey},
		},
		Resources:               HermesResources,
		Workspace:               provision.Workspace{Path: ClaudeCodeDataPath, Size: size, Persist: true},
		Ports:                   []provision.Port{{Name: provision.DefaultPortName, Port: DefaultGatewayPort}},
		Health:                  provision.Health{HTTPGetPath: DefaultGatewayHealthPath, PortName: provision.DefaultPortName},
		Security:                ClaudeCodeSecurity,
		Network:                 claudeCodeNetworkFor(cfg),
		Labels:                  labels,
		Repo:                    repo,
		TerminationGraceSeconds: HermesTerminationGraceSeconds,
	}
	if opts.Instructions != "" {
		spec.Files = []provision.File{{Path: HermesInstructionsPath, Content: []byte(opts.Instructions)}}
	}
	if err := spec.Validate(); err != nil {
		return provision.Spec{}, fmt.Errorf("agentspec: built an invalid spec for agent %q: %w", a.Name, err)
	}
	return spec, nil
}

// hermesModel is the OpenRouter model id a hermes agent runs: the agent's own
// model, else the deployment's, else the built-in (agents.ResolveModel) —
// muster's slugs name the provider first ("openrouter/<vendor>/<model>"), and
// Hermes, configured for the openrouter provider, takes the OpenRouter id
// after it.
//
// ⚠ AN OpenRouter ID IS vendor/model, SO ONE WITHOUT A SLASH IS OpenRouter's
// OWN: muster's "openrouter/auto" names OpenRouter's "openrouter/auto" router,
// and stripping the prefix there would ask for a model called "auto".
func hermesModel(a agents.Agent, cfg Config) (string, error) {
	slug := agents.ResolveModel(a.Model, cfg.Model)
	id, ok := strings.CutPrefix(slug, "openrouter/")
	if !ok || id == "" {
		return "", fmt.Errorf("agentspec: agent %q is kind %s, which runs on OpenRouter, and its model %q is not an "+
			"OpenRouter model (openrouter/<vendor>/<model>)", a.Name, agents.KindHermes, slug)
	}
	if !strings.Contains(id, "/") {
		id = "openrouter/" + id
	}
	return id, nil
}
