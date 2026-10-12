package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/taskstatus"
	"github.com/spf13/cobra"
)

// agentBrief is the subset of GET /api/agents this CLI reads. Plain
// encoding/json, so every other field the server sends (and every field a NEWER
// server adds) is simply ignored here — and `agent ls` never touches this type
// at all, it passes the server's bytes straight through.
type agentBrief struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Namespace   string `json:"namespace"`
	DisplayName string `json:"displayName"`
	Status      string `json:"status"`
}

func newAgentCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Agent roster, name->id resolution, chat history, and an agent's own task",
	}
	cmd.AddCommand(newAgentLsCmd(a), newAgentResolveCmd(a), newAgentMessagesCmd(a), newAgentTaskCmd(a))
	return cmd
}

// agentMessagesRoute is a TEMPLATE: `agent messages "$NAME"` with an unset NAME
// must fail at exit 2 having sent nothing, rather than GET /api/agents//messages
// — which ServeMux cleans into /api/agents/messages and answers with a redirect
// to a route that means something else.
const agentMessagesRoute = "/api/agents/{name}/messages"

// The `--limit` default and ceiling, mirroring the server's so `--help` states
// the numbers the server will actually apply.
//
// ⚠ THEY ARE COPIES, AND THE DRIFT IS GUARDED BY A TEST rather than by a build
// edge: the shipped binary deliberately does not import internal/api (that
// would pull the whole server into the client), so
// TestTheAgentMessageLimitsMatchTheServers imports it from a TEST file and
// fails when the two disagree in EITHER direction.
const (
	agentMessagesDefaultLimit = 50
	agentMessagesMaxLimit     = 200
)

// newAgentMessagesCmd wraps GET /api/agents/{name}/messages.
//
// 🔴 WHY IT IS IN THE `agent` FAMILY AND NOT UNDER `chief`. It is keyed on an
// agent NAME, which is what this family already deals in (`agent resolve`), and
// it reads the /api/agents namespace — it is the sibling of `agent ls`, one
// level down. And it is not chief-specific: every agent has chat history;
// `chief ask --agent` merely defaults to one of them.
func newAgentMessagesCmd(a *app) *cobra.Command {
	var (
		session int64
		before  int64
		limit   int
	)
	cmd := &cobra.Command{
		Use:   "messages <agent-name>",
		Short: "GET /api/agents/{name}/messages — one chat session's persisted transcript",
		Long: `Read what was said to an agent and what it said back.

This is the READ half of ` + "`chief ask`" + `, which posts a message and prints one reply
and leaves no way to see the conversation afterwards — not even the agent's own
past replies.

--session  a chat session id; default is the agent's MOST RECENTLY ACTIVE one,
           which is the same session ` + "`chief ask`" + ` posts into. A session id that
           belongs to a DIFFERENT agent is a 404, not that agent's transcript.
--limit    how many messages, keeping the NEWEST (default ` + strconv.Itoa(agentMessagesDefaultLimit) + `, ceiling ` + strconv.Itoa(agentMessagesMaxLimit) + `).
           A larger value is clamped, not refused, and the response reports the
           ` + "`limit`" + ` actually applied.
--before   a message id: return the page immediately OLDER than that message.
           Take it from the previous response's ` + "`nextBefore`" + `, which is emitted
           only while an older page exists — so paging backwards is
           ` + "`while nextBefore != 0`" + ` and never a guess.

🔴 A TURN IS SEVERAL MESSAGES, NOT ONE. An assistant turn persists as ordered rows
— narration ` + "`text`" + `, then ` + "`tool_call`" + ` (content = the raw JSON arguments), then
` + "`tool_result`" + ` (content = the dispatch output, ` + "`toolOk`" + ` = whether it succeeded),
interleaved as they happened. ` + "`kind`" + ` is what tells them apart and ` + "`toolId`" + ` is what
links a call to its result. Nothing here reassembles them: the interleaving IS the
record.

It reads on ` + envToken + ` (or ` + envTokenAlias + `). Inside a muster-dispatched instance that variable
carries the instance's own row token, which is what makes this verb reachable
from inside one.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// 🔴 CHECKED CLIENT-SIDE SO A TYPO FAILS AT THE PROMPT, with a DIFFERENT
			// message from the server's, so a test can tell which of the two fired.
			// The server refuses these too — this is a better error, never a gate.
			if limit <= 0 {
				return failf(exitUsage, "--limit must be a positive integer, got %d.", limit)
			}
			if before < 0 {
				return failf(exitUsage, "--before must be a positive message id, got %d.", before)
			}
			if session < 0 {
				return failf(exitUsage, "--session must be a positive chat session id, got %d.", session)
			}
			path, err := expandPath(agentMessagesRoute, map[string]string{"name": args[0]})
			if err != nil {
				return err
			}
			q := url.Values{}
			q.Set("limit", strconv.Itoa(limit))
			if session > 0 {
				q.Set("session", strconv.FormatInt(session, 10))
			}
			if before > 0 {
				q.Set("before", strconv.FormatInt(before, 10))
			}
			body, err := a.call(cmd.Context(), request{
				method: http.MethodGet, path: path, query: q, cred: credHook,
			})
			if err != nil {
				return err
			}
			return a.emit(body)
		},
	}
	cmd.Flags().Int64Var(&session, "session", 0, "chat session id (default: the agent's most recently active session)")
	cmd.Flags().Int64Var(&before, "before", 0, "return the page older than this message id (from a previous response's nextBefore)")
	cmd.Flags().IntVar(&limit, "limit", agentMessagesDefaultLimit,
		"how many messages to return, newest kept (clamped to "+strconv.Itoa(agentMessagesMaxLimit)+")")
	return cmd
}

// The agent self-service routes. Unlike /api/tasks/*, these carry NO task id:
// the caller's token IS the identity. api.requireAgentToken resolves the token
// to an agent row and attaches it, so the server already knows which task is
// bound to the caller and who is speaking.
const (
	agentTaskRoute        = "/agent/task"
	agentTaskCommentRoute = "/agent/task/comment"
	agentTaskStatusRoute  = "/agent/task/status"
)

// newAgentTaskCmd wraps the three /agent/task* routes — the API a dispatched
// agent uses to work its OWN bound task.
//
// 🔴 This is NOT a convenience alias for `task`. The two command families reach
// different routes with different identity models, and using the wrong one
// silently costs two properties:
//
//   - ATTRIBUTION. /agent/task/comment stores the resolved agent's name, so a
//     comment lands under it. `task comment` derives the author from the
//     provenance header, whose allowlist collapses anything unrecognised to
//     "api" — an agent using it would author as the generic coding-agent
//     source, indistinguishable from a human-driven local pickup.
//   - THE COMPLETE GATE. /agent/task/status refuses `complete`
//     (taskstatus.AllowedForAgent). PATCH /api/tasks/{id}/status deliberately
//     allows every status for any hook-token holder, so an agent routed there
//     could declare its own work done.
//
// Neither property is re-implemented here; both are the server's, and this
// command family exists so agents keep reaching the routes that enforce them.
func newAgentTaskCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "task",
		Short: "The calling agent's own bound task (/agent/task*), identified by the agent token",
		Long: `Read and update the task bound to the calling agent.

These commands are for a muster-dispatched agent acting on its OWN task. The
agent's token both authenticates and identifies the caller, so no task id is
passed: the server resolves the bound task from the token.

Inside a muster-dispatched instance there is nothing to configure: the
provisioner exports ` + envTokenAlias + ` and ` + envAPIURL + `, so resolution finds
them without a flag.

🔴 Do NOT pass the token as ` + "`--token`" + ` in an instance. argv is world-readable
via /proc; the environment and the env file both avoid that.

Prefer these over ` + "`task …`" + ` from inside an agent instance. The task commands
take an explicit id and attribute comments from a provenance header, which would
record an agent's report as a generic coding-agent session rather than its own
name, and their status route permits ` + "`complete`" + ` — which the agent route refuses.`,
	}
	cmd.AddCommand(newAgentTaskGetCmd(a), newAgentTaskCommentCmd(a), newAgentTaskStatusCmd(a))
	return cmd
}

func newAgentTaskGetCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "get",
		Short: "GET /agent/task — the calling agent's task, its status and its comment thread",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			body, err := a.call(cmd.Context(), request{method: http.MethodGet, path: agentTaskRoute, cred: credHook})
			if err != nil {
				return err
			}
			return a.emit(body)
		},
	}
}

func newAgentTaskCommentCmd(a *app) *cobra.Command {
	var (
		b        taskCommentBody
		bodyFile string
	)
	cmd := &cobra.Command{
		Use:   "comment",
		Short: "POST /agent/task/comment — report on the calling agent's task, authored as the agent",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if bodyFile != "" {
				if b.Body != "" {
					return failf(exitUsage, "--body and --body-file are mutually exclusive")
				}
				data, err := readBodyFile(bodyFile, cmd.InOrStdin())
				if err != nil {
					return err
				}
				b.Body = data
			}
			if strings.TrimSpace(b.Body) == "" {
				return failf(exitUsage, "a comment body is required: pass --body or --body-file (- reads stdin)")
			}
			// No --source and no provenance header: the author is the agent resolved
			// from the token, so there is nothing here to attribute and nothing to
			// downgrade. warnAttribution is deliberately not called for the same
			// reason.
			raw, err := a.call(cmd.Context(), request{
				method: http.MethodPost,
				path:   agentTaskCommentRoute,
				body:   b,
				cred:   credHook,
			})
			if err != nil {
				return err
			}
			a.warnTruncated(b.Body, raw)
			return a.emit(raw)
		},
	}
	cmd.Flags().StringVar(&b.Body, "body", "", "comment body (markdown)")
	cmd.Flags().StringVar(&bodyFile, "body-file", "", "read the body from a file, or - for stdin")
	return cmd
}

// newAgentTaskStatusCmd wraps PATCH /agent/task/status.
//
// The pre-flight uses taskstatus.AllowedForAgent — the SAME predicate the server
// enforces, imported rather than restated, so the vocabulary cannot drift into
// two disagreeing copies. It is a better error message for a typo, never a
// second gate: the server refuses `complete` whether or not this check runs.
func newAgentTaskStatusCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "status <" + strings.Join(agentTaskStatuses(), "|") + ">",
		Short: "PATCH /agent/task/status — move the calling agent's task through the working states",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			status := strings.TrimSpace(args[0])
			if !taskstatus.AllowedForAgent(status) {
				if taskstatus.Valid(status) {
					return failf(exitUsage,
						"refusing to send %q: an agent may not declare its own task complete. The server enforces this too (taskstatus.AllowedForAgent); set %s and let a human or the operator close it.",
						status, taskstatus.ReadyForReview)
				}
				return failf(exitUsage, "%s: %q. Statuses an agent may set are %s.",
					invalidStatusMsg, args[0], strings.Join(agentTaskStatuses(), ", "))
			}
			body, err := a.call(cmd.Context(), request{
				method: http.MethodPatch,
				path:   agentTaskStatusRoute,
				body:   taskStatusBody{Status: status},
				cred:   credHook,
			})
			if err != nil {
				return err
			}
			return a.emit(body)
		},
	}
}

// agentTaskStatuses is the agent-settable vocabulary, DERIVED by filtering the
// full list through the server's own predicate rather than being listed here. A
// status added to taskstatus.All() therefore appears in this help text without
// an edit.
func agentTaskStatuses() []string {
	var out []string
	for _, s := range taskstatus.All() {
		if taskstatus.AllowedForAgent(s) {
			out = append(out, s)
		}
	}
	return out
}

func newAgentLsCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "GET /api/agents — the agent roster as JSON",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			body, err := a.call(cmd.Context(), request{method: http.MethodGet, path: "/api/agents", cred: credHook})
			if err != nil {
				return err
			}
			return a.emit(body)
		},
	}
}

func newAgentResolveCmd(a *app) *cobra.Command {
	var idOnly bool
	cmd := &cobra.Command{
		Use:   "resolve <name>",
		Short: "Resolve an agent name to its id over GET /api/agents",
		Long: `Resolve an agent name (the slug, e.g. clever-fox) to its numeric id.

This is the read that replaces "SELECT id FROM agents WHERE name=..." — the
capability has existed in GET /api/agents all along, it was simply not
discoverable. Resolution is unambiguous by construction:

  exactly one match  -> the agent object (or just the id with --id), exit 0
  no match           -> exit 4, candidates listed on stderr
  more than one      -> exit 2, every candidate listed on stderr

An empty name is a usage error (exit 2) rather than a request, because an empty
value interpolated into a route is what produces the doubled-slash 301.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, brief, err := a.resolveAgent(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if idOnly {
				// A bare integer is still a valid JSON document, so stdout stays
				// parseable.
				return a.emit([]byte(strconv.FormatInt(brief.ID, 10)))
			}
			return a.emit(raw)
		},
	}
	cmd.Flags().BoolVar(&idOnly, "id", false, "print only the numeric id (still valid JSON)")
	return cmd
}

// resolveAgent fetches the roster and finds the single agent matching name. It
// returns the RAW roster element (so unknown fields survive) plus the decoded
// brief.
func (a *app) resolveAgent(ctx context.Context, name string) (json.RawMessage, agentBrief, error) {
	q := strings.TrimSpace(name)
	if q == "" {
		return nil, agentBrief{}, failf(exitUsage, "agent name is empty; refusing to resolve")
	}
	body, err := a.call(ctx, request{method: http.MethodGet, path: "/api/agents", cred: credHook})
	if err != nil {
		return nil, agentBrief{}, err
	}
	var rawList []json.RawMessage
	if err := json.Unmarshal(body, &rawList); err != nil {
		return nil, agentBrief{}, wrapf(exitNonJSON, err, "GET /api/agents did not return a JSON array")
	}
	briefs := make([]agentBrief, len(rawList))
	for i, r := range rawList {
		// A malformed element is skipped rather than fatal: one bad row must not
		// make every lookup fail.
		_ = json.Unmarshal(r, &briefs[i])
	}

	// Exact slug match first — that is the identifier the server uses for the
	// instance namespace and for /agents/{name}. Only if nothing matches exactly
	// do we widen to a case-insensitive name or display-name match, which is
	// where ambiguity can legitimately arise.
	var idx []int
	for i, b := range briefs {
		if b.Name == q {
			idx = append(idx, i)
		}
	}
	if len(idx) == 0 {
		for i, b := range briefs {
			if strings.EqualFold(b.Name, q) || strings.EqualFold(b.DisplayName, q) {
				idx = append(idx, i)
			}
		}
	}

	switch len(idx) {
	case 1:
		return rawList[idx[0]], briefs[idx[0]], nil
	case 0:
		fmt.Fprintf(a.stderr, "no agent named %q; %d agent(s) known:\n", q, len(briefs))
		for _, b := range briefs {
			fmt.Fprintf(a.stderr, "  %d\t%s\t%s\n", b.ID, b.Name, b.Status)
		}
		return nil, agentBrief{}, failf(exitNotFound, "no agent matches %q", q)
	default:
		fmt.Fprintf(a.stderr, "%q matches %d agents:\n", q, len(idx))
		for _, i := range idx {
			fmt.Fprintf(a.stderr, "  %d\t%s\t%s\n", briefs[i].ID, briefs[i].Name, briefs[i].Status)
		}
		return nil, agentBrief{}, failf(exitUsage, "agent name %q is ambiguous (%d matches); use the exact slug or the id", q, len(idx))
	}
}
