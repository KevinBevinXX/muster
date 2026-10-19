package plugins

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"os"
	"time"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
)

// Gateway is the half of a delivered turn that contacts no runtime: Resolve
// refuses an agent that cannot be reached BEFORE anything is sent, so such a
// failure leaves the message queued. agentgateway.Gateway satisfies it.
type Gateway interface {
	Resolve(ctx context.Context, a agents.Agent) (agentgateway.Target, error)
}

// TurnFunc runs one turn for agent a under sessionKey and returns the reply,
// streaming every event (text, thinking, tool_call, tool_result) to emit.
//
// 🔴 IT IS THE ORDINARY CHAT TURN, WITH THE AGENT'S TOOLS, AND NOT A TOOLLESS
// SEND. internal/api wires its chatTurn here: the agent's instructions, the tool
// definitions THAT agent is offered and the in-process dispatch for them. A
// delivered message is the turn on which the chief acts on it (files a gated
// task, drafts a reply), so a toolless send would leave a gateway-kind chief
// unable to call a single tool on exactly the turn the tools exist for. This
// package cannot import internal/api (it imports this one), hence a function.
type TurnFunc func(ctx context.Context, a agents.Agent, sessionKey, message string, emit agents.StreamEmit) (string, error)

// Outcome is what one delivery attempt came to.
type Outcome string

const (
	// OutcomeDelivered: the chief's turn completed; the message is `delivered`.
	OutcomeDelivered Outcome = "delivered"
	// OutcomeUnconfirmed: the send failed after the text may have been pasted;
	// the message is `unconfirmed` and is never sent again.
	OutcomeUnconfirmed Outcome = "unconfirmed"
	// OutcomePending: nothing was pasted (no chief, chief not running, the
	// session claim held, a typed refusal, a dial error…); the message stays
	// `queued` for a later attempt.
	OutcomePending Outcome = "pending"
	// OutcomeSettled: the message was no longer queued when re-read under the
	// claim (another attempt settled it); nothing was sent.
	OutcomeSettled Outcome = "settled"
)

// Result is one attempt's outcome and, for a pending one, why.
type Result struct {
	Outcome Outcome
	Reason  string
}

// DefaultTurnTimeout bounds one delivered turn for a kind with no budget of its own.
const DefaultTurnTimeout = 15 * time.Minute

const (
	kindSlack          = 2 * time.Minute
	claimMargin        = 2 * time.Minute
	bookkeepingTimeout = 15 * time.Second
)

// DelivererConfig is everything a Deliverer needs; every field but Logger is
// required.
type DelivererConfig struct {
	Store   Store
	Agents  agents.Store
	Gateway Gateway
	Turn    TurnFunc
	Logger  *log.Logger
	// Routing: session routing is on, so the turn says when to route
	// (RoutingSentence).
	Routing bool
	// Hints, when set, finds the sessions that may already be on a message's
	// subject; the turn then lists them after the message (hints.go). Nil: no
	// hint block, ever.
	Hints HintFunc
}

// Deliverer hands one stored message to the owner's chief as a fenced turn: the
// memo relay's shape (render → the agent's session claim → the turn → outcome),
// except that the turn is the chief's ordinary chat turn, tools included (TurnFunc).
//
// 🔴 IT IS CALLED, NOT A LOOP. The message route calls [Deliverer.Deliver] right
// after it stores a NEW message. A message left `queued` by a pending attempt is
// retried by a later caller passing its id — the seam the reply-claim route (a
// later PR) uses at the start of each claim; there is no background ticker.
type Deliverer struct {
	store  Store
	agents agents.Store
	gw     Gateway
	run    TurnFunc
	log    *log.Logger
	turn   time.Duration
	// routing: DelivererConfig.Routing.
	routing bool
	// hints: DelivererConfig.Hints.
	hints HintFunc
	// owner names this process in an agent's session claim.
	owner string
}

// NewDeliverer validates the configuration.
func NewDeliverer(cfg DelivererConfig) (*Deliverer, error) {
	switch {
	case cfg.Store == nil:
		return nil, errors.New("plugins: DelivererConfig.Store is required")
	case cfg.Agents == nil:
		return nil, errors.New("plugins: DelivererConfig.Agents is required")
	case cfg.Gateway == nil:
		return nil, errors.New("plugins: DelivererConfig.Gateway is required (a message is delivered as a turn)")
	case cfg.Turn == nil:
		return nil, errors.New("plugins: DelivererConfig.Turn is required (the tool-capable chat turn a message is delivered as)")
	}
	d := &Deliverer{store: cfg.Store, agents: cfg.Agents, gw: cfg.Gateway, run: cfg.Turn, log: cfg.Logger, turn: DefaultTurnTimeout,
		routing: cfg.Routing, hints: cfg.Hints}
	if d.log == nil {
		d.log = log.New(io.Discard, "", 0)
	}
	host, _ := os.Hostname()
	buf := make([]byte, 6)
	_, _ = rand.Read(buf)
	d.owner = host + "/plugins/" + hex.EncodeToString(buf)
	return d, nil
}

func pending(reason string) Result { return Result{Outcome: OutcomePending, Reason: reason} }

// Deliver attempts message messageID of plugin pluginID to chief. ctx carries the
// plugin OWNER's principal: row-level security then shows only that owner's
// rows, and the owner checks below refuse a chief that is not theirs anyway (a
// store without row-level security — every fake — must still be refused).
//
// The order is memorelay's: everything that can fail without contacting the
// runtime happens first and leaves the message queued; a send that provably
// pasted nothing (agentgateway.Unsent) leaves it queued too; anything else —
// success, or a failure after the text may have been pasted — settles it, and it
// is never sent again.
func (d *Deliverer) Deliver(parent context.Context, pluginID, messageID int64, chief agents.Agent) Result {
	turn := d.turn
	if k := agents.KindTurnTimeout(chief.Kind); k > 0 && k+kindSlack > turn {
		turn = k + kindSlack
	}
	ctx, cancel := context.WithTimeout(parent, turn)
	defer cancel()

	p, err := d.store.Plugin(ctx, pluginID)
	if err != nil {
		return pending("plugin_unreadable")
	}
	if p.RevokedAt != nil {
		// A revoked plugin's messages never reach the chief: Submit refuses new
		// ones and QueuedForOwner skips queued ones, and THIS is the check for a
		// caller that names one directly (a retry requeued just before the
		// revoke). It stays queued, uncounted, as QueuedForOwner leaves it.
		return pending("plugin_revoked")
	}
	if chief.ID == 0 {
		return d.attempted(parent, messageID, "no_chief")
	}
	if chief.OwnerID != p.OwnerID {
		// Never another owner's agent, whatever the caller resolved.
		return d.attempted(parent, messageID, "not_owners_chief")
	}

	// 🔴 THE CHIEF'S SESSION CLAIM, THE ONE A FIRST TURN AND A MEMO TURN HOLD
	// (agents.Store.ClaimKickoff). It keeps a plugin turn from taking the session
	// from a first turn still being handed over, and it makes "one message per
	// turn" hold across processes: two deliveries cannot be inside this section for
	// one chief, and the winner re-reads the message below.
	won, err := d.agents.ClaimKickoff(ctx, chief.ID, d.owner, turn+claimMargin)
	if err != nil {
		return d.attempted(parent, messageID, "claim_failed")
	}
	if !won {
		return d.attempted(parent, messageID, "session_claimed")
	}
	defer func() {
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(parent), bookkeepingTimeout)
		defer rcancel()
		if err := d.agents.ReleaseKickoffClaim(rctx, chief.ID, d.owner); err != nil {
			d.log.Printf("plugins: agent %d: release the session claim: %v", chief.ID, err)
		}
	}()

	// 🔴 CRASH RECOVERY, AND THE CLAIM IS WHAT MAKES IT SAFE. A delivery holds this
	// chief's session claim for its whole turn and settles its `delivering` row
	// before releasing it, so a row still `delivering` to this chief while WE hold
	// the claim was left by a process that died mid-turn (its claim has expired).
	// It may have been pasted: it settles unconfirmed and is never sent again.
	// ⚠ A row delivering to a chief that is never delivered to again (deleted,
	// re-provisioned as a new row) stays `delivering` — still never resent.
	if n, err := d.store.RecoverDeliveries(ctx, chief.ID); err != nil {
		d.log.Printf("plugins: agent %d: settle deliveries a dead process left: %v", chief.ID, err)
	} else if n > 0 {
		d.log.Printf("plugins: agent %d: %d delivery(ies) left mid-turn by a dead process settled unconfirmed", chief.ID, n)
	}

	// 🔴 RE-READ UNDER THE CLAIM: another attempt may have settled the message, and
	// the chief may have stopped or been handed a first turn meanwhile.
	m, err := d.store.Message(ctx, messageID)
	if err != nil {
		return pending("message_unreadable")
	}
	if m.Status != StatusQueued || m.PluginID != pluginID || m.OwnerID != p.OwnerID {
		return Result{Outcome: OutcomeSettled}
	}
	// A message the owner retried says so in its turn (retrySentence): its
	// earlier turn may already have acted on it.
	retries, err := d.store.Retries(ctx, messageID)
	if err != nil {
		return pending("message_unreadable")
	}
	fresh, err := d.agents.Get(ctx, chief.ID)
	if err != nil {
		return d.attempted(parent, messageID, "chief_unreadable")
	}
	switch {
	case fresh.OwnerID != p.OwnerID:
		return d.attempted(parent, messageID, "not_owners_chief")
	case fresh.Status != agents.StatusRunning:
		return d.attempted(parent, messageID, "chief_not_running")
	case agents.KickoffOwed(fresh):
		return d.attempted(parent, messageID, "kickoff_owed")
	case fresh.HooksToken == "":
		return d.attempted(parent, messageID, "no_token")
	}
	if _, err := d.gw.Resolve(ctx, fresh); err != nil {
		return d.attempted(parent, messageID, "unresolved")
	}
	sess, err := d.session(ctx, p, fresh)
	if err != nil {
		return d.attempted(parent, messageID, "no_session")
	}

	// 🔴 `delivering` TO THIS CHIEF, WRITTEN BEFORE THE TURN: the chief's tools
	// (task_create_gated, reply_draft) accept only a message delivered to it, and
	// THIS turn is the one on which it learns of the message and acts on it. Until
	// this write it could cite nothing. It also counts the attempt.
	if ok, err := d.store.BeginDelivery(ctx, messageID, fresh.ID); err != nil {
		return d.attempted(parent, messageID, "begin_failed")
	} else if !ok {
		return Result{Outcome: OutcomeSettled}
	}

	message := RenderMessage(p, m, d.routing, d.hintsFor(ctx, fresh, m), Retry{Count: retries, Now: time.Now()})
	pc := &agents.PartCollector{}
	reply, err := d.run(ctx, fresh, sess.SessionKey, message, pc.Observe)

	// A DETACHED BUDGET THAT KEEPS parent's VALUES: a turn cut off by shutdown is
	// the one most worth recording, and parent carries the owner's principal.
	bctx, bcancel := context.WithTimeout(context.WithoutCancel(parent), bookkeepingTimeout)
	defer bcancel()
	if reason := agentgateway.Unsent(err); reason != "" {
		// Provably nothing pasted: back to queued, the chief no longer named. If
		// this write fails the row stays `delivering` and is recovered as
		// unconfirmed — the direction that never sends it twice.
		if aerr := d.store.AbandonDelivery(bctx, messageID, fresh.ID); aerr != nil {
			d.log.Printf("plugins: message %d: return it to the queue (%s): %v", messageID, reason, aerr)
		}
		d.log.Printf("plugins: message %d left queued: %s", messageID, reason)
		return pending(reason)
	}
	outcome := StatusDelivered
	if err != nil {
		outcome = StatusUnconfirmed
	}
	if ok, rerr := d.store.RecordDelivery(bctx, messageID, fresh.ID, outcome); rerr != nil || !ok {
		d.log.Printf("plugins: message %d was sent (%s) but could NOT be recorded (settled=%v): %v",
			messageID, outcome, ok, rerr)
	}
	_, _ = d.agents.AddChatMessage(bctx, agents.ChatMessage{AgentID: fresh.ID, SessionID: sess.ID, Role: "user", Content: message})
	// The reply's ORDERED parts — text and tool calls interleaved — as an
	// ordinary chat turn stores them, so the session shows what the chief did.
	parts := pc.Parts()
	if len(parts) == 0 && reply != "" {
		parts = []agents.TurnPart{{Kind: "text", Content: reply}}
	}
	for _, part := range parts {
		_, _ = d.agents.AddChatMessage(bctx, agents.ChatMessage{AgentID: fresh.ID, SessionID: sess.ID, Role: "assistant",
			Kind: part.Kind, Content: part.Content, ToolID: part.ToolID, ToolName: part.ToolName, ToolOK: part.ToolOK})
	}
	d.log.Printf("plugins: plugin %d message %d: %s", pluginID, messageID, outcome)
	return Result{Outcome: Outcome(outcome)}
}

// attempted counts a pending attempt on the message and returns it.
func (d *Deliverer) attempted(parent context.Context, messageID int64, reason string) Result {
	bctx, bcancel := context.WithTimeout(context.WithoutCancel(parent), bookkeepingTimeout)
	defer bcancel()
	if err := d.store.RecordAttempt(bctx, messageID); err != nil {
		d.log.Printf("plugins: message %d: count the attempt: %v", messageID, err)
	}
	d.log.Printf("plugins: message %d left queued: %s", messageID, reason)
	return pending(reason)
}

// session is the chief chat session dedicated to plugin p: the recorded one when
// it still belongs to this chief, else a new one (recorded for next time). Its
// title comes from the first turn's first line, which begins with p's label.
func (d *Deliverer) session(ctx context.Context, p Plugin, chief agents.Agent) (agents.ChatSession, error) {
	if p.ChiefAgentID != nil && *p.ChiefAgentID == chief.ID && p.ChiefSessionID != nil {
		if s, err := d.agents.GetSession(ctx, *p.ChiefSessionID); err == nil && s.AgentID == chief.ID {
			return s, nil
		}
	}
	s, err := d.agents.CreateSession(ctx, chief.ID, chief.Name)
	if err != nil {
		return agents.ChatSession{}, err
	}
	if err := d.store.SetChiefSession(ctx, p.ID, chief.ID, s.ID); err != nil {
		d.log.Printf("plugins: plugin %d: record its chief session: %v", p.ID, err)
	}
	return s, nil
}
