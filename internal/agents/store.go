// Package agents is muster's agent domain: the stored record of a managed agent,
// its chat threads, the model catalogue that decides what it runs on, and the
// pure decision table a reconciler drives it with.
//
// 🔴 IT DOES NOT PROVISION ANYTHING, AND THAT IS THE CARVE'S WHOLE SHAPE.
// Upstream this package also owned a Helm client, a cluster client, a vendored
// chart and the agent's chat transport, all hanging off one type. Creating,
// scaling, destroying and reaching an instance is internal/provision's job here;
// this package holds what is true about an agent regardless of what runs it. The
// carve notes at the foot of reconcile.go and modelauth.go name what that leaves
// owed, and who closes it.
//
// This file holds the record and the Store interface.
package agents

import (
	"context"
	"strings"
	"time"
)

// Status values for an agent (mirrors the agents.status CHECK constraint).
const (
	StatusPending      = "pending"
	StatusProvisioning = "provisioning"
	StatusRunning      = "running"
	StatusStopped      = "stopped"
	StatusError        = "error"
)

// NamespacePrefix is the DEFAULT prefix an agent's [Agent.Namespace] is built
// from: the prefix plus the agent's slug Name.
//
// 🔴 IT IS A DEFAULT, NOT THE VALUE, AND THAT DISTINCTION IS A FIX FOR A
// MEASURED LIVE DEFECT. It used to be the value — [NamespaceFor] took only a
// name and returned NamespacePrefix+name — while the provisioning driver used
// the prefix its OWN configuration named. Two literals, in subsystems that never
// talk, and nothing fails when they differ: the value is recorded, displayed and
// labelled, never used to place anything, so `kubectl -n <what the row says>`
// returns nothing and reads as "the agent was never provisioned" while the
// instance is running one namespace over. Measured live on a deployment that
// sets MUSTER_AGENT_NAMESPACE_PREFIX=muster-agent-: a freshly provisioned
// agent's row said `devpod-lively-newt` while the driver had created
// `muster-agent-lively-newt`.
//
// The two sides now read ONE configured value. api.Extensions.AgentNamespace is
// the row-writing path's only expression, the deployment's
// MUSTER_AGENT_NAMESPACE_PREFIX feeds it and the driver from the same
// config field, and
// TestTheStoredNamespacePrefixIsWhatTheDriverIsConfiguredWith holds the pair
// against the namespace the driver DEMONSTRABLY creates — for a configured
// prefix and not only for this default, which is the case the old guard exempted
// by design and is why the defect shipped.
const NamespacePrefix = "devpod-"

// ResolveNamespacePrefix is the ONE place an unset prefix becomes
// [NamespacePrefix].
//
// 🔴 EVERY DEFAULTING SITE MUST BE THIS CALL. Two places resolving "" — the
// binary's config load and the HTTP layer's wiring — is how one of them comes to
// hold a different literal, which is the defect described above in miniature.
// Whitespace resolves to the default too: an operator's trailing newline in a
// ConfigMap would otherwise build a namespace Kubernetes refuses, inside a
// dispatch goroutine.
func ResolveNamespacePrefix(raw string) string {
	if p := strings.TrimSpace(raw); p != "" {
		return p
	}
	return NamespacePrefix
}

// NamespaceFor builds an agent's namespace from a CONFIGURED prefix and its slug
// name.
//
// 🔴 THE PREFIX IS A PARAMETER SO THAT NO CALLER CAN GET A NAMESPACE WITHOUT
// DECIDING ONE. This took only a name until the defect above; a zero-argument
// spelling is what let a caller capture the package constant while the driver
// used something else, and the caller looked correct at every line. An empty
// prefix resolves to the default via [ResolveNamespacePrefix] rather than
// producing a bare slug.
//
// ⚠ PREFER api.Extensions.AgentNamespace ON THE ROW-WRITING PATH. That method is
// the one expression the HTTP layer uses and the one the guard reads; this
// function is what it is built from.
func NamespaceFor(prefix, name string) string { return ResolveNamespacePrefix(prefix) + name }

// Agent is the stored record of one managed agent.
type Agent struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`      // short slug; also the provision.Ref.Name
	Namespace   string `json:"namespace"` // the driver's scoping unit (provision.Instance.Group)
	DisplayName string `json:"displayName"`
	Repo        string `json:"repo"` // owner/name
	RepoBranch  string `json:"repoBranch"`
	NoteID      *int64 `json:"noteId,omitempty"`
	NoteText    string `json:"noteText,omitempty"`

	// PendingNote is the first message an agent was created WITH: the text a
	// dispatch asked to be delivered as its opening turn. It is written once, by
	// internal/api's createAndDispatchAgent, and from then on only round-tripped
	// by the store.
	//
	// ✅ internal/agentkickoff DELIVERS IT. When the agent's instance is ready
	// and the row has never been kicked off, that background loop sends this text
	// as the agent's first turn through agentgateway.Gateway (Resolve, then Send,
	// which together are Gateway.Chat) and stamps KickedOff BEFORE the Send. The text is KEPT after delivery rather than cleared: it is
	// the task the agent was given, and a re-send after a lost recipient
	// (ActionResendKickoff) would need it.
	//
	// ⚠ THIS COMMENT ONCE SAID "NOTHING DELIVERS IT", which was true while
	// agentprovision.KickoffDeliveryWired was false — the field had no production
	// reader beyond the store. That constant is bound to a measurement of the
	// module's call graph (modulegate.TestKickoffDeliveryLedgerAgreesWithTheModule),
	// so it could not flip without a deliverer landing. On a deployment with no
	// gateway a dispatch asking for a first turn is still refused, so a row there
	// can still hold one of these for ever.
	//
	// [KickoffOwed] is its derived reader for the UI. It answers "is a first turn
	// still owed" from this field together with KickedOff, and that BOOLEAN is
	// what the agent card and the machine projection (api.agentJSON) report.
	//
	// 🔴 THE TEXT IS NEVER SERIALISED WITH THE ROW, AND `json:"-"` IS THE WHOLE
	// MECHANISM. ⚠ This read "NEVER LEAVES THE PROCESS", which delivery made false
	// by design: internal/agentkickoff sends it to the agent's own gateway and
	// writes it to the agent's transcript as the first user message — the same two
	// places any operator message goes. What stays true is that no projection of
	// the ROW carries it. It is operator-authored instruction text, this repository is
	// public, and the leak gate (tests/leakscan.py) is the standing reason. So the
	// derived boolean is surfaced and the content is not — asserted both ways by
	// agents.TestKickoffOwedTruthTable's sibling
	// TestAnOwedKickoffIsReportedWithoutTheNotesText in internal/api, which reads
	// the rendered card and the JSON body for the fixture's own words.
	PendingNote string `json:"-"`

	Model string `json:"model"` // OpenRouter slug; "" = cluster default
	// Kind is the runtime this agent is provisioned as: [KindGateway] or
	// [KindClaudeCode]. "" reads as KindGateway (see [ResolveKind]). Fixed at
	// creation — migration 0003's trigger refuses an UPDATE of it.
	Kind string `json:"kind"`
	// CCAccount is the Claude account (a name from MUSTER_AGENT_CC_ACCOUNTS) whose
	// setup-token this claude-code agent runs on; "" for every other kind. It is a
	// NAME, never the token. Chosen once at dispatch by internal/ccpool and kept
	// for the agent's life (migration 0003 enforces both halves).
	CCAccount string `json:"ccAccount,omitempty"`
	// WorkerPool is the named worker pool this claude-code agent serves machine
	// jobs for (agents.worker_pool, migration 0042); "" for an agent in none.
	// Set at creation (dispatch's worker_pool) and never written after; a job
	// naming the pool instead of an agent is given one of its members
	// (internal/api pickJobAgent). See [ValidWorkerPool].
	WorkerPool string `json:"workerPool,omitempty"`
	// OwnerID is the user this agent belongs to (agents.owner_id, migration
	// 0006). A request authenticated by THIS agent's own token acts as this
	// user (internal/api agentOwnerID). Not serialised: the JSON shape is
	// unchanged, and an owner id is not something an agent or a page needs.
	// Zero only for an Agent that never came from the database (a fake store).
	OwnerID    int64  `json:"-"`
	Status     string `json:"status"`
	HooksToken string `json:"-"`
	// GatewaySecret is the credential muster presents to a claude-code agent's
	// turn API (ccd's /v1/responses), minted per agent at provision time
	// (agents_gateway_secret, migration 0042). "" for every other kind, and for a
	// claude-code row provisioned before it existed — see [TurnBearer].
	//
	// 🔴 IT IS RANDOM AND NOT DERIVED FROM HooksToken, AND THAT IS ITS WHOLE
	// PURPOSE. HooksToken is the agent's muster identity, which its session must
	// hold (MUSTER_HOOK_TOKEN: the approval hook, the MCP entry, git routing), so
	// every tool call the session makes can read it. A turn credential computable
	// from it — muster's hooks-sha256 derivation — let any tool call drive the
	// agent's own turn API. Never serialised, never logged.
	GatewaySecret string    `json:"-"`
	KickedOff     bool      `json:"kickedOff"`
	LastOutput    string    `json:"lastOutput,omitempty"`
	ErrorMessage  string    `json:"errorMessage,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`

	// --- kickoff DELIVERY provenance (internal/db/migrations/0001_init.sql) ---
	//
	// KickedOff answers "was the message handed to a ready gateway"; these answer
	// "to WHICH container", which is the only way to tell a live agent still
	// holding the message from one whose recipient the kernel killed. See
	// kickoffLost (reconcile.go) for the full rationale. ⚠ This cited "migration
	// 0020" (and the claim columns "migration 0022"): those are the upstream
	// project's numbers. Every kickoff column here is in 0001_init.sql.

	// KickoffPod is the provision.Instance.InstanceID that received the
	// kickoff. Empty = unknown (a row predating these columns, or a delivery
	// whose instance lookup failed), which DISABLES restart detection for that
	// agent — the fail-safe direction.
	KickoffPod string `json:"kickoffPod,omitempty"`
	// KickoffRestarts is the container restart count observed AT delivery. A
	// later count strictly greater than this means the recipient was restarted
	// in place (the out-of-memory shape, where the instance id does not change).
	KickoffRestarts int32 `json:"kickoffRestarts,omitempty"`
	// KickoffAttempts counts deliveries made (the first dispatch plus each
	// recovery re-send). It bounds recovery at MaxKickoffAttempts so an agent
	// that dies deterministically ends VISIBLY failed rather than re-kicked
	// forever.
	KickoffAttempts int `json:"kickoffAttempts,omitempty"`
	// KickoffError is the last kickoff SEND failure ("unexpected EOF" when the
	// container died mid-turn). Cleared at the start of each delivery. It exists
	// because that failure used to be a log line and nothing else.
	KickoffError string `json:"kickoffError,omitempty"`
}

// ChatMessage is one persisted web-chat message in an agent's transcript.
type ChatMessage struct {
	ID        int64     `json:"id"`
	AgentID   int64     `json:"agentId"`
	SessionID int64     `json:"sessionId"` // the chat session this message belongs to
	Role      string    `json:"role"`      // "user" | "assistant"
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"createdAt"`

	// Structured-transcript fields (migration 0015). An assistant turn persists as
	// ORDERED parts so the canonical transcript matches the live stream. Kind is
	// 'text' (a normal message — the default for every legacy row), 'tool_call'
	// (Content = raw JSON args), or 'tool_result' (Content = dispatch output).
	Kind     string `json:"kind,omitempty"`
	ToolID   string `json:"toolId,omitempty"`   // links a tool_call to its tool_result
	ToolName string `json:"toolName,omitempty"` // function name (tool_call/tool_result)
	ToolOK   bool   `json:"toolOk,omitempty"`   // tool_result: dispatch success
}

// ChatSession is one independent conversation with an agent's gateway. Each
// session has its own gateway session key (= a fresh agent context), so a user
// can hold several parallel conversations with the same agent.
type ChatSession struct {
	ID         int64     `json:"id"`
	AgentID    int64     `json:"agentId"`
	SessionKey string    `json:"sessionKey"` // the session-key header value sent to the runtime
	Title      string    `json:"title"`      // derived from the first user message ("" until then)
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
	// ReadAt is when the human last opened this session. An assistant message
	// created after ReadAt counts as unread (the notification panel + FAB badge).
	ReadAt time.Time `json:"readAt"`
	// Runtime and Model are fixed when the session is created (migration 0030,
	// session_runtime.go): Runtime "" is a LEGACY session, on the agent's own
	// kind. IsDefault marks the one session plugin deliveries and `chief ask`
	// go to.
	Runtime   string `json:"runtime"`
	Model     string `json:"model"`
	IsDefault bool   `json:"isDefault"`
}

// SessionMatch is one SearchSessions hit: the session plus one excerpt around the
// newest matching kind='text' message body, or "" when only the title matched.
type SessionMatch struct {
	ChatSession
	Snippet string `json:"snippet"`
}

// SearchSnippetRadius is how many characters of context a snippet carries on each
// side of the match, and SearchSnippetLen its total length.
//
// 🔴 A SNIPPET IS BOUNDED BY THE STORE, NOT BY CSS. chat_messages.content has no
// length limit and the longest text row on the deployment this was extracted from
// was 174 KB
// — so a reader that returned whole bodies and let the browser clamp them would
// ship that 174 KB over the wire for one search result. The excerpt is cut in SQL.
const (
	SearchSnippetRadius = 60
	SearchSnippetLen    = 180
)

// SearchSessionsMaxLimit bounds what a caller can ask for, whatever it passes.
//
// ⚠ A limit <= 0 IS THE CALLER'S BUG AND YIELDS AN EMPTY PAGE, not everything —
// the same direction ListChatMessagesPage chose, and for the same reason: this
// reader is reachable from a query string.
const SearchSessionsMaxLimit = 50

// AgentUnread summarises one agent's unread assistant replies for the
// notifications panel: the total unread count plus the session to deep-link to
// (the agent's most-recently-active session — the link target).
type AgentUnread struct {
	AgentID     int64  `json:"agentId"`
	AgentName   string `json:"agentName"`
	DisplayName string `json:"displayName"`
	SessionID   int64  `json:"sessionId"` // the link target (most-recently-active session)
	Count       int    `json:"count"`     // total unread assistant messages across the agent
}

// Store is the agents persistence behaviour.
type Store interface {
	Create(ctx context.Context, a Agent) (Agent, error)
	Get(ctx context.Context, id int64) (Agent, error)
	GetByName(ctx context.Context, name string) (Agent, error)
	// GetByHooksToken identifies an agent by its unique per-agent hooks token
	// (the agent self-service auth path).
	GetByHooksToken(ctx context.Context, token string) (Agent, error)
	List(ctx context.Context) ([]Agent, error)
	// AgentsByNoteIDs returns, per requested note id, the note's LATEST linked
	// agent (the most-recently-created one when several point at the same note),
	// using the STORED agent status (no live k8s call — mirrors how the agents
	// list avoids per-agent pod lookups). Notes with no linked agent are simply
	// absent from the returned map. An empty ids slice returns an empty map.
	AgentsByNoteIDs(ctx context.Context, ids []int64) (map[int64]Agent, error)
	// LastMessageByAgentIDs returns, per requested agent id, the created_at of
	// that agent's most recent PERSISTED transcript message (any session, any
	// role, any kind). Agents with no persisted message are simply absent from
	// the returned map; an empty ids slice returns an empty map.
	//
	// It exists because agents.updated_at is NOT an activity signal: it is
	// bumped only by UpdateStatus / SetModel / SetDisplayName / SetHooksToken /
	// SetKickedOff, so an agent that reaches `running` and then works (or
	// wedges) for hours has a frozen updated_at. A transcript row, by contrast,
	// is written every time a turn completes, so it is the newest durable
	// evidence this service holds that the agent was alive.
	LastMessageByAgentIDs(ctx context.Context, ids []int64) (map[int64]time.Time, error)
	NameExists(ctx context.Context, name string) (bool, error)
	UpdateStatus(ctx context.Context, id int64, status, lastOutput, errMsg string) error
	// SetModel updates an agent's model (OpenRouter slug; "" = cluster default) and
	// returns the updated record. The new model takes effect on the agent's next
	// (re)provision — the caller rolls the pod.
	SetModel(ctx context.Context, id int64, model string) (Agent, error)
	// SetDisplayName updates an agent's human-facing display name (the slug Name —
	// the namespace key — is immutable) and returns the updated record. A blank
	// name falls back to the slug Name so the card is never label-less.
	SetDisplayName(ctx context.Context, id int64, name string) (Agent, error)
	SetHooksToken(ctx context.Context, id int64, token string) error
	// SetGatewaySecret stores the agent's turn credential ([Agent.GatewaySecret]).
	SetGatewaySecret(ctx context.Context, id int64, secret string) error
	// SetKickedOff stamps (or un-stamps) the first turn. Stamping also clears a
	// checkout-wait marker (WaitedForCheckout's kickoff_error) in the SAME write:
	// on a kicked-off row the marker would read as a failed kickoff, and cleared
	// in a separate earlier write, a failed stamp would leave a row that no longer
	// reads as waited, which the next tick errors past ProvisioningStuckTimeout.
	SetKickedOff(ctx context.Context, id int64, v bool) error
	// RecordKickoffDelivery stamps WHICH pod received the kickoff (name +
	// container restart count at that instant), increments the attempt counter,
	// and clears any previous send error. Called once per delivery, immediately
	// after the gateway is confirmed ready and KickedOff is flipped.
	//
	// An empty pod (unknown recipient) is written verbatim rather than skipped:
	// leaving a DEAD pod's name stamped would make the agent read as permanently
	// "lost" and burn its whole re-send budget against a recipient we never
	// confirmed. Empty disables detection — no worse than a row with no provenance.
	RecordKickoffDelivery(ctx context.Context, id int64, pod string, restarts int32) error
	// SetKickoffError records the reason a kickoff SEND failed, without touching
	// the agent's status: the pod may still be alive and mid-work (the 0.7.80
	// model-timeout case), so this is evidence, not a verdict. The reconciler is
	// what escalates a dead recipient to `error`.
	SetKickoffError(ctx context.Context, id int64, msg string) error
	// MarkKickoffStuck sets an owed row to `error` with errMsg (and clears
	// last_output, as UpdateStatus does) ONLY if the row is still the one the
	// caller judged stuck — updated_at exactly seenUpdatedAt, which every stamp,
	// delivery record and status write moves — and no live kickoff claim. It
	// reports whether the write applied.
	//
	// 🔴 IT IS CONDITIONAL BECAUSE THE VERDICT COMES FROM A LIST READ, AND THE ROW
	// CAN MOVE BEFORE THE WRITE. A delivery that stamped it meanwhile has paid a
	// turn; a Start meanwhile set it `provisioning` again (and bumped updated_at).
	// An unconditional write turned either into an `error` card. A live claim means
	// a delivery is past its own re-read and may be about to stamp. Pinned against
	// real Postgres by agentkickoff's TestAStuckVerdictDoesNotOverwriteARowThatMoved.
	MarkKickoffStuck(ctx context.Context, id int64, seenUpdatedAt time.Time, errMsg string) (bool, error)
	// MarkRunning moves a row from `provisioning` to `running` ONLY if it is still
	// the row the caller observed — status `provisioning` and updated_at exactly
	// seenUpdatedAt — and reports whether the write applied. It clears
	// error_message (a running agent has none) and leaves last_output alone.
	//
	// 🔴 CONDITIONAL FOR THE SAME REASON AS MarkKickoffStuck: the verdict ("its
	// instance is ready") comes from a list read and a separate instance read, and
	// the row can move in between — a Stop writes `stopped`, a Start or a
	// re-dispatch writes `provisioning` again with a newer updated_at, an error
	// writes `error`. Each of those must win over a promotion decided from the
	// older snapshot. See [PromoteToRunning] and internal/agentstatus.
	MarkRunning(ctx context.Context, id int64, seenUpdatedAt time.Time) (bool, error)
	// ClearKickoffDelivery forgets which pod received the kickoff, which disables
	// restart-detection for this agent until the next delivery re-stamps it.
	//
	// It exists for ONE case: a DELIBERATE scale-down (Stop). Start then creates a
	// pod with a new name, which is indistinguishable from an eviction — so
	// without this, stopping and starting a finished agent would re-send its
	// original task. A stop is the operator saying "the pod is meant to go away",
	// so the provenance it invalidates is dropped rather than acted on. The
	// attempt counter is deliberately NOT reset: it is history, not state.
	ClearKickoffDelivery(ctx context.Context, id int64) error

	// --- cross-process kickoff single-flight ---
	//
	// 🔴 These three are the CROSS-PROCESS half of the kickoff single-flight
	// guard, and a process-local guard is not a substitute. An in-memory map
	// excludes goroutines within ONE process; it cannot see another. Dispatch
	// and Start run off HTTP handlers on whichever replica the load balancer
	// picked and are deliberately NOT leader-gated — a follower must still serve
	// the dispatch the user just asked for — and a rolling update puts BOTH
	// replicas in the endpoint set. Without a shared claim, one agent gets two
	// kickoffs — two paid model turns — each re-stamping the other's delivery
	// provenance.

	// ClaimKickoff atomically takes the agent's kickoff claim for `owner`,
	// expiring `ttl` from now, and reports whether it WON. It wins iff the claim
	// is currently unheld or already expired; it never blocks and never waits.
	//
	// A losing caller must not run a kickoff. It must also NOT release: the claim
	// it saw belongs to someone else.
	//
	// 🔴 ON ERROR IT MUST RETURN won == false. The caller's fail-closed guarantee
	// rides entirely on `!won`: an implementation that returned (true, err) would
	// make the kickoff proceed on an unverified claim and reopen the duplicate
	// silently, on exactly the occasion the database is unhealthy.
	ClaimKickoff(ctx context.Context, id int64, owner string, ttl time.Duration) (bool, error)
	// RenewKickoffClaim extends the agent's claim to `ttl` from now, but ONLY if
	// `owner` still owns it — an owner mismatch means another process legitimately
	// took the claim after this one's TTL lapsed, and stamping over that would
	// resurrect the duplicate the claim exists to prevent. It reports whether the
	// extension landed; false is a real signal (this process no longer holds the
	// claim), not a no-op, and is logged loudly by the caller.
	//
	// An owner match with an already-lapsed expiry is deliberately renewable: the
	// claim lapsed but nobody took it, so re-fencing it is strictly better than
	// leaving the row takeable while the work is still running.
	RenewKickoffClaim(ctx context.Context, id int64, owner string, ttl time.Duration) (bool, error)
	// ReleaseKickoffClaim clears the agent's claim, but ONLY if `owner` still owns
	// it — same reason as the renew predicate. Called from the kickoff's defer, so
	// the next kickoff can start immediately instead of waiting out the TTL. The
	// TTL is the crash backstop, not the normal path.
	ReleaseKickoffClaim(ctx context.Context, id int64, owner string) error
	// ClearKickoffClaimsByOwnerPrefix clears every claim whose owner starts with
	// prefix, returning how many rows it cleared. It backs the STARTUP sweep
	// (Provisioner.SweepOwnStaleKickoffClaims), whose prefix is this pod's own
	// hostname — so it recovers claims stranded by an in-place container restart,
	// where the pod name survives but the process uuid does not.
	//
	// 🔴 The caller must scope the prefix to itself. A wildcard (or empty) prefix
	// would clear a LIVE peer pod's claims during a rollout — handing out a green
	// light for the very duplicate this mechanism prevents.
	ClearKickoffClaimsByOwnerPrefix(ctx context.Context, prefix string) (int64, error)

	Delete(ctx context.Context, id int64) error

	// --- chat sessions (multi-session web chat) ---

	// CreateSession opens a new conversation for an agent with a fresh, unique
	// gateway session key (so it starts with no context). Title is empty until
	// the first user message names it.
	CreateSession(ctx context.Context, agentID int64, agentName string) (ChatSession, error)
	// ListSessions returns an agent's sessions most-recently-active first.
	ListSessions(ctx context.Context, agentID int64) ([]ChatSession, error)
	// SearchSessions returns the agent's sessions whose TITLE or whose kind='text'
	// message content contains q (case-insensitive substring), RANKED: a title that
	// starts with q first, then a title with a word that starts with q, then every
	// other match — most-recently-active first within each rank — each carrying
	// one excerpt around the newest body match. The rank is applied before the
	// limit, so a prefix match is never cut off by a page of newer substrings. A session
	// matched by title alone carries an EMPTY Snippet — the title is already on
	// screen, and repeating it would claim a body match that does not exist.
	//
	// 🔴 kind='text' ONLY, AND EXCLUDING kind='tool_result' IS THE POINT RATHER
	// THAN AN OPTIMISATION. Those rows are file contents and command output: on the
	// deployment this was extracted from they were 11 of 34 chat_messages rows, and a
	// transcript tail is where an agent's `cat` of a config file lands. Searching
	// them would make every common word match noise AND would paste raw file bytes
	// on screen as a snippet, on a surface whose whole content-sensitivity question
	// is still open. tool_call args are excluded for the same reason.
	//
	// ⚠ SCOPED TO ONE agent_id, AND THE SCOPE IS THE SECURITY PROPERTY. The
	// caller is a panel showing one agent's own threads; an unscoped search would
	// return every agent's conversation with the operator, which is the exact
	// widening a neighbouring reader was narrowed to close.
	//
	// ⚠ A BLANK q RETURNS NO ROWS, not everything: the caller renders the plain
	// recent list for that case, and "an empty query dumps the table" is the shape
	// this reader exists not to have.
	SearchSessions(ctx context.Context, agentID int64, q string, limit int) ([]SessionMatch, error)
	// GetSession loads one session by id.
	GetSession(ctx context.Context, sessionID int64) (ChatSession, error)
	// LatestOrCreateSession returns the agent's most-recently-updated session, or
	// creates one if it has none. The first auto-created session reuses the LEGACY
	// gateway key (`agent:<name>:webchat`) so a pre-sessions agent keeps its
	// existing gateway context; subsequent sessions get fresh keys.
	LatestOrCreateSession(ctx context.Context, agentID int64, agentName string) (ChatSession, error)

	// AddChatMessage persists a message scoped to m.SessionID, bumps that
	// session's updated_at, and — when the session is still untitled and the
	// message is a user turn — names the session from the message's first line.
	AddChatMessage(ctx context.Context, m ChatMessage) (ChatMessage, error)
	ListChatMessages(ctx context.Context, sessionID int64) ([]ChatMessage, error)
	// ListRecentChatMessages returns the most recent limit messages for ONE
	// session in chronological order — the capped reader the page render uses so a
	// long transcript doesn't ship the whole history on every load.
	ListRecentChatMessages(ctx context.Context, sessionID int64, limit int) ([]ChatMessage, error)
	// ListChatMessagesPage returns up to limit messages for ONE session in
	// chronological order, ending immediately BEFORE beforeID. beforeID == 0 means
	// "the newest page", which makes this a strict generalisation of
	// ListRecentChatMessages rather than a second reader beside it — the PG
	// implementation of that method is one call to this one.
	//
	// 🔴 THE CURSOR IS THE MESSAGE id, NOT created_at, AND THAT IS NOT A DETAIL.
	// AddChatMessage persists an assistant turn as several rows (migration 0015:
	// narration text, tool_call, tool_result) written in one burst, so two rows in
	// one turn can share a created_at to whatever resolution the clock gives. A
	// timestamp cursor would then either re-serve or skip part of a turn at the page
	// boundary, and both failures look like a coherent conversation with a hole in
	// it. The id is the sequence the rows were written in and is unique by
	// construction.
	//
	// ⚠ A limit <= 0 is the CALLER's bug, not a request for everything: it returns
	// an empty page. ListRecentChatMessages' "<= 0 means the whole transcript"
	// fallback is deliberately NOT inherited — that reader is called by the page
	// render with a constant, while this one is reachable from a query string, and
	// "?limit=0 dumps the table" is the shape this route exists not to have.
	ListChatMessagesPage(ctx context.Context, sessionID, beforeID int64, limit int) ([]ChatMessage, error)
	// DeleteChatMessagesOlderThan removes chat messages created before cutoff and
	// returns the number removed (retention sweep).
	DeleteChatMessagesOlderThan(ctx context.Context, cutoff time.Time) (int64, error)

	// --- read-tracking / unread (agent-reply notifications) ---

	// MarkSessionRead stamps a session's read_at = now(), clearing its unread
	// assistant replies. Called when the human opens (or finishes a turn in) a
	// session.
	MarkSessionRead(ctx context.Context, sessionID int64) error
	// UnreadByAgent returns, per agent with ≥1 unread assistant message, the total
	// unread count and the session to deep-link to (the agent's most-recently-
	// active session). Ordered most-recent-unread first. Unread = an assistant
	// message whose created_at is after its session's read_at.
	UnreadByAgent(ctx context.Context) ([]AgentUnread, error)
}

// sessionTitleCap bounds an auto-derived session title length.
const sessionTitleCap = 48

// deriveSessionTitle builds a session title from a user message: the first
// non-empty line, trimmed and capped to sessionTitleCap runes (… on overflow).
func deriveSessionTitle(content string) string {
	line := strings.TrimSpace(content)
	for _, ln := range strings.Split(content, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			line = t
			break
		}
	}
	r := []rune(line)
	if len(r) > sessionTitleCap {
		return string(r[:sessionTitleCap]) + "…"
	}
	return line
}

// reverseChatChronological reverses an id-descending (newest-first) slice of
// messages in place into chronological (oldest-first) order. It backs the capped
// reader, which queries newest-first with a LIMIT then flips to chronological. It
// is pure so the ordering logic is unit-testable without a DB.
func reverseChatChronological(msgs []ChatMessage) {
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
}
