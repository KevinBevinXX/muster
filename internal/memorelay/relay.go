// Package memorelay RELAYS cairn scope memos to dispatched agents: muster reads the live memos of
// the scopes its agents work on from a cairn memo listener, with ONE read-only credential, and
// hands each agent each memo once, as a turn through the agent's own gateway.
//
// # Why muster relays, instead of each agent asking
//
// cairn's memos reach a host session through a hook that runs cairn's client with the host's own
// credential. An agent pod has neither: no client, no credential, and a network policy that keeps
// it off private addresses. Putting a credential in every pod would also multiply the one hazard
// the listener has — it locks a PEER out for 15 minutes after 5 refused credentials in a minute,
// and behind a node's masquerade every pod on that node is one peer, the node's own host sessions
// included. One process holding one credential is one backoff for the whole fleet ([Listener]).
//
// # How a memo reaches an agent
//
// Through [Gateway] — agentgateway.Gateway's Resolve and Send, the path an operator's chat message
// and a dispatch's first turn already travel. There is no second channel: a memo arrives as a turn,
// lands in the agent's transcript beside every other message, and waits for an agent that is
// mid-turn (the runtime's typed `busy` pastes nothing, and the next tick offers it again).
//
// ⚠ THAT WAIT IS A PROPERTY OF ONE RUNTIME, SO ONLY ITS AGENTS ARE SERVED. The claude-code
// runtime (cmd/ccd) refuses a turn that arrives during another before it pastes anything. No
// other kind is known to, so an agent of any other kind is counted ([ResultKindUnsupported])
// and sent nothing.
//
// What is delivered is a fenced block ([Render]): cairn's own fence, standing line and field
// replacement, re-applied here because the relay cannot know it is talking to a real listener.
//
// # Which agents, and which scope
//
// 🔴 ONLY THE OPERATOR'S AGENTS, AND THE DATABASE ENFORCES IT. The credential is the operator's
// view of their own cairn; an agent owned by any other user of this deployment must never be
// shown it. [Relay.Run] acts as authz.Operator, so row-level security hands it the operator's
// agents and nobody else's, and [Relay] checks the owner again itself (a store without row-level
// security — every fake — still has to be refused).
//
// An agent's scopes are derived, never configured per agent and never guessed: the repository the
// agent was dispatched for, else the repository its task names, through [ScopeForRepo] — and then
// through the deployment's [ScopeMap], which may name a SET of scopes for one repository (cairn
// files memos under a clone's directory name, which need not be the repository's). An agent with
// no repository gets nothing, and is counted ([ResultNoScope]).
//
// # Once
//
// "Already delivered" is a durable record per agent ROW ([Ledger]) — an agent's id is never
// reused, so a re-dispatch is a new agent and sees every live memo once. The record is written
// after the send, when it is true; [PGLedger] says what that order costs. A retracted or expired
// memo simply stops being listed by the listener, so it stops being delivered; one already
// delivered is not recalled (it is in the agent's context, as on a host).
//
// A delivery runs under the agent's session claim (agents.Store.ClaimKickoff — the claim a first
// turn holds), and re-reads the agent and the record under it. So a memo never takes the session
// from a first turn that is still being handed over, and two processes cannot both send one
// agent the same memo — unless the first one's record could not be written ([ResultRecordFailed]),
// which is the same exposure as a process killed before its record.
//
// The wire contract (`GET /client/v1/memos`, the memo's field names) and the block shape were read
// from cairn at revision f283823: internal/memowire, internal/memo/block.go, predicates.go.
package memorelay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/metrics"
)

// DefaultInterval is how often [Relay.Run] looks for memos to deliver. Each tick costs one
// listener request per distinct scope, one at a time.
const DefaultInterval = time.Minute

// DefaultTurnTimeout bounds one delivered turn for a kind with no budget of its own.
const DefaultTurnTimeout = 15 * time.Minute

// kindSlack is how much longer than a kind's per-request budget a delivery may run end to end.
const kindSlack = 2 * time.Minute

// bookkeepingTimeout bounds the writes that RECORD a delivery. A fresh budget, so a turn that
// used its whole deadline — or was cut off by shutdown — is still recorded.
const bookkeepingTimeout = 15 * time.Second

// The closed vocabulary of what one agent's check came to. Every listed agent is counted under
// exactly one, each tick.
const (
	ResultEligible    = "eligible"
	ResultForeign     = "foreign"      // a row this deployment's prefix did not produce
	ResultNotOperator = "not_operator" // owned by a user the credential does not belong to
	ResultNotRunning  = "not_running"  // any status but `running`
	// ResultKindUnsupported: a kind whose runtime is not KNOWN to refuse a turn while another
	// is running. Only the claude-code runtime answers a typed `busy` before pasting anything.
	ResultKindUnsupported   = "kind_unsupported"
	ResultKickoffOwed       = "kickoff_owed" // its first turn has not been handed over yet
	ResultNoToken           = "no_token"     // no gateway credential can be derived
	ResultNoScope           = "no_scope"     // neither the agent nor its task names a repository
	ResultScopeLookupFailed = "scope_lookup_failed"
)

// What one delivery came to (beyond [OutcomeDelivered] and [OutcomeUnconfirmed]). Every one but
// the last sends nothing, and the memos are offered again next tick.
const (
	ResultBusy           = agentgateway.UnsentBusy
	ResultNotReady       = agentgateway.UnsentNotReady
	ResultUnauthorized   = agentgateway.UnsentUnauthorized // the runtime refused muster's own bearer
	ResultRejected       = agentgateway.UnsentRejected     // the runtime refused the request's shape (a typed 400)
	ResultNeverConnected = agentgateway.UnsentNeverConnected
	ResultUnresolved     = "unresolved"
	ResultNoSession      = "no_session"
	// ResultSessionClaimed: another delivery holds the agent's session claim — its first turn
	// (internal/agentkickoff), another process's memo turn, or (the common case) THIS process's
	// own earlier memo turn, still running: once per tick for as long as it runs.
	ResultSessionClaimed = "session_claimed"
	// ResultClaimFailed: the claim could not be asked for at all (the database). Nothing is
	// sent on a claim that was not verified.
	ResultClaimFailed = "claim_failed"
	// ResultMoved: under the claim, the agent was no longer eligible for this scope, or had
	// already been sent everything in the batch.
	ResultMoved            = "moved"
	ResultLedgerUnreadable = "ledger_unreadable"
	ResultRecordFailed     = "record_failed"
)

// claimMargin is how much longer than a turn the agent's session claim lives, so a healthy turn
// always finishes inside its own claim. The TTL is the crash backstop, not the normal path.
const claimMargin = 2 * time.Minute

// Source is where live memos come from. [Listener] is the production one.
type Source interface {
	Live(ctx context.Context, scope string) ([]Memo, error)
}

// Gateway is a delivered turn's two halves, exactly internal/agentkickoff's: Resolve contacts no
// runtime, Send runs the turn. agentgateway.Gateway satisfies it.
type Gateway interface {
	Resolve(ctx context.Context, a agents.Agent) (agentgateway.Target, error)
	Send(ctx context.Context, t agentgateway.Target, sessionKey, message string, emit func(string)) (string, error)
}

// Config is everything the relay needs. Every field without a stated default is required.
type Config struct {
	// Agents is the agents store. REQUIRED.
	Agents agents.Store
	// TaskRepo returns the `owner/name` a task names ("" when it names none or no longer
	// exists). REQUIRED: it is the second half of the scope derivation.
	TaskRepo func(ctx context.Context, taskID int64) (string, error)
	// Gateway runs the turn. REQUIRED.
	Gateway Gateway
	// Source lists a scope's live memos. REQUIRED.
	Source Source
	// Ledger records what each agent was sent. REQUIRED.
	Ledger Ledger
	// NamespacePrefix is the deployment's configured agent namespace prefix; a row is this
	// deployment's iff its namespace is agents.NamespaceFor(prefix, name).
	NamespacePrefix string
	// ScopeMap maps a repository's derived scope to the set of scopes its agents read. Optional:
	// nil reads each repository's derived scope alone. See [ScopeMap].
	ScopeMap ScopeMap
	// Logger is optional. No line it is given ever carries memo text.
	Logger *log.Logger
}

// Relay delivers memos. Build it with [New].
type Relay struct {
	agents   agents.Store
	taskRepo func(context.Context, int64) (string, error)
	gw       Gateway
	src      Source
	ledger   Ledger
	prefix   string
	scopes   ScopeMap
	// owner names this process in an agent's session claim (hostname/random).
	owner string
	log   *log.Logger
	turn  time.Duration

	wg       sync.WaitGroup
	done     chan struct{}
	doneOnce sync.Once

	mu sync.Mutex
	// sent is what THIS PROCESS has sent each agent. It backs the ledger up: a memo whose record
	// could not be written is still never sent twice by the process that sent it.
	sent map[int64]map[Key]bool
	// said is the last non-eligible result logged per agent, so a standing reason is logged once.
	said   map[int64]string
	counts map[string]int
}

// New validates the configuration and builds the relay.
func New(cfg Config) (*Relay, error) {
	switch {
	case cfg.Agents == nil:
		return nil, errors.New("memorelay: Config.Agents is required")
	case cfg.TaskRepo == nil:
		return nil, errors.New("memorelay: Config.TaskRepo is required (an agent dispatched from a task " +
			"has no repository of its own to derive a scope from)")
	case cfg.Gateway == nil:
		return nil, errors.New("memorelay: Config.Gateway is required (a memo is delivered as a turn)")
	case cfg.Source == nil:
		return nil, errors.New("memorelay: Config.Source is required")
	case cfg.Ledger == nil:
		return nil, errors.New("memorelay: Config.Ledger is required (without a durable record every " +
			"restart would deliver every live memo again)")
	}
	r := &Relay{
		agents: cfg.Agents, taskRepo: cfg.TaskRepo, gw: cfg.Gateway, src: cfg.Source, ledger: cfg.Ledger,
		prefix: agents.ResolveNamespacePrefix(cfg.NamespacePrefix),
		scopes: cfg.ScopeMap,
		log:    cfg.Logger, turn: DefaultTurnTimeout,
		done: make(chan struct{}),
		sent: map[int64]map[Key]bool{}, said: map[int64]string{},
		counts: map[string]int{},
	}
	if r.log == nil {
		r.log = log.New(io.Discard, "", 0)
	}
	host, _ := os.Hostname()
	buf := make([]byte, 6)
	_, _ = rand.Read(buf)
	r.owner = host + "/memorelay/" + hex.EncodeToString(buf)
	return r, nil
}

// Run ticks every interval until ctx is cancelled, then waits for in-flight deliveries to record
// their outcome and closes [Relay.Done].
func (r *Relay) Run(ctx context.Context, interval time.Duration) {
	// 🔴 AS THE OPERATOR, NOT AS THE SYSTEM. See the package doc: row-level security then hands
	// this loop the operator's agents, tasks and audit rows and nothing of anyone else's.
	ctx = authz.Operator(ctx)
	if interval <= 0 {
		interval = DefaultInterval
	}
	defer r.doneOnce.Do(func() { close(r.done) })
	defer r.wg.Wait()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := r.Tick(ctx); err != nil && ctx.Err() == nil {
			r.log.Printf("memorelay: tick: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ScopeMap is the map this relay classifies with — what the wiring handed it, for the wiring's
// own test to read back.
func (r *Relay) ScopeMap() ScopeMap { return r.scopes }

// Wait blocks until every delivery started by [Relay.Tick] has finished.
func (r *Relay) Wait() { r.wg.Wait() }

// Done is closed once [Relay.Run] has returned — after every delivery it started has recorded its
// outcome. The process's shutdown waits on it before closing the database pool, so a turn cut off
// by a redeploy is recorded rather than delivered again by the next process.
func (r *Relay) Done() <-chan struct{} { return r.done }

// Counts is a snapshot of everything counted so far, keyed "<family>/<result>" with the families
// `agent`, `upstream` and `delivery`. The same increments feed internal/metrics.
func (r *Relay) Counts() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]int, len(r.counts))
	for k, v := range r.counts {
		out[k] = v
	}
	return out
}

const (
	familyAgent    = "agent"
	familyUpstream = "upstream"
	familyDelivery = "delivery"
)

func (r *Relay) count(family, result string) {
	switch family {
	case familyAgent:
		metrics.MemoRelayAgentChecks.WithLabelValues(result).Inc()
	case familyUpstream:
		metrics.MemoRelayUpstream.WithLabelValues(result).Inc()
	case familyDelivery:
		metrics.MemoRelayDeliveries.WithLabelValues(result).Inc()
	}
	r.mu.Lock()
	r.counts[family+"/"+result]++
	r.mu.Unlock()
}

// Tick makes one pass: classify every agent, ask the listener once per distinct scope, and start
// a delivery for each agent with memos it has not been sent.
//
// 🔴 A REFUSED CREDENTIAL ENDS THE TICK. The listener is asked for nothing more — not for the
// next scope, not on the next tick — until [RefusedBackoff] has passed; [Listener] holds that
// rule, and this loop simply stops asking when told. An unreachable listener ends the tick too
// (each further scope would only wait out another timeout).
func (r *Relay) Tick(ctx context.Context) error {
	rows, err := r.agents.List(ctx)
	if err != nil {
		return fmt.Errorf("list agents: %w", err)
	}
	byScope := map[string][]agents.Agent{}
	listed := map[int64]bool{}
	for _, a := range rows {
		listed[a.ID] = true
		scopes, result := r.classify(ctx, a)
		r.count(familyAgent, result)
		r.say(a, result)
		if result == ResultEligible {
			// An agent may read several scopes (Config.ScopeMap). Each is offered on its own:
			// a memo's identity is (scope, id), so the two scopes' memos are distinct and the
			// ledger keeps each from repeating.
			for _, scope := range scopes {
				byScope[scope] = append(byScope[scope], a)
			}
		}
	}
	r.forget(listed)

	scopes := make([]string, 0, len(byScope))
	for s := range byScope {
		scopes = append(scopes, s)
	}
	sort.Strings(scopes)
	for _, scope := range scopes {
		memos, err := r.src.Live(ctx, scope)
		r.count(familyUpstream, upstreamResult(err))
		switch {
		case err == nil:
		case errors.Is(err, ErrScopeBackedOff):
			// Said when the miss happened; nothing was sent now.
			continue
		case errors.Is(err, ErrScopeUnreadable), errors.Is(err, ErrMalformed), errors.Is(err, ErrUnexpectedStatus):
			// An ANSWER about this scope says nothing about the next one's, so the tick goes on
			// — and it is logged, because each of these stops every delivery on the scope (a
			// wrong listener URL, a credential not narrowed to it, a clone kept under another
			// name, a wire the listener changed) with a counter as the only other sign. The
			// error names a status, a defect or a memo id; never memo text.
			r.log.Printf("memorelay: scope %s: nothing delivered: %v", scope, err)
			continue
		default:
			if errors.Is(err, ErrRefused) {
				r.log.Printf("%s", refusedLine)
			} else if errors.Is(err, ErrUnreachable) {
				r.log.Printf("memorelay: %v", err)
			}
			return nil
		}
		for _, a := range byScope[scope] {
			r.offer(ctx, a, scope, memos)
		}
	}
	return nil
}

// refusedLine is what the relay logs on a 401. It is the operator's whole instruction at that
// moment, so it is pinned whole (TestARefusalTellsTheOperatorToRestartOnce).
//
// 🔴 IT SAYS "RESTART ONCE" BECAUSE THE TOKEN IS READ ONCE. MUSTER_MEMO_TOKEN comes from the
// environment at start and nothing reloads it, so a corrected Secret is never seen by a running
// process: after the backoff it sends the OLD token again. An earlier wording said "check the
// token, then wait: a corrected one is not tried sooner", which led an operator to fix the Secret
// and wait — for ever, one refused request in the shared lockout bucket every 15 minutes.
var refusedLine = "memorelay: the listener REFUSED the relay's credential; nothing is asked of it for " +
	RefusedBackoff.String() + ". The token is read once, at start: fix the Secret behind " +
	"MUSTER_MEMO_TOKEN, then restart muster ONCE. Until then this process re-sends the same token " +
	"every " + RefusedBackoff.String() + ". Never restart in a loop: each refused request counts " +
	"toward the listener's lockout of this node."

// upstreamResult names what one listener call came to.
func upstreamResult(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ErrNotYet):
		return "not_yet"
	case errors.Is(err, ErrBackedOff):
		return "backed_off"
	case errors.Is(err, ErrRefused):
		return "refused"
	case errors.Is(err, ErrScopeUnreadable):
		return "scope_unreadable"
	case errors.Is(err, ErrScopeBackedOff):
		return "scope_backed_off"
	case errors.Is(err, ErrMalformed):
		return "malformed"
	case errors.Is(err, ErrUnexpectedStatus):
		return "unexpected_status"
	default:
		return "unreachable"
	}
}

// classify answers whether a is sent memos at all and, if so, for which scopes (sorted, never
// empty when eligible). The order is the order of the claims: whose row it is, whose agent it
// is, whether it can take a turn, and only then what it works on.
func (r *Relay) classify(ctx context.Context, a agents.Agent) (scopes []string, result string) {
	switch {
	case a.Namespace != agents.NamespaceFor(r.prefix, a.Name):
		return nil, ResultForeign
	case a.OwnerID != authz.OperatorUserID:
		return nil, ResultNotOperator
	case a.Status != agents.StatusRunning:
		return nil, ResultNotRunning
	case agents.ResolveKind(a.Kind) != agents.KindClaudeCode:
		// 🔴 A MEMO IS NEVER POSTED INTO A RUNTIME THAT MIGHT BE MID-TURN. "Wait for an idle
		// agent" rests on the runtime REFUSING a second turn before it pastes anything, and
		// only ccd is known to (a typed 409 `busy`). What any other runtime does with a turn
		// that arrives during another is not established, so its agents are counted, not sent.
		return nil, ResultKindUnsupported
	case agents.KickoffOwed(a):
		// Its first turn has not been handed over. ⚠ This excludes only the row that is not
		// yet STAMPED: a first turn is stamped before it is sent and then re-sent for minutes
		// while the CLI starts, and through all of that the row reads as kicked off. What
		// keeps a memo out of THAT window is the session claim deliver takes.
		return nil, ResultKickoffOwed
	case a.HooksToken == "":
		return nil, ResultNoToken
	}
	if s := r.scopes.scopesFor(a.Repo); len(s) > 0 {
		return s, ResultEligible
	}
	if a.NoteID == nil {
		return nil, ResultNoScope
	}
	repo, err := r.taskRepo(ctx, *a.NoteID)
	if err != nil {
		return nil, ResultScopeLookupFailed
	}
	if s := r.scopes.scopesFor(repo); len(s) > 0 {
		return s, ResultEligible
	}
	return nil, ResultNoScope
}

// say logs a non-eligible result once per agent, and again only when it changes.
func (r *Relay) say(a agents.Agent, result string) {
	r.mu.Lock()
	prev := r.said[a.ID]
	r.said[a.ID] = result
	r.mu.Unlock()
	if result == prev || result == ResultEligible {
		return
	}
	r.log.Printf("memorelay: agent %d (%s) is sent no memos: %s", a.ID, a.Name, result)
}

// forget drops per-agent memory of agents that are no longer listed.
func (r *Relay) forget(listed map[int64]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id := range r.said {
		if !listed[id] {
			delete(r.said, id)
		}
	}
	for id := range r.sent {
		if !listed[id] {
			delete(r.sent, id)
		}
	}
}

// offer starts a delivery to a of the live memos it has no record of, oldest first, at most
// [MaxPerDelivery] of them.
//
// 🔴 AN UNREADABLE LEDGER DELIVERS NOTHING. "Could not read what was delivered" is not "nothing
// was delivered": treating it so would send every live memo again on every tick the database
// was unwell.
//
// ⚠ THERE IS NO IN-PROCESS IN-FLIGHT SET, for internal/agentkickoff's reason: the agent's session
// claim already excludes a second delivery, in this process and in any other, and a map kept
// beside it duplicated that for one process only (it was written, and a mutant deleting it
// survived). A tick that finds an agent still mid-turn starts a goroutine that loses the claim
// and returns. Pinned by TestAnAgentMidDeliveryIsNotStartedOnAgain.
func (r *Relay) offer(ctx context.Context, a agents.Agent, scope string, live []Memo) {
	if len(live) == 0 {
		return
	}
	pending, ok := r.pending(ctx, a, scope, live)
	if !ok || len(pending) == 0 {
		return
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.deliver(ctx, a, scope, pending)
	}()
}

// pending is the live memos a has no record of — in the ledger or in this process's memory —
// oldest first, at most [MaxPerDelivery]. ok is false when the ledger could not be read.
func (r *Relay) pending(ctx context.Context, a agents.Agent, scope string, live []Memo) (out []Memo, ok bool) {
	recorded, err := r.ledger.Delivered(ctx, a.ID)
	if err != nil {
		r.count(familyDelivery, ResultLedgerUnreadable)
		r.log.Printf("memorelay: agent %d (%s): could not read what it was already sent; sending nothing: %v",
			a.ID, a.Name, err)
		return nil, false
	}
	r.mu.Lock()
	for _, m := range live {
		k := Key{Scope: scope, ID: m.ID}
		if !recorded[k] && !r.sent[a.ID][k] {
			out = append(out, m)
		}
	}
	r.mu.Unlock()
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].created.Equal(out[j].created) {
			return out[i].created.Before(out[j].created)
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > MaxPerDelivery {
		out = out[:MaxPerDelivery]
	}
	return out, true
}

// deliver runs one turn carrying batch and records it.
//
// The order is the design. Everything that can fail without contacting the agent's runtime
// happens first and is simply offered again next tick. A send that provably pasted nothing (see
// [unsent]) is offered again too. Anything else — success, or a failure after the text may have
// been pasted — is recorded, and those memos are never sent to this agent again.
func (r *Relay) deliver(parent context.Context, a agents.Agent, scope string, batch []Memo) {
	turn := r.turn
	if k := agents.KindTurnTimeout(a.Kind); k > 0 && k+kindSlack > turn {
		turn = k + kindSlack
	}
	ctx, cancel := context.WithTimeout(parent, turn)
	defer cancel()

	// 🔴 THE AGENT'S SESSION CLAIM IS TAKEN FIRST, AND IT IS THE SAME CLAIM A FIRST TURN HOLDS
	// (agents.Store.ClaimKickoff). internal/agentkickoff stamps a row kicked-off BEFORE it sends
	// the first turn and then re-sends it for minutes while the CLI starts; all that time the
	// row reads as eligible here. A memo that took the session in that window would leave the
	// first turn answered `busy` — recorded as failed, never retried — and the agent would never
	// be told its task. The deliverer holds this claim for its whole delivery, so losing it IS
	// "a first turn is in flight"; and holding it makes a first turn wait for the memo's.
	//
	// It is also what makes "once" hold across PROCESSES: two replicas cannot be inside this
	// section for one agent, and the winner re-reads the row and the ledger below.
	won, err := r.agents.ClaimKickoff(ctx, a.ID, r.owner, turn+claimMargin)
	if err != nil {
		r.count(familyDelivery, ResultClaimFailed)
		r.log.Printf("memorelay: agent %d (%s): the session claim could not be taken; sending nothing: %v", a.ID, a.Name, err)
		return
	}
	if !won {
		r.count(familyDelivery, ResultSessionClaimed)
		return
	}
	defer func() {
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(parent), bookkeepingTimeout)
		defer rcancel()
		if err := r.agents.ReleaseKickoffClaim(rctx, a.ID, r.owner); err != nil {
			r.log.Printf("memorelay: agent %d (%s): release the session claim: %v", a.ID, a.Name, err)
		}
	}()

	// 🔴 RE-READ UNDER THE CLAIM. The row and the ledger were read before it; a first turn may
	// have been owed and delivered in between, and another process may have sent this batch.
	fresh, err := r.agents.Get(ctx, a.ID)
	if err != nil {
		r.count(familyDelivery, ResultUnresolved)
		return
	}
	if s, result := r.classify(ctx, fresh); result != ResultEligible || !slices.Contains(s, scope) {
		r.count(familyDelivery, ResultMoved)
		return
	}
	a = fresh
	batch, ok := r.pending(ctx, a, scope, batch)
	if !ok {
		return
	}
	if len(batch) == 0 {
		r.count(familyDelivery, ResultMoved)
		return
	}

	target, err := r.gw.Resolve(ctx, a)
	if err != nil {
		r.count(familyDelivery, ResultUnresolved)
		return
	}
	sess, err := r.agents.LatestOrCreateSession(ctx, a.ID, a.Name)
	if err != nil {
		r.count(familyDelivery, ResultNoSession)
		return
	}

	message := Render(scope, batch)
	reply, err := r.gw.Send(ctx, target, sess.SessionKey, message, nil)
	if result := unsent(err); result != "" {
		r.count(familyDelivery, result)
		return
	}
	outcome := OutcomeDelivered
	if err != nil {
		outcome = OutcomeUnconfirmed
	}

	ids := make([]int64, len(batch))
	r.mu.Lock()
	if r.sent[a.ID] == nil {
		r.sent[a.ID] = map[Key]bool{}
	}
	for i, m := range batch {
		ids[i] = m.ID
		r.sent[a.ID][Key{Scope: scope, ID: m.ID}] = true
	}
	r.mu.Unlock()

	// 🔴 A DETACHED BUDGET THAT KEEPS parent's VALUES. The failure most worth recording — a turn
	// cut off by shutdown — is the one whose ctx is already cancelled; and parent carries the
	// operator principal row-level security scopes every write to.
	bctx, bcancel := context.WithTimeout(context.WithoutCancel(parent), bookkeepingTimeout)
	defer bcancel()
	if err := r.ledger.Record(bctx, a, scope, ids, outcome); err != nil {
		r.count(familyDelivery, ResultRecordFailed)
		r.log.Printf("memorelay: agent %d (%s): memos %v on %s were sent (%s) but the record could NOT be "+
			"written: %v. This process will not send them again; a restarted one will", a.ID, a.Name, ids, scope, outcome, err)
	}
	_, _ = r.agents.AddChatMessage(bctx, agents.ChatMessage{AgentID: a.ID, SessionID: sess.ID, Role: "user", Content: message})
	if reply != "" {
		_, _ = r.agents.AddChatMessage(bctx, agents.ChatMessage{AgentID: a.ID, SessionID: sess.ID, Role: "assistant", Content: reply})
	}
	r.count(familyDelivery, outcome)
	r.log.Printf("memorelay: agent %d (%s): memos %v on %s: %s%s", a.ID, a.Name, ids, scope, outcome, failureNote(err))
}

// unsent names a send failure that PROVES nothing was pasted into the agent's session, or ""
// for success and for every failure that does not prove it. The rule is
// agentgateway.Unsent's (moved there so the plugin deliverer reads the same one); its reason
// names ARE this package's Result* values, which are declared as those constants.
func unsent(err error) string { return agentgateway.Unsent(err) }

// failureNote describes a send failure for the log WITHOUT quoting it: a runtime's error can
// carry a snippet of its response body, and a runtime that echoes its request would put memo
// text in the pod log.
func failureNote(err error) string {
	if err == nil {
		return ""
	}
	var rt *agents.RuntimeError
	switch {
	case errors.As(err, &rt):
		return fmt.Sprintf(" (the runtime answered HTTP %d, type %q)", rt.Status, rt.Type)
	case errors.Is(err, context.DeadlineExceeded):
		return " (the turn outlived its budget)"
	case errors.Is(err, context.Canceled):
		return " (the turn was cut off, most likely by this process shutting down)"
	default:
		return fmt.Sprintf(" (the send failed: %T)", err)
	}
}
