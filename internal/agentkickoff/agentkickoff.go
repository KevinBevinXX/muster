// Package agentkickoff DELIVERS an agent's first turn: it hands the note a
// dispatch was created with (agents.Agent.PendingNote) to the agent's model
// gateway once the agent's instance is ready, and records what happened on the row.
//
// 🔴 IT IS A BACKGROUND RECONCILE LOOP, NOT A STEP IN THE DISPATCH REQUEST, AND
// THE CODE THAT ALREADY EXISTED DECIDED THAT. agents.DecideReconcile is a
// per-tick decision table over (row, live instance, now): "a ready instance whose
// kickoff was never sent → retry while young, error once it has dwelt past
// ProvisioningStuckTimeout". That is the shape of a loop, and the alternatives are
// worse on their own terms:
//
//   - Dispatch runs inside a handler goroutine with a 5-minute operation budget
//     (agentprovision.DefaultOpTimeout) and returns when the Deployment is APPLIED,
//     not when the pod is ready. Delivering there means either blocking that
//     goroutine on image pull + repository clone + runtime boot, or giving up on a
//     slow-but-healthy pod. A loop simply looks again next tick.
//   - A first turn that fails BEFORE it is sent (no instance yet, no token) is
//     retried by the loop for free, and the table's dwell bound is what eventually
//     turns "never delivered" into a visible `error` — a mechanism a request-scoped
//     call has no equivalent of.
//   - Start of a saved agent owes the same first turn (agentprovision.Adapter.Start
//     says so) and has no request that waits for readiness either. One deliverer
//     serves both doors.
//
// 🔴 SINGLE-FLIGHT IS THE PER-AGENT CLAIM, NOT A LEADER LEASE. Two replicas
// ticking against one database both see `ready && !KickedOff`; each must win
// agents.Store.ClaimKickoff (a conditional UPDATE — a compare-and-set on the row)
// before doing anything, and the winner RE-READS the row under the claim, so a
// replica that lost the race or arrived after a delivery does nothing. No lease is
// needed and none exists in this module.
//
// 🔴 KickedOff IS STAMPED *BEFORE* THE MODEL TURN, SO A PAID TURN IS NEVER RUN
// TWICE. Once the stamp lands, agents.DecideReconcile stops returning
// ActionRetryKickoff for the row and this package never touches it again — even if
// the turn then fails, times out, or this process dies mid-turn. The cost is the
// mirror image and it is accepted: a turn that fails AFTER the stamp is recorded in
// agents.kickoff_error and is NOT retried automatically. Everything muster can check
// without opening a connection to the agent runtime — the token, the claim, a chat
// session, and the agent's address and credential (agentgateway.Gateway.Resolve,
// which for the k8s driver reads the Deployment from the Kubernetes API) — is checked
// before the stamp, and a failure there is retried.
//
// ⚠ ONE FAILURE THAT COSTS NOTHING STILL LANDS AFTER THE STAMP: a connection to the
// runtime that cannot be OPENED (DNS, refused). It happens inside the send, so it is
// recorded, as agents.KickoffNeverConnectedReason, and not retried. Retrying it would
// mean un-stamping a row that RecordKickoffDelivery has already counted as an
// attempt — a second write after the point of no return, and the kicked_off=f with
// attempts>0 state that round 0 deleted a guard for because no writer produced it.
// What the record buys instead is a remedy that says re-sending is safe
// (agents.KickoffResendSafe), which is true for this cause and for the next one.
//
// ⚠ AND ONE IS RE-SENT IN PLACE: a typed `not_ready` (ccd, before the Claude Code
// CLI is at its prompt — answered before anything is pasted). deliver re-sends it
// within the same delivery, under the same stamp, for a bounded wait; one that
// outlasts the bound is recorded as agents.KickoffNotAcceptedReason, also
// resend-safe. Nothing is un-stamped.
//
// 🔴 AND THAT UNRETRIED FAILURE IS VISIBLE, NOT JUST RECORDED (an operator
// decision). The stamp clears the "kickoff owed" badge, so before this a failed
// turn left a card reading healthy with the failure only in a column no page
// showed. agents.KickoffFailed (kicked_off AND kickoff_error set) now drives a
// "kickoff failed" badge carrying the error text and a remedy, and `kickoffFailed`
// on GET /api/agents. The remedy says re-sending by hand is safe only when nothing
// was sent (agents.KickoffResendSafe); for every other cause it says to check
// whether the agent is already working first, because the runtime may still be
// running the turn. A turn cut off by THIS process shutting down is recorded as such
// ([ShutdownCancelledReason]), and the server's shutdown waits for that record
// ([Deliverer.Done]) before it closes the pool. Each of these changes calls
// [Config.OnChange], so an open Agents list re-renders.
//
// 🔴 IT ACTS ONLY ON ROWS THIS DEPLOYMENT OWNS, AND NEVER ON A KICKED-OFF ROW. The
// agents table can hold rows another system wrote (a namespace the configured
// prefix did not produce); escalating those to `error` would be this process
// asserting a verdict about pods it cannot see. And a kicked-off row's recovery
// (agents.ActionResendKickoff / ActionErrorKickoffLost) is a re-delivery decision
// this package deliberately does not take — see [owesFirstTurn].
//
// 🔴 AN EMPTY REPLY IS A FAILED DELIVERY. The agent runtime's /v1/responses is
// known to discard its own successful retry for reasoning models and answer 200
// with no text; recording that as delivered would report an agent that was told
// its task when nothing says it was. It lands in agents.kickoff_error.
package agentkickoff

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/provision"
)

// DefaultInterval is how often [Deliverer.Run] looks for an owed first turn.
// A dispatched pod takes tens of seconds to become ready, so this adds at most
// that much latency to a first turn while costing two list reads per tick.
const DefaultInterval = 10 * time.Second

// DefaultTurnTimeout bounds one delivered first turn, end to end. The gateway's
// own HTTP client bounds each REQUEST; this bounds the turn this package owns.
const DefaultTurnTimeout = 15 * time.Minute

// claimMargin is how much longer than a turn the per-agent claim lives, so a
// healthy turn always finishes inside its own claim.
const claimMargin = 2 * time.Minute

// bookkeepingTimeout bounds each store write that RECORDS an outcome. It is a
// fresh budget so a turn that consumed its whole deadline can still be recorded.
const bookkeepingTimeout = 15 * time.Second

// EmptyReplyReason is written to agents.kickoff_error when the first turn came
// back with no text. It is agents.KickoffEmptyReplyReason, which the card's remedy
// reads.
const EmptyReplyReason = agents.KickoffEmptyReplyReason

// InstanceLister is what the deliverer needs from a provisioner: the live
// instances, so it can tell a ready recipient from one that is not.
// agentprovision.Adapter.Instances satisfies it.
type InstanceLister interface {
	Instances(ctx context.Context) ([]provision.Instance, error)
}

// Gateway is a first turn's two halves: Resolve (no connection to the runtime) and
// Send (the turn). Together they are agentgateway.Gateway.Chat, the method
// POST /api/agents/{name}/messages calls, so the kickoff travels the same path an
// operator's message does. agentgateway.Gateway satisfies it.
//
// 🔴 THEY ARE SEPARATE SO RESOLVE CAN RUN BEFORE THE STAMP. See [Deliverer.deliver].
type Gateway interface {
	Resolve(ctx context.Context, a agents.Agent) (agentgateway.Target, error)
	Send(ctx context.Context, t agentgateway.Target, sessionKey, message string, emit func(string)) (string, error)
}

// Config is everything the deliverer needs.
type Config struct {
	// Store is the agents store. REQUIRED.
	Store agents.Store
	// Instances lists live instances. REQUIRED.
	Instances InstanceLister
	// Gateway runs the turn. REQUIRED.
	Gateway Gateway
	// NamespacePrefix is the deployment's CONFIGURED agent namespace prefix — the
	// same value the row-writing path and the driver use. A row is this
	// deployment's iff its stored namespace is agents.NamespaceFor(prefix, name).
	NamespacePrefix string
	// Owner names this process in a kickoff claim. Defaults to hostname/random.
	Owner string
	// Logger is optional.
	Logger *log.Logger
	// Now is the clock DecideReconcile is evaluated against. Defaults to time.Now.
	Now func() time.Time
	// TurnTimeout defaults to [DefaultTurnTimeout].
	TurnTimeout time.Duration
	// OnChange, when set, is called with an agent's name after this package changes
	// what that agent's card shows: the stamp (the "kickoff owed" badge goes), a
	// recorded post-stamp failure (the "kickoff failed" badge comes), and a stuck
	// verdict (the card goes red). cmd/muster-server passes
	// api.Server.BroadcastAgentChanged, which the Agents list re-renders on.
	// ⚠ That clause was FALSE until the list's hx-trigger gained
	// `sse:agent.changed` (internal/ui's agentsListTrigger): before it, only the
	// Tasks board heard this broadcast, and an open Agents list showed none of it.
	//
	// 🔴 A PRE-SEND FAILURE DOES NOT CALL IT. It changes nothing a card shows (the row
	// still reads "kickoff owed"), and it recurs every tick while the pod boots, so
	// calling it there would re-render every open Agents list every 10 s.
	OnChange func(name string)
}

// Deliverer delivers owed first turns. Build it with [New].
type Deliverer struct {
	store  agents.Store
	insts  InstanceLister
	gw     Gateway
	prefix string
	owner  string
	log    *log.Logger
	now    func() time.Time
	turn   time.Duration
	// notReadyWait / notReadyPoll: see deliver's not_ready re-send.
	notReadyWait, notReadyPoll time.Duration
	notify                     func(name string)
	wg                         sync.WaitGroup
	// done closes when [Deliverer.Run] has returned, which is AFTER every
	// delivery it started has recorded its outcome. See [Deliverer.Done].
	done     chan struct{}
	doneOnce sync.Once
}

// New validates the configuration and builds the deliverer.
func New(cfg Config) (*Deliverer, error) {
	switch {
	case cfg.Store == nil:
		return nil, errors.New("agentkickoff: Config.Store is required")
	case cfg.Instances == nil:
		return nil, errors.New("agentkickoff: Config.Instances is required (without live " +
			"instances no recipient can be judged ready)")
	case cfg.Gateway == nil:
		return nil, errors.New("agentkickoff: Config.Gateway is required (a deliverer with no " +
			"gateway delivers nothing, which is the defect this package exists to close)")
	}
	d := &Deliverer{
		store:  cfg.Store,
		insts:  cfg.Instances,
		gw:     cfg.Gateway,
		prefix: agents.ResolveNamespacePrefix(cfg.NamespacePrefix),
		owner:  cfg.Owner,
		log:    cfg.Logger,
		now:    cfg.Now,
		turn:   cfg.TurnTimeout,
		notify: cfg.OnChange,
		done:   make(chan struct{}),
	}
	if d.owner == "" {
		d.owner = defaultOwner()
	}
	if d.log == nil {
		d.log = log.New(io.Discard, "", 0)
	}
	if d.now == nil {
		d.now = time.Now
	}
	if d.turn <= 0 {
		d.turn = DefaultTurnTimeout
	}
	d.notReadyWait, d.notReadyPoll = defaultNotReadyWait, defaultNotReadyPoll
	if d.notify == nil {
		d.notify = func(string) {}
	}
	return d, nil
}

// defaultOwner is hostname/random: the hostname says which pod, the suffix
// separates a restarted container from its predecessor.
func defaultOwner() string {
	host, _ := os.Hostname()
	buf := make([]byte, 6)
	_, _ = rand.Read(buf)
	return host + "/" + hex.EncodeToString(buf)
}

// Run ticks every interval until ctx is cancelled, then waits for in-flight turns
// and closes [Deliverer.Done].
func (d *Deliverer) Run(ctx context.Context, interval time.Duration) {
	// A background loop acts for every user (accounts PR 2/6); see internal/authz.
	ctx = authz.System(ctx)
	if interval <= 0 {
		interval = DefaultInterval
	}
	// Deferred FIRST so it runs LAST: Done closes only once d.Wait has returned,
	// i.e. once every turn this Run started has written its outcome.
	defer d.doneOnce.Do(func() { close(d.done) })
	defer d.Wait()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := d.Tick(ctx); err != nil && ctx.Err() == nil {
			d.log.Printf("agentkickoff: tick: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Wait blocks until every delivery started by [Deliverer.Tick] has finished.
func (d *Deliverer) Wait() { d.wg.Wait() }

// Done is closed once [Deliverer.Run] has returned — after its ctx was cancelled
// AND every turn it started has recorded its outcome.
//
// 🔴 THE PROCESS'S SHUTDOWN WAITS ON THIS, AND WITHOUT THAT WAIT A TURN CANCELLED
// BY A REDEPLOY WAS LOST SILENTLY. Run's ctx is the process's signal context, so
// SIGTERM cancels an in-flight turn AFTER the row was stamped kicked_off. The
// failure is then written on a DETACHED budget (bookkeeping), but main used to
// return straight into pool.Close and exit without waiting for Run's goroutine:
// the stamp had landed, the record had not, and the row read as a delivered
// kickoff with no error. cmd/muster-server's shutdown waits on this channel,
// bounded, before the pool closes.
//
// It is a channel rather than a second wg.Wait because a WaitGroup may not be
// waited on while Tick can still Add to it from Run's goroutine; Run alone both
// Adds and Waits, so only Run may say it is finished.
func (d *Deliverer) Done() <-chan struct{} { return d.done }

// owned reports whether a row was written by THIS deployment: its stored
// namespace is exactly the one the configured prefix produces for its name.
func (d *Deliverer) owned(a agents.Agent) bool {
	return a.Namespace == agents.NamespaceFor(d.prefix, a.Name)
}

// owesFirstTurn is the deliverer's scope: a row this deployment owns, holding a
// note, that has NEVER been kicked off.
//
// 🔴 `!a.KickedOff` IS A HARD EXCLUSION, NOT A DETAIL OF agents.KickoffOwed. A
// kicked-off row has already had a (possibly paid) turn handed to a gateway; the
// only decisions the table makes for it are re-delivery after a lost recipient and
// the error that ends that budget, and this package does not take them. Deciding to
// re-send a task is a different, costlier decision than paying a debt nobody has
// paid, and it is not taken here by omission.
func (d *Deliverer) owesFirstTurn(a agents.Agent) bool {
	return d.owned(a) && !a.KickedOff && a.PendingNote != ""
}

// Tick makes one pass: it decides, for every owed row, what agents.DecideReconcile
// says, acts on it, and starts each delivery in the background (see
// [Deliverer.Wait]).
//
// 🔴 A FAILED INSTANCE LIST ENDS THE TICK WITH NO ACTION. An unobserved backend is
// not an empty one: deciding against a nil instance would read every young row as
// absent and every old one as stuck, and error rows on no information.
func (d *Deliverer) Tick(ctx context.Context) error {
	rows, err := d.store.List(ctx)
	if err != nil {
		return fmt.Errorf("list agents: %w", err)
	}
	var owed []agents.Agent
	for _, a := range rows {
		if d.owesFirstTurn(a) {
			owed = append(owed, a)
		}
	}
	if len(owed) == 0 {
		return nil
	}
	insts, err := d.insts.Instances(ctx)
	if err != nil {
		return fmt.Errorf("list instances (no action taken this tick): %w", err)
	}
	idx := agents.InstanceIndex(insts)
	now := d.now()
	for _, a := range owed {
		inst := idx[a.Name]
		action, _ := agents.DecideReconcile(a, inst, now)
		switch action {
		case agents.ActionRetryKickoff:
			d.startDelivery(ctx, a, *inst)
		case agents.ActionError:
			d.failStuck(ctx, a)
		default:
			// ActionNone / ActionPersistStatus: nothing owed THIS tick. Persisting a
			// recomputed status is not this package's job: internal/agentstatus
			// promotes provisioning → running with a write conditional on the row
			// not having moved, which is what makes a stale snapshot harmless.
		}
	}
	return nil
}

// startDelivery runs one delivery in the background (see [Deliverer.Wait]).
//
// 🔴 THERE IS NO IN-PROCESS IN-FLIGHT SET, AND THE PER-AGENT CLAIM IS WHY NONE IS
// NEEDED. A tick that finds a row still owed while an earlier tick's delivery of
// it is running starts a second goroutine, and that goroutine does nothing:
//
//   - BEFORE the stamp, the first goroutine holds agents.Store.ClaimKickoff's
//     claim. ClaimKickoff wins only when the claim is unheld or EXPIRED — it does
//     not special-case its own owner — and the claim lives for the whole turn plus
//     claimMargin, so the second goroutine's claim LOSES and it returns.
//   - AFTER the stamp, the row is kicked_off and owesFirstTurn excludes it from the
//     tick's list, and a goroutine already past the list re-reads under its own
//     claim and finds it kicked off.
//
// An in-process map duplicated that guarantee for one process only, and was a
// second mechanism to keep correct. Pinned against the REAL claim SQL by
// TestASecondTickInTheSameProcessDoesNotRunASecondTurn.
func (d *Deliverer) startDelivery(ctx context.Context, a agents.Agent, inst provision.Instance) {
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		d.deliver(ctx, a, inst)
	}()
}

// failStuck marks an owed row `error` once the table says its first turn will not
// arrive: it dwelt past ProvisioningStuckTimeout without a ready instance accepting
// the turn. The last recorded send failure is appended, so error_message carries the
// primary evidence (the card itself shows a red status dot; the text is machine tier).
//
// 🔴 THE WRITE IS CONDITIONAL ON THE ROW NOT HAVING MOVED SINCE THE LIST READ
// (agents.Store.MarkKickoffStuck). The verdict was taken from a snapshot; a delivery
// may have stamped the row since, or the operator may have Started it again, and an
// unconditional write turned either into an `error` card.
//
// ⚠ THE LOG LINES ARE SCRUBBED OF THE PENDING NOTE, as recordError's is: msg carries
// the last kickoff_error, which can quote a runtime's response body.
func (d *Deliverer) failStuck(parent context.Context, a agents.Agent) {
	msg := fmt.Sprintf("kickoff never delivered: the agent dwelt more than %s without a ready "+
		"instance accepting its first turn", agents.ProvisioningStuckTimeout) +
		agents.KickoffErrorSuffix(a)
	shown := agents.ScrubNote(msg, a.PendingNote)
	ctx, cancel := bookkeeping(parent)
	defer cancel()
	applied, err := d.store.MarkKickoffStuck(ctx, a.ID, a.UpdatedAt, msg)
	if err != nil {
		d.log.Printf("agentkickoff: agent %d (%s): could not record %q: %v", a.ID, a.Name, shown, err)
		return
	}
	if !applied {
		d.log.Printf("agentkickoff: agent %d (%s): not marked stuck: the row changed after it "+
			"was read (stamped, restarted, or being delivered)", a.ID, a.Name)
		return
	}
	d.log.Printf("agentkickoff: agent %d (%s): %s", a.ID, a.Name, shown)
	d.notify(a.Name)
}

// turnFor is the budget for one agent's first turn: the configured turn, or —
// for a kind whose single request may legitimately run longer
// (agents.KindTurnTimeout; claude-code's outlasts ccd's own 30m budget) — that
// request budget plus kindSlack, whichever is longer. The gateway kind gets
// exactly d.turn, as before kinds existed.
func (d *Deliverer) turnFor(a agents.Agent) time.Duration {
	if k := agents.KindTurnTimeout(a.Kind); k > 0 && k+kindSlack > d.turn {
		return k + kindSlack
	}
	return d.turn
}

// kindSlack is how much longer than a kind's per-request budget its first turn
// may run end to end (resolve, session open, bookkeeping).
const kindSlack = 2 * time.Minute

// deliver runs one first turn. The order is the design: everything that can fail
// without contacting the agent runtime happens BEFORE KickedOff is stamped, and is
// therefore retried by the next tick; the turn itself happens only after the stamp,
// so it is never run twice. (The one free failure left after the stamp, a
// connection that cannot be opened, is named in the package doc.)
func (d *Deliverer) deliver(parent context.Context, a agents.Agent, inst provision.Instance) {
	turn := d.turnFor(a)
	ctx, cancel := context.WithTimeout(parent, turn)
	defer cancel()

	// A token-less row derives a well-formed WRONG bearer (agentgateway.reach), so
	// it is refused here, before the stamp, and retried until the dwell bound.
	if a.HooksToken == "" {
		d.recordError(parent, a, "kickoff not sent: the agent has no hooks token yet, so no gateway "+
			"credential can be derived (it is minted when the instance is created)")
		return
	}

	won, err := d.store.ClaimKickoff(ctx, a.ID, d.owner, turn+claimMargin)
	if err != nil || !won {
		// Another process holds it, or the claim could not be verified. Either way
		// this process must not run the turn — and must not release a claim it
		// does not hold.
		if err != nil {
			d.log.Printf("agentkickoff: agent %d (%s): claim: %v", a.ID, a.Name, err)
		}
		return
	}
	defer func() {
		rctx, rcancel := bookkeeping(parent)
		defer rcancel()
		if err := d.store.ReleaseKickoffClaim(rctx, a.ID, d.owner); err != nil {
			d.log.Printf("agentkickoff: agent %d (%s): release claim: %v", a.ID, a.Name, err)
		}
	}()

	// 🔴 RE-READ UNDER THE CLAIM. The row above came from a list taken before the
	// claim; another replica may have delivered in between. The stamp is what makes
	// that visible, so the fresh row is the only one this decision may use.
	fresh, err := d.store.Get(ctx, a.ID)
	if err != nil {
		d.log.Printf("agentkickoff: agent %d (%s): re-read under claim: %v", a.ID, a.Name, err)
		return
	}
	if !d.owesFirstTurn(fresh) || fresh.Status == agents.StatusError || fresh.HooksToken == "" {
		return
	}

	// The transcript session is opened BEFORE the stamp: failing to open one costs
	// nothing and is retried, whereas after the stamp it would strand the turn.
	sess, err := d.store.LatestOrCreateSession(ctx, fresh.ID, fresh.Name)
	if err != nil {
		d.recordError(parent, fresh, "kickoff not sent: could not open a chat session: "+err.Error())
		return
	}

	// 🔴 RESOLVED BEFORE THE STAMP, FROM THE FRESH ROW. Finding the agent's address
	// (a Kubernetes API read for the k8s driver) and deriving its bearer opens no
	// connection to the runtime. A transient failure here used to happen inside Chat,
	// after the stamp, and became a permanent "kickoff failed" for a turn that was
	// never sent. Pinned by TestAnUnresolvableAgentIsRetriedNotStamped.
	target, err := d.gw.Resolve(ctx, fresh)
	if err != nil {
		d.recordError(parent, fresh, "kickoff not sent: could not resolve the agent's gateway: "+err.Error())
		return
	}

	// ---- the point of no return: from here the turn is never re-run. ----
	if err := d.store.SetKickedOff(ctx, fresh.ID, true); err != nil {
		d.recordError(parent, fresh, "kickoff not sent: could not stamp kicked_off: "+err.Error())
		return
	}
	// 🔴 THE NOTE IS RE-READ AFTER THE STAMP. A forward to an agent that has not
	// had its first turn is appended to pending_note (agents.PGStore
	// AppendPendingNote, only while kicked_off is false), so one appended after
	// the read above but before the stamp is in the row now and would be lost by
	// sending the old copy. After the stamp no append succeeds, so this read
	// holds every one (task routing, audit round 2).
	if again, err := d.store.Get(ctx, fresh.ID); err == nil && again.PendingNote != "" {
		fresh.PendingNote = again.PendingNote
	}
	// The "kickoff owed" badge goes with the stamp; tell open Agents lists.
	d.notify(fresh.Name)
	provenance := d.store.RecordKickoffDelivery(ctx, fresh.ID, inst.InstanceID, inst.Restarts)
	if provenance != nil {
		// Not fatal: an unrecorded recipient disables restart detection for this
		// agent (agents.kickoffLost's fail-safe direction); the turn still runs.
		d.log.Printf("agentkickoff: agent %d (%s): record delivery provenance: %v",
			fresh.ID, fresh.Name, provenance)
	}

	_, _ = d.store.AddChatMessage(ctx, agents.ChatMessage{
		AgentID: fresh.ID, SessionID: sess.ID, Role: "user", Content: fresh.PendingNote,
	})
	// 🔴 A TYPED `not_ready` IS RE-SENT HERE, WITHIN THIS ONE DELIVERY, AND ONLY
	// UNTIL notReadyWait. A claude-code pod is Ready (its `/` answers) as soon as
	// ccd and tmux are up, which can be seconds before the CLI's SessionStart;
	// until then ccd refuses a turn with `503 not_ready` and pastes NOTHING
	// (cmd/ccd/server.go), so re-sending cannot pay a turn twice. Retrying inside
	// the delivery keeps ONE stamp, ONE transcript row and ONE attempt; and the
	// bound ends it — a TUI stuck on a login screen answers not_ready for ever,
	// and that becomes an ordinary recorded kickoff failure, not a loop. (An
	// earlier draft un-stamped and retried on the next tick; every retry moved
	// updated_at, so the dwell bound could never fire.)
	reply, err := d.gw.Send(ctx, target, sess.SessionKey, fresh.PendingNote, nil)
	for waited := time.Duration(0); err != nil && runtimeNotReady(err) && waited < d.notReadyWait; waited += d.notReadyPoll {
		select {
		case <-ctx.Done():
		case <-time.After(d.notReadyPoll):
		}
		if ctx.Err() != nil {
			break
		}
		reply, err = d.gw.Send(ctx, target, sess.SessionKey, fresh.PendingNote, nil)
	}
	if err != nil && runtimeNotReady(err) {
		// Still not_ready when the delivery ended — the bound, or a shutdown
		// mid-wait. Nothing was sent, which turnFailure's texts (shutdown, budget,
		// "handed to the gateway") would deny; this one says so, and makes the
		// card's remedy say a re-send is safe (agents.KickoffResendSafe).
		d.recordError(parent, fresh, agents.KickoffNotAcceptedReason+": "+err.Error())
		d.notify(fresh.Name)
		return
	}
	if err != nil {
		d.recordError(parent, fresh, turnFailure(parent, ctx, turn, err))
		d.notify(fresh.Name)
		return
	}
	if strings.TrimSpace(reply) == "" {
		d.recordError(parent, fresh, EmptyReplyReason)
		d.notify(fresh.Name)
		return
	}
	actx, acancel := bookkeeping(parent)
	defer acancel()
	if provenance != nil {
		// 🔴 RecordKickoffDelivery IS WHAT CLEARS kickoff_error, AND IT JUST FAILED.
		// A pre-send failure recorded on an earlier tick (no token yet, no session) is
		// therefore still on a row that is now kicked_off — exactly agents.KickoffFailed's
		// shape — and the card would badge a turn that SUCCEEDED as "kickoff failed".
		// The success has to clear it itself. Pinned by
		// TestASucceededTurnDoesNotKeepAStaleErrorWhenProvenanceFailed.
		if err := d.store.SetKickoffError(actx, fresh.ID, ""); err != nil {
			d.log.Printf("agentkickoff: agent %d (%s): clear stale kickoff error: %v",
				fresh.ID, fresh.Name, err)
		}
	}
	_, _ = d.store.AddChatMessage(actx, agents.ChatMessage{
		AgentID: fresh.ID, SessionID: sess.ID, Role: "assistant", Content: reply,
	})
	d.log.Printf("agentkickoff: agent %d (%s): first turn delivered to %s (%d-byte reply)",
		fresh.ID, fresh.Name, inst.InstanceID, len(reply))
}

// notReadyWait / notReadyPoll bound the in-delivery re-send of a typed
// `not_ready` (see deliver). Three minutes covers a CLI cold start several times
// over; a runtime still not ready then is recorded as a failed kickoff.
const (
	defaultNotReadyWait = 3 * time.Minute
	defaultNotReadyPoll = 5 * time.Second
)

// runtimeNotReady reports a typed `503 not_ready` — ccd's refusal before it
// pastes anything. Only ccd's typed body sets Type, so an untyped 503 from
// anything else is NOT read as "nothing was sent".
func runtimeNotReady(err error) bool {
	var rt *agents.RuntimeError
	return errors.As(err, &rt) && rt.Status == http.StatusServiceUnavailable && rt.Type == "not_ready"
}

// ShutdownCancelledReason opens agents.kickoff_error when a stamped first turn was
// cut off because THIS PROCESS was shutting down (SIGTERM — a redeploy, a node
// drain, a scale-down).
//
// 🔴 IT IS NAMED, RATHER THAN LEFT AS A BARE "context canceled", BECAUSE THE
// OPERATOR'S NEXT MOVE DEPENDS ON IT. The turn was handed to the gateway and the row
// was stamped, so it is not re-run (never paid twice) — but nothing about the agent
// or the task was wrong. "context canceled" reads as a fault in the agent; this
// reads as what it is: muster went away mid-turn. The runtime may have kept running
// the turn after muster stopped waiting, so the remedy is to check whether the agent
// is already working before re-sending (agents.KickoffResendSafe answers false).
const ShutdownCancelledReason = "kickoff turn CANCELLED because muster was shutting down " +
	"(a redeploy or pod stop) while the turn was in flight. It had already been handed to " +
	"the gateway, so it is not retried automatically (a first turn is never paid twice)"

// turnFailure is the kickoff_error text for a turn that failed AFTER the stamp. It
// separates the four causes an operator acts on differently: a connection that was
// never opened (nothing sent), this process shutting down (parent cancelled), the
// turn outliving its own budget, and the gateway or runtime failing.
//
// 🔴 THE DIAL CASE IS CHECKED FIRST, BECAUSE IT IS THE ONLY ONE THAT PROVES WHAT THE
// RUNTIME SAW. A shutdown or a deadline that interrupted a dial still sent nothing,
// and "never reached the runtime" is the record that lets the operator re-send
// (agents.KickoffResendSafe). See [neverConnected] for why a dial error proves it.
//
// ⚠ parent IS THE WITNESS FOR A SHUTDOWN, NOT THE TURN ctx. The turn ctx derives
// from parent, so a shutdown cancels it too and its Err() alone cannot separate
// "muster is stopping" from a gateway that failed on a cancelled request.
func turnFailure(parent, turn context.Context, budget time.Duration, err error) string {
	switch {
	case neverConnected(err):
		return agents.KickoffNeverConnectedReason + ": " + err.Error()
	case parent.Err() != nil:
		return ShutdownCancelledReason + ": " + err.Error()
	case errors.Is(turn.Err(), context.DeadlineExceeded):
		return fmt.Sprintf("kickoff turn exceeded its %s budget and was abandoned after it was "+
			"handed to the gateway (not retried, so it is never paid twice): %v", budget, err)
	default:
		return "kickoff turn failed after it was handed to the gateway (not retried, so it " +
			"is never paid twice): " + err.Error()
	}
}

// neverConnected reports whether err is a failure to OPEN a connection: a
// *net.OpError whose Op is "dial", which is what Go's HTTP client returns (wrapped
// in a *url.Error, which the transports wrap again with %w) for DNS failures,
// refused and unreachable connections.
//
// 🔴 WHY THAT PROVES NOTHING WAS SENT. A dial error comes only from opening a NEW
// connection, so no byte of the request was written on it; and net/http retries a
// request on a new connection after a failure on a reused one only when nothing was
// written or the request is replayable, which a POST without an Idempotency-Key is
// not (net/http's shouldRetryRequest). A timeout or cancellation that interrupts a
// dial can surface without the OpError, as a bare context error; that is recorded as
// a timeout or shutdown, which errs toward "check before re-sending".
func neverConnected(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

// recordError writes a send failure as evidence (agents.Store.SetKickoffError —
// it does not move the dwell clock) and logs it.
//
// 🔴 THE WRITE RUNS ON A DETACHED BUDGET (bookkeeping), NEVER ON THE TURN'S ctx.
// The failure most worth recording — a turn cut off by shutdown — is the one whose
// ctx is already cancelled, so a write on it would be refused by the driver before
// it left the process and the failure would vanish. Pinned by
// TestAShutdownCancelledTurnIsRecordedAsSuch, whose fake store refuses a write on a
// done ctx exactly as pgx does.
//
// ⚠ THE LOG LINE IS SCRUBBED OF THE PENDING NOTE (agents.ScrubNote). msg can carry
// runtime-authored bytes — agents.responses quotes up to 512 bytes of a non-200 body
// — and a runtime that echoes its request would put the operator's note in the pod
// log. The stored column is not scrubbed: the row already holds the note, and every
// surface that reads the column scrubs it there (agents.KickoffFailureText).
func (d *Deliverer) recordError(parent context.Context, a agents.Agent, msg string) {
	d.log.Printf("agentkickoff: agent %d (%s): %s", a.ID, a.Name, agents.ScrubNote(msg, a.PendingNote))
	ctx, cancel := bookkeeping(parent)
	defer cancel()
	if err := d.store.SetKickoffError(ctx, a.ID, msg); err != nil {
		d.log.Printf("agentkickoff: agent %d (%s): could not record kickoff error: %v",
			a.ID, a.Name, err)
	}
}

// bookkeeping is a FRESH budget for a store write that records an outcome:
// detached from parent's cancellation and deadline (a shutdown-cancelled turn
// must still be recorded), but KEEPING parent's values.
//
// 🔴 THE VALUES ARE THE POINT (accounts PR 3a). parent carries Run's
// authz.System principal, and row-level security scopes every store call to
// the principal on its context. This used to be context.Background(): under
// RLS that acts as NOBODY, so every recorded failure and every kickoff
// delivery stamp would update zero rows — silently.
func bookkeeping(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), bookkeepingTimeout)
}
