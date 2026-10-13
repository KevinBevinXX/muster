package ui

import (
	"bytes"
	"strings"
	"testing"
)

// Claude Code issues PARALLEL calls before any result: call A, call B, result A,
// result B. Each call must pair with its own result by id — two rows, each with
// its status — never four rows (the strictly-adjacent pairing drew A unpaired
// "running", then B wrongly unpaired, then two orphan results).
func TestParallelToolCallsPairWithTheirOwnResults(t *testing.T) {
	v := AgentDetailView{Name: "a", Status: "running", Messages: []ChatLine{
		{Role: "user", Content: "go"},
		{Role: "assistant", Kind: "tool_call", ToolID: "A", ToolName: "Bash", Content: `{"command":"make docs"}`},
		{Role: "assistant", Kind: "tool_call", ToolID: "B", ToolName: "Read", Content: `{"file_path":"/srv/x.txt"}`},
		{Role: "assistant", Kind: "tool_result", ToolID: "A", ToolName: "Bash", ToolOK: true, Content: "built"},
		{Role: "assistant", Kind: "tool_result", ToolID: "B", ToolName: "Read", ToolOK: false, Content: "missing"},
	}}
	var b bytes.Buffer
	if err := chatLogInner(v).Render(&b); err != nil {
		t.Fatal(err)
	}
	html := b.String()
	// <details> counts rows however they are marked, so this half also reads
	// the same against the pre-pairing renderer (which drew four).
	if n := strings.Count(html, "<details"); n != 2 {
		t.Fatalf("%d rows (<details>), want 2:\n%s", n, html)
	}
	if n := strings.Count(html, "data-tool-row"); n != 2 {
		t.Fatalf("%d tool rows, want 2:\n%s", n, html)
	}
	rows := strings.Split(html, "data-tool-row")[1:]
	for i, want := range []struct{ status, target, out string }{{"ok", "make docs", "built"}, {"error", "/srv/x.txt", "missing"}} {
		r := rows[i]
		if !strings.Contains(r, `data-tool-status="`+want.status+`"`) || !strings.Contains(r, want.target) || !strings.Contains(r, want.out) {
			t.Errorf("row %d lacks status %s / target %s / output %s:\n%s", i, want.status, want.target, want.out, r)
		}
	}
	if strings.Contains(html, `data-tool-status="running"`) {
		t.Error("a call whose result exists is drawn running")
	}
}

// A call with no result yet is running; a result whose call is not in the
// transcript still draws (as its own row).
func TestAnUnfinishedCallIsRunningAndAnOrphanResultStillDraws(t *testing.T) {
	v := AgentDetailView{Name: "a", Status: "running", Messages: []ChatLine{
		{Role: "assistant", Kind: "tool_result", ToolID: "gone", ToolOK: true, Content: "orphan out"},
		{Role: "assistant", Kind: "tool_call", ToolID: "C", ToolName: "Grep", Content: `{"pattern":"TODO"}`},
	}}
	var b bytes.Buffer
	_ = chatLogInner(v).Render(&b)
	html := b.String()
	if strings.Count(html, "data-tool-row") != 2 || !strings.Contains(html, "orphan out") ||
		!strings.Contains(html, `data-tool-status="running"`) || !strings.Contains(html, ">TODO<") {
		t.Fatalf("got:\n%s", html)
	}
}
