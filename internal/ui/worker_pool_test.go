package ui

import (
	stdhtml "html"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// workerPoolTag is the rendered worker_pool <input> tag, or "".
func workerPoolTag(html string) string {
	i := strings.Index(html, `<input id="worker-pool"`)
	if i < 0 {
		return ""
	}
	return html[i : i+strings.Index(html[i:], ">")+1]
}

// TestBothDispatchFormsOfferAWorkerPoolOnlyForClaudeCode: the FAB form and the
// task-scoped confirm both carry the field, inside a claude-code-only container,
// rendered disabled (the kind toggle enables it), with the server's pattern; a
// deployment without claude-code renders none.
func TestBothDispatchFormsOfferAWorkerPoolOnlyForClaudeCode(t *testing.T) {
	for name, html := range map[string]string{
		"fab":  renderString(t, DispatchModalBodyFor(bothKinds)),
		"task": renderString(t, DispatchModalTaskBody(TaskDispatchView{NoteID: 7, Kinds: bothKinds})),
	} {
		tag := workerPoolTag(html)
		if tag == "" {
			t.Fatalf("%s: no worker pool input", name)
		}
		for _, want := range []string{`name="worker_pool"`, " disabled"} {
			if !strings.Contains(tag, want) {
				t.Errorf("%s: worker pool input lacks %s: %s", name, want, tag)
			}
		}
		// The server is the one place the name is checked (D1): no client copy.
		for _, absent := range []string{"pattern=", "maxlength=", "title="} {
			if strings.Contains(tag, absent) {
				t.Errorf("%s: worker pool input carries %s, a client-side copy of the server's rule: %s", name, absent, tag)
			}
		}
		// Its container is one the toggle shows only for claude-code.
		at := strings.Index(html, `<input id="worker-pool"`)
		open := strings.LastIndex(html[:at], "<div data-cc-only")
		if open < 0 || strings.Contains(html[open:at], "</div>") {
			t.Errorf("%s: the worker pool input is not inside a data-cc-only container", name)
		}
		if !strings.Contains(html, stdhtml.EscapeString(workerPoolHelp)) {
			t.Errorf("%s: the field's explanation is missing", name)
		}
		// The toggle is what enables it for claude-code (and disables it again).
		if !strings.Contains(html, "input[name=worker_pool]") || !strings.Contains(kindToggleScript, "p.disabled=!cc") {
			t.Errorf("%s: the kind toggle does not manage the worker pool input", name)
		}
	}
	for _, k := range []KindChoices{{}, {Kinds: []string{agents.KindGateway}}} {
		if html := renderString(t, DispatchModalBodyFor(k)); strings.Contains(html, `name="worker_pool"`) {
			t.Errorf("kinds %v: a form without claude-code offers a worker pool", k.Kinds)
		}
	}
}

// TestAPoolWorkersCardAndPageShowItsPool: the card and the detail page carry
// the pool chip; an agent in no pool carries none.
func TestAPoolWorkersCardAndPageShowItsPool(t *testing.T) {
	card := AgentCardView{ID: 3, Name: "keen-wren", Kind: agents.KindClaudeCode, CCAccount: "alpha"}
	if html := renderString(t, agentCard(card)); strings.Contains(html, "data-worker-pool") {
		t.Fatalf("a card in no pool shows a pool chip")
	}
	card.WorkerPool = "reviewers"
	if html := renderString(t, agentCard(card)); !strings.Contains(html, `data-worker-pool="reviewers"`) {
		t.Fatalf("a pool worker's card has no pool chip:\n%s", html)
	}
	page := AgentDetailView{ID: 3, Name: "keen-wren", Kind: agents.KindClaudeCode, CCAccount: "alpha"}
	if html := renderString(t, AgentDetailPage(page, Features{})); strings.Contains(html, "data-worker-pool") {
		t.Fatalf("a detail page in no pool shows a pool chip")
	}
	page.WorkerPool = "reviewers"
	if html := renderString(t, AgentDetailPage(page, Features{})); !strings.Contains(html, `data-worker-pool="reviewers"`) {
		t.Fatalf("a pool worker's detail page has no pool chip")
	}
}
