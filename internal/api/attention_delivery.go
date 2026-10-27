package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/fence"
	"github.com/ZacxDev/muster/internal/mirror"
)

// ---------------------------------------------------------------------------
// AN ANSWER IS DELIVERED TO THE AGENT THAT ASKED (migration 0031;
// docs/attention.md#delivery).
//
// Until this, an agent's ask was answered PULL-ONLY: only attention_wait
// returned the answer, so an agent that asked and ended its turn — the agent
// routing a task to its owner, then "I'm not waiting on an answer" — never saw
// it. Now, when the operator answers an AGENT's ask on /attention, muster pushes
// the answer into that agent's DEFAULT chat session (0030's is_default: the
// agent's pinned session, any other agent's default) as a turn of its own, so
// the agent acts on it without polling.
//
// 🔴 THE TURN IS THE ORDINARY CHAT TURN, UNDER THE SAME LOCKS. It runs through
// chatTurn (the agent's instructions and tools; runDeliveredTurn stores it like
// any turn),
// holding the agent's SESSION CLAIM (agents.Store.ClaimKickoff — the claim a
// plugin delivery, a first turn and a route hand-back hold, so it never overlaps
// one of those) AND the in-process turn gate idle (turnGate.enterIdle — an
// operator's chat turn takes no claim, so the claim alone would not see it).
// Either busy, the delivery stays queued and the sweep retries it.
//
// 🔴 EXACTLY ONCE, BY THE ROW, NOT BY THIS PROCESS. Every move is one
// conditional UPDATE on the state it leaves (internal/attention): the claim
// (queued → delivering, with a lease), the outcome (delivering → delivered |
// unconfirmed, only by the lease holder), and the reader's MarkWaited (queued,
// pull, delivering or unconfirmed → waited; an in-flight one loses its lease). A restart re-reads the owed rows (DueDeliveries) and delivers each
// once; a delivery whose lease lapsed mid-turn (its process died) settles
// UNCONFIRMED and is never resent on its own — the plugin deliverer's rule, and
// the operator's Retry on /attention puts it back.
//
// 🔴 AN AGENT WAITING IN attention_wait GETS IT THERE, NOT TWICE. A wait in
// flight on the ask (waitingOn) skips the push, the claim only takes an answer
// at least attentionDeliveryGrace old (a waiter elsewhere has polled by then),
// and a wait that returns the answer records it (MarkWaited) — whichever of the
// wait and the claim moves the row first wins.
//
// A MIRRORED SESSION'S answer is not pushed: muster types into a mirrored window
// only for the operator's own message (the send queue's step-up gate), so its
// answer stays `pull` for its attention_wait. A SYSTEM asker's (muster itself)
// has nobody to reach: no delivery at all.
//
// 🔴 NO BACKFILL: an answer recorded before 0031 has no delivery state and is
// never pushed.
// ---------------------------------------------------------------------------

// attentionDeliveryGrace is how old an answer must be before it is pushed: an
// asker blocked in attention_wait reads it first (its wake-up is immediate in
// this process; attentionPoll bounds it otherwise).
const attentionDeliveryGrace = 3 * attentionPoll

// deliveryGrace is this server's grace: attentionDeliveryGrace, or a test's
// override (attentionState.graceOverride — per server, never a package var a
// delivery left running by another test's server would race on).
func (s *Server) deliveryGrace() time.Duration {
	if g := s.attention.graceOverride; g != nil {
		return *g
	}
	return attentionDeliveryGrace
}

// attentionDeliveryGiveUp is how long an answer may stay queued (since it was
// given, or retried) before the delivery stops: an agent that is stopped, never
// started, or busy for a day settles it unconfirmed, saying why, and the
// owner's Retry queues it again.
const attentionDeliveryGiveUp = 24 * time.Hour

// attentionDeliveryBatch bounds one sweep's deliveries.
const attentionDeliveryBatch = 50

// attentionDeliveryMarker is the delivered answer's fence marker.
const attentionDeliveryMarker = "attention-answer"

// attentionDeliveryLease is a delivery's lease: past the longest turn budget
// any kind has, so it cannot lapse while the turn still runs.
func attentionDeliveryLease() time.Duration {
	longest := time.Duration(0)
	for _, k := range agents.Kinds {
		longest = max(longest, agentTurnBudget(k))
	}
	return longest + 2*time.Minute
}

// deliveryOwner names this process in a delivery's lease and the agent's
// session claim.
func (s *Server) deliveryOwner() string {
	s.attention.deliveryByOnce.Do(func() {
		host, _ := os.Hostname()
		b := make([]byte, 6)
		_, _ = rand.Read(b)
		s.attention.deliveryBy = host + "/attention/" + hex.EncodeToString(b)
	})
	return s.attention.deliveryBy
}

// renderAttentionAnswer is the turn an answer becomes: muster's line naming
// the ask, then the question (the asker's own words, or — for a routed task —
// muster's line around an upstream tracker's title) and the owner's answer,
// fenced.
//
// 🔴 THE PREFACE NAMES THE ASK BY ID AND NOTHING ELSE. It is muster's voice,
// outside the fence; fence.Sanitize folds line breaks but not quotes, so any
// excerpt of the question there (a task title can be written upstream) could
// close the quote and speak as the owner. Every byte of the question and the
// answer is inside the fence. The one other thing it names is where an answer
// given through attention_answer came from (answeredViaLine): a host and a
// Claude session id, each re-checked against the mirror wire's shapes
// (lowercase host label; [0-9A-Za-z_-] id) before it is written there.
func renderAttentionAnswer(a attention.Ask) string {
	id := strconv.FormatInt(a.ID, 10)
	pre := "muster: your owner answered attention #" + id + ", which you asked" + answeredViaLine(a.AnsweredVia) +
		". The block below is your question and your owner's " +
		"answer, relayed by muster: act on the answer now. It is delivered once; you do not need to call attention_wait for it."
	lines := []string{"question:"}
	for _, l := range strings.Split(a.Question, "\n") {
		lines = append(lines, "  "+fence.Sanitize(l))
	}
	lines = append(lines, "answer:")
	for _, l := range strings.Split(a.Answer, "\n") {
		lines = append(lines, "  "+fence.Sanitize(l))
	}
	return fence.Block{Preface: pre, Marker: attentionDeliveryMarker, Count: 1, Lines: lines}.Render(fence.NewNonce())
}

// answeredViaLine is where an answer came from, as the delivered preface says
// it: "" for the Attention view; for attention_answer, " — answered by the
// operator via MCP from <host>/<session>, their mirrored Claude Code session,
// not on the Attention view".
//
// ⚠ THE SHAPE RE-CHECK IS DELIBERATE, NOT DEAD CODE. While 0034's CHECK holds,
// no row reaches here with a host or session id outside the wire's shapes, so
// the fallback ("a mirrored Claude Code session", nothing named) does not run
// on stored data. It stays because this text is written OUTSIDE the fence in
// muster's own voice, and a CHECK is one ALTER (or a restore without it) away;
// TestAnsweredViaLineNamesOnlyWireShapedIDs reaches it directly.
func answeredViaLine(src attention.AnswerSource) string {
	if !src.ViaSession() {
		return ""
	}
	return " — answered by the operator via MCP from " + mcpAnswerFrom(src) + ", not on the Attention view"
}

// mcpAnswerFrom names where an MCP answer came from, for text written OUTSIDE
// a fence in muster's voice (the delivered answer's preface, a dispatched
// agent's instructions preface): "<host>/<session>, their mirrored Claude Code
// session" when both have the mirror wire's shapes, else "a mirrored Claude
// Code session" (see answeredViaLine on why the re-check stays).
func mcpAnswerFrom(src attention.AnswerSource) string {
	if mirror.ValidHost(src.Host) == nil && mirror.ValidClaudeSession(src.Session) {
		return src.Host + "/" + src.Session + ", their mirrored Claude Code session"
	}
	return "a mirrored Claude Code session"
}

// answerSession is the session an answer to agent a goes to: its default
// session (agents.SessionRuntimeStore), else — a store without 0030's half —
// its latest.
func (s *Server) answerSession(ctx context.Context, a agents.Agent) (agents.ChatSession, error) {
	if st, ok := s.ext.Agents.(agents.SessionRuntimeStore); ok {
		return st.DefaultSession(ctx, a.ID, a.Name)
	}
	return s.ext.Agents.LatestOrCreateSession(ctx, a.ID, a.Name)
}

// startAnswerDelivery delivers ask id's answer in the background after the
// grace (an answer just given), registered so a shutdown waits for it — and
// ended by EndStreams, like a plugin delivery, so a turn cut off by shutdown is
// recorded (unconfirmed) rather than left to its lease.
func (s *Server) startAnswerDelivery(owner, id int64, after time.Duration) {
	if s.ext.Attention == nil || s.ext.Agents == nil {
		return
	}
	ctx, cancel := s.streamContext(context.Background())
	s.attention.deliveries.Add(1)
	safeGo(s.logger, "attention answer delivery", func() {
		defer s.attention.deliveries.Done()
		defer cancel()
		if after > 0 {
			t := time.NewTimer(after)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return // the sweep of the next process delivers it
			case <-t.C:
			}
		}
		s.deliverAnswer(ctx, owner, id)
	})
}

// WaitForAttentionDeliveries waits for every answer delivery this process
// started (shutdown, tests).
func (s *Server) WaitForAttentionDeliveries() { s.attention.deliveries.Wait() }

// Delivery results (deliverAnswer's answer; logged, and read by tests).
const (
	answerDelivered   = "delivered"
	answerUnconfirmed = "unconfirmed"
	answerLeftQueued  = "queued"
	answerNotTaken    = "not_taken"
)

// deliverAnswer pushes ask id's answer into the asking agent's session, once.
// It runs as owner (attentionPrincipal: row-level security shows that owner's
// rows only), and every check that can fail without contacting the runtime
// happens before the turn and leaves the delivery queued, saying why.
func (s *Server) deliverAnswer(parent context.Context, owner, id int64) string {
	if s.ext.Attention == nil || s.ext.Agents == nil || owner <= 0 {
		return answerNotTaken
	}
	// parent's cancellation is kept (shutdown ends the turn); every write after
	// the turn runs on a detached bookkeeping context.
	ctx := s.attentionPrincipal(parent, owner)
	if s.waitingOn(id) {
		// The asker is blocked in attention_wait on it here: the answer is the
		// waiter's (it records MarkWaited as it returns).
		return answerNotTaken
	}
	by := s.deliveryOwner()
	a, ok, err := s.ext.Attention.ClaimDelivery(ctx, id, by, attentionDeliveryLease(), s.deliveryGrace())
	if err != nil {
		s.logger.Printf("attention: ask %d: claim its delivery: %v", id, err)
		return answerNotTaken
	}
	if !ok {
		return answerNotTaken // delivered, read, in flight elsewhere, or not yet past the grace
	}
	key := attentionAskKey(a.ID)
	bookkeeping := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	}
	var settle func(delivered bool, session int64, detail string) string
	leave := func(detail string) string {
		if !a.DeliveryQueuedAt.IsZero() && time.Since(a.DeliveryQueuedAt) >= attentionDeliveryGiveUp {
			return settle(false, 0, "not taken within a day — "+detail)
		}
		bctx, cancel := bookkeeping()
		defer cancel()
		if err := s.ext.Attention.ReleaseDelivery(bctx, a.ID, by, detail); err != nil {
			s.logger.Printf("attention: ask %d: return its delivery to the queue (%s): %v", a.ID, detail, err)
		}
		s.logger.Printf("attention: ask %d: answer left queued: %s", a.ID, detail)
		s.attentionChanged(a.Owner, key)
		return answerLeftQueued
	}
	settle = func(delivered bool, session int64, detail string) string {
		bctx, cancel := bookkeeping()
		defer cancel()
		if held, err := s.ext.Attention.FinishDelivery(bctx, a.ID, by, delivered, session, detail); err != nil || !held {
			s.logger.Printf("attention: ask %d: the answer was sent (delivered=%v) but could NOT be recorded (lease held=%v): %v", a.ID, delivered, held, err)
		}
		s.attentionChanged(a.Owner, key)
		if delivered {
			s.logger.Printf("attention: ask %d: answer delivered to agent %d (session %d)", a.ID, a.Asker.AgentID, session)
			return answerDelivered
		}
		s.logger.Printf("attention: ask %d: answer delivery unconfirmed: %s", a.ID, detail)
		return answerUnconfirmed
	}
	if a.Owner != owner || a.Asker.AgentID <= 0 {
		// Never: Answer queues only an agent's ask, and the row is the owner's.
		return settle(false, 0, "not an agent's ask")
	}
	ag, err := s.ext.Agents.Get(ctx, a.Asker.AgentID)
	switch {
	case err != nil:
		return leave("the agent could not be read")
	case ag.OwnerID != a.Owner:
		return settle(false, 0, "the agent is not the owner's")
	case !agentCanBeSent(ag.Status):
		return leave("the agent is not running (" + ag.Status + ")")
	case agents.KickoffOwed(ag):
		return leave("the agent has not had its first turn yet")
	case s.answerTurn == nil:
		return leave("this muster has no agent gateway")
	}
	sess, err := s.answerSession(ctx, ag)
	if err != nil {
		return leave("the agent's session could not be opened")
	}
	budget := agentTurnBudget(ag.Kind)
	won, err := s.ext.Agents.ClaimKickoff(ctx, ag.ID, by, budget+time.Minute)
	if err != nil || !won {
		return leave("the agent is busy with another delivery")
	}
	defer func() {
		bctx, cancel := bookkeeping()
		defer cancel()
		if err := s.ext.Agents.ReleaseKickoffClaim(bctx, ag.ID, by); err != nil {
			s.logger.Printf("attention: ask %d: release agent %d's session claim: %v", a.ID, ag.ID, err)
		}
	}()
	if ok, why := s.turns.enterIdle(ag.ID); !ok {
		if why == "closed" {
			return leave("the agent is being replaced")
		}
		return leave("the agent is in the middle of a turn")
	}
	defer s.turns.leave(ag.ID)

	tctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	parts, err := s.answerTurn(tctx, ag, sess, renderAttentionAnswer(a), nil)
	if reason := agentgateway.Unsent(err); reason != "" {
		// Provably nothing reached the runtime: queued again, for the sweep.
		return leave("the agent's runtime took nothing (" + reason + ")")
	}
	bctx, bcancel := bookkeeping()
	defer bcancel()
	if err != nil {
		s.persistTurn(bctx, ag, sess, []agents.TurnPart{{Kind: "text", Content: AgentTurnFailedPrefix + err.Error()}})
		return settle(false, sess.ID, "the agent's turn failed: "+err.Error())
	}
	s.persistTurn(bctx, ag, sess, parts)
	return settle(true, sess.ID, "")
}

// runDeliveredTurn is the delivery's turn: the ordinary chat turn (chatTurn —
// the agent's instructions and tools), with the delivered text stored in the
// session only when it may have reached the runtime — the plugin deliverer's
// order. runEnteredTurn stores it FIRST, so a turn the runtime provably refused
// (agentgateway.Unsent: queued again, delivered later) would leave the answer in
// the transcript twice. UseExtensions sets s.answerTurn to it when a gateway is
// wired.
func (s *Server) runDeliveredTurn(ctx context.Context, a agents.Agent, session agents.ChatSession, text string, emit agents.StreamEmit) ([]agents.TurnPart, error) {
	s.markAgentActive(requestOwner(ctx), a.Name, "thinking")
	pc := &agents.PartCollector{}
	reply, err := s.chatTurn(ctx, a, session.SessionKey, text, pc.Observe)
	s.markAgentIdle(a.Name)
	if agentgateway.Unsent(err) != "" {
		return nil, err
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if _, serr := s.ext.Agents.AddChatMessage(sctx, agents.ChatMessage{AgentID: a.ID, SessionID: session.ID, Role: "user", Content: text}); serr != nil {
		s.logger.Printf("attention: store the delivered answer in agent %d's session %d: %v", a.ID, session.ID, serr)
	}
	if err != nil {
		return nil, err
	}
	parts := pc.Parts()
	if len(parts) == 0 && reply != "" {
		parts = []agents.TurnPart{{Kind: "text", Content: reply}}
	}
	return parts, nil
}

// deliverDueAnswers is one delivery sweep: settle every delivery a dead
// process left mid-turn (unconfirmed, never resent), then deliver every owed
// answer past the grace, each as its owner. On the attention sweep.
func (s *Server) deliverDueAnswers(ctx context.Context) {
	if s.ext.Attention == nil || s.ext.Agents == nil {
		return
	}
	lapsed, err := s.ext.Attention.RecoverDeliveries(ctx)
	if err != nil {
		s.logger.Printf("attention: settle lapsed deliveries: %v", err)
	}
	for _, a := range lapsed {
		s.logger.Printf("attention: ask %d: its delivery's process stopped mid-turn: unconfirmed (Retry on /attention)", a.ID)
		s.attentionChanged(a.Owner, attentionAskKey(a.ID))
	}
	due, err := s.ext.Attention.DueDeliveries(ctx, s.deliveryGrace(), attentionDeliveryBatch)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			s.logger.Printf("attention: owed deliveries: %v", err)
		}
		return
	}
	// Each in its own goroutine: a turn may run for its whole budget, and the
	// sweep also expires asks. Two sweeps starting one ask both race its claim,
	// which exactly one wins.
	for _, a := range due {
		s.startAnswerDelivery(a.Owner, a.ID, 0)
	}
}
