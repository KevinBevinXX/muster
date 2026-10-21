package agentspec

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

// The claude-code kind's profile: the Claude Code pod (cmd/ccd + the pinned CLI,
// images/claude-code-agent).
//
// Every value below is a property of THAT IMAGE, read from its Dockerfile,
// entrypoint and ccd's own config, not chosen here:
//
//   - port 18789, named provision.DefaultPortName: ccd's listen port
//     (agentspec.DefaultGatewayPort — ccd serves muster's /v1/responses wire on it);
//   - health "/" on that port, startup + liveness and NO readiness (the k8s
//     driver's fixed shape, see k8s/render.go): ccd's `GET /` gates only on its
//     tmux session and its CLI not crash-looping, NEVER on auth or a rate limit,
//     so a rate-limited account cannot get its pod restarted;
//   - uid/gid 1000: the image's `USER 1000:1000`;
//   - the volume at /data: CLAUDE_CONFIG_DIR=/data/claude and
//     CCD_WORKSPACE=/data/workspace are siblings under it, so the conversation
//     transcripts `claude --resume <id>` resumes survive a pod restart;
//   - resources: the scoping measurement (an idle TUI ~300Mi/128m; long-running
//     real sessions 460-562MiB RSS) — request 512Mi/250m, limit 3Gi, no CPU limit.
const (
	// EnvClaudeOAuthToken is the variable the Claude Code CLI reads its
	// subscription `setup-token` from.
	//
	// 🔴 IT IS PLACED IN EXACTLY ONE PLACE: the claude-code agent's own per-agent
	// Secret (Spec.Secrets, rendered by the k8s driver as the `<agent>-env`
	// Secret, consumed by envFrom). Never Env, never a File, never another kind's
	// spec. TestTheClaudeTokenNameAppearsOnlyInTheClaudeCodeKindsSecret is the seam
	// ledger that holds it there.
	EnvClaudeOAuthToken = "CLAUDE_CODE_OAUTH_TOKEN"

	// ClaudeCodeDataPath is where the agent's persistent volume is mounted.
	ClaudeCodeDataPath = "/data"
	// ClaudeCodeWorkspacePath is the CLI's workspace (the image's CCD_WORKSPACE).
	ClaudeCodeWorkspacePath = "/data/workspace"
	// ClaudeCodeUID is the image's numeric user and group.
	ClaudeCodeUID = 1000
	// DefaultClaudeCodeStorage is the volume size when the profile names none.
	DefaultClaudeCodeStorage = "10Gi"
	// LabelKind is the label a claude-code instance carries naming its kind.
	LabelKind = "muster.agent/kind"
)

// ClaudeCodeResources is the claude-code kind's compute budget.
var ClaudeCodeResources = provision.Resources{CPURequest: "250m", MemoryRequest: "512Mi", MemoryLimit: "3Gi"}

// ClaudeCodeSecurity is the claude-code kind's isolation.
//
// 🔴 NoServiceAccountToken IS TRUE: a shell-capable agent holding a subscription
// token does not also get a Kubernetes API credential. A privilege profile
// granted to such an agent binds RBAC to a ServiceAccount whose token is never
// mounted, so the grant has no effect on it — by design, until a deliberate
// opt-in exists (scoping proposal Q6).
var ClaudeCodeSecurity = provision.Security{
	RunAsUser:             ClaudeCodeUID,
	RunAsGroup:            ClaudeCodeUID,
	FSGroup:               ClaudeCodeUID,
	RunAsNonRoot:          true,
	Restricted:            true,
	NoServiceAccountToken: true,
}

// ClaudeCodeNetwork is the claude-code kind's network confinement: reachable
// only by muster, on ccd's port; able to reach only name resolution and TCP 443
// on public addresses (the Anthropic API and GitHub).
//
// 🔴 IT IS UNCONDITIONAL, LIKE ClaudeCodeSecurity, AND THAT IS WHAT MAKES THE
// KIND FAIL CLOSED. There is no knob that builds a claude-code spec without it:
// a driver that cannot confine an instance refuses the spec (provision.CheckSpec)
// instead of starting a shell-capable pod holding a subscription token with the
// cluster's API, its neighbours and the operator's LAN in reach.
//
// ⚠ WHAT IT DOES NOT BUY: this is "public internet on 443", not "Anthropic and
// GitHub". Nothing here names a host. A prompt-injected agent can still send
// what it holds to any public HTTPS endpoint.
//
// ⚠ MUSTER ITSELF IS NOT REACHABLE FROM THE POD through this policy, although
// the spec still hands it MUSTER_API_URL: that address is in-cluster, so it is on
// the wrong side of this. The ONE deliberate allowance is the connector surface:
// see claudeCodeNetworkFor, which adds egress to muster's connect port (and no
// other) when the deployment offers the connector store.
func ClaudeCodeNetwork() provision.Network {
	return provision.Network{Isolate: true, PublicEgressTCPPorts: []int{ClaudeCodePublicEgressPort}}
}

// claudeCodeNetworkFor is ClaudeCodeNetwork plus, when the deployment offers the
// connector store ([Config.ConnectURL]), egress to muster's connector port — the
// one allowance that lets the pod reach muster, and only that surface of it.
func claudeCodeNetworkFor(cfg Config) provision.Network {
	n := ClaudeCodeNetwork()
	if cfg.ClaudeCodeMCPOffered() {
		n.ControllerEgressTCPPorts = []int{cfg.ConnectPort}
	}
	return n
}

// ClaudeCodeMCPOffered reports whether a claude-code agent built from this
// Config can reach muster's MCP server — its ONLY route to muster's tools (the
// image carries no muster CLI). ccd registers that server exactly when the pod
// carries [EnvConnectURL] (cmd/ccd/seed.go seedMCP), which claudeCodeEnv sets
// from this same predicate, and the network policy's one controller allowance
// is decided by it too. A caller writing prose that names those tools reads it
// here rather than spelling `ConnectURL != ""` a second time.
func (c Config) ClaudeCodeMCPOffered() bool { return c.ConnectURL != "" }

// ClaudeCodeInstructionsPath is where a claude-code agent's [Options.Instructions]
// is placed: the Claude Code CLI's MANAGED-POLICY memory file on Linux.
//
// Documented at https://code.claude.com/docs/en/memory ("Choose where to put
// CLAUDE.md files": managed policy, "Linux and WSL: /etc/claude-code/CLAUDE.md";
// and "Managed policy CLAUDE.md files cannot be excluded"). It is loaded at every
// launch, first in load order, alongside — never instead of — any CLAUDE.md the
// session's working directory holds. MEASURED in the pinned CLI (2.1.296, the
// linux-x64 binary's bundled source): the "Managed" memory file is the managed
// directory joined with "CLAUDE.md", and that directory's non-macOS, non-Windows
// case returns "/etc/claude-code". Re-check it when CLAUDE_CODE_VERSION moves.
//
// 🔴 WHY THERE AND NOT THE WORKSPACE: /data/workspace is the agent's persistent
// volume and whatever it holds is the session's. A project CLAUDE.md placed there
// by a subPath mount would SHADOW a file of the same name the session wrote or a
// repository brought in, and would itself be edited by nobody. The managed path
// is outside every volume, so nothing the agent owns is touched, the file is
// read-only to it, and the driver re-renders it from the spec on every apply
// (the instance picks it up when its pod next starts — the same as the gateway
// kind's AGENTS.md).
//
// ⚠ NOT --append-system-prompt: the CLI is launched by ccd's supervisor with a
// fixed argv (`claude` / `claude --resume <id>`), so a flag would mean an image
// change, and the docs scope that flag to scripted use.
const ClaudeCodeInstructionsPath = "/etc/claude-code/CLAUDE.md"

// ClaudeCodePublicEgressPort is the one TCP port a claude-code agent may reach
// on public addresses: HTTPS.
const ClaudeCodePublicEgressPort = 443

// ClaudeCodeConfig enables the claude-code kind. A nil *ClaudeCodeConfig on
// [Config] means the kind is NOT enabled, and Build refuses a claude-code row.
type ClaudeCodeConfig struct {
	// Image is the FULL image reference, tag or digest included. Required.
	Image string
	// Accounts maps an account name to its setup-token. The agent row names its
	// account (agents.Agent.CCAccount); Build looks the token up here.
	Accounts map[string]string
	// StorageSize is the /data volume's size. Empty means [DefaultClaudeCodeStorage].
	StorageSize string
}

// buildClaudeCode is Build for a claude-code row. Pure, like Build.
//
// 🔴 EVERY MISSING INPUT IS A REFUSAL, NAMING IT. A claude-code pod with no
// image, no account, an unknown account or no hooks token would still be a
// valid-looking Deployment — and would boot into a TUI that cannot answer one
// turn (or a ccd that refuses to start), which reads on the card as a slow agent.
//
// ⚠ Options IS MOSTLY NOT APPLICABLE HERE, AND WHAT IS NOT APPLICABLE IS
// REFUSED: SeedFiles, ExtraEnv and CairnEligible — a caller that asked for them
// would otherwise get an agent silently without them. Instructions IS honoured:
// it is placed at [ClaudeCodeInstructionsPath], the CLI's managed-policy
// CLAUDE.md (see that constant for why there and not in the workspace). So is
// GatewaySidecar (chief_sidecar.go).
func buildClaudeCode(a agents.Agent, cfg Config, opts Options) (provision.Spec, error) {
	cc := cfg.ClaudeCode
	if cc == nil {
		return provision.Spec{}, fmt.Errorf("agentspec: agent %q is kind %s, which this deployment has not "+
			"enabled (MUSTER_AGENT_KINDS)", a.Name, agents.KindClaudeCode)
	}
	if strings.TrimSpace(cc.Image) == "" {
		return provision.Spec{}, fmt.Errorf("agentspec: ClaudeCodeConfig.Image is required")
	}
	if strings.TrimSpace(cfg.APIBaseURL) == "" {
		return provision.Spec{}, fmt.Errorf("agentspec: Config.APIBaseURL is required (an instance with no base URL cannot reach muster)")
	}
	if len(opts.SeedFiles) > 0 || len(opts.ExtraEnv) > 0 || opts.CairnEligible {
		return provision.Spec{}, fmt.Errorf("agentspec: agent %q is kind %s, which takes no seed files, extra "+
			"environment or subsystem-store credential", a.Name, agents.KindClaudeCode)
	}
	if a.CCAccount == "" {
		return provision.Spec{}, fmt.Errorf("agentspec: agent %q is kind %s but names no Claude account", a.Name, agents.KindClaudeCode)
	}
	token, ok := cc.Accounts[a.CCAccount]
	if !ok || token == "" {
		// The account NAME only — never a token, and not the configured set's
		// values either.
		return provision.Spec{}, fmt.Errorf("agentspec: agent %q runs on Claude account %q, which is not "+
			"configured (MUSTER_AGENT_CC_ACCOUNTS); an agent keeps its account for life, so restore that "+
			"account's token rather than expecting a re-selection", a.Name, a.CCAccount)
	}
	repo, err := podRepo(a)
	if err != nil {
		return provision.Spec{}, err
	}
	if a.HooksToken == "" {
		return provision.Spec{}, fmt.Errorf("agentspec: agent %q has no hooks token; ccd refuses to start "+
			"without one (it derives the bearer muster sends from it)", a.Name)
	}
	if cfg.ConnectURL != "" && (cfg.ConnectPort < 1 || cfg.ConnectPort > 65535) {
		return provision.Spec{}, fmt.Errorf("agentspec: Config.ConnectURL is set but Config.ConnectPort (%d) is not a "+
			"port; a claude-code agent's network policy could not allow it to reach the connector surface", cfg.ConnectPort)
	}

	ref := provision.Ref{Name: a.Name, ID: a.ID}
	if err := ref.Validate(); err != nil {
		return provision.Spec{}, fmt.Errorf("agentspec: agent %d has an unusable name: %w", a.ID, err)
	}
	size := cc.StorageSize
	if size == "" {
		size = DefaultClaudeCodeStorage
	}
	labels := buildLabels(a, cfg)
	labels[LabelKind] = agents.KindClaudeCode

	var sidecar *provision.Sidecar
	if opts.GatewaySidecar != nil {
		if sidecar, err = buildGatewaySidecar(a, cfg); err != nil {
			return provision.Spec{}, err
		}
	}
	spec := provision.Spec{
		Ref: ref,
		// 🔴 NO WorkingDir, and the image has none under /data either: a container
		// runtime creates a missing working directory as ROOT after mounting the
		// volume, which on a fresh volume made /data/workspace root-owned 0755 and
		// crash-looped every agent with a repository (0.49.0). The entrypoint's
		// `ccd seed` creates the workspace as uid 1000, the entrypoint then cd's
		// into CCD_WORKSPACE, and ccd starts the CLI there
		// (images/claude-code-agent/entrypoint.sh).
		Runtime: provision.Runtime{Image: cc.Image},
		Env:     claudeCodeEnv(cfg, sidecar != nil, repo),
		// 🔴 THE ONLY PLACE EnvClaudeOAuthToken IS EVER WRITTEN.
		Secrets: []provision.EnvVar{
			{Name: EnvToken, Value: a.HooksToken},
			{Name: EnvGatewayToken, Value: a.HooksToken},
			{Name: EnvClaudeOAuthToken, Value: token},
		},
		Resources: ClaudeCodeResources,
		Workspace: provision.Workspace{Path: ClaudeCodeDataPath, Size: size, Persist: true},
		Ports:     []provision.Port{{Name: provision.DefaultPortName, Port: DefaultGatewayPort}},
		Health:    provision.Health{HTTPGetPath: DefaultGatewayHealthPath, PortName: provision.DefaultPortName},
		Security:  ClaudeCodeSecurity,
		Network:   claudeCodeNetworkFor(cfg),
		Labels:    labels,
		Sidecar:   sidecar,
		Repo:      repo,
	}
	if opts.Instructions != "" {
		spec.Files = []provision.File{{Path: ClaudeCodeInstructionsPath, Content: []byte(opts.Instructions)}}
	}
	if sidecar != nil {
		// ccd presents the sidecar's OWN credential when it hands a gateway turn
		// on, never the one muster sent it (chief_sidecar.go).
		var own string
		for _, e := range sidecar.Secrets {
			if e.Name == EnvGatewayBearer {
				own = e.Value
			}
		}
		if own == "" {
			return provision.Spec{}, fmt.Errorf("agentspec: the gateway runtime beside agent %q has no credential "+
				"of its own (%s)", a.Name, EnvGatewayBearer)
		}
		spec.Secrets = append(spec.Secrets, provision.EnvVar{Name: EnvCCDGatewayUpstreamBearer, Value: own})
		if err := checkSidecarIsolation(spec, cfg.RuntimeConfig.DeriveBearer(a.HooksToken)); err != nil {
			return provision.Spec{}, err
		}
	}
	if err := spec.Validate(); err != nil {
		return provision.Spec{}, fmt.Errorf("agentspec: built an invalid spec for agent %q: %w", a.Name, err)
	}
	return spec, nil
}

// claudeCodeEnv is the claude-code kind's plain environment. With the gateway
// sidecar, ccd is told where it is (EnvCCDGatewayUpstream): the one thing that
// turns a gateway session's turn from not_ready into a hand-off.
//
// With a repository (repo.URL set), ccd is also told which one to check out and
// where, and the CLI's working directory becomes that checkout (EnvCCDWorkspace
// overrides the image's CCD_WORKSPACE): see podRepo. Without one, none of
// the three is set and the pod is exactly what it was before repositories
// existed for this kind (TestAClaudeCodeSpecWithoutARepoIsUnchanged).
func claudeCodeEnv(cfg Config, gatewaySidecar bool, repo provision.Repo) []provision.EnvVar {
	env := []provision.EnvVar{
		{Name: EnvAPIURL, Value: cfg.APIBaseURL},
		{Name: EnvGitTerminalPrompt, Value: "0"},
	}
	if cfg.ClaudeCodeMCPOffered() {
		env = append(env, provision.EnvVar{Name: EnvConnectURL, Value: cfg.ConnectURL})
	}
	if gatewaySidecar {
		env = append(env, provision.EnvVar{Name: EnvCCDGatewayUpstream, Value: gatewaySidecarUpstream()})
	}
	if repo.URL != "" {
		env = append(env, provision.EnvVar{Name: EnvCCDRepo, Value: repo.URL})
		if repo.Branch != "" {
			env = append(env, provision.EnvVar{Name: EnvCCDRepoBranch, Value: repo.Branch})
		}
		env = append(env, provision.EnvVar{Name: EnvCCDWorkspace, Value: repo.Path})
	}
	return env
}

// The variables that hand a claude-code pod its repository. ccd reads them
// (cmd/ccd/checkout.go); TestCcdReadsTheRepositoryVariablesAgentspecWrites pins
// the spellings on both sides.
const (
	// EnvCCDRepo is the repository, owner/name, ccd clones from
	// https://github.com/<owner/name> before it starts the CLI.
	EnvCCDRepo = "CCD_REPO"
	// EnvCCDRepoBranch is the branch to check out (empty: the remote's default).
	EnvCCDRepoBranch = "CCD_REPO_BRANCH"
	// EnvCCDWorkspace is the CLI's working directory. The image sets it to
	// ClaudeCodeWorkspacePath; the spec overrides it ONLY for an agent with a
	// repository, to the checkout, so the session's project is the repository —
	// its CLAUDE.md and .claude/skills load, and the workspace trust ccd seeds is
	// for that directory.
	EnvCCDWorkspace = "CCD_WORKSPACE"
)

// ccRepoDirName is a checkout directory name ccd may create: one path element,
// no traversal.
var ccRepoDirName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)

// podRepo declares a claude-code or hermes agent's repository: the row's
// owner/name and branch, checked out by the pod's supervisor (ccd, hermesd) at
// ClaudeCodeWorkspacePath/<name>. No repository: the zero Repo.
//
// 🔴 THE CHECKOUT IS INSIDE THE WORKSPACE, ON THE AGENT'S VOLUME, so a pod
// restart finds it (neither supervisor re-clones over an existing checkout) and
// the conversation the agent resumes is one of that directory's.
//
// The name must be a plain directory name: a row naming "x/.." would otherwise
// make the agent's working directory somewhere else on the volume.
func podRepo(a agents.Agent) (provision.Repo, error) {
	if a.Repo == "" {
		// A branch alone means nothing without a repository, and was ignored
		// before repositories existed for this kind; it still is.
		return provision.Repo{}, nil
	}
	owner, name, ok := strings.Cut(strings.TrimSpace(a.Repo), "/")
	if !ok || !ccRepoDirName.MatchString(owner) || !ccRepoDirName.MatchString(name) {
		return provision.Repo{}, fmt.Errorf("agentspec: agent %q is kind %s and names repository %q, which is not "+
			"owner/name", a.Name, agents.ResolveKind(a.Kind), a.Repo)
	}
	return provision.Repo{
		URL:    owner + "/" + name,
		Branch: a.RepoBranch,
		Path:   path.Join(ClaudeCodeWorkspacePath, name),
	}, nil
}
