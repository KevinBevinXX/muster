package ui

import (
	"regexp"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/cctranscript"
)

var askQs = []askq.Question{
	{Question: "Which color do you prefer?", Header: "Color", Options: []askq.Option{{Label: "Red", Description: "warm"}, {Label: "Blue", Description: "cool"}}},
	{Question: "Which pets do you have?", Header: "Pets", Options: []askq.Option{{Label: "Cat"}, {Label: "Dog"}, {Label: "Fish"}}, MultiSelect: true},
	{Question: "What should the project be called?", Header: "Name", Options: []askq.Option{{Label: "Alpha"}, {Label: "Beta"}}},
}

// TestAnAskCallIsACardNotAToolRow: the conversation draws an AskUserQuestion
// call as a question card — never a generic tool row, never folded into a
// tool group — and its result redraws THAT card, read-only, with the answers.
func TestAnAskCallIsACardNotAToolRow(t *testing.T) {
	var tr MirrorTranscript
	ops := tr.Ops([]cctranscript.Event{
		{Kind: cctranscript.KindToolCall, ToolID: "t0", Tool: "Read", Target: "a.go"},
		{Kind: cctranscript.KindToolCall, ToolID: "toolu_Q", Tool: askq.ToolName, Input: `{"questions":"…"}`, Ask: &cctranscript.Ask{Questions: askQs}},
		{Kind: cctranscript.KindToolCall, ToolID: "t1", Tool: "Read", Target: "b.go"},
	})
	if len(ops) != 3 || ops[1].Op != "append" || ops[1].ID != toolRowID("toolu_Q") || ops[1].Into != "" {
		t.Fatalf("ops %+v", ops)
	}
	card := ops[1].HTML
	for _, want := range []string{`data-ask=""`, `data-ask-state="waiting"`, "Which color do you prefer?", "Choose any number", "warm", "Waiting for the answer in the terminal."} {
		if !strings.Contains(card, want) {
			t.Errorf("the call card lacks %q:\n%s", want, card)
		}
	}
	if strings.Contains(card, "data-tool-row") {
		t.Fatalf("the ask call was drawn as a tool row:\n%s", card)
	}
	// t0 and t1 are not one run: the card between them ended it.
	if ops[2].Op != "append" || ops[2].Into != "" {
		t.Fatalf("the call after the card joined a group: %+v", ops[2])
	}
	res := tr.Ops([]cctranscript.Event{{Kind: cctranscript.KindToolResult, ToolID: "toolu_Q", OK: true, Output: "Your questions have been answered",
		Ask: &cctranscript.Ask{Questions: askQs, Answers: map[string]string{"Which color do you prefer?": "Red", "Which pets do you have?": "Cat, Fish", "What should the project be called?": "my own <b>name</b>"}}}})
	if len(res) != 1 || res[0].Op != "replace" || res[0].ID != toolRowID("toolu_Q") {
		t.Fatalf("result ops %+v", res)
	}
	got := res[0].HTML
	for _, want := range []string{`data-ask-state="history"`, ">answered<", "Cat, Fish", "my own &lt;b&gt;name&lt;/b&gt;"} {
		if !strings.Contains(got, want) {
			t.Errorf("the answered card lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "<form") || strings.Contains(got, "<input") {
		t.Fatalf("a historical card is answerable:\n%s", got)
	}
}

// TestAnAskResultAloneDrawsTheCard: a socket that never saw the call (a
// backfill window, a resume) draws the card from the result's own
// toolUseResult; a resumed one replaces the row the page already has.
func TestAnAskResultAloneDrawsTheCard(t *testing.T) {
	ev := cctranscript.Event{Kind: cctranscript.KindToolResult, ToolID: "toolu_Q", OK: true,
		Ask: &cctranscript.Ask{Questions: askQs[:1], Answers: map[string]string{"Which color do you prefer?": "Blue"}}}
	var fresh MirrorTranscript
	if ops := fresh.Ops([]cctranscript.Event{ev}); len(ops) != 1 || ops[0].Op != "append" || !strings.Contains(ops[0].HTML, "Blue") {
		t.Fatalf("fresh: %+v", ops)
	}
	resumed := MirrorTranscript{Resumed: true}
	if ops := resumed.Ops([]cctranscript.Event{ev}); len(ops) != 1 || ops[0].Op != "replace" || ops[0].ID != toolRowID("toolu_Q") {
		t.Fatalf("resumed: %+v", ops)
	}
	// A dismissed picker: the call's questions, no answer, the result's text.
	var tr MirrorTranscript
	tr.Ops([]cctranscript.Event{{Kind: cctranscript.KindToolCall, ToolID: "toolu_D", Tool: askq.ToolName, Ask: &cctranscript.Ask{Questions: askQs[:1]}}})
	ops := tr.Ops([]cctranscript.Event{{Kind: cctranscript.KindToolResult, ToolID: "toolu_D", Output: "The user doesn't want to proceed"}})
	if len(ops) != 1 || !strings.Contains(ops[0].HTML, "not answered") || !strings.Contains(ops[0].HTML, "doesn&#39;t want to proceed") {
		t.Fatalf("dismissed: %+v", ops)
	}
}

// TestThePendingFormOffersEveryQuestionsChoicesAndOther: radio for a
// single-select question, checkboxes for a multi-select one, each option's
// description, an Other choice + text box per question, the call's address
// in hidden fields, one Submit — and every control labelled.
func TestThePendingFormOffersEveryQuestionsChoicesAndOther(t *testing.T) {
	html := render(MirrorAskLive(MirrorAskLiveView{Ref: MirrorRef{Host: "lap", Session: "work", Window: "@3"}, ToolID: "toolu_Q", State: AskStatePending, Questions: askQs}))
	count := func(re string) int { return len(regexp.MustCompile(re).FindAllString(html, -1)) }
	if n := count(`type="radio" id="[^"]+" name="a0"`); n != 3 {
		t.Errorf("question 1 has %d radios, want 2 options + Other", n)
	}
	if n := count(`type="checkbox" id="[^"]+" name="a1"`); n != 4 {
		t.Errorf("question 2 has %d checkboxes, want 3 options + Other", n)
	}
	if n := count(`type="radio" id="[^"]+" name="a2"`); n != 3 {
		t.Errorf("question 3 has %d radios", n)
	}
	if n := count(`value="` + askq.OtherValue + `"`); n != 3 {
		t.Errorf("%d Other choices, want one per question", n)
	}
	if n := count(`<textarea name="o[0-2]"`); n != 3 {
		t.Errorf("%d free-text boxes, want one per question", n)
	}
	if n := count(`<fieldset`); n != 3 {
		t.Errorf("%d fieldsets", n)
	}
	if n := count(`type="submit"`); n != 1 {
		t.Errorf("%d submits, want ONE for every question", n)
	}
	for _, want := range []string{`name="host" value="lap"`, `name="session" value="work"`, `name="window" value="@3"`, `name="tool" value="toolu_Q"`,
		`action="` + MirrorAskPath + `"`, "warm", "cool", `aria-label="Your own answer: Which pets do you have?"`, `data-ask-tool="toolu_Q"`,
		`data-ask-row="` + toolRowID("toolu_Q") + `"`, `data-ask-state="pending"`} {
		if !strings.Contains(html, want) {
			t.Errorf("the form lacks %s", want)
		}
	}
	// Every input that is not hidden has a <label for> naming it.
	for _, m := range regexp.MustCompile(`<input type="(?:radio|checkbox)" id="([^"]+)"`).FindAllStringSubmatch(html, -1) {
		if !strings.Contains(html, `for="`+m[1]+`"`) {
			t.Errorf("input %s has no label", m[1])
		}
	}
}

// TestTheLiveCardSaysHowItWent: every live state, its words, and that only a
// pending, answerable question has a form.
func TestTheLiveCardSaysHowItWent(t *testing.T) {
	ref := MirrorRef{Host: "lap", Session: "work", Window: "@3"}
	red := map[string]string{"Which color do you prefer?": "Red"}
	terminalLink := `href="/mirror/terminal?host=lap&amp;session=work&amp;window=%403"`
	for _, c := range []struct {
		v            MirrorAskLiveView
		want         []string
		form, linked bool
	}{
		{MirrorAskLiveView{State: AskStatePending}, []string{"whichever is answered first counts"}, true, false},
		{MirrorAskLiveView{State: AskStatePending, Unanswerable: MirrorReadOnlyNote}, []string{"read-only", "The terminal shows this question too."}, false, true},
		{MirrorAskLiveView{State: AskStateTyping, Answers: red}, []string{"Typing your answer into the terminal", "Answer: </span>Red"}, false, false},
		{MirrorAskLiveView{State: AskStateFailed, Note: "Someone has started answering it in the terminal — finish it there."}, []string{"Someone has started answering"}, false, true},
		{MirrorAskLiveView{State: AskStateAnswered, Answers: red}, []string{"Answered here", "Answer: </span>Red"}, false, false},
		{MirrorAskLiveView{State: AskStateAnsweredTerminal, Answers: red}, []string{"Answered in the terminal", "Answer: </span>Red"}, false, false},
		{MirrorAskLiveView{State: AskStateDeclined}, []string{"Dismissed in the terminal"}, false, false},
		{MirrorAskLiveView{State: AskStateExpired}, []string{"No answer was seen"}, false, true},
	} {
		c.v.Ref, c.v.ToolID, c.v.Questions = ref, "toolu_Q", askQs[:1]
		html := render(MirrorAskLive(c.v))
		for _, w := range c.want {
			if !strings.Contains(html, w) {
				t.Errorf("%s: lacks %q:\n%s", c.v.State, w, html)
			}
		}
		if strings.Contains(html, "<form") != c.form {
			t.Errorf("%s: has a form = %v, want %v", c.v.State, !c.form, c.form)
		}
		if strings.Contains(html, terminalLink) != c.linked {
			t.Errorf("%s: links the terminal = %v, want %v", c.v.State, !c.linked, c.linked)
		}
		if !strings.Contains(html, `data-ask-state="`+c.v.State+`"`) {
			t.Errorf("%s: the card does not carry its state", c.v.State)
		}
	}
}

// TestTheRenderedViewHasTheLiveRegion: the page carries #mv-asks (hidden
// until the poll fills it) pointed at this window, and the script that runs it.
func TestTheRenderedViewHasTheLiveRegion(t *testing.T) {
	html := render(MirrorViewPage(MirrorViewView{Host: "lap", Session: "work", Window: "@3", Interactive: true, StepUp: &StepUpView{ConfirmPath: "/auth/confirm"}}))
	if !strings.Contains(html, `id="mv-asks" data-src="/mirror/view/asks?host=lap&amp;session=work&amp;window=%403"`) {
		t.Fatalf("no live region for this window")
	}
	if !strings.Contains(html, "form[data-ask-form]") {
		t.Fatalf("no ask script")
	}
}
