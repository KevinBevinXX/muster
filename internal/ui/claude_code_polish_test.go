package ui

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/notes"
)

var bothKinds = KindChoices{Kinds: []string{agents.KindGateway, agents.KindClaudeCode}, Accounts: []string{"alpha"}}

// gatewayOnlyBlock returns the markup of the FIRST data-gateway-only container in
// html, up to the next sibling marker it can bound on (the end marker), or "".
func gatewayOnlyBlock(html, end string) string {
	// Anchored on the ELEMENT, not the bare attribute name: the kind toggle's
	// script text names [data-gateway-only] earlier in the document.
	i := strings.Index(html, "<div "+gatewayOnlyAttr)
	if i < 0 {
		return ""
	}
	j := strings.Index(html[i:], end)
	if j < 0 {
		return html[i:]
	}
	return html[i : i+j]
}

// TestEveryOpenRouterControlInTheDispatchFormsIsGatewayOnly pins WHICH controls
// the kind toggle hides, in both dispatch forms: the model selector (the
// OpenRouter-specific one — curated OpenRouter slugs and the live OpenRouter
// search, data-combobox-remote="/api/openrouter/models") and the repository.
// The behavioural half — that they actually disappear on a tap — is
// e2e/tests/claude-code-polish.spec.ts.
func TestEveryOpenRouterControlInTheDispatchFormsIsGatewayOnly(t *testing.T) {
	forms := map[string]string{
		"FAB form":         renderNode(t, DispatchModalBodyFor(bothKinds)),
		"task-scoped form": renderNode(t, DispatchModalTaskBody(TaskDispatchView{NoteID: 9, Model: "openrouter/vendor/model-x", Kinds: bothKinds})),
	}
	for name, html := range forms {
		block := gatewayOnlyBlock(html, `type="submit"`)
		if block == "" {
			t.Errorf("%s: no data-gateway-only container", name)
			continue
		}
		for _, want := range []string{`data-combobox-remote="/api/openrouter/models"`, `name="model"`, `name="repo"`} {
			if !strings.Contains(block, want) {
				t.Errorf("%s: %s is outside the gateway-only container — it stays on screen for a "+
					"Claude Code dispatch", name, want)
			}
		}
		// Nothing OpenRouter-specific may sit outside it.
		outside := strings.Replace(html, block, "", 1)
		if strings.Contains(outside, "/api/openrouter/models") || strings.Contains(outside, `name="model"`) {
			t.Errorf("%s: a model/OpenRouter control renders outside the gateway-only container", name)
		}
		if !strings.Contains(html, "[data-gateway-only]") {
			t.Errorf("%s: the kind toggle does not select [data-gateway-only], so nothing hides it", name)
		}
	}
}

// TestTheBoardCardPrintsTheTimeTheBoardIsOrderedBy: the task board is ordered by
// most recent activity, so its card prints that time; the detail page keeps the
// creation age. Times are pairwise distinct (created < updated < activity).
func TestTheBoardCardPrintsTheTimeTheBoardIsOrderedBy(t *testing.T) {
	created := time.Date(2025, 4, 1, 8, 0, 0, 0, time.UTC)
	n := notes.Note{ID: 31, Body: "a task", Status: notes.StatusOpen,
		CreatedAt: created, UpdatedAt: created.Add(2 * time.Hour), ActivityAt: created.Add(30 * time.Hour)}
	ts := func(at time.Time) string { return `data-ts="` + strconv.FormatInt(at.UnixMilli(), 10) + `"` }

	board := renderNode(t, noteCard(TaskCardView{Note: n}))
	if !strings.Contains(board, ts(n.ActivityAt)) {
		t.Errorf("the board card does not print its last activity %s", ts(n.ActivityAt))
	}
	if strings.Contains(board, ts(created)) {
		t.Errorf("the board card still prints its creation time %s over an activity-ordered list", ts(created))
	}

	detail := renderNode(t, noteCard(TaskCardView{Note: n, Detail: true}))
	if !strings.Contains(detail, ts(created)) {
		t.Errorf("the detail card lost its creation age %s", ts(created))
	}

	// A single-card re-render reads the note through Get, which computes no
	// ActivityAt: the card must then print UpdatedAt, never fall back to creation.
	n.ActivityAt = time.Time{}
	rerender := renderNode(t, noteCard(TaskCardView{Note: n}))
	if !strings.Contains(rerender, ts(n.UpdatedAt)) {
		t.Errorf("a re-rendered board card without ActivityAt does not print UpdatedAt %s", ts(n.UpdatedAt))
	}
}

// TestTheAgentsListRefreshesOnServerEvents: the list re-renders (and so
// re-orders) on the SSE events that change it, not only on a local action's
// HX-Trigger — and each card's recent-log slot is refreshed after the list has
// settled rather than racing it.
func TestTheAgentsListRefreshesOnServerEvents(t *testing.T) {
	html := renderNode(t, AgentsPanel(true))
	i := strings.Index(html, `id="agents-list"`)
	if i < 0 {
		t.Fatal("instrument check FAILED: no #agents-list")
	}
	list := html[i : i+strings.Index(html[i:], ">")]
	for _, ev := range []string{"sse:agent.changed from:body", "sse:chat.reply from:body", "agents:changed from:body"} {
		if !strings.Contains(list, ev) {
			t.Errorf("#agents-list does not re-render on %q", ev)
		}
	}
	slot := renderNode(t, agentRecentSlot(AgentCardView{ID: 4, LazyRecent: true}))
	if strings.Contains(slot, "agents:changed") {
		t.Error("the recent-log slot still listens to agents:changed directly, racing the list's morph")
	}
	if !strings.Contains(slot, recentRefreshEvent) || !strings.Contains(list, recentRefreshEvent) {
		t.Errorf("the list does not refresh the slot after it settles (%s)", recentRefreshEvent)
	}
}
