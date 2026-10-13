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
// An agent's scope is derived, never configured per agent and never guessed: the repository the
// agent was dispatched for, else the repository its task names, through [ScopeForRepo]. An agent
// with neither gets nothing, and is counted ([ResultNoScope]).
//
// # Once
//
// "Already delivered" is a durable record per agent ROW ([Ledger]) — an agent's id is never
// reused, so a re-dispatch is a new agent and sees every live memo once. The record is written
// after the send, when it is true; [PGLedger] says what that order costs. A retracted or expired
// memo simply stops being listed by the listener, so it stops being delivered; one already
// delivered is not recalled (it is in the agent's context, as on a host).
//
// ⚠ ONE PROCESS. Nothing here takes a cross-process claim: two replicas ticking together could
// each send an agent the same memo before either records it. muster runs one replica (its
// background loops have no leader lease), and a new process sends nothing for [StartDelay].
//
// The wire contract (`GET /client/v1/memos`, the memo's field names) and the block shape were read
// from cairn at revision f283823: internal/memowire, internal/memo/block.go, predicates.go.
package memorelay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
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
	ResultEligible          = "eligible"
	ResultForeign           = "foreign"      // a row this deployment's prefix did not produce
	ResultNotOperator       = "not_operator" // owned by a user the credential does not belong to
	ResultNotRunning        = "not_running"  // any status but `running`
	ResultKickoffOwed       = "kickoff_owed" // its first turn has not been handed over yet
	ResultNoToken           = "no_token"     // no gateway credential can be derived
	ResultNoScope           = "no_scope"     // neither the agent nor its task names a repository
	ResultScopeLookupFailed = "scope_lookup_failed"
)

// What one delivery came to (beyond [OutcomeDelivered] and [OutcomeUnconfirmed]). The first five
// send nothing and are offered again next tick.
const (
	ResultBusy             = "busy"
	ResultNotReady         = "not_ready"
	ResultNeverConnected   = "never_connected"
	ResultUnresolved       = "unresolved"
	ResultNoSession        = "no_session"
	ResultLedgerUnreadable = "ledger_unreadable"
	ResultRecordFailed     = "record_failed"
)

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
	// Logger is optional. No line it is given ever carries memo text.
	Logger *log.Logger
	// TurnTimeout defaults to [DefaultTurnTimeout].
	TurnTimeout time.Duration
}

// Relay delivers memos. Build it with [New].
type Relay struct {
	agents   agents.Store
	taskRepo func(context.Context, int64) (string, error)
	gw       Gateway
	src      Source
	ledger   Ledger
	prefix   string
	log      *log.Logger
	turn     time.Duration

	wg       sync.WaitGroup
	done     chan struct{}
	doneOnce sync.Once

	mu sync.Mutex
	// inFlight holds the agents a delivery is running for: a second one is not started, since
	// the runtime would only answer `busy`.
	inFlight map[int64]bool
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
		log:    cfg.Logger, turn: cfg.TurnTimeout,
		done:     make(chan struct{}),
		inFlight: map[int64]bool{}, sent: map[int64]map[Key]bool{}, said: map[int64]string{},
		counts: map[string]int{},
	}
	if r.log == nil {
		r.log = log.New(io.Discard, "", 0)
	}
	if r.turn <= 0 {
		r.turn = DefaultTurnTimeout
	}
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
		scope, result := r.classify(ctx, a)
		r.count(familyAgent, result)
		r.say(a, result)
		if result == ResultEligible {
			byScope[scope] = append(byScope[scope], a)
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
		case errors.Is(err, ErrScopeUnreadable), errors.Is(err, ErrMalformed):
			// This scope's answer says nothing about the next one's.
			continue
		default:
			if errors.Is(err, ErrRefused) {
				r.log.Printf("memorelay: the listener REFUSED the relay's credential; nothing is asked of it "+
					"for %s. Check the token, then wait: a corrected one is not tried sooner", RefusedBackoff)
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
	case errors.Is(err, ErrMalformed):
		return "malformed"
	default:
		return "unreachable"
	}
}

// classify answers whether a is sent memos at all and, if so, for which scope. The order is the
// order of the claims: whose row it is, whose agent it is, whether it can take a turn, and only
// then what it works on.
func (r *Relay) classify(ctx context.Context, a agents.Agent) (scope, result string) {
	switch {
	case a.Namespace != agents.NamespaceFor(r.prefix, a.Name):
		return "", ResultForeign
	case a.OwnerID != authz.OperatorUserID:
		return "", ResultNotOperator
	case a.Status != agents.StatusRunning:
		return "", ResultNotRunning
	case agents.KickoffOwed(a):
		// Its first turn is the task itself; a memo must not take the session ahead of it.
		return "", ResultKickoffOwed
	case a.HooksToken == "":
		return "", ResultNoToken
	}
	if s := ScopeForRepo(a.Repo); s != "" {
		return s, ResultEligible
	}
	if a.NoteID == nil {
		return "", ResultNoScope
	}
	repo, err := r.taskRepo(ctx, *a.NoteID)
	if err != nil {
		return "", ResultScopeLookupFailed
	}
	if s := ScopeForRepo(repo); s != "" {
		return s, ResultEligible
	}
	return "", ResultNoScope
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
		if !listed[id] && !r.inFlight[id] {
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
func (r *Relay) offer(ctx context.Context, a agents.Agent, scope string, live []Memo) {
	if len(live) == 0 || !r.begin(a.ID) {
		return
	}
	started := false
	defer func() {
		if !started {
			r.end(a.ID)
		}
	}()
	recorded, err := r.ledger.Delivered(ctx, a.ID)
	if err != nil {
		r.count(familyDelivery, ResultLedgerUnreadable)
		r.log.Printf("memorelay: agent %d (%s): could not read what it was already sent; sending nothing: %v",
			a.ID, a.Name, err)
		return
	}
	var pending []Memo
	r.mu.Lock()
	for _, m := range live {
		k := Key{Scope: scope, ID: m.ID}
		if !recorded[k] && !r.sent[a.ID][k] {
			pending = append(pending, m)
		}
	}
	r.mu.Unlock()
	if len(pending) == 0 {
		return
	}
	sort.SliceStable(pending, func(i, j int) bool {
		if !pending[i].created.Equal(pending[j].created) {
			return pending[i].created.Before(pending[j].created)
		}
		return pending[i].ID < pending[j].ID
	})
	if len(pending) > MaxPerDelivery {
		pending = pending[:MaxPerDelivery]
	}
	started = true
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer r.end(a.ID)
		r.deliver(ctx, a, scope, pending)
	}()
}

func (r *Relay) begin(agentID int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inFlight[agentID] {
		return false
	}
	r.inFlight[agentID] = true
	return true
}

func (r *Relay) end(agentID int64) {
	r.mu.Lock()
	delete(r.inFlight, agentID)
	r.mu.Unlock()
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
// for success and for every failure that does not prove it.
//
//   - a typed 409 `busy` and a typed 503 `not_ready` are the runtime's own refusals, answered
//     before it pastes anything. Only its typed body sets Type, so an untyped 409 or 503 from
//     anything else is NOT read as "nothing was sent";
//   - a dial error is a connection that was never opened, so no byte of the request left.
func unsent(err error) string {
	if err == nil {
		return ""
	}
	var rt *agents.RuntimeError
	if errors.As(err, &rt) {
		switch {
		case rt.Status == http.StatusConflict && rt.Type == "busy":
			return ResultBusy
		case rt.Status == http.StatusServiceUnavailable && rt.Type == "not_ready":
			return ResultNotReady
		}
		return ""
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return ResultNeverConnected
	}
	return ""
}

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
