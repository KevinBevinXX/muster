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
	"github.com/ZacxDev/muster/internal/github"
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
	// EventAgentStream carries ONE live event of an agent's kickoff turn.
	EventAgentStream = "agent.stream"
	// EventConnectorChanged announces a connector request, decision or change.
	EventConnectorChanged = "connector.changed"
)

// agentStreamPayload is the JSON Data of an EventAgentStream SSE event: the agent
// name (so the chat page filters to its own agent) plus the StreamEvent fields the
// browser renders — keyed the same way the user-turn WS keys them so the shared
// client renderer handles both. Empty fields are omitted to keep the payload small.
type agentStreamPayload struct {
	Agent      string `json:"agent"`
	Kind       string `json:"kind"`
	Text       string `json:"text,omitempty"`
	ToolID     string `json:"toolId,omitempty"`
	ToolName   string `json:"toolName,omitempty"`
	ToolArgs   string `json:"toolArgs,omitempty"`
	ToolOK     bool   `json:"toolOk,omitempty"`
	ToolOutput string `json:"toolOutput,omitempty"`
}

// Server is muster's HTTP application.
type Server struct {
	bus    *sse.Broadcaster
	logger *log.Logger
	auth   AuthConfig
	router RouterPort

	// streams ends every open SSE stream when shutdown begins. See EndStreams.
	streams streamEnd

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

	// terminals is every live agent terminal, so a shutdown can end them and
	// wait for their releases. See EndTerminals.
	terminals terminalSet

	// loginRate throttles password attempts on the human tier.
	loginRate rateLimiter

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

	// listPullRequests is the GitHub PR read, injectable for tests.
	listPullRequests func(ctx context.Context, token, repo, state string) ([]github.PullRequest, error)

	// --- debounced task notifications ---
	taskCreateAfter  afterFunc
	taskNotifyMu     sync.Mutex
	taskNotifyBuf    []createdTaskRef
	taskNotifyTimer  resettableTimer
	taskCommentAfter afterFunc
	taskCommentMu    sync.Mutex
	taskCommentBuf   []commentedTaskRef
	taskCommentTimer resettableTimer

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
	// ⚠ "Open" here means muster asks for no session. On the PUBLIC host the edge
	// still gates every one of these except the PWA metadata it deliberately
	// lets through (GET/HEAD /manifest.webmanifest, /static/icons/*.png, /sw.js —
	// see pwa.go for why, and for the rule that keeps those three free of
	// anything per-user).
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

	s.registerNotesRoutes(mux)
	s.registerGitHubRoutes(mux)
	s.registerAgentRoutes(mux)
	s.registerAgentSelfServiceRoutes(mux)
	s.registerConnectorRoutes(mux)
	s.registerApprovalRoutes(mux)
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
	return http.StripPrefix("/static/", http.FileServerFS(sub))
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
func (s *Server) shellFeatures() ui.Features {
	return ui.Features{GitHub: s.ext.GitHub != nil, Connectors: s.ext.Connectors != nil}
}

// handleIndex serves the document shell. The active tab is derived from the
// request path so each tab deep-links.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// /tasks?new=1 is the manifest's "New task" shortcut: the board with the
	// composer open. It is the same mechanism a share uses (ComposePage).
	if r.URL.Query().Get("new") == "1" && tabFromPath(r.URL.Path) == "tasks" {
		if err := ui.RenderComposePage(w, s.shellFeatures(), ""); err != nil {
			s.logger.Printf("error rendering composer page: %v", err)
		}
		return
	}
	if err := ui.RenderPage(w, tabFromPath(r.URL.Path), s.shellFeatures()); err != nil {
		s.logger.Printf("error rendering index page: %v", err)
	}
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

	id, ch := s.bus.Subscribe()
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
		s.broadcast(EventTaskChanged, strconv.FormatInt(n.ID, 10))
		reaped++
	}
	if reaped > 0 {
		s.logger.Printf("task-reap: tagged %d idle task(s) %s (untouched for >%s)", reaped, staleTaskTag, s.taskReapAfter)
	}
}

// --- broadcast helpers ------------------------------------------------------

// broadcast publishes one SSE event carrying an id.
//
// 🔴 IT ALSO MIRRORS ONTO THE ROUTER'S BUS WHEN ONE IS WIRED, so an operator
// with the router's page open sees muster's changes without a second
// EventSource. Best-effort by design: a failed mirror must never fail the write
// that caused it — a task was still created, and refusing it because a
// notification did not land would be strictly worse than a panel one poll stale.
func (s *Server) broadcast(name, id string) {
	data, _ := json.Marshal(map[string]any{"id": id})
	if s.bus != nil {
		s.bus.Broadcast(sse.Event{Name: name, Data: string(data)})
	}
	s.mirrorToRouter(name, string(data))
}

// pushBroadcastTimeout bounds one notification fan-out.
const pushBroadcastTimeout = 15 * time.Second

// goNotify is the ONE place a notification fan-out is spawned.
//
// 🔴 "ONE PLACE" IS ASSERTED, NOT DESCRIBED: TestNotifyFanOutHasOneCallSite
// fails if a second call site appears. Every notification helper in this package
// routes through it, so the registration on s.pushInFlight, the timeout and the
// "only log a delivery that reached someone" rule are each spelled once instead
// of once per caller.
//
// It runs the fan-out in its own goroutine — delivery latency must never block
// the handler that triggered it — but performs the Add SYNCHRONOUSLY, on the
// caller's goroutine. That ordering is the whole contract: a shutdown that
// called Wait between the goroutine starting and its Add would wait for nothing
// and return immediately, and the notification would be lost on exit.
//
// 🔴 THE FAN-OUT IS THE ROUTER'S NOW, AND THE FAILURE IS LOGGED RATHER THAN
// SWALLOWED. muster holds no push subscriptions — the browser subscribes
// against the router, which owns the VAPID key and the subscription table — so
// this is an HTTP call that can fail in ways an in-process fan-out could not.
// It is best-effort by contract at every call site, but a silent best-effort is
// indistinguishable from a working one, which is how upstream's own push
// pipeline was dead for months without anyone noticing.
//
// onDelivered, when non-nil, is called ONLY when the router accepted the
// notification. The device count is not available across the hop, so it is
// called with -1: an unknown count, spelled as one rather than as a plausible
// zero or one.
func (s *Server) goNotify(n RouterNotification, onDelivered func(int)) {
	if s.router == nil {
		return
	}
	// 🔴 THE Add IS ON THE CALLER'S GOROUTINE AND THE Done IS fn's FIRST DEFER,
	// AND safeGo DOES NOT DISTURB EITHER. safeGo's recover is registered OUTSIDE
	// fn, so on a panic fn's own defers unwind first — Done runs, then the
	// recover logs. A shutdown therefore cannot be left waiting on a fan-out that
	// panicked, which is the one property this pairing has to keep.
	//
	// ⚠ IT WAS BARE, AND s.router BEING NIL-CHECKED ABOVE IS NOT WHAT MADE THAT
	// SAFE — NOTHING DID. The nil check covers the field; the panic this recovers
	// is whatever happens INSIDE an HTTP call to another service, which is the
	// case safeGo's own doc names ("gateway responses"). A nil-map write or a
	// bad decode in that client would have taken the whole single-replica process
	// down from a best-effort notification.
	s.pushInFlight.Add(1)
	safeGo(s.logger, "router notify tag="+n.Tag, func() {
		defer s.pushInFlight.Done()
		ctx, cancel := context.WithTimeout(context.Background(), pushBroadcastTimeout)
		defer cancel()
		if err := s.router.Notify(ctx, n); err != nil {
			s.logger.Printf("router: notify tag=%s: %v", n.Tag, err)
			return
		}
		if onDelivered != nil {
			onDelivered(-1)
		}
	})
}

// mirrorToRouter forwards one event to the router's SSE bus, if configured.
func (s *Server) mirrorToRouter(name, data string) {
	if s.router == nil {
		return
	}
	// Same pairing, same reasoning, as goNotify above.
	s.pushInFlight.Add(1)
	safeGo(s.logger, "router publish "+name, func() {
		defer s.pushInFlight.Done()
		ctx, cancel := context.WithTimeout(context.Background(), pushBroadcastTimeout)
		defer cancel()
		if err := s.router.PublishEvent(ctx, name, data); err != nil {
			s.logger.Printf("router: publish %s: %v", name, err)
		}
	})
}

// BroadcastAgentChanged fires an SSE agent.changed nudge for the named agent so
// an open Tasks tab and the Agents tab refresh live. Best-effort + nil-safe: it
// is wired into the reconciler's status-change hook, which runs on a background
// goroutine, so it must never panic.
func (s *Server) BroadcastAgentChanged(agentName string) {
	if s == nil {
		return
	}
	s.broadcast(EventAgentChanged, agentName)
	// Ride the same status-change hook to fire the "agent ready" notification
	// ONCE when a task-linked agent first reaches `running`. It re-reads the
	// agent to see the new status + NoteID and dedupes internally.
	s.notifyAgentRunning(agentName)
}

// BroadcastChatReply fires an SSE chat.reply nudge for the named agent so an
// open chat page live-refreshes its #chat-log.
func (s *Server) BroadcastChatReply(agentName string) {
	if s == nil {
		return
	}
	s.broadcast(EventChatReply, agentName)
}

// BroadcastAgentStream fires an SSE agent.stream event carrying ONE live event
// of the named agent's kickoff turn, so an open chat page renders it live. The
// payload mirrors the user-turn WS fields so the browser uses one renderer.
func (s *Server) BroadcastAgentStream(agentName string, ev agents.StreamEvent) {
	if s == nil || s.bus == nil {
		return
	}
	data, err := json.Marshal(agentStreamPayload{
		Agent:      agentName,
		Kind:       ev.Kind,
		Text:       ev.Text,
		ToolID:     ev.ToolID,
		ToolName:   ev.ToolName,
		ToolArgs:   ev.ToolArgs,
		ToolOK:     ev.ToolOK,
		ToolOutput: ev.ToolOutput,
	})
	if err != nil {
		return
	}
	s.bus.Broadcast(sse.Event{Name: EventAgentStream, Data: string(data)})
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
