// Package api is muster's HTTP surface: the Server, its dependencies, and the
// single place a route may be registered.
//
// 🔴 ROUTE REGISTRATION IS UNCONDITIONAL, AND THAT IS A DELIBERATE DEPARTURE
// FROM THE CODE THIS WAS CARVED OUT OF.
//
// Upstream, roughly half the route table sits behind `if s.ext.X == nil {
// return }` guards, so the set of registered routes is a function of the
// FIXTURE as much as of the code. That is not a hypothetical cost: it left that
// project's route golden seven routes short, with two separate guards green
// over the hole, because the fixture the recorder used had one dependency
// unset. The golden then described the fixture and was read as describing
// production.
//
// Here, every route registers on every boot and each handler answers its own
// "this server has nothing to serve from" case — a 503 naming the missing
// dependency, or a rendered panel saying so on screen. Three things follow, and
// they are the reason this is worth the handful of extra nil checks:
//
//   - the route golden is a claim about the CODE. `RegisterRoutes` takes no
//     fixture and cannot be given one, so there is no configuration in which
//     the recording and production disagree. TestRegistrationIsIndependentOfDependencies
//     asserts exactly that, by recording from a bare server AND a fully-wired
//     one and demanding set equality.
//   - a no-database boot serves a UI that explains itself instead of a
//     navigation of 404s. A panel that says "no database is configured" is a
//     better first run than a tab whose partial 404s silently.
//   - "which routes exist" stops being a thing a reader has to simulate.
//
// The precedent is upstream's own: its seam and gate registrars are
// unconditional for this exact reason, and their comments say so.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/ZacxDev/muster/internal/askq"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/github"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/sendqueue"
	"github.com/ZacxDev/muster/internal/sse"
	"github.com/ZacxDev/muster/internal/ui"
	"github.com/ZacxDev/muster/web"
)

// --- SSE event names --------------------------------------------------------
//
// 🔴 THIS IS THE muster HALF OF A THIRTEEN-NAME BLOCK. The eight that are not
// here — request created/resolved, auto-approve, suggestion, attention, tmux,
// terminal-write and transcript — name changes to state this service does not
// hold. A browser watching muster's /events sees these and nothing else.
// (privilege.created was one of them until the privilege subsystem was removed.)
const (
	// EventTaskChanged nudges an open Tasks board to re-fetch a card.
	EventTaskChanged = "task.changed"
	// EventAgentChanged announces an agent status/identity change.
	EventAgentChanged = "agent.changed"
	// EventChatReply announces a persisted assistant turn.
	EventChatReply = "chat.reply"
	// EventConnectorChanged announces a connector request, decision or change.
	EventConnectorChanged = "connector.changed"
	// EventAttentionChanged announces that something entered or left the
	// owner's attention queue (attention.go). Spelled with "_queue" so it can
	// never be read as the router's own attention events on a mirrored bus.
	EventAttentionChanged = "attention_queue.changed"
)

// Server is muster's HTTP application.
type Server struct {
	bus    *sse.Broadcaster
	logger *log.Logger
	// asks is the question store (mirror_ask.go): the AskUserQuestion calls the
	// muster-mirror ask hook registered, shown in the rendered view while the
	// terminal shows its own picker, until the CLI's record resolves them.
	asks *askq.Store
	// askWatch is the running question watches (mirror_ask.go).
	askWatch askWatchers
	auth     AuthConfig
	router   RouterPort

	// streams ends every open SSE stream when shutdown begins. See EndStreams.
	streams streamEnd

	// inviteLimits throttles invite redemption and the password reset
	// (accounts PR 5; login_invite.go).
	inviteLimits inviteLimits

	// approvals is the claude-code approval relay's in-memory ledger: which agent
	// filed which request, and which agents report a blocked terminal. See
	// approvals.go.
	approvals approvalLedger
	// approvalGatePort is the queue the approval relay files through: a
	// standalone deployment may arm the relay without arming any router
	// behaviour. See UseApprovalGate.
	approvalGatePort GatePort

	startedAt time.Time
	now       func() time.Time

	// pushInFlight registers every outstanding notification fan-out so a
	// graceful shutdown can wait for them. Add happens on the CALLER's
	// goroutine; see goNotify.
	pushInFlight sync.WaitGroup
	// pluginInFlight registers every plugin message delivery started by the
	// message route (plugins.go), so shutdown waits for each to record its
	// outcome; pluginDeliverer is built by UseExtensions (nil: none wired).
	pluginInFlight  sync.WaitGroup
	pluginDeliverer *plugins.Deliverer
	// taskForwardTurn starts the turn a task_forward hands an agent
	// (task_routing.go); built by UseExtensions only when a gateway is wired
	// (nil: the forward is stored and no turn starts).
	taskForwardTurn func(ctx context.Context, a agents.Agent, session agents.ChatSession, text string)
	// answerTurn runs the turn an answer delivery pushes into the asking
	// agent's session (attention_delivery.go): runDeliveredTurn, set by
	// UseExtensions only when a gateway is wired (nil: the answer stays queued).
	answerTurn func(ctx context.Context, a agents.Agent, session agents.ChatSession, text string, emit agents.StreamEmit) ([]agents.TurnPart, error)
	// pluginRetrying holds the owners whose queued messages a claim is
	// retrying now (plugin_drafts.go), so concurrent claims start one retry;
	// draftWake wakes waiting claims when this process approves a draft.
	pluginRetrying sync.Map
	draftWake      draftSignal
	// routeKick wakes the route sweep (RunRouteTimeouts) early: a route just
	// ended owing its chief a hand-back.
	routeKick     chan struct{}
	routeKickOnce sync.Once
	// attention is the attention queue's process state (attention.go): the
	// waiters' wake-up and each owner's last-seen waiting sessions.
	attention attentionState
	// jobLanes runs one machine job per agent at a time (jobs.go); ownJobs is
	// the jobs this process accepted and has not finished; jobSecond
	// is a test seam for a job's timeout unit (zero: a real second); jobNow is
	// the reaper's clock (nil: time.Now); jobFinishBackoff is the pause between
	// outcome-write attempts (zero: jobFinishRetryPause).
	jobLanes         jobLanes
	ownJobs          ownJobs
	jobSecond        time.Duration
	jobNow           func() time.Time
	jobFinishBackoff time.Duration
	// mirrorMCPFails counts failed mirror-token checks on /mirror/mcp, per
	// (address, token hash) (mirror_mcp.go).
	mirrorMCPFails boundedWindow

	// terminals is every live agent terminal, so a shutdown can end them and
	// wait for their releases. See EndTerminals.
	terminals terminalSet
	// sends remembers delivered composer messages by nonce, so a send posted
	// twice is delivered once (composer.go).
	sends sendNonces
	// transcribe throttles the message box's microphone (transcribe.go).
	transcribe transcribeLimits
	// uploadLimits throttles the composer's attach button (uploads.go).
	uploadLimits uploadLimits

	// loginRate throttles password attempts on the human tier.
	loginRate rateLimiter
	// scrollRate throttles the Terminal view's drag-to-scroll, per user
	// (mirror_scroll.go).
	scrollRate keyedRate

	// gt is the GoTrue wiring (accounts PR 4); nil unless UseGoTrue was
	// called. See accounts.go.
	gt *gotrueState

	// agentCLIPath is where the agent-facing CLI binary is read from.
	agentCLIPath string

	// readyCheck is the readiness probe's dependency check (e.g. a DB ping).
	readyCheck func(context.Context) error

	// extDefects are wiring mistakes found at UseExtensions time that must not
	// be served with. See handleReady.
	extDefects []string

	ext Extensions

	// --- agent activity bookkeeping ---
	activeAgentsMu  sync.Mutex
	activeAgents    map[string]agentPhase
	activeAgentsNow func() time.Time
	// turns counts chat turns per agent and lets the chief replacement close
	// one (turn_gate.go).
	turns turnGate

	// sendQueue is the send queue's dispatcher (send_queue.go); nil until
	// RunSendQueue starts it.
	sendQueueMu sync.Mutex
	sendQueue   *sendqueue.Dispatcher
	// sendQueueClock is the queue's clock; nil is time.Now (a test sets it).
	sendQueueClock func() time.Time
	// focusRefresher runs session focus refreshes (focus.go); built once, on
	// first use, from the extensions.
	focusOnce      sync.Once
	focusRefresher *focus.Refresher

	// listPullRequests is the GitHub PR read, injectable for tests.
	listPullRequests func(ctx context.Context, token, repo, state string) ([]github.PullRequest, error)

	// --- debounced task notifications, one buffer PER OWNER (push_task.go) ---
	taskCreateAfter  afterFunc
	createdBuf       ownerBuffer[createdTaskRef]
	taskCommentAfter afterFunc
	commentBuf       ownerBuffer[commentedTaskRef]

	// pushLimit is the per-owner notification rate cap (push_routes.go).
	pushLimit ownerRateLimit

	taskProvisionedMu       sync.Mutex
	taskProvisionedNotified map[int64]bool

	taskDoneMu       sync.Mutex
	taskDoneNotified map[int64]bool

	// taskReapAfter is the idle window the stale-task reaper uses.
	taskReapAfter time.Duration

	// taskLeader is the cross-process singleton gate for the background loops.
	taskLeader LeaderGate

	// connectProxy is the connector proxy, built by UseExtensions when a
	// connector store is wired. See connectors.go.
	connectProxy *connectors.Proxy

	// principalProbe, when set, is called by admit (principal.go) with every
	// request it hands to a handler. TEST-ONLY: nothing in production sets it,
	// and it exists so principal_test.go can observe the principal at the
	// hand-off without running the handler.
	principalProbe func(*http.Request)

	// sessionUserOverride, when set, names the user a valid browser session
	// acts as (principal.go's sessionUserID). TEST-ONLY until PR 4's GoTrue
	// mode resolves the session's user for real.
	sessionUserOverride func(*http.Request) int64

	// droppedEventProbe, when set, is called with every event publish drops
	// for naming no owner. TEST-ONLY: the positive control for "no production
	// path broadcasts without an owner".
	droppedEventProbe func(sse.Event)
}

// LeaderGate is the cross-process singleton gate the background loops consult.
//
// 🔴 DECLARED HERE, CONSUMER-SIDE, RATHER THAN IMPORTED FROM THE DATABASE
// PACKAGE. The implementation is a Postgres session advisory lock, which means
// it belongs to whatever owns the pool — and this package must be buildable,
// and testable, without one. Upstream this was a concrete type from the db
// package and the coupling was invisible until the carve tried to move the
// loops without the pool.
type LeaderGate interface {
	// TryAcquire reports whether this process currently holds the lease.
	TryAcquire(ctx context.Context) bool
}

// RouterPort is everything muster asks of the permission router it was
// extracted from.
//
// 🔴 IT IS ONE INTERFACE, DECLARED CONSUMER-SIDE, SO "WHAT muster STILL NEEDS
// FROM THE OTHER SERVICE" IS A LIST A READER CAN COUNT. That is the number the
// extraction is judged on: every method here is a coupling that survived, and
// an interface that grows is the extraction going backwards. internal/router
// implements it over HTTP; a test implements it in memory.
//
// ⚠ A nil RouterPort IS A SUPPORTED DEPLOYMENT. muster runs standalone — the
// board, the agents, the connectors and the repos all work — and the four
// features below degrade, each saying so where a human can see it.
type RouterPort interface {
	SessionLivenessProbe
	// PublishEvent puts one event on the router's SSE bus.
	PublishEvent(ctx context.Context, name, data string) error
	// Notify fans a notification out to the router's push subscriptions.
	Notify(ctx context.Context, n RouterNotification) error
	// Directories returns distinct working directories the router has observed.
	Directories(ctx context.Context, query string, limit int) ([]string, error)
	// SessionMeta returns what the router knows about one session, and whether
	// it holds a record at all.
	//
	// 🔴 IT IS SEPARATE FROM SessionsExisting RATHER THAN A WIDER VERSION OF IT,
	// AND THE NARROWNESS OF THE OTHER ONE IS THE REASON. The decorator that
	// composes DetailAvailable wants ONE BIT per id and must not be able to reach
	// for a project, a cwd or a transcript — a port that can is a port a future
	// handler will use to render another service's data. This method serves the
	// WRITE path, which genuinely needs the project/cwd to denormalise onto the
	// link row, and it is a single lookup rather than a batch because its caller
	// links one session at a time.
	SessionMeta(ctx context.Context, sessionID string) (SessionMeta, bool, error)
}

// SessionMeta is what the router can say about one session.
type SessionMeta struct {
	SessionID string
	Project   string
	Cwd       string
	Host      string
}

// RouterNotification is the notification body RouterPort.Notify takes.
type RouterNotification struct {
	Type  string
	ID    string
	Title string
	Body  string
	Tag   string
	Data  map[string]string
}

// rateLimiter is a fixed-window counter. A token bucket would be smoother; a
// window is three fields and no goroutine, and the property that matters — a
// hard ceiling on attempts per minute — is the same.
//
// ⚠ IT IS GLOBAL, NOT PER-CALLER, AND THAT IS THE ORIGINAL BEHAVIOUR KEPT ON
// PURPOSE. A per-IP bucket sounds better and is worse here: the ceiling exists
// to bound guessing against ONE shared operator password, and an attacker with
// more than one source address would get the whole budget per address. The cost
// is that a guesser can lock a legitimate operator out for up to a window, on a
// service with one operator.
type rateLimiter struct {
	mu          sync.Mutex
	windowStart time.Time
	count       int
}

// allow reports whether one more attempt may proceed at now, and records it.
func (l *rateLimiter) allow(now time.Time, burst int, window time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.windowStart) >= window {
		l.windowStart = now
		l.count = 0
	}
	if l.count >= burst {
		return false
	}
	l.count++
	return true
}

// --- retention windows ------------------------------------------------------
const (
	// chatRetention bounds chat_messages (the agent web-chat transcript).
	chatRetention = 90 * 24 * time.Hour
	// connectorAuditRetention bounds connector_audit (proxied requests and grant
	// decisions).
	connectorAuditRetention = 90 * 24 * time.Hour

	// defaultTaskReapAfter is how long a task may sit untouched before the
	// sweep tags it stale.
	defaultTaskReapAfter = 7 * 24 * time.Hour

	// TaskReapInterval is how often the idle-task reaper sweeps.
	//
	// 🔴 IT IS NOT THE RETENTION INTERVAL, AND THE DIFFERENCE IS A FIXED BUG
	// RATHER THAN A PREFERENCE. The reap used to ride a 24h ticker, whose first
	// tick lands a full interval after boot — so whether it ran at all was
	// decided by whether the pod outlived a deploy, and it usually did not.
	TaskReapInterval = 30 * time.Minute
)

// staleTaskTag is the tag the idle-task reaper adds. It is a DESCRIPTIVE tag with
// no reserved namespace, so an operator can delete the chip from the Edit modal
// like any other; nothing in this codebase removes it automatically, and
// re-flagging deliberately does not depend on it — see notes.ReapCommentAuthor.
const staleTaskTag = "stale"

// New builds a Server. Attach dependencies with UseExtensions before Handler.
func New(bus *sse.Broadcaster, authCfg AuthConfig, logger *log.Logger) *Server {
	if logger == nil {
		logger = log.Default()
	}
	return &Server{
		asks:                    askq.New(),
		bus:                     bus,
		logger:                  logger,
		auth:                    authCfg,
		startedAt:               time.Now(),
		now:                     time.Now,
		agentCLIPath:            defaultAgentCLIPath,
		activeAgents:            make(map[string]agentPhase),
		activeAgentsNow:         time.Now,
		listPullRequests:        github.ListPullRequests,
		taskCreateAfter:         realAfterFunc,
		taskCommentAfter:        realAfterFunc,
		taskProvisionedNotified: make(map[int64]bool),
		taskDoneNotified:        make(map[int64]bool),
		taskReapAfter:           defaultTaskReapAfter,
	}
}

// UseRouter attaches the permission-router client. Call before Handler.
//
// ⚠ A nil port is the standalone deployment and is supported; see RouterPort.
func (s *Server) UseRouter(r RouterPort) { s.router = r }

// UseApprovalGate attaches the queue the claude-code approval relay
// (approvals.go) files cards through, and nothing else uses. A nil gate is
// supported: the relay answers 503 and every prompt falls back to the terminal.
func (s *Server) UseApprovalGate(g GatePort) { s.approvalGatePort = g }

// SetTaskReapAfter overrides the idle window the stale-task sweep uses.
func (s *Server) SetTaskReapAfter(d time.Duration) { s.taskReapAfter = d }

// SetReadyCheck installs the readiness probe's dependency check (e.g. a DB ping).
func (s *Server) SetReadyCheck(fn func(context.Context) error) { s.readyCheck = fn }

// SetTaskLeaderGate installs the cross-process singleton gate for the
// background loops: RunTaskRetention and RunTaskReap.
//
// A nil gate leaves the loops unguarded, which is the single-process dev path.
func (s *Server) SetTaskLeaderGate(g LeaderGate) { s.taskLeader = g }

// leadsTaskLoops reports whether this process may run a background loop body.
func (s *Server) leadsTaskLoops(ctx context.Context) bool {
	if s.taskLeader == nil {
		return true
	}
	return s.taskLeader.TryAcquire(ctx)
}

// Handler builds the http.Handler for the whole service.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.registerAll(mux)
	return s.withMiddleware(mux)
}

// --- route registration -----------------------------------------------------

// registerAll wires every muster route onto mux.
//
// 🔴 IT IS THE ONLY REGISTRATION SITE IN THE MODULE, AND THE ROUTE GATE RESTS
// ON THAT. RegisterRoutes (routes.go) calls exactly this, against a bare
// server; Handler calls it against the real one. A second registration site
// would make the golden a record of a subset while reading as a record of the
// whole — which is the failure the gate exists to prevent, so
// TestRoutePatternScannerCanSeeALiteralRoute derives the set of literal route
// patterns from the sources and checks it against what the recorder saw.
//
// 🔴 NO `if dep == nil { return }` MAY APPEAR IN THIS CALL TREE. See the
// package doc. TestRegistrationIsIndependentOfDependencies is the mechanical
// check, and it is a RELATIONSHIP guard — it records twice, from two different
// servers, and compares — not a count.
func (s *Server) registerAll(mux Mux) {
	// --- Open: no app auth. Health, metrics, assets, PWA. ---
	//
	// ⚠ "Open" here means muster asks for no session — and do not assume an edge
	// in front of it asks either. A deployment that relies on muster's own
	// sign-in (per-user accounts, invites) has no forward-auth gate: its edge may
	// refuse /metrics, /readyz, /mcp, /agent/ and /connect/ and rate-limit the
	// sign-in routes, but /health, /static/, /manifest.webmanifest and /sw.js
	// are then reachable by anyone. Keep every one of them free of anything
	// per-user (pwa.go).
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.Handle("GET /metrics", promhttp.Handler())
	mux.Handle("GET /static/", staticHandler())
	// PWA: served from root so the service worker's scope is "/" and the
	// manifest resolves relative to the document root.
	mux.HandleFunc("GET /sw.js", s.handleServiceWorker)
	mux.HandleFunc("GET /manifest.webmanifest", s.handleManifest)
	// Browser/PWA auto-fetches we do not serve an asset for: answer 204 (open,
	// no auth) so they stop generating 404 noise.
	mux.HandleFunc("GET /favicon.ico", handleNoContent)
	mux.HandleFunc("GET /apple-touch-icon.png", handleNoContent)
	mux.HandleFunc("GET /apple-touch-icon-precomposed.png", handleNoContent)

	// --- The human login tier: open by necessity, since it is how a caller
	// stops being anonymous. ---
	s.registerLoginRoutes(mux)
	// Accounts PR 5: the invitee's page, the password reset (open, like
	// login), and the admin pages (requireAdmin).
	s.registerInviteRoutes(mux)
	s.registerAdminRoutes(mux)
	// Personal API tokens (accounts PR 6): /api/whoami, /api/tokens, /account/tokens.
	s.registerAPITokenRoutes(mux)
	// The machine job API (jobs.go): the service tier only.
	s.registerJobRoutes(mux)

	// --- The SPA shell. Each tab is a real route (deep-linking + back/forward);
	// they all serve the shell with the matching tab active, derived from the
	// path. ---
	//
	// 🔴 THE SET IS DERIVED FROM ui.TabKeys, NOT TYPED OUT. Upstream kept a
	// hand-written run of HandleFunc calls beside a hand-written tab registry and
	// its own comment records the result: a "sixth tab registry" that no
	// structural test could see, where a tab present in the nav and absent from
	// the route table still rendered — with the wrong heading, from the fallback
	// branch, silently. Deriving the routes from the same slice the nav is
	// derived from removes the failure mode instead of testing for it.
	mux.HandleFunc("GET /{$}", s.requireSession(s.handleIndex))
	for _, tab := range ui.TabKeys() {
		mux.HandleFunc("GET /"+tab, s.requireSession(s.handleIndex))
	}
	mux.HandleFunc("GET /events", s.requireSession(s.handleEvents))
	// The manifest's share_target. Session-gated like the shell it renders; a
	// GET, so a share that meets the login page survives the round trip in
	// ?next=. It writes nothing — see handleShare.
	mux.HandleFunc("GET /share", s.requireSession(s.handleShare))
	// The desktop app badge's count. Session-gated: it is task data.
	mux.HandleFunc("GET /ui/tasks/review-count", s.requireSession(s.handleReviewCount))
	mux.HandleFunc("GET /ui/chief/panel", s.requireSession(s.handleChiefPanel))
	// 🔴 THREAD SEARCH IS requireSession AND ONLY requireSession. It returns
	// MESSAGE CONTENT — an excerpt cut out of a chat message body — so no machine
	// credential may reach it, however many neighbouring routes those credentials
	// open. TestChiefThreadSearchIsOperatorOnly asserts the refusals.
	mux.HandleFunc("GET /ui/chief/threads/search", s.requireSession(s.handleChiefThreadSearch))
	// chief is the reserved-name agent; nothing else can create one.
	mux.HandleFunc("POST /chief/provision", s.requireSession(s.requireLifecycleProvisioner(s.handleChiefProvision)))
	// Replacing the chief (chief_replace.go): delete + re-create on another kind,
	// behind an explicit confirm. requireArmedHookToken: a machine write that
	// destroys an agent must not be served open on an unarmed server; a personal
	// token acts as its owner.
	mux.HandleFunc("POST /api/chief/replace", s.requireArmedHookToken(s.requireLifecycleProvisioner(s.handleChiefReplace)))

	s.registerNotesRoutes(mux)
	s.registerGitHubRoutes(mux)
	s.registerAgentRoutes(mux)
	// Pins (internal/pins): the Agents list's pin toggles, 404 without a store.
	s.registerPinRoutes(mux)
	// The send queue (internal/sendqueue): Queue, its list, Cancel; 404 without a store.
	s.registerSendQueueRoutes(mux)
	// Focus history (internal/focus): one page of a session's past focus; 404 without a store.
	s.registerFocusHistoryRoutes(mux)
	s.registerAgentSelfServiceRoutes(mux)
	s.registerConnectorRoutes(mux)
	s.registerCredentialRoutes(mux)
	s.registerApprovalRoutes(mux)
	s.registerMirrorRoutes(mux)
	// Browser flows (internal/flows): /flows, /api/flows/jobs, /runner/flows/*.
	s.registerFlowRoutes(mux)
	// Plugins (internal/plugins): /plugins and /plugin/v1/*, 404 while off.
	s.registerPluginRoutes(mux)
	// The attention queue (attention.go): /attention, its list and its badge; 404 without a store.
	s.registerAttentionRoutes(mux)
	// The mirrored sessions' MCP door (mirror_mcp.go): a host's token, its own sessions; 404 without the mirror plugin.
	s.registerMirrorMCPRoutes(mux)
	// muster-native Web Push (push_routes.go): the subscribe flow and the
	// account page's Notifications section.
	s.registerPushRoutes(mux)
	// Dictation (transcribe.go): the message box's microphone.
	s.registerTranscribeRoutes(mux)
	// File attachments (uploads.go): the attach button, and the signed link.
	s.registerUploadRoutes(mux)
}

// registerAgentSelfServiceRoutes wires the agent callback API. These are
// authenticated by the calling agent's unique hooks token (not a session), so an
// agent pod can read/comment/advance its task.
func (s *Server) registerAgentSelfServiceRoutes(mux Mux) {
	mux.HandleFunc("GET /agent/muster", s.requireAgentToken(s.handleAgentCLIDownload))
	mux.HandleFunc("GET /agent/task", s.requireAgentToken(s.handleAgentTask))
	mux.HandleFunc("POST /agent/task/comment", s.requireAgentToken(s.handleAgentTaskComment))
	mux.HandleFunc("PATCH /agent/task/status", s.requireAgentToken(s.handleAgentTaskStatus))
}

// --- open handlers ----------------------------------------------------------

func handleNoContent(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// staticHandler serves the embedded web/static directory under /static/.
func staticHandler() http.Handler {
	sub, err := fs.Sub(web.Static, "static")
	if err != nil {
		// web.Static always contains "static"; an error here is a programming
		// bug, not a runtime condition.
		panic("api: embedded static fs missing 'static' dir: " + err.Error())
	}
	files := http.StripPrefix("/static/", http.FileServerFS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", staticCacheControl(r.URL.Path, r.URL.Query().Get("v")))
		files.ServeHTTP(w, r)
	})
}

// staticCacheControl is the Cache-Control for a /static/ request.
//
// 🔴 ONLY A URL CARRYING THE FILE'S CURRENT HASH IS CACHED FOR LONG. Pages link
// web.Asset's /static/<file>?v=<content hash>: that URL names these exact
// bytes, so it is immutable. Anything else — the bare path, or a ?v= from an
// older build — is "no-cache" (stored, but revalidated): with no header at
// all, the edge applied its own max-age=14400 and served 0.43.0 the previous
// release's app.css for hours.
func staticCacheControl(path, v string) string {
	if h := web.AssetHash(path); h != "" && v == h {
		return "public, max-age=31536000, immutable"
	}
	return "no-cache"
}

// shellFeatures says which optional subsystems this server holds, for the
// documents that draw a navigation.
//
// 🔴 IT IS THE ONE DERIVATION, AND EVERY DOCUMENT GOES THROUGH IT. Four handlers
// render a document carrying the sidebar; deciding the tab set at each of them
// would be four copies of one predicate, which is the shape that ends up wrong at
// three of the four in the same direction. It reads the SAME field the handler
// guards on (Extensions.GitHub), so the page and the route cannot disagree about
// whether a subsystem exists.
//
// ⚠ IT IS NOT A READINESS OR PERMISSION CHECK. A tab hidden here is a surface the
// build does not have, not one this caller may not see — every route stays
// registered and answers for itself (see internal/api/routes.go and github.go).
//
// Admin (accounts PR 5) draws the sidebar's Admin link: the request's
// principal is an admin AND the admin pages have a store to serve. It is a
// LINK only — requireAdmin is what refuses a member, whatever the nav drew.
func (s *Server) shellFeatures(ctx context.Context) ui.Features {
	p, _ := authz.From(ctx)
	return ui.Features{GitHub: s.ext.GitHub != nil || s.githubApp(ctx) != nil, Connectors: s.ext.Connectors != nil,
		Admin: p.Role == authz.RoleAdmin && s.adminPagesServed(), Push: s.pushServed(), Attention: s.ext.Attention != nil}
}

// handleIndex serves the document shell. The active tab is derived from the
// request path so each tab deep-links.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// /tasks?new=1 is the manifest's "New task" shortcut: the board with the
	// composer open. It is the same mechanism a share uses (ComposePage).
	if r.URL.Query().Get("new") == "1" && tabFromPath(r.URL.Path) == "tasks" {
		if err := ui.RenderComposePage(w, s.shellFeatures(r.Context()), ""); err != nil {
			s.logger.Printf("error rendering composer page: %v", err)
		}
		return
	}
	tab := tabFromPath(r.URL.Path)
	if err := ui.RenderPageWith(w, tab, s.shellFeatures(r.Context()), s.shellData(r.Context(), tab)); err != nil {
		s.logger.Printf("error rendering index page: %v", err)
	}
}

// shellData is what the shell draws on its first paint (ui.ShellData): on the
// Agents tab, the "Recently viewed" strip, so it is not filled in above the list
// a moment later and push everything below it down. Any other tab draws its
// Agents panel hidden, where a late strip moves nothing on screen.
//
// 🔴 IT IS NOT THE LIST'S BUILD, AND MUST NOT BE. agentsListCards asks the
// provisioner for live instances (a Kubernetes read) and reads every agent's
// last message; the shell's first byte waited on both, and GET /agents then
// built the whole list a second time for /ui/agents. The strip needs only
// which agents exist, what they are called and which are pinned — one store
// read — plus the mirror hub's cards, which are in memory, and their hosts'
// icons, which are one mirror_tokens read (mirror.Service.HostRanks, uncached;
// none when the viewer has no hosts). The live status arrives with the list's
// own load, which MORPHS the strip out of band moments later
// (ui.recentStrip): an icon swap that moves nothing.
// TestTheAgentsShellReadsNoInstances counts the provisioner calls.
//
// ⚠ A FAILED READ DRAWS THE OLD WAY (an empty strip the list's own load fills),
// not an error: the list's load reports its own failure.
func (s *Server) shellData(ctx context.Context, tab string) ui.ShellData {
	if tab != "agents" || s.ext.Agents == nil {
		return ui.ShellData{}
	}
	list, err := s.ext.Agents.List(ctx)
	if err != nil {
		s.logger.Printf("agents: shell recents: %v", err)
		return ui.ShellData{}
	}
	pinned := s.pinSet(ctx)
	cards := make([]ui.AgentCardView, 0, len(list))
	notChief := func(agents.Agent) bool { return false }
	for _, a := range list {
		// The ONE card composer (cardViewIndexed; TestOneCardComposerFeedsEverySurface)
		// with a NIL instance index: liveStatusIndexed then answers the STORED
		// status, with no provisioner read on the document's path
		// (TestTheAgentsShellReadsNoInstances). The list's own load replaces the
		// strip with the live one; a stored status that has since moved changes
		// colour in the same 12px box, which moves nothing. Chief and focus are
		// left zero: the strip draws neither.
		cards = append(cards, s.cardViewIndexed(a, nil, time.Time{}, notChief, pinned, nil))
	}
	// No focus index: the strip draws no focus (ui/recents.go), and reading it
	// would be a second store read on the shell's first byte.
	return ui.ShellData{}.WithRecent(cards, s.mirrorCards(ctx, pinned, nil), s.recentRefs(ctx))
}

// tabFromPath maps a request path to a tab key.
//
// 🔴 IT DOES NOT ENUMERATE THE TABS. It takes the first path segment and lets
// the UI clamp it, so this function cannot fall out of step with the tab
// registry — the upstream "sixth registry" defect. An unknown segment clamps to
// the default tab, which is also what an unrouted path would have done.
func tabFromPath(p string) string {
	seg := strings.TrimPrefix(p, "/")
	if i := strings.IndexByte(seg, '/'); i >= 0 {
		seg = seg[:i]
	}
	return seg
}

// handleHealth is the LIVENESS probe: the process is up and serving. It does NOT
// touch the database, so a slow/down database never triggers a restart loop.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	uptime := s.now().Sub(s.startedAt).Seconds()
	s.writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": BuildVersion,
		"uptime":  uptime,
	})
}

// handleReady is the READINESS probe: serve traffic only if the backing
// dependencies are reachable AND the wiring is sound.
//
// 🔴 THE WIRING CHECK IS NOT DECORATION, AND IT IS THE THING THAT MAKES
// "FORGET TO COMPOSE SESSION LIVENESS" LOUD. A Server whose notes store was
// never wrapped serves perfectly: every page renders, nothing errors, and every
// transcript row says "no transcript recorded" over transcripts that are alive.
// There is no request that fails, so there is nothing for a probe to catch —
// unless the probe asks about the wiring itself. It does, here, and a defect
// keeps the pod out of rotation rather than putting a wrong sentence in front
// of an operator. See UseExtensions.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if len(s.extDefects) > 0 {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "not ready",
			"error":  "server wiring is incomplete",
			"detail": s.extDefects,
		})
		return
	}
	if s.readyCheck != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.readyCheck(ctx); err != nil {
			s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"status": "not ready", "error": "database unreachable"})
			return
		}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"status": "ready", "version": BuildVersion})
}

// --- PWA assets: see pwa.go (the manifest, sw.js, /share, the badge count) ---

// handleEvents is muster's own SSE stream.
//
// 🔴 muster SERVES ITS OWN /events RATHER THAN PROXYING THE ROUTER'S, AND THE
// ROUTE IS MARKED `shared` IN THE PARTITION MANIFEST FOR THAT REASON. A browser
// on muster's origin cannot open an EventSource against another origin without
// CORS, and a page that renders muster's board must see muster's task.changed
// events. The router keeps its own copy for its own events; the two streams
// carry disjoint event names.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	if s.bus == nil {
		http.Error(w, "this server has no event bus", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// 🔴 THE STREAM IS THE SESSION USER'S, AND ONLY THEIRS (accounts PR 3b).
	// requireSession attached the principal; the bus delivers this subscriber
	// only the events its user owns. A principal-less or scope-all request
	// cannot reach here (requireSession admits a user), and would subscribe as
	// nobody (0) if it did.
	id, ch := s.bus.Subscribe(requestOwner(r.Context()))
	defer s.bus.Unsubscribe(id)

	// Initial comment so clients know the stream is open and proxies flush.
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	ctx, cancel := s.streamContext(r.Context())
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, open := <-ch:
			if !open {
				return
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Name, ev.Data)
			flusher.Flush()
		}
	}
}

// --- background loops -------------------------------------------------------

// RunTaskRetention drives the daily retention sweep until ctx is cancelled:
// each tick deletes connector audit entries and agent chat messages older than
// their windows, then reaps idle tasks.
//
// 🔴 A FOLLOWER SKIPS THE TICK AND CONTINUES — never `return`. A follower that
// exited would be permanently sweep-less for the rest of the process's life and
// would look identical to a healthy one.
func (s *Server) RunTaskRetention(ctx context.Context, interval time.Duration) {
	// A background loop acts for every user (accounts PR 2/6); see internal/authz.
	ctx = authz.System(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !s.leadsTaskLoops(ctx) {
				continue
			}
			s.taskRetentionPass(ctx)
		}
	}
}

// taskRetentionPass runs one retention sweep over muster's domains — connector
// audit entries, agent chat messages — and then reaps idle tasks.
//
// 🔴 THE SINGLE-SNAPSHOT PROPERTY IS PRESERVED WITHIN THIS PASS. It takes one
// timestamp and keys every domain's cutoff on it. No cutoff here was ever
// compared against one in the router's half, and reapIdleTasks' `now` is only
// used for its own idle-window arithmetic, so the two services' clocks have
// nothing to disagree about.
//
// Ordering: the deletes, then the reap. (Runbook runs and resolved privilege
// requests were swept here too until both subsystems were removed.)
func (s *Server) taskRetentionPass(ctx context.Context) {
	now := s.now()
	if s.ext.Connectors != nil {
		if n, err := s.ext.Connectors.PruneAudit(ctx, now.Add(-connectorAuditRetention)); err != nil {
			s.logger.Printf("retention: delete old connector audit entries: %v", err)
		} else if n > 0 {
			s.logger.Printf("retention: deleted %d connector audit entr(ies) older than %s", n, connectorAuditRetention)
		}
	}
	if s.ext.Agents != nil {
		if n, err := s.ext.Agents.DeleteChatMessagesOlderThan(ctx, now.Add(-chatRetention)); err != nil {
			s.logger.Printf("retention: delete old chat messages: %v", err)
		} else if n > 0 {
			s.logger.Printf("retention: deleted %d chat message(s) older than %s", n, chatRetention)
		}
	}
	s.reapIdleTasks(ctx, now)
}

// RunTaskReap drives the idle-task reaper until ctx is cancelled.
//
// 🔴 It is a SEPARATE loop from RunTaskRetention, and that is a fixed bug rather
// than a preference — see TaskReapInterval.
func (s *Server) RunTaskReap(ctx context.Context, interval time.Duration) {
	// A background loop acts for every user (accounts PR 2/6); see internal/authz.
	ctx = authz.System(ctx)
	// 🔴 Announce at ENTRY, unconditionally. The pass is silent when it flags
	// nothing, so without this line a process whose reaper was never wired and one
	// sweeping a board with no idle tasks emit BYTE-IDENTICAL logs. That
	// indistinguishability is what let the ticker defect hide for the whole of the
	// feature's production life upstream.
	s.logger.Printf("task reaper: sweeping every %s for tasks idle for >%s", interval, s.taskReapAfter)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !s.leadsTaskLoops(ctx) {
				continue
			}
			s.reapIdleTasks(ctx, s.now())
		}
	}
}

// reapIdleTasks tags tasks untouched for longer than taskReapAfter as `stale`
// and posts one system comment, so an idle task is visible but flagged rather
// than silently destroyed. A non-positive taskReapAfter disables it.
//
// 🔴 IT MUST NOT WRITE notes.updated_at, WHICH IS THE FIELD IT SELECTS ON. The
// whole reap is delegated to a single Store.FlagIdle call for that reason: a
// previous implementation called AddTags then AddComment, and BOTH write
// `updated_at = now()`, so every sweep reset the idle clock on everything it
// touched (self-perpetuating re-reaps) and, because the list came back
// `updated_at DESC` and the loop bumped in that order, sorted the MOST idle task
// HIGHEST. Do not "simplify" this back into AddTags+AddComment.
//
// 🔴 ListSummaries, NOT List. This loop reads exactly two fields — n.ID and
// n.UpdatedAt — and List hydrates every attachment, EVERY COMMENT OF EVERY NOTE
// and every session link on the way. At this interval that unused fan-out would
// be paid ~48 times a day over a set that only grows.
func (s *Server) reapIdleTasks(ctx context.Context, now time.Time) {
	if s.ext.Notes == nil || s.taskReapAfter <= 0 {
		return
	}
	all, err := s.ext.Notes.ListSummaries(ctx)
	if err != nil {
		s.logger.Printf("task-reap: list tasks: %v", err)
		return
	}
	cutoff := now.Add(-s.taskReapAfter)
	body := fmt.Sprintf("Task idle for >%s — tagged **%s** by the retention sweep.", s.taskReapAfter, staleTaskTag)
	reaped := 0
	for _, n := range all {
		if !n.UpdatedAt.Before(cutoff) {
			continue
		}
		ok, err := s.ext.Notes.FlagIdle(ctx, n.ID, staleTaskTag, body)
		if err != nil {
			s.logger.Printf("task-reap: flag idle task %d: %v", n.ID, err)
			continue
		}
		if !ok {
			continue
		}
		s.broadcastTo(n.OwnerID, EventTaskChanged, strconv.FormatInt(n.ID, 10))
		reaped++
	}
	if reaped > 0 {
		s.logger.Printf("task-reap: tagged %d idle task(s) %s (untouched for >%s)", reaped, staleTaskTag, s.taskReapAfter)
	}
}

// --- broadcast helpers ------------------------------------------------------

// broadcast publishes one SSE event carrying an id, OWNED BY THE USER THE
// REQUEST ACTS AS (ctx's principal).
//
// 🔴 EVERY EVENT HAS AN OWNER AND REACHES ONLY THAT USER (accounts PR 3b). The
// bus used to fan every event to every open stream, which with a second user
// is a cross-user leak with no handler bug (task ids, agent names, connector
// grant ids). The classification of every call site:
//
//   - request handlers (session, hook/service, agent, connect, relay doors):
//     broadcast(ctx, …). Under row-level security a request can only have
//     touched its OWN principal's rows, so the principal's user IS the owner
//     of the task / agent / connector / grant the event names.
//   - the idle-task reaper (authz.System): broadcastTo(n.OwnerID, …) — the
//     task row's owner, read with it (notes.ListSummaries).
//   - the agent-named nudge handed only a NAME, by the loops' OnChange hooks
//     (BroadcastAgentChanged): agentChangedFor(s.agentOwner(name), …).
//   - the mirror plugin's host nudge (BroadcastAgentsChangedFor): the host's
//     owner, named by the hub.
//
// None is global: there is no "every user" event, so there is no path that
// delivers to everyone. A zero owner (a System context reaching broadcast, or
// an agent name that no longer resolves) is DROPPED and logged — fail closed
// — never sent to all. TestEveryBroadcastCallSiteIsClassified pins the list.
func (s *Server) broadcast(ctx context.Context, name, id string) {
	s.broadcastTo(requestOwner(ctx), name, id)
}

// requestOwner is the user a request-scoped event belongs to: the principal's
// user, or 0 ("nobody") for a scope-all or principal-less context.
func requestOwner(ctx context.Context) int64 {
	p, ok := authz.From(ctx)
	if !ok || p.All() {
		return 0
	}
	return p.UserID
}

// broadcastTo publishes one SSE event carrying an id to owner's streams.
func (s *Server) broadcastTo(owner int64, name, id string) {
	data, _ := json.Marshal(map[string]any{"id": id})
	s.publish(sse.Event{Name: name, Data: string(data), OwnerID: owner})
}

// publish is the ONE place an event leaves for the bus and the router mirror.
//
// 🔴 IT ALSO MIRRORS ONTO THE ROUTER'S BUS WHEN ONE IS WIRED, so an operator
// with the router's page open sees muster's changes without a second
// EventSource — BUT ONLY AN ADMIN'S EVENTS. The router is the operator's own
// service (the same one the approval relay files cards on, which takes only
// admin-owned agents — operator decision), and its bus has no notion of a
// muster user, so a member's event mirrored there would be the very leak the
// owner filter closes. Best-effort by design: a failed mirror must never fail
// the write that caused it.
func (s *Server) publish(ev sse.Event) {
	if ev.OwnerID <= 0 {
		s.logger.Printf("sse: dropped %s %s: it names no owner (fail closed; see broadcast)", ev.Name, ev.Data)
		if s.droppedEventProbe != nil {
			s.droppedEventProbe(ev)
		}
		return
	}
	if s.bus != nil {
		s.bus.Broadcast(ev)
	}
	s.mirrorToRouter(ev.OwnerID, ev.Name, ev.Data)
}

// agentOwner is the owner of the agent named name, or 0 when it does not
// resolve. It reads under authz.System — the nudges it serves are handed only
// a name, by background loops (no principal) as well as handlers — and hands
// nothing on but the id. System ledger entry.
func (s *Server) agentOwner(name string) int64 {
	if s.ext.Agents == nil {
		return 0
	}
	a, err := s.ext.Agents.GetByName(authz.System(context.Background()), name)
	if err != nil {
		return 0
	}
	return a.OwnerID
}

// pushBroadcastTimeout bounds one notification fan-out.
const pushBroadcastTimeout = 15 * time.Second

// goNotify is the ONE place a notification fan-out is spawned.
//
// 🔴 "ONE PLACE" IS ASSERTED, NOT DESCRIBED: TestNotifyFanOutHasOneCallSite
// fails if a second call site of router.Notify appears, and
// TestNativePushHasOneSendSite pins PushService.SendTo to sendPush, whose
// only callers are this function and the account page's "send test". Every
// notification helper in this package routes through here, so the
// registration on s.pushInFlight, the timeout, the owner's preferences, the
// rate cap and the "only log a delivery that reached someone" rule are each
// spelled once instead of once per caller.
//
// 🔴 EVERY NOTIFICATION HAS ONE OWNER, AND ONLY THAT OWNER'S DEVICES ARE ASKED.
// owner is the user the event is ABOUT (the task's, the agent's, the
// connector request's); muster-native push delivers to that owner's own
// subscriptions (push.Service.SendTo → the store's owner-filtered read), and
// the router — the operator's service, with no notion of a muster user — is
// reached only for an admin owner (routerServes), as before. event is the
// /account toggle that can mute it (push.Events).
//
// It runs the fan-out in its own goroutine — delivery latency must never block
// the handler that triggered it — but performs the Add SYNCHRONOUSLY, on the
// caller's goroutine. That ordering is the whole contract: a shutdown that
// called Wait between the goroutine starting and its Add would wait for nothing
// and return immediately, and the notification would be lost on exit.
//
// The router hop is logged on failure rather than swallowed: a silent
// best-effort is indistinguishable from a working one, which is how upstream's
// own push pipeline was dead for months without anyone noticing.
//
// onDelivered, when non-nil, is called when a delivery reached someone: with
// the device count for muster-native push, and with -1 (an unknown count,
// spelled as one rather than as a plausible zero or one) for the router.
func (s *Server) goNotify(owner int64, event string, n RouterNotification, onDelivered func(int)) {
	if !s.notifiable() || owner <= 0 {
		return
	}
	// 🔴 THE Add IS ON THE CALLER'S GOROUTINE AND THE Done IS fn's FIRST DEFER,
	// AND safeGo DOES NOT DISTURB EITHER. safeGo's recover is registered OUTSIDE
	// fn, so on a panic fn's own defers unwind first — Done runs, then the
	// recover logs. A shutdown therefore cannot be left waiting on a fan-out that
	// panicked, which is the one property this pairing has to keep.
	s.pushInFlight.Add(1)
	safeGo(s.logger, "notify tag="+n.Tag, func() {
		defer s.pushInFlight.Done()
		ctx, cancel := context.WithTimeout(context.Background(), pushBroadcastTimeout)
		defer cancel()
		if s.pushServed() && s.pushAllowed(ctx, owner, event) {
			rep := s.sendPush(ctx, owner, push.Payload{Type: n.Type, ID: n.ID, Title: n.Title, Body: n.Body, Tag: n.Tag, Data: n.Data})
			if rep.Delivered > 0 && onDelivered != nil {
				onDelivered(rep.Delivered)
			}
		}
		// The router knows nothing of "resolved" (it closes its own cards).
		// Nor of the attention queue's items: it has no attention card, and its
		// own notifications for a waiting session are its own (attention.go).
		if s.router == nil || event == push.EventResolved || event == push.EventAttention || !s.routerServes(owner) {
			return
		}
		if err := s.router.Notify(ctx, n); err != nil {
			s.logger.Printf("router: notify tag=%s: %v", n.Tag, err)
			return
		}
		if onDelivered != nil {
			onDelivered(-1)
		}
	})
}

// notifiable reports whether ANY notification can be delivered: muster's own
// push, or the router. Every notify helper returns early when it cannot, so a
// deployment with neither does no buffering, no timers and no reads.
func (s *Server) notifiable() bool { return s.router != nil || s.pushServed() }

// mirrorToRouter forwards one of owner's events to the router's SSE bus, if
// configured and owner is an admin (see publish).
func (s *Server) mirrorToRouter(owner int64, name, data string) {
	if s.router == nil {
		return
	}
	// Same pairing, same reasoning, as goNotify above.
	s.pushInFlight.Add(1)
	safeGo(s.logger, "router publish "+name, func() {
		defer s.pushInFlight.Done()
		ctx, cancel := context.WithTimeout(context.Background(), pushBroadcastTimeout)
		defer cancel()
		if s.roleOf(ctx, owner) != authz.RoleAdmin {
			return
		}
		if err := s.router.PublishEvent(ctx, name, data); err != nil {
			s.logger.Printf("router: publish %s: %v", name, err)
		}
	})
}

// BroadcastAgentChanged fires an SSE agent.changed nudge for the named agent so
// an open Tasks tab and the Agents tab refresh live. Best-effort + nil-safe: it
// is wired into the reconciler's status-change hook, which runs on a background
// goroutine, so it must never panic.
//
// It is handed only a NAME (the loops' OnChange hooks), so the owner the
// nudge reaches is read from the row (agentOwner). A request that has the
// agent in hand calls agentChangedFor with the request's owner instead.
func (s *Server) BroadcastAgentChanged(agentName string) {
	if s == nil {
		return
	}
	s.agentChangedFor(s.agentOwner(agentName), agentName)
}

// BroadcastAgentsChangedFor nudges owner's open Agents views to re-fetch, with
// no agent named — the mirror plugin's host connected / dropped / re-listed
// hook, which knows the host's owner and nothing else.
func (s *Server) BroadcastAgentsChangedFor(owner int64) {
	if s == nil {
		return
	}
	s.broadcastTo(owner, EventAgentChanged, "")
	// The same nudge is how the attention queue learns a mirrored session
	// started (or stopped) waiting on the person.
	s.attentionMirrorChanged(owner)
}

// agentChangedFor is BroadcastAgentChanged for an owner already known.
func (s *Server) agentChangedFor(owner int64, agentName string) {
	s.broadcastTo(owner, EventAgentChanged, agentName)
	// Ride the same status-change hook to fire the "agent ready" notification
	// ONCE when a task-linked agent first reaches `running`. It re-reads the
	// agent to see the new status + NoteID and dedupes internally.
	s.notifyAgentRunning(agentName)
}

// WaitForPushes blocks until every in-flight notification fan-out has finished.
// Call during graceful shutdown.
func (s *Server) WaitForPushes() { s.pushInFlight.Wait() }

// --- small helpers ----------------------------------------------------------

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.logger.Printf("error writing JSON response: %v", err)
	}
}

func jsonQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// truncate shortens s to at most n runes, appending an ellipsis when cut.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// hasPrefix reports whether s begins with prefix, ignoring any parameters after
// a ';' (e.g. "application/json; charset=utf-8").
func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
