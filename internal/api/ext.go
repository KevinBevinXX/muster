package api

import (
	"context"
	"github.com/ZacxDev/muster/internal/users"
	"io"
	"net/http"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/apitokens"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/flows"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/github"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/pins"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/recents"
	"github.com/ZacxDev/muster/internal/sendqueue"
)

// Extensions are the server's optional dependencies. A Server built without
// them still boots, serves the shell, answers /health and /readyz, and refuses
// every data route with a reason — which is the behaviour a first run against
// no database should have.
//
// 🔴 THIS STRUCT IS THE muster HALF OF A STRUCT THAT USED TO HOLD EIGHTEEN
// FIELDS, AND THE NINE THAT ARE MISSING ARE MISSING ON PURPOSE. Upstream it
// also carried Suggest, Attention, Tmux, Transcripts, TranscriptArchive,
// TermWrite, Layout, AutoApprove and GenSuggestions — every one of which backs
// a surface on the permission router's side of the route partition. Carrying
// them across would give this service nine dependencies it has no route for,
// and would make the extraction's central claim — that the two halves are
// separable — false in the type system while looking true in the diff.
//
// 🔴 `Suggest` IS THE ONE WHOSE ABSENCE IS LOAD-BEARING RATHER THAN TIDY, AND
// IT MUST NOT COME BACK. It is the store that owns the session table, and the
// whole of the upstream work that preceded this carve existed to remove that
// table from the domain layer so the domain layer could move without it. The
// bit it used to supply — SessionLink.DetailAvailable — is now composed from a
// SEAM instead; see UseExtensions and session_liveness.go. A reviewer who sees
// a `Suggest` field reappear here should read that as the extraction being
// undone, not as a convenience.
type Extensions struct {
	Notes  notes.Store
	Agents agents.Store
	GitHub github.Store
	// Users answers a user's role for the request Principal (principal.go's
	// roleOf). Nil: user 1 is admin and everyone else a member.
	Users users.Store
	// APITokens is the personal API token store (accounts PR 6;
	// api_tokens.go). Nil: no personal token is admitted anywhere (503).
	APITokens apitokens.Store

	// AgentNamespacePrefix is the prefix [Extensions.AgentNamespace] builds a new
	// agent's stored namespace from. It is the DEPLOYMENT's prefix — the same
	// MUSTER_AGENT_NAMESPACE_PREFIX the provisioning driver is configured with —
	// and NOT a default this package chooses.
	//
	// 🔴 IT IS HERE BECAUSE A HARDCODED CONSTANT WAS SHIPPED IN ITS PLACE AND
	// DISAGREED WITH THE DRIVER, SILENTLY. The row-writing handler used
	// agents.NamespaceFor(name) over agents.NamespacePrefix ("devpod-") while the
	// driver created `muster-agent-<name>`. Nothing fails when those differ: the
	// value is recorded, served on GET /api/agents and labelled onto the
	// instance's objects, and NOTHING places anything with it — so a card names a
	// namespace that holds nothing and reads as "never provisioned" while the
	// instance runs one namespace over. Measured live.
	//
	// 🔴 EMPTY IS NOT A SUPPORTED PRODUCTION STATE WHEN AGENTS CAN BE CREATED, and
	// defects() says so rather than this comment: an unset prefix beside a wired
	// Agents store and Provisioner means the wiring was FORGOTTEN, and the
	// consequence is the defect above rather than a missing feature. It resolves
	// to agents.NamespacePrefix so a test fixture need not set it, which is
	// exactly why the omission has to be caught somewhere that is not a reader's
	// attention.
	AgentNamespacePrefix string

	// SessionLiveness answers "does a transcript record for this session still
	// exist" — the one bit SessionLink.DetailAvailable is made of.
	//
	// 🔴 IT IS A PORT, NOT A STORE, BECAUSE THE DATA IS NOT THIS SERVICE'S. The
	// session table stays with the permission router. Nil is TOLERATED and is not
	// a supported production state: the wrapper still applies, but every link
	// resolves to false and the three-state transcript row collapses to two. See
	// withSessionLiveness for why that is tolerated rather than fatal, and
	// TestExtensionsWiringPairsNotesWithALivenessProbe for what stops it shipping.
	SessionLiveness SessionLivenessProbe

	// TagAutoDispatch arms the `auto:dispatch` routing tag
	// (MUSTER_TAG_AUTODISPATCH). It ships FALSE and implements PLUMBING ONLY
	// (recognition + eligibility + a log line + the metric) — no hands-off
	// dispatch behaviour. Rationale: the MANUAL dispatch loop has never closed
	// end-to-end, and automating a path that has never once worked by hand
	// converts an unproven feature into an unattended one. While false,
	// `auto:dispatch` behaves as a descriptive tag.
	TagAutoDispatch bool

	// Provisioner drives agent instance LIFECYCLE (provision/start/stop/delete,
	// state, logs). Nil disables the seven lifecycle routes even if Agents is set,
	// through requireLifecycleProvisioner.
	Provisioner Provisioner

	// Gateway drives agent CHAT. Nil disables the two chat routes even if
	// Provisioner is set, through requireGatewayProvisioner.
	//
	// 🔴 IT IS A SEPARATE FIELD FROM Provisioner ON PURPOSE — see [Gateway]. The
	// two nils are independent, so a deployment that can provision but not chat
	// renders truthfully instead of having to choose which lie to tell.
	Gateway Gateway

	// Kinds answers which agent kinds a dispatch may ask for and, for the
	// claude-code kind, which Claude account it runs on. Nil means the gateway
	// kind alone — every deployment before kinds existed — and the dispatch form
	// shows no picker.
	Kinds AgentKinds

	// GitHubOAuth configures the OAuth web flow for connecting an account.
	GitHubOAuth GitHubOAuthConfig

	// Push is muster's own Web Push (internal/push; push_routes.go). nil: no
	// VAPID keys are configured — no Enable button is drawn, the key route
	// answers 503, and nothing is ever sent.
	Push PushService

	// Transcriber turns a recording into text for the message box's microphone
	// (transcribe.go; internal/stt). nil: no speech-to-text service is
	// configured (MUSTER_STT_TOKEN unset) — no microphone is drawn on any
	// message box and POST /transcribe answers 404.
	Transcriber Transcriber

	// Credentials is the per-owner model-provider credential store behind
	// Account → Credentials (internal/credentials). nil: none configured — the
	// page says so, and every member then has no credential (never the
	// admin's). See credentials.go.
	Credentials CredentialStore

	// Connectors is the secure connector store. nil means the feature is off
	// (no MUSTER_CONNECTOR_ENCRYPTION_KEY): every connector route answers 503 and
	// the tab is not drawn. See internal/connectors and connectors.go.
	Connectors connectors.Store
	// ConnectorPolicy is the proxy's upstream network policy. The zero value is
	// production's strict one; see connectors.Policy.
	ConnectorPolicy connectors.Policy

	// Mirror is the optional MIRRORED-sessions plugin (internal/mirror). nil
	// means the plugin is off (MUSTER_MIRROR unset), which is the default: GET
	// /mirror/connect and the mirror terminal routes answer 404 and the Agents
	// view draws no mirrored card. See mirror.go.
	Mirror Mirror

	// Flows is the browser-flow queue and its runner tokens (internal/flows,
	// migration 0012). nil (no database): every flow route answers 503 and
	// /flows says so.
	Flows flows.Store

	// Plugins is the plugin interface's store (internal/plugins, migration
	// 0015). nil means plugins are OFF (MUSTER_PLUGINS unset), which is the
	// default: every /plugin/* route and /plugins answer 404. See plugins.go.
	Plugins plugins.Store
	// PluginGateway resolves the owner's chief before a plugin message is handed
	// to it (agentgateway.Gateway satisfies it); the turn itself is chatTurn's,
	// over Gateway, so the chief has its tools on it. nil (or a nil Gateway) with
	// Plugins set: messages are stored and stay queued — nothing is delivered.
	PluginGateway plugins.Gateway

	// Pins is the Agents list's per-user pins (internal/pins, migration 0017).
	// nil (no database): nothing is pinned, no card is drawn pinned, and the pin
	// routes answer 404.
	Pins pins.Store

	// Recents is the Agents list's per-user "recently viewed" entries
	// (internal/recents, migration 0018). nil (no database): no visit is
	// recorded and the strip is never drawn.
	Recents recents.Store

	// SendQueue is the server-side send queue (internal/sendqueue, migration
	// 0019). nil (no database): no composer draws Queue, the queue routes answer
	// 404, and RunSendQueue does nothing.
	SendQueue SendQueue
	// SendQueueIdle is how long a target must have been idle before a queued
	// message is sent to it (MUSTER_SEND_QUEUE_IDLE); 0 is
	// sendqueue.DefaultIdleFor.
	SendQueueIdle time.Duration

	// Focus is session focus (internal/focus, migration 0020): what each of a
	// user's sessions is working on. nil (no database): no card shows a focus
	// and the focus tools are not offered.
	Focus focus.Store
	// FocusSummarizer computes a focus from a transcript tail (the ScaleDown
	// summarizer through OpenRouter) — focus's only writer. nil: no focus is
	// ever written; RunFocusRefresh does nothing and focus_refresh says why.
	FocusSummarizer focus.Summarizer
	// FocusRetention is how long a mirrored session's focus outlives the last
	// time a pass saw it (MUSTER_FOCUS_RETENTION); 0 keeps it forever.
	FocusRetention time.Duration

	// Routes is session routing's store (internal/plugins/routes.go, migration
	// 0021) and Routing its configuration. Routing nil — MUSTER_SESSION_ROUTING
	// unset — is OFF: the chief's route_to_session tool is not offered,
	// POST /api/routes/{id}/reply answers 404, and the chief's plugin turns do
	// not mention routing.
	Routes  plugins.RouteStore
	Routing *Routing

	// Attention is the attention queue's explicit asks (internal/attention,
	// migration 0025). nil (no database): the attention tools are not offered,
	// /attention and /api/attention answer 404, and no link is drawn.
	Attention attention.Store
}

// SendQueue is the send queue's store as this package consumes it:
// *sendqueue.PGStore. The first six run under the request's principal;
// CancelOwner and the embedded DispatchStore are the system scope's.
type SendQueue interface {
	Enqueue(ctx context.Context, it sendqueue.Item) (sendqueue.Item, error)
	List(ctx context.Context, t sendqueue.Target) ([]sendqueue.Item, error)
	Get(ctx context.Context, id int64) (sendqueue.Item, error)
	Cancel(ctx context.Context, id int64, now time.Time) (sendqueue.Item, error)
	HeadOf(ctx context.Context, t sendqueue.Target) (sendqueue.Head, bool, error)
	NotePaste(ctx context.Context, t sendqueue.Target, since int64) error
	// CancelOwner is the system scope's: a disable or a sign-out everywhere
	// cancels the user's queued messages.
	CancelOwner(ctx context.Context, owner int64, now time.Time, detail string) (int, error)
	sendqueue.DispatchStore
}

// Routing is session routing's configuration (MUSTER_SESSION_ROUTING=1).
type Routing struct {
	// Allow is the per-owner sender allowlist whose routed answers are relayed
	// WITHOUT the owner's approval (MUSTER_ROUTE_RELAY_ALLOW); everyone else's
	// fall back to a pending draft.
	Allow plugins.RelayAllowlist
	// Timeout is how long a routed message waits for its session's answer
	// before it is handed back to the chief (0: DefaultRouteTimeout).
	Timeout time.Duration
}

// Mirror is the mirror plugin as this package consumes it. *mirror.Service
// implements it. Hosts and Attach act for the principal on ctx: another user's
// hosts are not in Hosts and not attachable.
type Mirror interface {
	// ServeConnect is GET /mirror/connect: a host's muster-mirror dialling in.
	ServeConnect(w http.ResponseWriter, r *http.Request)
	// Hosts is the viewing principal's hosts and their sessions.
	Hosts(ctx context.Context) []mirror.HostView
	// 🔴 EACH OF THE THREE BELOW TAKES A WINDOW: the tmux window id of the entry
	// (mirror.Window.ID), or "" for the session's ACTIVE window — the only
	// address a per-session host has, and what a URL with no `window` means.
	// The plugin resolves "" to an explicit window on a per-window host.

	// AttachWindow attaches a terminal to one window of one of the viewing
	// principal's host sessions, with Provisioner.AttachTerminal's shape so one
	// terminal bridge serves both.
	AttachWindow(ctx context.Context, host, session, window string, readOnly bool, stdin io.Reader, stdout io.Writer, resize <-chan provision.TermSize) error
	// WatchWindow streams the transcript of the Claude Code session in that
	// window's pane (the rendered view), resuming at (gen, offset) when it can,
	// until ctx ends.
	WatchWindow(ctx context.Context, host, session, window, gen string, offset int64, out func(mirror.Msg) error) error
	// PasteWindow types a message into that window's pane and presses Enter:
	// its Claude pane only, or its active pane whatever runs there (target).
	// The caller has already required the step-up; the plugin requires it
	// again. opts: mirror.ExpectTurn (a queued message: refused as
	// superseded if the window's turn moved) and mirror.ReportWindow (the
	// window id "" resolved to).
	PasteWindow(ctx context.Context, host, session, window, text string, target mirror.PasteTarget, opts ...mirror.PasteOption) error
	// AgentState is whether the Claude Code session in that window is busy,
	// idle or waiting, and since when — or why it is unknown (an older
	// muster-mirror reports none). What the send queue waits on.
	AgentState(ctx context.Context, host, session, window string) mirror.AgentState
	// ScrollWindow scrolls that window's active pane through its history in
	// tmux copy-mode by lines (positive is back). No step-up: see
	// mirror.Hub.ScrollWindow.
	ScrollWindow(ctx context.Context, host, session, window string, lines int) error
}

// GitHubOAuthConfig holds the OAuth App credentials + the public base URL used
// to build the redirect URI.
type GitHubOAuthConfig struct {
	ClientID     string
	ClientSecret string
	// BaseURL is the externally reachable origin used to construct the callback
	// URL. If empty the callback is derived from the inbound request host.
	BaseURL string
	// StateKey MACs the OAuth `state` (github.OAuthStateKey, derived from the
	// store's encryption key). Empty: no state is minted and every callback is
	// refused — the flow never runs on an unkeyed MAC.
	StateKey []byte
}

// Configured reports whether the OAuth App credentials are present.
func (c GitHubOAuthConfig) Configured() bool {
	return c.ClientID != "" && c.ClientSecret != ""
}

// UseExtensions attaches the dependencies. Call before Handler.
//
// 🔴 IT WRAPS Notes, AND THAT WRAP IS THE WHOLE REASON THIS FUNCTION EXISTS
// RATHER THAN A PLAIN FIELD ASSIGNMENT. internal/notes does not join the
// session table — it cannot, the table is another service's — so every
// SessionLink it returns has DetailAvailable=false until something composes it.
// This is the ONE place that happens. Doing it in handlers instead would be
// twenty-odd places to forget, and forgetting is SILENT: false renders the
// words "no transcript recorded" over a live transcript, with nothing logged
// and nothing 404ing. Upstream that exact conflation disguised a months-long
// outage as ordinary retention.
//
// ⚠ It is applied here rather than in the binary's wiring on purpose: this is
// the only door into s.ext, so a fixture and the binary get the same store.
func (s *Server) UseExtensions(ext Extensions) {
	s.extDefects = ext.defects()
	ext.Notes = withSessionLiveness(ext.Notes, ext.SessionLiveness)
	s.ext = ext
	s.pluginDeliverer = nil
	if ext.Plugins != nil && ext.PluginGateway != nil && ext.Gateway != nil && ext.Agents != nil {
		// 🔴 Turn IS chatTurn: the delivered message runs as an ordinary chat turn
		// WITH the chief's tools, never as a toolless send (plugins.TurnFunc).
		d, err := plugins.NewDeliverer(plugins.DelivererConfig{Store: ext.Plugins, Agents: ext.Agents,
			Gateway: ext.PluginGateway, Turn: s.chatTurn, Logger: s.logger, Routing: routingOn(ext)})
		if err != nil {
			s.logger.Printf("plugins: no deliverer: %v", err)
		}
		s.pluginDeliverer = d
	}
	s.connectProxy = nil
	if ext.Connectors != nil {
		s.connectProxy = &connectors.Proxy{
			Store:            ext.Connectors,
			Policy:           ext.ConnectorPolicy,
			Logger:           s.logger,
			OnGrantRequested: s.onConnectorRequested,
			// The SAME signal EndStreams cancels: a relayed event stream ends with
			// this package's own streams, on whichever listener it arrived.
			StreamsEnd: s.streams.context(),
		}
	}
}

// defects lists wiring mistakes that must not be SERVED with, as opposed to
// dependencies that are merely absent.
//
// 🔴 THE DISTINCTION IS "DEGRADES VISIBLY" VERSUS "LIES SILENTLY", AND IT IS
// THE ONLY test applied here. A nil GitHub store is not a defect: the Repos
// tab is simply not drawn, which is TRUE — this server has no GitHub
// connection. A notes store with no liveness probe IS a defect: every transcript
// row renders "no transcript recorded" over transcripts that may be perfectly
// alive, and there is no request that fails, nothing logged, and nothing for an
// operator to notice. The first is a configuration; the second is a page that
// states a falsehood.
//
// 🔴 THE CONSEQUENCE IS /readyz, NOT A PANIC, AND THAT IS DELIBERATE. A panic
// in a wiring call is a crash loop that takes the whole service down over a
// feature; a readiness failure keeps the process up, keeps /health green for
// liveness, and keeps the pod OUT OF SERVICE until someone reads the reason —
// which handleReady prints. It is the loudest signal that is not also an
// outage.
//
// ⚠ IT IS A METHOD ON Extensions, NOT ON Server, SO IT CAN BE TESTED WITHOUT
// ONE — and so a future second caller (a config validator, a boot log) reaches
// the same list rather than writing a second one.
func (e Extensions) defects() []string {
	var out []string
	if e.Notes != nil && e.SessionLiveness == nil {
		out = append(out, "Notes is wired but SessionLiveness is not: every task's transcript "+
			"links would render \"no transcript recorded\" whether or not a transcript exists, "+
			"silently and on every surface. Wire a liveness probe (internal/router satisfies it) "+
			"or leave Notes unset.")
	}
	// ⚠ A SECOND ENTRY USED TO SIT HERE: a wired Provisioner beside a wired
	// privilege store with no applier. The privilege subsystem was removed whole
	// (superseded by connectors), so there is no grant to render and nothing for
	// that entry to refuse.

	// 🔴 A FORGOTTEN NAMESPACE PREFIX IS THE "LIES SILENTLY" SHAPE IN
	// ITS PUREST FORM, AND IT IS THE DEFECT THIS ENTRY WAS ADDED FOR. Every agent
	// row this server writes records AgentNamespacePrefix+name. Nothing places
	// anything with that value — it is recorded, served on GET /api/agents and
	// labelled onto the instance's objects — so a prefix that does not match the
	// driver's produces rows naming namespaces that hold nothing, with no failing
	// request, nothing logged, and nothing for an operator to notice. That is
	// exactly what shipped: `devpod-lively-newt` on the row, `muster-agent-lively-newt`
	// in the cluster.
	//
	// 🔴 IT IS A DEFECT RATHER THAN A DEFAULT *BECAUSE* THE DEFAULT EXISTS.
	// AgentNamespace resolves an empty prefix to agents.NamespacePrefix, which is
	// what keeps ~20 struct-literal test fixtures from having to set it — and is
	// therefore precisely what would let a production wiring omit it and look
	// fine. The binary always resolves a non-empty prefix
	// (agents.ResolveNamespacePrefix, from cmd/muster-server's config load), so
	// empty HERE, with agents creatable, can only mean the wiring was dropped.
	//
	// ⚠ IT IS GATED ON Provisioner AS WELL AS Agents ON PURPOSE. With no
	// provisioner there is no driver to disagree with and no route that can create
	// an agent (all three callers of createAndDispatchAgent sit behind
	// requireLifecycleProvisioner), so the value is inert and refusing to serve
	// over it would be the "configuration, not a page that states a falsehood"
	// case this function's header rules out.
	if e.Agents != nil && e.Provisioner != nil && e.AgentNamespacePrefix == "" {
		out = append(out, "Agents and Provisioner are wired but AgentNamespacePrefix is empty: "+
			"every agent row created here would record agents.NamespacePrefix+name while the "+
			"provisioning driver places the instance under the prefix IT was configured with "+
			"(MUSTER_AGENT_NAMESPACE_PREFIX). Nothing fails when those disagree — the value is "+
			"recorded, served on GET /api/agents and labelled onto the instance's objects, and "+
			"nothing places anything with it — so `kubectl -n <what the row says>` returns "+
			"nothing and reads as \"never provisioned\" while the instance runs one namespace "+
			"over. That is a shipped defect, not a hypothetical. Set it from the same config "+
			"value the driver gets: cmd/muster-server passes cfg.AgentNamespacePrefix, which "+
			"agents.ResolveNamespacePrefix has already defaulted.")
	}
	return out
}

// AgentNamespace is the ONE expression that turns an agent's slug name into the
// namespace its row records.
//
// 🔴 IT IS A METHOD ON Extensions, NOT A PACKAGE FUNCTION, SO IT CANNOT BE CALLED
// WITHOUT THE DEPLOYMENT'S PREFIX IN HAND. The defect it replaces was a
// zero-argument call — agents.NamespaceFor(name) — that captured a package
// constant while the driver used the configured prefix; every line of the caller
// looked correct. Reaching this value now requires holding the wiring that
// carries the prefix, and defects() refuses to serve when that wiring is empty
// beside a creatable agent.
//
// ⚠ IT IS EXPORTED FOR THE GUARD AS WELL AS THE HANDLER.
// TestTheStoredNamespacePrefixIsWhatTheDriverIsConfiguredWith calls it from
// cmd/muster-server and compares the result against the namespace the real
// driver is OBSERVED to create — which is only a claim about the production path
// if the guard and the handler evaluate the same expression, rather than two
// copies of it.
func (e Extensions) AgentNamespace(name string) string {
	return agents.NamespaceFor(e.AgentNamespacePrefix, name)
}

// ProvisionerUnwiredField is the JSON key both provisioner wrappers set to `true` on
// its 503 body.
//
// 🔴 IT IS THE FIELD, NOT THE SENTENCE, THAT A CLIENT BRANCHES ON — the same
// shape HookUnarmedField gives the machine tier, and for a sharper reason. Every
// OTHER 503 a caller meets on these routes is a transient outage worth retrying;
// this one is a permanent property of the BUILD, and a client that cannot tell
// them apart retries for ever against a server whose answer can never change.
// A discriminator made of prose is one a reword silently breaks.
const ProvisionerUnwiredField = "provisionerUnwired"

// The provisioner wrappers refuse a route that cannot do its work without
// Extensions.Provisioner, rather than letting the handler reach a nil interface.
//
// 🔴 THIS EXISTS BECAUSE "REGISTERED AND REFUSES" WAS NOT TRUE, AND THE BOOT
// BANNER SAID IT ON EVERY START. Measured against the image this tree builds,
// with Provisioner nil (the only state cmd/muster-server can be in today):
// POST /agents answered 200 and rendered a card while the row sat
// `provisioning` for ever; POST /agents/{id}/start answered 200 with 3,590
// bytes of card HTML; POST /agents/{id}/stop answered 200; DELETE /agents/{id}
// answered 200 and htmx removed the card while the row was still there on the
// next read; and GET /agents/{name}/logs/stream answered 200 with `: connected`
// and then EOF, which a reader sees as "this agent has no logs".
// muster_panics_total{source="goroutine"} stood at 4. Every one of those is a
// surface stating a falsehood with nothing logged where a user is looking.
//
// 🔴 IT IS THE SAME OBSERVABLE doc_seams.go ENTRY 1 ARGUES AGAINST. That entry
// refuses to wire a FAKE provisioner because a fake "would make the Agents tab
// LOOK operational: dispatch would report success, no pod would exist, and the
// agent would sit in `provisioning` for ever." Wiring nil produced exactly that,
// byte for byte. The nil was never the honest option on its own — this wrapper
// is what makes it one.
//
// 🔴 WHY A WRAPPER AND NOT A defects() ENTRY, STATED SO THE NEXT READER DOES NOT
// RE-OPEN IT. Adding `Provisioner == nil && Agents != nil` to defects() would
// fail /readyz, which is fail-closed and simpler — and would make this module
// UNDEPLOYABLE until the provisioner adapter lands, taking the task board, notes,
// repos and projects down with a seam that is open on purpose. The
// refusal belongs at the routes that cannot work, not at the whole process. With
// this wrapper a nil Provisioner moves from the "lies silently" side of
// defects()'s own line to the "degrades visibly" side, which is precisely why it
// is NOT listed there.
//
// ⚠ IT IS THE INNER WRAPPER, WITH AUTH OUTSIDE IT, AND THE ORDER IS LOAD-BEARING.
// Registered as requireSession(requireLifecycleProvisioner(h)) the session check runs
// first, so an anonymous caller is refused before this reply can tell them which
// dependencies this deployment has wired. The reverse order turns every one of
// these routes into an unauthenticated probe of the server's build.
//
// ⚠ IT DOES NOT MOVE THE ROUTE GOLDEN, WHICH IS WHY THE ROUTES STAY REGISTERED.
// The golden records patterns only (see routes_golden_test.go's KNOWN LIMIT
// banner), and a wrapper changes the handler, not the pattern. doc_seams.go
// entry 1 says the golden moving for this nil would itself be a defect; it does
// not move.
// 🔴 THERE ARE TWO WRAPPERS NOW, NOT ONE, AND EVERY WORD ABOVE STILL APPLIES TO
// BOTH. The split is described on [Gateway]: one nil used to gate nine routes, so
// wiring the seven lifecycle ones meant also claiming the two chat ones. Each
// wrapper refuses at the same door, in the same vocabulary, naming the dependency
// the route actually needs — which is the reason the split was preferred over an
// adapter whose chat methods returned a rendered refusal from inside a handler.
// One condition, one refusal shape.
func (s *Server) requireLifecycleProvisioner(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.ext.Provisioner == nil {
			s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error": "this build has no agent provisioner wired, so it cannot " +
					"provision, start, stop, destroy or stream logs from an agent. " +
					"This is a declared seam, not an outage and not your credential: " +
					"see cmd/muster-server/doc_seams.go entry 1.",
				ProvisionerUnwiredField: true,
			})
			return
		}
		next(w, r)
	}
}

// requireGatewayProvisioner is the chat half. See requireLifecycleProvisioner.
func (s *Server) requireGatewayProvisioner(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.ext.Gateway == nil {
			s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error": "this build has no agent gateway wired, so it cannot chat " +
					"with an agent. Provisioning may still work — the two are separate " +
					"dependencies. This is a declared seam, not an outage and not your " +
					"credential: see cmd/muster-server/doc_seams.go entry 1.",
				ProvisionerUnwiredField: true,
			})
			return
		}
		next(w, r)
	}
}

// ProfileReapplier reconciles a running instance to a freshly-rendered spec, which
// rolls it. Optional: the model-change path type-asserts for it, so a nil
// provisioner is unaffected.
//
// ⚠ THE NAME IS HISTORICAL. It was written for the privilege grant path, which
// re-rendered an agent's granted-profile env into its pod; that path is gone and
// the model change is the only caller. It reconciles the WHOLE spec, not
// "profiles".
//
// ⚠ ITS PREVIOUS WORDING WAS "re-applies an agent's granted-profile env/kubeconfig to
// its running POD (a HELM UPGRADE; the pod rolls)" — the same helm-and-pod vocabulary
// removed from Destroy and StreamLogs below, and missed in the same sweep
// that removed them. It is false of the only implementation (which calls the driver's
// Update), false for a driver with no pods, and it names a mechanism the provisioner
// contract deliberately does not have.
type ProfileReapplier interface {
	ReapplyProfiles(ctx context.Context, agentID int64) error
}

// Provisioner is the agent-instance LIFECYCLE driver the agent handlers depend
// on: bring one into existence, scale it, destroy it, read its state and its
// logs. It is defined here (consumer side) so this package does not import the
// heavy cluster dependencies unless the binary wires a real provisioner in.
//
// ⚠ CHAT IS NOT HERE ANY MORE — see [Gateway] for why the two are separate.
type Provisioner interface {
	// Dispatch provisions an instance for the agent when kickoff is true, and
	// creates NOTHING when it is false — see the note below on both halves.
	//
	// 🔴 THIS DOC USED TO DESCRIBE BEHAVIOUR NO IMPLEMENTATION HAS. It read
	// "provisions a pod for the agent and, when kickoff is true, sends the note as
	// the first message once the gateway is reachable", which is wrong twice over
	// for internal/agentprovision — the only implementation in this module:
	//
	//   - it does NOT send the note. Delivery needs an api.Gateway and there is no
	//     implementation of one here, so the non-delivery is recorded on the row
	//     instead (agentprovision.UndeliveredKickoffReason). An interface doc
	//     promising a delivery is how a future implementer comes to call
	//     SetKickedOff, which is the one thing that package must never do.
	//   - kickoff=false creates NOTHING rather than provisioning-then-storing-
	//     stopped. The caller's OTHER action is the UI's "Save for later", whose
	//     gate check (agents.go, the `gate:<reason>` refusal) allows a save on the
	//     stated grounds that it "provisions nothing".
	//
	// An implementer that provisions on kickoff=false, or that reports a delivery
	// it did not make, is wrong against this interface — not merely different.
	//
	// 🔴 REFUSING A KICKOFF IT CANNOT DELIVER IS ALSO LEGAL, AND IS WHAT
	// internal/agentprovision NOW DOES when nothing in its process could deliver
	// one: it creates nothing and returns an error
	// (agentprovision.ErrKickoffUndeliverable). The paragraph above describes the
	// CREATE-THEN-RECORD path. Both are honest; what this interface forbids is the
	// third option, doing the expensive half and reporting success.
	//
	// ⚠ THIS USED TO SAY CREATE-THEN-RECORD WAS "the gateway-configured one only",
	// WHICH MADE A CONFIGURED GATEWAY SOUND SUFFICIENT. It was not while nothing
	// CALLED the gateway. internal/agentkickoff now does, and the first bullet
	// above is superseded for a deployment that names a gateway: the adapter
	// still never sends the note itself, but it records no non-delivery either —
	// it creates the instance and leaves the owed row to that deliverer, which is
	// the one place SetKickedOff is called. See agentprovision.KickoffDeliveryWired.
	//
	// ✅ AND THE REFUSAL NOW REACHES THE CALLER, through
	// KickoffUndeliverableReason below rather than through this method. This
	// paragraph read "THE CALLER OF THIS METHOD IN THIS PACKAGE CANNOT SURFACE THAT
	// REFUSAL … A refusal an operator can see needs a synchronous check in the
	// create handler, before the row", and that check exists: handleAgentCreate
	// answers 409 with the reason before Agents.Create. The asynchronous half is
	// unchanged and still true of THIS method — createAndDispatchAgent calls it
	// inside safeGo, after the row exists — which is why the pre-flight is a
	// separate question rather than a different return value here.
	//
	// 🔴 ctx CARRIES THE CALLER'S PRINCIPAL, NOT ITS DEADLINE (all four lifecycle
	// methods). The work outlives the request (these run inside safeGo), so an
	// implementation must detach from ctx's cancellation — context.WithoutCancel —
	// but keep its values: under row-level security every store read and write
	// the operation makes acts as ctx's principal, and a context.Background()
	// budget would see no rows (accounts PR 3a).
	Dispatch(ctx context.Context, agentID int64, kickoff bool) error
	// Start brings a stopped or never-provisioned agent up, creating its instance
	// when the driver reports the backend does not have one.
	//
	// ⚠ IT DOES NOT KICK OFF, AND THIS DOC SAID IT DID. It read "(kicking off if
	// pending)" — the same retracted claim Dispatch's doc above corrects, left
	// behind in the sibling method for one round. The only implementation records
	// the non-delivery instead; see agentprovision.UndeliveredKickoffReason.
	Start(ctx context.Context, agentID int64) error
	// Stop scales a running agent down to zero replicas, keeping its declaration.
	// An instance the backend does not have is already stopped, not an error.
	Stop(ctx context.Context, agentID int64) error
	// Destroy removes the instance and everything the driver created for it, then
	// deletes the stored row.
	//
	// ⚠ IT DELETES THE ROW ONLY WHEN THE BACKEND HOLDS NOTHING, and this doc did
	// not say so — which is the contract the lifecycle adapter added. A terminal
	// ownership refusal or an unreachable backend KEEPS the row, because the row is
	// then the only record of a live instance.
	//
	// ⚠ ITS PREVIOUS WORDING WAS "uninstalls the release and deletes the
	// namespace" — helm and Kubernetes vocabulary in an interface whose Instances
	// doc makes a 🔴 point of having removed exactly that, and false for any driver
	// without namespaces.
	Destroy(ctx context.Context, agentID int64) error
	// Instances returns the live state of every agent workload, for status
	// reconciliation.
	//
	// 🔴 IT IS provision.Instance, NOT A POD, AND THE RENAME IS THE CARVE. The
	// method this replaces was `Pods` returning a Kubernetes-shaped struct, which
	// made every status consumer in this package a Kubernetes consumer by
	// transitivity. The provisioner contract's own vocabulary is what lets
	// agents.ComputeStatus answer for an agent running under any driver.
	Instances(ctx context.Context) ([]provision.Instance, error)
	// StreamLogs follows the agent's instance output, invoking emit per line.
	StreamLogs(ctx context.Context, a agents.Agent, emit func(string)) error
	// AttachTerminal attaches a terminal to the agent's live session until ctx
	// ends or the session's client exits: what is written to stdin is typed,
	// what the terminal shows arrives on stdout, and each size on resize is
	// applied. readOnly attaches a client that cannot type.
	//
	// It refuses — agents.ErrNoTerminal for a kind with no session,
	// provision.ErrUnsupported for a driver that cannot exec — rather than
	// returning without having attached anything.
	//
	// ⚠ ON THE INTERFACE RATHER THAN TYPE-ASSERTED, for the reason
	// KickoffUndeliverableReason below gives: an optional interface an adapter
	// forgets yields ok=false with no trace, and the terminal route would then
	// refuse every agent while every capability reported it could exec.
	AttachTerminal(ctx context.Context, a agents.Agent, readOnly bool, stdin io.Reader, stdout io.Writer, resize <-chan provision.TermSize) error
	// KickoffUndeliverableReason answers why this process cannot deliver an
	// agent's FIRST TURN, and "" when it can. It must not create, write or
	// provision anything: it is the question Dispatch answers by refusing, asked
	// before anything exists to refuse over.
	//
	// 🔴 IT IS ON THIS INTERFACE RATHER THAN BEING A TYPE ASSERTION, AND THE
	// POLARITY OF THE FAILURE IS WHY. An optional interface that an
	// implementation does not satisfy yields ok=false and leaves NO TRACE —
	// doc_seams.go entry 3 is about exactly that — and here the no-trace branch
	// is the permissive one: no pre-flight, so a dispatch the deployment cannot
	// satisfy answers 200 again and the operator is back to reading a `running`
	// card over an agent nobody told what to do. A method on the interface makes
	// omitting it a compile error instead.
	//
	// 🔴 A CALLER MUST NOT RE-DERIVE THIS FROM ITS OWN CONFIGURATION. The answer
	// has to be the SAME fact the implementation's own Dispatch branches on, or
	// the pre-flight and the dispatch disagree — which is worse than no
	// pre-flight, because it refuses requests that would have worked or admits
	// ones that will not. internal/agentprovision returns the field Dispatch
	// reads; see its own doc on why.
	//
	// ⚠ "" IS DELIVERABLE, so the zero return is the permissive one. The
	// fail-closed argument lives on agentprovision.Config.KickoffDeliverable,
	// where a wiring can FORGET a struct field; a method cannot be forgotten.
	KickoffUndeliverableReason() string
}

// Gateway is the agent CHAT surface: talking to a running instance's model
// gateway.
//
// 🔴 IT IS A SEPARATE INTERFACE FROM Provisioner, AND THE SPLIT IS THE WHOLE
// POINT. Both used to be one, behind one nil and one wrapper, so the seven
// LIFECYCLE methods could not be wired without also claiming the two CHAT ones.
// That is not a packaging preference: provision.Provisioner — the driver contract
// a lifecycle adapter is built on — has no notion of a chat turn at all, so an
// adapter over it can implement the seven honestly and the two not at all. Behind
// one interface the only ways to express that were to wire a stub (which
// doc_seams.go entry 1 rejects, because dispatch reporting success over a pod that
// does not exist is the defect the wrapper was built to remove) or to leave
// lifecycle unwired for as long as chat is. Two interfaces make "lifecycle works,
// chat honestly refuses" sayable.
//
// ⚠ A DEPLOYMENT MAY WIRE EITHER, BOTH, OR NEITHER, and each combination has a
// truthful rendering. Chat without lifecycle is unusual but not incoherent — an
// installation whose instances are provisioned by something else entirely.
type Gateway interface {
	// Chat sends a user message to the agent gateway under the given chat-session
	// key (each session = an independent gateway context), streaming assistant
	// deltas to emit, and returns the full assistant reply. It carries no tools.
	//
	// ⚠ IT DOES NOT RETURN agents.ErrResponsesUnsupported, AND THE ASYMMETRY WITH
	// ChatWithTools BELOW IS THE POINT. An implementation picks the transport
	// itself — the one documented on agentgateway.Gateway.Chat runs over the
	// responses endpoint and drops to chat-completions on a 404 — because no caller
	// has information about which wire format an agent's image speaks. There is
	// nothing for a caller to branch on here, so a caller that received that
	// sentinel from ChatWithTools has exactly one move: call this.
	Chat(ctx context.Context, a agents.Agent, sessionKey, message string, emit func(string)) (string, error)
	// ChatWithTools runs a tool-enabled turn via the gateway's responses API
	// under the given chat-session key: the model gets the native function tools
	// and dispatch executes each call in-process. emit (nil-safe) streams live
	// text/thinking deltas plus tool_call/tool_result events as the loop runs.
	// Returns agents.ErrResponsesUnsupported when the agent's gateway lacks the
	// responses API, so the caller can fall back to Chat.
	ChatWithTools(ctx context.Context, a agents.Agent, sessionKey, instructions, message string, tools []agents.ToolDef, dispatch agents.ToolDispatch, emit agents.StreamEmit) (string, error)
}

// AgentKinds is the dispatch side of agent kinds (cmd/muster-server implements it
// over internal/ccpool and internal/credentials).
//
// 🔴 EVERY ACCOUNT QUESTION IS ASKED FOR AN OWNER. Members bring their own
// credentials (accounts plan, operator decisions): the names a member's form
// lists, the account a member's dispatch selects and whether a kind is usable
// at all are answers about THAT member — never the deployment's pool, which is
// the admin's.
type AgentKinds interface {
	// Enabled is the kinds a dispatch may name, in picker order. It always
	// includes agents.KindGateway.
	Enabled() []string
	// ClaudeAccounts is owner's Claude account NAMES (never tokens), for the
	// pin picker.
	ClaudeAccounts(ctx context.Context, owner int64) ([]string, error)
	// SelectClaudeAccount picks the account owner's new claude-code agent runs
	// on, from owner's accounts only, or returns the pin when it names one of
	// them. An error is a dispatch refusal.
	SelectClaudeAccount(ctx context.Context, owner int64, pin string) (string, error)
	// Unavailable maps each enabled kind owner has NO credential for to the
	// sentence telling them what to add. Absent = usable.
	Unavailable(ctx context.Context, owner int64) map[string]string
}

// enabledKinds is Extensions.Kinds.Enabled with the nil case resolved.
func (e Extensions) enabledKinds() []string {
	if e.Kinds == nil {
		return []string{agents.KindGateway}
	}
	return e.Kinds.Enabled()
}

// kindEnabled reports whether a dispatch may ask for kind.
func (e Extensions) kindEnabled(kind string) bool {
	for _, k := range e.enabledKinds() {
		if k == agents.ResolveKind(kind) {
			return true
		}
	}
	return false
}
