package cctranscript

import (
	"strings"
	"testing"
)

// AskUserQuestion's records, in the shapes Claude Code 2.1.289 writes them
// (measured in a scratch session): the call's input carries the questions; the
// user record that carries its result carries toolUseResult {questions,
// answers} beside the tool_result block — an object when it was answered, a
// string when the picker was dismissed.

const askInput = `{"questions":[` +
	`{"question":"Which color do you prefer?","header":"Color","options":[{"label":"Red","description":"` + longDesc + `"},{"label":"Blue","description":"` + longDesc + `"},{"label":"Green","description":"` + longDesc + `"}],"multiSelect":false},` +
	`{"question":"Which pets do you have?","header":"Pets","options":[{"label":"Cat","description":"` + longDesc + `"},{"label":"Dog","description":"` + longDesc + `"},{"label":"Fish","description":"` + longDesc + `"}],"multiSelect":true}]}`

// longDesc makes the input bigger than MaxInput, as a real question set with
// descriptions is.
const longDesc = "a description long enough that six of them push the call's input past the rendered view's input cap, which is what a real question set with explanatory descriptions does, often with a second sentence on the trade-off it implies for the work that follows, and a third naming what it rules out so the choice is an informed one"

func decode(t *testing.T, line string) Record {
	t.Helper()
	r, ok := DecodeLine([]byte(line))
	if !ok {
		t.Fatalf("not a record: %s", line)
	}
	return r
}

// TestAnAskCallCarriesItsQuestionsPastTheInputCap: the call's Input is capped
// (a real question set's is cut to "…"), so the questions ride on Ask, whole.
func TestAnAskCallCarriesItsQuestionsPastTheInputCap(t *testing.T) {
	if len(askInput) <= MaxInput {
		t.Fatalf("the fixture is %d bytes, not past MaxInput (%d): it tests nothing", len(askInput), MaxInput)
	}
	evs := Events(decode(t, `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_A","name":"AskUserQuestion","input":`+askInput+`}]}}`))
	if len(evs) != 1 || evs[0].Kind != KindToolCall {
		t.Fatalf("events %+v", evs)
	}
	if strings.Contains(evs[0].Input, "Which pets") {
		t.Fatalf("the capped Input still holds the questions — the fixture does not exercise the cap: %s", evs[0].Input)
	}
	a := evs[0].Ask
	if a == nil || len(a.Questions) != 2 || a.Questions[1].Question != "Which pets do you have?" || !a.Questions[1].MultiSelect ||
		len(a.Questions[0].Options) != 3 || a.Questions[0].Options[2].Description != longDesc || a.Answers != nil {
		t.Fatalf("Ask %+v", a)
	}
	// Another tool's call carries none, whatever its input says.
	if evs := Events(decode(t, `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_B","name":"Bash","input":`+askInput+`}]}}`)); evs[0].Ask != nil {
		t.Fatalf("a Bash call carries Ask: %+v", evs[0].Ask)
	}
}

func TestAnAskResultCarriesTheAnswersFromToolUseResult(t *testing.T) {
	line := `{"type":"user","promptId":"p","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_A","content":"Your questions have been answered: \"Which color do you prefer?\"=\"teal\". You can now continue with these answers in mind."}]},` +
		`"toolUseResult":{"questions":[{"question":"Which color do you prefer?","header":"Color","options":[{"label":"Red","description":"Red"},{"label":"Blue","description":"Blue"}],"multiSelect":false}],"answers":{"Which color do you prefer?":"teal"}}}`
	evs := Events(decode(t, line))
	if len(evs) != 1 || evs[0].Kind != KindToolResult || evs[0].ToolID != "toolu_A" {
		t.Fatalf("events %+v", evs)
	}
	if a := evs[0].Ask; a == nil || a.Answers["Which color do you prefer?"] != "teal" || len(a.Questions) != 1 {
		t.Fatalf("Ask %+v", evs[0].Ask)
	}
	// A dismissed picker: toolUseResult is a string — no Ask, the result as is.
	dismissed := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_A","content":"The user doesn't want to proceed","is_error":true}]},"toolUseResult":"User rejected tool use"}`
	if evs := Events(decode(t, dismissed)); len(evs) != 1 || evs[0].Ask != nil {
		t.Fatalf("a dismissed picker: %+v", evs)
	}
	// Two results in one record: toolUseResult cannot be told apart, so neither
	// takes it.
	two := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"a","content":"x"},{"type":"tool_result","tool_use_id":"b","content":"y"}]},` +
		`"toolUseResult":{"questions":[{"question":"Q?","options":[{"label":"A"},{"label":"B"}]}],"answers":{"Q?":"A"}}}`
	if evs := Events(decode(t, two)); len(evs) != 2 || evs[0].Ask != nil || evs[1].Ask != nil {
		t.Fatalf("two results: %+v", evs)
	}
}
