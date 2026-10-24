package main

import (
	"crypto/rsa"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/api"
	"github.com/ZacxDev/muster/internal/ccpool"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/githubapp"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/sendqueue"
	"github.com/ZacxDev/muster/internal/stepup"
	"github.com/ZacxDev/muster/internal/ui"
)

// defaultPort is the port this service listens on when MUSTER_PORT is unset.
//
// ⚠ IT IS NOT 8104. That is the permission router's port, and the two services
// are expected to run side by side on the same host during the extraction — a
// shared default would make "which process is answering" a question rather than
// a fact.
const defaultPort = 8105

// Environment variable names.
//
// 🔴 EVERY NAME IS SPELLED EXACTLY ONCE, HERE, AND THAT IS NOT TIDINESS. The
// finding this file answers is that internal/api and internal/ui document a
// dozen MUSTER_* variables in their comments and NOTHING READ ANY OF THEM —
// there was no binary to read them. A second spelling in a wiring call is how
// that state comes back one variable at a time: the comment says one name, the
// code reads another, and the operator sets the one the comment names.
//
// 🔴 THE CITATION THAT USED TO CLOSE THIS PARAGRAPH NAMED A GUARD THAT WAS
// NEVER WRITTEN. It read "envNamesAreDocumented asserts these constants against
// the documentation"; enumerated over the module, that identifier had exactly
// one occurrence — the sentence itself. It slipped
// TestEveryCitedTestExistsOrIsLedgered because that gate resolves `Test[A-Z]…`
// names only, and a lower-case one is invisible to it. Cite a guard by its
// `Test*` name, which is the spelling the gate can check; see the KNOWN LIMIT
// banner in internal/modulegate/citations_test.go.
//
// What actually holds this paragraph up is
// TestEveryServerEnvNameIsSpelledOnceAndRead (config_env_names_test.go): every
// environment-variable literal in this binary's sources is one of the constants
// below, and every constant below is read somewhere outside this block.
const (
	envPort     = "MUSTER_PORT"
	envDatabase = "DATABASE_URL"

	envUIPassword    = "MUSTER_UI_PASSWORD"
	envHookToken     = "MUSTER_HOOK_TOKEN"
	envServiceToken  = "MUSTER_SERVICE_TOKEN"
	envSecureCookies = "MUSTER_SECURE_COOKIES"

	// Accounts PR 4: the human tier's mode and muster's GoTrue. See
	// internal/api/accounts.go and validateAuthMode.
	envAuthMode     = "MUSTER_AUTH_MODE"
	envGoTrueURL    = "MUSTER_GOTRUE_URL"
	envGoTrueSecret = "MUSTER_GOTRUE_JWT_SECRET"
	envBrowserPort  = "MUSTER_BROWSER_PORT"

	envGitHubToken     = "MUSTER_GITHUB_TOKEN"
	envGitHubEncKey    = "MUSTER_GITHUB_ENCRYPTION_KEY"
	envPublicURL       = "MUSTER_PUBLIC_URL"
	envGitHubClientID  = "GITHUB_CLIENT_ID"
	envGitHubClientSec = "GITHUB_CLIENT_SECRET"

	// The GitHub App (internal/githubapp): muster's GitHub identity (for admins)
	// when set, taking precedence over the OAuth connection above (deprecated).
	// The ID and the key are a pair, and the account allowlist is REQUIRED with
	// them: the App is public, so anyone can install it.
	envGitHubAppID       = "MUSTER_GITHUB_APP_ID"
	envGitHubAppKey      = "MUSTER_GITHUB_APP_PRIVATE_KEY"
	envGitHubAppAccounts = "MUSTER_GITHUB_APP_ACCOUNTS"

	// How long a claude-code agent's first turn waits for its repository checkout
	// (agents.CheckoutWaitTimeout) before the agent is marked error.
	envCheckoutWaitTimeout = "MUSTER_CHECKOUT_WAIT_TIMEOUT"

	// The secure connector store (internal/connectors). The KEY turns it on; the
	// connect PORT is a second listener serving ONLY the agent-facing connector
	// surface (api.ConnectSurfacePrefixes), so the network allowance an isolated
	// agent is given reaches nothing else; the agent URL is what agents are told
	// (as MUSTER_CONNECT_URL) to reach that listener by.
	envConnectorEncKey = "MUSTER_CONNECTOR_ENCRYPTION_KEY"
	envConnectPort     = "MUSTER_CONNECT_PORT"
	envAgentConnectURL = "MUSTER_AGENT_CONNECT_URL"

	envFaroURL     = "MUSTER_FARO_URL"
	envFaroKey     = "MUSTER_FARO_API_KEY"
	envFaroTracing = "MUSTER_FARO_TRACING"

	// The operator's own hosts (ui.HostPreferences): the host tabs named
	// first, in order, and a host's icon by its name.
	envHostOrder = "MUSTER_HOST_ORDER"
	envHostIcons = "MUSTER_HOST_ICONS"

	envTagAutoDispatch = "MUSTER_TAG_AUTODISPATCH"
	envTaskReapAfter   = "MUSTER_TASK_REAP_AFTER"
	// The stopped-agent reaper (internal/api agent_reap.go): how long an agent
	// may stay stopped before it is destroyed. A Go duration, 336h when unset,
	// "0"/"off" turns the reaper off, refused below 1h otherwise.
	envAgentReapStoppedAfter = "MUSTER_AGENT_REAP_STOPPED_AFTER"
	// The send queue (internal/sendqueue): how long a target must have been idle
	// before a queued message is sent to it. A Go duration, 30s when unset,
	// refused outside 1s–1h.
	envSendQueueIdle = "MUSTER_SEND_QUEUE_IDLE"
	// Session focus (internal/focus): how often the server recomputes each
	// session's focus. A Go duration, 15m when unset, "0" turns the periodic
	// pass off (focus_refresh still works), refused outside 1m–24h otherwise.
	envFocusInterval = "MUSTER_FOCUS_INTERVAL"
	// How long a mirrored session's focus is kept after a pass last saw the
	// session on any host. A Go duration, 168h when unset, refused outside
	// 1h–2160h (90 days).
	envFocusRetention = "MUSTER_FOCUS_RETENTION"
	// The Agents list's Idle section (internal/ui/idle_section.go): how long an
	// entry must have had no activity to be drawn under the collapsed "Idle"
	// header. Days ("14d") or a Go duration, 14d when unset, "0"/"off" turns it
	// off, refused outside 1h–365d otherwise.
	envAgentsIdleAfter = "MUSTER_AGENTS_IDLE_AFTER"

	// The optional MIRRORED-sessions plugin (internal/mirror): "1" turns it on.
	// Off, GET /mirror/connect answers 404 and nothing mirrored is listed.
	envMirror = "MUSTER_MIRROR"
	// The optional PLUGIN interface (internal/plugins): "1" turns it on, with a
	// database. Off, every /plugin/* route and /plugins answer 404.
	envPlugins = "MUSTER_PLUGINS"
	// envPluginTaskNamespaces lists the tag namespaces POST /plugin/v1/tasks
	// accepts keys in (internal/api/plugin_tasks.go), comma-separated.
	envPluginTaskNamespaces = "MUSTER_PLUGIN_TASK_NAMESPACES"
	// SESSION ROUTING (internal/plugins/routes.go): "1" lets the chief hand a
	// plugin message to one of its owner's sessions (route_to_session) and relay
	// the session's answer. Needs the plugin interface on. Off, the routing
	// tools are not offered and POST /api/routes/{id}/reply answers 404.
	envSessionRouting = "MUSTER_SESSION_ROUTING"
	// The per-owner sender allowlist whose routed answers are relayed WITHOUT
	// the owner's approval: "<user id>:<plugin>:<address, *@domain, owner/handle
	// or owner/*>,…", each matched only on mail that plugin marked sender_verified. Unset: every
	// routed answer becomes a pending draft the owner approves.
	envRouteRelayAllow = "MUSTER_ROUTE_RELAY_ALLOW"
	// How long a routed message waits for its session's answer before it is
	// handed back to the chief (api.DefaultRouteTimeout, 30m, when unset). A Go
	// duration between api.MinRouteTimeout and api.MaxRouteTimeout, and — with
	// routing on — longer than MUSTER_SEND_QUEUE_IDLE; anything else refuses to
	// boot.
	envRouteTimeout = "MUSTER_ROUTE_TIMEOUT"
	// TASK ROUTING (internal/api/task_routing.go): "1" lets a plugin hand a task
	// it files (POST /plugin/v1/tasks, route_to_chief) to the owner's chief,
	// which routes it with task_route_attention / task_forward. Needs the plugin
	// interface on. Off, route_to_chief is ignored and the tools are not offered.
	envTaskRouting = "MUSTER_TASK_ROUTING"
	// The users (ids, comma-separated) who opted in to their chief starting
	// agents. Non-empty (with task routing on) is what offers the chief
	// task_dispatch, for the listed owners only; empty, it is not offered.
	envChiefDispatchOwners = "MUSTER_CHIEF_DISPATCH_OWNERS"
	// CHIEF SESSION RUNTIMES (internal/api/session_runtime.go): the runtimes a
	// chief chat session may be started on, comma-separated — claude-code (a CLI
	// of its own in ccd, with a chosen Claude model; needs a claude-code chief on
	// a ccd that runs one CLI per session) and gateway (an OpenRouter model, on
	// the gateway runtime — in a claude-code chief, the one beside ccd). Unset is
	// OFF: no picker, and a session is the chief's own kind as before.
	envChiefSessionRuntimes = "MUSTER_CHIEF_SESSION_RUNTIMES"
	// How long a sign-in stays fresh enough to TYPE into a mirrored session
	// (internal/stepup): a Go duration, 12h when unset, refused outside
	// 1m–24h. Freshness is per browser session, never per user.
	envStepUpWindow = "MUSTER_STEPUP_WINDOW"

	// Browser flows: the deployment's own infrastructure names (comma-separated)
	// that an AGENT's browser-flow text may not carry (internal/flows
	// AgentTextRefusal). Configuration, not source: this repository is public.
	envFlowAgentDenyWords = "MUSTER_FLOW_AGENT_DENY_WORDS"
	// The cairn memo relay (internal/memorelay): the memo listener's base URL
	// and the ONE read-only credential muster reads live memos with. The token
	// alone decides whether the relay runs — see config.validate.
	envMemoURL   = "MUSTER_MEMO_URL"
	envMemoToken = "MUSTER_MEMO_TOKEN"
	// The relay's repo → scope-set map (memorelay.ParseScopeMap's syntax,
	// `repo=scope,scope;…`): which cairn scopes an agent working on a
	// repository reads, when that is not just the repository's own name.
	envMemoScopeMap = "MUSTER_MEMO_SCOPE_MAP"
	// Dictation (internal/stt; internal/api/transcribe.go): the speech-to-text
	// service the message box's microphone is transcribed by, and the bearer
	// token muster presents to it. The token alone turns dictation on — see
	// config.validateSTT.
	envSTTURL   = "MUSTER_STT_URL"
	envSTTToken = "MUSTER_STT_TOKEN"
	// File attachments (internal/uploads; internal/api/uploads.go): the
	// S3-compatible store the composer's files are kept in, the key the agent's
	// links are signed with, and the two knobs. All five of endpoint, bucket,
	// both keys and the signing key (plus MUSTER_PUBLIC_URL and a database)
	// turn it on; a partial set leaves it OFF with a MISCONFIGURED banner line —
	// it never refuses to boot. See buildUploads.
	envS3Endpoint       = "MUSTER_S3_ENDPOINT"
	envS3Bucket         = "MUSTER_S3_BUCKET"
	envS3AccessKey      = "MUSTER_S3_ACCESS_KEY"
	envS3SecretKey      = "MUSTER_S3_SECRET_KEY"
	envS3Region         = "MUSTER_S3_REGION"
	envUploadSigningKey = "MUSTER_UPLOAD_SIGNING_KEY"
	envUploadMaxBytes   = "MUSTER_UPLOAD_MAX_BYTES"
	envUploadLinkTTL    = "MUSTER_UPLOAD_LINK_TTL"

	envRouterURL   = "MUSTER_ROUTER_URL"
	envRouterToken = "MUSTER_ROUTER_TOKEN"
	envRouterActor = "MUSTER_ROUTER_ACTOR"

	envStandalone = "MUSTER_STANDALONE"

	// The approval relay's OWN gate (internal/api/approvals.go): the permission
	// router's /api/gate door, reached for ONE purpose, allowed beside
	// MUSTER_STANDALONE. It turns on no router behaviour. See config.ApprovalGateURL.
	envApprovalGateURL   = "MUSTER_APPROVAL_GATE_URL"
	envApprovalGateToken = "MUSTER_APPROVAL_GATE_TOKEN"

	// muster-native Web Push (internal/push): muster's OWN VAPID identity, a
	// triple that is all-or-nothing (validate). Unset: no push — the Enable
	// button is not drawn and /api/push/vapid-public-key answers 503. Mint one
	// with `muster-server vapid generate`.
	envVAPIDPublicKey  = "MUSTER_VAPID_PUBLIC_KEY"
	envVAPIDPrivateKey = "MUSTER_VAPID_PRIVATE_KEY"
	envVAPIDSubject    = "MUSTER_VAPID_SUBJECT"
	// The operator's own push hosts (exact names, https, default port), added to
	// the browser push services an endpoint may be on — a self-hosted ntfy for
	// the Android app (internal/push.Allowlist). Empty: browser hosts only.
	envPushExtraHosts = "MUSTER_PUSH_EXTRA_HOSTS"
	// The Android app's PIN-key policy (docs/design/android.md, "The step-up
	// key"): set, a phone with no strong biometric may pair with a
	// biometric-or-PIN step-up key; unset, it pairs read-only.
	envAndroidAllowPINKeys = "MUSTER_ANDROID_ALLOW_PIN_KEYS"

	// --- agent provisioning (see provisioner.go) ---
	envAgentProvisioner  = "MUSTER_AGENT_PROVISIONER"
	envAgentGateway      = "MUSTER_AGENT_GATEWAY"
	envAgentGatewayModel = "MUSTER_AGENT_GATEWAY_MODEL"
	envAgentGatewayPort  = "MUSTER_AGENT_GATEWAY_PORT"
	// The agent runtime's OWN configuration, operator-supplied. Both are the
	// CONTENT of a key in a ConfigMap the operator creates, projected into this
	// pod with valueFrom.configMapKeyRef — see config.AgentRuntimeConfig.
	envAgentRuntimeConfig  = "MUSTER_AGENT_RUNTIME_CONFIG"
	envAgentRuntimeInstall = "MUSTER_AGENT_RUNTIME_INSTALL"
	envAgentImageRepo      = "MUSTER_AGENT_IMAGE_REPO"
	envAgentImageTag       = "MUSTER_AGENT_IMAGE_TAG"
	envAgentAPIURL         = "MUSTER_AGENT_API_URL"
	envAgentModel          = "MUSTER_AGENT_MODEL"
	envAgentOpenRouterKey  = "MUSTER_AGENT_OPENROUTER_KEY"
	envAgentNamespace      = "MUSTER_AGENT_NAMESPACE"
	envAgentNSPrefix       = "MUSTER_AGENT_NAMESPACE_PREFIX"
	envAgentNSShared       = "MUSTER_AGENT_NAMESPACE_SHARED"
	envAgentWorkspaceKeep  = "MUSTER_AGENT_WORKSPACE_PERSIST"
	envAgentStorageClass   = "MUSTER_AGENT_WORKSPACE_STORAGE_CLASS"
	envAgentEndpointTmpl   = "MUSTER_AGENT_ENDPOINT_TEMPLATE"

	// The hosted subsystem-store ("cairn") coordinates handed to the SUPERVISOR
	// agent only. Both unset is the state this ships in, and in that state nothing
	// cairn-related reaches any instance — see internal/agentspec/cairn.go.
	//
	// ⚠ THE PREFIX IS MUSTER_AGENT_, NOT MUSTER_. They are parameters of the agent
	// SPEC, like every other name in this block, and a bare MUSTER_CAIRN_* pair
	// would read as a property of this server — which reads nothing from the store
	// and never contacts it.
	envAgentCairnURL   = "MUSTER_AGENT_CAIRN_URL"
	envAgentCairnToken = "MUSTER_AGENT_CAIRN_TOKEN"

	// --- agent KINDS (internal/agents.Kinds) and the claude-code kind's profile ---
	//
	// MUSTER_AGENT_KINDS is a comma list of the kinds a dispatch may ask for; unset
	// means "gateway" alone, which is every deployment before kinds existed. The
	// claude-code kind's Claude accounts are NAMED by MUSTER_AGENT_CC_ACCOUNTS
	// (a comma list of [a-z0-9-] names) and each account's `claude setup-token` is
	// read from MUSTER_AGENT_CC_TOKEN_<NAME> — the name upper-cased with '-' as
	// '_' (ccpool.EnvSuffix): account `work-2` -> MUSTER_AGENT_CC_TOKEN_WORK_2.
	// 🔴 THE LIST IS EXPLICIT ON PURPOSE: a token variable no list names is not an
	// account, so a stray variable cannot silently join the pool, and a listed
	// name with no token is a boot refusal naming the variable.
	envAgentKinds         = "MUSTER_AGENT_KINDS"
	envAgentCCImage       = "MUSTER_AGENT_CC_IMAGE"
	envAgentCCAccounts    = "MUSTER_AGENT_CC_ACCOUNTS"
	envAgentCCTokenPrefix = "MUSTER_AGENT_CC_TOKEN_"
	envAgentCCStorage     = "MUSTER_AGENT_CC_STORAGE_SIZE"

	// The account pool's two limits (internal/ccpool.Limits): how long an
	// account rests after a `rate_limited` whose message states no readable reset
	// time, and how many jobs one account may run at once. Unset cooldown is
	// ccpool.DefaultCooldown; unset cap is NO cap (the cap is opt-in).
	envAgentCCCooldown = "MUSTER_AGENT_CC_RATE_LIMIT_COOLDOWN"
	envAgentCCMaxJobs  = "MUSTER_AGENT_CC_MAX_CONCURRENT_JOBS"

	// envAgentCCBypassPools is a comma list of worker-pool names whose
	// claude-code agents run their JOB lanes with permissions BYPASSED
	// (agentspec.ClaudeCodeConfig.BypassPools -> CCD_JOB_PERMISSION_MODE in the
	// pod, cmd/ccd/jobmode.go). Unset is none. 🔴 A listed pool's job lanes run
	// any command on the code they are handed, without approval, and can read
	// that worker's own Claude setup-token: for pools reviewing trusted members'
	// PRs only. Names are lower-cased as read (splitList; pool names are
	// lower-case only). Refused at boot without the claude-code kind, and for a
	// name agents.ValidWorkerPool refuses.
	envAgentCCBypassPools = "MUSTER_AGENT_CC_BYPASS_POOLS"

	// RETIRED: the removed `hermes` agent kind's image and /data volume size.
	// 🔴 READ ONLY TO BE NAMED IN A BOOT WARNING, NEVER USED. A deployment that
	// still sets them (or still lists `hermes` in MUSTER_AGENT_KINDS) boots, and
	// logBanner says loudly that they are ignored (config.RetiredKindSettings),
	// rather than crash-looping on its first rollout of this build.
	envAgentHermesImage   = "MUSTER_AGENT_HERMES_IMAGE"
	envAgentHermesStorage = "MUSTER_AGENT_HERMES_STORAGE_SIZE"

	// --- the claude-code kind's NetworkPolicy (kubernetes driver only) ---
	//
	// A claude-code agent's NetworkPolicy admits connections from MUSTER ITSELF
	// and from nothing else, and these two say which pods that is: the namespace
	// this server runs in, and a comma list of `key=value` labels its pods carry.
	//
	// 🔴 THERE IS NO DEFAULT FOR EITHER, AND A WRONG ONE DOES NOT FAIL OPEN. They
	// are facts about how THIS deployment is labelled, which is installation
	// state. A value that matches nothing produces a policy muster's own pod is
	// not admitted by: the agent's pod reports Ready (a kubelet probe is not a
	// pod-to-pod connection) and every turn times out. Read both off the muster
	// Deployment's pod template.
	envAgentNetpolFromNS     = "MUSTER_AGENT_NETPOL_FROM_NAMESPACE"
	envAgentNetpolFromLabels = "MUSTER_AGENT_NETPOL_FROM_POD_LABELS"

	// --- browser-test fixture (read ONLY by a `-tags e2eterm` build) ---
	//
	// A directory gating the e2e fake driver's readiness: with it set, every
	// running instance reports not-ready until a file named after the instance
	// exists in it. The shipped binary has no reader of it; see e2eterm.go.
	envE2EHoldReadyDir = "MUSTER_E2E_HOLD_READY_DIR"
	// The session window ("CxR") the e2e fake pod reports to muster's
	// window-size query (internal/termsize's no-shrink rule); unset, the query
	// fails as on a pod with no session. Read only by e2eterm.go.
	envE2ETermWindow = "MUSTER_E2E_TERM_WINDOW"
	// A fake GitHub API base and a fake OpenRouter model list, so the dispatch
	// dialog's repo and model pickers can be driven with fixture data. Read only
	// by e2eupstreams.go.
	envE2EGitHubAPI        = "MUSTER_E2E_GITHUB_API"
	envE2EOpenRouterModels = "MUSTER_E2E_OPENROUTER_MODELS"
	// The GoTrue verifier's clock skew (a Go duration) instead of
	// gotrue.DefaultSkew, so the specs that outlive an access token wait
	// seconds rather than 35 s. Read only by e2egotrue.go.
	envE2EGoTrueSkew = "MUSTER_E2E_GOTRUE_SKEW"
	// A loopback base URL every agent's gateway endpoint resolves to, so a
	// browser test can answer an agent's turn with a fake runtime (the answer
	// delivery's spec, attention-delivery.spec.ts). Read only by e2eterm.go.
	envE2EAgentEndpoint = "MUSTER_E2E_AGENT_ENDPOINT"
)

// The values MUSTER_AGENT_PROVISIONER accepts.
//
// 🔴 THE DEFAULT IS none, AND IT HAS TO BE. Every previous revision of this
// binary had NO provisioner at all, so a default that provisioned would change
// what an unchanged deployment does on the next image bump — and the thing it
// would change is "create pods in a cluster". Opting in is a deliberate act.
const (
	provisionerNone = "none"
	provisionerNoop = "noop"
	provisionerK8s  = "kubernetes"
)

// provisionerChoices is every legal value, for the refusal message and for the
// guard that pins the set.
var provisionerChoices = []string{provisionerNone, provisionerNoop, provisionerK8s}

// The values MUSTER_AGENT_GATEWAY accepts: which agent RUNTIME's chat wire this
// deployment talks. See internal/agentgateway.Runtime for what the name selects.
//
// 🔴 THE DEFAULT IS none FOR THE SAME REASON THE PROVISIONER'S IS, AND ONE MORE.
// The provisioner's argument is that an image bump must not start creating pods;
// chat creates nothing, but it does send an agent a message and pay for a model
// turn. More importantly the runtime name is a CLAIM about what is inside the
// image, and a default would make that claim on an operator's behalf — for an
// image they chose, whose gateway may authenticate by a different formula
// entirely.
const (
	gatewayNone = "none"
	// The scheme names are the agentgateway package's own, not copies: a second
	// spelling of a value an operator sets is how configuration stops matching the
	// code that checks it.
	gatewayHooksSHA256 = agentgateway.SchemeHooksSHA256
)

// gatewayChoices is every legal value, for the refusal message and for the guard
// that pins the set.
var gatewayChoices = []string{gatewayNone, gatewayHooksSHA256}

// agentGateway resolves the empty value to gatewayNone, for the reason
// agentProvisioner's doc gives at length: buildApp accepts a config that did not
// come from loadConfig, and every wiring test is one.
func (c config) agentGateway() string {
	if c.AgentGateway == "" {
		return gatewayNone
	}
	return c.AgentGateway
}

// agentProvisioner resolves the empty value to provisionerNone.
//
// 🔴 IT EXISTS BECAUSE buildApp ACCEPTS A config THAT DID NOT COME FROM
// loadConfig, AND EVERY WIRING TEST IS ONE. loadConfig normalises "" to
// provisionerNone; a config value constructed directly does not, and the first
// draft of the wiring compared the raw field against provisionerNone — so a
// zero-value config never reached the "none" branch at all.
//
// FIVE pre-existing boot tests went red on it, on TWO different branches, and
// that is the only reason it was found: TestReadyzRefusesAnUndeclaredMissingRouter
// and TestReadyzAcceptsADeclaredStandaloneDeployment fell through to the
// "unhandled provisioner name" refusal in buildDriver, while
// TestTheServerListensAndServesHealth,
// TestTheServerServesTheDocumentRootThroughTheUILayer and
// TestTheServerServesTheEmbeddedStylesheet hit the nil-agents-store refusal
// instead, because they configure no database. ⚠ THE COUNT IN THIS COMMENT READ
// "THREE" AND WAS MEASURED WRONG — it was five, and naming only one branch would
// have sent the next reader looking for a single cause. Nothing about a zero-value
// struct announces which of its fields were meant to be defaulted elsewhere.
// missingRouterCredential's own doc records the same property of buildApp for the
// router actor.
func (c config) agentProvisioner() string {
	if c.AgentProvisioner == "" {
		return provisionerNone
	}
	return c.AgentProvisioner
}

// agentGatewayPort is the port an instance's gateway will be reached on, with
// the unset case resolved.
//
// ⚠ IT DELEGATES RATHER THAN BRANCHING, and the delegation is the point: the
// number is agentspec's, with a measurement beside it, and this binary must not
// become a second authority on it. The only reason this method exists is that
// the boot banner PRINTS the resolved value — a banner naming the raw field
// would print `0` on every deployment, which is not a port.
func (c config) agentGatewayPort() int {
	return agentspec.ResolveGatewayPort(c.AgentGatewayPort)
}

// routerActorLabel and routerReservedActor are the permission router's rule for
// the actor name a service-tier caller asserts (a DNS-label-like shape, and one
// reserved backfill sentinel). The router refuses anything else with 403, so
// validate checks RouterActor against it when the approval gate is configured.
// A copy of a rule another service owns: if the router widens it, this only
// refuses a value that would have worked; if it narrows it, prompts 403 again.
var routerActorLabel = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

const routerReservedActor = "pre-0035-unattributed"

// defaultRouterActor is the name this service asserts to the permission router
// when MUSTER_ROUTER_ACTOR is unset. The router validates it as an actor label.
const defaultRouterActor = "muster"

// config is everything this binary reads from the environment, resolved once.
//
// 🔴 IT IS A VALUE, NOT A PILE OF os.Getenv CALLS SCATTERED THROUGH main(), SO
// THE WIRING IS TESTABLE WITHOUT AN ENVIRONMENT. buildApp takes one of these;
// every test below constructs one directly. A wiring bug found by a test that
// had to mutate the process environment is a test that cannot run in parallel
// with any other.
type config struct {
	Port     int
	Database string

	UIPassword    string
	HookToken     string
	ServiceToken  string
	SecureCookies bool

	// AuthMode is MUSTER_AUTH_MODE: "password" (the default) or "gotrue".
	AuthMode string
	// GoTrueURL and GoTrueSecret reach muster's GoTrue. Set in either mode:
	// in password mode they arm only the admin claim, so the operator's login
	// can be bound before the mode is flipped.
	GoTrueURL    string
	GoTrueSecret string
	// BrowserPort, when non-zero, binds the BROWSER listener: the only one on
	// which gotrue mode serves a session (api.BrowserListener). The main port
	// then serves machine tokens only. The reverse holds too: in gotrue mode the
	// browser listener refuses the SHARED hook/service tokens (403) and admits
	// only personal mst_ tokens on the /api/ tier (refuseSharedTokenOnBrowserListener).
	BrowserPort int

	GitHubToken        string
	GitHubEncKey       []byte
	PublicURL          string
	GitHubClientID     string
	GitHubClientSecret string

	// GitHubAppID / GitHubAppKey configure the GitHub App (both or neither;
	// loadConfig refuses half, and a key that does not parse).
	// GitHubAppAccounts is the REQUIRED allowlist of installation accounts.
	GitHubAppID       int64
	GitHubAppKey      *rsa.PrivateKey
	GitHubAppAccounts []githubapp.Account

	// CheckoutWaitTimeout is agents.CheckoutWaitTimeout for this server: 0 means
	// the default (agents.DefaultCheckoutWaitTimeout).
	CheckoutWaitTimeout time.Duration

	// ConnectorEncKey is the 32-byte AES key connector secrets are sealed with.
	// Empty means the connector store is not built (the feature is off).
	ConnectorEncKey []byte
	// ConnectPort, when non-zero, binds a second listener serving only the
	// agent-facing connector surface.
	ConnectPort int
	// AgentConnectURL is handed to every agent as MUSTER_CONNECT_URL, and is
	// what makes a claude-code agent's network policy allow ConnectPort to muster.
	AgentConnectURL string

	FaroURL     string
	FaroKey     string
	FaroTracing bool

	// HostPrefs is MUSTER_HOST_ORDER and MUSTER_HOST_ICONS, checked
	// (ui.HostPreferences.Normalized); the zero value names no host.
	HostPrefs ui.HostPreferences

	TagAutoDispatch bool
	TaskReapAfter   time.Duration
	// AgentReapStoppedAfter is MUSTER_AGENT_REAP_STOPPED_AFTER
	// (api.DefaultAgentReapStoppedAfter when unset; 0 = the reaper is off).
	AgentReapStoppedAfter time.Duration
	// SendQueueIdle is MUSTER_SEND_QUEUE_IDLE (sendqueue.DefaultIdleFor when
	// unset).
	SendQueueIdle time.Duration
	// FocusInterval is MUSTER_FOCUS_INTERVAL (focus.DefaultInterval when
	// unset; 0 = no periodic pass).
	FocusInterval time.Duration
	// FocusRetention is MUSTER_FOCUS_RETENTION (focus.DefaultRetention unset).
	FocusRetention time.Duration
	// AgentsIdleAfter is MUSTER_AGENTS_IDLE_AFTER (ui.DefaultIdleSectionAfter
	// when unset; 0 = no Idle section). AgentsIdleAfterRaw is the value as
	// set ("" unset), for the banner.
	AgentsIdleAfter    time.Duration
	AgentsIdleAfterRaw string

	// Mirror turns the mirrored-sessions plugin on (MUSTER_MIRROR=1).
	Mirror bool
	// Plugins turns the plugin interface on (MUSTER_PLUGINS=1).
	Plugins bool
	// PluginTaskNamespaces is MUSTER_PLUGIN_TASK_NAMESPACES, split on commas.
	PluginTaskNamespaces []string
	// SessionRouting is MUSTER_SESSION_ROUTING=1; RouteRelayAllow is
	// MUSTER_ROUTE_RELAY_ALLOW, parsed (validate refuses one that does not parse).
	SessionRouting     bool
	RouteRelayAllowRaw string
	RouteRelayAllow    plugins.RelayAllowlist
	// RouteTimeout is MUSTER_ROUTE_TIMEOUT (api.DefaultRouteTimeout when unset):
	// a route's deadline.
	RouteTimeout time.Duration
	// TaskRouting is MUSTER_TASK_ROUTING=1; ChiefDispatchOwners
	// MUSTER_CHIEF_DISPATCH_OWNERS, parsed (loadConfig refuses one that does not
	// parse) — non-empty turns task_dispatch on.
	TaskRouting            bool
	ChiefDispatchOwnersRaw string
	ChiefDispatchOwners    map[int64]bool
	// ChiefSessionRuntimes is MUSTER_CHIEF_SESSION_RUNTIMES, split on commas
	// (validateSessionRuntimes refuses an unknown or repeated entry).
	ChiefSessionRuntimes []string
	// StepUpWindow is internal/stepup's freshness window (MUSTER_STEPUP_WINDOW;
	// stepup.DefaultWindow when unset). loadConfig refuses an out-of-bounds one.
	StepUpWindow time.Duration

	// FlowAgentDenyWords is MUSTER_FLOW_AGENT_DENY_WORDS, split on commas.
	FlowAgentDenyWords []string
	// MemoURL and MemoToken are the cairn memo listener the relay reads
	// (internal/memorelay). The relay runs iff MemoToken is set; MemoURL alone
	// is the state a deployment sits in before its credential is issued.
	MemoURL   string
	MemoToken string
	// MemoScopeMap is MUSTER_MEMO_SCOPE_MAP as written; validate parses it.
	MemoScopeMap string
	// STTURL and STTToken are the speech-to-text service behind the message
	// box's microphone. Dictation is on iff STTToken is set; STTURL alone is a
	// valid, inert state (the deployment names the service before its secret
	// exists), and a token with no URL refuses to boot.
	STTURL   string
	STTToken string
	// Uploads: the raw MUSTER_S3_* / MUSTER_UPLOAD_* values. buildUploads
	// parses and validates them; nothing here refuses to boot.
	S3Endpoint       string
	S3Bucket         string
	S3AccessKey      string
	S3SecretKey      string
	S3Region         string
	UploadSigningKey string
	UploadMaxBytes   string
	UploadLinkTTL    string

	RouterURL   string
	RouterToken string
	RouterActor string

	// Standalone is the operator's EXPLICIT declaration that this deployment has
	// no permission router behind it, and therefore no session transcripts.
	//
	// 🔴 IT EXISTS BECAUSE "NO ROUTER CONFIGURED" AND "FORGOT TO CONFIGURE THE
	// ROUTER" ARE THE SAME OBSERVABLE, AND THEY MUST NOT GET THE SAME
	// BEHAVIOUR. api.Extensions.defects treats a notes store with no session
	// liveness probe as a READINESS DEFECT precisely because the degraded
	// rendering ("no transcript recorded") is indistinguishable from the truth —
	// it is a page that states a falsehood, silently, on every row. This flag is
	// how an operator says the sentence is TRUE here: there is no session store,
	// so no transcript is recorded, and the page is correct.
	//
	// ⚠ SETTING IT DOES NOT MAKE THE SERVER STANDALONE. It records a claim about
	// the deployment. If a router IS configured, the router's probe wins and this
	// flag changes nothing — see buildSessionLiveness.
	Standalone bool

	// ApprovalGateURL / ApprovalGateToken point the claude-code approval relay at
	// the permission router's gate door (POST/GET/DELETE /api/gate) and NOTHING
	// else. They are a both-or-neither pair (validate refuses half of one).
	//
	// 🔴 THEY DO NOT MAKE THIS A ROUTER DEPLOYMENT. They are legal beside
	// Standalone, they never reach buildSessionLiveness or api.Server.UseRouter, and
	// the client built from them is handed to the server ONLY as the relay's
	// api.GatePort. They are the relay's ONLY gate: when unset the relay answers
	// 503 and every prompt falls back to the agent's terminal, whether or not a
	// permission router is configured. The actor asserted is RouterActor
	// (default "muster"), checked by validate against the router's label rule.
	ApprovalGateURL   string
	ApprovalGateToken string

	// VAPIDPublicKey / VAPIDPrivateKey / VAPIDSubject are muster's own Web Push
	// identity (internal/push). ALL OR NONE: validate refuses one or two of
	// them, and refuses three that are not a working identity (the private key
	// must derive the public one; the subject must be mailto: or https).
	VAPIDPublicKey  string
	VAPIDPrivateKey string
	VAPIDSubject    string
	// PushExtraHosts is MUSTER_PUSH_EXTRA_HOSTS as read; validate parses it
	// (push.ParseExtraHosts) and pushExtraHosts returns the clean list.
	PushExtraHosts string
	// AndroidAllowPINKeys is MUSTER_ANDROID_ALLOW_PIN_KEYS.
	AndroidAllowPINKeys bool

	// AgentProvisioner names the provisioning backend: one of
	// provisionerChoices. Default provisionerNone, which leaves
	// api.Extensions.Provisioner nil and every lifecycle route refusing exactly
	// as it did before this knob existed.
	AgentProvisioner string

	// AgentGateway names the agent runtime whose chat wire this deployment talks:
	// one of gatewayChoices. Default gatewayNone, which leaves
	// api.Extensions.Gateway nil and both chat routes refusing.
	//
	// 🔴 IT IS A SECOND KNOB RATHER THAN A BOOLEAN ON THE FIRST, BECAUSE THE TWO
	// ANSWER DIFFERENT QUESTIONS. AgentProvisioner says where instances live;
	// this says what protocol the process inside one speaks. A deployment can
	// legitimately want lifecycle without chat — and did, for the whole of the
	// step that wired the provisioner.
	AgentGateway string

	// AgentGatewayModel is the agent runtime's passthrough sentinel — the value its
	// gateway requires in the wire's `model` field. Required whenever AgentGateway
	// is not none, and refused at boot rather than at the first turn.
	//
	// 🔴 IT IS CONFIGURATION AND NOT A CONSTANT BECAUSE THE VALUE BELONGS TO THE
	// IMAGE. It is not the model an agent runs — the runtime selects that itself
	// — and there is no value this project could default it to that would be
	// right for an image it has never seen.
	AgentGatewayModel string

	// AgentGatewayPort is the port the agent runtime's gateway listens on. Zero
	// means agentspec.DefaultGatewayPort, and zero is what every deployment runs
	// with.
	//
	// 🔴 IT IS A PARAMETER OF THE SPEC, NOT OF THE CHAT TIER, DESPITE THE NAME.
	// agentspec.Build declares the port UNCONDITIONALLY — see its constant's own
	// note — because the port reaches the cluster at CREATE time, in the Service
	// and the Deployment annotation the driver resolves an address from (see
	// k8s.AnnotationPort). Gating
	// it on AgentGateway would leave every instance provisioned while chat was off
	// permanently addressless, and turning chat on later would silently require
	// re-provisioning each one. It is spelled MUSTER_AGENT_GATEWAY_PORT anyway
	// because the gateway is what the port is FOR and that is the word an operator
	// will search for.
	//
	// ⚠ ZERO IS THE UNSET SENTINEL AND NOT A SECOND DEFAULT. loadConfig refuses a
	// 0 that was WRITTEN, so this binary never has to decide what the number
	// should be — agentspec owns it, with the measurement beside it.
	AgentGatewayPort int

	// AgentRuntimeConfig and AgentRuntimeInstall are the agent runtime's own
	// configuration bundle: the configuration FILE the runtime reads, and the
	// script that installs it. Both are CONTENT, not names, and both are
	// REQUIRED whenever AgentGateway is not none — refused at boot, not at the
	// first dispatch.
	//
	// 🔴 THEY ARE CONFIGURATION AND NOT CONSTANTS FOR THE REASON
	// AgentGatewayModel IS, ONE STEP FURTHER. The sentinel "belongs to the
	// IMAGE"; so does a configuration file's SCHEMA, and so do the executable
	// names an install script has to re-exec. This module is public and the
	// runtime's identifier is a denied token, so carrying either would be a leak
	// as well as a coupling. internal/agentspec/runtimeconfig.go argues the
	// design at length and records the measurements behind it.
	//
	// 🔴 WHY THEY HOLD CONTENT RATHER THAN A ConfigMap NAME, WHICH IS A
	// DELIBERATE DEPARTURE FROM THE SHAPE THIS WAS ASKED FOR. A name would make
	// muster READ the object from the apiserver, and that costs three things the
	// content form does not: a `get configmaps` permission in muster's own
	// namespace — on a deployment whose single biggest out-of-repo blocker is
	// already RBAC (see provisioner.go prerequisite 1) — a resolution of which
	// namespace muster itself runs in, which nothing here knows today; and an
	// asymmetry between the two drivers, because the `noop` driver exists
	// precisely so the tiers above the provisioner can be developed with NO
	// cluster, and it would then either need in-cluster credentials or silently
	// build a different spec from the `kubernetes` one. With the content
	// projected by valueFrom.configMapKeyRef the operator still edits ONE
	// ConfigMap and `kubectl rollout restart` still applies it, the refusal below
	// is a pure function of the environment, every wiring test can construct the
	// configured state directly, and the spec is identical under both drivers.
	//
	// ⚠ WHAT IT COSTS, SO THE TRADE IS ON THE RECORD: the operator edits TWO
	// manifests rather than one — the ConfigMap and this Deployment's env — and a
	// key renamed in the ConfigMap without the env being updated fails the POD
	// rather than this refusal, as a CreateContainerConfigError naming the missing
	// key. That failure is loud and names the key, which is why it was accepted.
	//
	// ⚠ A ConfigMap IS NOT A SECRET STORE AND NEITHER OF THESE IS CONFIDENTIAL.
	// The credential the installed configuration ends up holding is NOT in here:
	// it is derived per agent from the row's token and travels to the instance as
	// agentspec.EnvGatewayBearer, in the spec's Secrets. Putting a credential in
	// this bundle would put it in a ConfigMap and in every instance's non-secret
	// file store.
	AgentRuntimeConfig  string
	AgentRuntimeInstall string

	// AgentImageRepo and AgentAPIURL are agentspec.Config's two required fields.
	// Required whenever AgentProvisioner is not none, and refused at boot rather
	// than at the first dispatch — see validate.
	AgentImageRepo string
	AgentAPIURL    string

	AgentImageTag      string
	AgentModel         string
	AgentOpenRouterKey string

	// AgentCairnURL and AgentCairnToken are the hosted subsystem-store
	// coordinates, reaching agentspec.Config as a BOTH-OR-NEITHER pair.
	//
	// 🔴 THE TOKEN IS A READ+WRITE CREDENTIAL ACROSS EVERY SCOPE OF THAT STORE, and
	// it is handed to ONE agent — the supervisor — by internal/agentprovision, not
	// to the fleet. Unset is the default and in that state no instance gets the
	// client, the credential or the prose that describes them. There is deliberately
	// no default URL: a built-in would collapse the gate to "is the token set".
	AgentCairnURL   string
	AgentCairnToken string

	// AgentNamespaceShared puts every instance in AgentNamespace instead of
	// giving each its own.
	//
	// 🔴 IT IS PHRASED AS THE NON-DEFAULT, WHICH IS THE OPPOSITE OF THE DRIVER'S
	// OWN FLAG, AND THAT IS DELIBERATE. k8s.Config.NamespacePerInstance defaults
	// FALSE, so a driver built from a zero config quietly shares one namespace —
	// while the row this service writes says `devpod-<name>`, a namespace that
	// would then hold nothing. Defaulting the deployment to per-instance keeps
	// the row and the cluster agreeing; an operator who wants the shared layout
	// says so and supplies the namespace.
	AgentNamespaceShared bool
	AgentNamespace       string
	AgentNamespacePrefix string

	// AgentWorkspacePersist asks for a workspace that survives a restart.
	//
	// ⚠ IT IS ALSO WHAT DECIDES WHETHER AgentStorageClass IS SENT AT ALL, because
	// the driver's WorkspaceStorageClass is a *string whose three states are nil
	// (refuse persistence), "" (the cluster's default class) and a name — and an
	// environment variable cannot tell unset from empty. So: persist off -> nil;
	// persist on -> a pointer to whatever the class variable holds, empty
	// included.
	AgentWorkspacePersist bool
	AgentStorageClass     string

	AgentEndpointTemplate string

	// AgentKinds is MUSTER_AGENT_KINDS as written (lower-cased, trimmed, in
	// order). Empty means agents.KindGateway alone — see config.agentKinds.
	AgentKinds []string
	// AgentCCImage is the claude-code kind's FULL image reference (tag or digest
	// required). There is no default: an image reference names a registry, and
	// that is installation state, the argument AgentImageRepo already makes.
	AgentCCImage string
	// AgentCCAccountNames is MUSTER_AGENT_CC_ACCOUNTS as written, in order, and
	// AgentCCTokens maps each name to its MUSTER_AGENT_CC_TOKEN_<NAME> value ("" when
	// that variable is unset — validateKinds refuses it).
	//
	// 🔴 AgentCCTokens HOLDS SUBSCRIPTION CREDENTIALS. Nothing may format this
	// struct with %v, and the banner prints names only.
	AgentCCAccountNames []string
	AgentCCTokens       map[string]string
	// AgentCCStorage is the claude-code kind's /data volume size ("" = 10Gi).
	AgentCCStorage string
	// AgentCCCooldown and AgentCCMaxJobs are the account pool's limits
	// (MUSTER_AGENT_CC_RATE_LIMIT_COOLDOWN, MUSTER_AGENT_CC_MAX_CONCURRENT_JOBS);
	// zero is unset: ccpool.DefaultCooldown for the cooldown, and NO cap.
	AgentCCCooldown time.Duration
	AgentCCMaxJobs  int
	// AgentCCBypassPools is MUSTER_AGENT_CC_BYPASS_POOLS read by splitList:
	// trimmed, in order, LOWER-CASED (pool names are lower-case only, so
	// "Pr-Review" names the pool "pr-review"). Empty is none.
	AgentCCBypassPools []string

	// RetiredKindSettings names every setting of a REMOVED kind this
	// deployment still sets — a `hermes` entry in MUSTER_AGENT_KINDS (dropped
	// from AgentKinds by loadConfig), MUSTER_AGENT_HERMES_IMAGE, or its storage
	// size — in the words the boot WARNING prints. Names only, never a value.
	RetiredKindSettings []string

	// AgentNetpolFromNS and AgentNetpolFromLabels say which pods a claude-code
	// agent's NetworkPolicy admits: muster's own. AgentNetpolFromLabels is the
	// variable AS WRITTEN; parsePodLabels is its one parser, and validateKinds
	// refuses what that rejects. Both are required exactly when the claude-code
	// kind is enabled on the kubernetes driver, and refused otherwise.
	AgentNetpolFromNS     string
	AgentNetpolFromLabels string
}

// agentKinds resolves the unset list to the gateway kind alone, for the reason
// agentProvisioner's doc gives about configs built outside loadConfig.
func (c config) agentKinds() []string {
	if len(c.AgentKinds) == 0 {
		return []string{agents.KindGateway}
	}
	return c.AgentKinds
}

// claudeCodeEnabled reports whether the claude-code kind is in the list.
func (c config) claudeCodeEnabled() bool {
	for _, k := range c.agentKinds() {
		if k == agents.KindClaudeCode {
			return true
		}
	}
	return false
}

// confinedKindsEnabled reports whether a kind whose pod is confined and keeps a
// persistent /data volume is enabled: claude-code, which declares network
// isolation unconditionally (agentspec.ClaudeCodeNetwork) and a persistent
// workspace.
func (c config) confinedKindsEnabled() bool {
	return c.claudeCodeEnabled()
}

// agentNetworkPolicy reports whether this deployment renders a NetworkPolicy
// for its confined agents (claude-code): such a kind is enabled AND the
// driver is the one that has NetworkPolicies.
//
// 🔴 ONE PREDICATE, THREE READERS, AND THEY MUST NOT DISAGREE: validateKinds
// (are the two selector variables required, or refused), k8sDriverConfig (is
// k8s.Config.NetworkPolicy set) and the banner (which arm is printed). The kind
// declares network isolation in its spec unconditionally, so where this is true
// the driver MUST be given the selector: k8sDriverConfig sets the pointer on
// exactly this predicate, and validateKinds requires the two variables on
// exactly this predicate. If those two disagreed, k8sDriverConfig would either
// hand the driver an empty selector (k8s.New refuses it, at boot) or leave the
// pointer nil (the driver then refuses every claude-code spec at CheckSpec, on
// whatever path reached it). A banner that disagreed would print the wrong arm
// — "WRITTEN" over a deployment that writes nothing, or the reverse.
func (c config) agentNetworkPolicy() bool {
	return c.confinedKindsEnabled() && c.agentProvisioner() == provisionerK8s
}

// parsePodLabels parses a comma list of `key=value` pod labels.
//
// 🔴 IT DOES NOT LOWER-CASE, UNLIKE splitList. A label value is case-sensitive
// to the apiserver, so normalising one here would render a selector that matches
// nothing — and a selector that matches nothing is a policy that admits nobody.
//
// It refuses rather than skips: an entry with no `=`, an empty key or value, a
// key written twice, and an empty list. Each of those dropped silently would
// WIDEN or EMPTY the selector the operator wrote.
func parsePodLabels(v string) (map[string]string, error) {
	out := map[string]string{}
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, value, ok := strings.Cut(part, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !ok || key == "" || value == "" {
			return nil, fmt.Errorf("entry %q is not key=value", part)
		}
		if msgs := validation.IsQualifiedName(key); len(msgs) > 0 {
			return nil, fmt.Errorf("entry %q: label key %q: %s", part, key, strings.Join(msgs, "; "))
		}
		if msgs := validation.IsValidLabelValue(value); len(msgs) > 0 {
			return nil, fmt.Errorf("entry %q: label value %q: %s", part, value, strings.Join(msgs, "; "))
		}
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("label key %q is written twice", key)
		}
		out[key] = value
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("it names no label")
	}
	return out, nil
}

// parseHostIcons parses MUSTER_HOST_ICONS: a comma list of `<host>=<icon>`.
// Which hosts and icons are allowed is ui.HostPreferences.Normalized's; this
// refuses only an entry that is not host=icon. Empty is none.
func parseHostIcons(v string) (map[string]string, error) {
	out := map[string]string{}
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		host, icon, ok := strings.Cut(part, "=")
		host, icon = strings.TrimSpace(host), strings.TrimSpace(icon)
		if !ok || host == "" || icon == "" {
			return nil, fmt.Errorf("entry %q is not host=icon", part)
		}
		if _, dup := out[strings.ToLower(host)]; dup {
			return nil, fmt.Errorf("host %q is given two icons", host)
		}
		out[strings.ToLower(host)] = icon
	}
	return out, nil
}

// splitList parses a comma list: trimmed, lower-cased, empties dropped.
func splitList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.ToLower(strings.TrimSpace(part)); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseOwnerIDs parses a comma list of positive user ids. Empty is no owners.
func parseOwnerIDs(v string) (map[int64]bool, error) {
	out := map[int64]bool{}
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("%q is not a user id", part)
		}
		out[id] = true
	}
	return out, nil
}

// envFlag reads a boolean env switch. Anything other than an explicit
// affirmative is FALSE — an unset, empty or misspelled value must never arm
// something, and a permissive parse is how a typo turns into an armed surface.
func envFlag(getenv func(string) string, name string) bool {
	switch strings.ToLower(strings.TrimSpace(getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// loadConfig resolves the whole configuration from getenv.
//
// ⚠ IT DOES NOT VALIDATE. Parsing and refusing are separate so a test can build
// an intentionally incomplete config and assert on the refusal text, and so a
// single malformed value reports its own name rather than the first `Fatalf`
// the wiring happens to reach. See config.validate.
func loadConfig(getenv func(string) string) (config, error) {
	c := config{
		Port:                 defaultPort,
		Database:             strings.TrimSpace(getenv(envDatabase)),
		UIPassword:           getenv(envUIPassword),
		HookToken:            getenv(envHookToken),
		ServiceToken:         getenv(envServiceToken),
		SecureCookies:        envFlag(getenv, envSecureCookies),
		AuthMode:             strings.ToLower(strings.TrimSpace(getenv(envAuthMode))),
		GoTrueURL:            strings.TrimRight(strings.TrimSpace(getenv(envGoTrueURL)), "/"),
		GoTrueSecret:         strings.TrimSpace(getenv(envGoTrueSecret)),
		GitHubToken:          strings.TrimSpace(getenv(envGitHubToken)),
		PublicURL:            strings.TrimSpace(getenv(envPublicURL)),
		GitHubClientID:       strings.TrimSpace(getenv(envGitHubClientID)),
		GitHubClientSecret:   strings.TrimSpace(getenv(envGitHubClientSec)),
		FaroURL:              strings.TrimSpace(getenv(envFaroURL)),
		FaroKey:              strings.TrimSpace(getenv(envFaroKey)),
		FaroTracing:          envFlag(getenv, envFaroTracing),
		TagAutoDispatch:      envFlag(getenv, envTagAutoDispatch),
		Mirror:               envFlag(getenv, envMirror),
		Plugins:              envFlag(getenv, envPlugins),
		PluginTaskNamespaces: splitList(getenv(envPluginTaskNamespaces)),
		SessionRouting:       envFlag(getenv, envSessionRouting),
		RouteRelayAllowRaw:   strings.TrimSpace(getenv(envRouteRelayAllow)),
		FlowAgentDenyWords:   splitList(getenv(envFlowAgentDenyWords)),
		MemoURL:              strings.TrimRight(strings.TrimSpace(getenv(envMemoURL)), "/"),
		MemoToken:            strings.TrimSpace(getenv(envMemoToken)),
		MemoScopeMap:         strings.TrimSpace(getenv(envMemoScopeMap)),
		STTURL:               strings.TrimRight(strings.TrimSpace(getenv(envSTTURL)), "/"),
		STTToken:             strings.TrimSpace(getenv(envSTTToken)),
		S3Endpoint:           strings.TrimRight(strings.TrimSpace(getenv(envS3Endpoint)), "/"),
		S3Bucket:             strings.TrimSpace(getenv(envS3Bucket)),
		S3AccessKey:          strings.TrimSpace(getenv(envS3AccessKey)),
		S3SecretKey:          strings.TrimSpace(getenv(envS3SecretKey)),
		S3Region:             strings.TrimSpace(getenv(envS3Region)),
		UploadSigningKey:     strings.TrimSpace(getenv(envUploadSigningKey)),
		UploadMaxBytes:       strings.TrimSpace(getenv(envUploadMaxBytes)),
		UploadLinkTTL:        strings.TrimSpace(getenv(envUploadLinkTTL)),
		RouterURL:            strings.TrimSpace(getenv(envRouterURL)),
		RouterToken:          strings.TrimSpace(getenv(envRouterToken)),
		RouterActor:          strings.TrimSpace(getenv(envRouterActor)),
		Standalone:           envFlag(getenv, envStandalone),
		ApprovalGateURL:      strings.TrimSpace(getenv(envApprovalGateURL)),
		ApprovalGateToken:    strings.TrimSpace(getenv(envApprovalGateToken)),
		VAPIDPublicKey:       strings.TrimSpace(getenv(envVAPIDPublicKey)),
		VAPIDPrivateKey:      strings.TrimSpace(getenv(envVAPIDPrivateKey)),
		VAPIDSubject:         strings.TrimSpace(getenv(envVAPIDSubject)),
		PushExtraHosts:       strings.TrimSpace(getenv(envPushExtraHosts)),
		AndroidAllowPINKeys:  envFlag(getenv, envAndroidAllowPINKeys),

		AgentProvisioner:  strings.ToLower(strings.TrimSpace(getenv(envAgentProvisioner))),
		AgentGateway:      strings.ToLower(strings.TrimSpace(getenv(envAgentGateway))),
		AgentGatewayModel: strings.TrimSpace(getenv(envAgentGatewayModel)),
		// ⚠ TrimSpace IS SAFE ON BOTH AND IS NOT A PARSE. A YAML block scalar in
		// the operator's ConfigMap carries a trailing newline, and `sh` reads a
		// final line without one; neither a JSON document nor a shell script
		// changes meaning when surrounding whitespace is removed. What it buys is
		// that a key holding only a newline reads as EMPTY and hits the refusal
		// below, rather than reaching an instance as a one-byte script.
		AgentRuntimeConfig:    strings.TrimSpace(getenv(envAgentRuntimeConfig)),
		AgentRuntimeInstall:   strings.TrimSpace(getenv(envAgentRuntimeInstall)),
		AgentImageRepo:        strings.TrimSpace(getenv(envAgentImageRepo)),
		AgentImageTag:         strings.TrimSpace(getenv(envAgentImageTag)),
		AgentAPIURL:           strings.TrimSpace(getenv(envAgentAPIURL)),
		AgentModel:            strings.TrimSpace(getenv(envAgentModel)),
		AgentOpenRouterKey:    strings.TrimSpace(getenv(envAgentOpenRouterKey)),
		AgentNamespaceShared:  envFlag(getenv, envAgentNSShared),
		AgentNamespace:        strings.TrimSpace(getenv(envAgentNamespace)),
		AgentNamespacePrefix:  strings.TrimSpace(getenv(envAgentNSPrefix)),
		AgentWorkspacePersist: envFlag(getenv, envAgentWorkspaceKeep),
		AgentStorageClass:     strings.TrimSpace(getenv(envAgentStorageClass)),
		AgentEndpointTemplate: strings.TrimSpace(getenv(envAgentEndpointTmpl)),
		AgentCairnURL:         strings.TrimSpace(getenv(envAgentCairnURL)),
		AgentCairnToken:       strings.TrimSpace(getenv(envAgentCairnToken)),
		AgentKinds:            splitList(getenv(envAgentKinds)),
		AgentCCImage:          strings.TrimSpace(getenv(envAgentCCImage)),
		AgentCCAccountNames:   splitList(getenv(envAgentCCAccounts)),
		AgentCCStorage:        strings.TrimSpace(getenv(envAgentCCStorage)),
		AgentCCBypassPools:    splitList(getenv(envAgentCCBypassPools)),
		AgentNetpolFromNS:     strings.TrimSpace(getenv(envAgentNetpolFromNS)),
		AgentNetpolFromLabels: strings.TrimSpace(getenv(envAgentNetpolFromLabels)),
	}
	c.AgentKinds, c.RetiredKindSettings = dropRetiredKinds(c.AgentKinds, getenv)
	if len(c.AgentCCAccountNames) > 0 {
		c.AgentCCTokens = make(map[string]string, len(c.AgentCCAccountNames))
		for _, name := range c.AgentCCAccountNames {
			c.AgentCCTokens[name] = strings.TrimSpace(getenv(envAgentCCTokenPrefix + ccpool.EnvSuffix(name)))
		}
	}
	if c.RouterActor == "" {
		c.RouterActor = defaultRouterActor
	}
	if c.AgentProvisioner == "" {
		c.AgentProvisioner = provisionerNone
	}
	if c.AgentGateway == "" {
		c.AgentGateway = gatewayNone
	}
	// 🔴 RESOLVED ONCE, HERE, AND HANDED TO BOTH SIDES OF THE NAMESPACE PAIR.
	// This field is the ONLY source for two consumers that must not disagree:
	// k8sDriverConfig gives it to the driver as k8s.Config.NamespacePrefix
	// (where the instance goes), and newApp gives it to
	// api.Extensions.AgentNamespacePrefix (what the row records). One field, two
	// readers — so a `config` built anywhere, including every struct-literal test
	// fixture in this package, feeds them the same value and cannot configure one
	// without the other.
	//
	// The defect this closes was measured live on a deployment that sets
	// MUSTER_AGENT_NAMESPACE_PREFIX=muster-agent-: the row-writing path used
	// agents.NamespacePrefix ("devpod-") instead of this field, so a freshly
	// provisioned agent's row said `devpod-<name>` while the driver had created
	// `muster-agent-<name>` — a card pointing at an empty namespace, which reads
	// as "never provisioned".
	//
	// ⚠ agents.ResolveNamespacePrefix IS THE DEFAULTING SITE, NOT AN `== ""`
	// BRANCH HERE. api.Extensions.AgentNamespace defaults too (its fixtures do not
	// set the field), and two defaulting expressions is how one of them comes to
	// hold a different literal.
	c.AgentNamespacePrefix = agents.ResolveNamespacePrefix(c.AgentNamespacePrefix)

	if v := strings.TrimSpace(getenv(envAgentCCCooldown)); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Minute || d > 7*24*time.Hour {
			return config{}, fmt.Errorf("invalid %s %q: want a Go duration between 1m and 168h (unset means %s)",
				envAgentCCCooldown, v, ccpool.DefaultCooldown)
		}
		c.AgentCCCooldown = d
	}
	if v := strings.TrimSpace(getenv(envAgentCCMaxJobs)); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 64 {
			return config{}, fmt.Errorf("invalid %s %q: want an integer 1-64 (unset means no per-account cap)",
				envAgentCCMaxJobs, v)
		}
		c.AgentCCMaxJobs = n
	}

	if v := strings.TrimSpace(getenv(envPort)); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil || p < 1 || p > 65535 {
			return config{}, fmt.Errorf("invalid %s %q: must be an integer 1-65535", envPort, v)
		}
		c.Port = p
	}

	// 🔴 PARSED HERE, AND REFUSED HERE, FOR THE REASON EVERY OTHER AGENT-SPEC
	// VARIABLE IS REFUSED AT BOOT: agentspec.Build refuses a bad port too, and it
	// is called from inside a dispatch goroutine whose only output is a log line
	// — by which point internal/api's createAndDispatchAgent has already answered
	// the operator's POST with 200. A typo in this variable has to stop the
	// process, not one dispatch.
	//
	// ⚠ 1-65535 RATHER THAN >=0, so a written `0` is refused instead of silently
	// meaning "the default". Zero is the UNSET sentinel on config.AgentGatewayPort
	// — agentspec resolves it to its own measured constant — and an operator who
	// typed 0 did not mean that.
	//
	// The range is restated rather than shared with MUSTER_PORT's check above
	// because the two answer different questions (a listen port this process binds
	// versus a port inside somebody else's image) and would not move together; the
	// one rule that IS shared is agentspec.buildPorts', which refuses the same
	// range on the field this feeds.
	if v := strings.TrimSpace(getenv(envAgentGatewayPort)); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil || p < 1 || p > 65535 {
			return config{}, fmt.Errorf("invalid %s %q: must be an integer 1-65535, or unset for "+
				"agentspec.DefaultGatewayPort (the port measured against the agent runtime image)",
				envAgentGatewayPort, v)
		}
		c.AgentGatewayPort = p
	}

	w, err := stepup.ParseWindow(getenv(envStepUpWindow))
	if err != nil {
		return config{}, fmt.Errorf("invalid %s: %w", envStepUpWindow, err)
	}
	c.StepUpWindow = w
	if v := strings.TrimSpace(getenv(envTaskReapAfter)); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return config{}, fmt.Errorf("invalid %s %q: %w (want a Go duration, e.g. 168h)", envTaskReapAfter, v, err)
		}
		if d <= 0 {
			return config{}, fmt.Errorf("invalid %s %q: must be positive", envTaskReapAfter, v)
		}
		c.TaskReapAfter = d
	}
	d, err := parseAgentReapStoppedAfter(getenv(envAgentReapStoppedAfter))
	if err != nil {
		return config{}, err
	}
	c.AgentReapStoppedAfter = d
	c.SendQueueIdle = sendqueue.DefaultIdleFor
	if v := strings.TrimSpace(getenv(envSendQueueIdle)); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < sendqueue.MinIdleFor || d > sendqueue.MaxIdleFor {
			return config{}, fmt.Errorf("invalid %s %q: want a Go duration between %s and %s (e.g. 30s)",
				envSendQueueIdle, v, sendqueue.MinIdleFor, sendqueue.MaxIdleFor)
		}
		c.SendQueueIdle = d
	}
	c.FocusInterval = focus.DefaultInterval
	if v := strings.TrimSpace(getenv(envFocusInterval)); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || (d != 0 && (d < time.Minute || d > 24*time.Hour)) {
			return config{}, fmt.Errorf("invalid %s %q: want 0 (off) or a Go duration between 1m and 24h (e.g. 15m)", envFocusInterval, v)
		}
		c.FocusInterval = d
	}
	c.AgentsIdleAfterRaw = strings.TrimSpace(getenv(envAgentsIdleAfter))
	idleAfter, err := ui.ParseIdleSectionAfter(c.AgentsIdleAfterRaw)
	if err != nil {
		return config{}, fmt.Errorf("invalid %s: %v", envAgentsIdleAfter, err)
	}
	c.AgentsIdleAfter = idleAfter
	c.FocusRetention = focus.DefaultRetention
	if v := strings.TrimSpace(getenv(envFocusRetention)); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Hour || d > 90*24*time.Hour {
			return config{}, fmt.Errorf("invalid %s %q: want a Go duration between 1h and 2160h (e.g. 168h)", envFocusRetention, v)
		}
		c.FocusRetention = d
	}
	c.RouteTimeout = api.DefaultRouteTimeout
	if v := strings.TrimSpace(getenv(envRouteTimeout)); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < api.MinRouteTimeout || d > api.MaxRouteTimeout {
			return config{}, fmt.Errorf("invalid %s %q: want a Go duration between %s and %s (e.g. 30m)",
				envRouteTimeout, v, api.MinRouteTimeout, api.MaxRouteTimeout)
		}
		c.RouteTimeout = d
	}
	// 🔴 A ROUTE MUST OUTLAST ITS OWN PASTE. A routed message is pasted only
	// once the window has been idle for MUSTER_SEND_QUEUE_IDLE, so a deadline
	// no longer than that wait times the route out before its message can
	// reach the session — every route would go back to the chief unread.
	// Checked only with routing on: with it off neither value is about routes,
	// and a long idle wait alone is a valid setting.
	if c.SessionRouting && c.RouteTimeout <= c.SendQueueIdle {
		return config{}, fmt.Errorf("invalid %s %s: it must be longer than %s (%s), the idle wait before a routed message is even pasted",
			envRouteTimeout, c.RouteTimeout, envSendQueueIdle, c.SendQueueIdle)
	}
	if c.RouteRelayAllowRaw != "" {
		l, err := plugins.ParseRelayAllowlist(c.RouteRelayAllowRaw)
		if err != nil {
			return config{}, fmt.Errorf("invalid %s: %v (want <user id>:<plugin>:<ASCII address, *@domain, agent-mail owner/handle or owner/*>, comma-separated)", envRouteRelayAllow, err)
		}
		c.RouteRelayAllow = l
	}
	c.TaskRouting = envFlag(getenv, envTaskRouting)
	c.ChiefDispatchOwnersRaw = strings.TrimSpace(getenv(envChiefDispatchOwners))
	owners, err := parseOwnerIDs(c.ChiefDispatchOwnersRaw)
	if err != nil {
		return config{}, fmt.Errorf("invalid %s: %v (want user ids, comma-separated)", envChiefDispatchOwners, err)
	}
	c.ChiefDispatchOwners = owners
	c.ChiefSessionRuntimes = splitList(getenv(envChiefSessionRuntimes))

	icons, err := parseHostIcons(getenv(envHostIcons))
	if err != nil {
		return config{}, fmt.Errorf("invalid %s: %v (want <host>=<icon>, comma-separated)", envHostIcons, err)
	}
	if c.HostPrefs, err = (ui.HostPreferences{Order: splitList(getenv(envHostOrder)), Icons: icons}).Normalized(); err != nil {
		return config{}, fmt.Errorf("invalid %s / %s: %v", envHostOrder, envHostIcons, err)
	}

	if v := strings.TrimSpace(getenv(envGitHubEncKey)); v != "" {
		key, err := decodeEncryptionKey(v)
		if err != nil {
			return config{}, fmt.Errorf("invalid %s: %w", envGitHubEncKey, err)
		}
		c.GitHubEncKey = key
	}

	if err := c.loadGitHubApp(getenv); err != nil {
		return config{}, err
	}
	if v := strings.TrimSpace(getenv(envCheckoutWaitTimeout)); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Hour || d > 30*24*time.Hour {
			return config{}, fmt.Errorf("invalid %s %q: a Go duration from 1h to 720h (unset means %s)",
				envCheckoutWaitTimeout, v, agents.DefaultCheckoutWaitTimeout)
		}
		c.CheckoutWaitTimeout = d
	}

	if v := strings.TrimSpace(getenv(envConnectorEncKey)); v != "" {
		key, err := decodeEncryptionKey(v)
		if err != nil {
			return config{}, fmt.Errorf("invalid %s: %w", envConnectorEncKey, err)
		}
		c.ConnectorEncKey = key
	}
	if v := strings.TrimSpace(getenv(envConnectPort)); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil || p < 1 || p > 65535 {
			return config{}, fmt.Errorf("invalid %s %q: must be an integer 1-65535", envConnectPort, v)
		}
		c.ConnectPort = p
	}
	c.AgentConnectURL = strings.TrimRight(strings.TrimSpace(getenv(envAgentConnectURL)), "/")
	if v := strings.TrimSpace(getenv(envBrowserPort)); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil || p < 1 || p > 65535 {
			return config{}, fmt.Errorf("invalid %s %q: must be an integer 1-65535", envBrowserPort, v)
		}
		c.BrowserPort = p
	}
	if c.AuthMode == "" {
		c.AuthMode = string(api.AuthModePassword)
	}

	return c, nil
}

// gotrueIssuer and gotrueAudience are what muster's GoTrue stamps on its
// tokens (GOTRUE_JWT_ISSUER / GOTRUE_JWT_AUD in its deployment). Verify
// refuses any other value.
const (
	gotrueIssuer   = "muster-auth"
	gotrueAudience = "authenticated"
)

// minGoTrueSecretLen is the shortest shared secret muster will verify tokens
// with. GoTrue's own guidance is at least 32 characters; a shorter value is a
// placeholder, and a guessable HS256 secret is a forgeable session for anyone.
const minGoTrueSecretLen = 32

// goTrueConfigured reports whether muster's GoTrue is wired at all.
// pluginsEnabled is THE predicate for "the plugin interface is on": the flag AND
// a database (its tokens and messages are rows; buildApp connects one exactly
// when Database is set, or refuses to boot). It decides two things that must
// agree — whether the plugin store (and with it task_create_gated) is wired, and
// whether the chief's prose describes plugins (agentSpecConfig) — so both read it
// here rather than spelling it twice.
func (c config) pluginsEnabled() bool { return c.Plugins && c.Database != "" }

func (c config) goTrueConfigured() bool { return c.GoTrueURL != "" && c.GoTrueSecret != "" }

// validateAuthMode refuses an auth configuration that would boot unsafe.
//
// 🔴 gotrue MODE REFUSES TO BOOT UNLESS EVERY ONE OF THESE HOLDS, and each is
// a real failure rather than tidiness:
//
//   - MUSTER_GOTRUE_URL and MUSTER_GOTRUE_JWT_SECRET (≥32 characters): without
//     them no session can be minted or verified.
//   - an ARMED hook tier (MUSTER_HOOK_TOKEN set). The hook tier is
//     enforce-when-set: unset, every hook route admits ANYONE as the machine
//     user, user 1 — the admin. In password mode that is the documented
//     back-compat debt; once other people have accounts it is an anonymous
//     admin door beside a per-user login, so gotrue mode will not boot with it.
//   - MUSTER_BROWSER_PORT, distinct from the main and connect ports: the only
//     listener a browser session is served on.
//
// The GoTrue pair is validated in password mode too whenever either half is
// set, because there it arms the admin claim.
func (c config) validateAuthMode() error {
	switch api.AuthMode(c.AuthMode) {
	// "" is a config built without loadConfig (every struct-literal fixture):
	// the default, password.
	case "", api.AuthModePassword, api.AuthModeGoTrue:
	default:
		return fmt.Errorf("%s=%q is not a mode this server has: use %q (the default) or %q",
			envAuthMode, c.AuthMode, api.AuthModePassword, api.AuthModeGoTrue)
	}
	if (c.GoTrueURL == "") != (c.GoTrueSecret == "") {
		return fmt.Errorf("%s and %s are a pair and only one is set (%s=%s, %s=%s). Set both or neither",
			envGoTrueURL, envGoTrueSecret, envGoTrueURL, presence(c.GoTrueURL), envGoTrueSecret, presence(c.GoTrueSecret))
	}
	if c.GoTrueURL != "" {
		u, err := url.Parse(c.GoTrueURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("%s=%q is not an http(s) URL", envGoTrueURL, c.GoTrueURL)
		}
		if len(c.GoTrueSecret) < minGoTrueSecretLen {
			return fmt.Errorf("%s is shorter than %d characters: it signs every session, so a short "+
				"value is a forgeable login. Use GoTrue's GOTRUE_JWT_SECRET", envGoTrueSecret, minGoTrueSecretLen)
		}
	}
	if c.BrowserPort != 0 && (c.BrowserPort == c.Port || c.BrowserPort == c.ConnectPort) {
		return fmt.Errorf("%s=%d collides with another listener's port", envBrowserPort, c.BrowserPort)
	}
	if api.AuthMode(c.AuthMode) != api.AuthModeGoTrue {
		return nil
	}
	if !c.goTrueConfigured() {
		return fmt.Errorf("%s=gotrue needs %s and %s: without them no session can be minted or verified",
			envAuthMode, envGoTrueURL, envGoTrueSecret)
	}
	if c.HookToken == "" {
		return fmt.Errorf("%s=gotrue refuses to boot with %s unset: the hook tier is enforce-when-set, so "+
			"unset it admits ANYONE as the machine user — user 1, the admin. With per-user logins that is an "+
			"anonymous admin door. Set %s", envAuthMode, envHookToken, envHookToken)
	}
	if c.BrowserPort == 0 {
		return fmt.Errorf("%s=gotrue needs %s: browser sessions are served only on that listener, and the "+
			"main port serves machine tokens only. Route the public edge to it", envAuthMode, envBrowserPort)
	}
	return nil
}

// validateConnectors refuses a connector configuration that could only fail
// later, or that would quietly weaken a guarantee.
//
//   - the connector key equal to the GitHub key: one key protecting two
//     unrelated stores means a leak of either is a leak of both. Generate two.
//   - a connect port equal to the main port: the second listener could not bind,
//     and "only the connector surface on this port" would be false anyway.
//   - an agent connect URL without the store or without the connect port: agents
//     would be handed a URL that answers 503, or isolated agents a network
//     allowance to a port nothing serves the connector surface on.
//   - a connect port without the store: a listener serving routes that all 503.
//   - 🔴 the store WITHOUT the agent connect URL. Every agent-facing piece of
//     the store (the connector tools, the gateway prompt's push recipe, a
//     claude-code pod's MCP server and git routing) tells the agent to use
//     $MUSTER_CONNECT_URL, and with no URL configured that variable is empty in
//     every pod: agents would be offered tools and instructions that lead
//     nowhere. Refusing here makes "the store is on" imply "agents can reach
//     it", so the API server's one predicate (Extensions.Connectors != nil)
//     stays true to what agents can actually do, with no second flag to keep in
//     step with it.
func (c config) validateConnectors() error {
	if c.GitHubAppID != 0 && len(c.ConnectorEncKey) == 0 {
		return fmt.Errorf("%s is set but %s is not: the GitHub App's tokens reach agents only through the "+
			"connector proxy, whose grants live in the connector store. Set %s too", envGitHubAppID,
			envConnectorEncKey, envConnectorEncKey)
	}
	if len(c.ConnectorEncKey) > 0 && len(c.GitHubEncKey) > 0 && string(c.ConnectorEncKey) == string(c.GitHubEncKey) {
		return fmt.Errorf("%s is the same key as %s: generate a separate key for each store", envConnectorEncKey, envGitHubEncKey)
	}
	if c.ConnectPort != 0 && len(c.ConnectorEncKey) == 0 {
		return fmt.Errorf("%s is set but %s is not: the connect listener would serve only refusals", envConnectPort, envConnectorEncKey)
	}
	if len(c.ConnectorEncKey) > 0 && c.AgentConnectURL == "" {
		return fmt.Errorf("%s is set but %s is not: agents would be told to use a connector store they cannot "+
			"reach. Set %s (with %s) to the connect listener's in-cluster URL, or unset %s",
			envConnectorEncKey, envAgentConnectURL, envAgentConnectURL, envConnectPort, envConnectorEncKey)
	}
	if c.ConnectPort != 0 && c.ConnectPort == c.Port {
		return fmt.Errorf("%s (%d) is the same as the main port: pick a separate port for the connect listener", envConnectPort, c.ConnectPort)
	}
	if c.AgentConnectURL != "" {
		if len(c.ConnectorEncKey) == 0 || c.ConnectPort == 0 {
			return fmt.Errorf("%s is set but the connector store is not fully configured: it needs %s and %s",
				envAgentConnectURL, envConnectorEncKey, envConnectPort)
		}
		u, err := url.Parse(c.AgentConnectURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
			(u.Path != "" && u.Path != "/") || u.RawQuery != "" {
			return fmt.Errorf("invalid %s %q: want a base URL like http://muster.<namespace>.svc:<port>", envAgentConnectURL, c.AgentConnectURL)
		}
	}
	return nil
}

// githubLoginRE is a GitHub user or organization login.
var githubLoginRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)

// loadGitHubApp reads the GitHub App's three variables.
//
// 🔴 HALF A PAIR, AND A KEY THAT DOES NOT PARSE, REFUSE TO BOOT. Either would
// boot a server whose `github` connector fails every token mint — visible only
// hours later, as an agent whose clone never succeeds.
func (c *config) loadGitHubApp(getenv func(string) string) error {
	id := strings.TrimSpace(getenv(envGitHubAppID))
	key := getenv(envGitHubAppKey)
	accounts := splitList(getenv(envGitHubAppAccounts))
	if id == "" && strings.TrimSpace(key) == "" {
		if len(accounts) > 0 {
			return fmt.Errorf("%s is set but %s is not: an account allowlist needs its App", envGitHubAppAccounts, envGitHubAppID)
		}
		return nil
	}
	if id == "" || strings.TrimSpace(key) == "" {
		return fmt.Errorf("%s and %s are a pair and only one is set: set both (the App's numeric ID, and the "+
			"PEM private key GitHub generated for it) or neither", envGitHubAppID, envGitHubAppKey)
	}
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil || n <= 0 {
		return fmt.Errorf("invalid %s %q: the App's numeric ID (the App's settings page, \"App ID\")", envGitHubAppID, id)
	}
	k, err := githubapp.ParsePrivateKey(key)
	if err != nil {
		// The error names the shape, never the value.
		return fmt.Errorf("invalid %s: %v", envGitHubAppKey, err)
	}
	// 🔴 REQUIRED. The App must be installable on "Any account" to reach an
	// organization its owner does not own, so a stranger can install it on their
	// own repositories; without the list those would become "connected".
	if len(accounts) == 0 {
		return fmt.Errorf("%s is set but %s is empty: name the GitHub accounts (users or organizations, "+
			"comma-separated) whose installations muster may use — anyone can install a public App",
			envGitHubAppID, envGitHubAppAccounts)
	}
	// 🔴 EACH ENTRY IS <login>:<id>, AND THE ID IS WHAT IS MATCHED. A login can be
	// renamed and then re-registered by anyone; the numeric id cannot. A bare
	// login is refused rather than resolved: resolving it at boot would trust
	// whoever holds the name today.
	parsed := make([]githubapp.Account, 0, len(accounts))
	seen := map[int64]string{}
	for _, a := range accounts {
		login, idStr, ok := strings.Cut(a, ":")
		if !ok {
			return fmt.Errorf("invalid %s entry %q: give the account's numeric id as well, as %s:<id> — a "+
				"login alone can be renamed and re-registered by someone else. Find the id with "+
				"`gh api users/%s -q .id`", envGitHubAppAccounts, a, a, a)
		}
		if !githubLoginRE.MatchString(login) {
			return fmt.Errorf("invalid %s entry %q: %q is not a GitHub login", envGitHubAppAccounts, a, login)
		}
		aid, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil || aid <= 0 {
			return fmt.Errorf("invalid %s entry %q: %q is not a numeric account id (`gh api users/%s -q .id`)",
				envGitHubAppAccounts, a, idStr, login)
		}
		// A repeated id is two labels for one account: the rename check could
		// only ever agree with one of them. Refused rather than guessed.
		if prev, dup := seen[aid]; dup {
			return fmt.Errorf("invalid %s: account id %d is listed twice (%q and %q); list each account once",
				envGitHubAppAccounts, aid, prev, a)
		}
		seen[aid] = a
		parsed = append(parsed, githubapp.Account{Login: login, ID: aid})
	}
	c.GitHubAppID, c.GitHubAppKey, c.GitHubAppAccounts = n, k, parsed
	return nil
}

// decodeEncryptionKey decodes the 32-byte AES key the GitHub token is encrypted
// at rest with. Standard or raw base64, padded or not.
//
// 🔴 THERE IS NO "GENERATE ONE IF UNSET" BRANCH, AND ITS ABSENCE IS THE POINT.
// An ephemeral key would make every previously stored GitHub token undecryptable
// on the next restart, which presents as "the Repos tab forgot my account" with
// nothing in the log — the failure is a DATA loss dressed as a UI bug. Unset
// means the GitHub store is not built at all, which is visible on screen.
func decodeEncryptionKey(v string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		if b, err := enc.DecodeString(v); err == nil {
			if len(b) != 32 {
				return nil, fmt.Errorf("decodes to %d bytes, want 32 (AES-256)", len(b))
			}
			return b, nil
		}
	}
	return nil, errors.New("not valid base64 (want 32 random bytes, base64-encoded)")
}

// validate refuses a configuration this binary must not serve behind.
//
// 🔴 DATABASE_URL IS REQUIRED, AND THE PRECEDENT THAT MAKES IT OPTIONAL
// ELSEWHERE DOES NOT APPLY HERE. The permission router keeps an in-memory
// fallback because its core object — a pending approval request — is ephemeral
// by nature and a database-less run is a real, useful mode. muster's core object
// is a TASK, and a task board that came up empty because no database was
// configured renders exactly like a task board with no tasks: an empty list, no
// error, nothing logged, nothing for an operator to notice. That is the "lies
// silently" side of the line api.Extensions.defects draws, so it is refused at
// boot rather than served.
//
// ⚠ buildApp DOES accept a config with no DSN, and that is deliberate rather
// than an inconsistency: the assembly path and the deployment policy are
// separate claims, and the boot test needs the first without the second. The
// policy is HERE, it is tested, and main() applies it.
func (c config) validate() error {
	// Accounts PR 6: an `mst_` value on the machine tier is a personal API
	// token or nothing, so neither shared secret may live in that namespace —
	// nor in the plugin token's (`mpl_`), refused on every other door.
	if reason := c.authConfig().SharedSecretPrefixRefusal(); reason != "" {
		return errors.New(reason)
	}
	if c.Database == "" {
		return fmt.Errorf("%s is not set: muster is its Postgres database, and a server "+
			"started without one serves an EMPTY board that is indistinguishable from a "+
			"board with no tasks — no error, nothing logged. Set %s", envDatabase, envDatabase)
	}
	if c.Standalone && c.RouterURL != "" {
		return fmt.Errorf("%s=1 and %s are both set: they are contradictory claims about "+
			"whether a permission router exists. Unset one", envStandalone, envRouterURL)
	}
	if (c.ApprovalGateURL == "") != (c.ApprovalGateToken == "") {
		return fmt.Errorf("%s and %s are a pair and only one is set (%s=%s, %s=%s): half a gate "+
			"configuration would boot a relay that can never file a card. Set both or neither",
			envApprovalGateURL, envApprovalGateToken, envApprovalGateURL, presence(c.ApprovalGateURL),
			envApprovalGateToken, presence(c.ApprovalGateToken))
	}
	if c.ApprovalGateURL != "" {
		actor := c.RouterActor
		if actor == "" {
			actor = defaultRouterActor
		}
		if !routerActorLabel.MatchString(actor) || actor == routerReservedActor {
			return fmt.Errorf("%s=%q is not an actor label the permission router accepts "+
				"(%s, and not %q): with %s set, every approval card would be refused 403 "+
				"and every prompt would fall back to the terminal. Fix or unset it",
				envRouterActor, actor, routerActorLabel, routerReservedActor, envApprovalGateURL)
		}
	}
	if err := c.validateMemoRelay(); err != nil {
		return err
	}
	if err := c.validateVAPID(); err != nil {
		return err
	}
	if _, err := push.ParseExtraHosts(c.PushExtraHosts); err != nil {
		return fmt.Errorf("%s: %v", envPushExtraHosts, err)
	}
	if err := c.validateSTT(); err != nil {
		return err
	}
	if err := c.validateConnectors(); err != nil {
		return err
	}
	if err := c.validateAuthMode(); err != nil {
		return err
	}
	if err := notes.ValidatePluginTaskNamespaces(c.PluginTaskNamespaces); err != nil {
		return fmt.Errorf("%s: %w", envPluginTaskNamespaces, err)
	}
	return c.validateProvisioner()
}

// pushConfigured reports whether the VAPID triple is set (validate has
// already refused a partial one).
func (c config) pushConfigured() bool {
	return c.VAPIDPublicKey != "" && c.VAPIDPrivateKey != "" && c.VAPIDSubject != ""
}

// vapid is the configured identity.
func (c config) vapid() push.VAPIDConfig {
	return push.VAPIDConfig{PublicKey: c.VAPIDPublicKey, PrivateKey: c.VAPIDPrivateKey, Subject: c.VAPIDSubject}
}

// pushAllowlist is the hosts a push endpoint may be on: the browser push
// services and MUSTER_PUSH_EXTRA_HOSTS (validate has already refused a bad
// entry, so a parse error here cannot happen and reads as no extra hosts).
func (c config) pushAllowlist() push.Allowlist {
	hosts, _ := push.ParseExtraHosts(c.PushExtraHosts)
	return push.NewAllowlist(hosts)
}

// validateVAPID refuses a partial VAPID triple, and a complete one that is not
// a working identity.
//
// 🔴 HALF A CONFIGURATION REFUSES BOOT, LIKE THE APPROVAL-GATE PAIR. With one
// or two of the three set the operator meant push to be on; booting without
// it would draw no Enable button and log nothing anyone reads, which is the
// "silent death" this subsystem's plan names as its first risk.
func (c config) validateVAPID() error {
	set := 0
	for _, v := range []string{c.VAPIDPublicKey, c.VAPIDPrivateKey, c.VAPIDSubject} {
		if v != "" {
			set++
		}
	}
	if set == 0 {
		return nil
	}
	if set != 3 {
		return fmt.Errorf("%s, %s and %s are a triple and only %d of them are set (%s=%s, %s=%s, %s=%s): "+
			"half a push configuration would boot with no push. Set all three (`muster-server vapid generate`) "+
			"or none", envVAPIDPublicKey, envVAPIDPrivateKey, envVAPIDSubject, set,
			envVAPIDPublicKey, presence(c.VAPIDPublicKey), envVAPIDPrivateKey, presence(c.VAPIDPrivateKey),
			envVAPIDSubject, presence(c.VAPIDSubject))
	}
	if err := c.vapid().Validate(); err != nil {
		return fmt.Errorf("%s/%s/%s: %v", envVAPIDPublicKey, envVAPIDPrivateKey, envVAPIDSubject, err)
	}
	return nil
}

// presence renders whether a value is set, WITHOUT rendering the value.
//
// 🔴 IT EXISTS SO A REFUSAL CAN NAME WHICH HALF OF A PAIR IS MISSING WITHOUT
// PRINTING EITHER HALF. The runtime-config bundle is a multi-line configuration
// file and a multi-line shell script; `%q` on one of those produces a refusal
// nobody can read, and a truncation produces one that looks like corruption.
// Neither value is confidential — the credential is derived per agent and never
// travels in this bundle — so this is about legibility, not secrecy, and saying
// so matters: a reader who assumes secrecy will add redaction somewhere it is not
// needed, or assume the pair IS safe to log and print the next thing that is not.
func presence(v string) string {
	if v == "" {
		return "unset"
	}
	return "set"
}

// oneOf reports whether value is in choices.
//
// 🔴 IT IS ONE FUNCTION BECAUSE IT WAS TWO IDENTICAL LOOPS, AND THE SECOND ARRIVED
// WITH THE CHAT TIER. claude/RULES.md's "one rule, one place" is about the
// PREDICATE, not about any particular helper: a membership check open-coded per
// knob is wrong at N−1 sites in the same direction the moment one of them grows a
// case (a trim, a fold, an alias), and the disagreement is silent — an unrecognised
// value reads as "this tier was never enabled". The change that added the second
// copy also cited that rule while paying it elsewhere, which is how it was found.
func oneOf(value string, choices []string) bool {
	for _, choice := range choices {
		if value == choice {
			return true
		}
	}
	return false
}

// validateProvisioner refuses an agent-provisioning configuration that could
// only fail later.
//
// 🔴 IT IS A SEPARATE FUNCTION CALLED FROM validate, NOT A BLOCK BESIDE THE
// ROUTER CHECK, FOR THE REASON THE ROUTER CHECK ITSELF RECORDS: MUSTER_STANDALONE
// and MUSTER_ROUTER_URL are mutually exclusive and validate is where that is
// enforced, so a new knob added *beside* the existing checks rather than checked
// *against* them is how a contradictory pair ships unnoticed. Nothing here
// interacts with the router pair — stated because "I checked" and "there was
// nothing to check" are different claims and only one of them is worth writing.
//
// ⚠ EVERY REFUSAL HERE IS ABOUT A COMBINATION THAT WOULD OTHERWISE FAIL AT THE
// FIRST DISPATCH, IN A BACKGROUND GOROUTINE, WHOSE ONLY OUTPUT IS A LOG LINE the
// user who clicked dispatch never sees. That is the whole argument for boot-time
// refusal: the same misconfiguration is either one fatal line at start or a card
// that silently never provisions.
func (c config) validateProvisioner() error {
	named := c.agentProvisioner()
	if !oneOf(named, provisionerChoices) {
		return fmt.Errorf("invalid %s %q: want one of %s", envAgentProvisioner,
			c.AgentProvisioner, strings.Join(provisionerChoices, ", "))
	}

	// 🔴 THE GATEWAY IS CHECKED *AGAINST* THE PROVISIONER, NOT BESIDE IT, AND THAT
	// IS THIS FUNCTION'S OWN STATED LESSON APPLIED TO THE NEXT KNOB. The pair below
	// is the mutually-exclusive kind: this binary's only source of an agent's
	// address is a driver, so a named runtime with no named driver is a chat tier
	// that can never resolve an endpoint. It would boot, print WIRED, and answer
	// every turn with a resolution error from inside a request handler.
	//
	// ⚠ THE REFUSED COMBINATION IS COHERENT IN api's CONTRACT AND UNBUILDABLE HERE,
	// which is a different claim and worth the distinction. api.Gateway's own doc
	// says chat without lifecycle describes "an installation whose instances are
	// provisioned by something else entirely" — true, and it needs an endpoint
	// source that is not a driver. There is none in this binary, so the refusal is
	// about this implementation rather than about the design.
	gw := c.agentGateway()
	if !oneOf(gw, gatewayChoices) {
		return fmt.Errorf("invalid %s %q: want one of %s", envAgentGateway,
			c.AgentGateway, strings.Join(gatewayChoices, ", "))
	}
	if gw != gatewayNone && c.AgentGatewayModel == "" {
		return fmt.Errorf("%s=%s but %s is not set: the runtime's passthrough sentinel is REQUIRED "+
			"in the wire's model field and there is no defensible default — it is the attached "+
			"image's own value, not this project's, and a wrong or missing one is an HTTP 400 from "+
			"inside a chat turn rather than a boot failure",
			envAgentGateway, gw, envAgentGatewayModel)
	}
	if gw != gatewayNone && named == provisionerNone {
		return fmt.Errorf("%s=%s but %s=%s: agent chat resolves each instance's address through "+
			"the provisioning driver, so there is nothing to talk to. Name a driver (%s), or unset "+
			"%s and leave the two chat routes refusing at api.requireGatewayProvisioner",
			envAgentGateway, gw, envAgentProvisioner, named,
			strings.Join([]string{provisionerNoop, provisionerK8s}, "/"), envAgentGateway)
	}

	// 🔴 THE RUNTIME-CONFIG BUNDLE IS CHECKED AGAINST THE GATEWAY IN BOTH
	// DIRECTIONS, AND THE TWO REFUSALS ARE DIFFERENT FAILURES RATHER THAN ONE
	// SYMMETRY. Missing-with-a-gateway is the measured defect: a provisioner that
	// comes up healthy and creates pods that CRASHLOOP, exit 78, restart for ever
	// and report 0/1 — because the agent runtime's gateway refuses to start
	// without its own configuration, and nothing in this binary was installing
	// one. Present-with-no-gateway is the armed-switch shape this function
	// already refuses twice: the bundle's installer writes a gateway CREDENTIAL,
	// and muster can only derive one when a scheme is named, so the bundle would
	// be placed and the credential would be empty — a well-formed configuration
	// the runtime accepts and then refuses every turn against with a 401.
	//
	// ⚠ THE PAIR IS CHECKED, NOT EACH HALF, so an operator who sets one of the
	// two keys gets a message naming the OTHER one rather than a pod that fails on
	// a missing input at startup.
	//
	// ⚠ IT IS PLACED AFTER THE DRIVER CROSS-CHECK DELIBERATELY, AND THE FIRST DRAFT
	// HAD IT BEFORE. A deployment that names a runtime with NO driver provisions
	// nothing, so a bundle is not what it is missing; refusing it for an absent
	// bundle sends the operator to create a ConfigMap no instance would ever read.
	// Each refusal in this function is ordered so the one an operator should act on
	// FIRST fires first.
	//
	// ⚠ NAMING A RUNTIME NOW COSTS AN OPERATOR THREE VARIABLES RATHER THAN ONE,
	// which is a real widening of this knob's commitment and is stated rather than
	// discovered: the sentinel, and these two.
	//
	// 🔴 WHAT THIS DOES *NOT* COVER, NAMED SO IT IS NOT READ AS COMPLETE:
	// MUSTER_AGENT_PROVISIONER=kubernetes WITH MUSTER_AGENT_GATEWAY=none still
	// provisions crashlooping pods, exactly as before this change. That
	// combination is a SUPPORTED deployment — config.AgentGateway's own doc
	// defends "lifecycle without chat", and it is what this binary ran for the
	// whole of the step that wired the provisioner — so refusing it here would
	// break a documented configuration to close a defect it shares. Keying the
	// requirement on the provisioner instead was considered and rejected for the
	// credential reason above: with no scheme named there is no bearer to inject,
	// so muster can place a bundle it cannot complete. doc_seams.go entry 1 carries
	// this as an open gap with its closing condition.
	if gw != gatewayNone && (c.AgentRuntimeConfig == "" || c.AgentRuntimeInstall == "") {
		return fmt.Errorf("%s=%s but %s and %s are not both set (%s is %s, %s is %s): the agent "+
			"runtime's gateway REFUSES TO START without its own configuration file — measured, it "+
			"exits 78 with `Missing config` and the pod crashloops for ever reporting 0/1 — and the "+
			"file's schema belongs to the image, not to this project, so it is supplied rather than "+
			"generated. Create the ConfigMap described in internal/agentspec/runtimeconfig.go and "+
			"project both keys into this pod, or unset %s",
			envAgentGateway, gw, envAgentRuntimeConfig, envAgentRuntimeInstall,
			envAgentRuntimeConfig, presence(c.AgentRuntimeConfig),
			envAgentRuntimeInstall, presence(c.AgentRuntimeInstall), envAgentGateway)
	}
	if gw == gatewayNone && (c.AgentRuntimeConfig != "" || c.AgentRuntimeInstall != "") {
		return fmt.Errorf("%s=%s but a runtime-config bundle is set (%s is %s, %s is %s): the bundle's "+
			"installer writes the agent gateway's CREDENTIAL, and this binary derives one only from a "+
			"NAMED scheme — so with no scheme the bundle would be installed with an empty credential, "+
			"which the runtime accepts and then refuses every turn against with a 401. Name a runtime "+
			"(%s), or unset both keys",
			envAgentGateway, gw, envAgentRuntimeConfig, presence(c.AgentRuntimeConfig),
			envAgentRuntimeInstall, presence(c.AgentRuntimeInstall), gatewayHooksSHA256)
	}

	if err := c.validateKinds(); err != nil {
		return err
	}
	if err := c.validateSessionRuntimes(); err != nil {
		return err
	}

	if named == provisionerNone {
		// Nothing else is read in this mode, so nothing else is refused. An
		// operator who set the image repository and forgot to name a driver is
		// covered by the banner, which says the seam is open.
		return nil
	}
	if c.AgentImageRepo == "" {
		return fmt.Errorf("%s=%s but %s is not set: an agent instance needs a runtime image and "+
			"there is no defensible default for an installation-specific registry. "+
			"agentspec.Build refuses this too, but it refuses inside a dispatch goroutine whose "+
			"only output is a log line", envAgentProvisioner, named, envAgentImageRepo)
	}
	if c.AgentAPIURL == "" {
		return fmt.Errorf("%s=%s but %s is not set: an instance with no base URL cannot reach "+
			"this server, so it would come up healthy and report nothing — the failure would look "+
			"like an idle agent", envAgentProvisioner, named, envAgentAPIURL)
	}
	if named == provisionerK8s && c.AgentNamespaceShared && c.AgentNamespace == "" {
		return fmt.Errorf("%s=1 but %s is not set: the shared layout puts every instance in one "+
			"named namespace and the kubernetes driver requires it. Either name the namespace or "+
			"unset %s and let each instance have its own (%s%s)",
			envAgentNSShared, envAgentNamespace, envAgentNSShared, c.AgentNamespacePrefix, "<name>")
	}
	// ⚠ THERE IS DELIBERATELY NO REFUSAL FOR MUSTER_AGENT_WORKSPACE_PERSIST, and
	// the first draft of this function had one — "the noop driver has no storage
	// to persist to". Measured against provision.DefaultNoopCapabilities, which
	// declares Persistence TRUE: the noop driver accepts a persistent workspace
	// and records it. The refusal would have rejected a working configuration
	// while reading like a safety check.
	return nil
}

// validateKinds refuses an agent-kinds configuration that could only fail at a
// dispatch.
//
// 🔴 EVERY REFUSAL HERE IS A CLAUDE-CODE POD THAT WOULD OTHERWISE BE BUILT BROKEN
// OR NOT AT ALL, IN A BACKGROUND GOROUTINE, AFTER THE OPERATOR'S CLICK ANSWERED
// 200: an unknown kind, an image that does not pin a version, an account list
// with no tokens behind it, an empty pool. And the armed-switch shape: claude-code
// settings with the kind not enabled are refused rather than ignored.
// validateSessionRuntimes refuses a MUSTER_CHIEF_SESSION_RUNTIMES entry that is
// not a session runtime or is listed twice, and claude-code while the
// claude-code kind is off (no chief could run such a session).
func (c config) validateSessionRuntimes() error {
	seen := map[string]bool{}
	for _, rt := range c.ChiefSessionRuntimes {
		if !slices.Contains(agents.SessionRuntimes, rt) {
			return fmt.Errorf("invalid %s entry %q: want a comma list of %s", envChiefSessionRuntimes, rt, strings.Join(agents.SessionRuntimes, ", "))
		}
		if seen[rt] {
			return fmt.Errorf("invalid %s: %q is listed twice", envChiefSessionRuntimes, rt)
		}
		seen[rt] = true
	}
	// A claude-code chief's gateway sessions run on the gateway runtime BESIDE
	// ccd, built from the same runtime bundle as a gateway-kind agent: without
	// it, the chief's next Start would fail to build its spec.
	if seen[agents.SessionRuntimeGateway] && c.claudeCodeEnabled() && c.agentGateway() == gatewayNone {
		return fmt.Errorf("invalid %s: it names %s and %s enables %s, so a %s chief runs the gateway runtime beside "+
			"ccd — and %s is %s, so there is no runtime bundle to build it from",
			envChiefSessionRuntimes, agents.SessionRuntimeGateway, envAgentKinds, agents.KindClaudeCode,
			agents.KindClaudeCode, envAgentGateway, gatewayNone)
	}
	if seen[agents.SessionRuntimeClaudeCode] && !c.claudeCodeEnabled() {
		return fmt.Errorf("invalid %s: it names %s, but %s does not enable that kind, so no chief could run such a session",
			envChiefSessionRuntimes, agents.SessionRuntimeClaudeCode, envAgentKinds)
	}
	return nil
}

func (c config) validateKinds() error {
	seen := map[string]bool{}
	for _, k := range c.agentKinds() {
		if !agents.ValidKind(k) {
			return fmt.Errorf("invalid %s entry %q: want a comma list of %s", envAgentKinds, k, strings.Join(agents.Kinds, ", "))
		}
		if seen[k] {
			return fmt.Errorf("invalid %s: %q is listed twice", envAgentKinds, k)
		}
		seen[k] = true
	}
	if !seen[agents.KindGateway] {
		return fmt.Errorf("invalid %s %q: it must include %s — the supervisor and every agent "+
			"created before kinds existed are that kind, and the dispatch form defaults to it",
			envAgentKinds, strings.Join(c.AgentKinds, ","), agents.KindGateway)
	}
	// 🔴 THE NETWORK-POLICY SELECTOR IS REFUSED WHEREVER NOTHING WOULD RENDER IT,
	// AND THIS RUNS BEFORE THE KIND CHECKS BELOW ON PURPOSE. It has two ways to be
	// an armed switch — no confined kind (claude-code) is enabled, or one
	// is enabled on a driver that has no NetworkPolicies — and an operator who set
	// it believes their agents are confined to callers they named.
	if !c.agentNetworkPolicy() && (c.AgentNetpolFromNS != "" || c.AgentNetpolFromLabels != "") {
		return fmt.Errorf("%s or %s is set but nothing would render a NetworkPolicy from it: that needs %s to "+
			"include %s AND %s=%s (it is %s). Left set it would be an armed switch that does nothing — "+
			"agents you believe are confined, with no policy. Unset both, or enable the kind on that driver",
			envAgentNetpolFromNS, envAgentNetpolFromLabels, envAgentKinds, agents.KindClaudeCode,
			envAgentProvisioner, provisionerK8s, c.agentProvisioner())
	}
	if err := c.validateClaudeCodeKind(); err != nil {
		return err
	}
	// 🔴 THE KUBERNETES DRIVER WRITES A CONFINED AGENT'S NetworkPolicy BEFORE ITS
	// POD AND REFUSES THE AGENT WITHOUT IT — AND THE POLICY CANNOT BE RENDERED
	// WITHOUT KNOWING WHO MUSTER IS. The confined kinds' specs declare network
	// isolation unconditionally (agentspec.ClaudeCodeNetwork), so the driver has
	// to be given the selector. k8s.New refuses an empty one as well, at boot —
	// but in the driver's words (Config.NetworkPolicy.ControllerNamespace), which
	// name nothing an operator can set. Refused here first, naming both variables.
	if c.agentNetworkPolicy() {
		if c.AgentNetpolFromNS == "" || c.AgentNetpolFromLabels == "" {
			return fmt.Errorf("%s enables a confined kind (%s) on %s=%s but %s and %s are not both set (%s is %s, %s is %s): a "+
				"confined agent is created with a NetworkPolicy that admits connections from this server's own pods "+
				"and nothing else, and those two say which pods that is — the namespace this server runs in "+
				"and its pods' labels as key=value,key=value. There is no default: both are read off this "+
				"deployment's own pod template",
				envAgentKinds, agents.KindClaudeCode, envAgentProvisioner, provisionerK8s,
				envAgentNetpolFromNS, envAgentNetpolFromLabels,
				envAgentNetpolFromNS, presence(c.AgentNetpolFromNS),
				envAgentNetpolFromLabels, presence(c.AgentNetpolFromLabels))
		}
		if msgs := validation.IsDNS1123Label(c.AgentNetpolFromNS); len(msgs) > 0 {
			return fmt.Errorf("invalid %s %q: not a namespace name (%s)", envAgentNetpolFromNS,
				c.AgentNetpolFromNS, strings.Join(msgs, "; "))
		}
		if _, err := parsePodLabels(c.AgentNetpolFromLabels); err != nil {
			return fmt.Errorf("invalid %s %q: %v (want key=value,key=value — the labels on this server's own pods)",
				envAgentNetpolFromLabels, c.AgentNetpolFromLabels, err)
		}
	}
	return nil
}

// connectorStoreConfigured reports whether this deployment builds a connector
// store: a database to hold it and a key to seal its secrets.
func (c config) connectorStoreConfigured() bool {
	return c.Database != "" && len(c.ConnectorEncKey) > 0
}

// ccPoolLimits is the account pool's limits: the configured values, with
// ccpool.DefaultCooldown where the cooldown is unset. An unset cap stays zero,
// which is NO cap: the per-account cap is opt-in.
func (c config) ccPoolLimits() ccpool.Limits {
	l := ccpool.DefaultLimits()
	if c.AgentCCCooldown != 0 {
		l.Cooldown = c.AgentCCCooldown
	}
	l.MaxConcurrentJobs = c.AgentCCMaxJobs
	return l
}

// validateClaudeCodeKind checks the claude-code kind's own settings: refused
// while the kind is off (an armed switch), required while it is on.
func (c config) validateClaudeCodeKind() error {
	if !c.claudeCodeEnabled() {
		if c.AgentCCImage != "" || len(c.AgentCCAccountNames) > 0 || c.AgentCCStorage != "" {
			return fmt.Errorf("%s, %s or %s is set but %s does not include %s: those settings would be "+
				"an armed switch that does nothing. Add %s to %s, or unset them",
				envAgentCCImage, envAgentCCAccounts, envAgentCCStorage, envAgentKinds, agents.KindClaudeCode,
				agents.KindClaudeCode, envAgentKinds)
		}
		if c.AgentCCCooldown != 0 || c.AgentCCMaxJobs != 0 {
			return fmt.Errorf("%s or %s is set but %s does not include %s: there is no Claude account pool for "+
				"them to limit. Add %s to %s, or unset them", envAgentCCCooldown, envAgentCCMaxJobs, envAgentKinds,
				agents.KindClaudeCode, agents.KindClaudeCode, envAgentKinds)
		}
		if len(c.AgentCCBypassPools) > 0 {
			return fmt.Errorf("%s is set but %s does not include %s: there are no Claude Code job lanes for it to "+
				"bypass permissions on, so it would be an armed switch that does nothing. Add %s to %s, or unset it",
				envAgentCCBypassPools, envAgentKinds, agents.KindClaudeCode, agents.KindClaudeCode, envAgentKinds)
		}
		return nil
	}
	for _, p := range c.AgentCCBypassPools {
		if !agents.ValidWorkerPool(p) {
			return fmt.Errorf("invalid %s entry %q: a worker pool name is 1-63 of [a-z0-9-], starting with a "+
				"letter or digit", envAgentCCBypassPools, p)
		}
	}
	if c.agentProvisioner() == provisionerNone {
		return fmt.Errorf("%s includes %s but %s=%s: a kind is something a DRIVER provisions, so there is "+
			"nothing to build it with", envAgentKinds, agents.KindClaudeCode, envAgentProvisioner, provisionerNone)
	}
	if c.AgentCCImage == "" {
		return fmt.Errorf("%s includes %s but %s is not set: there is no default image (a reference names "+
			"a registry, which is installation state)", envAgentKinds, agents.KindClaudeCode, envAgentCCImage)
	}
	if !imagePinned(c.AgentCCImage) {
		return fmt.Errorf("invalid %s %q: it must pin a tag or a digest — the image carries a pinned CLI and "+
			"ccd's wire contract, and an unpinned reference lets two agents run different ones", envAgentCCImage, c.AgentCCImage)
	}
	if c.AgentCCStorage != "" {
		if _, err := resource.ParseQuantity(c.AgentCCStorage); err != nil {
			return fmt.Errorf("invalid %s %q: %v (want a Kubernetes quantity, e.g. 10Gi)", envAgentCCStorage, c.AgentCCStorage, err)
		}
	}
	if len(c.AgentCCAccountNames) == 0 {
		return fmt.Errorf("%s includes %s but %s names no account: the setup-token pool is EMPTY, so every "+
			"claude-code dispatch would build a pod with no credential", envAgentKinds, agents.KindClaudeCode, envAgentCCAccounts)
	}
	names := map[string]bool{}
	for _, name := range c.AgentCCAccountNames {
		if !ccpool.ValidName(name) {
			return fmt.Errorf("invalid %s entry %q: an account name is 1-32 of [a-z0-9-], not starting or "+
				"ending with '-'", envAgentCCAccounts, name)
		}
		if names[name] {
			return fmt.Errorf("invalid %s: account %q is listed twice", envAgentCCAccounts, name)
		}
		names[name] = true
		if c.AgentCCTokens[name] == "" {
			return fmt.Errorf("%s names account %q but %s%s is not set: an account with no token is a pod "+
				"that cannot answer one turn", envAgentCCAccounts, name, envAgentCCTokenPrefix, ccpool.EnvSuffix(name))
		}
	}
	return nil
}

// dropRetiredKinds removes every REMOVED kind (agents.RetiredKinds — `hermes`)
// from the MUSTER_AGENT_KINDS list as written, and names each retired setting
// this deployment still sets, for the boot WARNING (logBanner).
//
// 🔴 IGNORED LOUDLY, NOT REFUSED: the live deployment that ran the hermes kind
// lists it and sets its image, so refusing would crash-loop the first rollout
// of the build that removed it. Nothing reads these values any more; the
// WARNING names them so the operator deletes them. Names only, never values.
func dropRetiredKinds(kinds []string, getenv func(string) string) (kept, ignored []string) {
	for _, k := range kinds {
		if agents.IsRetiredKind(k) {
			ignored = append(ignored, fmt.Sprintf("%s entry %q", envAgentKinds, k))
			continue
		}
		kept = append(kept, k)
	}
	for _, name := range []string{envAgentHermesImage, envAgentHermesStorage} {
		if strings.TrimSpace(getenv(name)) != "" {
			ignored = append(ignored, name)
		}
	}
	return kept, ignored
}

// imagePinned reports whether an image reference names a digest or a tag (a ':'
// after the last '/').
func imagePinned(ref string) bool {
	if strings.Contains(ref, "@") {
		return true
	}
	slash := strings.LastIndex(ref, "/")
	return strings.Contains(ref[slash+1:], ":")
}

// osGetenv is os.Getenv as a value, so main() can hand loadConfig the real
// environment and tests can hand it a map.
func osGetenv(name string) string { return os.Getenv(name) }

// minAgentReapStoppedAfter is the shortest stopped window accepted. The reaper
// DESTROYS what it selects, so a value meant as days but written as minutes
// ("14m") is refused at boot rather than destroying every stopped agent on the
// next sweep.
const minAgentReapStoppedAfter = time.Hour

// parseAgentReapStoppedAfter reads MUSTER_AGENT_REAP_STOPPED_AFTER: unset is
// api.DefaultAgentReapStoppedAfter, "0" or "off" is 0 (the reaper is off), and
// anything else must be a Go duration of at least minAgentReapStoppedAfter.
func parseAgentReapStoppedAfter(raw string) (time.Duration, error) {
	v := strings.TrimSpace(raw)
	switch strings.ToLower(v) {
	case "":
		return api.DefaultAgentReapStoppedAfter, nil
	case "0", "off":
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w (want a Go duration, e.g. 336h, or 0/off)", envAgentReapStoppedAfter, v, err)
	}
	if d < minAgentReapStoppedAfter {
		return 0, fmt.Errorf("invalid %s %q: must be at least %s (or 0/off to turn the reaper off)", envAgentReapStoppedAfter, v, minAgentReapStoppedAfter)
	}
	return d, nil
}
