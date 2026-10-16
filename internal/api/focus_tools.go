package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/focus"
)

// THE FOCUS TOOLS (internal/focus): how the chief finds the session working on
// something. agents_list, focus_search, focus_refresh — all CHIEF-ONLY: they
// enumerate (or spend summarizer budget on) the owner's WHOLE fleet, which is
// the chief's job and no worker's. Token-efficient by design: ids, kinds,
// names, statuses, titles and — where a session has no focus or a stale one —
// the short reason why; a search hit carries a snippet of at most
// focus.MaxSnippetRunes. All three are on MCP too, so a claude-code chief
// reaches them. Focus itself has ONE writer, muster's own refresh.

const (
	agentsListToolName   = "agents_list"
	focusSearchToolName  = "focus_search"
	focusRefreshToolName = "focus_refresh"
	// focusRefreshTimeout bounds focus_refresh on one subject: a tail read and
	// one summarizer call.
	focusRefreshTimeout = 90 * time.Second
)

func focusToolDefs() []agents.ToolDef {
	subject := strProp(`A session id as agents_list and focus_search return it: "agent:<id>" or "mirror:<claude session id>".`)
	return []agents.ToolDef{
		{Type: "function", Name: agentsListToolName,
			Description: "List your owner's sessions — dispatched agents and mirrored Claude Code windows — compactly: " +
				"id, kind, name, status, focus title (what it is working on) and, when a session has no focus or a stale one, " +
				"focus_problem: the short reason (e.g. no OpenRouter key, rate-limited). Use focus_search to find one by topic.",
			Parameters: objSchema(map[string]any{})},
		{Type: "function", Name: focusSearchToolName,
			Description: "Full-text search over your owner's sessions' focus (what each is working on). Returns the best " +
				"matches first: id, kind, title and a short snippet. Use it to find which session is best placed to handle something.",
			Parameters: objSchema(map[string]any{
				"query": strProp("Words to look for, e.g. \"invoice reconciliation\"."),
				"limit": map[string]any{"type": "integer", "description": "At most this many results (1-20, default 5)."},
			}, "query")},
		{Type: "function", Name: focusRefreshToolName,
			Description: "Recompute one session's focus now and wait for it. muster computes it — it costs you no tokens — " +
				"and skips a session whose transcript has not changed. Every session that moved is refreshed on its own " +
				"every few minutes; use this only when you need one NOW.",
			Parameters: objSchema(map[string]any{"subject": subject}, "subject")},
	}
}

// agentsListRow is one agents_list row: nothing a decision does not need.
type agentsListRow struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Focus  string `json:"focus,omitempty"`
	// FocusProblem is why the session has no focus, or a stale one: the
	// latest refresh's short, non-secret reason (focus.Reason). Absent when
	// the focus is current.
	FocusProblem string `json:"focus_problem,omitempty"`
}

func (s *Server) toolAgentsList(ctx context.Context) string {
	fi := s.focusIndex(ctx)
	rows := []agentsListRow{}
	if s.ext.Agents != nil {
		list, err := s.ext.Agents.List(ctx)
		if err != nil {
			return toolErr("could not list agents")
		}
		idx := s.instanceIndex(ctx)
		for _, a := range list {
			sub := focus.AgentSubject(a.ID)
			rows = append(rows, agentsListRow{ID: sub.String(), Kind: agents.ResolveKind(a.Kind),
				Name: firstNonBlank(a.DisplayName, a.Name), Status: liveStatusIndexed(a, idx), Focus: fi[sub].Title, FocusProblem: fi[sub].Problem()})
		}
	}
	seen := map[string]bool{}
	for _, e := range s.mirrorClaudeEntries(ctx) {
		if seen[e.ClaudeSession] {
			continue
		}
		seen[e.ClaudeSession] = true
		sub := focus.MirrorSubject(e.ClaudeSession)
		st := s.ext.Mirror.AgentState(ctx, e.Host, e.Session, e.Window)
		status := st.Status
		if status == "" {
			status = "unknown"
		}
		rows = append(rows, agentsListRow{ID: sub.String(), Kind: "mirror", Name: e.DisplayTitle() + " @ " + e.Host,
			Status: status, Focus: fi[sub].Title, FocusProblem: fi[sub].Problem()})
	}
	return toolJSON(map[string]any{"sessions": rows})
}

func (s *Server) toolFocusSearch(ctx context.Context, arguments string) string {
	var in struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal([]byte(orEmptyJSON(arguments)), &in); err != nil || strings.TrimSpace(in.Query) == "" {
		return toolErr("arguments are {query, limit?}")
	}
	if in.Limit <= 0 {
		in.Limit = 5
	}
	hits, err := s.ext.Focus.Search(ctx, in.Query, in.Limit)
	if err != nil {
		s.logger.Printf("focus: search: %v", err)
		return toolErr("the search failed")
	}
	type row struct {
		ID      string `json:"id"`
		Kind    string `json:"kind"`
		Title   string `json:"title"`
		Snippet string `json:"snippet,omitempty"`
	}
	out := make([]row, 0, len(hits))
	for _, h := range hits {
		out = append(out, row{ID: h.Subject.String(), Kind: string(h.Subject.Kind), Title: h.Title, Snippet: h.Snippet})
	}
	return toolJSON(map[string]any{"results": out})
}

func (s *Server) toolFocusRefresh(ctx context.Context, a agents.Agent, arguments string) string {
	r := s.refresher()
	if r == nil {
		return toolErr("focus cannot be recomputed here: no summarizer is configured on this deployment")
	}
	var in struct {
		Subject string `json:"subject"`
	}
	_ = json.Unmarshal([]byte(orEmptyJSON(arguments)), &in)
	owner := agentOwnerID(a)
	// 🔴 A SUBJECT IS REQUIRED. There is no whole-owner form: the periodic pass
	// already refreshes every session that moved.
	if strings.TrimSpace(in.Subject) == "" {
		return toolErr("subject is required: the session to refresh (agent:<id> or mirror:<claude session id>)")
	}
	sub, err := focus.ParseSubject(in.Subject)
	if err != nil {
		return toolErr("%s", strings.TrimPrefix(err.Error(), "focus: "))
	}
	ctx, cancel := context.WithTimeout(ctx, focusRefreshTimeout)
	defer cancel()
	o, err := r.Refresh(ctx, owner, sub)
	switch {
	case errors.Is(err, focus.ErrCapped):
		return toolErr("%s", strings.TrimPrefix(err.Error(), "focus: "))
	case errors.Is(err, focus.ErrNoSession):
		return toolErr("%s is not a session of your owner's that muster can see now", sub)
	case err != nil:
		return toolJSON(map[string]any{"subject": sub.String(), "outcome": string(o), "focus_problem": focus.Reason(err),
			"error": "the refresh failed; the previous focus is kept"})
	}
	res := map[string]any{"subject": sub.String(), "outcome": string(o)}
	if f, ok, _ := s.ext.Focus.Get(ctx, sub); ok && f.Has() {
		res["title"], res["description"] = f.Title, f.Description
	}
	return toolJSON(res)
}

// firstNonBlank is name when it is not blank, else fallback.
func firstNonBlank(name, fallback string) string {
	if strings.TrimSpace(name) != "" {
		return name
	}
	return fallback
}
