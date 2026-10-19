package ui

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/mirror"
)

func agentCall(id, desc, typ string) cctranscript.Event {
	return cctranscript.Event{Kind: cctranscript.KindToolCall, ToolID: id, Tool: "Agent", Target: desc,
		Input: `{"description":"` + desc + `","prompt":"p","subagent_type":"` + typ + `"}`}
}

func opsHTML(ops []MirrorOp) string {
	var b strings.Builder
	for _, o := range ops {
		b.WriteString(o.Op + "|" + o.ID + "|" + o.Name + "=" + o.Value + "|" + o.HTML + "\n")
	}
	return b.String()
}

// 🔴 AN OLDER HOST'S Agent CALL IS THE PLAIN TOOL ROW: no card unless the
// stream said the host reports subagents.
func TestAnAgentCallIsACardOnlyForAHostThatReportsSubagents(t *testing.T) {
	var old MirrorTranscript
	html := opsHTML(old.Ops([]cctranscript.Event{agentCall("toolu_a", "Survey", "Explore")}))
	if strings.Contains(html, "data-subagent-card") || !strings.Contains(html, "data-tool-row") {
		t.Fatalf("an older host's Agent call:\n%s\nwant a plain tool row", html)
	}

	t2 := MirrorTranscript{Subagents: true}
	ops := t2.Ops([]cctranscript.Event{agentCall("toolu_a", "Survey the repo", "Explore"), agentCall("toolu_b", "Second", "general-purpose")})
	if len(ops) != 2 || ops[0].Op != "append" || ops[1].Op != "append" {
		t.Fatalf("two Agent calls: %s\nwant two appended cards, never a group", opsHTML(ops))
	}
	card := ops[0].HTML
	for _, want := range []string{`id="` + toolRowID("toolu_a") + `"`, "data-subagent-card", `data-subagent-state="starting"`,
		">Survey the repo<", ">Explore<", `id="` + toolRowID("toolu_a") + `-s"`, "data-subagent-tail"} {
		if !strings.Contains(card, want) {
			t.Errorf("card lacks %q:\n%s", want, card)
		}
	}
	if strings.Contains(card, "data-tool-row") {
		t.Error("a card is drawn as a tool row (it would be grouped)")
	}
}

// A summary redraws only its card's <summary> and attributes; the same summary
// again draws nothing; a summary before its card is kept for it.
func TestSubagentOps(t *testing.T) {
	tr := MirrorTranscript{Subagents: true}
	// A summary for a call not drawn yet (and this socket is not resumed).
	early := mirror.Subagent{Agent: "a2", ToolID: "toolu_b", State: mirror.SubagentRunning, Type: "Plan", Started: 1_000, Last: "Read: x.go"}
	if ops := tr.SubagentOps([]mirror.Subagent{early}); len(ops) != 0 {
		t.Fatalf("a summary for an undrawn card drew %s", opsHTML(ops))
	}
	tr.Ops([]cctranscript.Event{agentCall("toolu_a", "Survey", "Explore")})
	cardB := tr.Ops([]cctranscript.Event{agentCall("toolu_b", "Later", "Plan")})
	if !strings.Contains(cardB[0].HTML, `data-subagent-state="running"`) || !strings.Contains(cardB[0].HTML, "Read: x.go") ||
		!strings.Contains(cardB[0].HTML, `data-agent="a2"`) {
		t.Fatalf("the card did not take its earlier summary:\n%s", cardB[0].HTML)
	}

	s := mirror.Subagent{Agent: "a1", ToolID: "toolu_a", Type: "Explore", Description: "Survey the repo", State: mirror.SubagentDone,
		Started: 10_000, Updated: 75_000, Last: "Found it."}
	ops := tr.SubagentOps([]mirror.Subagent{s, early})
	id := toolRowID("toolu_a")
	if len(ops) != 3 || ops[0].Op != "patch" || ops[0].ID != id+"-s" ||
		ops[1] != (MirrorOp{Op: "attr", ID: id, Name: "data-subagent-state", Value: "done"}) ||
		ops[2] != (MirrorOp{Op: "attr", ID: id, Name: "data-agent", Value: "a1"}) {
		t.Fatalf("ops:\n%s", opsHTML(ops))
	}
	for _, want := range []string{">done<", "Found it.", `data-started="10000"`, `data-ended="75000"`, ">1m 05s<"} {
		if !strings.Contains(ops[0].HTML, want) {
			t.Errorf("summary lacks %q:\n%s", want, ops[0].HTML)
		}
	}
	if again := tr.SubagentOps([]mirror.Subagent{s, early}); len(again) != 0 {
		t.Fatalf("an unchanged list drew %s", opsHTML(again))
	}

	// The call's result redraws the whole card, keeping the summary's state.
	res := tr.Ops([]cctranscript.Event{{Kind: cctranscript.KindToolResult, ToolID: "toolu_a", OK: true, Output: "the report"}})
	if len(res) != 1 || res[0].Op != "replace" || res[0].ID != id || !strings.Contains(res[0].HTML, "the report") ||
		!strings.Contains(res[0].HTML, `data-subagent-state="done"`) {
		t.Fatalf("result:\n%s", opsHTML(res))
	}

	// A resumed socket redraws a summary for a card an earlier socket drew.
	resumed := MirrorTranscript{Subagents: true, Resumed: true}
	if ops := resumed.SubagentOps([]mirror.Subagent{s}); len(ops) != 3 || !strings.Contains(ops[0].HTML, "Survey the repo") {
		t.Fatalf("resumed:\n%s", opsHTML(ops))
	}
}

// The tail is drawn as text: a step's markup is escaped.
func TestSubagentTailFragment(t *testing.T) {
	html := render(SubagentTailFragment(SubagentTailView{Tail: mirror.SubagentTail{More: true, Steps: []mirror.SubagentStep{
		{Tool: "Bash", Text: "echo <script>alert(1)</script>"}, {Text: "done"}}}}))
	if strings.Contains(html, "<script>") || !strings.Contains(html, "&lt;script&gt;") || strings.Count(html, "data-subagent-step") != 2 ||
		!strings.Contains(html, "Earlier steps not shown.") {
		t.Fatalf("tail:\n%s", html)
	}
	if html := render(SubagentTailFragment(SubagentTailView{Note: "The host is offline."})); !strings.Contains(html, "The host is offline.") {
		t.Fatalf("note:\n%s", html)
	}
	if html := render(SubagentTailFragment(SubagentTailView{})); !strings.Contains(html, "No steps yet.") {
		t.Fatalf("empty:\n%s", html)
	}
}

func TestSubagentElapsed(t *testing.T) {
	for ms, want := range map[int64]string{0: "0s", 9_400: "9s", 65_000: "1m 05s", 3_725_000: "1h 02m", -5: "0s"} {
		if got := subagentElapsed(ms); got != want {
			t.Errorf("%d: %q, want %q", ms, got, want)
		}
	}
}
