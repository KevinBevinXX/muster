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
// html — the element itself, to its matching </div> — or "". (end is unused
// since the repository moved out of the container; kept for the call sites.)
func gatewayOnlyBlock(html, _ string) string {
	// Anchored on the ELEMENT, not the bare attribute name: the kind toggle's
	// script text names [data-gateway-only] earlier in the document.
	i := strings.Index(html, "<div "+gatewayOnlyAttr)
	if i < 0 {
		return ""
	}
	depth := 0
	for j := i; j < len(html); {
		open, closing := strings.Index(html[j:], "<div"), strings.Index(html[j:], "</div>")
		if closing < 0 {
			return html[i:]
		}
		if open >= 0 && open < closing {
			depth++
			j += open + len("<div")
			continue
		}
		depth--
		j += closing + len("</div>")
		if depth == 0 {
			return html[i:j]
		}
	}
	return html[i:]
}

// TestEveryOpenRouterControlInTheDispatchFormsIsGatewayOnly pins WHICH controls
// the kind toggle hides, in both dispatch forms: the model selector (the
// OpenRouter-specific one — curated OpenRouter slugs and the live OpenRouter
// search, data-combobox-remote="/api/openrouter/models") — and which it does
// NOT: the repository, which a claude-code agent now takes (its session starts
// in a checkout). The behavioural half — that they actually disappear on a tap — is
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
		for _, want := range []string{`data-combobox-remote="/api/openrouter/models"`, `name="model"`} {
			if !strings.Contains(block, want) {
				t.Errorf("%s: %s is outside the gateway-only container — it stays on screen for a "+
					"Claude Code dispatch", name, want)
			}
		}
		if strings.Contains(block, `name="repo"`) || !strings.Contains(html, `name="repo"`) {
			t.Errorf("%s: the repository must render OUTSIDE the gateway-only container (a claude-code agent takes one)", name)
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
// HX-Trigger.
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
}
