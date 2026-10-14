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

// Gateway is a delivered turn's two halves — internal/memorelay's and
// internal/agentkickoff's: Resolve contacts no runtime, Send runs the turn.
// agentgateway.Gateway satisfies it.
type Gateway interface {
	Resolve(ctx context.Context, a agents.Agent) (agentgateway.Target, error)
	Send(ctx context.Context, t agentgateway.Target, sessionKey, message string, emit func(string)) (string, error)
}

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
	Logger  *log.Logger
}

// Deliverer hands one stored message to the owner's chief as a fenced turn: the
// memo relay's shape (render → the agent's session claim → Send → outcome).
//
// 🔴 IT IS CALLED, NOT A LOOP. The message route calls [Deliverer.Deliver] right
// after it stores a NEW message. A message left `queued` by a pending attempt is
// retried by a later caller passing its id — the seam the reply-claim route (a
// later PR) uses at the start of each claim; there is no background ticker.
type Deliverer struct {
	store  Store
	agents agents.Store
	gw     Gateway
	log    *log.Logger
	turn   time.Duration
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
	}
	d := &Deliverer{store: cfg.Store, agents: cfg.Agents, gw: cfg.Gateway, log: cfg.Logger, turn: DefaultTurnTimeout}
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

	// 🔴 RE-READ UNDER THE CLAIM: another attempt may have settled the message, and
	// the chief may have stopped or been handed a first turn meanwhile.
	m, err := d.store.Message(ctx, messageID)
	if err != nil {
		return pending("message_unreadable")
	}
	if m.Status != StatusQueued || m.PluginID != pluginID || m.OwnerID != p.OwnerID {
		return Result{Outcome: OutcomeSettled}
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
	target, err := d.gw.Resolve(ctx, fresh)
	if err != nil {
		return d.attempted(parent, messageID, "unresolved")
	}
	sess, err := d.session(ctx, p, fresh)
	if err != nil {
		return d.attempted(parent, messageID, "no_session")
	}

	message := RenderMessage(p, m)
	reply, err := d.gw.Send(ctx, target, sess.SessionKey, message, nil)
	if reason := agentgateway.Unsent(err); reason != "" {
		return d.attempted(parent, messageID, reason)
	}
	outcome := StatusDelivered
	if err != nil {
		outcome = StatusUnconfirmed
	}

	// A DETACHED BUDGET THAT KEEPS parent's VALUES: a turn cut off by shutdown is
	// the one most worth recording, and parent carries the owner's principal.
	bctx, bcancel := context.WithTimeout(context.WithoutCancel(parent), bookkeepingTimeout)
	defer bcancel()
	if ok, rerr := d.store.RecordDelivery(bctx, messageID, fresh.ID, outcome); rerr != nil || !ok {
		d.log.Printf("plugins: message %d was sent (%s) but could NOT be recorded (settled=%v): %v",
			messageID, outcome, ok, rerr)
	}
	_, _ = d.agents.AddChatMessage(bctx, agents.ChatMessage{AgentID: fresh.ID, SessionID: sess.ID, Role: "user", Content: message})
	if reply != "" {
		_, _ = d.agents.AddChatMessage(bctx, agents.ChatMessage{AgentID: fresh.ID, SessionID: sess.ID, Role: "assistant", Content: reply})
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
