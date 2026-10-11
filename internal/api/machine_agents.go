package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/agents"
)

// agentJSON is the machine-API projection of a provisioned agent. It is the ONE
// shape emitted by BOTH `GET /api/agents` and the `agent` key on a JSON task read
// (GET /api/tasks, GET /api/tasks/{id}), so a host tool learns a single schema and
// the two reads can never disagree about what an agent looks like.
//
// It is a DEDICATED DTO rather than agents.Agent or ui.AgentBrief, deliberately:
//
//   - agents.Agent tags `errorMessage` and `noteId` with `omitempty`. Those are
//     precisely the two values a monitor must see EXPLICITLY — `""` means "no
//     recorded reason", `null` means "not linked to a task" — and omitempty
//     deletes both keys, so a consumer cannot distinguish "absent because empty"
//     from "absent because this server is too old to send it". Every field below
//     is emitted unconditionally.
//   - ui.AgentBrief is a template view type with NO json tags at all (it would
//     marshal as `ID`/`Name`/`DisplayName`/`Status`) and carries only the four
//     fields a task card renders — it has no room for the liveness signal.
//
// 🔴 The two fields that had NO read path before this type existed:
//
//   - ErrorMessage is written by Provisioner.fail (provision.go) and by the
//     reconciler's stuck-in-provisioning backstop (reconcile.go). It was read by
//     nothing — it is the only record of WHY an agent went red, and it was
//     invisible outside `psql`.
//   - KickedOff is loaded into ui.AgentCardView and rendered by no template. It
//     separates "provisioned but the kickoff never landed" (false) from "the task
//     was delivered" (true) — the difference between a dead pod and a wedged run.
type agentJSON struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`      // slug; also the pod namespace key
	Namespace   string `json:"namespace"` // devpod-<name>, for kubectl follow-up
	DisplayName string `json:"displayName"`
	Status      string `json:"status"` // pending|provisioning|running|stopped|error
	// NoteID is the linked task id, or null when the agent is unlinked (an agent
	// created outside a task, or one whose task was deleted — the note_id FK is
	// ON DELETE SET NULL). NOT omitempty: null is a meaningful answer.
	NoteID     *int64 `json:"noteId"`
	Repo       string `json:"repo"`
	RepoBranch string `json:"repoBranch"`
	Model      string `json:"model"`
	// Kind is the agent's kind (agents.Kinds); CCAccount the Claude account NAME a
	// claude-code agent runs on ("" otherwise). Never a token.
	Kind      string `json:"kind"`
	CCAccount string `json:"ccAccount"`
	// KickedOff reports whether the kickoff message was DELIVERED to the agent's
	// gateway. NOT omitempty — `false` is the whole signal ("provisioned but never
	// started"), and omitempty would erase exactly the case worth alerting on.
	KickedOff bool `json:"kickedOff"`
	// KickoffOwed is agents.KickoffOwed: a first message is STORED on the row and
	// nothing delivered it. NOT omitempty, for the same reason as KickedOff.
	//
	// 🔴 IT IS NOT `!kickedOff`, WHICH IS WHY A CONSUMER COULD NOT COMPUTE IT.
	// The other conjunct is agents.pending_note, which is `json:"-"` and reaches
	// no surface at all — so `kickedOff: false` alone cannot separate an agent
	// that is owed a turn from one that was never asked to take one (saved for
	// later with no note, or created outside a dispatch). This field is the half
	// the wire was missing, and it is the BOOLEAN only: the note's content is
	// operator-authored instruction text and is never serialised.
	//
	// ⚠ IT IS TRUE ALONGSIDE ANY status, `running` INCLUDED, and that pairing is
	// the alert worth writing: a live, ready pod that was never told what to do.
	// See agents.KickoffOwed for the measured defect.
	KickoffOwed bool `json:"kickoffOwed"`
	// KickoffFailed is agents.KickoffFailed: the first turn was handed to a gateway
	// (kickedOff is true, so kickoffOwed is false) and it did not complete; the cause
	// is in kickoffError. NOT omitempty, same reasoning as KickoffOwed.
	//
	// 🔴 IT IS NOT DERIVABLE AS `kickedOff && kickoffError != ""` BY A CONSUMER WITHOUT
	// KNOWING THAT IS THE CONTRACT, so it is stated on the wire. It is the alert for
	// the at-most-once delivery's accepted cost: such a turn is never retried
	// automatically. Re-sending it through the agent's chat is safe only when the
	// recorded cause proves nothing was sent (agents.KickoffResendSafe); otherwise the
	// runtime may still be running the turn, and the card's remedy says to check first.
	KickoffFailed bool `json:"kickoffFailed"`
	// ErrorMessage is the stored provisioning/reconcile failure reason, or "" when
	// none was recorded. NOT omitempty, for the same reason: a red agent with an
	// empty reason is a different (and worse) fact than a red agent with one.
	ErrorMessage string `json:"errorMessage"`
	// KickoffError is the last kickoff SEND failure, or "" when none. NOT
	// omitempty, same reasoning as ErrorMessage.
	//
	// 🔴 IT IS SCRUBBED OF THE PENDING NOTE (agents.ScrubNote), AND SO IS
	// ErrorMessage. A failed turn's error quotes up to 512 bytes of the runtime's
	// response body, and the stuck-row verdict appends the last send error to
	// error_message; a runtime that echoes its request would otherwise put the
	// note — `json:"-"` on agents.Agent — on this wire.
	//
	// 🔴 It is the field that separates the two states `kickedOff: true` cannot:
	// an agent working on the message, and an agent whose gateway died holding it.
	// Live, `kickoff message for bold-moth failed: unexpected EOF` existed ONLY in
	// the muster pod's stdout, so the wire shape said `running` / `kickedOff:
	// true` / `errorMessage: ""` about an agent that had never cloned anything.
	KickoffError string `json:"kickoffError"`
	// KickoffAttempts is how many times the kickoff has been DELIVERED — 1 on a
	// normal dispatch, higher once the reconciler has recovered a lost one. A
	// value > 1 is the machine-readable signal that this agent's pod has been
	// dying underneath it.
	KickoffAttempts int `json:"kickoffAttempts"`
	// LastMessageAt is the created_at of the agent's most recent PERSISTED
	// transcript message, or null when it has never produced one. See the contract
	// note on LastActivityAt.
	LastMessageAt *time.Time `json:"lastMessageAt"`
	// LastActivityAt is the LIVENESS field: the newest durable event muster holds
	// for this agent — max(LastMessageAt, UpdatedAt), computed by
	// agents.LastActivity, which is also what the Agents list orders its cards by.
	// Never null, so a consumer can always threshold on it.
	//
	// 🔴 WHY A STATUS FIELD IS NOT ENOUGH, and what this can and cannot detect.
	//
	// WHAT MAKES IT MOVE:
	//   - UpdatedAt moves on a status transition or a config write
	//     (UpdateStatus / SetModel / SetDisplayName / SetHooksToken / SetKickedOff).
	//     It does NOT move while an agent works: an agent that reaches `running`
	//     and then runs for hours has a frozen UpdatedAt, which is exactly how a
	//     wedged agent (status=running, kickedOff=true, pod Running, silent for
	//     four hours) passed every status-only health check.
	//   - LastMessageAt moves when a TURN COMPLETES and its transcript is
	//     persisted: the dispatch kickoff turn (provision.go), each web-chat turn,
	//     and each operator message/reply.
	//
	// HOW TO USE IT: for reference, two verified-healthy dispatched runs closed in
	// 138 s and 149 s. So `status == "running" && now - lastActivityAt > ~10m` is a
	// wedge candidate; `!kickedOff && now - lastActivityAt > 15m` is the
	// never-started case the reconciler is also meant to catch.
	//
	// WHAT IT CANNOT DETECT:
	//   - PROGRESS WITHIN A TURN. A transcript row is written when the turn
	//     RETURNS, not while it streams, so an agent 30 minutes into one long
	//     legitimate turn is indistinguishable here from an agent that died 30
	//     minutes ago. This errs toward FALSE ALARM, never toward false health.
	//   - WORK THAT PRODUCES NO GATEWAY TURN. Commits, file edits and shell work
	//     inside the pod are invisible to muster; only turns are persisted.
	//   - DEATH ITSELF. Nothing emits an event when a pod goes silent — staleness
	//     is inferred by the consumer, not reported by the server.
	//   - It is not a heartbeat and no heartbeat was invented for it: this is only
	//     the newest timestamp already in the database.
	//
	// One erosion to know about: the retention sweep
	// (DeleteChatMessagesOlderThan) can delete a long-idle agent's transcript, at
	// which point LastMessageAt goes back to null and LastActivityAt falls back to
	// UpdatedAt. The fallback keeps the field monotone in meaning ("newest thing we
	// still know about"), never null, and never newer than reality.
	LastActivityAt time.Time `json:"lastActivityAt"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

// newAgentJSON projects a stored agent plus its resolved last-transcript
// timestamp (the zero time when it has none) into the wire shape.
func newAgentJSON(a agents.Agent, lastMsg time.Time) agentJSON {
	v := agentJSON{
		ID:              a.ID,
		Name:            a.Name,
		Namespace:       a.Namespace,
		DisplayName:     a.DisplayName,
		Status:          a.Status,
		Repo:            a.Repo,
		RepoBranch:      a.RepoBranch,
		Model:           a.Model,
		Kind:            agents.ResolveKind(a.Kind),
		CCAccount:       a.CCAccount,
		KickedOff:       a.KickedOff,
		KickoffOwed:     agents.KickoffOwed(a),
		KickoffFailed:   agents.KickoffFailed(a),
		ErrorMessage:    agents.ScrubNote(a.ErrorMessage, a.PendingNote),
		KickoffError:    agents.ScrubNote(a.KickoffError, a.PendingNote),
		KickoffAttempts: a.KickoffAttempts,
		CreatedAt:       a.CreatedAt,
		UpdatedAt:       a.UpdatedAt,
		// agents.LastActivity is the one definition; the Agents list sorts by it.
		LastActivityAt: agents.LastActivity(a, lastMsg),
	}
	if a.NoteID != nil {
		id := *a.NoteID // copy: never alias the store's pointer into a response
		v.NoteID = &id
	}
	if !lastMsg.IsZero() {
		at := lastMsg
		v.LastMessageAt = &at
	}
	return v
}

// lastMessageByAgents resolves the last-persisted-transcript timestamp for the
// given agents in one batched store read.
//
// Best-effort, and the direction of the degradation is deliberate: a lookup
// failure yields an empty map, so LastMessageAt reads null and LastActivityAt
// falls back to the (real) UpdatedAt. A monitor then sees a STALER timestamp than
// the truth and may raise a false alarm — it can never be told an agent is fresher
// than it is. Failing the whole read instead would take away the agent link that
// is the point of the endpoint.
func (s *Server) lastMessageByAgents(ctx context.Context, list []agents.Agent) map[int64]time.Time {
	if s.ext.Agents == nil || len(list) == 0 {
		return map[int64]time.Time{}
	}
	ids := make([]int64, 0, len(list))
	for _, a := range list {
		ids = append(ids, a.ID)
	}
	m, err := s.ext.Agents.LastMessageByAgentIDs(ctx, ids)
	if err != nil {
		s.logger.Printf("agents: last message by ids: %v", err)
		return map[int64]time.Time{}
	}
	return m
}

// handleAPIAgentList handles GET /api/agents (machine, hook-token-gated): every
// provisioned agent as JSON, newest-first as the store returns them.
//
// It closes a hard gap: the only JSON agent read was GET /operator/agents, gated
// behind the reserved Operator agent's OWN token, so host tooling holding the
// shared hook token could dispatch work and never see what became of it. ⚠ THAT
// ROUTE NO LONGER EXISTS AT ALL (task #653 phase two deleted the operator tier),
// so this is now the only JSON agent read, full stop. Unlike the operator route
// it makes NO live k8s call — it is a pure DB read of the
// stored status, matching how the agents list and the task-card join already
// avoid per-agent pod lookups, so it is cheap enough to poll.
func (s *Server) handleAPIAgentList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	list, err := s.ext.Agents.List(ctx)
	if err != nil {
		s.logger.Printf("agents: api list: %v", err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not list agents"})
		return
	}
	lastMsg := s.lastMessageByAgents(ctx, list)
	out := make([]agentJSON, 0, len(list))
	for _, a := range list {
		out = append(out, newAgentJSON(a, lastMsg[a.ID]))
	}
	s.writeJSON(w, http.StatusOK, out)
}

// --- the machine READ of an agent's chat transcript ------------------------

// The bounds on one `messages` read.
//
// 🔴 THE CEILING IS chatHistoryCap, THE SAME NUMBER THE HUMAN PAGE SHIPS, AND IT IS
// REFERENCED RATHER THAN RETYPED. handleAgentDetail and handleAgentChatLog both
// render at most chatHistoryCap messages, so a machine caller cannot ask this route
// for more of a conversation in one request than the operator's own browser loads —
// which is the honest shape for a tier whose whole argument is "no wider than what
// already exists". A second literal here would drift from that one silently, and the
// drift's direction is the bad one: the machine read growing past the page.
//
// ⚠ THE DEFAULT IS DELIBERATELY WELL BELOW THE CEILING. The intended caller is an
// agent spending its context window on the answer, and a chat message is not a
// tweet: a tool_call row carries raw JSON arguments and a tool_result row carries
// dispatch output. 50 is a few screens of conversation; a caller that wants more
// asks for it and reads `hasMore` to learn whether it got everything.
//
// ⚠ THEY ARE EXPORTED FOR ONE REASON, THE SAME ONE ChiefApprovalMaxWait IS:
// muster states both numbers in its `--help` and applies the default itself, and
// this binary deliberately does not link the server package — so the relationship is
// pinned by a test that imports both (cmd/muster's
// TestTheAgentMessageLimitsMatchTheServers) rather than by a build edge. A help
// text quoting a bound the server no longer applies is the thing that rots.
const (
	AgentMessagesDefaultLimit = 50
	AgentMessagesMaxLimit     = chatHistoryCap
)

// agentSessionsCap bounds the session INDEX this response carries.
//
// An agent accumulates a session per conversation and nothing prunes them, so the
// list is unbounded in the table. It is included at all because without it a machine
// caller can read the default session and has no way to learn that any other one
// exists — `sessionCount` reports the true total so a truncated index says so
// rather than reading as "these are all of them".
const agentSessionsCap = 20

// chatMessageJSON is one persisted chat message on the wire.
//
// 🔴 IT IS A DEDICATED DTO RATHER THAN agents.ChatMessage RE-TAGGED, for the reason
// agentJSON above is: that type tags Kind, ToolID, ToolName and ToolOK `omitempty`,
// and `toolOk: false` is the single most load-bearing value in this whole shape — it
// is the difference between a tool call that worked and one that failed. omitempty
// deletes exactly that case, leaving a consumer unable to tell "the dispatch failed"
// from "this server is too old to say". Every field below is emitted
// unconditionally.
//
// ⚠ NO REASSEMBLY IS PERFORMED AND NONE IS NEEDED. Migration 0015's "parts" are
// ordinary chat_messages ROWS, not a side table: one assistant turn is a run of rows
// sharing nothing but their order, distinguished by Kind. So the faithful machine
// projection is the rows themselves, in order, with Kind carried — reassembling them
// into one blob here would destroy the interleaving the migration exists to record
// and would put a second rendering rule in this package beside the template's.
type chatMessageJSON struct {
	ID        int64  `json:"id"`
	SessionID int64  `json:"sessionId"`
	Role      string `json:"role"` // "user" | "assistant"
	// Kind is 'text' | 'tool_call' | 'tool_result'. Legacy rows read 'text'.
	Kind string `json:"kind"`
	// Content is the message body; for a tool_call it is the raw JSON arguments,
	// for a tool_result the dispatch output.
	Content   string    `json:"content"`
	ToolID    string    `json:"toolId"`   // links a tool_call to its tool_result
	ToolName  string    `json:"toolName"` // the function name
	ToolOK    bool      `json:"toolOk"`   // tool_result only: did the dispatch succeed
	CreatedAt time.Time `json:"createdAt"`
}

// chatSessionJSON is one row of the session index.
type chatSessionJSON struct {
	ID        int64     `json:"id"`
	Title     string    `json:"title"` // derived from the first user message; "" until then
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	Active    bool      `json:"active"` // the session this response's messages came from
}

// handleAPIAgentMessages handles GET /api/agents/{name}/messages (machine tier):
// one agent chat session's persisted transcript as JSON.
//
// 🔴 THE GAP IT CLOSES, MEASURED AGAINST LIVE 0.8.42 BEFORE IT EXISTED: nothing
// holding a machine credential could read an agent's chat at all. GET
// /ui/agents/{name}/chat-log is wrapped in requireSession — the operator's signed-in
// browser — and answers 401 to the hook token, to X-Muster-Token and to no
// credential alike. The only machine GETs were /api/tmux/snapshot,
// /api/transcripts/{sessionId} and /api/agents, none of which carries a message. So
// `muster chief ask` could post a message and print ONE reply, and nothing
// could read the conversation afterwards — not even the agent's own past replies.
// The remaining ways to read it were the operator's browser and a SELECT against
// chat_messages, which is a backdoor rather than an interface.
//
// 🔴 requireHookOrServiceToken, THE SAME WRAPPER GET /api/transcripts/{sessionId}
// USES, AND THAT PRECEDENT IS THE ARGUMENT RATHER THAN A CONVENIENCE. A transcript
// tail is Claude Code's own record of a session: tool INPUTS, i.e. file paths, bash
// command lines and edit bodies, plus tool RESULTS, i.e. file contents and command
// output. Chat is what a human typed to an agent and what it typed back. The
// transcript route is strictly the more sensitive of the two and it is already on
// this tier, so this is a route joining an existing trust boundary rather than a
// widening of one. It is written down in cmd/muster's agentRowRoutes with that
// reason, because the set of routes on this wrapper is the thing that must never
// grow quietly.
//
// ⚠ WHAT THAT TIER COSTS, STATED THE WAY transcripts.go STATES IT: the wrapper is
// ENFORCE-WHEN-SET. On a server with no MUSTER_HOOK_TOKEN it serves this to anyone
// who can reach the LAN NodePort. The deployed server sets one.
//
// 🔴 THERE IS NO AGENT HALF, AND THE SENTENCE THAT USED TO SIT HERE CLAIMING ONE
// WAS UNARMED IS THE REASON. It read "the agent half is unarmed in production (no
// MUSTER_CHIEF_TOKEN), so today the shared secret is the only live path" — and
// measured against the live cluster, the door had been ARMED since the previous
// day, a matching agent pod was Running, and that pod's credential had
// already been seen resolving through this tier. The route is now requireHookToken:
// the shared secret, and nothing else. See the registration in agents.go for the
// full measurement and for why self-scoping was not the fix chosen.
//
// ⚠ IT NEVER CREATES A SESSION. resolveSessions/LatestOrCreateSession — what every
// browser path uses — creates one for an agent that has none, which is right for a
// page that is about to be typed into and wrong for a read: an agent that has never
// been spoken to would acquire an empty session as a side effect of being monitored.
// This resolves read-only and answers `sessionId: 0` with an empty list instead.
func (s *Server) handleAPIAgentMessages(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "an agent name is required"})
		return
	}
	a, err := s.ext.Agents.GetByName(ctx, name)
	if err != nil {
		// 🔴 A JSON 404, NOT THE ROUTER'S text/plain ONE — the contract
		// handleTranscriptGet states and muster branches on: a JSON 404 means
		// "the route exists, that agent does not" while a text/plain one means "this
		// client is newer than that server".
		if errors.Is(err, pgx.ErrNoRows) {
			s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "no agent by that name"})
			return
		}
		s.logger.Printf("agents: api messages get %q: %v", name, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not load that agent"})
		return
	}

	q := r.URL.Query()

	// 🔴 AN UNPARSEABLE limit IS A 400, NOT A SILENT FALL-BACK TO THE DEFAULT.
	// `?limit=1OO` answered with 50 rows is a typo that reads as data — the caller
	// believes it asked for 100 and that the conversation is 50 long. An
	// OVER-CEILING limit is different and is deliberately CLAMPED rather than
	// refused: asking for more than the server will give is not a malformed request,
	// and the response reports the limit actually applied so the caller can see it.
	limit := AgentMessagesDefaultLimit
	if v := strings.TrimSpace(q.Get("limit")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "limit must be a positive integer"})
			return
		}
		limit = min(n, AgentMessagesMaxLimit)
	}
	var before int64
	if v := strings.TrimSpace(q.Get("before")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			s.writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "before must be a positive message id (take it from a previous response's nextBefore)",
			})
			return
		}
		before = n
	}

	sessions, err := s.ext.Agents.ListSessions(ctx, a.ID)
	if err != nil {
		s.logger.Printf("agents: api messages list sessions for %q: %v", name, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not list that agent's chat sessions"})
		return
	}

	// 🔴 AN EXPLICIT ?session= THAT DOES NOT BELONG TO THIS AGENT IS A 404, NOT
	// SOMEBODY ELSE'S CONVERSATION. The id is a global sequence over chat_sessions,
	// so without the ownership check `/api/agents/alice/messages?session=<bob's>`
	// would answer bob's transcript under alice's name — a cross-agent read reached
	// by incrementing an integer, and one that looks correct in the response because
	// the response would echo the name from the path.
	active := chatSessionJSON{}
	var activeID int64
	if v := strings.TrimSpace(q.Get("session")); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "session must be a positive chat session id"})
			return
		}
		var owned bool
		for _, c := range sessions {
			if c.ID == id {
				owned, activeID = true, id
				break
			}
		}
		if !owned {
			s.writeJSON(w, http.StatusNotFound, map[string]any{
				"error": "that chat session does not belong to this agent",
			})
			return
		}
	} else if len(sessions) > 0 {
		// The most-recently-active session — ListSessions orders updated_at DESC,
		// and it is the SAME session POST /api/agents/{name}/messages steers into
		// (LatestOrCreateSession), which is what `muster chief ask` posts to. A
		// default that resolved differently would mean asking a question here and
		// reading the answer there.
		activeID = sessions[0].ID
	}

	msgs := []agents.ChatMessage{}
	if activeID != 0 {
		// 🔴 limit+1 IS HOW hasMore IS MEASURED RATHER THAN GUESSED. Reporting
		// `hasMore: len(page) == limit` is wrong at exactly the boundary that
		// matters — a conversation whose remaining length is an exact multiple of
		// the limit would advertise another page forever, and a caller looping on it
		// would ask for an empty one. One extra row answers the question directly.
		page, err := s.ext.Agents.ListChatMessagesPage(ctx, activeID, before, limit+1)
		if err != nil {
			s.logger.Printf("agents: api messages page for %q session %d: %v", name, activeID, err)
			s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not read that chat session"})
			return
		}
		msgs = page
	}
	// The page is CHRONOLOGICAL, so the extra row — the one OLDER than the page —
	// is at the FRONT. Trimming the back would silently drop the newest message and
	// hand the caller a transcript missing its last turn.
	hasMore := false
	if len(msgs) > limit {
		hasMore = true
		msgs = msgs[len(msgs)-limit:]
	}

	out := make([]chatMessageJSON, 0, len(msgs))
	for _, m := range msgs {
		kind := m.Kind
		if kind == "" {
			// Migration 0015 defaults the column to 'text'; a store or fixture that
			// leaves it empty must still emit the vocabulary a consumer branches on.
			kind = "text"
		}
		out = append(out, chatMessageJSON{
			ID: m.ID, SessionID: m.SessionID, Role: m.Role, Kind: kind, Content: m.Content,
			ToolID: m.ToolID, ToolName: m.ToolName, ToolOK: m.ToolOK, CreatedAt: m.CreatedAt,
		})
	}

	// nextBefore is the cursor for the page OLDER than this one: the oldest id
	// returned. It is emitted only when there IS an older page, so a caller loops
	// `while nextBefore != 0` rather than having to compare ids itself.
	var nextBefore int64
	if hasMore && len(out) > 0 {
		nextBefore = out[0].ID
	}

	index := make([]chatSessionJSON, 0, min(len(sessions), agentSessionsCap))
	for i, c := range sessions {
		row := chatSessionJSON{ID: c.ID, Title: c.Title, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt, Active: c.ID == activeID}
		if c.ID == activeID {
			active = row
		}
		if i < agentSessionsCap {
			index = append(index, row)
		}
	}

	s.writeJSON(w, http.StatusOK, map[string]any{
		"agent":        a.Name,
		"agentId":      a.ID,
		"sessionId":    activeID,
		"sessionTitle": active.Title,
		// The TRUE total, never len(sessions[:cap]) — a truncated index that reported
		// its own length would read as "this agent has 20 sessions".
		"sessionCount": len(sessions),
		"sessions":     index,
		"messages":     out,
		"count":        len(out),
		"limit":        limit,
		"maxLimit":     AgentMessagesMaxLimit,
		"before":       before,
		"hasMore":      hasMore,
		"nextBefore":   nextBefore,
	})
}

// --- the machine WRITE: send one message to an agent and print its reply -----

// handleAPIAgentSendMessage handles POST /api/agents/{name}/messages (machine
// tier): post one message to an agent's current chat session and return its reply.
//
// 🔴 IT IS THE RE-HOMED `chief ask` DOOR (task #633 criterion 1), AND THE POINT IS
// WHICH TIER IT IS **NOT** ON. The verb used to reach
// POST /operator/agents/{id}/message behind requireOperatorToken, which resolved
// the bearer to an agent ROW and required that row to be the reserved Operator.
// That made the only machine path to chief a dependency on a second privileged
// AGENT existing — so deleting that agent broke the CLI. Nothing about "send a
// message to an agent" needs the operator's identity; it needed a route, and the
// route had already been built for the operator's browser. ⚠ THAT ROUTE AND THAT
// GATE ARE BOTH DELETED as of task #653 phase two — every sentence about them here
// is history.
//
// 🔴 KEYED ON {name}, DELIBERATELY, AND IT IS NOT COSMETIC. `chief ask --agent`
// addresses an ARBITRARY agent — measured against three distinct agents in the
// week before this change — so a chief-only door would have satisfied the card's
// criterion (which exercises `--agent chief`) while deleting a working feature.
// The name is also the key the READ next door already uses, so the pair
// GET/POST /api/agents/{name}/messages is one resource rather than two spellings.
//
// 🔴 IT STEERS THROUGH LatestOrCreateSession — THE SAME CALL THE (NOW DELETED)
// OPERATOR ROUTE MADE, AND THE SAME SESSION THE READ ABOVE DEFAULTS TO. The message must land in
// the gateway context and the transcript a human already sees, or a conversation
// held here would be invisible from `agent messages` and from the agent's detail
// page. That relationship is asserted, not assumed:
// TestTheMachineAgentMessageWriteSteersIntoTheSessionTheReadReturns posts here and
// reads it back over the GET.
//
// 🔴 requireArmedHookToken — THE SHARED SECRET, NOT THE AGENT TIER, AND FAIL-CLOSED
// RATHER THAN ENFORCE-WHEN-SET. The credential choice is the reason the READ is on
// it: the handler keys on an arbitrary {name} with NO self-scoping, so on the wider
// tier one admitted pod could steer EVERY other agent. A write is strictly worse
// than the read that argument was made about. The caller this exists for is
// `chief ask`, run from the operator's own host, which holds the shared secret
// already.
//
// ⚠ THE ARMING HALF IS A SEPARATE CLAIM AND IT IS THE ONE THE ROUND-1 AUDIT FORCED.
// Every other requireHookToken route serves OPEN when MUSTER_HOOK_TOKEN is unset;
// this one answers 503. The route it re-homed off (requireOperatorToken) demanded a
// bearer unconditionally, so inheriting the open default would have turned a
// fail-CLOSED agent-steering write into a fail-OPEN one as a side effect of moving
// it. It also concedes what the tier comparison above does not: the credential to
// steer an agent went from the reserved operator's in-cluster uuid — held by one pod
// — to a shared secret that names no caller and travels in cleartext to the LAN
// NodePort many times a day. Full argument at the registration site
// (registerAgentRoutes) and on the wrapper (requireArmedHookToken).
//
// ⚠ IT USES Gateway.Chat, NOT chatTurn: the operator route it replaces ran a
// plain (tool-less) turn, and this change re-homes a path rather than widening
// what the agent can do while answering on it.
//
// 🔴 THIS IS THE ROUTE THE EMPTY-REPLY DEFECT WAS MEASURED ON, AND THE READING IT
// PRODUCES IS WORTH KNOWING: three consecutive turns against a live agent answered
// HTTP 200 with `{"reply":""}` in under three seconds each, because the model was a
// reasoning model and the tool-less turn ran over streaming chat-completions, which
// drops every content delta for that model class. Gateway.Chat runs over
// /v1/responses now; the measurement and the controls are on its doc. Note what this
// handler does with an empty reply — it writes NO assistant message and still
// answers 200 — so the only trace of such a turn is a user message with no answer
// beside it in the transcript.
func (s *Server) handleAPIAgentSendMessage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "an agent name is required"})
		return
	}
	var body struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Message) == "" {
		// An unset shell variable must not become an empty message the agent
		// answers anyway — the same refusal `chief ask` makes client-side.
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "message required"})
		return
	}

	a, err := s.ext.Agents.GetByName(ctx, name)
	if err != nil {
		// A JSON 404, for the reason the read states: "the route exists, that agent
		// does not" must be distinguishable from the router's text/plain 404, which
		// means "this client is newer than that server".
		if errors.Is(err, pgx.ErrNoRows) {
			s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "no agent by that name"})
			return
		}
		s.logger.Printf("agents: api message get %q: %v", name, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not load that agent"})
		return
	}

	sess, err := s.ext.Agents.LatestOrCreateSession(ctx, a.ID, a.Name)
	if err != nil {
		s.logger.Printf("agents: api message session for %q: %v", name, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not open a chat session for that agent"})
		return
	}
	_, _ = s.ext.Agents.AddChatMessage(ctx, agents.ChatMessage{
		AgentID: a.ID, SessionID: sess.ID, Role: "user", Content: body.Message,
	})
	reply, err := s.ext.Gateway.Chat(ctx, a, sess.SessionKey, body.Message, nil)
	if err != nil {
		s.writeJSON(w, http.StatusBadGateway, map[string]any{"error": "agent did not respond: " + err.Error()})
		return
	}
	if reply != "" {
		_, _ = s.ext.Agents.AddChatMessage(ctx, agents.ChatMessage{
			AgentID: a.ID, SessionID: sess.ID, Role: "assistant", Content: reply,
		})
	}
	// `agent`, `agentId` and `sessionId` are emitted alongside the reply — which
	// the operator route did NOT carry. They are what lets a caller prove which
	// agent answered and which conversation to read the answer back from; a bare
	// `{"reply": …}` cannot tell a mis-addressed message from a correct one.
	s.writeJSON(w, http.StatusOK, map[string]any{
		"agent":     a.Name,
		"agentId":   a.ID,
		"sessionId": sess.ID,
		"reply":     reply,
	})
}
