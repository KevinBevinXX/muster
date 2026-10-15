// Command muster-server is muster's HTTP service: the task board, the agent
// surfaces, the connectors and the machine API, over the Postgres
// database internal/db migrates.
//
// 🔴 IT EXISTS BECAUSE EVERYTHING BELOW IT DID NOT LINK INTO ANYTHING. Before
// this file, `go list -deps ./cmd/...` resolved four packages of this module —
// cmd/muster, cmd/muster-migrate, internal/db and internal/taskstatus — and
// internal/api appeared in ZERO of them. Sixteen of the module's twenty
// packages, including every HTTP handler and every view, were reachable only
// from _test.go files: they compiled, they were tested, and no process could
// ever execute them. The README said so in one place ("there is no server") and
// contradicted itself in another; a reader who saw a non-empty RegisterRoutes
// concluded a running service. TestTheHTTPLayerLinksIntoABinary and
// TestTheServerListensAndServesHealth are what stop that inference being
// available again — the first pins the link graph, the second drives a real
// listener.
//
// Configuration is read from the environment, once, in config.go — which is the
// only place a MUSTER_* name is spelled in this binary.
//
// 🔴 EVERY api DEPENDENCY NOW HAS AN IMPLEMENTATION IN THIS MODULE, AND EACH IS
// STILL OFF BY DEFAULT. This paragraph read "THREE api DEPENDENCIES ARE
// DELIBERATELY LEFT NIL: Provisioner, PrivilegeApply and the ProfileReapplier a
// provisioner may also satisfy." internal/agentprovision now satisfies
// Provisioner AND ProfileReapplier and internal/agentgateway satisfies Gateway;
// PrivilegeApply was removed with the privilege subsystem. What survives is the
// SHAPE of the argument — a fake is worse than a nil — and the defaults:
// MUSTER_AGENT_PROVISIONER and MUSTER_AGENT_GATEWAY both ship off, so an unchanged deployment gets the same nils it always had. The
// full statement — what, why, the closing condition and who checks it — is in
// doc_seams.go, beside the code it affects, and the boot banner says which tiers
// are in force out loud on every start.
package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/ZacxDev/muster/internal/users"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/agentkickoff"
	"github.com/ZacxDev/muster/internal/agentprovision"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/agentstatus"
	"github.com/ZacxDev/muster/internal/api"
	"github.com/ZacxDev/muster/internal/apitokens"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/ccpool"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/credentials"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/flows"
	"github.com/ZacxDev/muster/internal/github"
	"github.com/ZacxDev/muster/internal/gotrue"
	"github.com/ZacxDev/muster/internal/memorelay"
	"github.com/ZacxDev/muster/internal/metrics"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/router"
	"github.com/ZacxDev/muster/internal/sse"
	"github.com/ZacxDev/muster/internal/stepup"
	"github.com/ZacxDev/muster/internal/ui"
)

const (
	// sseBuffer is the per-subscriber event buffer of the broadcast bus.
	sseBuffer = 16

	// retentionInterval is the daily prune of the task-side domains.
	retentionInterval = 24 * time.Hour

	// readHeaderTimeout bounds how long a client may take to send its headers.
	// Without it a slow-loris holds a connection open indefinitely.
	readHeaderTimeout = 10 * time.Second

	// shutdownDrainTimeout bounds the in-flight-request drain on SIGTERM.
	shutdownDrainTimeout = 10 * time.Second

	// pushDrainTimeout bounds the wait for outstanding notification fan-outs
	// AFTER the HTTP drain. It is a FRESH budget rather than the drain's
	// leftovers: reusing an expired context would make this step a silent no-op
	// in exactly the case it matters, which is the failure shape the permission
	// router's own exit path records against reusing a drain context.
	pushDrainTimeout = 5 * time.Second

	// kickoffDrainTimeout bounds the wait for the kickoff deliverer to RECORD the
	// turns SIGTERM cancelled (agentkickoff.Deliverer.Done). A cancelled turn returns
	// at once and its record is one UPDATE, so this is slack, not a turn budget.
	// 10 + 5 + 10 = 25 s, inside Kubernetes' default 30 s
	// terminationGracePeriodSeconds — past that the kubelet SIGKILLs and nothing
	// is recorded at all.
	kickoffDrainTimeout = 10 * time.Second

	// terminalDrainTimeout bounds the wait for open agent terminals to end and
	// RELEASE their tmux clients (api.Server.EndTerminals). It runs CONCURRENTLY
	// with the HTTP drain, so it adds nothing to the 25 s above while it is no
	// longer than shutdownDrainTimeout. It is the release's own budget: a
	// terminal ends at once on cancel, and the release is one short exec.
	terminalDrainTimeout = agentprovision.TerminalReleaseTimeout
)

func main() {
	// `muster-server mirror …` is the operator's mirror-token admin, run where
	// the server's DATABASE_URL is (e.g. `kubectl exec` into the muster pod).
	if len(os.Args) > 1 && os.Args[1] == "mirror" {
		os.Exit(runMirrorCLI(context.Background(), os.Args[2:], osGetenv, os.Stdout, os.Stderr))
	}
	// `muster-server vapid generate`: mint muster's Web Push keypair. Needs no
	// database and no configuration.
	if len(os.Args) > 1 && os.Args[1] == "vapid" {
		os.Exit(runVAPIDCLI(os.Args[2:], os.Stdout, os.Stderr))
	}
	logger := log.New(os.Stdout, "", log.LstdFlags|log.LUTC)

	// One-shot account commands (`muster-server invite --claim-admin`,
	// `muster-server user disable <id>`): run and exit. See accounts_cmd.go.
	if len(os.Args) > 1 {
		os.Exit(runAccountCommand(context.Background(), os.Args[1:], osGetenv, os.Stdout, os.Stderr))
	}

	cfg, err := loadConfig(osGetenv)
	if err != nil {
		logger.Fatalf("config: %v", err)
	}
	if err := cfg.validate(); err != nil {
		logger.Fatalf("config: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	app, err := buildApp(ctx, cfg, logger)
	if err != nil {
		logger.Fatalf("startup: %v", err)
	}
	defer app.Close()

	if err := app.Run(ctx, nil); err != nil {
		logger.Fatalf("http server error: %v", err)
	}
	logger.Print("shutdown complete")
}

// app is the assembled service: every store, the HTTP server, and the teardown
// for whatever was opened to build it.
type app struct {
	cfg    config
	logger *log.Logger
	srv    *api.Server
	http   *http.Server
	// connect is the second listener that serves ONLY the agent-facing connector
	// surface (api.OnlyConnectSurface); nil unless MUSTER_CONNECT_PORT is set.
	connect *http.Server
	// browser is the listener gotrue mode serves browser sessions on
	// (api.BrowserListener); nil unless MUSTER_BROWSER_PORT is set.
	browser *http.Server
	pool    *pgxpool.Pool
	// kickoff delivers dispatched agents' first turns; nil when no gateway is
	// built. See buildKickoffDeliverer.
	kickoff *agentkickoff.Deliverer
	// status promotes provisioning agents to running once their instance is
	// ready; nil when no provisioner is configured. See buildStatusPromoter.
	status *agentstatus.Promoter
	// memo relays cairn scope memos to the operator's agents; nil unless the
	// relay's credential is set and a gateway exists. See buildMemoRelay.
	memo *memorelay.Relay
}

// buildApp wires the whole service from cfg WITHOUT binding a port.
//
// 🔴 THE SPLIT BETWEEN THIS AND Run IS WHAT MAKES "IT SERVES" TESTABLE RATHER
// THAN ASSERTED. A main() that assembles and listens in one function can only be
// checked by reading it, and reading it is exactly what produced the claim this
// binary exists to correct.
//
// ⚠ A config with no DSN is accepted here and refused by config.validate. See
// the note there; the assembly path and the deployment policy are separate
// claims and the boot test needs the first without the second.
func buildApp(ctx context.Context, cfg config, logger *log.Logger) (*app, error) {
	if logger == nil {
		logger = log.Default()
	}

	bus := sse.New(sseBuffer)

	// Frontend RUM. An empty MUSTER_FARO_URL disables it entirely (no script
	// tags rendered), which is the local and test default. The API key is
	// client-public by design — it gates the public Faro collect endpoint only.
	ui.SetFaroConfig(ui.FaroSettings{
		URL:     cfg.FaroURL,
		Key:     cfg.FaroKey,
		Version: api.BuildVersion,
		Tracing: cfg.FaroTracing,
	})
	// Without this the page's connect-src stays 'self' and every telemetry send
	// is blocked by CSP, silently, in the browser only.
	api.SetTelemetryConnectSrc(cfg.FaroURL)

	metrics.BuildInfo.WithLabelValues(api.BuildVersion).Set(1)

	a := &app{cfg: cfg, logger: logger}

	// The permission router client. nil when unconfigured, and every method on a
	// nil client answers router.ErrNotConfigured rather than a zero value — see
	// that package's header for why the distinction is load-bearing.
	port := router.NewPort(router.Config{
		BaseURL: cfg.RouterURL,
		Token:   cfg.RouterToken,
		Actor:   cfg.RouterActor,
	})

	ext := api.Extensions{
		TagAutoDispatch: cfg.TagAutoDispatch,
		// 🔴 THE SAME FIELD THE DRIVER GETS, AND THAT IS THE WHOLE POINT.
		// k8sDriverConfig hands cfg.AgentNamespacePrefix to the driver as
		// k8s.Config.NamespacePrefix (where the instance goes); this hands the same
		// field to the row-writing path (what agents.namespace records). They used
		// to be a config field and a hardcoded constant, which disagreed in
		// production and told nobody. api.Extensions.defects refuses to serve if
		// this line is ever dropped, because an empty prefix beside a wired
		// Provisioner reproduces that defect exactly.
		AgentNamespacePrefix: cfg.AgentNamespacePrefix,
		GitHubOAuth: api.GitHubOAuthConfig{
			ClientID:     cfg.GitHubClientID,
			ClientSecret: cfg.GitHubClientSecret,
			BaseURL:      cfg.PublicURL,
		},
	}

	if cfg.Database != "" {
		pool, err := db.Connect(ctx, cfg.Database)
		if err != nil {
			return nil, fmt.Errorf("db connect: %w", err)
		}
		a.pool = pool
		if err := db.Migrate(ctx, pool, logger); err != nil {
			a.Close()
			return nil, fmt.Errorf("db migrate: %w", err)
		}

		// 🔴 THE NOTES STORE IS WRAPPED, NOT RAW. notes.WithStatusLogging is the
		// one choke point that logs every task-status transition (id, old, new,
		// writer) and raises a WARNING on a lifecycle downgrade — the shape that
		// silently ate a task's completion upstream with no trace in any log.
		// Unwrapping it here re-opens that blind spot for every writer at once.
		ext.Notes = notes.WithStatusLogging(notes.NewPG(pool), logger)
		ext.Agents = agents.NewPG(pool)
		// The request Principal's role comes from users.role (accounts PR 3b);
		// without this store only user 1 would be admin, by fallback.
		ext.Users = users.NewPG(pool)
		// Personal API tokens (accounts PR 6): mst_… on the machine tier.
		ext.APITokens = apitokens.NewPG(pool)
		// Browser flows (migration 0012): the owner-scoped job queue and the
		// per-host runner tokens. Always on with a database: with no runner
		// token minted, nothing ever claims a job and each one expires.
		ext.Flows = flows.NewPG(pool).WithAgentDenyWords(cfg.FlowAgentDenyWords)

		if len(cfg.ConnectorEncKey) > 0 {
			cs, err := connectors.NewPG(pool, cfg.ConnectorEncKey)
			if err != nil {
				a.Close()
				return nil, fmt.Errorf("connector store: %w", err)
			}
			ext.Connectors = cs
		}

		if len(cfg.GitHubEncKey) > 0 {
			gh, err := github.NewPG(pool, cfg.GitHubEncKey)
			if err != nil {
				a.Close()
				return nil, fmt.Errorf("github store: %w", err)
			}
			ext.GitHub = gh
			// The OAuth state's MAC key, derived from the same secret under its
			// own label (github.OAuthStateKey) — never from the UI password.
			if ext.GitHubOAuth.StateKey, err = github.OAuthStateKey(cfg.GitHubEncKey); err != nil {
				a.Close()
				return nil, fmt.Errorf("github oauth state key: %w", err)
			}
			if cfg.GitHubToken != "" {
				// Backgrounded: it is one network round trip to github.com and a
				// boot that blocks on a third party is a boot that a third party's
				// outage can stop.
				go connectStaticGitHub(ctx, gh, cfg.GitHubToken, logger)
			}
		}
	}

	// 🔴 THE ASSIGNMENT IS TWO STEPS, AND THE nil-CHECK IS NOT PEDANTRY. Assigning
	// a typed nil pointer to an interface field produces a NON-nil interface
	// holding a nil pointer, so `s.ext.Provisioner == nil` in
	// api.requireLifecycleProvisioner would be FALSE and every lifecycle route
	// would sail past the wrapper into a nil-pointer method call — a panic in a
	// goroutine instead of a 503 at the door. buildProvisioner returns a concrete
	// *agentprovision.Adapter precisely so this is checkable here rather than
	// invisible behind an interface return.
	//
	// ⚠ AND IT IS NOW TWO ASSIGNMENTS FROM ONE CALL, FOR THE SAME REASON: a nil
	// *agentgateway.Gateway assigned into ext.Gateway would make
	// api.requireGatewayProvisioner's nil check false and turn a 503 into a
	// nil-pointer dereference inside a chat handler.
	// The claude-code kind's account pool, over the same database. nil when that
	// kind is not enabled — and a claude-code deployment with no database is
	// refused here (buildClaudePool), before any agent tier is built.
	var marks ccpool.Store
	if a.pool != nil {
		marks = ccpool.NewPG(a.pool)
	}
	//
	// 🔴 THE CREDENTIAL PLANE IS BUILT ONCE AND HANDED TO ALL THREE CONSUMERS —
	// the pool (as its account Source), the dispatch form (kindSet) and the pod
	// builder (agentprovision, through credResolver) — so "whose credentials"
	// is one decision, not three that could disagree. Members' credentials live
	// in the per-owner store, sealed under a key derived from the connector key;
	// with no such key there is no store, and every member has no credential
	// (their kinds are refused; the environment is never lent to them).
	var personal credentials.Personal
	var roles credentials.RoleReader
	var credStore *credentials.PGStore
	if a.pool != nil {
		roles = users.NewPG(a.pool)
		if len(cfg.ConnectorEncKey) > 0 {
			cs, err := credentials.NewPG(a.pool, cfg.ConnectorEncKey)
			if err != nil {
				a.Close()
				return nil, fmt.Errorf("credential store: %w", err)
			}
			credStore, personal = cs, cs
		}
	}
	plane, err := buildCredentialPlane(cfg, personal, roles, logger)
	if err != nil {
		a.Close()
		return nil, fmt.Errorf("credential plane: %w", err)
	}
	if credStore != nil {
		ext.Credentials = credentialPage{PGStore: credStore, plane: plane}
	}
	ccPool, err := buildClaudePool(cfg, plane, marks)
	if err != nil {
		a.Close()
		return nil, fmt.Errorf("claude account pool: %w", err)
	}
	ext.Kinds = kindSet{kinds: cfg.agentKinds(), pool: ccPool, plane: plane}
	prov, gw, err := buildAgentPlaneWith(cfg, ext.Agents, ccPool, plane, logger)
	if err != nil {
		a.Close()
		// "agent plane", not "agent provisioner": this call builds ALL THREE tiers,
		// so a gateway construction failure once booted under a prefix naming the
		// other subsystem — which sends the reader to the provisioner's
		// configuration.
		return nil, fmt.Errorf("agent plane: %w", err)
	}
	if prov != nil {
		ext.Provisioner = prov
	}
	if gw != nil {
		ext.Gateway = gw
	}
	ext.SessionLiveness = buildSessionLiveness(cfg, port, logger)
	// 🔴 THE ZERO POLICY IS PRODUCTION'S. Only a binary built with the e2e tag
	// sets this hook (e2econnect.go), so a deployable binary cannot be configured
	// to proxy to loopback or over plain HTTP — there is no variable for it.
	if e2eConnectorPolicy != nil {
		ext.ConnectorPolicy = *e2eConnectorPolicy
	}

	// The mirrored-agents plugin: only with MUSTER_MIRROR=1 AND a database (its
	// tokens and rows live there; config.validate already refuses a deployment
	// with no database). Its agents-changed nudge is installed once srv exists.
	// The step-up window (internal/stepup) is process-wide and read by both
	// internal/api and the plugin below, so it is set before either exists.
	// loadConfig already bounded it (and a config built without loadConfig —
	// a test's — leaves it zero, which keeps stepup.DefaultWindow).
	if cfg.StepUpWindow != 0 {
		if err := stepup.SetWindow(cfg.StepUpWindow); err != nil {
			return nil, err
		}
	}
	// The plugin interface: only with MUSTER_PLUGINS=1 AND a database (its
	// tokens and messages are rows). A message is handed to the owner's chief
	// through the agent gateway; with none, messages are stored and stay queued.
	if cfg.pluginsEnabled() {
		ext.Plugins = plugins.NewPG(a.pool)
		if gw != nil {
			ext.PluginGateway = gw
		}
	}
	var mirrorSvc *mirror.Service
	if cfg.Mirror && a.pool != nil {
		mirrorSvc = mirror.NewService(mirror.NewPG(a.pool), logger)
		ext.Mirror = mirrorSvc
	}

	// muster-native Web Push: only with the VAPID triple AND a database (the
	// subscriptions are rows; config.validate refuses a partial triple).
	if cfg.pushConfigured() && a.pool != nil {
		ext.Push = push.NewService(push.NewPG(a.pool), push.NewWebpushPusher(cfg.vapid()), cfg.vapid(), logger)
	}

	// Dictation: the message box's microphone, only with MUSTER_STT_TOKEN.
	transcriber, err := buildTranscriber(cfg)
	if err != nil {
		a.Close()
		return nil, fmt.Errorf("dictation: %w", err)
	}
	ext.Transcriber = transcriber

	srv := api.New(bus, cfg.authConfig(), logger)
	srv.UseExtensions(ext)
	if cfg.goTrueConfigured() && a.pool != nil {
		srv.UseGoTrue(goTrueAuth(cfg, a.pool))
	}
	if mirrorSvc != nil {
		// No agent name: a mirrored session is not an agent. The nudge makes the
		// host OWNER's open Agents views re-fetch — and only that owner's: an SSE
		// event reaches its owner's streams alone (accounts PR 3b), so a nudge
		// with no owner would reach nobody.
		mirrorSvc.SetNotify(srv.BroadcastAgentsChangedFor)
	}
	if port.Configured() {
		srv.UseRouter(port)
	}
	// The claude-code approval relay's gate: its OWN field, so a standalone
	// deployment can arm it without UseRouter / session liveness.
	if g := approvalGateFor(cfg); g != nil {
		srv.UseApprovalGate(g)
	}
	if cfg.TaskReapAfter > 0 {
		srv.SetTaskReapAfter(cfg.TaskReapAfter)
	}
	if a.pool != nil {
		// Readiness pings the database, so a pod whose Postgres is unreachable is
		// pulled from rotation rather than serving 500s. It runs AFTER the wiring
		// check handleReady performs first; both must pass.
		pool := a.pool
		srv.SetReadyCheck(func(ctx context.Context) error { return db.Ping(ctx, pool) })
	}
	a.srv = srv

	// 🔴 THE DELIVERER COMES FROM THE SAME prov AND gw THE ADAPTER'S
	// deliverability was computed from, so "a dispatch is accepted" and "something
	// delivers it" cannot disagree. startBackgroundLoops runs it. It is built after
	// srv so its card changes reach open Agents lists through srv's broadcast.
	if a.kickoff, err = buildKickoffDeliverer(cfg, ext.Agents, prov, gw, srv.BroadcastAgentChanged, logger); err != nil {
		a.Close()
		return nil, fmt.Errorf("kickoff deliverer: %w", err)
	}
	if a.status, err = buildStatusPromoter(cfg, ext.Agents, prov, srv.BroadcastAgentChanged, logger); err != nil {
		a.Close()
		return nil, fmt.Errorf("status promoter: %w", err)
	}
	if a.memo, err = buildMemoRelay(cfg, a.pool, ext.Agents, ext.Notes, gw, logger); err != nil {
		a.Close()
		return nil, fmt.Errorf("memo relay: %w", err)
	}

	handler := srv.Handler()
	a.http = &http.Server{
		Addr:              net.JoinHostPort("", strconv.Itoa(cfg.Port)),
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
	}
	if cfg.ConnectPort != 0 {
		a.connect = &http.Server{
			Addr:              net.JoinHostPort("", strconv.Itoa(cfg.ConnectPort)),
			Handler:           api.OnlyConnectSurface(handler),
			ReadHeaderTimeout: readHeaderTimeout,
		}
	}
	if cfg.BrowserPort != 0 {
		a.browser = &http.Server{
			Addr:              net.JoinHostPort("", strconv.Itoa(cfg.BrowserPort)),
			Handler:           api.BrowserListener(handler),
			ReadHeaderTimeout: readHeaderTimeout,
		}
	}

	a.logBanner(ext, port)
	return a, nil
}

// approvalGateFor builds the gate the claude-code approval relay files through:
// a client for MUSTER_APPROVAL_GATE_URL + _TOKEN, asserting RouterActor, or nil
// when they are unset (the relay then answers 503 and prompts fall back to the
// agent's terminal).
//
// 🔴 THERE IS NO FALLBACK TO A CONFIGURED PERMISSION ROUTER'S CLIENT. One existed
// and was deleted (#48 audit round 0): no deployment runs router mode, the
// operator chose standalone, and a second source would be a second credential
// path into the gate for no consumer. A router deployment that wants approvals
// sets these two variables like any other.
//
// 🔴 THE RESULT IS HANDED TO THE SERVER ONLY AS THE RELAY'S api.GatePort
// (Server.UseApprovalGate). It is never passed to UseRouter or
// buildSessionLiveness, so the gate turns on no router behaviour —
// TestTheApprovalGateDoesNotTurnOnRouterBehaviour pins that.
//
// ⚠ A nil api.GatePort, never a typed nil: an unconfigured router.Port inside the
// interface would make the relay call a client that answers ErrNotConfigured on
// every request (a 502 per prompt) instead of the honest 503.
func approvalGateFor(cfg config) api.GatePort {
	if cfg.ApprovalGateURL == "" || cfg.ApprovalGateToken == "" {
		return nil
	}
	if p := router.NewPort(router.Config{
		BaseURL: cfg.ApprovalGateURL, Token: cfg.ApprovalGateToken, Actor: cfg.RouterActor,
	}); p.Configured() {
		return p
	}
	return nil
}

// buildSessionLiveness decides what answers "does this session still have a
// transcript".
//
// 🔴 THE THREE-WAY CHOICE HERE IS THE MOST CONSEQUENTIAL LINE OF WIRING IN THIS
// BINARY, AND IT IS THE ONE api.Extensions.defects EXISTS TO POLICE.
//
//   - A CONFIGURED ROUTER answers it for real. Use it.
//   - NO ROUTER, AND THE OPERATOR SAID SO (MUSTER_STANDALONE=1): answer "no
//     session has a transcript", which in a deployment with no session store is
//     TRUE. The page renders "no transcript recorded" and is correct.
//   - NO ROUTER, AND NOBODY SAID ANYTHING: return nil, which makes the server
//     report NOT READY for as long as a notes store is wired. That is the whole
//     point. "There is no router" and "I forgot to configure the router" are the
//     same observable, and the second renders a confident falsehood over live
//     transcripts on every surface with nothing logged and nothing 404ing. It is
//     refused at the readiness door instead of guessed at.
//
// 🔴 AN UNCONFIGURED router.Port MUST NEVER REACH THIS FIELD. It satisfies the
// interface and every call on it returns ErrNotConfigured — and a failed
// liveness read is an ERROR, not a false, by design (see session_liveness.go).
// Assigning one would therefore fail every board read outright rather than
// degrade, which is a different and much louder bug than the one above, but it
// is still a bug this wiring must not create. `port.Configured()` is the guard.
func buildSessionLiveness(cfg config, port router.Port, logger *log.Logger) api.SessionLivenessProbe {
	if port.Configured() {
		return port
	}
	if cfg.Standalone {
		return standaloneSessionLiveness{}
	}
	return nil
}

// standaloneSessionLiveness is the probe for a deployment that has no session
// store at all.
//
// 🔴 IT IS A DECLARATION, NOT A STUB, AND THE DIFFERENCE IS WHETHER THE PAGE
// LIES. It reports that no session id has a transcript, WITHOUT an error —
// which is an accurate statement about a deployment where transcripts are not
// recorded anywhere. Reaching it requires an operator to set
// MUSTER_STANDALONE=1, i.e. to assert that fact; nothing infers it.
//
// ⚠ IT MUST NOT BE USED TO SILENCE A READINESS FAILURE ON A DEPLOYMENT THAT
// DOES HAVE A ROUTER. buildSessionLiveness prefers a configured router over
// this unconditionally, and config.validate refuses the contradictory pair, so
// the only way to reach it is a deployment where the assertion holds.
type standaloneSessionLiveness struct{}

func (standaloneSessionLiveness) SessionsExisting(context.Context, []string) (map[string]bool, error) {
	return map[string]bool{}, nil
}

// Run binds the port and serves until ctx is cancelled, then drains.
//
// ready, when non-nil, is called with the bound address once the listener is
// up and before the first request is accepted. It exists so a test can drive
// the REAL listen path on an ephemeral port instead of asserting that a call to
// ListenAndServe appears in the source.
func (a *app) Run(ctx context.Context, ready func(net.Addr)) error {
	ln, err := net.Listen("tcp", a.http.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", a.http.Addr, err)
	}
	var connectLn, browserLn net.Listener
	if a.connect != nil {
		if connectLn, err = net.Listen("tcp", a.connect.Addr); err != nil {
			ln.Close()
			return fmt.Errorf("listen %s (%s): %w", a.connect.Addr, envConnectPort, err)
		}
	}
	if a.browser != nil {
		if browserLn, err = net.Listen("tcp", a.browser.Addr); err != nil {
			ln.Close()
			if connectLn != nil {
				connectLn.Close()
			}
			return fmt.Errorf("listen %s (%s): %w", a.browser.Addr, envBrowserPort, err)
		}
	}
	if ready != nil {
		ready(ln.Addr())
	}

	a.startBackgroundLoops(ctx)

	serveErr := make(chan error, 3)
	go func() {
		a.logger.Printf("muster %s listening on %s (auth tiers reported above)", api.BuildVersion, ln.Addr())
		serveErr <- a.http.Serve(ln)
	}()
	if connectLn != nil {
		go func() {
			a.logger.Printf("muster connector surface listening on %s (only %v)", connectLn.Addr(), api.ConnectSurfacePrefixes)
			serveErr <- a.connect.Serve(connectLn)
		}()
	}
	if browserLn != nil {
		go func() {
			a.logger.Printf("muster browser listener on %s (%s)", browserLn.Addr(), envBrowserPort)
			serveErr <- a.browser.Serve(browserLn)
		}()
	}

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		a.logger.Print("shutdown signal received, draining...")
		a.shutdown()
		return nil
	}
}

// startBackgroundLoops starts the task-side singleton loops.
//
// 🔴 THEY RUN UNGUARDED, AND THAT IS A STATED LIMIT RATHER THAN AN OVERSIGHT.
// api.Server.SetTaskLeaderGate takes a cross-process gate and a nil gate means
// "unguarded" — the single-process path. This module has no lease
// implementation (internal/db carries none), so there is nothing to hand it.
// The consequence is bounded and named in doc_seams.go: at replicas: 1 the
// loops are correct; at replicas > 1, or across the overlap window of a rolling
// update, each prune and each reap runs once PER POD. Both are idempotent
// row-level sweeps, so the cost is duplicated work rather than duplicated
// effect — but it is not zero, and it is why this deployment is
// single-replica until the gate exists.
func (a *app) startBackgroundLoops(ctx context.Context) {
	// 🔴 THE KICKOFF DELIVERER IS STARTED BEFORE THE pool GUARD, AND THE ORDER IS
	// NOT INCIDENTAL. It exists only when the adapter was told a kickoff is
	// deliverable, which already implies a store (buildAgentPlane refuses a
	// provisioner without one); gating it on a second condition would be one more
	// way for "accepted" and "delivered" to come apart. It needs no leader: each
	// delivery takes a per-agent claim (agents.Store.ClaimKickoff) and re-reads the
	// row under it, so replicas cannot both pay one turn.
	if a.kickoff != nil {
		go a.kickoff.Run(ctx, agentkickoff.DefaultInterval)
	}
	// Same reasoning, and it needs no leader either: each promotion is a write
	// conditional on the row not having moved (agents.Store.MarkRunning), so two
	// replicas promoting one row apply it once.
	if a.status != nil {
		go a.status.Run(ctx, agentstatus.DefaultInterval)
	}
	// The memo relay needs no leader either: each delivery takes the agent's
	// session claim (agents.Store.ClaimKickoff) and re-reads the agent and its
	// delivery record under it, so two replicas cannot both send one agent a memo.
	if a.memo != nil {
		go a.memo.Run(ctx, memorelay.DefaultInterval)
	}
	if a.pool == nil {
		return
	}
	go a.srv.RunTaskRetention(ctx, retentionInterval)
	// 🔴 ITS OWN LOOP, ON ITS OWN INTERVAL, NOT THE DAILY ONE. A ticker's first
	// tick lands a full interval in, so on a 24h ticker a pod had to survive a
	// day to reap even once — upstream, across 11 pod lifetimes, only 2 did, and
	// the reaper had effectively never run. api.TaskReapInterval carries the
	// measurement.
	go a.srv.RunTaskReap(ctx, api.TaskReapInterval)
}

// shutdown drains in-flight requests and the kickoff deliverer, then waits for
// outstanding push fan-outs.
//
// 🔴 THE ORDER IS FIXED: HTTP AND THE DELIVERER FIRST, FAN-OUTS LAST. A fan-out
// is registered on the goroutine that triggers it (see api.Server.pushInFlight)
// — an HTTP handler, or the deliverer's card-change broadcast — so waiting for
// fan-outs before either has stopped would wait on a set that is still growing.
func (a *app) shutdown() {
	// Streams end FIRST, by one explicit call, before EITHER server's Shutdown.
	// Shutdown waits for active connections and an SSE stream never goes idle,
	// so an open stream would otherwise hold the drain to its deadline — see
	// api.EndStreams; that covers event streams relayed by the /connect/ proxy,
	// on BOTH listeners, too. It is a call here rather than a RegisterOnShutdown
	// hook per server so that no listener's drain depends on another's having
	// begun first: one hook on the main server alone covered the connect
	// listener only because a.http happens to be drained first.
	// Pinned by TestShutdownDoesNotWaitOutAnOpenEventStream and
	// TestShutdownEndsAProxiedEventStreamOnBothListeners; that ONLY streams end,
	// by TestShutdownEndsStreamsButLetsAnOrdinaryRequestFinish and
	// TestShutdownLetsAnOrdinaryProxiedRequestFinish.
	a.srv.EndStreams()
	// Terminals next, and CONCURRENTLY with the HTTP drain: their sockets are
	// hijacked, so Shutdown neither ends nor waits for them, and each one's
	// release (a second exec into its pod) must run before the process exits or
	// its tmux client stays attached in the pod.
	terminalsEnded := make(chan bool, 1)
	go func() { terminalsEnded <- a.srv.EndTerminals(terminalDrainTimeout) }()
	drainCtx, cancel := context.WithTimeout(context.Background(), shutdownDrainTimeout)
	defer cancel()
	if err := a.http.Shutdown(drainCtx); err != nil {
		a.logger.Printf("drain did not complete within %s: %v", shutdownDrainTimeout, err)
	}
	if a.connect != nil {
		if err := a.connect.Shutdown(drainCtx); err != nil {
			a.logger.Printf("connect listener drain did not complete within %s: %v", shutdownDrainTimeout, err)
		}
	}
	if a.browser != nil {
		if err := a.browser.Shutdown(drainCtx); err != nil {
			a.logger.Printf("browser listener drain did not complete within %s: %v", shutdownDrainTimeout, err)
		}
	}
	if !<-terminalsEnded {
		a.logger.Printf("agent terminals did not finish releasing within %s; exiting anyway "+
			"(their tmux clients may stay attached in the agents' pods until those restart)", terminalDrainTimeout)
	}
	// The deliverer registers fan-outs too (its card-change broadcast), so it is
	// waited for BEFORE the fan-out set, for the same reason HTTP is.
	// The memo relay records a cut-off turn the same way, and is waited for
	// CONCURRENTLY under the same bound, so it adds nothing to the budget above.
	memoDone := make(chan struct{})
	go func() {
		defer close(memoDone)
		a.waitForMemoRelay(kickoffDrainTimeout)
	}()
	// Plugin deliveries record a cut-off turn the same way (EndStreams has
	// already cancelled them), under the same bound and concurrently.
	pluginsDone := make(chan struct{})
	go func() {
		defer close(pluginsDone)
		a.srv.WaitForPluginDeliveries()
	}()
	a.waitForKickoffs(kickoffDrainTimeout)
	<-memoDone
	select {
	case <-pluginsDone:
	case <-time.After(kickoffDrainTimeout):
		a.logger.Printf("plugin deliveries did not record their outcome within %s; exiting anyway", kickoffDrainTimeout)
	}

	done := make(chan struct{})
	go func() {
		a.srv.WaitForPushes()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(pushDrainTimeout):
		a.logger.Printf("notification fan-outs did not finish within %s; exiting anyway", pushDrainTimeout)
	}
}

// waitForKickoffs waits, bounded, for the kickoff deliverer to finish.
//
// 🔴 WITHOUT IT A FIRST TURN CANCELLED BY A REDEPLOY WAS LOST WITHOUT A TRACE.
// The deliverer's ctx is the signal context, so SIGTERM cancels an in-flight
// turn AFTER its row was stamped kicked_off; the deliverer then records the
// failure on a detached budget — but Run returned here, main's deferred Close
// shut the pool, and the process exited while that record was still in flight.
// The row then read as a delivered kickoff with no error: no "kickoff owed"
// badge (stamped) and no "kickoff failed" badge (nothing recorded). Pinned by
// TestShutdownWaitsForACancelledKickoffToBeRecorded.
//
// ⚠ IT IS AFTER THE HTTP DRAIN, NOT BEFORE: the two are independent, and the
// deliverer began stopping when ctx was cancelled, so by the time the drain is
// done its record has usually landed and this returns immediately.
func (a *app) waitForKickoffs(timeout time.Duration) {
	if a.kickoff == nil {
		return
	}
	select {
	case <-a.kickoff.Done():
	case <-time.After(timeout):
		a.logger.Printf("kickoff deliverer did not finish recording within %s; exiting anyway "+
			"(a cancelled first turn may be left stamped with no kickoff_error)", timeout)
	}
}

// Close releases what buildApp opened. Safe to call more than once.
func (a *app) Close() {
	if a.pool != nil {
		a.pool.Close()
		a.pool = nil
	}
}

// logBanner states every posture this server booted in, unconditionally and in
// BOTH directions.
//
// 🔴 A SILENT HEALTHY STATE AND A SILENT REFUSING STATE ARE OTHERWISE
// BYTE-IDENTICAL IN THE LOG. An operator debugging a 503 needs to know whether
// the door was ever armed, and "no line about it" answers neither question. Each
// line below is printed whether the thing is on or off.
func (a *app) logBanner(ext api.Extensions, port router.Port) {
	l := a.logger

	if a.pool != nil {
		l.Printf("persistence: Postgres (%s set); task and agent stores wired", envDatabase)
	} else {
		l.Printf("persistence: NONE (%s unset) — every data surface is empty. This is not a "+
			"supported deployment; config.validate refuses it in main()", envDatabase)
	}

	a.logAuthBanner()

	if api.AuthMode(a.cfg.AuthMode) == api.AuthModeGoTrue {
		l.Printf("human web tier: GOTRUE (%s=gotrue) — per-user sign-in; %s is NOT used for "+
			"sign-in and the shared-password cookie is ignored", envAuthMode, envUIPassword)
	} else if reason := a.cfg.browserAuthRefusal(); reason != "" {
		l.Printf("human web tier: REFUSED (fail-closed) — %s. Every browser surface answers "+
			"503 and /login says which variable is missing", reason)
	} else {
		l.Printf("human web tier: CONFIGURED — %s is set and long enough; the browser surface "+
			"gates on a session cookie this server signs", envUIPassword)
	}

	// 🔴 THE COOKIE FLAG IS A SEPARATE LINE FROM THE TIER ABOVE, BECAUSE IT IS A
	// SEPARATE FAILURE. "Human web tier: CONFIGURED" is about whether login
	// works; this is about whether the cookie login hands out is safe to carry
	// over the wire. It defaults FALSE — deliberately, because the common path
	// here is a plain-HTTP LAN port where a Secure cookie is never sent and login
	// would appear to succeed and then not stick — so a TLS deployment that never
	// sets it ships a session cookie with no Secure attribute, and nothing on
	// this banner said so. That is exactly the "silent healthy state and silent
	// refusing state are byte-identical" case this function's own header is about.
	if a.cfg.SecureCookies {
		l.Printf("session cookie: SECURE — %s is set, so the cookie is sent over HTTPS only. "+
			"A plain-HTTP LAN port will NOT be able to log in against this process",
			envSecureCookies)
	} else {
		gotrueNote := ""
		if api.AuthMode(a.cfg.AuthMode) == api.AuthModeGoTrue {
			gotrueNote = " (this is the PASSWORD-mode cookie, unused here: gotrue mode's " +
				"cookies are always Secure — see the auth mode line)"
		}
		l.Printf("session cookie: NOT Secure — %s is unset, which is the DEFAULT and is "+
			"correct for a plain-HTTP LAN port. ⚠ If this deployment is reached over TLS, "+
			"set it: without it the session cookie travels with no Secure attribute and any "+
			"plain-HTTP request to the same host carries it%s", envSecureCookies, gotrueNote)
	}

	if a.cfg.HookToken != "" {
		l.Printf("machine hook tier: ENFORCED — %s is set and required on the hook endpoints", envHookToken)
	} else {
		l.Printf("machine hook tier: OPEN — %s is unset, so the hook endpoints require NO "+
			"credential. That is enforce-when-set back-compat, not a default anyone chose; "+
			"the LAN port is included", envHookToken)
	}

	if reason := a.cfg.serviceWriteRefusal(); reason != "" {
		l.Printf("service-to-service tier: DISABLED (fail-closed) — %s. Those routes answer 503 "+
			"until it is armed; the other tiers are unaffected", reason)
	} else {
		l.Printf("service-to-service tier: CONFIGURED — %s is set and differs from every other "+
			"door's secret. ⚠ NOT YET KNOWN TO BE USABLE: a caller must also assert a legal "+
			"actor name or it is refused 403", envServiceToken)
	}

	switch {
	case port.Configured():
		// ⚠ IT NAMES THE VARIABLE AS WELL AS ITS VALUE, AND THE VARIABLE IS THE
		// PART THAT WAS MISSING. This branch printed the URL alone, so an operator
		// grepping the banner for MUSTER_ROUTER_URL found it in the failure
		// directions and NOT in the working one — the search that answers "did
		// this process see my variable at all" returned nothing precisely when
		// the answer was yes. Found by the both-directions ledger, not by review.
		l.Printf("permission router: CONFIGURED — %s=%s, actor %q — session liveness, event "+
			"publish, notifications and the directory picker are live (the "+
			"claude-code approval relay is NOT: see the approval relay gate line)",
			envRouterURL, a.cfg.RouterURL, a.cfg.RouterActor)
	case a.cfg.Standalone:
		l.Printf("permission router: NONE, DECLARED (%s=1) — session transcript links will all "+
			"read \"no transcript recorded\", which is TRUE here because nothing records "+
			"transcripts. Notifications and the directory picker degrade "+
			"and say so where a human can see it", envStandalone)
	// 🔴 THE PARTIAL CASE IS ITS OWN LINE, BECAUSE THE TWO-BRANCH VERSION NAMED
	// THE WRONG VARIABLE. router.New returns nil when base, token OR actor is
	// empty, so `port.Configured()` is false for all three — and the default
	// branch reported "MUSTER_ROUTER_URL is unset" at an operator who had set it
	// and forgotten the token. Measured live: URL set, token absent, banner
	// blamed the URL. It sends the reader to re-check the one variable that was
	// right, which is worse than saying nothing.
	case a.cfg.RouterURL != "":
		// ⚠ THE SUBJECT OF THE SECOND CLAUSE IS router.New, NOT THE ENVIRONMENT
		// VARIABLE. It read "MUSTER_ROUTER_URL builds nothing without them",
		// which names the variable as the builder — and this line's entire job is
		// to stop the reader blaming the variable that is already set correctly.
		// router.New is the function whose predicate the doc comment on
		// missingRouterCredential quotes, so it is the name a reader can go and
		// check.
		l.Printf("permission router: PARTIALLY CONFIGURED, THEREFORE NOT CONFIGURED — %s is "+
			"set (%s) but %s. A router client needs all three; router.New builds nothing "+
			"without them, so this server behaves exactly as if no router were configured "+
			"and will NOT report ready while a notes store is wired",
			envRouterURL, a.cfg.RouterURL, missingRouterCredential(a.cfg))
	default:
		l.Printf("permission router: NONE, UNDECLARED — %s is unset and %s is not 1. This "+
			"server will NOT report ready while a notes store is wired, on purpose: see "+
			"/readyz for the reason", envRouterURL, envStandalone)
	}

	// The claude-code approval relay's gate, on its own line: it is configurable
	// apart from the router (and beside MUSTER_STANDALONE), so "router: NONE" does
	// not answer whether prompts reach the operator's phone.
	if approvalGateFor(a.cfg) != nil {
		l.Printf("approval relay gate: DEDICATED — %s=%s (%s set), actor %q. Used ONLY by "+
			"/agent/approvals to file and poll agent-permission cards, which the router's "+
			"auto-approve may decide (auto-approve-ALL, or a window on the pod's own "+
			"project muster-agent/<name>); it turns on no router "+
			"behaviour (session liveness, events, notifications are unchanged)",
			envApprovalGateURL, a.cfg.ApprovalGateURL, envApprovalGateToken, a.cfg.RouterActor)
	} else {
		l.Printf("approval relay gate: NONE — %s/%s are unset, so /agent/approvals answers 503 "+
			"and every claude-code permission prompt falls back to the agent's own terminal "+
			"(a configured permission router is NOT used for this)", envApprovalGateURL, envApprovalGateToken)
	}

	// Push, on its own line: "is anyone's phone going to buzz" must be
	// answerable from the boot log.
	if ext.Push != nil {
		l.Printf("web push: ENABLED — %s/%s/%s are set (subject %q); devices subscribe on the session "+
			"tier and each notification reaches only its owner's devices", envVAPIDPublicKey, envVAPIDPrivateKey,
			envVAPIDSubject, a.cfg.VAPIDSubject)
	} else {
		l.Printf("web push: OFF — %s/%s/%s are unset (or there is no database), so no Enable button is drawn, "+
			"%s answers 503 and nothing is sent. Mint a keypair with `muster-server vapid generate`",
			envVAPIDPublicKey, envVAPIDPrivateKey, envVAPIDSubject, "/api/push/vapid-public-key")
	}

	if ext.Connectors != nil {
		listener := "no dedicated listener (" + envConnectPort + " unset): the connector routes are on the main port only"
		if a.cfg.ConnectPort != 0 {
			listener = fmt.Sprintf("%s=%d serves ONLY the agent-facing connector surface", envConnectPort, a.cfg.ConnectPort)
		}
		agentsTold := envAgentConnectURL + " is unset, so agents are not told where the proxy is and isolated agents get no network allowance to it"
		if a.cfg.AgentConnectURL != "" {
			agentsTold = fmt.Sprintf("%s=%s is handed to agents as MUSTER_CONNECT_URL", envAgentConnectURL, a.cfg.AgentConnectURL)
		}
		l.Printf("connector store: ENABLED — %s is set; secrets are sealed with AES-256-GCM and agents use them "+
			"only through the proxy. %s; %s", envConnectorEncKey, listener, agentsTold)
	} else {
		l.Printf("connector store: DISABLED — %s is unset, so every connector route answers 503, the "+
			"Connectors tab is hidden, and %s / %s have no effect", envConnectorEncKey, envConnectPort, envAgentConnectURL)
	}

	if ext.Mirror != nil {
		l.Printf("mirrored sessions: ENABLED (%s=1) — hosts running muster-mirror dial GET %s with a "+
			"per-host mirror token (mint one with `muster-server mirror token create <host>`), and their "+
			"tmux sessions are shown (in memory, to the token owner only) as Mirrored cards whose terminal attaches through that tunnel. "+
			"Typing into one needs a sign-in from the last %s in that same browser session (%s)",
			envMirror, mirror.ConnectPath, stepup.WindowText(), envStepUpWindow)
	} else {
		l.Printf("mirrored sessions: OFF — %s is unset (or there is no database), so GET %s answers 404, "+
			"no mirrored session is listed and the plugin's UI is not drawn", envMirror, mirror.ConnectPath)
	}

	switch {
	case ext.Plugins != nil && ext.PluginGateway != nil:
		l.Printf("plugins: ENABLED (%s=1) — owners mint plugin tokens on /plugins; a plugin POSTs messages to "+
			"/plugin/v1/messages and each is handed to its owner's chief as a fenced turn", envPlugins)
	case ext.Plugins != nil:
		l.Printf("WARNING — plugins: ENABLED (%s=1) but there is NO agent gateway (%s), so plugin messages are "+
			"stored and stay queued: nothing is delivered to any chief", envPlugins, envAgentGateway)
	default:
		l.Printf("plugins: OFF — %s is unset (or there is no database), so every /plugin/* route and /plugins "+
			"answer 404", envPlugins)
	}

	// The agent door to browser flows. The deployment's own names are
	// configuration, and their ABSENCE is a state an operator must be told
	// about: the screen then runs on its generic patterns alone. Non-fatal —
	// the owner's approval of the visible text is the control either way.
	if n := len(a.cfg.FlowAgentDenyWords); n > 0 {
		l.Printf("browser-flow agent screen: %d deployment name(s) from %s are refused in agent-composed "+
			"flow text, on top of the generic patterns (a heuristic; the owner's approval on /flows is the gate)",
			n, envFlowAgentDenyWords)
	} else {
		l.Printf("WARNING — browser-flow agent screen: %s is UNSET, so agent-composed flow text is screened by "+
			"the GENERIC patterns only (addresses, host names, token shapes) and NOT for this deployment's own "+
			"cluster/host/project names. Set it to a comma-separated list. The owner's approval on /flows is "+
			"still required for every run", envFlowAgentDenyWords)
	}
	a.logMemoRelayBanner(ext)
	a.logSTTBanner(ext)

	if ext.GitHub != nil {
		l.Printf("github connection: ENCRYPTED AT REST (%s set). %s",
			envGitHubEncKey, githubAuthPosture(a.cfg))
	} else {
		l.Printf("github connection: DISABLED — %s is unset, so there is nowhere to store a "+
			"token encrypted and the Repos tab has no account to connect", envGitHubEncKey)
	}

	// 🔴 THE SEAM, ANNOUNCED ON EVERY BOOT. See doc_seams.go.
	//
	// 🔴 THIS LINE USED TO SAY THE ROUTES "REFUSE", AND THAT WAS MEASURED FALSE
	// FOR NINE OF THEM. They answered 200 and did nothing: a dispatch rendered a
	// card over a row that sat `provisioning` for ever, a delete removed the card
	// and left the row, and the log stream sent `: connected` and closed, which
	// reads as "no logs". The refusal is real now and has a NAME — the sentence
	// cites the wrappers BY NAME so the claim can be checked against a symbol
	// rather than believed. Do not soften this back into an unqualified
	// "refuses"; that word is what stopped anyone looking.
	//
	// 🔴 AND IT NAMES TWO WRAPPERS, NOT ONE, BECAUSE THERE ARE TWO NILS. api's
	// combined Provisioner was split into Provisioner (lifecycle) and Gateway
	// (chat), each with its own wrapper, so that a deployment can wire one and have
	// the other keep refusing honestly. A banner that still named one wrapper would
	// be citing a symbol that does not exist — the exact "check it against a symbol"
	// property this line was rewritten to have.
	//
	// 🔴 AND IT IS NOW TWO SENTENCES ABOUT TWO NILS, WHICH IS THE STATE THE SPLIT
	// MADE REACHABLE. A single line describing "agent provisioning" as one thing
	// was true only while both halves were always unwired together. With a
	// lifecycle adapter wired and no gateway, a one-line banner has to pick which
	// half to describe — and either choice is a false claim about the other.
	switch {
	case ext.Provisioner == nil && ext.Gateway == nil:
		// ⚠ THE LINE NOW NAMES THE VARIABLE THAT TURNS IT ON, which is the other
		// half of the both-directions rule bannerLedger enforces: an operator
		// reading "UNWIRED" needs the name they can set, and it was previously
		// findable only by reading this file.
		//
		// ⚠ AND IT NAMES THE RUNTIME-CONFIG BUNDLE TOO, WHICH BRINGS THIS ARM'S
		// COUNT OF "none of these is a misconfiguration" VARIABLES TO FIVE. The pair is
		// REFUSED at boot in this state — config.validateProvisioner will not accept a
		// bundle with no named scheme, because there is then no credential derivation to
		// complete it with — so the only reachable value here is unset. That is exactly
		// why it has to be named: an operator who created the ConfigMap and projected
		// both keys but left MUSTER_AGENT_GATEWAY unset never reaches this line at all
		// (boot refuses), while one who does reach it is told the pair is inert rather
		// than left to infer it from its absence.
		// 🔴 AND IT NAMES *BOTH* VARIABLES, BECAUSE THIS BRANCH IS BOTH TIERS OFF.
		// There are two knobs now and this arm is the only place a reader sees the
		// fully-off state; naming one of them would send an operator who wants chat
		// to set the provisioner variable and conclude, correctly, that it did not
		// turn chat on.
		l.Printf("agent provisioning: UNWIRED (%s=%s, %s=%s) — no api.Provisioner and no "+
			"api.Gateway is wired, so every agent-control route (dispatch, "+
			"start, stop, destroy, logs, chat) is registered and answers 503 at request time "+
			"through api.requireLifecycleProvisioner or api.requireGatewayProvisioner, "+
			"after its own auth check and with provisionerUnwired:true in the body. "+
			"%s HAS NO EFFECT in this state — it is still "+
			"parsed and range-checked at boot, so a malformed value refuses to start, but "+
			"nothing is provisioned so no Service carries a gateway port. The agent "+
			"runtime-config bundle (%s, %s) is UNSET and must be: with no runtime named there "+
			"is no credential derivation to complete it with, so boot refuses the pair. None "+
			"of the four is a misconfiguration; see cmd/muster-server/doc_seams.go",
			envAgentProvisioner, a.cfg.agentProvisioner(), envAgentGateway, a.cfg.agentGateway(),
			envAgentGatewayPort,
			envAgentRuntimeConfig, envAgentRuntimeInstall)
	default:
		// 🔴 THE GATEWAY PORT IS REPORTED ON THE *LIFECYCLE* LINE, NOT ONLY ON THE CHAT
		// ONE, AND THAT IS WHERE IT BELONGS RATHER THAN WHERE IT IS CONVENIENT.
		// agentspec.Build declares the port UNCONDITIONALLY — see its constant's note —
		// so with a provisioner wired it shapes the Service and the `muster.dev/port`
		// annotation of every instance created from here on, whether or not chat is on.
		//
		// 🔴 IT WAS A SILENT STATE AND THE FIRST DRAFT EXEMPTED IT FROM THE BANNER
		// ENTIRELY, with a reason claiming "with no gateway named, nothing in this
		// process resolves an endpoint at all, so the CHAT: UNWIRED line has nothing to
		// say about the port". That is true about RESOLUTION and irrelevant to the
		// hazard, which is at CREATE time. Reachable, with no exotic input: an operator
		// sets MUSTER_AGENT_GATEWAY_PORT to the runtime's SKILLS port — one higher than
		// the gateway's, so an ordinary typo — while chat is off. Nothing says
		// anything. Every agent provisioned in that window gets a Service on the wrong
		// port, and arming chat later does NOT fix them, because Driver.Endpoint reads
		// the create-time annotation and not the current configuration. An exemption
		// documenting that would have been the silencer bannerExempt exists to prevent,
		// so the variable is ledgered and announced in both directions instead.
		//
		// 🔴 THE SERVICE SENTENCE IS QUALIFIED BY *DRIVER*, AND UNQUALIFIED IT WAS FALSE
		// FOR A SUPPORTED DEPLOYMENT. It read "Every instance created from here on gets a
		// Service on gateway port N … recorded in its pod-template annotation" and was
		// printed on BOTH driver arms — but provision.Noop creates no Service, no pod and
		// no annotation, and resolves from DefaultNoopEndpointTemplate instead. `noop` is
		// a deliberately-kept deployment and the banner fixture renders this very line
		// with it. The ledger guard cannot catch that: it checks the variable is NAMED in
		// both arms and that the two lines DIFFER — a words check over a state claim.
		//
		// ⚠ AND THE ANNOTATION PATH IS NOT RESTATED HERE ANY MORE. It said "pod-template
		// annotation", which is wrong — it is on the DEPLOYMENT. That fact now lives in
		// exactly one place, k8s.AnnotationPort's own comment, because it was open-coded
		// in six places and wrong in all six.
		// 🔴 AND THE NON-k8s ARM'S "instead" WAS FALSE IN THE SAME CLASS, ONE DRAFT
		// LATER. It read "…and an address resolves from the driver's own endpoint
		// template instead", which reads as "this variable is inert on noop". It is
		// not: provision.Noop.Endpoint passes spec.PortNumber(DefaultPortName) into
		// ResolveEndpoint and the template supplies only the HOST, so the port this
		// line prints is the port a turn would dial. Measured at three points —
		// 18789 -> http://probe.noop.invalid:18789, 29999 -> :29999, and port 0 is a
		// hard ErrNoEndpoint. So removing one false driver claim from this line left a
		// weaker one in the same place; the sentence now says what is true of noop.
		svcNote := "no Service and no port annotation is created by this driver — the " +
			"address is this driver's own template HOST with that port, so the value still " +
			"decides what a chat turn would dial"
		if a.cfg.agentProvisioner() == provisionerK8s {
			svcNote = "every instance created from here on gets a Service on that port, " +
				"recorded on the Deployment at CREATE time (see k8s.AnnotationPort) — " +
				"changing the variable later does not move an existing instance"
		}
		l.Printf("agent provisioning: LIFECYCLE %s (%s=%s) — dispatch, start, stop, destroy "+
			"and the log routes go through api.requireLifecycleProvisioner. Gateway port %d "+
			"(%s): %s",
			wiredWord(ext.Provisioner == nil), envAgentProvisioner, a.cfg.agentProvisioner(),
			a.cfg.agentGatewayPort(), envAgentGatewayPort, svcNote)
		// Agent kinds. The claude-code line names ACCOUNTS, never a token: those are
		// subscription credentials and a boot log is read by whoever reads logs.
		if a.cfg.claudeCodeEnabled() {
			l.Printf("agent kinds: %s (%s). %s: image %s (%s), Claude accounts [%s] (%s; tokens from %s<NAME>, "+
				"never logged), per-agent PVC at /data, no ServiceAccount token, turn budget %s",
				strings.Join(a.cfg.agentKinds(), ", "), envAgentKinds, agents.KindClaudeCode,
				a.cfg.AgentCCImage, envAgentCCImage, strings.Join(a.cfg.AgentCCAccountNames, ", "),
				envAgentCCAccounts, envAgentCCTokenPrefix, agents.ClaudeCodeTurnTimeout)
			// ⚠ "WRITTEN", NOT "ENFORCED". This process writes a NetworkPolicy object;
			// the cluster's network plugin is what enforces one, and nothing here can
			// observe that. The line says what a NEW or RE-APPLIED agent gets — an
			// agent created by an older build keeps running without a policy until
			// it is next applied, which for an operator means stop then start. It
			// also names the RBAC rule, because on this configuration DESTROYING ANY
			// agent needs it (doc_seams.go entry 5).
			if a.cfg.claudeCodeNetworkPolicy() {
				l.Printf("%s network: a NetworkPolicy is WRITTEN per agent before its pod, and the agent is "+
					"refused if it cannot be — ingress only from pods labelled [%s] (%s) in namespace %s (%s), "+
					"egress only DNS and TCP %d to public IPv4 addresses. Enforcement is the cluster network "+
					"plugin's, not this server's. That selector is what a policy written FROM NOW ON gets: an "+
					"agent created before this build has NO policy, and an existing policy keeps its old "+
					"selector, until that agent is stopped and started. This server's ServiceAccount needs "+
					"%s on networkpolicies.networking.k8s.io — to create a %s agent, and to DESTROY ANY agent",
					agents.KindClaudeCode, a.cfg.AgentNetpolFromLabels, envAgentNetpolFromLabels,
					a.cfg.AgentNetpolFromNS, envAgentNetpolFromNS, agentspec.ClaudeCodePublicEgressPort,
					strings.Join(networkPolicyVerbs(), "/"), agents.KindClaudeCode)
			} else {
				l.Printf("%s network: NOT CONFINED — %s=%s writes no NetworkPolicy (%s and %s apply to the %s "+
					"driver only)",
					agents.KindClaudeCode, envAgentProvisioner, a.cfg.agentProvisioner(),
					envAgentNetpolFromNS, envAgentNetpolFromLabels, provisionerK8s)
			}
		} else {
			l.Printf("agent kinds: %s only (%s unset or naming only it)", agents.KindGateway, envAgentKinds)
		}
		// 🔴 THE CHAT HALF GETS ITS OWN LINE AND NAMES ITS OWN WRAPPER, because
		// with lifecycle wired this is the half an operator will be surprised by.
		// A dispatched agent exists, its pod runs, its logs stream — and its first
		// turn never happens, because nothing can hand it the note. The line says
		// where that shows up (agents.kickoff_error) so the reader is not left to
		// infer it from a card that looks healthy.
		if ext.Gateway == nil {
			// ⚠ THE SENTENCE "no api.Gateway implementation exists in this module" WAS
			// TRUE AND IS NOT ANY MORE — internal/agentgateway is one. What makes this
			// branch reachable now is a deployment that did not NAME a runtime, so the
			// line says that instead: an operator reading UNWIRED needs the variable
			// they can set, which is the property bannerLedger enforces in both
			// directions.
			// 🔴 THIS LINE USED TO SAY A KICKOFF DISPATCH "CREATES THE INSTANCE AND
			// CANNOT DELIVER THE FIRST MESSAGE", AND THAT IS NO LONGER WHAT HAPPENS —
			// recorded here rather than silently reworded, because the old sentence is
			// what an operator debugging an older pod will remember. Such a dispatch is
			// now REFUSED before anything is built
			// (agentprovision.KickoffRefusalReason): the promise cannot be kept on this
			// configuration, so it is not made. Both field names stay, because both are
			// still where something is written — pending_note by the create handler, and
			// kickoff_error by a START, which is deliberately NOT refused (see
			// agentprovision.Adapter.Start on why refusing it would remove capability
			// that works).
			l.Printf("agent provisioning CHAT: UNWIRED (%s=%s) — no agent runtime is named, so "+
				"nothing is wired to api.Extensions.Gateway and the two chat routes answer 503 "+
				"through api.requireGatewayProvisioner with %s:true. A dispatch with a kickoff is "+
				"therefore REFUSED and provisions NOTHING — the row goes to `error` carrying the "+
				"reason and the remedy, the note stays in agents.pending_note, and the refusal "+
				"names %s as the variable to set. A START is still allowed and still records an "+
				"owed first turn in agents.kickoff_error. 🔴 AND NO AGENT RUNTIME-CONFIG BUNDLE IS "+
				"INSTALLED (%s and %s are unset, and boot requires them unset in this state): a "+
				"provisioned instance then runs the image's own entrypoint with NO configuration, "+
				"which is MEASURED to exit 78 with `Missing config` and crashloop reporting 0/1. So "+
				"this combination provisions pods that never serve — name a runtime to install one. "+
				"See cmd/muster-server/doc_seams.go entry 1",
				envAgentGateway, a.cfg.agentGateway(), api.ProvisionerUnwiredField, envAgentGateway,
				envAgentRuntimeConfig, envAgentRuntimeInstall)
		} else {
			// 🔴 IT NAMES THE RUNTIME, NOT JUST "WIRED", BECAUSE THE RUNTIME IS WHAT
			// DECIDES THE BEARER DERIVATION AND THE MODEL SENTINEL. Those are wire
			// contracts with the agent IMAGE, and both fail as a 401 or a 400 from
			// inside a turn — the two failures an operator is most likely to read as a
			// credential or model problem.
			//
			// 🔴 AND THE NAME IS READ OFF THE CONSTRUCTED GATEWAY, NOT OFF THE CONFIG.
			// An earlier revision printed a.cfg.agentGateway() while its own comment
			// claimed the line said "which formula this process is using" — a readback
			// of the REQUEST, not of the object, so a buildGateway that mapped a value
			// to the wrong Runtime would print the value the operator set and be wrong.
			// The type assertion is what makes this the object's own answer.
			//
			// ⚠ AND IT IS UNPINNED, STRUCTURALLY, WHICH IS WORTH SAYING SO A LATER
			// SIMPLIFICATION DOES NOT SILENTLY REVERT IT. With exactly one legal
			// non-none scheme the config value and the object's answer CANNOT disagree,
			// so deleting this assertion leaves the whole suite green — measured. It
			// becomes pinnable the moment a second Runtime exists — AND SO DOES THE
			// MAPPING, which an earlier wording said was already "covered by" the
			// gw.Runtime() assertion in
			// TestNamingARuntimeWiresAGatewayAndNotNamingOneWiresNothing. That assertion
			// is real but cannot distinguish a correct mapping from a wrong one while
			// only one scheme is legal: it conceded the unpinnability in its first half
			// and asserted coverage in its second. Both tests are owed by whoever adds
			// the second Runtime. ⚠ The name is on ONE line deliberately: splitting it
			// across a line break made internal/modulegate's citation gate read a test
			// that does not exist — the gate was right, and it caught this.
			scheme := a.cfg.agentGateway()
			if r, ok := ext.Gateway.(interface{ Runtime() string }); ok {
				scheme = r.Runtime()
			}
			// 🔴 "WIRED" IS NOT "REACHABLE", AND SAYING ONLY THE FIRST IS THE FALSEHOOD
			// THIS LINE SHIPPED IN REVIEW. BOTH of the blockers that sentence was about
			// are closed now, and the line is rewritten rather than deleted because what
			// replaces them is weaker evidence, not no gap:
			//
			//   - THE ADDRESS. It read "agentspec.Build declares no port and no endpoint,
			//     so this driver resolves no address … every such turn fails with
			//     <ErrNoEndpoint>". agentspec.Build now declares the gateway port
			//     (agentspec.DefaultGatewayPort, overridable by
			//     MUSTER_AGENT_GATEWAY_PORT), the kubernetes driver renders the Service,
			//     and Endpoint() resolves —
			//     TestAnAgentThisBinaryProvisionsResolvesAnEndpoint drives that through
			//     the real driver over a fake clientset.
			//   - THE CREDENTIAL. doc_seams entry 1 blocker (2): the bearer is
			//     sha256("gw-" + $HOOKS_TOKEN), read from the agent CONTAINER's own
			//     environment, and agentspec shipped the row's token only as
			//     MUSTER_HOOK_TOKEN. It now ships under agentspec.EnvGatewayToken too,
			//     and TestTheProvisionedContainerCanDeriveTheBearerMusterSends
			//     reproduces the container's derivation over the built spec and requires
			//     the two bearers to match.
			//
			// 🔴 SO WHY THE LINE STILL REFUSES TO SAY "REACHABLE": the container half of
			// that derivation is a shell command in the agent image's OWN deployment, in
			// another repository this module cannot read, and no turn has ever been made
			// against an instance THIS binary created. Two tests agreeing about bytes is
			// not a runtime accepting them — the Makefile's test-liveenv target is what
			// closes that, and it needs a reachable runtime. "NOT VERIFIED REACHABLE" is
			// the honest word and it is deliberately not the old one: claiming the fix
			// landed is a different claim from claiming it works.
			l.Printf("agent provisioning CHAT: WIRED %s=%s (%s=%s) — the two chat routes no longer "+
				"refuse at api.requireGatewayProvisioner. 🔴 WIRED IS NOT VERIFIED REACHABLE: both "+
				"in-repo blockers are closed — the spec declares port %d so the driver renders a "+
				"Service and resolves an address, and the row's token now ships as %s, the variable "+
				"this bearer is derived from — but the container half of that derivation lives in "+
				"the agent image's own deployment, which nothing here can read, so the first turn "+
				"against an agent this binary provisioned is still the measurement. A dispatch with a "+
				"kickoff is ACCEPTED: the kickoff deliverer (internal/agentkickoff, every %s) hands "+
				"each owned agent's pending note to its instance through this gateway once the "+
				"instance is ready, and records a failed or EMPTY first turn in agents.kickoff_error. "+
				"The agent runtime-config bundle IS INSTALLED: %s (%d bytes) is placed at %s, %s (%d bytes) "+
				"REPLACES the image's entrypoint as the container's command, and the derived "+
				"credential travels as %s. 🔴 THAT SCRIPT IS THE OPERATOR'S AND NOTHING HERE CAN "+
				"CHECK IT: if it does not install the file and re-exec the real entrypoint, the "+
				"instance does not serve — the startup probe is what reports that. "+
				"See cmd/muster-server/doc_seams.go entry 1",
				envAgentGateway, scheme, envAgentGatewayModel, a.cfg.AgentGatewayModel,
				a.cfg.agentGatewayPort(), agentspec.EnvGatewayToken, agentkickoff.DefaultInterval,
				envAgentRuntimeConfig, len(a.cfg.AgentRuntimeConfig), agentspec.RuntimeConfigPath,
				envAgentRuntimeInstall, len(a.cfg.AgentRuntimeInstall), agentspec.EnvGatewayBearer)
		}
	}
}

// wiredWord renders a nil-ness as the banner's vocabulary.
//
// ⚠ IT TAKES "IS IT NIL" RATHER THAN "IS IT WIRED" SO THE CALL SITE READS AS THE
// CHECK IT PERFORMS. The inverted form (`wiredWord(ext.Provisioner != nil)`) is
// one keystroke from its own opposite and the banner would then confidently
// report the state the server is not in — which is the failure this whole
// function exists to prevent.
func wiredWord(isNil bool) string {
	if isNil {
		return "UNWIRED"
	}
	return "WIRED"
}

// missingRouterCredential names the router variable(s) that are empty when the
// URL is set but router.New still built nothing.
//
// 🔴 IT NAMES WHAT IS ACTUALLY EMPTY RATHER THAN THE FIRST PLAUSIBLE CAUSE.
// router.New's predicate is `base == "" || token == "" || actor == ""`, and the
// banner that this replaces collapsed all three into one sentence blaming the
// URL. The whole value of the line is that the reader goes and sets the right
// variable, so the predicate here has to be the SAME one router.New applies —
// which is why it reads the fields rather than re-deriving a reason.
//
// ⚠ THE ACTOR ARM IS REACHABLE ONLY FROM A CONFIG THAT DID NOT COME FROM
// loadConfig, WHICH DEFAULTS IT TO defaultRouterActor. buildApp takes a config
// value, so a caller constructing one directly (every wiring test does) can
// still present an empty actor — and a banner that could not say so would be
// silent about the one case that reaches it.
func missingRouterCredential(c config) string {
	var missing []string
	if c.RouterToken == "" {
		missing = append(missing, envRouterToken)
	}
	if c.RouterActor == "" {
		missing = append(missing, envRouterActor)
	}
	switch len(missing) {
	case 0:
		// Not reachable through the branch that calls this — it is guarded by a
		// non-empty URL and a non-Configured port — but a silent "" here would
		// read as a complete sentence with the reason missing.
		return "the router client was not built even though every variable is set, " +
			"which should be impossible and is worth reporting"
	case 1:
		return missing[0] + " is unset"
	default:
		return strings.Join(missing, " and ") + " are unset"
	}
}

// githubAuthPosture names which of the two ways into a GitHub account is armed.
// Both may be: the static token connects at boot, the OAuth App lets an operator
// connect a different account from the Repos tab.
func githubAuthPosture(c config) string {
	switch {
	case c.GitHubToken != "" && c.GitHubClientID != "":
		return fmt.Sprintf("both %s and the OAuth App (%s) are configured", envGitHubToken, envGitHubClientID)
	case c.GitHubToken != "":
		return fmt.Sprintf("%s is configured; no OAuth App", envGitHubToken)
	case c.GitHubClientID != "":
		return fmt.Sprintf("the OAuth App (%s) is configured; no static token", envGitHubClientID)
	default:
		return fmt.Sprintf("no credential: neither %s nor %s is set, so no account can be connected",
			envGitHubToken, envGitHubClientID)
	}
}

// authConfig is the ONE place this binary's config becomes an api.AuthConfig.
//
// 🔴 A SECOND CONSTRUCTION SITE IS HOW THE BOOT BANNER STOPS DESCRIBING THE
// SERVER. buildApp and every refusal line below read the same value, so a field
// added to api.AuthConfig and wired in one place but not the other cannot make
// the banner report a posture the server is not in.
func (c config) authConfig() api.AuthConfig {
	return api.AuthConfig{
		HookToken:     c.HookToken,
		ServiceToken:  c.ServiceToken,
		UIPassword:    c.UIPassword,
		SecureCookies: c.SecureCookies,
		Mode:          api.AuthMode(c.AuthMode),
		PublicURL:     c.PublicURL,
	}
}

// goTrueAuth builds the GoTrue wiring from cfg over pool.
func goTrueAuth(cfg config, pool *pgxpool.Pool) api.GoTrueAuth {
	secret := []byte(cfg.GoTrueSecret)
	store := users.NewPG(pool)
	return api.GoTrueAuth{
		Client:   &gotrue.Client{BaseURL: cfg.GoTrueURL, Secret: secret, Issuer: gotrueIssuer},
		Verifier: gotrue.Verifier{Secret: secret, Issuer: gotrueIssuer, Audience: gotrueAudience},
		Users:    store,
		Accounts: store,
	}
}

// browserAuthRefusal reports why the human web tier will refuse, or "".
func (c config) browserAuthRefusal() string { return c.authConfig().BrowserAuthRefusal() }

// serviceWriteRefusal reports why the service-to-service tier will refuse, or "".
func (c config) serviceWriteRefusal() string { return c.authConfig().ServiceWriteRefusal() }

// connectStaticGitHub resolves the static token's login and stores it encrypted,
// so the Repos tab and agent dispatch work WITHOUT an OAuth App.
//
// 🔴 A FAILURE HERE IS LOGGED, NOT FATAL. The token may be expired or the
// network may be down at boot, and neither is a reason to refuse to serve a task
// board. The Repos tab shows "not connected", which is what is true.
//
// 🔴 IT ACTS AS THE OPERATOR (authz.Operator), BECAUSE THE STATIC TOKEN IS THE
// OPERATOR'S. There is no request to take a principal from, and row-level
// security (accounts PR 3a) scopes the write to whoever the context names: with
// none it would write nothing. NOT authz.System — that names no user, and this
// is one user's connection, not a sweep over everyone's.
func connectStaticGitHub(ctx context.Context, store github.Store, token string, logger *log.Logger) {
	ctx = authz.Operator(ctx)
	login, err := github.FetchLogin(ctx, token)
	if err != nil {
		logger.Printf("github: %s is set but its login could not be resolved (%v); no account connected",
			envGitHubToken, err)
		return
	}
	if err := store.Save(ctx, login, "", token); err != nil {
		logger.Printf("github: could not store the connection for %q: %v", login, err)
		return
	}
	logger.Printf("github: connected as %q from %s", login, envGitHubToken)
}
