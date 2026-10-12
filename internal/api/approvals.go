package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
)

// ---------------------------------------------------------------------------
// THE APPROVAL RELAY: a claude-code pod's permission prompts, routed to the
// operator's phone THROUGH muster.
//
// 🔴 WHY THE POD DOES NOT TALK TO THE PERMISSION ROUTER ITSELF. The router has
// no credential an agent pod can safely hold. Its shared hook token
// is one value whose tier also READS every
// session's transcript and every tmux pane, resolves anyone's attention entries,
// deletes anyone's pending request by id and sends arbitrary push
// notifications; its terminal and chief tokens type into terminals; its service
// token is muster's own. A shell-capable agent holding any of them could read or
// interfere with the operator's other sessions. So the pod authenticates to
// MUSTER with its own per-agent token (the connector surface's credential, with
// the shared tokens refused by name), and muster files the request through the
// router's gate door on the service tier, using the relay's OWN gate
// (Server.UseApprovalGate), built only from MUSTER_APPROVAL_GATE_URL/_TOKEN. A
// STANDALONE muster can have that gate without any router behaviour, and a
// configured router's client is never used for it: without the dedicated gate
// the relay answers 503.
//
// WHAT AN AGENT CAN DO HERE, AND NOTHING ELSE:
//   - file a request (POST /agent/approvals). The card's Host and Project are
//     muster's, derived from the agent's identity, never the caller's, so a card
//     always names the agent that asked;
//   - read or clear a request IT filed (GET/DELETE /agent/approvals/{id}). Any
//     other id answers 404, whoever filed it;
//   - report that its terminal is blocked on a permission prompt
//     (POST /agent/blocked), which only drives this agent's own badge.
// There is no route that DECIDES. A decision is made only on the router: by a
// human on its human tier, or by an auto-approve window the operator opened there.
//
// ⚠ THE CARD FOLLOWS THE ROUTER'S AUTO-APPROVAL, SCOPED TO THE POD'S OWN PROJECT.
// It is filed as the gate type `agent-permission` (gateTypeAgentPermission), which
// the router treats like any other permission prompt: its auto-approve-ALL mode
// approves it, and a per-project auto-approve window approves it ONLY when that
// window's project equals the card's Project. muster forces that Project to
// ApprovalCardHost(agent) = "muster-agent/<name>" — never a value the pod
// sends, and never the bare agent name, which a laptop project could share —
// and the router refuses to mint this type under any other project. So an
// auto-approve window opened for a laptop project cannot approve a pod's tool
// call. An auto-approved card is already decided when it is filed;
// the pod's first poll reads `approve`. (It used to be a `checkpoint`, which the
// router exempts from every auto-approval path; the operator's decision is
// that a pod's prompts go through the router's auto-approve.)
//
// 🔴 DEPLOY ORDER: the router must know `agent-permission` BEFORE this ships.
// An older router answers 400 for the type, the relay answers 502, and every
// pod prompt falls back to the terminal (fail closed, but no phone approvals).
//
// ⚠ THE LEDGER IS IN MEMORY. A muster restart forgets which agent filed which
// request; the pod's next poll then answers 404 and its hook falls back to the
// in-terminal prompt, so no decision is ever invented. ⚠ IT IS NOT CLEAN FOR THE
// PHONE: the forgotten card is not cleared (ccd's DELETE also answers 404), so it
// stays on the operator's screen until the router's own TTL, and approving it
// there reaches nobody.
// ---------------------------------------------------------------------------

const (
	// approvalOwnerTTL bounds how long a filed request stays readable by its
	// agent. The pod's hook gives up after ~170s; this only has to outlive it.
	approvalOwnerTTL = 10 * time.Minute
	// approvalMaxOutstanding caps the requests one agent may have open (filed and
	// not yet decided, cleared or expired) at once, counting mints in flight. It
	// does NOT bound how often cards are filed: an agent that files and clears in
	// a loop never has more than one open. approvalMintBurst/approvalMintEvery
	// bound that.
	approvalMaxOutstanding = 4
	// approvalMintBurst and approvalMintEvery are the per-agent MINT RATE: a
	// token bucket holding approvalMintBurst cards, refilled one per
	// approvalMintEvery. Every mint attempt spends a token (a failed one too), and
	// every mint is one card plus one Web Push on the operator's phone, so this is
	// what bounds a filing loop nobody answers: 3 at once, then about one per 10s.
	// A card that was APPROVED OR DENIED — by a person or by the router's
	// auto-approval — gives its token back (settleDecided), so an operator
	// answering prompts back to back, or a pod under an auto-approve window, is
	// not throttled; clears, "gone" and dismissals do not refund.
	approvalMintBurst = 3
	approvalMintEvery = 10 * time.Second
	// approvalCeilingBurst and approvalCeilingEvery are the per-agent CEILING: a
	// second token bucket that is NEVER refunded, whatever decided the card. The
	// refund above lifts the mint rate for every approved or denied card, so while
	// an auto-approve window covers a pod nothing else bounds a filing loop, and
	// each card costs the router a stored row, events to every open client and one
	// control push per subscribed device. 30 at once, then one per 2s (30 a
	// minute sustained): a session's gated tool calls each follow a model turn, so a
	// turn that fans out parallel calls stays inside the burst and a steady session
	// does not sustain one card per 2s. A refused mint answers 429
	// and the pod's hook shows the terminal prompt, like the mint rate.
	approvalCeilingBurst = 30
	approvalCeilingEvery = 2 * time.Second
	// approvalGoneReads is how many CONSECUTIVE "gone" reads muster needs before it
	// believes a card is gone. The router's store reports a card absent on ANY
	// query error, not only when it is evicted, and its own in-process waiter
	// tolerates 3 consecutive misses for that reason; one transient error must not
	// orphan a card the operator can still see and approve.
	approvalGoneReads = 3
	// blockedReportTTL is how long a "blocked on a prompt" report counts without
	// a refresh. ccd re-reports every minute while blocked, so a pod that died
	// blocked stops showing the badge within this window.
	blockedReportTTL = 3 * time.Minute
	// approvalTextCap bounds each free-text field the agent supplies.
	approvalTextCap = 2000
	// approvalContextCap bounds the context lines the agent supplies.
	approvalContextCap = 12
)

// approvalLedger is the relay's in-memory state.
type approvalLedger struct {
	mu sync.Mutex
	// owners maps a filed request id to the agent that filed it.
	owners map[string]approvalOwner
	// blocked maps an agent id to its latest "blocked on a prompt" report.
	blocked map[int64]blockedReport
	// minting counts, per agent, mints in flight: reserved under mu before the
	// network call so concurrent POSTs cannot all pass the outstanding cap.
	minting map[int64]int
	// buckets is the per-agent mint-rate token bucket (refunded on approve/deny).
	buckets map[int64]mintBucket
	// ceilings is the per-agent ceiling bucket (never refunded).
	ceilings map[int64]mintBucket
	// burst and every override approvalMintBurst/approvalMintEvery when non-zero
	// (tests isolate the outstanding cap from the rate limit with them).
	burst int
	every time.Duration
}

type approvalOwner struct {
	agentID int64
	filed   time.Time
	// goneStreak counts consecutive "gone" reads; any other state resets it.
	goneStreak int
}

type mintBucket struct {
	tokens float64
	at     time.Time
}

// approveOrDeny reports whether a decided card's response is an approve or a
// deny. A dismissal ("ignore") or anything else is not.
//
// ⚠ IT DOES NOT SAY WHO DECIDED. The router's gate read carries no actor, so a
// human's approve and an auto-approval are the same answer here. The router does
// write the comment "auto-approved" on the latter, and the relay passes comments
// through to the pod unchanged (a human's deny reason is worth the agent
// reading); nothing in muster branches on that text, because a comment is a
// word any path can spell.
func approveOrDeny(response string) bool {
	switch strings.ToLower(strings.TrimSpace(response)) {
	case "approve", "allow", "reject", "deny":
		return true
	}
	return false
}

// settleDecided forgets a decided card and, when it was approved or denied,
// gives its mint-rate token back. Both happen under one lock and only if the
// card was still tracked, so a card refunds at most once however many polls see
// it.
//
// 🔴 ONLY AN APPROVE OR A DENY REFUNDS, whether a person or the router's
// auto-approval made it. Without the refund an operator answering prompts faster
// than one per approvalMintEvery would push the 4th to the terminal, and so
// would a pod working under an auto-approve window, which is the wait the window
// exists to remove. A clear (DELETE), a confirmed "gone" and a dismissal do NOT
// refund, so a file-and-clear loop stays limited to the mint rate.
//
// ⚠ SO THE MINT RATE DOES NOT BOUND A LOOP OF APPROVED CARDS; THE CEILING DOES
// (approvalCeilingBurst/approvalCeilingEvery), and it is never refunded. An
// auto-approved card is not free: see the ceiling's comment for what one costs.
func (s *Server) settleDecided(id string, agentID int64, refund bool) {
	l := &s.approvals
	l.mu.Lock()
	defer l.mu.Unlock()
	l.init()
	if _, ok := l.owners[id]; !ok {
		return
	}
	delete(l.owners, id)
	if refund {
		l.refundMintTokenLocked(agentID)
	}
}

// refundMintTokenLocked returns one mint token to agentID's bucket, never above
// the burst. Caller holds mu.
func (l *approvalLedger) refundMintTokenLocked(agentID int64) {
	burst := approvalMintBurst
	if l.burst > 0 {
		burst = l.burst
	}
	b, ok := l.buckets[agentID]
	if !ok {
		return
	}
	b.tokens++
	if b.tokens > float64(burst) {
		b.tokens = float64(burst)
	}
	l.buckets[agentID] = b
}

// takeMintTokenLocked spends one of agentID's mint-rate tokens, reporting false
// when the bucket is empty. Caller holds mu.
func (l *approvalLedger) takeMintTokenLocked(agentID int64, now time.Time) bool {
	burst, every := approvalMintBurst, approvalMintEvery
	if l.burst > 0 {
		burst = l.burst
	}
	if l.every > 0 {
		every = l.every
	}
	return takeToken(l.buckets, agentID, burst, every, now)
}

// takeCeilingTokenLocked spends one of agentID's ceiling tokens, reporting false
// when the bucket is empty. Nothing gives one back. Caller holds mu.
func (l *approvalLedger) takeCeilingTokenLocked(agentID int64, now time.Time) bool {
	return takeToken(l.ceilings, agentID, approvalCeilingBurst, approvalCeilingEvery, now)
}

// takeToken refills agentID's bucket in m for the time since it was last
// touched, then spends one token if there is one.
func takeToken(m map[int64]mintBucket, agentID int64, burst int, every time.Duration, now time.Time) bool {
	b, ok := m[agentID]
	if !ok {
		b = mintBucket{tokens: float64(burst), at: now}
	}
	b.tokens += float64(now.Sub(b.at)) / float64(every)
	if b.tokens > float64(burst) {
		b.tokens = float64(burst)
	}
	b.at = now
	if b.tokens < 1 {
		m[agentID] = b
		return false
	}
	b.tokens--
	m[agentID] = b
	return true
}

type blockedReport struct {
	since, seen time.Time
}

func (l *approvalLedger) init() {
	if l.owners == nil {
		l.owners = map[string]approvalOwner{}
	}
	if l.blocked == nil {
		l.blocked = map[int64]blockedReport{}
	}
	if l.minting == nil {
		l.minting = map[int64]int{}
	}
	if l.buckets == nil {
		l.buckets = map[int64]mintBucket{}
	}
	if l.ceilings == nil {
		l.ceilings = map[int64]mintBucket{}
	}
}

// pruneLocked drops expired owner entries and stale blocked reports.
func (l *approvalLedger) pruneLocked(now time.Time) {
	for id, o := range l.owners {
		if now.Sub(o.filed) > approvalOwnerTTL {
			delete(l.owners, id)
		}
	}
	for id, b := range l.blocked {
		if now.Sub(b.seen) > blockedReportTTL {
			delete(l.blocked, id)
		}
	}
}

// AwaitingApproval reports whether an agent is waiting on a human: it has a
// request filed through the relay that is still open, or its terminal reported
// a permission prompt recently. It drives the agent's "awaiting approval" badge.
func (s *Server) AwaitingApproval(agentID int64) bool {
	l := &s.approvals
	l.mu.Lock()
	defer l.mu.Unlock()
	l.init()
	l.pruneLocked(s.now())
	if _, ok := l.blocked[agentID]; ok {
		return true
	}
	for _, o := range l.owners {
		if o.agentID == agentID {
			return true
		}
	}
	return false
}

func (s *Server) registerApprovalRoutes(mux Mux) {
	mux.HandleFunc("POST /agent/approvals", s.requireRelayAgent(s.handleApprovalCreate))
	mux.HandleFunc("GET /agent/approvals/{id}", s.requireRelayAgent(s.handleApprovalGet))
	mux.HandleFunc("DELETE /agent/approvals/{id}", s.requireRelayAgent(s.handleApprovalDelete))
	mux.HandleFunc("POST /agent/blocked", s.requireRelayAgent(s.handleAgentBlocked))
}

// requireRelayAgent authenticates the calling agent by its own token, exactly as
// the connector surface does (authenticateCallingAgent), without requiring the
// connector store to be configured.
func (s *Server) requireRelayAgent(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.ext.Agents == nil {
			s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error": "this server has no agent store", HookUnarmedField: true,
			})
			return
		}
		a, ok := s.authenticateCallingAgent(w, r)
		if !ok {
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), agentCtxKey{}, a)))
	}
}

// ApprovalRequest is the POST /agent/approvals body the pod's hook sends.
type ApprovalRequest struct {
	Tool    string   `json:"tool"`
	Command string   `json:"command"`
	Input   string   `json:"input"`
	Cwd     string   `json:"cwd"`
	Session string   `json:"session"`
	Context []string `json:"context"`
}

// ApprovalCardHost is BOTH the Host and the Project a relayed card carries:
// muster's, naming the agent. As the Project it is the only per-project
// auto-approve window on the router that can approve the card.
//
// 🔴 IT IS NAMESPACED ("muster-agent/<name>"), NOT THE BARE AGENT NAME, AND THE
// CALLER CANNOT CHOOSE IT. The router matches a window to a card by exact
// Project. A bare name could equal a laptop project's name, and a pod-supplied
// one could name any project with an open window; either would let a window
// opened for something else approve this pod's tool call.
func ApprovalCardHost(a agents.Agent) string { return "muster-agent/" + a.Name }

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	// Cut on a rune boundary.
	for n > 0 && !utf8RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

func (s *Server) handleApprovalCreate(w http.ResponseWriter, r *http.Request) {
	a, _ := agentFromContext(r.Context())
	var body ApprovalRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}
	tool := clip(body.Tool, 200)
	if tool == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "`tool` is required"})
		return
	}
	cmd := clip(body.Command, approvalTextCap)
	if cmd == "" {
		cmd = clip(body.Input, approvalTextCap)
	}
	var ctxLines []string
	for _, l := range body.Context {
		if len(ctxLines) == approvalContextCap {
			break
		}
		if l = clip(l, 500); l != "" {
			ctxLines = append(ctxLines, l)
		}
	}
	g := s.approvalGatePort
	if g == nil {
		// No approval queue: the hook falls back to the in-terminal prompt.
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": errNoGate.Error(), HookUnarmedField: true})
		return
	}

	// 🔴 THE CAP CHECK, THE RATE CHECK AND THE SLOT RESERVATION ARE ONE CRITICAL
	// SECTION. Counting, unlocking and then minting let N parallel POSTs all see
	// the same count and all mint.
	l := &s.approvals
	now := s.now()
	l.mu.Lock()
	l.init()
	l.pruneLocked(now)
	open := l.minting[a.ID]
	for _, o := range l.owners {
		if o.agentID == a.ID {
			open++
		}
	}
	if open >= approvalMaxOutstanding {
		l.mu.Unlock()
		s.writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": "this agent already has the maximum number of open approval requests",
		})
		return
	}
	// The ceiling is asked FIRST, and a request it refuses spends no mint-rate
	// token: otherwise a pod at its ceiling would also drain the mint rate, which
	// refills five times slower, and stay refused long after the ceiling refilled.
	if !l.takeCeilingTokenLocked(a.ID, now) {
		l.mu.Unlock()
		s.writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": "this agent has filed too many approval requests in a short time",
		})
		return
	}
	if !l.takeMintTokenLocked(a.ID, now) {
		l.mu.Unlock()
		s.writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": "this agent is filing approval requests too fast",
		})
		return
	}
	l.minting[a.ID]++
	l.mu.Unlock()
	release := func() {
		l.minting[a.ID]--
		if l.minting[a.ID] <= 0 {
			delete(l.minting, a.ID)
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	id, err := g.MintGate(ctx, GateSpec{
		Type:    gateTypeAgentPermission,
		Tool:    tool,
		Command: cmd,
		Host:    ApprovalCardHost(a),
		Project: ApprovalCardHost(a),
		Cwd:     clip(body.Cwd, 500),
		Session: clip(body.Session, 200),
		Context: ctxLines,
	})
	if err != nil {
		l.mu.Lock()
		release()
		l.mu.Unlock()
		s.logger.Printf("approvals: %s: filing a card: %v", a.Name, err)
		s.writeJSON(w, http.StatusBadGateway, map[string]any{"error": "the approval queue could not be reached"})
		return
	}
	l.mu.Lock()
	release()
	l.owners[id] = approvalOwner{agentID: a.ID, filed: now}
	l.mu.Unlock()
	s.logger.Printf("approvals: %s filed %s tool=%q", a.Name, id, tool)
	s.writeJSON(w, http.StatusCreated, map[string]any{"requestId": id})
}

// ownedBy reports whether request id was filed by agent a and is still tracked.
func (s *Server) ownedBy(id string, a agents.Agent) bool {
	l := &s.approvals
	l.mu.Lock()
	defer l.mu.Unlock()
	l.init()
	l.pruneLocked(s.now())
	o, ok := l.owners[id]
	return ok && o.agentID == a.ID
}

// noteGoneRead records one "gone" read of id and reports whether it is the
// approvalGoneReads-th in a row.
func (s *Server) noteGoneRead(id string) bool {
	l := &s.approvals
	l.mu.Lock()
	defer l.mu.Unlock()
	l.init()
	o, ok := l.owners[id]
	if !ok {
		return true
	}
	o.goneStreak++
	l.owners[id] = o
	return o.goneStreak >= approvalGoneReads
}

// resetGoneReads clears id's consecutive-gone count after any other state.
func (s *Server) resetGoneReads(id string) {
	l := &s.approvals
	l.mu.Lock()
	defer l.mu.Unlock()
	l.init()
	if o, ok := l.owners[id]; ok {
		o.goneStreak = 0
		l.owners[id] = o
	}
}

func (s *Server) forgetApproval(id string) {
	l := &s.approvals
	l.mu.Lock()
	defer l.mu.Unlock()
	l.init()
	delete(l.owners, id)
}

// errNotYours is the single refusal for an id this agent did not file, whether
// it exists or not: answering differently would tell an agent which ids exist.
var errNotYours = errors.New("no such approval request for this agent")

func (s *Server) handleApprovalGet(w http.ResponseWriter, r *http.Request) {
	a, _ := agentFromContext(r.Context())
	id := r.PathValue("id")
	if !s.ownedBy(id, a) {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": errNotYours.Error()})
		return
	}
	g := s.approvalGatePort
	if g == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": errNoGate.Error()})
		return
	}
	d, err := g.ReadGate(r.Context(), id)
	if err != nil {
		// A failed read is NOT a state. The hook keeps polling until its own
		// deadline and then falls back to the terminal.
		s.writeJSON(w, http.StatusBadGateway, map[string]any{"error": "the approval queue could not be read"})
		return
	}
	state := d.State
	switch state {
	case GateStateDecided:
		s.settleDecided(id, a.ID, approveOrDeny(d.Response))
	case GateStateGone:
		// 🔴 ONE "gone" IS NOT EVIDENCE. Until approvalGoneReads arrive in a row the
		// card may still be on the operator's screen, so the agent is told to keep
		// waiting. On the last one muster gives up, forgets the id and clears the
		// card (best effort) so nothing stays approvable that nobody waits on.
		if !s.noteGoneRead(id) {
			state = GateStatePending
			break
		}
		s.forgetApproval(id)
		if err := g.ClearGate(r.Context(), id); err != nil {
			s.logger.Printf("approvals: clearing gone card %s: %v", id, err)
		}
	default:
		s.resetGoneReads(id)
	}
	out := map[string]any{"state": state}
	if d.State == GateStateDecided {
		out["response"] = d.Response
		if d.Comment != "" {
			out["comment"] = d.Comment
		}
	}
	s.writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleApprovalDelete(w http.ResponseWriter, r *http.Request) {
	a, _ := agentFromContext(r.Context())
	id := r.PathValue("id")
	if !s.ownedBy(id, a) {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": errNotYours.Error()})
		return
	}
	s.forgetApproval(id)
	if g := s.approvalGatePort; g != nil {
		if err := g.ClearGate(r.Context(), id); err != nil {
			s.logger.Printf("approvals: clearing card %s: %v", id, err)
		}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// blockedBody is the POST /agent/blocked body ccd sends.
type blockedBody struct {
	Blocked bool `json:"blocked"`
}

func (s *Server) handleAgentBlocked(w http.ResponseWriter, r *http.Request) {
	a, _ := agentFromContext(r.Context())
	var body blockedBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}
	l := &s.approvals
	now := s.now()
	l.mu.Lock()
	l.init()
	prev, was := l.blocked[a.ID]
	if body.Blocked {
		since := now
		if was {
			since = prev.since
		}
		l.blocked[a.ID] = blockedReport{since: since, seen: now}
	} else {
		delete(l.blocked, a.ID)
	}
	l.mu.Unlock()
	if was != body.Blocked {
		s.broadcast(EventAgentChanged, a.Name)
	}
	w.WriteHeader(http.StatusNoContent)
}
