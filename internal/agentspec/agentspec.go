// Package agentspec builds a [provision.Spec] from an agent row.
//
// It is plan step 22a of the extraction this project came out of (Phase 3.5). It is
// the FIRST of the three pieces that close doc_seams.go entry 1, and it is
// deliberately the piece that touches neither the provisioner wrappers nor
// the chat path: this package has no HTTP client, no cluster client, and no
// knowledge of any driver.
//
// # What it replaces, and why the shape changed
//
// Upstream, the equivalent code was buildHelmValues — a 428-line function
// returning map[string]any shaped for one vendored Helm chart, supported by
// ~1,000 further lines of helpers. Its output was only meaningful to that
// chart's templates, which is the coupling [provision.Spec] exists to remove.
//
// 🔴 THE SINGLE BIGGEST CHANGE IS THAT FILES ARE DATA. Upstream, every file
// placed into an agent travelled as
//
//	{ printf '%s' '<base64>' | base64 -d > /path; } || :
//
// appended to one giant extraInitCommands string. The `|| :` was not caution: the
// chart's startup script ran under `set -e`, so a bare non-zero exit was a crash
// loop rather than a missing file — and an audit found three shapes where the
// guard had been missed. Every file this package emits is a [provision.File]
// instead, so the driver places it natively and a test can assert the CONTENT
// rather than parsing a shell string. See [provision.File]'s own doc comment.
//
// 🔴 A COROLLARY WORTH STATING: Init survives, and it is not a dumping ground.
// A step belongs there only if it is genuinely imperative — something that must
// RUN, not something that must EXIST. Placing content in Init is how the upstream
// string grew; this package emits exactly one Init entry and argues for it below.
//
// # What this package deliberately does NOT do
//
// Each of these is a named part of another step, not an oversight. They are
// listed here because a reader's first question is "where did the rest of
// buildHelmValues go", and a missing answer reads as a gap.
//
//   - It does not generate INSTRUCTION CONTENT. Upstream that was skill.go's
//     instructionsFor/skillsFor (902 lines), which plan step 23 deletes and
//     ranked item 3 ports. This package takes the rendered text as an input
//     ([Options.Instructions]) and places it as a file. That is the right seam:
//     the mechanism and the prose have different owners and different review
//     needs.
//   - ⚠ IT DOES TOUCH CAIRN NOW, AND THIS BULLET USED TO SAY IT DID NOT. Ranked
//     item 3 ("port the chief→cairn integration") has landed: see cairn.go for
//     the client install, the credential file and the one gate that decides
//     whether either exists, and [ChiefInstructions] for the prose half. What is
//     still true is the SHAPE the old bullet was defending — the coordinates are
//     [Config] fields, eligibility is an [Options] input, and nothing in this
//     package compares an agent name against a constant to decide who gets a
//     credential. Both-unset emits nothing at all, which is the state an
//     installation that has not configured a store runs in.
//   - It does not build the GATEWAY wiring — the bearer derivation, the chat
//     transport, the responses model sentinel. That is plan step 22c, and
//     internal/agents/responses.go already carries its own OWED record naming
//     the two inputs a caller must supply.
//     ⚠ IT DOES DECLARE THE GATEWAY'S PORT NOW, AND THIS BULLET USED TO SAY
//     "the chat endpoint" AMONG WHAT IT OMITS. [DefaultGatewayPort] and
//     [Config.GatewayPort] are read by buildPorts, which is what lets a driver
//     RESOLVE an address for an instance this package specified — without it the
//     k8s driver created no Service and answered provision.ErrNoEndpoint for
//     every agent muster provisioned. That is the ADDRESS, not the transport:
//     nothing here speaks the chat wire, derives a bearer or names a model
//     sentinel, and the sentence is corrected rather than deleted because the
//     separation it draws is still the one that governs what may be added.
//   - It does not ADAPT anything to [api.Provisioner]. That is plan step 22b.
//     Nothing here implements a lifecycle method; Build is a pure function.
//
// # Purity, and what it buys
//
// Build reads no environment, no clock, no filesystem and no network. Every
// input arrives as an argument, so the same inputs always produce the same
// [provision.Spec] — which is what makes the golden in agentspec_test.go a
// meaningful pin rather than a snapshot of one machine. If you find yourself
// wanting os.Getenv here, put it in [Config] instead and let the binary resolve
// it.
package agentspec

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

// DefaultWorkspacePath is where an agent's working files live when [Config] does
// not say otherwise. It matches what the upstream chart used, because the agent
// runtime images already expect to find a workspace there.
const DefaultWorkspacePath = "/data/workspace"

// DefaultImageTag is used when [Config.ImageTag] is empty.
//
// ⚠ IT IS "latest", AND THAT IS A DEFAULT RATHER THAN A RECOMMENDATION. A
// mutable tag means two instances created a week apart can run different code
// with identical specs, which is exactly the ambiguity that made muster's own
// deployment pin a digest. Callers that care should set [Config.ImageTag]; this
// constant exists so that an unset field produces a runnable spec instead of an
// image reference ending in a bare colon.
const DefaultImageTag = "latest"

// DefaultGatewayPort is the port an agent runtime's MODEL GATEWAY listens on,
// used when [Config.GatewayPort] is zero.
//
// 🔴 WITHOUT A PORT IN THE SPEC, AN AGENT muster PROVISIONS HAS NO ADDRESS AT
// ALL. The chain is mechanical and every link of it is in this repository:
// Build produced Ports nil, so k8s's renderService returned nil and created no
// Service; (*k8s.Driver).Endpoint read the port back from the Deployment's
// `muster.dev/port` annotation, which renderAnnotations only writes when
// Spec.PortNumber(provision.DefaultPortName) is non-zero; so
// provision.ResolveEndpoint was handed port 0 and answered
// provision.ErrNoEndpoint — "declares no port". internal/agentgateway resolves
// an endpoint BEFORE it builds a request, so that error was the only observable
// of a chat turn against such an agent. This constant is what breaks that chain.
//
// 🔴 MEASUREMENT, SO THE NUMBER IS NOT A GUESS NOBODY CAN RE-CHECK: a live pod
// of the agent runtime image the deployment this was written against pins
// answered HTTP 200 for a real `/v1/responses` turn on this port, over
// loopback inside the pod. The same pod's injected service environment exposed
// the port under the `..._SERVICE_PORT_GATEWAY` name — the same word
// provision.DefaultPortName carries — and a SECOND port one higher, which is
// that image's skills API and is NOT what a chat turn wants:
// agents.ResponsesURL appends `/v1/responses` to whatever endpoint resolves, so
// the port declared here has to be the gateway's.
//
// 🔴 A DEFAULT *WITH* AN OVERRIDE, WHICH IS STRICTLY MORE THAN THE MODEL
// SENTINEL BESIDE IT GETS, AND THE ASYMMETRY IS THE ARGUMENT. cmd/muster-server's
// config.AgentGatewayModel is configuration and NOT a constant because "the value
// belongs to the IMAGE" and no default could be right for an image this project
// has never seen. The port belongs to the image in exactly the same way — so it
// takes [Config.GatewayPort] for the same reason — but unlike an opaque sentinel
// string it has a defensible default: it is a number that was MEASURED against
// the image a deployment actually runs, and the alternative to defaulting it is
// that every deployment is unreachable until an operator discovers a variable,
// which is the state this constant exists to end. Pointing muster at a runtime
// whose gateway listens elsewhere is therefore a configuration change rather
// than a code change — the same three-layer shape provision.EndpointTemplate
// argues for on the HOST half of the address.
//
// ⚠ IT IS DECLARED WHETHER OR NOT A CHAT GATEWAY IS CONFIGURED, deliberately.
// The port reaches the cluster at CREATE time (the Service and the annotation are
// both written by k8s apply), so gating it on the gateway tier would leave every
// instance provisioned while chat was off permanently addressless — turning on
// MUSTER_AGENT_GATEWAY would then need each agent re-provisioned, and nothing
// would say so.
const DefaultGatewayPort = 18789

// Environment variable names handed to the instance.
//
// 🔴 THESE ARE NOT INVENTED HERE, AND THEY MUST NOT BE RENAMED IN THIS PACKAGE.
// They are the spellings cmd/muster (the in-pod CLI) already reads — see that
// package's envAPIURL/envToken and the comment there, which names the pod case
// explicitly: "Inside a managed AGENT POD it is that pod's OWN per-agent token".
// A rename here would leave the CLI reading names nothing sets, and the failure
// would look like a broken agent rather than a broken spec.
//
// ⚠ THERE IS ONE BASE URL, NOT TWO, AND THAT IS A DELIBERATE NARROWING.
// Upstream injected a ROUTER-side base URL variable *and* a TASK-side one, because
// the permission router and the task board were separate services, and a cutover
// made those two values diverge. muster serves both route families from
// its own mux, so a second variable would have no different value to hold.
// cmd/muster/config.go makes the same argument and cites the test that checks it
// (TestEveryWiredRouteIsServedByThisProject).
const (
	// EnvAPIURL is the base URL the instance's CLI and skills talk to.
	EnvAPIURL = "MUSTER_API_URL"
	// EnvToken carries the agent's OWN per-agent token. It is a SECRET, and
	// Build puts it in [provision.Spec.Secrets] rather than Env for that
	// reason — see the note on Build.
	EnvToken = "MUSTER_HOOK_TOKEN"
	// EnvGatewayToken carries the SAME per-agent token under the name the
	// runtime's OWN chat gateway derives its half of the bearer from. Also a
	// SECRET, and in Secrets for the same reason.
	//
	// 🔴 IT IS A WIRE CONTRACT WITH A SECOND IMPLEMENTATION IN ANOTHER
	// REPOSITORY, WHICH IS WHY THE NAME CANNOT BE CHOSEN HERE. The agent
	// container computes its own gateway credential as
	//
	//	GATEWAY_TOKEN=$(echo -n "gw-${HOOKS_TOKEN}" | sha256sum | cut -d' ' -f1)
	//
	// — see internal/agentgateway/runtime.go, whose Bearer() is muster's half of
	// exactly that formula. The deployment that implements the container half is
	// not importable from here, so the agreement is held by a test rather than by
	// the type system: cmd/muster-server's
	// TestTheProvisionedContainerCanDeriveTheBearerMusterSends reproduces the
	// container's derivation over the built spec's own environment and compares
	// the two bearers.
	//
	// 🔴 IT IS A SECOND NAME FOR ONE VALUE AND *NOT* A RENAME OF EnvToken, AND
	// RENAMING WOULD HAVE TRADED ONE 401 FOR ANOTHER. EnvToken is what the in-pod
	// CLI reads (cmd/muster/config.go's envToken) and what the work-autosave
	// daemon sends when it posts a durability alarm to the agent's own task
	// thread; dropping it would break the instance's call-back path, which fails
	// as "the agent went quiet" rather than as a missing variable. Both names
	// carry a.HooksToken, and buildSecrets emits them together.
	//
	// ⚠ IT IS A CONSTANT RATHER THAN CONFIGURATION, UNLIKE THE PORT AND THE MODEL
	// SENTINEL BESIDE IT, AND THE LINE IS WHERE THE SCHEME'S EDGE IS. This name
	// is part of the `hooks-sha256` derivation, not an independent property of
	// the image: the formula that consumes it is itself hardcoded in
	// agentgateway.HooksSHA256, and MUSTER_AGENT_GATEWAY is the knob that selects
	// the whole scheme. A runtime that read a different variable is a different
	// scheme and brings its own name — which is the change that should also move
	// this constant onto agentgateway.Runtime as a method.
	//
	// ⚠ DECLARING IT IN agentgateway TODAY WAS RULED OUT, not overlooked: this
	// package deliberately imports no HTTP client (see the package doc), and
	// agentgateway holds one. So the name lives in this const block — whose own
	// header already says these spellings "ARE NOT INVENTED HERE" — and the
	// cross-package agreement is held by the seam test named above.
	EnvGatewayToken = "HOOKS_TOKEN"
	// EnvNodeOptions passes Node heap/flag tuning through to the runtime.
	EnvNodeOptions = "NODE_OPTIONS"
	// EnvGitTerminalPrompt is pinned to "0" unconditionally.
	//
	// 🔴 IT IS A HANG GUARD, NOT A PREFERENCE. Git prompts for credentials on a
	// terminal it believes is interactive. In a pod there is nobody to answer,
	// so a clone against a repository the instance cannot read BLOCKS instead of
	// failing — and a blocked init step is indistinguishable from a slow one
	// until the deadline fires. Failing fast is the whole point.
	EnvGitTerminalPrompt = "GIT_TERMINAL_PROMPT"
	// EnvOpenRouterKey is the shared model-provider credential. Also a secret.
	EnvOpenRouterKey = "OPENROUTER_API_KEY"
	// EnvRuntimeConfig tells the operator's install script where muster placed
	// the runtime-configuration template: [RuntimeConfigPath].
	//
	// 🔴 IT EXISTS SO THE SCRIPT DOES NOT HARDCODE A muster PATH. The script
	// lives in the operator's ConfigMap and muster owns where the bundle is
	// mounted; a script spelling the path itself would break on any change to
	// [RuntimeConfigDir], in a container, at startup, with no version of the two
	// values ever compared. It is NOT a secret: it names a file, and the bundle
	// it names holds no credential.
	EnvRuntimeConfig = "MUSTER_RUNTIME_CONFIG"
	// EnvGatewayBearer carries the credential the agent's OWN gateway must
	// accept, already derived. It is a SECRET.
	//
	// 🔴 IT IS THE DERIVED VALUE AND NOT ANOTHER COPY OF THE TOKEN, WHICH IS WHAT
	// MAKES IT DIFFERENT FROM [EnvGatewayToken] BESIDE IT. EnvGatewayToken ships
	// the raw token for a runtime that derives its own credential from it; this
	// ships the RESULT, so the operator's install script needs no hash and no
	// second implementation of muster's formula. See runtimeconfig.go on why that
	// second implementation is the failure mode worth removing: two sha256
	// expressions that disagree produce a 401 reading as a bad credential.
	//
	// ⚠ BOTH NAMES ARE SHIPPED, NOT ONE. EnvGatewayToken stays because it is part
	// of the `hooks-sha256` scheme's contract with runtimes that read it, and
	// because removing a variable the agent image may read is a change this
	// package cannot verify. They carry different values and neither is the
	// other's rename.
	EnvGatewayBearer = "MUSTER_GATEWAY_BEARER"
	// EnvCairnConfig points the subsystem-store client at [CairnConfigPath].
	//
	// 🔴 IT IS SET ONLY WHEN THE INTEGRATION IS ON, and it is NOT a secret: it
	// names a path, and the credential it names travels as a confidential
	// [provision.File]. It is in this block — and in buildEnv's reserved set —
	// because a caller shadowing it through [Options.ExtraEnv] would point the
	// client at a file muster did not write, which fails as "the store is
	// unreachable" rather than as a configuration mistake. See cairn.go.
	EnvCairnConfig = "CAIRN_CONFIG"
	// EnvConnectURL is the base URL of muster's agent-facing connector surface
	// (the proxy at /connect/… and /agent/connectors). Set only when the
	// deployment configures one ([Config.ConnectURL]); not a secret.
	EnvConnectURL = "MUSTER_CONNECT_URL"
)

// InstructionsFileName is the file [Options.Instructions] is written to, inside
// the workspace.
const InstructionsFileName = "AGENTS.md"

// ConfigKeyModel is the one key Build writes into [provision.Spec.Config].
//
// ⚠ Spec.Config IS OPAQUE TO EVERY DRIVER by contract, so this key is a
// statement about the RUNTIME inside the instance, not about provisioning. It is
// named here so the runtime side and this side agree in one place rather than in
// two string literals.
const ConfigKeyModel = "model"

// Config is the deployment-wide part of a spec: everything that is true of every
// agent in this installation rather than of one agent row.
//
// ⚠ EVERY FIELD IS OPTIONAL EXCEPT ImageRepo AND APIBaseURL, and Build says so
// by returning an error rather than by producing a spec that fails later inside
// a driver. The two required ones are required because there is no defensible
// default: an image repository is installation-specific, and a base URL that
// guessed at a service name would produce an instance that cannot phone home.
type Config struct {
	// ImageRepo is the runtime image WITHOUT a tag, e.g.
	// "ghcr.io/example-org/agent-runtime". Required.
	ImageRepo string
	// ImageTag defaults to [DefaultImageTag].
	ImageTag string
	// APIBaseURL is what [EnvAPIURL] is set to. Required.
	APIBaseURL string
	// OpenRouterAPIKey, when set, is injected as [EnvOpenRouterKey] in the
	// SECRETS list. Empty means the installation expects the runtime image to
	// carry its own provider credentials.
	OpenRouterAPIKey string
	// Model is the DEPLOYMENT-WIDE DEFAULT primary model slug, not the slug
	// every agent gets: an agent row carrying its own Model outranks it (see
	// resolvePrimaryModel, which delegates that precedence to
	// [agents.ResolveModel]). Empty means the runtime decides, which is why
	// Build omits the config key entirely rather than writing "".
	Model string
	// ModelFallbacks are tried in order after Model.
	//
	// ⚠ DEPLOYMENT-WIDE, AND DROPPED FOR AN AGENT THAT NAMES ITS OWN MODEL.
	// There is no per-agent fallbacks column, so pairing this chain with a
	// hand-picked per-agent primary would produce a combination no operator
	// authored; buildRuntimeConfig's own comment records the full reasoning.
	ModelFallbacks []string
	// MemoryRequest and MemoryLimit are passed to the driver verbatim, in the
	// units it understands. See [provision.Resources] on why they are strings.
	MemoryRequest string
	MemoryLimit   string
	// CPURequest and CPULimit likewise.
	CPURequest string
	CPULimit   string
	// NodeOptions, when set, becomes [EnvNodeOptions].
	NodeOptions string
	// GatewayPort is the port the runtime image's model gateway listens on. Zero
	// means [DefaultGatewayPort]; see that constant for the measurement and for
	// why this field exists rather than the number being written inline.
	//
	// ⚠ A VALUE OUTSIDE 1-65535 IS REFUSED BY Build NAMING THIS FIELD.
	// [provision.Spec.Validate] would refuse it too, one layer down, but its
	// message names the PORT ("port \"gateway\" number -1 out of range") and not
	// the knob somebody set — and the only way this field holds a bad number is
	// that somebody set it.
	GatewayPort int
	// ConnectURL, when set, is handed to every agent as [EnvConnectURL]: where
	// muster's connector proxy is. Empty means the connector store is not offered.
	ConnectURL string
	// ConnectPort is the POD port of that surface on muster's own pods. A
	// claude-code agent's network policy allows egress to it (and only it) when
	// ConnectURL is set. Required with ConnectURL for that kind.
	ConnectPort int
	// WorkspacePath defaults to [DefaultWorkspacePath].
	WorkspacePath string
	// WorkspaceSize is the workspace volume size, e.g. "10Gi". Empty leaves the
	// driver's own default in place.
	WorkspaceSize string
	// WorkspacePersist asks for storage that survives a restart.
	WorkspacePersist bool
	// Labels are added to every spec this Config builds, BEFORE the per-agent
	// labels, which win on a key collision. Nil is fine.
	Labels map[string]string
	// AutosaveExcludePatterns are gitignore patterns for the agent RUNTIME's own
	// bookkeeping files, handed to the work-autosave daemon so it does not
	// force-push them into the agent's repository.
	//
	// 🔴 THERE IS NO DEFAULT, DELIBERATELY. See autosave.go's note: a built-in list
	// names one specific runtime, and is wrong for every other one in two
	// directions at once. Nil is valid and silent. Anchor each entry with a leading
	// "/" unless you mean it to match at every depth.
	AutosaveExcludePatterns []string

	// CairnURL and CairnToken are the hosted subsystem-store coordinates.
	//
	// 🔴 BOTH-OR-NEITHER, AND INERT WHEN UNSET. With either one empty nothing
	// cairn-related is emitted — no install step, no credential file, no
	// environment variable — and the prose half claims nothing. A token without a
	// URL is useless in exactly the case a credential exists for. Read the
	// pair through [Config.CairnConfigured], never field by field.
	//
	// 🔴 CairnToken IS A READ+WRITE KEY ACROSS EVERY SCOPE OF THE STORE, which is
	// why eligibility is an explicit per-call input ([Options.CairnEligible])
	// rather than something every agent gets. cairn.go's header carries the
	// blast-radius argument in full.
	//
	// ⚠ THERE IS DELIBERATELY NO DEFAULT URL. A built-in would collapse the gate
	// to "is the token set", and a store's address is deployment state rather
	// than a constant this package should assert.
	CairnURL   string
	CairnToken string

	// Plugins reports that the plugin interface is on for this deployment
	// (MUSTER_PLUGINS=1 with a database). It decides one thing here: whether the
	// supervisor's prose carries the plugin section ([ChiefInstructions]). The
	// server sets it from the same predicate that wires the plugin store.
	Plugins bool

	// RuntimeConfig is the operator-supplied bundle that makes the agent
	// runtime's own gateway START. Zero means nothing is installed and the
	// image's entrypoint runs unchanged, which is what every deployment did
	// before this field existed — and, measured, is what crashlooped. See
	// runtimeconfig.go's header for the defect, the measurements, and why the
	// content is the operator's rather than this package's.
	//
	// ⚠ IT IS A deployment-WIDE FIELD HOLDING ONE PER-AGENT FUNCTION, which is
	// the only shape in this struct that mixes the two. The template and the
	// script are properties of the IMAGE, so they belong here; the credential is
	// derived from the agent ROW, so it cannot be a value here — hence
	// [RuntimeConfig.DeriveBearer] rather than a fourth [Options] field, which
	// would have let a caller ship a bundle with no derivation.
	RuntimeConfig RuntimeConfig

	// ClaudeCode enables the claude-code agent kind and carries its profile's
	// deployment half (image, account tokens). Nil means the kind is not enabled.
	// It is read ONLY by a claude-code row's build: the gateway kind's spec is
	// identical whether or not this is set. See claudecode.go.
	ClaudeCode *ClaudeCodeConfig
}

// Options is the per-call part that is neither deployment config nor a column on
// the agent row.
type Options struct {
	// Instructions is the rendered agent-instruction document. When non-empty
	// Build writes it to [InstructionsFileName] inside the workspace.
	//
	// 🔴 ITS CONTENT IS NOT THIS PACKAGE'S BUSINESS, and that separation is the
	// point. Upstream this text was generated by skill.go, which plan step 23
	// deletes; ranked item 3 owns porting the generator. Keeping it an input
	// means item 3 changes what the file SAYS without touching how it is
	// PLACED, and a test here can assert placement without asserting prose.
	Instructions string

	// SeedFiles are extra files to place in the workspace. Keys are names
	// relative to the workspace root; a key containing a path separator is an
	// error rather than a silently-created subdirectory, because the upstream
	// shell form could not express one and a caller writing "a/b.md" is more
	// likely to be confused than deliberate.
	//
	// Upstream these were the chief-only BOOTSTRAP.md / IDENTITY.md / USER.md
	// writes in workspaceSeedInitCommands, selected by comparing the agent name
	// against a hardcoded constant. That selection is a CALLER's policy, not a
	// spec-building rule, so it does not live here.
	SeedFiles map[string]string

	// ExtraEnv is appended after the env this package computes. A name that
	// collides with one of the constants above is an error: silently shadowing
	// [EnvAPIURL] would point an instance at the wrong server, and silently
	// dropping the caller's value would be worse.
	ExtraEnv []provision.EnvVar

	// CairnEligible declares that THIS agent may hold the subsystem-store
	// credential. It is one half of the gate; [Config.CairnConfigured] is the
	// other, and both must be true before anything is emitted.
	//
	// 🔴 IT IS AN INPUT BECAUSE ELIGIBILITY IS A CALLER'S POLICY, exactly as
	// [Options.SeedFiles] argues for its own case: upstream this was an agent name
	// compared against a hardcoded constant inside the spec builder, and a
	// name-to-privilege rule is not a spec-building rule. The caller that knows
	// which agent is the supervisor sets it. Today that is one agent; see
	// cairn.go's header for why widening it is a one-line change here and an
	// unrecoverable one in the store.
	//
	// ⚠ SETTING IT WITH NO CREDENTIAL CONFIGURED IS NOT AN ERROR, it is simply
	// inert. The alternative — refusing — would make an installation that has not
	// configured a store unable to dispatch its supervisor at all.
	CairnEligible bool
}

// Build assembles the spec for one agent.
//
// 🔴 SECRETS GO IN Secrets, NEVER IN Env, AND THE SPLIT IS THE WHOLE REASON
// [provision.Spec] HAS TWO FIELDS. The type system is what makes the
// confidential set enumerable: a driver cannot accidentally log, diff or
// ConfigMap something it was handed through Secrets. Two SECRETS qualify here —
// the agent's own per-agent token and the shared provider key — and both are
// routed accordingly. If you add a third, ask which list it belongs in before
// asking where in the function to put it.
//
// ⚠ TWO SECRETS, THREE ENTRIES: this sentence said "Two values" and the list has
// three. The per-agent token travels under BOTH [EnvToken] and
// [EnvGatewayToken] — one value, two names, because the in-pod CLI and the
// runtime's own gateway read different variables. buildSecrets emits the pair
// from one branch so they cannot drift apart; the count differing from the
// secret count is the thing a reader is owed rather than left to discover.
//
// The returned spec is validated before it is returned, so a nil error means the
// spec satisfies [provision.Spec.Validate] and every driver's precondition that
// does not depend on the driver's own capabilities. A driver may still refuse it
// through CheckSpec — that is a capability question, not a construction one.
func Build(a agents.Agent, cfg Config, opts Options) (provision.Spec, error) {
	// 🔴 THE KIND IS DECIDED FIRST, AND THE GATEWAY KIND'S PATH BELOW IS THE
	// PRE-KINDS Build UNCHANGED. A row with no kind, or kind gateway, falls through
	// to exactly the code every agent was built by before kinds existed — its
	// golden (testdata/spec.golden.json) is byte-identical. A claude-code row takes
	// its own profile (claudecode.go) and NOTHING from the gateway kind's image,
	// runtime-config bundle, model or provider key.
	switch kind := agents.ResolveKind(a.Kind); kind {
	case agents.KindGateway:
		if a.CCAccount != "" {
			return provision.Spec{}, fmt.Errorf("agentspec: agent %q is kind %s and names a Claude account; "+
				"only a %s agent may", a.Name, kind, agents.KindClaudeCode)
		}
	case agents.KindClaudeCode:
		return buildClaudeCode(a, cfg, opts)
	default:
		return provision.Spec{}, fmt.Errorf("agentspec: agent %q has unknown kind %q", a.Name, a.Kind)
	}
	if strings.TrimSpace(cfg.ImageRepo) == "" {
		return provision.Spec{}, fmt.Errorf("agentspec: Config.ImageRepo is required (there is no defensible default for an installation-specific registry)")
	}
	if strings.TrimSpace(cfg.APIBaseURL) == "" {
		return provision.Spec{}, fmt.Errorf("agentspec: Config.APIBaseURL is required (an instance with no base URL cannot reach muster, and a guessed service name would fail at runtime instead of here)")
	}

	ref := provision.Ref{Name: a.Name, ID: a.ID}
	if err := ref.Validate(); err != nil {
		return provision.Spec{}, fmt.Errorf("agentspec: agent %d has an unusable name: %w", a.ID, err)
	}

	workspace := cfg.WorkspacePath
	if workspace == "" {
		workspace = DefaultWorkspacePath
	}

	env, err := buildEnv(cfg, opts)
	if err != nil {
		return provision.Spec{}, err
	}
	files, err := buildFiles(workspace, cfg, opts)
	if err != nil {
		return provision.Spec{}, err
	}
	ports, err := buildPorts(cfg)
	if err != nil {
		return provision.Spec{}, err
	}

	repo := buildRepo(a, workspace)
	// 🔴 OFF-INSTANCE DURABILITY, GATED ON THERE BEING A REPOSITORY TO PUSH TO.
	// See autosave.go for what this is and why it arrived in this step. The gate is
	// Repo, not a credential check: whether the push can actually authenticate is
	// not knowable here — provision.Repo is DECLARED, NOT CLONED, so nothing in
	// this package has a working tree or a credential helper to test. The daemon
	// answers that question itself at runtime and posts a durability alarm to the
	// agent's own task thread when it cannot push, which is a better place for it
	// than a build-time guess.
	init, err := buildInit(workspace, cfg, opts)
	if err != nil {
		return provision.Spec{}, err
	}
	if repo.URL != "" {
		files = append(files, autosaveFile())
		cmd, err := autosaveInvocation(repo.Path, a.Name, a.ID, cfg.AutosaveExcludePatterns)
		if err != nil {
			return provision.Spec{}, err
		}
		init = append(init, cmd)
	}

	// 🔴 THE BUNDLE'S THREE EFFECTS ARE DECIDED BY ONE PREDICATE, HERE, AND THAT
	// IS WHAT MAKES A HALF-INSTALL UNEXPRESSIBLE. The files, the command override
	// and the derived credential are useless apart: a script with no command
	// override never runs, a command override with no script leaves the container
	// executing a path that does not exist, and either without the credential
	// installs an empty one. buildFiles, buildEnv and the secret below each ask
	// [RuntimeConfig.Configured] rather than each being gated on a field of its
	// own, which is the same single-branch discipline buildSecrets uses for the
	// token pair and buildFiles uses for the cairn pair.
	//
	// ⚠ THE CREDENTIAL IS DERIVED *BEFORE* THE SPEC IS ASSEMBLED, so a tokenless
	// agent is refused with its own message instead of producing a spec whose
	// installer writes an empty one. See runtimeConfigBearer.
	var bearer string
	if cfg.RuntimeConfig.Configured() {
		bearer, err = runtimeConfigBearer(cfg, a.HooksToken)
		if err != nil {
			return provision.Spec{}, err
		}
	}

	runtime := provision.Runtime{
		Image:      imageRef(cfg),
		WorkingDir: workspace,
	}
	if cfg.RuntimeConfig.Configured() {
		// 🔴 THIS OVERRIDES THE IMAGE'S ENTRYPOINT *AND* DISCARDS ITS DEFAULT
		// ARGUMENTS, because Kubernetes drops CMD whenever Command is set and Args
		// is empty. Args is deliberately left empty rather than carrying the
		// image's own default command through: muster does not know what that
		// command is, and a guess would be the vendor identifier this whole design
		// keeps out of the repository. Re-exec'ing the real entrypoint is the
		// operator's script's last line — see [RuntimeConfig.Install].
		runtime.Command = runtimeConfigCommand()
	}

	spec := provision.Spec{
		Ref:     ref,
		Runtime: runtime,
		Env:     env,
		Secrets: buildSecrets(a, cfg, bearer),
		Files:   files,
		Init:    init,
		Resources: provision.Resources{
			CPURequest:    cfg.CPURequest,
			CPULimit:      cfg.CPULimit,
			MemoryRequest: cfg.MemoryRequest,
			MemoryLimit:   cfg.MemoryLimit,
		},
		Workspace: provision.Workspace{
			Path:    workspace,
			Size:    cfg.WorkspaceSize,
			Persist: cfg.WorkspacePersist,
		},
		Repo:   repo,
		Config: buildRuntimeConfig(a, cfg),
		Ports:  ports,
		// 🔴 DECLARED UNCONDITIONALLY, FOR THE REASON [DefaultGatewayPort] IS:
		// both reach the cluster at CREATE time, in the pod template, so gating
		// either on a tier being configured would leave every instance
		// provisioned beforehand permanently without it and nothing would say so.
		// A crashlooping agent reporting 0/1 for ever with nothing timing it out
		// is the defect this closes, and it does not depend on whether chat is on.
		//
		// ⚠ THE PORT IS THE ONE THIS NAMES, VIA provision.DefaultPortName, SO THE
		// PROBE AND THE SERVICE CANNOT DISAGREE. buildPorts declares exactly that
		// name and provision.Spec.Validate refuses a Health naming a port the spec
		// does not declare, so the two are checked against each other rather than
		// maintained in parallel.
		Health: provision.Health{
			HTTPGetPath: DefaultGatewayHealthPath,
			PortName:    provision.DefaultPortName,
		},
		Labels: buildLabels(a, cfg),
	}

	if err := spec.Validate(); err != nil {
		return provision.Spec{}, fmt.Errorf("agentspec: built an invalid spec for agent %q: %w", a.Name, err)
	}
	return spec, nil
}

// imageRef joins the repo and tag. It does NOT validate the result: a registry's
// legal reference shapes are the registry's business, and a parser here would be
// a second, wrong authority — the same argument [provision.Resources] makes for
// keeping quantities as strings.
func imageRef(cfg Config) string {
	tag := cfg.ImageTag
	if tag == "" {
		tag = DefaultImageTag
	}
	return cfg.ImageRepo + ":" + tag
}

// buildEnv computes the non-sensitive environment.
//
// The order is fixed rather than map-derived so that the golden test pins a
// sequence: a spec whose env order varies between runs cannot be compared
// byte-for-byte, and losing that comparison costs more than the ordering
// constrains.
func buildEnv(cfg Config, opts Options) ([]provision.EnvVar, error) {
	env := []provision.EnvVar{
		{Name: EnvAPIURL, Value: cfg.APIBaseURL},
		{Name: EnvGitTerminalPrompt, Value: "0"},
	}
	if cfg.NodeOptions != "" {
		env = append(env, provision.EnvVar{Name: EnvNodeOptions, Value: cfg.NodeOptions})
	}
	// The subsystem-store client's config path. See cairn.go — it names a file,
	// so it is env rather than a secret, and it is absent entirely when the
	// integration is off.
	if cairnEnabled(cfg, opts) {
		env = append(env, provision.EnvVar{Name: EnvCairnConfig, Value: CairnConfigPath})
	}
	// The runtime-config template's path, for the operator's install script. It
	// names a file, so it is env rather than a secret, and it is absent entirely
	// when no bundle is configured — a variable pointing at a file muster did not
	// place is worse than no variable, because a script under `-u` would proceed
	// on it rather than fail.
	if cfg.RuntimeConfig.Configured() {
		env = append(env, provision.EnvVar{Name: EnvRuntimeConfig, Value: RuntimeConfigPath})
	}
	// Where the connector proxy is. Absent when not configured, so a deployment
	// without the store builds byte-identical specs (and fingerprints).
	if cfg.ConnectURL != "" {
		env = append(env, provision.EnvVar{Name: EnvConnectURL, Value: cfg.ConnectURL})
	}

	// A collision check, not a merge. See Options.ExtraEnv.
	//
	// ⚠ EnvCairnConfig IS RESERVED UNCONDITIONALLY, not only when the integration
	// is on. A reserved set that changed with the configuration would accept a
	// caller's CAIRN_CONFIG in the default deployment and refuse the identical
	// call once a store was configured — a collision check whose answer depends on
	// unrelated state is worse than none.
	// ⚠ EnvGatewayToken IS RESERVED FOR A REASON THE OTHERS DO NOT SHARE: a caller
	// shadowing it does not merely point the instance somewhere wrong, it changes
	// the value the instance HASHES, so the container's bearer stops matching the
	// one muster derives from the row. The failure is a 401 on every chat turn
	// attributed to the credential rather than to the shadow.
	// ⚠ THE TWO RUNTIME-CONFIG NAMES ARE RESERVED UNCONDITIONALLY, like
	// EnvCairnConfig above and for the identical reason: a reserved set that
	// changed with the configuration would accept a caller's value on a
	// deployment with no bundle and refuse the identical call once one was
	// configured. EnvGatewayBearer is the one whose shadow is worst — a caller's
	// value would be installed as the gateway's credential while muster sent its
	// own, which is a 401 on every turn attributed to the derivation.
	reserved := map[string]bool{
		EnvAPIURL:            true,
		EnvGitTerminalPrompt: true,
		EnvNodeOptions:       true,
		EnvToken:             true,
		EnvGatewayToken:      true,
		EnvGatewayBearer:     true,
		EnvRuntimeConfig:     true,
		EnvOpenRouterKey:     true,
		EnvCairnConfig:       true,
		EnvConnectURL:        true,
		// Reserved so no caller can hand the Claude subscription token to a
		// gateway-kind agent through ExtraEnv; see EnvClaudeOAuthToken.
		EnvClaudeOAuthToken: true,
	}
	for _, e := range opts.ExtraEnv {
		if reserved[e.Name] {
			return nil, fmt.Errorf("agentspec: Options.ExtraEnv may not set %s — this package computes it, and shadowing it would point the instance at the wrong server or credential", e.Name)
		}
		env = append(env, e)
	}
	return env, nil
}

// ResolveGatewayPort is the ONE place a zero [Config.GatewayPort] becomes
// [DefaultGatewayPort].
//
// 🔴 IT IS EXPORTED BECAUSE THERE ARE TWO READERS AND THEY MUST NOT DISAGREE,
// WHICH IS THE ARGUMENT agents.ResolveNamespacePrefix ALREADY WON IN THIS
// REPOSITORY. buildPorts resolves it to put the number in the spec;
// cmd/muster-server's boot banner resolves it to PRINT the number an operator
// will compare against a `kubectl get svc`. Two `if n == 0` branches holding the
// same literal is how one of them comes to hold a different one — and the
// observable of that drift is a banner naming a port the cluster does not
// publish, which sends a reader to debug the Service.
//
// It does NOT range-check: a caller that wants the refusal wants it to name the
// field it read, and this function does not know which field that was. buildPorts
// and cmd/muster-server's loadConfig each refuse in their own words.
func ResolveGatewayPort(port int) int {
	if port == 0 {
		return DefaultGatewayPort
	}
	return port
}

// buildPorts declares the one port that makes an instance addressable.
//
// 🔴 THE NAME IS provision.DefaultPortName AND THAT IS THE WHOLE MECHANISM, NOT
// A LABEL. k8s's renderAnnotations writes the `muster.dev/port` annotation from
// Spec.PortNumber(provision.DefaultPortName), which Driver.Endpoint reads back
// and hands to provision.ResolveEndpoint. PortNumber falls back to "the single
// declared port when there is exactly one", so a differently-named sole port
// would resolve TODAY and silently stop resolving the moment a second port is
// added — a regression whose cause would be a port added somewhere else
// entirely. Naming it is what makes the resolution independent of how many
// ports this list grows to.
//
// ⚠ ONE PORT, NOT EVERY PORT THE IMAGE LISTENS ON. The runtime measured for
// DefaultGatewayPort also serves a skills API on a second port, and that port is
// deliberately absent: nothing in muster talks to it, and a Service port nothing
// consumes is reachable surface inside the cluster that no code here needs.
// Adding it is a decision for whoever writes the first consumer.
//
// Protocol is left empty rather than spelled "TCP": provision.Port documents
// empty AS TCP, and k8s's renderService/renderDeployment both default it that
// way, so writing it would be a second place for the same fact to be stated.
func buildPorts(cfg Config) ([]provision.Port, error) {
	port := ResolveGatewayPort(cfg.GatewayPort)
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("agentspec: Config.GatewayPort %d is not a port (1-65535); "+
			"leave it zero for the measured default (%d)", port, DefaultGatewayPort)
	}
	return []provision.Port{{Name: provision.DefaultPortName, Port: port}}, nil
}

// buildSecrets computes the sensitive environment. See Build's note on the split.
//
// 🔴 ONE TOKEN GOES OUT UNDER TWO NAMES, AND THEY MOVE TOGETHER OR THE CHAT TIER
// 401s. [EnvToken] is what the instance calls muster back with; [EnvGatewayToken]
// is what the instance's own gateway hashes into the bearer muster sends it. They
// are emitted from a single `if` deliberately: shipping the first without the
// second is the state this repository was in, and its observable was a 401 on
// every chat turn that reads exactly like a bad credential rather than like a
// variable nobody set. A caller cannot ask for one and not the other, which is
// the only way to make that drift unexpressible.
//
// ⚠ THE EMPTY-TOKEN BRANCH EMITS NEITHER, which is also deliberate.
// agentgateway's reach() refuses an agent with no token before it builds a
// request, naming the cause — whereas sha256("gw-" + "") is a perfectly
// well-formed 64-hex credential the runtime would reject 401. An empty secret
// placed under either name would convert a diagnosable refusal into that 401.
// ⚠ THE DERIVED BEARER ARRIVES AS AN ARGUMENT RATHER THAN BEING COMPUTED HERE,
// and the reason is the error. [RuntimeConfig.DeriveBearer] cannot be called on
// an empty token without producing a well-formed useless credential, so the call
// has to be able to REFUSE — and a buildSecrets that returned an error would make
// every caller handle one for the sake of a case Build already decided. Build
// derives it, Build refuses, and this function places what it is given. Empty
// means "no bundle configured", which the caller's own single predicate decided.
func buildSecrets(a agents.Agent, cfg Config, gatewayBearer string) []provision.EnvVar {
	var secrets []provision.EnvVar
	if a.HooksToken != "" {
		secrets = append(secrets,
			provision.EnvVar{Name: EnvToken, Value: a.HooksToken},
			provision.EnvVar{Name: EnvGatewayToken, Value: a.HooksToken},
		)
	}
	if gatewayBearer != "" {
		secrets = append(secrets, provision.EnvVar{Name: EnvGatewayBearer, Value: gatewayBearer})
	}
	if cfg.OpenRouterAPIKey != "" {
		secrets = append(secrets, provision.EnvVar{Name: EnvOpenRouterKey, Value: cfg.OpenRouterAPIKey})
	}
	return secrets
}

// buildFiles turns instruction text and seed content into data.
//
// Files are emitted in a DETERMINISTIC order — the instructions file first, then
// seeds sorted by name, then the cairn pair — because Options.SeedFiles is a map
// and Go randomises map iteration. Without the sort the golden would fail
// intermittently, which is the worst kind of gate: it trains people to re-run.
//
// The cairn pair goes LAST rather than interleaved with the seeds: neither is in
// the workspace, so sorting them among workspace-relative names would put an
// absolute path in the middle of a list a reader scans as "what is in the
// workspace".
func buildFiles(workspace string, cfg Config, opts Options) ([]provision.File, error) {
	var files []provision.File
	if opts.Instructions != "" {
		files = append(files, provision.File{
			Path:    path.Join(workspace, InstructionsFileName),
			Content: []byte(opts.Instructions),
		})
	}

	names := make([]string, 0, len(opts.SeedFiles))
	for name := range opts.SeedFiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if name == "" {
			return nil, fmt.Errorf("agentspec: Options.SeedFiles has an empty file name")
		}
		if strings.ContainsRune(name, '/') {
			return nil, fmt.Errorf("agentspec: Options.SeedFiles key %q contains a path separator; keys are names relative to the workspace root", name)
		}
		if name == InstructionsFileName && opts.Instructions != "" {
			return nil, fmt.Errorf("agentspec: Options.SeedFiles key %q collides with Options.Instructions, which is already written to that path", name)
		}
		files = append(files, provision.File{
			Path:    path.Join(workspace, name),
			Content: []byte(opts.SeedFiles[name]),
		})
	}

	// 🔴 THE TWO CAIRN FILES MOVE TOGETHER OR NOT AT ALL. The wrapper without the
	// credential is a command that refuses every store call; the credential
	// without the wrapper is a secret placed for nobody. One condition emits both,
	// which is what makes the pair unable to drift apart.
	if cairnEnabled(cfg, opts) {
		if err := checkCairnWorkspace(workspace); err != nil {
			return nil, err
		}
		files = append(files, cairnWrapperFile(workspace), cairnCredentialFile(cfg))
	}

	// 🔴 THE RUNTIME-CONFIG BUNDLE GOES LAST, AND ITS PATHS MUST NOT BE INSIDE
	// THE WORKSPACE. [RuntimeConfigDir]'s own note argues why; what this call site
	// adds is that a workspace-relative seed could COLLIDE with it if the two
	// shared a root, and provision.Spec.Validate refuses a duplicate file path —
	// so the collision would surface as a dispatch failure rather than as one file
	// silently winning. Keeping the bundle outside the workspace makes the
	// collision unreachable instead of merely detected.
	if cfg.RuntimeConfig.Configured() {
		files = append(files, runtimeConfigFiles(cfg)...)
	}
	return files, nil
}

// buildInit returns the imperative steps.
//
// ⚠ IT IS NO LONGER UNCONDITIONALLY EMPTY, AND THIS COMMENT USED TO SAY IT WAS.
// It read "🔴 IT IS EMPTY, DELIBERATELY … When the first real entry arrives, this
// comment is what tells the author why the list was empty rather than lost." The
// first real entry has arrived: the subsystem-store client install (cairn.go).
// The sentence is kept in this corrected form rather than deleted, because the
// argument it was making is the one that still governs what may be added.
//
// 🔴 THE BAR FOR AN ENTRY: IT MUST HAVE TO *RUN*, NOT HAVE TO *EXIST*. Upstream
// this was one joined string with seven contributors, and the reason it grew is
// that appending to it was always the cheapest option. Every contributor that was
// really "place this content" is now a [provision.File]. The cairn entry passes
// the bar for a reason worth stating: it FETCHES from a network at a pinned
// revision and then PROVES what it fetched imports, and neither of those is
// expressible as data — the client is deliberately not vendored here. The file it
// needs on PATH is still a [provision.File]; only the fetch is a step.
//
// Still not here, each for a reason that is a decision rather than an oversight:
//
//   - the in-pod CLI download needs muster's own /agent/muster route and a
//     verified artifact, which is plan step 22b's territory because it is the
//     first thing that requires the instance to reach a live server;
//   - submodule init and git credentials need the repository to have been cloned,
//     and [provision.Repo] is DECLARED, NOT CLONED by design — so whoever does
//     the cloning owns those steps.
//
// The order is fixed and the cairn entry is FIRST, ahead of the autosave
// invocation Build appends after this returns. That ordering is load-bearing in
// one direction only: the autosave entry BACKGROUNDS a daemon, so a step placed
// after it races the daemon's first snapshot, while a step placed before it
// cannot. Nothing here depends on cairn's output, and nothing should — an Init
// entry that needed a previous entry's side effect would be one step split in
// two.
func buildInit(workspace string, cfg Config, opts Options) ([]string, error) {
	var init []string
	if cairnEnabled(cfg, opts) {
		cmd, err := cairnInstallCommand(workspace)
		if err != nil {
			return nil, err
		}
		init = append(init, cmd)
	}
	return init, nil
}

// buildRepo declares the source repository. NOTHING HERE CLONES IT — see
// [provision.Repo], which explains that the upstream template cloned the
// repository itself roughly 250 lines before init commands ran, and why that is
// not reproduced.
//
// Path is derived from the repo NAME rather than from its owner/name pair,
// because the instance works in a directory, not in a namespace.
func buildRepo(a agents.Agent, workspace string) provision.Repo {
	if a.Repo == "" {
		return provision.Repo{}
	}
	return provision.Repo{
		URL:    a.Repo,
		Branch: a.RepoBranch,
		Path:   path.Join(workspace, repoDirName(a.Repo)),
	}
}

// repoDirName takes the last path element of an "owner/name" repo reference.
//
// ⚠ IT IS NOT A URL PARSER. It handles the "owner/name" shape the agent row
// carries, and for anything else it returns the input's last segment, which is
// the same thing `git clone` would name the directory. A trailing slash yields
// the segment before it rather than an empty string, because an empty directory
// name would join to the workspace root and silently make the repo path the
// workspace itself.
func repoDirName(repo string) string {
	trimmed := strings.TrimRight(repo, "/")
	if trimmed == "" {
		return ""
	}
	if i := strings.LastIndex(trimmed, "/"); i >= 0 {
		return trimmed[i+1:]
	}
	return trimmed
}

// resolvePrimaryModel picks the primary slug for ONE agent, delegating the
// agent-over-deployment precedence to [agents.ResolveModel].
//
// 🔴 THE DELEGATION IS THE POINT, NOT A STYLE CHOICE. Until this existed the
// agent row's Model was stored, served over the API and displayed, and then
// dropped on the floor at provision time: buildRuntimeConfig took only the
// deployment Config, so an operator who chose a model in the dispatch modal got
// the deployment-wide default with nothing anywhere saying so. ResolveModel had
// ZERO production callers and its own docstring argues against a second inlined
// copy of the three-way choice, so this routes through it rather than
// re-spelling `if a.Model != ""` here.
//
// ⚠ WHY THE BOTH-EMPTY CASE SHORT-CIRCUITS INSTEAD OF CALLING ResolveModel.
// The two packages end their precedence chains in DIFFERENT places and both are
// right for their own layer: agents.ResolveModel falls through to a built-in
// slug (it answers "which model does an agent run on"), while this package's
// contract is that unset at BOTH levels writes no key at all and lets the
// runtime decide (see Config.Model, MUSTER_AGENT_MODEL's own documentation, and
// TestNoModelMeansNoConfigKeyRatherThanAnEmptyOne). Calling ResolveModel
// unconditionally would silently start stamping a hardcoded slug into
// MUSTER_CONFIG for every deployment that sets no model — a behaviour change
// nobody asked for and the opposite of "the runtime decides". So: ResolveModel
// owns PRECEDENCE, this package owns EXPRESSIBILITY. Whenever either input is
// set, ResolveModel's built-in tier is unreachable and it reduces to exactly the
// precedence wanted here.
func resolvePrimaryModel(a agents.Agent, cfg Config) string {
	if a.Model == "" && cfg.Model == "" {
		return ""
	}
	return agents.ResolveModel(a.Model, cfg.Model)
}

// buildRuntimeConfig writes the opaque runtime config for ONE agent.
//
// It writes the model key ONLY when there is a model to name. An empty primary
// with a populated fallback list is not expressible upstream either, and writing
// {"primary": ""} would hand the runtime a value it has to special-case — which
// is how "unset" and "empty" stop being distinguishable, the exact defect
// plan §4.2 records against the original config.
//
// 🔴 A PER-AGENT PRIMARY DROPS THE DEPLOYMENT'S FALLBACK CHAIN. THIS IS A
// DECISION, NOT AN OVERSIGHT, AND IT IS STATED BECAUSE EITHER ANSWER IS A REAL
// BEHAVIOUR. Config.ModelFallbacks is deployment-wide and there is no per-agent
// fallbacks column, so KEEPING it would pair an agent-specific primary with a
// deployment-specific chain — a combination no operator ever authored and none
// can see. Worse, it defeats the only thing the per-agent knob is for: an
// operator names a specific model precisely because the others misbehave for
// this agent, so a chain that can route the turn to a DIFFERENT model silently
// reinstates the problem being worked around. A visible hard failure on the
// chosen model beats an invisible success on the wrong one.
//
// ⚠ OBSERVATIONALLY INERT TODAY, WHICH IS WHY IT IS CHEAP TO STATE NOW: nothing
// populates Config.ModelFallbacks (cmd/muster-server's agentSpecConfig maps
// Model but no fallbacks), so production builds carry no fallbacks key either
// way. The branch exists so that whenever a deployment-wide chain IS wired, it
// does not quietly acquire authority over an agent whose model was chosen by
// hand.
func buildRuntimeConfig(a agents.Agent, cfg Config) map[string]any {
	primary := resolvePrimaryModel(a, cfg)

	fallbacks := cfg.ModelFallbacks
	if a.Model != "" {
		fallbacks = nil
	}

	if primary == "" && len(fallbacks) == 0 {
		return nil
	}
	model := map[string]any{}
	if primary != "" {
		model["primary"] = primary
	}
	if len(fallbacks) > 0 {
		// Copied, not aliased: Spec.Config is handed to a driver and a caller
		// mutating its own slice afterwards must not change a built spec.
		copied := make([]string, len(fallbacks))
		copy(copied, fallbacks)
		model["fallbacks"] = copied
	}
	return map[string]any{ConfigKeyModel: model}
}

// buildLabels merges the installation's labels with this agent's.
//
// The per-agent labels are applied SECOND so they win a key collision, and the
// map is always freshly allocated so a caller's Config.Labels is never mutated
// by building a spec from it.
func buildLabels(a agents.Agent, cfg Config) map[string]string {
	labels := make(map[string]string, len(cfg.Labels)+2)
	for k, v := range cfg.Labels {
		labels[k] = v
	}
	labels["muster.agent/name"] = a.Name
	if a.Namespace != "" {
		labels["muster.agent/group"] = a.Namespace
	}
	return labels
}
