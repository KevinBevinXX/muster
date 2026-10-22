package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/pins"
	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/ui"
)

// chatHistoryCap bounds how many of an agent's most recent chat messages the
// page renders ship (newest kept). The full transcript still lives in the DB and
// streams over the chat WebSocket; this only caps the initial page payload.
const chatHistoryCap = 200

// registerAgentRoutes wires the Agents tab + lifecycle + realtime endpoints when
// an agents store is set. Control/logs/chat routes additionally require a
// provisioner (a real in-cluster wiring); without one only the list renders.
// 🔴 NO EARLY RETURN ON A nil DEPENDENCY — see the package doc in server.go.
// Every handler below answers its own "nothing to serve from" case, so the
// recorded route set is a function of the CODE and never of the fixture.
func (s *Server) registerAgentRoutes(mux Mux) {
	mux.HandleFunc("GET /ui/agents", s.requireSession(s.handleAgentsContent))
	// Live status icon poll for the agent-detail / operator header (the bare
	// colored dot, no label). Works without a provisioner (falls back to stored
	// status), so it's registered alongside the list route.
	mux.HandleFunc("GET /ui/agents/{id}/status", s.requireSession(s.handleAgentStatusIcon))
	// Agent-reply notifications: the FAB's panel + OOB badge, and the per-session
	// mark-read used by the chat client when a turn finishes. Both are pure DB
	// reads/writes, so they're registered without a provisioner.
	mux.HandleFunc("GET /ui/notifications", s.requireSession(s.handleNotifications))
	mux.HandleFunc("POST /agents/sessions/{sid}/read", s.requireSession(s.handleSessionRead))
	// Machine read (hook-token-gated): the agent roster as JSON for host tooling.
	// Registered here, BEFORE the provisioner gate, because it is a pure DB read —
	// it never touches k8s, so it works in a provisioner-less deployment too.
	mux.HandleFunc("GET /api/agents", s.requireHookToken(s.handleAPIAgentList))
	// 🔴 THE MACHINE READ OF AN AGENT'S CHAT, ON THE AGENT-IDENTIFIED TIER — SAID
	// OUT LOUD RATHER THAN SLIPPED IN BESIDE THE ROSTER ABOVE. Before it, no
	// machine credential could read an agent's messages at all: the chat-log route
	// two dozen lines below is requireSession (the operator's browser), and the only
	// machine GETs in the whole server were the roster, the tmux snapshot and the
	// transcripts. So `muster chief ask` could post a message and print one
	// reply, and nothing could read the conversation afterwards.
	//
	// 🔴 THE SHARED SECRET ONLY — requireHookToken, EXACTLY LIKE THE ROSTER ABOVE,
	// AND THE FIRST DRAFT OF THIS ROUTE GOT IT WRONG IN THE REASSURING DIRECTION.
	// It shipped on requireHookOrAgentToken (the wrapper Phase 0 step 5 replaced with
	// requireHookOrServiceToken) carrying the sentence "unarmed, which
	// is production, that half admits nobody". Measured against the live cluster at
	// commit 70d689337, every clause of that was false: the chief credential IS
	// wired into the deployment (it was armed the day BEFORE this route was
	// written), an agent pod holding a matching row
	// token is Running, and that pod's credential had already been observed
	// resolving through this very tier — an agent-attributed refusal naming it by
	// name. So the half described as admitting nobody admitted a live pod.
	//
	// 🔴 AND THE HANDLER KEYS ON AN ARBITRARY {name} WITH NO SELF-SCOPING, so the
	// blast radius was never "an agent reads its own conversation" — it was any
	// admitted agent reading EVERY agent's conversation with the operator. A
	// self-scoping check would have been the other available fix; it is not the one
	// taken, because nobody asked for an in-pod caller at all.
	//
	// ⚠ THE REQUIREMENT THIS ROUTE EXISTS FOR IS SATISFIED HERE. The caller who
	// asked for it runs on an operator host and presents the SHARED hook token, which
	// this wrapper admits; the agent half bought that caller nothing and cost a
	// fleet-wide chat read. The transcripts precedent (GET /api/transcripts/{id},
	// on the wider tier and carrying strictly more) is real and still stands — it
	// is simply not a reason to widen a route no in-pod caller needs.
	//
	// ⚠ REGISTERED BEFORE THE PROVISIONER GATE, like the roster read and for the
	// same reason: it is a pure DB read and never touches k8s, so it works on a
	// provisioner-less deployment.
	mux.HandleFunc("GET /api/agents/{name}/messages", s.requireHookToken(s.handleAPIAgentMessages))
	// 🔴 THE MACHINE WRITE HALF, AND THE ROUTE `muster chief ask` NOW POSTS TO
	// (task #633 criterion 1). It used to post to POST /operator/agents/{id}/message
	// behind requireOperatorToken — a gate that resolved the bearer to an agent ROW
	// and required that row to be the RESERVED OPERATOR. So the only machine path to
	// talk to chief was a dependency on a second privileged AGENT existing, and
	// deleting that agent broke the CLI. Nothing about "message an agent" needs the
	// operator's identity. ⚠ BOTH THE ROUTE AND THE GATE ARE DELETED as of task #653
	// phase two, so the comparison below is with history, not with a live surface.
	//
	// 🔴 ON THE HOOK CREDENTIAL, NOT THE AGENT TIER, for the reason stated at the
	// READ above only more so. The handler keys on an ARBITRARY {name} with no
	// self-scoping, so on the agent tier one admitted pod could STEER every other
	// agent — strictly worse than the fleet-wide chat READ that argument was
	// originally made about. Its caller runs on the operator's host and presents the
	// shared secret.
	//
	// 🔴 requireARMEDHookToken — FAIL-CLOSED, AND THE ONE ROUTE ON THIS CREDENTIAL
	// THAT IS. Every other requireHookToken route is enforce-when-set: with
	// MUSTER_HOOK_TOKEN unset the wrapper calls the handler and the route is open to
	// anyone who can reach the LAN NodePort — tmux.go, transcripts.go, layout.go,
	// attention.go and comment_delete.go each carry that warning at their own routes.
	// This one refuses instead. Two reasons, and the audit that produced them is
	// task #633 round 1:
	//
	//   - IT REPLACED A FAIL-CLOSED GATE. POST /operator/agents/{id}/message sat on
	//     requireOperatorToken, which demanded a bearer UNCONDITIONALLY and resolved it
	//     to one specific agent row. Measured on one server with no bearer, BEFORE task
	//     #653 phase two deleted that route (it answers 404 now):
	//     POST /api/agents/chief/messages -> 200, POST /operator/agents/1/message ->
	//     401. Inheriting the enforce-when-set convention would have turned an
	//     agent-steering WRITE from fail-closed into fail-open as a side effect of
	//     moving it. Back-compat is a debt to callers that already exist; this route
	//     shipped with this change and has none.
	//   - THE CREDENTIAL ITSELF GOT WEAKER, AND THIS PR ARGUED THE WRONG COMPARISON.
	//     The bullet above compares this tier with the AGENT tier and wins. Against
	//     what it REPLACED it loses: steering an agent used to need the reserved
	//     operator's in-cluster uuid, held by one pod and readable only from that
	//     pod's Secret. It now needs MUSTER_HOOK_TOKEN — one shared value handed to
	//     every hook script on both hosts, travelling in cleartext to the LAN
	//     NodePort many times a day, naming no caller (auth.go's ChiefWriteRefusal
	//     says so at length, which is why that door refuses to equal it). That is a
	//     deliberate trade for a CLI that works without a second privileged agent,
	//     and refusing when the shared secret is absent is the least it should cost.
	//
	// ⚠ ITS GET TWIN STAYS enforce-when-set. Not an oversight: the read discloses a
	// transcript, the write runs arbitrary work inside an agent instance (a shell,
	// its repository checkout, whatever connectors it has been granted), and the
	// read predates this change.
	//
	// ⚠ REGISTERED AFTER THE PROVISIONER GATE, unlike its GET twin: the write
	// reaches Provisioner.Chat, so on a provisioner-less deployment there is nothing
	// for it to talk to.
	mux.HandleFunc("POST /api/agents/{name}/messages", s.requireArmedHookToken(s.requireGatewayProvisioner(s.handleAPIAgentSendMessage)))
	mux.HandleFunc("GET /ui/agents/new", s.requireSession(s.handleAgentNewModal))
	mux.HandleFunc("GET /ui/agents/repos", s.requireSession(s.handleAgentRepoOptions))
	mux.HandleFunc("GET /ui/agents/notes", s.requireSession(s.handleAgentNoteOptions))
	// Inline rename: the pencil swaps in the edit form; cancel re-fetches the card.
	mux.HandleFunc("GET /ui/agents/{id}/rename", s.requireSession(s.handleAgentRenameForm))
	mux.HandleFunc("GET /ui/agents/{id}/card", s.requireSession(s.handleAgentCard))
	mux.HandleFunc("GET /api/openrouter/models", s.requireSession(s.handleOpenRouterModels))
	mux.HandleFunc("POST /agents", s.requireSession(s.requireLifecycleProvisioner(s.handleAgentCreate)))
	mux.HandleFunc("POST /agents/{id}/start", s.requireSession(s.requireLifecycleProvisioner(s.handleAgentStart)))
	mux.HandleFunc("POST /agents/{id}/stop", s.requireSession(s.requireLifecycleProvisioner(s.handleAgentStop)))
	mux.HandleFunc("POST /agents/{id}/name", s.requireSession(s.handleAgentName))
	mux.HandleFunc("POST /agents/{id}/model", s.requireSession(s.handleAgentModel))
	// Same-origin, like every other form that changes where the chief's turns
	// go (requireOperatorWrite): a cross-site POST could otherwise open sessions
	// or move the default session plugin deliveries and `chief ask` land in.
	mux.HandleFunc("POST /agents/{id}/sessions", s.requireOperatorWrite(s.handleAgentSessionCreate))
	mux.HandleFunc("POST /agents/{id}/sessions/{sid}/default", s.requireOperatorWrite(s.handleAgentSessionDefault))
	mux.HandleFunc("DELETE /agents/{id}", s.requireSession(s.requireLifecycleProvisioner(s.handleAgentDelete)))
	mux.HandleFunc("GET /agents/{name}", s.requireSession(s.handleAgentDetail))
	// Live transcript partial: the detail page's #chat-log re-fetches this on the
	// sse:chat.reply event so a SERVER-SIDE (kickoff) turn — which never streams over
	// this page's WS — appears without a reload.
	mux.HandleFunc("GET /ui/agents/{name}/chat-log", s.requireSession(s.handleAgentChatLog))
	mux.HandleFunc("GET /ui/agents/{name}/task", s.requireSession(s.handleAgentTaskModal))
	mux.HandleFunc("GET /agents/{name}/logs/stream", s.requireSession(s.requireLifecycleProvisioner(s.handleAgentLogsStream)))
	mux.HandleFunc("GET /agents/{name}/ws", s.requireSession(s.requireGatewayProvisioner(s.handleAgentWS)))
	// The claude-code agent's live terminal (terminal.go): the page, and the
	// socket bridging it to a pty exec in the pod. requireSession ONLY — the
	// hook token must not open a terminal — and the socket is lifecycle, not
	// chat: it reaches the driver's exec, not the agent's gateway.
	// A message as a form post (agent_send.go): the Terminal page's composer,
	// and the chat form when its socket cannot carry it. GET is where such a
	// post lands when it comes back from a sign-in at a forward-auth edge.
	//
	// ⚠ The gateway check sits OUTSIDE the same-origin one (requireOperatorWrite
	// is the session and same-origin wrappers, spelled out here to put it
	// between them): "this build cannot deliver a message at all" is the more
	// useful refusal, and the one provisioner_seam_test.go drives every route
	// on its ledger for.
	mux.HandleFunc("POST /agents/{name}/send", s.requireSession(s.requireGatewayProvisioner(s.requireSameOrigin(s.handleAgentSend))))
	mux.HandleFunc("GET /agents/{name}/send", s.requireSession(s.handleAgentSendAgain))
	mux.HandleFunc("GET /agents/{name}/terminal", s.requireSession(s.handleAgentTerminalPage))
	mux.HandleFunc("GET /agents/{name}/terminal/ws", s.requireSession(s.requireLifecycleProvisioner(s.handleAgentTerminalWS)))
}

// handleAgentsContent serves the /ui/agents partial: cards (status reconciled
// with live pods) plus the FAB + dispatch modal.
func (s *Server) handleAgentsContent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cards, mirrors, err := s.agentsListCards(ctx)
	if err != nil {
		s.logger.Printf("agents: list: %v", err)
		http.Error(w, "could not load agents", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := ui.RenderAgentsList(w, cards, mirrors, s.recentRefs(ctx)); err != nil {
		s.logger.Printf("agents: render: %v", err)
	}
}

// agentsListCards is the Agents list's cards as the viewer sees them: agents
// most recently active first, pins marked, and the viewer's mirrored sessions.
// The list partial renders them, and the shell resolves the "Recently viewed"
// strip against them (handleIndex), so both read the one list.
//
// Every card carries its attention badge (withCardAttention).
func (s *Server) agentsListCards(ctx context.Context) ([]ui.AgentCardView, []ui.MirrorCardView, error) {
	list, err := s.ext.Agents.List(ctx)
	if err != nil {
		return nil, nil, err
	}

	instIdx := s.instanceIndex(ctx)
	// 🔴 MOST RECENTLY ACTIVE FIRST, BY agents.LastActivity — the same definition
	// GET /api/agents publishes as lastActivityAt. The store's own order
	// (created_at DESC) is left alone because the machine read documents
	// "newest-first as the store returns them" to its consumers; only this view
	// re-orders. One batched transcript read serves both the order and the
	// footer's "active … ago", so the two cannot disagree.
	lastMsg := s.lastMessageByAgents(ctx, list)
	agents.SortByLastActivity(list, lastMsg)

	// The viewer's pins, read once: each card is marked, and the list draws the
	// pinned ones first (ui.AgentsCardsWithMirrors — the one place that orders
	// by pin, and that interleaves these agents with the mirrored entries by
	// activity; the order above decides ties among agents).
	pinned := s.pinSet(ctx)
	cards := make([]ui.AgentCardView, 0, len(list))
	isChief := chiefPredicate(list)
	// The viewer's session focus (internal/focus), read once for every card.
	focused := s.focusIndex(ctx)
	for _, a := range list {
		cards = append(cards, s.cardViewIndexed(a, instIdx, lastMsg[a.ID], isChief, pinned, focused))
	}
	// The viewer's mirrored sessions (internal/mirror), from the plugin's
	// in-memory hub, scoped to this request's principal. The list interleaves
	// them with the agents by activity (ui.AgentsCardsWithMirrors).
	mirrors := s.mirrorCards(ctx, pinned, focused)
	s.withCardAttention(ctx, cards, mirrors)
	return cards, mirrors, nil
}

// handleAgentNewModal serves the /ui/agents/new partial: the dispatch form. It
// renders INSTANTLY — the repo + task option lists (which hit the GitHub API /
// DB) lazy-load into animated skeletons via /ui/agents/repos and /ui/agents/notes
// once the modal opens, so the form never blocks on a slow upstream.
//
// When ?note=<id> is set (the Task-card Dispatch path) it loads the task and
// renders the TASK-SCOPED confirm (fixed Model/Repository rows
// pre-filled from the task's dispatch config). A missing/failed load falls back
// to the plain FAB form so the modal always renders something usable.
func (s *Server) handleAgentNewModal(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if noteParam := r.URL.Query().Get("note"); noteParam != "" && s.ext.Notes != nil {
		if id, err := strconv.ParseInt(noteParam, 10, 64); err == nil {
			if n, err := s.ext.Notes.Get(r.Context(), id); err == nil {
				v := s.buildTaskDispatchView(r.Context(), n)
				// Which document is opening this modal decides where its submit can
				// land: #agents-list exists only on the shell. See
				// ui.TaskDispatchView.FromDetailPage.
				v.FromDetailPage = rendersInDetailShape(r, n.ID)
				v.Kinds = s.kindChoices(r.Context())
				if rerr := ui.RenderDispatchModalTask(w, v); rerr != nil {
					s.logger.Printf("agents: render task dispatch modal: %v", rerr)
				}
				return
			} else {
				s.logger.Printf("agents: dispatch modal load task %d: %v", id, err)
			}
		}
	}
	if err := ui.RenderDispatchModal(w, s.kindChoices(r.Context())); err != nil {
		s.logger.Printf("agents: render dispatch modal: %v", err)
	}
}

// kindChoices is what the dispatch form offers THE CALLER: the enabled kinds,
// the caller's OWN Claude account names (never tokens — and never the admin's
// pool for a member), and which kinds the caller has no credential for.
func (s *Server) kindChoices(ctx context.Context) ui.KindChoices {
	k := ui.KindChoices{Kinds: s.ext.enabledKinds()}
	if s.ext.Kinds != nil {
		owner := callerID(ctx)
		if names, err := s.ext.Kinds.ClaudeAccounts(ctx, owner); err == nil {
			k.Accounts = names
		} else {
			s.logger.Printf("agents: user %d's Claude accounts: %v", owner, err)
		}
		k.Unavailable = s.ext.Kinds.Unavailable(ctx, owner)
	}
	return k
}

// callerID is the acting principal's user, or 0 (no user: every per-owner
// answer is then "nothing").
func callerID(ctx context.Context) int64 {
	if p, ok := authz.From(ctx); ok && !p.All() {
		return p.UserID
	}
	return 0
}

// buildTaskDispatchView resolves a task's dispatch config into the task-scoped
// modal view: the stored model/repo/branch and — when the task has no stored
// repo — a best-effort repo inference from its directory.
func (s *Server) buildTaskDispatchView(ctx context.Context, n notes.Note) ui.TaskDispatchView {
	// ui.TaskTitle, NOT n.Directory: a producer that sends the real `title` field
	// (the capture extension since #244) would otherwise show its body's first
	// line here instead of its heading.
	label := ui.TaskTitle(n)
	if label == "" {
		label = firstLine(n.Body)
	}
	v := ui.TaskDispatchView{
		NoteID:     n.ID,
		Label:      truncate(label, 80),
		Model:      n.Model,
		Repo:       n.Repo,
		RepoBranch: n.RepoBranch,
	}
	// Infer the repo from the task's directory when none is stored (best-effort;
	// non-fatal): match the directory (or its last path segment) against a
	// connected repo's full name / last segment.
	if v.Repo == "" && n.Directory != "" {
		v.Repo = s.inferRepoFromDirectory(ctx, n.Directory)
	}
	return v
}

// inferRepoFromDirectory returns a connected repo whose full name equals the
// directory, or whose last path segment matches the directory's last segment;
// "" when there's no GitHub connection, the list fails, or nothing matches. Cheap
// and non-fatal — a miss just leaves the repo blank in the confirm. The match
// itself is the pure matchRepoByDirectory (unit-tested).
func (s *Server) inferRepoFromDirectory(ctx context.Context, directory string) string {
	list, err := s.githubRepos(ctx)
	if err != nil {
		return ""
	}
	names := make([]string, 0, len(list))
	for _, rp := range list {
		names = append(names, rp.FullName)
	}
	return matchRepoByDirectory(directory, names)
}

// matchRepoByDirectory picks the repo full name that best matches a task's
// working directory: an exact full-name match wins; otherwise the first repo
// whose last path segment equals the directory's last segment. "" when nothing
// matches. Pure (no I/O) so the inference logic is unit-testable with a faked
// repo list.
func matchRepoByDirectory(directory string, repoFullNames []string) string {
	if strings.TrimSpace(directory) == "" {
		return ""
	}
	for _, name := range repoFullNames {
		if name == directory {
			return name
		}
	}
	dirBase := path.Base(strings.TrimRight(directory, "/"))
	for _, name := range repoFullNames {
		if path.Base(name) == dirBase {
			return name
		}
	}
	return ""
}

// handleAgentRepoOptions serves /ui/agents/repos: the repo combobox <li> options
// (slow — hits the GitHub API), swapped in over the skeleton.
func (s *Server) handleAgentRepoOptions(w http.ResponseWriter, r *http.Request) {
	repos := make([]ui.RepoView, 0)
	if list, err := s.githubRepos(r.Context()); err == nil {
		for _, rp := range list {
			repos = append(repos, ui.RepoView{FullName: rp.FullName, Private: rp.Private})
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := ui.RenderRepoOptions(w, repos); err != nil {
		s.logger.Printf("agents: render repo options: %v", err)
	}
}

// handleAgentNoteOptions serves /ui/agents/notes: the task combobox <li> options,
// swapped in over the skeleton.
func (s *Server) handleAgentNoteOptions(w http.ResponseWriter, r *http.Request) {
	notesOpts := make([]ui.NoteOption, 0)
	if s.ext.Notes != nil {
		if list, err := s.ext.Notes.ListSummaries(r.Context()); err == nil {
			for _, n := range list {
				// ui.TaskTitle, NOT n.Directory — see buildTaskDispatchView.
				label := ui.TaskTitle(n)
				if label == "" {
					label = firstLine(n.Body)
				}
				notesOpts = append(notesOpts, ui.NoteOption{ID: n.ID, Label: truncate(label, 60)})
			}
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := ui.RenderNoteOptions(w, notesOpts); err != nil {
		s.logger.Printf("agents: render note options: %v", err)
	}
}

// handleAgentStatusIcon serves GET /ui/agents/{id}/status: the bare status icon
// (no label) for the agent-detail / operator header, reflecting live pod state.
// It loads the agent, computes the live status against pods (when a provisioner
// is wired) exactly like handleAgentDetail, else falls back to the stored status,
// and writes ui.RenderStatusIcon. Polled by liveStatusIcon (load + every 10s).
func (s *Server) handleAgentStatusIcon(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	a, err := s.ext.Agents.Get(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		s.logger.Printf("agents: status %d: %v", id, err)
		http.Error(w, "could not load agent", http.StatusInternalServerError)
		return
	}
	status := s.liveStatus(ctx, a)
	awaiting := s.AwaitingApproval(a.ID)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The card says what it was drawn with (ui.StatusPollPath); when that is no
	// longer true its data-wait is stale, so the list is asked to re-render.
	stale := false
	if drawn := r.URL.Query().Get("awaiting"); drawn != "" && (drawn == "1") != awaiting {
		stale = true
	}
	// The work chip too (agent_work.go): a report that expired is announced by no
	// POST, so this poll is what takes a dead pod's chip off the card.
	if drawn := r.URL.Query().Get("work"); drawn != "" && drawn != ui.WorkPollToken(s.cardWork(a.ID, status)) {
		stale = true
	}
	if stale {
		w.Header().Set("HX-Trigger", ui.AgentsAwaitingEvent)
	}
	if err := ui.RenderStatusIcon(w, status, awaiting); err != nil {
		s.logger.Printf("agents: render status %d: %v", id, err)
	}
}

// handleNotifications serves GET /ui/notifications: the agent-reply panel rows
// plus the out-of-band FAB badge, built from the per-agent unread summary. The
// shell's FAB hx-gets this on load, on the SSE chat.reply event, and on a poll.
func (s *Server) handleNotifications(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	unread, err := s.ext.Agents.UnreadByAgent(ctx)
	if err != nil {
		s.logger.Printf("agents: unread by agent: %v", err)
		// Degrade to an empty panel (badge hidden) rather than 500 the shell.
		unread = nil
	}
	rows := make([]ui.NotificationRow, 0, len(unread))
	for _, u := range unread {
		rows = append(rows, ui.NotificationRow{
			Name: u.AgentName, DisplayName: u.DisplayName, SessionID: u.SessionID, Count: u.Count,
		})
	}
	active := s.activeNotificationRows(ctx)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := ui.RenderNotifications(w, active, rows); err != nil {
		s.logger.Printf("agents: render notifications: %v", err)
	}
}

// activeNotificationRows resolves the server-side active-agent registry into UI
// rows: a display name + a deep-link session for each, plus a live phase or a
// relative "Xm ago" label. Best-effort — a per-agent lookup failure links to the
// bare detail page (the server redirects it to the active session) rather than
// dropping the row or failing the panel.
func (s *Server) activeNotificationRows(ctx context.Context) []ui.ActiveRow {
	if s.ext.Agents == nil {
		return nil
	}
	now := s.activeAgentsNow()
	entries := s.activeAgentsList(requestOwner(ctx))
	rows := make([]ui.ActiveRow, 0, len(entries))
	for _, e := range entries {
		row := ui.ActiveRow{
			Name:     e.Name,
			Phase:    e.Phase,
			InFlight: e.InFlight,
		}
		if !e.InFlight {
			row.Since = humanizeSince(now.Sub(e.At))
		}
		// Resolve display name + latest session to deep-link, on the caller's
		// own (row-level-secured) context. 🔴 AN AGENT THE CALLER CANNOT READ IS
		// DROPPED, not shown by name: the registry is server-wide, and this
		// lookup is the second, independent owner check behind the list's
		// filter (it used to keep the row "non-fatally" — which, with a second
		// user, named that user's agent in this one's panel).
		a, err := s.ext.Agents.GetByName(ctx, e.Name)
		if err != nil {
			continue
		}
		row.DisplayName = a.DisplayName
		if sess, serr := s.ext.Agents.LatestOrCreateSession(ctx, a.ID, a.Name); serr == nil {
			row.SessionID = sess.ID
		}
		rows = append(rows, row)
	}
	return rows
}

// humanizeSince renders an elapsed duration as a compact "just now" / "Xs ago" /
// "Xm ago" label for the recently-active notification rows.
func humanizeSince(d time.Duration) string {
	if d < 5*time.Second {
		return "just now"
	}
	if d < time.Minute {
		return strconv.Itoa(int(d.Seconds())) + "s ago"
	}
	return strconv.Itoa(int(d.Minutes())) + "m ago"
}

// handleSessionRead handles POST /agents/sessions/{sid}/read: clear a session's
// unread replies (the chat client calls this when a turn finishes). Responds 204.
func (s *Server) handleSessionRead(w http.ResponseWriter, r *http.Request) {
	sid, err := strconv.ParseInt(r.PathValue("sid"), 10, 64)
	if err != nil {
		http.Error(w, "bad session id", http.StatusBadRequest)
		return
	}
	if err := s.ext.Agents.MarkSessionRead(r.Context(), sid); err != nil {
		s.logger.Printf("agents: mark session %d read: %v", sid, err)
		http.Error(w, "could not mark read", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// cardViewIndexed is the ONE place an agent row becomes a card view.
//
// 🔴 IT IS ONE FUNCTION BECAUSE IT WAS TWO, AND THE DUPLICATE IS WHAT LET A FIELD
// BE RENDERED ON ONE SURFACE AND NOT THE OTHER. handleAgentsContent open-coded
// this literal for the LIST and cardViewFor wrote its own for the single-card
// re-render (the inline rename's Cancel, GET /ui/agents/{id}/card) — the same
// eleven fields, spelled twice, with the only difference being that one fetched
// the instance index itself. A card composed in two places is a card that
// disagrees with itself after the next additive field: the list would show a
// badge the re-rendered card drops, which reads as "the warning went away".
// TestOneCardComposerFeedsEverySurface pins the single site.
//
// The status is reconciled against live instances through liveStatusIndexed, so
// callers in a loop pass an index they fetched once; cardViewFor is the
// single-agent spelling.
func (s *Server) cardViewIndexed(a agents.Agent, idx map[string]*provision.Instance, lastMsg time.Time, isChief func(agents.Agent) bool, pinned pins.Set, focused focusIndex) ui.AgentCardView {
	status := liveStatusIndexed(a, idx)
	return ui.AgentCardView{
		ID: a.ID, Name: a.Name, DisplayName: a.DisplayName,
		Kind: agents.ResolveKind(a.Kind), CCAccount: a.CCAccount, WorkerPool: a.WorkerPool,
		Repo: a.Repo, Status: status, KickedOff: a.KickedOff, Model: a.Model,
		// 🔴 THE BOOLEAN, NEVER a.PendingNote. agents.KickoffOwed reads the note to
		// answer whether one is outstanding; what crosses into the view layer is its
		// answer. The note is operator-authored text and this repository is public.
		KickoffOwed: agents.KickoffOwed(a),
		// Same rule: the predicate's answer and the SCRUBBED text, never raw
		// kickoff_error — a failed turn's error can quote a runtime body that echoes
		// the note. agents.KickoffFailureText is the one reader both tiers use.
		KickoffFailed:  agents.KickoffFailed(a),
		KickoffFailure: agents.KickoffFailureText(a),
		// Decided from the RAW kickoff_error (its muster-written prefix); only the
		// boolean crosses into the view.
		KickoffResendSafe: agents.KickoffResendSafe(a),
		// Decided from kickoff_error's muster-written prefix too; the boolean crosses.
		KickoffAwaitingGrant: agents.KickoffAwaitingGrant(a),
		GitHubProblem:        s.githubAgentProblem(a),
		Namespace:            a.Namespace,
		NoteID:               a.NoteID, CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt,
		LastActiveAt: agents.LastActivity(a, lastMsg),
		// The ONE chief predicate's answer (chief_agent.go), so the menu never
		// offers a Delete the server would refuse.
		IsChief: isChief(a),
		// The viewer's pin (internal/pins), read once per render by the caller.
		Pinned: pinned.Agent(a.ID),
		// The agent's focus (internal/focus), read once per render by the caller.
		Focus: focused.view(focus.AgentSubject(a.ID)),
		// The polled badge's own answer (in memory), so "Waiting on ask" finds an
		// agent blocked at a permission prompt.
		AwaitingApproval: s.AwaitingApproval(a.ID),
		// The pod's live work counts (agent_work.go): none unless it is running,
		// and expired when stale.
		Work: s.cardWork(a.ID, status),
	}
}

// cardViewFor builds the single-card view for one agent, fetching the live
// instance index itself. Used by the inline-rename re-render and the cancel/card
// re-fetch. Do NOT use it in a loop — see liveStatus.
//
// The chief answer needs the whole list (the chief is resolved across it). A
// failed read degrades to the reserved-slug half of the predicate only — the
// card then may offer a Delete the server still refuses, which is the safe
// direction to be wrong in.
func (s *Server) cardViewFor(ctx context.Context, a agents.Agent) ui.AgentCardView {
	list, err := s.ext.Agents.List(ctx)
	if err != nil {
		s.logger.Printf("agents: card %d: list for chief check: %v", a.ID, err)
		list = nil
	}
	v := s.cardViewIndexed(a, s.instanceIndex(ctx), s.lastMessageByAgents(ctx, []agents.Agent{a})[a.ID], chiefPredicate(list), s.pinSet(ctx), s.focusIndex(ctx))
	// The same badge the list draws, so a re-rendered card keeps it.
	one := []ui.AgentCardView{v}
	s.withCardAttention(ctx, one, nil)
	return one[0]
}

// handleAgentRenameForm serves GET /ui/agents/{id}/rename: the inline rename
// form that REPLACES the card (outerHTML swap of #agent-{id}).
func (s *Server) handleAgentRenameForm(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	a, err := s.ext.Agents.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		s.logger.Printf("agents: rename form %d: %v", id, err)
		http.Error(w, "could not load agent", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := ui.RenderAgentRename(w, ui.AgentCardView{ID: a.ID, Name: a.Name, DisplayName: a.DisplayName}); err != nil {
		s.logger.Printf("agents: render rename form %d: %v", id, err)
	}
}

// handleAgentCard serves GET /ui/agents/{id}/card: re-renders one agent card
// (used to cancel the inline rename), swapped as outerHTML of #agent-{id}.
func (s *Server) handleAgentCard(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	a, err := s.ext.Agents.Get(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		s.logger.Printf("agents: card %d: %v", id, err)
		http.Error(w, "could not load agent", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := ui.RenderAgentCard(w, s.cardViewFor(ctx, a)); err != nil {
		s.logger.Printf("agents: render card %d: %v", id, err)
	}
}

// handleAgentName handles POST /agents/{id}/name: update the agent's display
// name (the slug Name is immutable) and re-render the card (outerHTML swap of
// #agent-{id}). A blank name falls back to the slug so the card stays labelled.
func (s *Server) handleAgentName(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	ctx := r.Context()
	updated, err := s.ext.Agents.SetDisplayName(ctx, id, name)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		s.logger.Printf("agents: set name %d: %v", id, err)
		http.Error(w, "could not update name", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := ui.RenderAgentCard(w, s.cardViewFor(ctx, updated)); err != nil {
		s.logger.Printf("agents: render card after rename %d: %v", id, err)
	}
}

// handleAgentCreate handles POST /agents: build the record, then provision only when
// the action is a dispatch.
//
// 🔴 THIS COMMENT HAS NOW BEEN WRONG IN TWO OPPOSITE DIRECTIONS, WHICH IS WHY IT SAYS
// SO. It read "save = provision at 0 replicas". That was wrong when written — a save
// reached Provisioner.Dispatch(kickoff=false), and provision.Spec.Replicas ZERO MEANS
// ONE, so the instance came up with a replica running — and it is wrong again now in
// the other direction, because the lifecycle adapter creates NOTHING on that path.
// A save provisions nothing at all; Start is what provisions a saved agent. The gate
// check below relies on exactly that, which is why this sentence is load-bearing
// rather than decorative.
func (s *Server) handleAgentCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	action := r.FormValue("action")
	// 🔴 REFUSE A DISPATCH THIS DEPLOYMENT CANNOT SATISFY *HERE*, BEFORE THE ROW,
	// BECAUSE THE PROVISIONER'S OWN REFUSAL CANNOT REACH THE PERSON WHO CLICKED.
	// createAndDispatchAgent calls Dispatch inside safeGo — after the row exists and
	// after this handler has answered — so a refusal there is a log line plus a red
	// dot whose reason reaches no HTML surface. That was measured: with a kickoff
	// undeliverable, POST /agents answered 200, rendered the agents list, and the
	// operator's only evidence was a status colour.
	//
	// ⚠ ONLY action=dispatch IS REFUSED. "Save for later" asks for nothing this
	// deployment cannot do, it is the operation the refusal's own remedy text points
	// at, and refusing it would make that remedy a lie — the same asymmetry
	// agentprovision.Adapter.Dispatch draws one layer down, and for the same reason.
	// Both halves are pinned by TestADispatchIsRefusedWhileASaveAndAStartAreNot.
	//
	// 🔴 THE ANSWER IS THE PROVISIONER'S, NOT A SECOND OPINION ABOUT THE
	// CONFIGURATION. Asking the implementation is what makes this pre-flight unable
	// to disagree with what the dispatch would have done; computing it here from
	// Extensions fields would be two expressions over one condition.
	if action == "dispatch" {
		if reason := s.ext.Provisioner.KickoffUndeliverableReason(); reason != "" {
			// 409 rather than 503: the request conflicts with the deployment's
			// current state and NOTHING about retrying it later is different, which
			// is what 503 would promise. Same status and same shape as the
			// `gate:<reason>` refusal below — internal/ui's htmx:responseError
			// handler toasts the body verbatim, so this text is what the operator
			// reads.
			http.Error(w, reason, http.StatusConflict)
			return
		}
	}
	repo := strings.TrimSpace(r.FormValue("repo"))
	branch := strings.TrimSpace(r.FormValue("repo_branch"))
	model := strings.TrimSpace(r.FormValue("model"))
	noteText := strings.TrimSpace(r.FormValue("note_text"))
	kind := strings.TrimSpace(r.FormValue("kind"))
	ccAccount := strings.TrimSpace(r.FormValue("cc_account"))
	workerPool := strings.TrimSpace(r.FormValue("worker_pool"))

	// Resolve the kickoff note: an existing note's body takes precedence.
	var noteID *int64
	if v := r.FormValue("note_id"); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil && s.ext.Notes != nil {
			if n, err := s.ext.Notes.Get(ctx, id); err == nil {
				// A `gate:<reason>` routing tag means the task is NOT dispatchable yet.
				// The card disables its Dispatch button, but this is the authoritative
				// check (defense in depth — the modal can be opened by other paths).
				// Refuse a real dispatch with 409 + the reason; "save for later" is
				// still allowed (it provisions nothing).
				if reason, gated := notes.GateReason(n.Tags); gated && action == "dispatch" {
					http.Error(w, "task is gated ("+reason+") — remove the gate: tag to dispatch", http.StatusConflict)
					return
				}
				noteText = n.Body
				noteID = &id
			}
		}
	}

	if _, err := s.createAndDispatchAgent(ctx, dispatchParams{
		Repo: repo, RepoBranch: branch, Model: model, NoteID: noteID, NoteText: noteText,
		Kickoff: action == "dispatch",
		Kind:    kind, CCAccount: ccAccount, WorkerPool: workerPool,
	}); err != nil {
		s.logger.Printf("agents: create/dispatch: %v", err)
		// A kind/account refusal is decided BEFORE the row exists and is the
		// operator's to act on — the toast shows this text verbatim.
		if errors.Is(err, ErrDispatchRefused) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		// An uncredentialed model is the OPERATOR's typo, not a server fault, and
		// the message carries the corrected slug — answer 400 with it rather than
		// burying the one useful sentence in a log behind a generic 500.
		if errors.Is(err, agents.ErrModelNotCredentialed) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, "could not create agent", http.StatusInternalServerError)
		return
	}

	// Dispatching FROM a task advances it open→in_progress so its card reflects
	// that an agent is now working it. Only for a real dispatch (not "save"), only
	// when the task is still open (never downgrade a later status), and always
	// best-effort — a status failure must not fail the dispatch the user just made.
	if action == "dispatch" && noteID != nil {
		s.advanceTaskToInProgress(ctx, *noteID)
	}

	// 🔴 THE ANSWER HAS TO FIT THE DOCUMENT THAT ASKED. The default response is
	// the agents list, morphed into #agents-list — an element that exists ONLY on
	// the shell. Dispatching from /tasks/{id} would target nothing, and htmx
	// resolves hx-target BEFORE sending, so the POST was never made at all: no
	// agent, no error, no toast, and a modal that stayed open. The detail page
	// gets its own card back instead, which is where the dispatch's effect is
	// visible anyway (open → in_progress, plus the new agent's status chip).
	if noteID != nil && rendersInDetailShape(r, *noteID) {
		note, err := s.ext.Notes.Get(ctx, *noteID)
		if err == nil {
			s.renderNoteCard(w, r, note)
			return
		}
		// 🔴 DO NOT FALL THROUGH TO THE AGENTS LIST HERE, and the comment that used
		// to sit in this branch called it a merely "wrong target". It is worse than
		// that: #task-{id} DOES exist on this page, so handleAgentsContent's
		// agents-list markup morphs cleanly OVER the task card and the detail page
		// silently shows an unrelated panel where its task was, until something
		// triggers a resync.
		//
		// 204 instead. htmx does not swap a No Content response, so the page keeps
		// the card it already has — which is correct: the dispatch SUCCEEDED, only
		// the re-read failed. A request that worked still answers 2xx; nothing is
		// rendered that is not true.
		//
		// 🔴 AND THE RECOVERY IS NOW REAL. This comment used to end "…and
		// taskDetailLive re-fetches on the next sse:agent.changed /
		// muster:resync anyway", which was NOT established: the agent.changed
		// for this dispatch fires from inside createAndDispatchAgent, i.e. BEFORE
		// the status write above, so there was frequently no next one and the page
		// could sit on `open` until the reader changed tab. advanceTaskToInProgress
		// now broadcasts task.changed AFTER its commit, which taskDetailLive also
		// listens for — so this branch strands nothing, and the re-fetch it
		// triggers reads the advanced status rather than racing it.
		s.logger.Printf("agents: reload task %d after dispatch: %v", *noteID, err)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	s.handleAgentsContent(w, r)
}

// advanceTaskToInProgress flips a task open→in_progress after a dispatch. It
// re-reads the task's current status and only advances it when still open, so a
// task already in a later state (ready_for_review / complete) is never
// downgraded. Best-effort: every failure is logged and swallowed so a status
// hiccup can't fail the dispatch.
func (s *Server) advanceTaskToInProgress(ctx context.Context, noteID int64) {
	if s.ext.Notes == nil {
		return
	}
	n, err := s.ext.Notes.Get(ctx, noteID)
	if err != nil {
		s.logger.Printf("agents: dispatch status get task %d: %v", noteID, err)
		return
	}
	if n.Status != notes.StatusOpen {
		return
	}
	if _, err := s.setNoteStatus(ctx, writerDispatchAdvance, noteID, notes.StatusInProgress); err != nil {
		s.logger.Printf("agents: dispatch status set task %d in_progress: %v", noteID, err)
		return
	}
	// 🔴 BROADCAST AFTER THE COMMIT, AND THIS IS THE ONLY POST-COMMIT EVENT THE
	// DISPATCH EMITS. This used to read "Every other status writer in this package
	// broadcasts; this one did not" — MEASURED FALSE (upstream task 487 item 1):
	// the HUMAN route (PATCH /tasks/{id}/status) broadcast nothing either, because
	// its htmx response swaps the card and that was mistaken for reaching every
	// client. It reaches only the browser that asked. Both are fixed now — the
	// human/machine pair broadcasts from applyTaskStatus, this one from here —
	// but do not restate "every other writer broadcasts" as a premise: the writers
	// that call setNoteStatus directly still each broadcast from their own
	// handler, so the property holds by repetition, not by construction.
	//
	// Before the fix the ONLY nudge a dispatch produced was the
	// agent.changed fired from inside createAndDispatchAgent — which happens
	// BEFORE this write. Measured: the re-fetch that event triggers is issued at
	// +233ms and reads the task still `open`, 41ms before this commit lands.
	//
	// Two things follow. Other open tabs and the board learned of the advance
	// only by accident (an agent status tick, or a resync). And the 204 branch in
	// handleAgentCreate promised "taskDetailLive re-fetches on the next
	// sse:agent.changed / muster:resync anyway" — a promise nothing kept, since
	// the agent.changed for THIS dispatch had already fired. This broadcast is
	// what makes it true: it is emitted after the status is committed, so the
	// re-fetch it triggers reads in_progress, and the stale-card guard in
	// resyncScript keeps the earlier, older read from landing on top of it.
	s.broadcast(ctx, EventTaskChanged, strconv.FormatInt(noteID, 10))
}

// dispatchParams are the inputs to create + provision an agent.
type dispatchParams struct {
	// Name is a fixed slug (today: only agents.ChiefName, from handleChiefProvision);
	// empty → auto-generated by BuildUniqueAgentName. ⚠ It read "e.g. the operator"
	// until upstream task #653 phase two deleted handleOperatorProvision, which was
	// the other caller.
	Name string
	// DisplayName, when set, is used verbatim as the agent's human-facing name
	// (an explicit override). When empty and NoteID is set, createAndDispatchAgent
	// derives a task-labelled name ("#<id> · <title>"); otherwise it falls back to
	// the slug Name.
	DisplayName string
	Repo        string
	RepoBranch  string // explicit branch; empty → resolved to the repo's GitHub default (fallback "main")
	Model       string
	NoteText    string
	NoteID      *int64
	Kickoff     bool
	// Kind is the agent kind to create ("" = agents.KindGateway). It must be one
	// Extensions.Kinds enables.
	Kind string
	// CCAccount PINS a claude-code agent's Claude account; "" lets the pool
	// choose. Refused for any other kind.
	CCAccount string
	// WorkerPool makes a claude-code agent a member of that machine-job worker
	// pool (agents.Agent.WorkerPool); "" for none. Refused for any other kind
	// and for a name agents.ValidWorkerPool refuses.
	WorkerPool string
}

// claudeCodeNoModel is the one sentence both model writes refuse with.
const claudeCodeNoModel = "a claude-code agent has no per-agent model (the CLI's own settings choose it); leave the model unset"

// hermesModelRefusal applies agentspec.HermesModelID — the rule a hermes pod's
// spec is built by — to a model a dispatch or a model change names for a
// hermes agent, so a model the pod cannot run is refused before anything is
// created or stored. An empty model (the deployment's default) is left to the
// build. Any other kind: nil.
func hermesModelRefusal(kind, model string) error {
	if agents.ResolveKind(kind) != agents.KindHermes || model == "" {
		return nil
	}
	if _, err := agentspec.HermesModelID(model); err != nil {
		return fmt.Errorf("%w: a %s agent: %v", ErrDispatchRefused, agents.KindHermes, err)
	}
	return nil
}

// ErrDispatchRefused marks a dispatch refused before anything was created: a
// kind this deployment does not enable, an account pin for a kind that has none,
// or a claude-code pool with no usable account.
var ErrDispatchRefused = errors.New("dispatch refused")

// resolveKind is resolveKindAccount's kind half: the kind a dispatch names,
// resolved, known, enabled on this deployment and usable by the caller — or the
// refusal. The chief's task_dispatch asks it BEFORE it spends a reservation, so
// a kind it can never start is refused without one; createAndDispatchAgent asks
// it again, through resolveKindAccount.
func (s *Server) resolveKind(ctx context.Context, want string) (string, error) {
	kind := agents.ResolveKind(strings.ToLower(want))
	if !agents.ValidKind(kind) {
		return "", fmt.Errorf("%w: unknown agent kind %q", ErrDispatchRefused, want)
	}
	if !s.ext.kindEnabled(kind) {
		return "", fmt.Errorf("%w: agent kind %q is not enabled on this deployment (MUSTER_AGENT_KINDS)", ErrDispatchRefused, kind)
	}
	// 🔴 A KIND THE CALLER HAS NO CREDENTIAL FOR IS REFUSED HERE, BEFORE THE ROW,
	// with the sentence that says what to add — never dispatched onto the
	// admin's credentials. (The pod-spec path refuses it again from the owner's
	// own credentials: internal/agentprovision.specFor.)
	if s.ext.Kinds != nil {
		if why := s.ext.Kinds.Unavailable(ctx, callerID(ctx))[kind]; why != "" {
			return "", fmt.Errorf("%w: %s", ErrDispatchRefused, why)
		}
	}
	return kind, nil
}

// resolveKindAccount decides a new agent's kind and Claude account, BEFORE its
// row exists.
//
// 🔴 THE ACCOUNT IS CHOSEN HERE, ONCE, AND STORED ON THE ROW — the agent keeps
// it for life (migration 0003 refuses a change). Every later build of this
// agent's spec reads the stored name; nothing re-selects.
func (s *Server) resolveKindAccount(ctx context.Context, p dispatchParams) (kind, account string, err error) {
	kind, err = s.resolveKind(ctx, p.Kind)
	if err != nil {
		return "", "", err
	}
	if kind != agents.KindClaudeCode {
		if p.CCAccount != "" {
			return "", "", fmt.Errorf("%w: a Claude account can only be pinned for a %s agent", ErrDispatchRefused, agents.KindClaudeCode)
		}
		if err := hermesModelRefusal(kind, p.Model); err != nil {
			return "", "", err
		}
		return kind, "", nil
	}
	if p.Model != "" {
		return "", "", fmt.Errorf("%w: %s", ErrDispatchRefused, claudeCodeNoModel)
	}
	// A repository IS taken (unlike a model): ccd checks it out before it starts
	// the CLI and runs the CLI in the checkout (agentspec.podRepo,
	// cmd/ccd/checkout.go).
	if s.ext.Kinds == nil {
		return "", "", fmt.Errorf("%w: no Claude account pool is configured", ErrDispatchRefused)
	}
	account, err = s.ext.Kinds.SelectClaudeAccount(ctx, callerID(ctx), p.CCAccount)
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrDispatchRefused, err)
	}
	return kind, account, nil
}

// resolveRepoBranch decides which branch a repo-backed agent clones. An explicit
// branch wins; otherwise it looks up the repo's GitHub default branch (so a repo
// whose default isn't "main" — e.g. "trunk" — clones correctly), falling back to
// "main" when there's no repo info, no GitHub connection, or the lookup fails.
// Returns "" for a repo-less dispatch.
func (s *Server) resolveRepoBranch(ctx context.Context, repo, branch string) string {
	if branch != "" {
		return branch
	}
	if repo == "" {
		return ""
	}
	if b, err := s.githubDefaultBranch(ctx, repo); err == nil && b != "" {
		return b
	}
	return "main"
}

// dispatchDisplayName derives a task-labelled agent display name from a source
// task: "#<id> · <title>", where the title is the task body's first non-empty
// line. Falls back to just "#<id>" when the body has no usable text.
//
// 🔴 STORED UNCUT, TO A STORAGE BOUND ONLY. It used to be cut to 40 runes here,
// which landed mid-markdown — "Fix the [login page](https://example.com…" —
// and a cut link can never be plained at render. The title rule
// (internal/textplain) runs on the whole line at render, and the card clamps
// the PLAIN text (clampTitle). dispatchTitleMaxRunes only keeps a runaway
// first line out of the row.
func dispatchDisplayName(noteID int64, noteText string) string {
	id := strconv.FormatInt(noteID, 10)
	title := truncate(strings.TrimSpace(firstNonEmptyLine(noteText)), dispatchTitleMaxRunes)
	if title == "" {
		return "#" + id
	}
	return "#" + id + " · " + title
}

// dispatchTitleMaxRunes bounds the stored task title in a dispatched agent's
// display name: generous, so a markdown link survives whole for the renderer.
const dispatchTitleMaxRunes = 300

// createAndDispatchAgent inserts an agent record and provisions it in the
// background (kickoff sends the note as the first message). Shared by the human
// dispatch form and the chief provision.
func (s *Server) createAndDispatchAgent(ctx context.Context, p dispatchParams) (agents.Agent, error) {
	kind, account, err := s.resolveKindAccount(ctx, p)
	if err != nil {
		return agents.Agent{}, err
	}
	// A worker pool is machine jobs', which run on claude-code agents only
	// (migration 0041's agents_worker_pool_check refuses the row as well).
	if p.WorkerPool != "" {
		if kind != agents.KindClaudeCode {
			return agents.Agent{}, fmt.Errorf("%w: worker_pool %q: only a %s agent can be a machine-job worker", ErrDispatchRefused,
				p.WorkerPool, agents.KindClaudeCode)
		}
		if !agents.ValidWorkerPool(p.WorkerPool) {
			return agents.Agent{}, fmt.Errorf("%w: worker_pool %q is not 1-63 of [a-z0-9-], not starting with '-'", ErrDispatchRefused, p.WorkerPool)
		}
	}
	// 🔴 A CLAUDE-CODE AGENT'S REPOSITORY MUST BE A CONNECTED ONE, from every
	// door (the forms, the API, task_dispatch): its session starts PRE-TRUSTED
	// in the checkout (ccd seed), so the repository's .claude/settings.json
	// (hooks, env) runs beside the subscription token. One predicate,
	// connectedRepo, the same task_dispatch applies to every kind before it
	// reserves. Before the row, so a refusal creates nothing.
	// The same holds for a hermes agent: hermesd checks the repository out and
	// Hermes runs in it, loading its AGENTS.md (agents.RunsPodSupervisor).
	if agents.RunsPodSupervisor(kind) && p.Repo != "" {
		if why := s.connectedRepo(ctx, p.Repo); why != "" {
			return agents.Agent{}, fmt.Errorf("%w: %s", ErrDispatchRefused, why)
		}
	}
	// 🔴 REFUSE A MODEL THE POD WILL HAVE NO CREDENTIAL FOR, BEFORE the record
	// exists. This is the FIRST of the two places an operator can set an agent's
	// model (the other is handleAgentModel); both call the same predicate so the
	// rule cannot be right in one and absent in the other. See
	// agents/modelauth.go for the measured failure this prevents — an agent that
	// runs forever on its FALLBACK model while reporting nothing wrong.
	if err := agents.ValidateModelCredential(p.Model); err != nil {
		return agents.Agent{}, err
	}
	name := p.Name
	if name == "" {
		n, err := agents.BuildUniqueAgentName(ctx, s.ext.Agents)
		if err != nil {
			return agents.Agent{}, err
		}
		name = n
	}
	// Name the agent after its source task when dispatched from one, so the card
	// reads "#42 · fix the login bug" rather than the opaque slug. The k8s-safe
	// slug Name is left untouched (the pod/namespace key stays stable). An explicit
	// p.DisplayName wins; a task-less dispatch keeps the slug as the display name.
	displayName := name
	if p.DisplayName != "" {
		displayName = p.DisplayName
	} else if p.NoteID != nil {
		displayName = dispatchDisplayName(*p.NoteID, p.NoteText)
	}
	// 🔴 Namespace BELOW IS THE DEPLOYMENT'S PREFIX, NOT THIS PACKAGE'S CONSTANT.
	// It read agents.NamespaceFor(name) — the package default — while the driver
	// created `<MUSTER_AGENT_NAMESPACE_PREFIX><name>`, which shipped rows naming
	// namespaces that hold nothing and read as "never provisioned". Nothing places
	// anything with this value, so the disagreement failed nowhere. See
	// Extensions.AgentNamespacePrefix and Extensions.AgentNamespace.
	rec, err := s.ext.Agents.Create(ctx, agents.Agent{
		Name:        name,
		Namespace:   s.ext.AgentNamespace(name),
		DisplayName: displayName,
		Repo:        p.Repo,
		RepoBranch:  s.resolveRepoBranch(ctx, p.Repo, p.RepoBranch),
		Model:       p.Model,
		NoteID:      p.NoteID,
		NoteText:    p.NoteText,
		PendingNote: p.NoteText,
		Status:      agents.StatusProvisioning,
		Kind:        kind,
		CCAccount:   account,
		WorkerPool:  p.WorkerPool,
	})
	if err != nil {
		return agents.Agent{}, err
	}
	// The agent was just created (provisioning): nudge open Tasks/Agents tabs so a
	// task's linked-agent chip + the agents list appear live (best-effort).
	s.agentChangedFor(requestOwner(ctx), rec.Name)
	// WithoutCancel: the dispatch outlives this request, but it must keep the
	// request's principal — row-level security scopes every store call the
	// adapter makes to it (see Provisioner).
	opCtx := context.WithoutCancel(ctx)
	safeGo(s.logger, "dispatch "+rec.Name, func() {
		if err := s.ext.Provisioner.Dispatch(opCtx, rec.ID, p.Kickoff); err != nil {
			s.logger.Printf("agents: dispatch %s: %v", rec.Name, err)
			return
		}
	})
	return rec, nil
}

func (s *Server) handleAgentStart(w http.ResponseWriter, r *http.Request) {
	// 🔴 A START IS REFUSED, LIKE A DISPATCH, FOR A KIND THE CALLER HAS NO
	// CREDENTIAL FOR — 409 with what to add. Start re-applies the agent's spec
	// through its owner's credentials (agentprovision.Adapter.Start), so a
	// member who deleted their key would only get a background failure; this
	// says why at the door. The row is read as the caller (row-level security),
	// so another user's agent is not found here and falls through to the
	// lifecycle path exactly as before.
	if s.ext.Kinds != nil && s.ext.Agents != nil {
		if id, err := strconv.ParseInt(r.PathValue("id"), 10, 64); err == nil {
			if a, err := s.ext.Agents.Get(r.Context(), id); err == nil {
				if why := s.ext.Kinds.Unavailable(r.Context(), callerID(r.Context()))[agents.ResolveKind(a.Kind)]; why != "" {
					http.Error(w, fmt.Sprintf("%v: %s", ErrDispatchRefused, why), http.StatusConflict)
					return
				}
			}
		}
	}
	s.agentLifecycle(w, r, func(ctx context.Context, id int64) error { return s.ext.Provisioner.Start(ctx, id) }, "start")
}

func (s *Server) handleAgentStop(w http.ResponseWriter, r *http.Request) {
	// A stopped pod cannot post its own clear: forget its work report now.
	if id, err := strconv.ParseInt(r.PathValue("id"), 10, 64); err == nil {
		s.clearAgentWork(id)
	}
	s.agentLifecycle(w, r, func(ctx context.Context, id int64) error { return s.ext.Provisioner.Stop(ctx, id) }, "stop")
}

// agentLifecycle runs a start/stop action in the background then re-renders the
// agents panel.
func (s *Server) agentLifecycle(w http.ResponseWriter, r *http.Request, action func(context.Context, int64) error, label string) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	// Detached from the request's cancellation, NOT from its principal.
	opCtx := context.WithoutCancel(r.Context())
	safeGo(s.logger, label, func() {
		if err := action(opCtx, id); err != nil {
			s.logger.Printf("agents: %s %d: %v", label, id, err)
		}
	})
	s.handleAgentsContent(w, r)
}

// handleAgentModel handles POST /agents/{id}/model: update the agent's model in
// the store, then (if it has a live release) re-provision in the background so
// the pod rolls with the new model. An empty model string means "use the cluster
// default" and is stored as "". Responds 200 with HX-Trigger so the UI refreshes;
// a stopped agent simply picks up the new model at its next dispatch.
func (s *Server) handleAgentModel(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	model := strings.TrimSpace(r.FormValue("model"))
	// The SECOND of the two operator-facing model writes (createAndDispatchAgent
	// is the first). Same predicate, deliberately — this path re-provisions a
	// RUNNING agent, so an uncredentialed slug here silently degrades a live
	// agent onto its fallback model rather than merely mis-creating a new one.
	if err := agents.ValidateModelCredential(model); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	// 🔴 THE SAME RULE createAndDispatchAgent APPLIES (resolveKindAccount): a
	// claude-code agent has no per-agent model — its spec ignores one, so storing
	// and showing it would be a setting that does nothing. Refused, not stored.
	// ⚠ FAIL CLOSED: a read error other than "no such agent" refuses the write
	// rather than storing a model on an agent whose kind could not be checked.
	// A missing agent falls through to SetModel's own 404, as before.
	cur, gerr := s.ext.Agents.Get(ctx, id)
	if gerr != nil && !errors.Is(gerr, pgx.ErrNoRows) {
		s.logger.Printf("agents: set model %d: read kind: %v", id, gerr)
		http.Error(w, "could not update model", http.StatusInternalServerError)
		return
	}
	if gerr == nil {
		if err := hermesModelRefusal(cur.Kind, model); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
	}
	if gerr == nil && agents.ResolveKind(cur.Kind) == agents.KindClaudeCode {
		http.Error(w, ErrDispatchRefused.Error()+": "+claudeCodeNoModel, http.StatusConflict)
		return
	}
	updated, err := s.ext.Agents.SetModel(ctx, id, model)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		s.logger.Printf("agents: set model %d: %v", id, err)
		http.Error(w, "could not update model", http.StatusInternalServerError)
		return
	}

	// Roll the pod only when the agent has a live release (running/provisioning);
	// a stopped agent re-renders its model at the next Dispatch. ReapplyProfiles
	// re-renders ALL helm values — including the freshly-stored a.Model — and
	// upgrades the release in place (a no-op when no release is installed yet).
	if updated.Status == agents.StatusRunning || updated.Status == agents.StatusProvisioning {
		if rp, ok := s.ext.Provisioner.(ProfileReapplier); ok {
			// WithoutCancel, not Background: the roll outlives the request but
			// must keep its principal (row-level security, accounts PR 3a).
			opCtx := context.WithoutCancel(r.Context())
			safeGo(s.logger, "reapply model", func() {
				if err := rp.ReapplyProfiles(opCtx, updated.ID); err != nil {
					s.logger.Printf("agents: reapply model for %d: %v", updated.ID, err)
				}
			})
		}
	}

	w.Header().Set("HX-Trigger", "agents:changed")
	w.WriteHeader(http.StatusOK)
}

// handleAgentDelete tears down the agent (release + namespace + record) in the
// background and removes its card.
//
// 🔴 THE CHIEF IS REFUSED WITH 409, BEFORE ANYTHING IS STARTED. The teardown is
// asynchronous, so a check inside it could only log; the refusal has to happen
// here, while there is still a response to put it in. A failed read of the
// agent table refuses too (500) — see agentIsChief on failing closed.
func (s *Server) handleAgentDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	var chief errDestroyChief
	// The provisioner is called HERE, in this route's own closure: the route is
	// behind requireLifecycleProvisioner, which is its nil barrier.
	destroy := func(ctx context.Context, id int64) error { return s.ext.Provisioner.Destroy(ctx, id) }
	switch _, err := s.destroyAgent(r.Context(), id, destroy, destroyHooks{}); {
	case err == nil:
	case errors.As(err, &chief):
		http.Error(w, chief.Error(), http.StatusConflict)
		return
	default:
		s.logger.Printf("agents: delete %d: %v", id, err)
		http.Error(w, "could not check whether this is the chief agent; nothing was deleted", http.StatusInternalServerError)
		return
	}
	// Empty 200: htmx removes the targeted #agent-<id> card.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
}

// errDestroyChief is destroyAgent's refusal of the chief; its text is
// chiefRefusal's.
type errDestroyChief struct{ a agents.Agent }

func (e errDestroyChief) Error() string { return chiefRefusal(e.a) }

// destroyHooks are what a destroyAgent caller adds after the shared teardown.
type destroyHooks struct {
	// done runs in the teardown's goroutine after a SUCCESSFUL Destroy and the
	// shared after-steps, on the same principal-keeping context. nil: nothing
	// more.
	done func(ctx context.Context, a agents.Agent)
}

// destroyAgent is THE agent teardown both doors take — the browser's DELETE
// /agents/{id} (handleAgentDelete) and a mirrored session's agent_destroy
// (agent_destroy_tool.go): the chief check, then the provisioner's Destroy in
// the background (instance, then the row; see agentprovision.Adapter.Destroy),
// exactly as the browser always did. It returns the agent row the chief check
// read (zero when the id names no agent the caller can see — the browser's
// path still starts the teardown then, as it always has, and Destroy's own
// row read fails and is logged).
//
// destroy is the provisioner's Destroy, passed by each door rather than called
// here, so that each door's own body is where the provisioner is reached and
// nil-checked (provisioner_seam_test.go's walk is function-granular: the
// browser's route is wrapped in requireLifecycleProvisioner, the MCP tool checks
// in its body). TestEveryDestroyCallSiteIsChiefGuarded accepts a door that
// routes through this function as chief-guarded, and pins that this one asks
// agentIsChief.
//
// 🔴 THE CHIEF IS REFUSED HERE, BEFORE ANYTHING IS STARTED (errDestroyChief),
// and a failed read of the agent table refuses too (agentIsChief fails closed).
// The teardown is asynchronous, so a check inside it could only log.
func (s *Server) destroyAgent(ctx context.Context, id int64, destroy func(context.Context, int64) error, h destroyHooks) (agents.Agent, error) {
	a, chief, err := s.agentIsChief(ctx, id)
	if err != nil {
		return agents.Agent{}, fmt.Errorf("chief check: %w", err)
	}
	if chief {
		return a, errDestroyChief{a}
	}
	// A destroyed pod cannot post its own clear: forget its work report now.
	s.clearAgentWork(id)
	opCtx := context.WithoutCancel(ctx) // keeps the principal; see Provisioner
	safeGo(s.logger, "destroy", func() {
		if err := destroy(opCtx, id); err != nil {
			s.logger.Printf("agents: destroy %d: %v", id, err)
			return
		}
		// After a SUCCESSFUL teardown, on both doors: the owner's destroy frees
		// the chief's one start on the agent's task when the chief's start made
		// this agent (operator decision), and the owner's open tabs re-fetch —
		// the Agents views (agent.changed) and the attention queue, which lost
		// the agent's asks with its row (attention_queue.changed).
		s.releaseChiefStart(opCtx, a)
		if a.ID != 0 {
			s.broadcastTo(a.OwnerID, EventAgentChanged, a.Name)
			s.attentionChanged(a.OwnerID, "agent-destroyed-"+strconv.FormatInt(a.ID, 10))
		}
		if h.done != nil {
			h.done(opCtx, a)
		}
	})
	return a, nil
}

// handleAgentDetail renders the full detail page (logs + chat).
func (s *Server) handleAgentDetail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a, err := s.ext.Agents.GetByName(ctx, r.PathValue("name"))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		s.logger.Printf("agents: get %q: %v", r.PathValue("name"), err)
		http.Error(w, "could not load agent", http.StatusInternalServerError)
		return
	}
	status := s.liveStatus(ctx, a)
	sessionParam := r.URL.Query().Get("session")
	// ⚠ THE LIST-READ FAILURE IS DISCARDED HERE, AND THAT IS THE PAGE'S EXISTING
	// BEHAVIOUR RATHER THAN AN OVERSIGHT: sessionDrawer renders no failure state, so
	// there is nothing to hand it. resolveSessions logs the read for every caller.
	active, sessions, _ := s.resolveSessions(ctx, a, sessionParam)
	// On a bare open (no ?session=) put the resolved session in the URL so the
	// page deep-links + the WS/mark-read paths agree on the session. Only redirect
	// when the param is absent (no redirect loop once it's present).
	if sessionParam == "" && active.ID != 0 {
		http.Redirect(w, r, "/agents/"+a.Name+"?session="+strconv.FormatInt(active.ID, 10), http.StatusSeeOther)
		return
	}
	// The viewer opened this agent: its "recently viewed" entry (best-effort).
	s.recordAgentVisit(ctx, a.ID)
	// Opening a session clears its unread replies (best-effort).
	if err := s.ext.Agents.MarkSessionRead(ctx, active.ID); err != nil {
		s.logger.Printf("agents: mark session %d read on detail: %v", active.ID, err)
	}
	msgs, _ := s.ext.Agents.ListRecentChatMessages(ctx, active.ID, chatHistoryCap)
	lines := make([]ui.ChatLine, 0, len(msgs))
	for _, m := range msgs {
		lines = append(lines, ui.ChatLine{Role: m.Role, Content: m.Content, Kind: m.Kind, ToolID: m.ToolID, ToolName: m.ToolName, ToolOK: m.ToolOK, ID: m.ID})
	}
	noteID, taskStatus := s.agentTaskStatus(ctx, a)
	view := ui.AgentDetailView{
		ID: a.ID, Name: a.Name, DisplayName: a.DisplayName, Status: status, Repo: a.Repo, Model: a.Model,
		Kind: agents.ResolveKind(a.Kind), CCAccount: a.CCAccount, WorkerPool: a.WorkerPool,
		Messages: lines, Sessions: sessions, ActiveSessionID: active.ID,
		NoteID: noteID, TaskStatus: taskStatus, DraftUser: draftUser(ctx),
		// The chat form's fallback when its socket cannot carry a message
		// (agent_send.go): its nonce, and the outcome of such a send.
		SendNonce: newSendNonce(),
		// The chat form's microphone (transcribe.go); "" when there is none.
		Dictate: s.dictatePath(),
		// The agent's focus history (focus_history.go); "" without a store.
		FocusHistory: s.focusHistoryPath(focus.AgentSubject(a.ID)),
	}
	view.SendNotice, view.SendNoticeOK = composerNotice(r)
	// A claude-code agent's chat is the Rendered half of its Rendered |
	// Terminal switch, which its terminal opens in a frame (ui/view_swap.go).
	if agents.ResolveKind(view.Kind) == agents.KindClaudeCode {
		allowSameOriginFrame(w)
	}
	formDocument(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := ui.RenderAgentDetail(w, view, s.shellFeatures(r.Context())); err != nil {
		s.logger.Printf("agents: render detail: %v", err)
	}
}

// agentTaskStatus resolves the source-task id + current lifecycle status for the
// completion banner (Task 6). Best-effort: a task-less agent (NoteID nil), a
// missing Notes store, or a failed lookup all yield an empty status (→ no banner).
// The task status is re-read on every render — including the chat-log refresh
// driven by the chat-reply and task-changed SSE events — so a just-finished task
// reveals its banner live.
func (s *Server) agentTaskStatus(ctx context.Context, a agents.Agent) (*int64, string) {
	if a.NoteID == nil || s.ext.Notes == nil {
		return a.NoteID, ""
	}
	n, err := s.ext.Notes.Get(ctx, *a.NoteID)
	if err != nil {
		return a.NoteID, ""
	}
	return a.NoteID, n.Status
}

// handleAgentChatLog serves GET /ui/agents/{name}/chat-log: the INNER content of
// #chat-log (transcript bubbles + provisioning/working indicators) for the active
// session. The detail page re-fetches it on sse:chat.reply so a kickoff turn (which
// persists server-side, off this page's WS) shows live. Status is computed the same
// way as handleAgentDetail so the provisioning indicator self-clears once running.
func (s *Server) handleAgentChatLog(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a, err := s.ext.Agents.GetByName(ctx, r.PathValue("name"))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		s.logger.Printf("agents: chat-log get %q: %v", r.PathValue("name"), err)
		http.Error(w, "could not load agent", http.StatusInternalServerError)
		return
	}
	status := s.liveStatus(ctx, a)
	active, _ := s.resolveSession(ctx, a, r.URL.Query().Get("session"))
	msgs, _ := s.ext.Agents.ListRecentChatMessages(ctx, active.ID, chatHistoryCap)
	lines := make([]ui.ChatLine, 0, len(msgs))
	for _, m := range msgs {
		lines = append(lines, ui.ChatLine{Role: m.Role, Content: m.Content, Kind: m.Kind, ToolID: m.ToolID, ToolName: m.ToolName, ToolOK: m.ToolOK, ID: m.ID})
	}
	noteID, taskStatus := s.agentTaskStatus(ctx, a)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := ui.RenderAgentChatLog(w, ui.AgentDetailView{
		Name: a.Name, Status: status, Messages: lines, ActiveSessionID: active.ID,
		NoteID: noteID, TaskStatus: taskStatus,
		// The surface the refetch is FOR. #chat-log inside the chief panel asks with
		// ?surface=chief-panel, and that is what decides whether the zero-message
		// intro is part of this partial — which is what makes the intro disappear the
		// moment the thread has content, on the server's own count. An unknown or
		// absent value is the page surface, i.e. every existing caller is unchanged.
		Surface: chatSurfaceFromRequest(r),
	}); err != nil {
		s.logger.Printf("agents: render chat-log: %v", err)
	}
}

// resolveSessions picks the active chat session for a request and builds the
// session-switcher view list (most-recently-active first). The active session is
// the ?session=<id> one when it's valid AND belongs to this agent; otherwise the
// agent's latest session (created if it has none). Shared by the agent-detail and
// operator pages and by the chief panel, so all three get the same switcher
// behaviour.
//
// 🔴 THE THIRD RETURN VALUE IS THE SESSION-LIST READ'S OWN FAILURE, AND IT EXISTS
// BECAUSE DISCARDING IT SHIPPED A LIE. `list, _ := ListSessions(...)` left every
// caller holding an EMPTY slice on a failed read, with no way to tell that from an
// agent that genuinely has no threads — so the chief panel's first paint rendered
// "No threads yet." when one database read had failed, i.e. told the operator their
// entire thread history was gone. The rows and this error are one answer, so they
// are returned together; a renderer that draws the rows must be able to say which
// of the two it got.
//
// ⚠ THE TWO PAGE SURFACES DISCARD IT ON PURPOSE AND THEIR BEHAVIOUR IS UNCHANGED.
// sessionDrawer has no failure state to render and giving it one is a separate
// change to two surfaces with their own tests; the diagnostic they used to lack is
// the log line below, which every caller now gets from the one place the read
// happens. Nothing about `active` or `views` moved.
//
// 🔴 SO SAY WHAT THOSE TWO SURFACES STILL DO, AT FULL SCOPE — a narrower reading of
// the paragraph above is how this gets closed as already-handled. They draw a FAILED
// read as an agent with no threads, AND they draw a TRUNCATED read — the `out,
// rows.Err()` shape, rows beside an error — as a COMPLETE list: sessionDrawer gets the
// rows and never learns the read broke, so a partial thread history is presented on
// those pages as the whole of it, which is the same lie the chief panel was fixed for.
// Both are DECLARED and out of scope here, not unknown; the "UNCHANGED" cases of
// api.TestAFailedThreadListReadIsNEVERRenderedAsAnEmptyHistory pin their behaviour so
// a fix for the panel cannot silently reach them.
func (s *Server) resolveSessions(ctx context.Context, a agents.Agent, sessionParam string) (agents.ChatSession, []ui.SessionView, error) {
	var active agents.ChatSession
	if id, err := strconv.ParseInt(sessionParam, 10, 64); err == nil && id > 0 {
		if sess, err := s.ext.Agents.GetSession(ctx, id); err == nil && sess.AgentID == a.ID {
			active = sess
		}
	}
	if active.ID == 0 {
		active, _ = s.ext.Agents.LatestOrCreateSession(ctx, a.ID, a.Name)
	}
	list, err := s.ext.Agents.ListSessions(ctx, a.ID)
	if err != nil {
		// Logged HERE — the single place the read is issued — so the page surfaces that
		// discard the error still leave a trace, and no caller has to remember to.
		s.logger.Printf("agents: list sessions for %d: %v", a.ID, err)
	}
	return active, s.sessionViews(ctx, a, list, active.ID), err
}

// handleAgentSessionCreate handles POST /agents/{id}/sessions: open a chat
// session for the agent and redirect (htmx) to the detail page focused on it.
//
// To avoid spawning duplicate empty sessions when "New chat" is double-tapped on
// a blank session, it first checks the agent's latest session: if that one is
// empty (no messages) it REUSES it; only when the latest session already has
// messages does it create a fresh one.
func (s *Server) handleAgentSessionCreate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	a, err := s.ext.Agents.Get(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		s.logger.Printf("agents: session create lookup %d: %v", id, err)
		http.Error(w, "could not load agent", http.StatusInternalServerError)
		return
	}
	// 🔴 THE PANEL BRANCH IS CHIEF-SCOPED, AND THE SUBJECT IS RESOLVED *BEFORE* THE
	// WRITE. chiefNewThreadButton bakes the chief's row id into its hx-post when the
	// panel BODY is rendered, and the chief is resolved by mutable DISPLAY NAME with a
	// lowest-id tie-break — so the id in this path is a snapshot that a SetDisplayName
	// (or a re-provision) invalidates while the panel sits open. Taking it on trust
	// created the thread on the OLD chief and then rendered the NEW chief's panel
	// (writeChiefPanelBody resolves the chief itself and ignored `a` entirely):
	// resolveSessions rejects the just-created id because its AgentID does not match,
	// so the operator silently got somebody else's latest thread and a stray empty
	// chat_sessions row was left behind, with nothing anywhere saying so.
	//
	// 🔴 SO THE MISMATCH RETARGETS THE WRITE RATHER THAN BEING REFUSED, AND THE ORDER
	// IS THE FIX. Refusing at the old branch point was too late — the session had
	// already been created — and refusing at all leaves the operator's tap doing
	// nothing on a control that looks live. `?surface=chief-panel` says "this is the
	// chief slide-out's New thread", the panel is the chief's by construction (its
	// search route, its thread rows and its transcript are all chief-scoped), so the
	// authoritative subject is the chief resolved NOW, not the id a stale button
	// carries. Both harms close together: nothing is written to the wrong agent, and
	// the body this answers with re-renders the launcher slot with the CURRENT id, so
	// the button heals itself.
	//
	// ⚠ AND A CHIEF THAT CANNOT BE RESOLVED WRITES NOTHING *HERE*. writeChiefPanelBody
	// renders the honest statement for that case — the configuration one for ErrNoChief,
	// "could not read the agent list" for a failed read — and creating a thread on
	// whatever `{id}` named would be the same wrong-agent write by another route.
	//
	// ⚠ "NOTHING AT ALL" WOULD BE TOO STRONG, AND THE DIFFERENCE IS A TRANSIENT ERROR.
	// This branch treats every chiefAgent failure as "do not write here", but it then
	// delegates to a renderer that resolves the chief AGAIN — and resolveSessions ends
	// in LatestOrCreateSession. So a read that fails here and SUCCEEDS there does create
	// a session: on the chief that second read named, which is the agent the operator is
	// then looking at, so it is benign rather than the wrong-agent write above. Closing
	// even that would mean a panel-body renderer that cannot create a session, i.e. a
	// second renderer of this partial — which is the multiplication
	// writeChiefPanelBodyFor's header exists to prevent. Left as is, deliberately.
	if chatSurfaceFromRequest(r) == ui.ChatSurfaceChiefPanel {
		chief, cerr := s.chiefAgent(ctx)
		if cerr != nil {
			// 🔴 ErrNoChief AND A BROKEN READ ARE NOT THE SAME EVENT, AND ONE LINE FOR BOTH
			// MISLABELS AN OUTAGE AS A CONFIGURATION STATE. "no chief" sends whoever reads
			// this log to check display names; a failed agents-table read is a database
			// problem and clears on its own.
			if errors.Is(cerr, ErrNoChief) {
				s.logger.Printf("agents: panel new thread on %d: no agent carries the chief display name", id)
			} else {
				s.logger.Printf("agents: panel new thread on %d: could not resolve the chief: %v", id, cerr)
			}
			s.writeChiefPanelBody(ctx, w, "")
			return
		}
		if chief.ID != a.ID {
			s.logger.Printf("agents: panel new thread asked for agent %d but the chief is now %d "+
				"(%s); the thread goes to the chief and NOT to the stale id", a.ID, chief.ID, chief.Name)
			a = chief
		}
	}

	// Reuse the latest session if it's empty; otherwise open a fresh one.
	// 🔴 THE TWO LOGS BELOW NAME `a.ID`, NOT THE PATH's `id`, AND THAT MATTERS ON
	// EXACTLY THE INCIDENT THE BRANCH ABOVE EXISTS FOR. The panel branch RETARGETS the
	// write when the chief has moved, so `id` is the stale button's snapshot while the
	// write goes to `a.ID`. Logging `id` attributed a failure to the agent that was
	// never touched — and the retarget already logged both ids, so `a.ID` alone is
	// unambiguous here.
	//
	// 🔴 A SESSION ASKED FOR ON A RUNTIME IS ALWAYS NEW (session_runtime.go): the
	// empty latest one may be on another runtime or model, and both are fixed for
	// a session's life, so reusing it would answer the picker with a session that
	// is not what was picked.
	if rt := r.FormValue("runtime"); rt != "" {
		sess, err := s.createRuntimeSession(ctx, a, rt, r.FormValue("model"))
		if err != nil {
			s.logger.Printf("agents: create %s session for %d: %v", rt, a.ID, err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.answerSessionOpened(ctx, w, r, a, sess)
		return
	}
	latest, err := s.ext.Agents.LatestOrCreateSession(ctx, a.ID, a.Name)
	if err != nil {
		s.logger.Printf("agents: latest session for %d: %v", a.ID, err)
		http.Error(w, "could not load session", http.StatusInternalServerError)
		return
	}
	sess := latest
	if msgs, _ := s.ext.Agents.ListRecentChatMessages(ctx, latest.ID, 1); len(msgs) > 0 {
		// The latest session has content — open a genuinely new one.
		sess, err = s.ext.Agents.CreateSession(ctx, a.ID, a.Name)
		if err != nil {
			s.logger.Printf("agents: create session for %d: %v", a.ID, err)
			http.Error(w, "could not create session", http.StatusInternalServerError)
			return
		}
	}
	s.answerSessionOpened(ctx, w, r, a, sess)
}

// answerSessionOpened answers a request that opened (or chose) session sess of
// agent a: the panel body re-rendered on it, or a redirect to it.
func (s *Server) answerSessionOpened(ctx context.Context, w http.ResponseWriter, r *http.Request, a agents.Agent, sess agents.ChatSession) {
	// 🔴 THE PANEL GETS A BODY, THE PAGES KEEP THE REDIRECT, AND BOTH HALVES ARE
	// LOAD-BEARING. HX-Redirect makes htmx navigate; on /agents/{name} (and, before
	// task #633 deleted it, /operator) that is exactly right and is an existing
	// contract with its own tests. On /tmux
	// it would throw the grid, the chief panel, its live WebSocket and the
	// operator's place in the fleet away in order to open a thread in a slide-out
	// they were already looking at. So a request that says it came from the panel is
	// answered with the panel body, re-rendered on the NEW session — no redirect
	// header at all, because a response carrying both would navigate as well as swap.
	//
	// 🔴 AND IT RENDERS FOR THE AGENT THIS HANDLER JUST WROTE TO, not for a chief
	// resolved a second time. `a` is the chief here — the branch above made it so
	// before any write — so the write and the render cannot disagree about which agent
	// the operator is looking at. writeChiefPanelBody (the ctx-resolving front door)
	// would re-read the agent list and could answer about a different one.
	if chatSurfaceFromRequest(r) == ui.ChatSurfaceChiefPanel {
		s.writeChiefPanelBodyFor(ctx, w, a, strconv.FormatInt(sess.ID, 10))
		return
	}
	// Every agent lives at /agents/{name}. htmx follows HX-Redirect with a
	// navigation.
	//
	// ⚠ THERE USED TO BE A SPECIAL CASE HERE sending the reserved operator to
	// /operator instead. Task #633 deleted that page; task #653 phase two then
	// deleted the route that could re-provision such an agent, so `operator` is now
	// an unmintable name — but if a row of that name ever existed it would render at
	// /agents/operator like any other.
	target := "/agents/" + a.Name + "?session=" + strconv.FormatInt(sess.ID, 10)
	w.Header().Set("HX-Redirect", target)
	w.WriteHeader(http.StatusOK)
}

// chatSurfaceFromRequest reads the rendering surface off the query string.
//
// 🔴 IT FAILS TO THE PAGE SURFACE, AND THAT DIRECTION IS THE SAFE ONE. An
// unrecognised value must not select the panel branch: a page render that answered
// with a panel partial would replace an agent-detail document with a fragment, and
// the redirect contract the page surfaces depend on would be gone. An enumerated
// match rather than a non-empty test, so a typo is the page and not a third state.
func chatSurfaceFromRequest(r *http.Request) ui.ChatSurface {
	if r.URL.Query().Get(ui.ChatSurfaceQueryParam) == string(ui.ChatSurfaceChiefPanel) {
		return ui.ChatSurfaceChiefPanel
	}
	return ui.ChatSurfacePage
}

// handleAgentTaskModal renders the task-detail modal for a task-linked agent
// (opened by tapping the chat header title). It resolves the agent's linked task
// and renders its full markdown body + status + repo/model + comment thread. An
// unlinked agent (or any lookup failure) returns an empty body so the title's
// hx-swap simply clears the modal mount — no error surfaced to the user.
func (s *Server) handleAgentTaskModal(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if s.ext.Agents == nil || s.ext.Notes == nil {
		return
	}
	a, err := s.ext.Agents.GetByName(r.Context(), r.PathValue("name"))
	if err != nil || a.NoteID == nil {
		return
	}
	n, err := s.ext.Notes.Get(r.Context(), *a.NoteID)
	if err != nil {
		s.logger.Printf("agents: task modal: get note %d: %v", *a.NoteID, err)
		return
	}
	view := ui.TaskModalView{ID: n.ID, Status: n.Status, Body: n.Body, Repo: n.Repo, Model: n.Model}
	if comments, err := s.ext.Notes.ListComments(r.Context(), n.ID); err == nil {
		for _, c := range comments {
			view.Comments = append(view.Comments, ui.TaskModalComment{
				Author: c.Author, Body: c.Body, When: c.CreatedAt.Format("Jan 2 15:04"),
				// Carried, not dropped: this page has no retraction control, but it
				// must still draw the tombstone rather than an empty comment box.
				Retracted: c.Retracted,
			})
		}
	}
	if err := ui.RenderTaskModal(w, view); err != nil {
		s.logger.Printf("agents: render task modal: %v", err)
	}
}

// handleAgentLogsStream streams the agent pod's logs as SSE "log" events.
func (s *Server) handleAgentLogsStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	a, err := s.ext.Agents.GetByName(r.Context(), r.PathValue("name"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	ctx, cancel := s.streamContext(r.Context())
	defer cancel()
	err = s.ext.Provisioner.StreamLogs(ctx, a, func(line string) {
		// SSE data cannot contain raw newlines; logs are single lines already.
		fmt.Fprintf(w, "event: log\ndata: %s\n\n", line)
		flusher.Flush()
	})
	if err != nil && ctx.Err() == nil {
		s.logger.Printf("agents: stream logs %s: %v", a.Name, err)
	}
}

// handleAgentWS proxies a browser chat WebSocket to the agent gateway, streaming
// assistant deltas back and persisting the transcript.
func (s *Server) handleAgentWS(w http.ResponseWriter, r *http.Request) {
	a, err := s.ext.Agents.GetByName(r.Context(), r.PathValue("name"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// Resolve the chat session this socket talks to: the ?session=<id> one when
	// valid + owned by this agent, else the agent's latest (created if none). The
	// session's key is the gateway context; messages persist under its id.
	session, _ := s.resolveSession(r.Context(), a, r.URL.Query().Get("session"))
	// Same-origin only: previously OriginPatterns:["*"] let ANY website open an
	// authenticated agent-control socket in a logged-in user's browser. Allow the
	// host the browser connected to (r.Host) plus any configured public host (for
	// proxy setups that rewrite Host), and nothing else.
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: s.wsAllowedOrigins(r)})
	if err != nil {
		s.logger.Printf("agents: ws accept: %v", err)
		return
	}
	defer c.CloseNow()
	ctx := r.Context()
	// The socket's open and end, like a terminal's attached/detached: how many
	// chat sockets a page really holds (the browser suite counts them).
	s.logger.Printf("agents: chat %s connected", a.Name)
	defer s.logger.Printf("agents: chat %s disconnected", a.Name)

	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return // client closed or context done
		}
		var in struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(data, &in); err != nil || in.Type != "message" || strings.TrimSpace(in.Text) == "" {
			continue
		}
		// 🔴 THE TURN GATE FIRST, THEN THE ASKS (chief replacement × #134): a
		// message the gate refuses (the agent is being replaced) starts no turn,
		// so it must not answer the agent's asks either.
		if !s.turns.enter(a.ID) {
			_ = wsWriteJSON(ctx, c, map[string]string{"type": "error", "content": ErrAgentBeingReplaced.Error()})
			continue
		}
		// The operator wrote to the agent: its open asks are answered here.
		s.askerAddressed(ctx, attention.Asker{AgentID: a.ID}, in.Text)
		parts, err := s.runEnteredTurn(ctx, a, session, in.Text, wsStreamEmit(ctx, c, s, a.Name))
		s.turns.leave(a.ID)
		if err != nil {
			_ = wsWriteJSON(ctx, c, map[string]string{"type": "error", "content": err.Error()})
			continue
		}
		// 🔴 "done" BEFORE the turn is persisted and chat.reply broadcast: the page
		// cancels a #chat-log refresh while its turn is still streaming, and the
		// refresh that chat.reply asks for is the one that swaps the streamed
		// bubbles for the stored ones.
		_ = wsWriteJSON(ctx, c, map[string]string{"type": "done"})
		s.persistTurn(ctx, a, session, parts)
	}
}

// runUserTurn stores the user's message and runs the turn it starts against
// a's gateway, with every stream event passed to emit (nil-safe). It returns
// the reply's ORDERED parts — text and tool calls interleaved, so a reload
// shows the full response and not just its last text segment — for
// persistTurn to store, or the turn's error.
//
// 🔴 THE ONE WAY A USER MESSAGE BECOMES A TURN, for both doors that take one:
// the chat page's socket (handleAgentWS, which streams the events to the
// browser) and the composer's POST (agent_send.go, which has no stream to send
// them down). Two copies would be two transcripts' worth of disagreement about
// what is stored.
func (s *Server) runUserTurn(ctx context.Context, a agents.Agent, session agents.ChatSession, text string, emit agents.StreamEmit) ([]agents.TurnPart, error) {
	// Counted so the chief replacement can refuse while a turn is in flight, and
	// refused BEFORE the message is stored when the agent is being replaced
	// (turn_gate.go).
	if !s.turns.enter(a.ID) {
		return nil, ErrAgentBeingReplaced
	}
	defer s.turns.leave(a.ID)
	return s.runEnteredTurn(ctx, a, session, text, emit)
}

// runEnteredTurn is runUserTurn for a caller that has ALREADY entered a's turn
// gate (s.turns.enter) and leaves it afterwards: the operator's two doors, which
// must know the turn is admitted before they answer the agent's asks.
func (s *Server) runEnteredTurn(ctx context.Context, a agents.Agent, session agents.ChatSession, text string, emit agents.StreamEmit) ([]agents.TurnPart, error) {
	_, _ = s.ext.Agents.AddChatMessage(ctx, agents.ChatMessage{AgentID: a.ID, SessionID: session.ID, Role: "user", Content: text})

	// Mark active immediately (a turn always starts thinking) so the agent
	// shows in the notification panel's ACTIVE section before its first stream
	// event lands; the emit refines the phase as events arrive.
	s.markAgentActive(requestOwner(ctx), a.Name, "thinking")
	pc := &agents.PartCollector{}
	collect := func(ev agents.StreamEvent) {
		pc.Observe(ev)
		if emit != nil {
			emit(ev)
		}
	}
	reply, err := s.chatTurn(ctx, a, session.SessionKey, text, collect)
	// The turn ended (reply or error): drop the live phase. The entry lingers
	// as "recently active" until activeAgentWindow lapses.
	s.markAgentIdle(a.Name)
	if err != nil {
		return nil, err
	}
	parts := pc.Parts()
	if len(parts) == 0 && reply != "" {
		parts = []agents.TurnPart{{Kind: "text", Content: reply}} // fallback
	}
	return parts, nil
}

// persistTurn stores a finished turn's parts and tells open pages a reply was
// persisted.
func (s *Server) persistTurn(ctx context.Context, a agents.Agent, session agents.ChatSession, parts []agents.TurnPart) {
	for _, part := range parts {
		_, _ = s.ext.Agents.AddChatMessage(ctx, agents.ChatMessage{
			AgentID: a.ID, SessionID: session.ID, Role: "assistant",
			Kind: part.Kind, Content: part.Content, ToolID: part.ToolID, ToolName: part.ToolName, ToolOK: part.ToolOK,
		})
	}
	if len(parts) > 0 {
		// Nudge the shell's notification FAB to refetch its panel + badge: one
		// broadcast per completed turn. Carries the agent name (the client only
		// needs the nudge).
		s.broadcast(ctx, EventChatReply, a.Name)
	}
}

// resolveSession picks the chat session for a request: the ?session=<id> one
// when valid + owned by this agent, else the agent's latest (created if none).
// Returns just the session (no view list) for the WS + operator-steer paths.
func (s *Server) resolveSession(ctx context.Context, a agents.Agent, sessionParam string) (agents.ChatSession, error) {
	if id, err := strconv.ParseInt(sessionParam, 10, 64); err == nil && id > 0 {
		if sess, err := s.ext.Agents.GetSession(ctx, id); err == nil && sess.AgentID == a.ID {
			return sess, nil
		}
	}
	return s.ext.Agents.LatestOrCreateSession(ctx, a.ID, a.Name)
}

// wsAllowedOrigins returns muster's own origin hosts: the host the browser
// connected to (r.Host) plus the configured public host (MUSTER_PUBLIC_URL,
// for proxy setups that rewrite Host). This is the same-origin enforcement that
// replaces the old OriginPatterns:["*"] on the WebSockets, and the ONE list
// requireSameOrigin checks state-changing operator requests against.
func (s *Server) wsAllowedOrigins(r *http.Request) []string {
	out := make([]string, 0, 2)
	if r.Host != "" {
		out = append(out, r.Host)
	}
	if base := s.ext.GitHubOAuth.BaseURL; base != "" {
		if u, err := url.Parse(base); err == nil && u.Host != "" {
			out = append(out, u.Host)
		}
	}
	if len(out) == 0 {
		out = append(out, "localhost")
	}
	return out
}

// chatTurn runs one chat turn with native tools (the worker tool set),
// dispatching tool calls in-process via /v1/responses and surfacing live
// text/thinking/tool events to emit (nil-safe). Falls back to a plain (tool-less)
// chat when the agent's gateway predates /v1/responses (older image) — there the
// only stream is text, so the StreamEmit is adapted to a text-delta callback.
//
// 🔴 THIS BRANCH DROPS *TOOLS*, NOT A TRANSPORT, AND THAT DISTINCTION IS WHY IT
// SURVIVED Gateway.Chat MOVING ONTO /v1/responses. Chat now runs over the responses
// endpoint too and falls back to chat-completions internally on a 404, so a reading
// of this branch as "retry the same failing endpoint" is available and wrong: what
// ChatWithTools lost on a 404 is the TOOLS, and api.Gateway's own doc puts that
// decision here because this package is the only place that knows a toolless answer
// is acceptable for an interactive chat turn (it is not, for a kickoff). Delete this
// branch and an agent on an image that predates the endpoint loses every chat turn
// to an error instead of answering without tools.
//
// ⚠ WHAT IT COSTS ON THAT PATH, STATED BECAUSE NOTHING ELSE CAN SEE IT: a turn
// against such an image is now THREE requests — ChatWithTools' 404, Chat's own 404
// probe, and the chat-completions call that answers. The alternative is a second
// transport-specific method on api.Gateway, which would push the choice of wire
// format into this package; two cheap 404s against an already-reachable pod buys
// keeping it out. Nothing here may assume an image is new enough to skip it —
// internal/agentspec takes the agent image tag from configuration and has no
// default, so a deployment can still point at one that answers 404.
//
// 🔴 DELETION-TRACKING NOTE (upstream task #653, phase two). THERE USED TO BE TWO
// TOOL SETS HERE, selected by `if a.Name == reservedOperatorSlug`: the operator arm
// handed that one row operatorSystemPrompt + operatorToolDefs() +
// s.operatorToolDispatch(ctx), i.e. task-board writes including `complete`, agent
// dispatch/steer/stop, and — the widest of them — operator_grant_profile, which
// applied live RBAC to an ARBITRARY agent id with no request on file. All of that
// went with internal/api/operator.go.
//
// ⚠ IT WAS NOT MERELY DEAD, IT IS NOW UNREACHABLE BY CONSTRUCTION, and the two are
// different claims. Phase one deleted the operator agent ROW and the
// devpod-operator namespace, which made the branch dead IN PRODUCTION — a state no
// code asserted and an operator could have undone with one re-provision. Phase two
// deleted handleOperatorProvision, which was the last call site that set
// dispatchParams.Name to anything but agents.ChiefName; BuildUniqueAgentName draws
// adjective-noun pairs, so a hyphen-free name cannot be generated either. ⚠ THAT
// SECOND CLAIM TAKES TWO PINS AND THIS LINE CITED ONE.
// TestReservedAgentNamesAreNotPoolShaped (chief_provision_test.go) checks only that
// the reserved names contain no hyphen; that the POOL always emits one is
// internal/agents/names_test.go's TestGenerateNameFormat. There is now no path in
// this repo that can create an agent named `operator`.
//
// 🔴 THE POD-RENDER LEDGER MOVED WITH IT. chatTurn and handleAgentWS were on
// pod_render_callers_test.go's derived set ONLY through this branch's
// dispatchOperatorTool arms; with it gone, a chat turn cannot render a pod at all.
// That ledger is updated in the same commit — do not re-add a row for a path that
// no longer exists.
func (s *Server) chatTurn(ctx context.Context, a agents.Agent, sessionKey, text string, emit agents.StreamEmit) (string, error) {
	// The session's runtime and model ride on the requests (session_runtime.go);
	// a legacy session carries nothing, exactly as before.
	ctx = s.sessionRoute(ctx, a, sessionKey)
	instr, tools, dispatch := s.turnInstructions(ctx, a), s.AgentToolDefsFor(ctx, a), s.AgentToolDispatch(ctx, a)
	reply, err := s.ext.Gateway.ChatWithTools(ctx, a, sessionKey, instr, text, tools, dispatch, emit)
	// 🔴 ONLY A 404 ON THE FIRST REQUEST MEANS "THIS IMAGE HAS NO /v1/responses". A
	// 404 on request 2+ comes after the runtime answered request 1 and its tools
	// ran; resending the message toolless there pastes it into the session twice
	// (agents.AfterFirstRequestError).
	if errors.Is(err, agents.ErrResponsesUnsupported) && !agents.PastFirstRequest(err) {
		s.logger.Printf("agents: %s gateway lacks /v1/responses; retrying the turn WITHOUT tools "+
			"(that runtime's image predates the endpoint, so it answers over chat-completions)", a.Name)
		var textEmit func(string)
		if emit != nil {
			textEmit = func(delta string) { emit(agents.StreamEvent{Kind: "text", Text: delta}) }
		}
		return s.ext.Gateway.Chat(ctx, a, sessionKey, text, textEmit)
	}
	return reply, err
}

// wsStreamEmit builds an agents.StreamEmit that translates each StreamEvent into
// the exact WS JSON message the chat client is built to consume. This is the
// server→browser half of the streaming bridge; it serves BOTH the worker and the
// operator chat (they share the WS path).
//
// As a side effect it updates the server's active-agent registry so the
// notification panel's ACTIVE section reflects the live phase: a text delta means
// the agent is "responding"; a thinking event means it is "thinking". The caller
// is responsible for the start ("thinking" on turn start) and the clear
// (markAgentIdle on turn end). srv may be nil (tests / paths without a server),
// in which case only the WS translation happens.
func wsStreamEmit(ctx context.Context, c *websocket.Conn, srv *Server, agentName string) agents.StreamEmit {
	owner := requestOwner(ctx) // the chat's session user: the agent's owner under RLS
	return func(ev agents.StreamEvent) {
		if srv != nil {
			switch ev.Kind {
			case "text":
				srv.markAgentActive(owner, agentName, "responding")
			case "thinking":
				srv.markAgentActive(owner, agentName, "thinking")
			}
		}
		switch ev.Kind {
		case "text":
			_ = wsWriteJSON(ctx, c, map[string]any{"type": "delta", "content": ev.Text})
		case "thinking":
			_ = wsWriteJSON(ctx, c, map[string]any{"type": "thinking", "content": ev.Text})
		case "tool_call":
			_ = wsWriteJSON(ctx, c, map[string]any{"type": "tool_call", "id": ev.ToolID, "name": ev.ToolName, "args": ev.ToolArgs,
				"target": cctranscript.ToolTarget(ev.ToolName, ev.ToolArgs)})
		case "tool_result":
			_ = wsWriteJSON(ctx, c, map[string]any{"type": "tool_result", "id": ev.ToolID, "name": ev.ToolName, "ok": ev.ToolOK, "output": ev.ToolOutput})
		}
	}
}

func wsWriteJSON(ctx context.Context, c *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.Write(ctx, websocket.MessageText, b)
}

// firstNonEmptyLine returns the first line of s that is not blank, trimmed.
func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}
