package askq

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
)

// The testdata payloads were captured from a real Claude Code 2.1.289 session
// (interactive, a scratch directory, a PreToolUse hook on AskUserQuestion; the
// claude config directory in transcript_path replaced). pretooluse-output-proven.json is
// the hook output that session ACCEPTED: the call was answered with no picker
// on screen, and the model's tool result read `Your questions have been
// answered: "Which color do you prefer?"="Red", "Which pets do you have?"="Cat,
// Dog". …`.

func readPayload(t *testing.T) HookPayload {
	t.Helper()
	b, err := os.ReadFile("testdata/pretooluse-askuserquestion.json")
	if err != nil {
		t.Fatal(err)
	}
	var p HookPayload
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func sameJSON(t *testing.T, got, want []byte) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("got is not JSON: %v\n%s", err, got)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("want is not JSON: %v", err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("JSON differs\n got: %s\nwant: %s", got, want)
	}
}

// TestHookOutputIsTheProvenPayload: the answers a person gave, for the real
// call, produce EXACTLY the output the CLI was measured to accept.
func TestHookOutputIsTheProvenPayload(t *testing.T) {
	p := readPayload(t)
	out, err := HookOutput(p.ToolInput, map[string]string{"Which color do you prefer?": "Red", "Which pets do you have?": "Cat, Dog"})
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/pretooluse-output-proven.json")
	if err != nil {
		t.Fatal(err)
	}
	sameJSON(t, out, bytes.TrimSpace(want))
}

// TestHookOutputKeepsEveryInputField: updatedInput REPLACES the call's input,
// so a field muster does not know (metadata, a preview) must survive.
func TestHookOutputKeepsEveryInputField(t *testing.T) {
	in := json.RawMessage(`{"questions":[{"question":"Q?","header":"H","options":[{"label":"A","preview":"<b>x</b>"},{"label":"B"}]}],"metadata":{"source":"remember"}}`)
	out, err := HookOutput(in, map[string]string{"Q?": "A"})
	if err != nil {
		t.Fatal(err)
	}
	sameJSON(t, out, []byte(`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","updatedInput":{"questions":[{"question":"Q?","header":"H","options":[{"label":"A","preview":"<b>x</b>"},{"label":"B"}]}],"metadata":{"source":"remember"},"answers":{"Q?":"A"}}}}`))
}

func gateQuestions(t *testing.T) []Question {
	t.Helper()
	qs, err := ParseInput(readPayload(t).ToolInput)
	if err != nil {
		t.Fatal(err)
	}
	return qs
}

func TestParseInputReadsTheRealCall(t *testing.T) {
	qs := gateQuestions(t)
	want := []Question{
		{Question: "Which color do you prefer?", Header: "Color", Options: []Option{{"Red", "Red"}, {"Blue", "Blue"}, {"Green", "Green"}}},
		{Question: "Which pets do you have?", Header: "Pets", Options: []Option{{"Cat", "Cat"}, {"Dog", "Dog"}, {"Fish", "Fish"}}, MultiSelect: true},
	}
	if !reflect.DeepEqual(qs, want) {
		t.Fatalf("got %+v", qs)
	}
}

func TestParseQuestionsRefusesWhatMusterWillNotShow(t *testing.T) {
	q := func(s string) string { return `[` + s + `]` }
	opt2 := `"options":[{"label":"A"},{"label":"B"}]`
	cases := map[string]string{
		"not an array":       `{"question":"Q?"}`,
		"none":               `[]`,
		"five questions":     q(strings.Repeat(`{"question":"Q?",`+opt2+`},`, 4) + `{"question":"Q5?",` + opt2 + `}`),
		"empty question":     q(`{"question":" ",` + opt2 + `}`),
		"repeated question":  q(`{"question":"Q?",` + opt2 + `},{"question":"Q?",` + opt2 + `}`),
		"no options":         q(`{"question":"Q?","options":[]}`),
		"seven options":      q(`{"question":"Q?","options":[{"label":"1"},{"label":"2"},{"label":"3"},{"label":"4"},{"label":"5"},{"label":"6"},{"label":"7"}]}`),
		"repeated label":     q(`{"question":"Q?","options":[{"label":"A"},{"label":"A"}]}`),
		"empty label":        q(`{"question":"Q?","options":[{"label":""},{"label":"A"}]}`),
		"reserved label":     q(`{"question":"Q?","options":[{"label":"__other__"},{"label":"A"}]}`),
		"control character":  q(`{"question":"Q\u001b?",` + opt2 + `}`),
		"oversized question": q(`{"question":"` + strings.Repeat("x", MaxQuestion+1) + `",` + opt2 + `}`),
		"oversized label":    q(`{"question":"Q?","options":[{"label":"` + strings.Repeat("x", MaxLabel+1) + `"},{"label":"B"}]}`),
		"text kind":          q(`{"question":"Q?","kind":"text"}`),
	}
	for name, raw := range cases {
		if _, err := ParseQuestions(json.RawMessage(raw)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err %v, want ErrInvalid", name, err)
		}
	}
	if _, err := ParseQuestions(json.RawMessage(q(`{"question":"Q?","kind":"choice",` + opt2 + `}`))); err != nil {
		t.Errorf("an explicit choice question: %v", err)
	}
}

func form(kv ...string) url.Values {
	v := url.Values{}
	for i := 0; i < len(kv); i += 2 {
		v.Add(kv[i], kv[i+1])
	}
	return v
}

// TestFromFormBuildsTheAnswersTheCLITakes: a single-select label, a
// multi-select join in OPTION order (not click order) with ", ", and free text
// passed through as typed.
func TestFromFormBuildsTheAnswersTheCLITakes(t *testing.T) {
	qs := gateQuestions(t)
	got, err := FromForm(qs, form("a0", "Red", "a1", "Fish", "a1", "Cat"))
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"Which color do you prefer?": "Red", "Which pets do you have?": "Cat, Fish"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
	got, err = FromForm(qs, form("a0", OtherValue, "o0", "  teal, mostly  ", "a1", "Dog", "a1", OtherValue, "o1", "a hamster"))
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"Which color do you prefer?": "teal, mostly", "Which pets do you have?": "Dog, a hamster"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
	// Text typed beside a chosen option is not the answer unless Other is chosen.
	got, err = FromForm(qs, form("a0", "Blue", "o0", "ignored", "a1", "Cat"))
	if err != nil || got["Which color do you prefer?"] != "Blue" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestFromFormRefusesWhatTheQuestionsDoNotAllow(t *testing.T) {
	qs := gateQuestions(t)
	cases := map[string]url.Values{
		"unknown question":          form("a0", "Red", "a1", "Cat", "a2", "Red"),
		"unknown question by text":  form("a0", "Red", "a1", "Cat", "o5", "hi"),
		"unanswered":                form("a0", "Red"),
		"not an option":             form("a0", "Purple", "a1", "Cat"),
		"two answers to one":        form("a0", "Red", "a0", "Blue", "a1", "Cat"),
		"label and other to one":    form("a0", "Red", "a0", OtherValue, "o0", "x", "a1", "Cat"),
		"chosen twice":              form("a0", "Red", "a1", "Cat", "a1", "Cat"),
		"other with no text":        form("a0", OtherValue, "a1", "Cat"),
		"other text too long":       form("a0", OtherValue, "o0", strings.Repeat("x", MaxFreeText+1), "a1", "Cat"),
		"other text with a control": form("a0", OtherValue, "o0", "a\x1bb", "a1", "Cat"),
	}
	for name, v := range cases {
		if _, err := FromForm(qs, v); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err %v, want ErrInvalid", name, err)
		}
	}
}

func TestCheckAnswers(t *testing.T) {
	qs := gateQuestions(t)
	ok := map[string]string{"Which color do you prefer?": "Red", "Which pets do you have?": "Cat"}
	if err := CheckAnswers(qs, ok); err != nil {
		t.Fatal(err)
	}
	bad := map[string]map[string]string{
		"missing one": {"Which color do you prefer?": "Red"},
		"unknown key": {"Which color do you prefer?": "Red", "Something else?": "Cat"},
		"extra key":   {"Which color do you prefer?": "Red", "Which pets do you have?": "Cat", "x": "y"},
		"empty":       {"Which color do you prefer?": " ", "Which pets do you have?": "Cat"},
		"too long":    {"Which color do you prefer?": strings.Repeat("x", MaxAnswer+1), "Which pets do you have?": "Cat"},
	}
	for name, a := range bad {
		if err := CheckAnswers(qs, a); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err %v, want ErrInvalid", name, err)
		}
	}
}

func TestParseResultReadsTheTranscriptsToolUseResult(t *testing.T) {
	raw := json.RawMessage(`{"questions":[{"question":"Which color do you prefer?","header":"Color","options":[{"label":"Red","description":"Red"},{"label":"Blue","description":"Blue"}],"multiSelect":false}],"answers":{"Which color do you prefer?":"Red","stray":"x"}}`)
	r, ok := ParseResult(raw)
	if !ok || len(r.Questions) != 1 || !reflect.DeepEqual(r.Answers, map[string]string{"Which color do you prefer?": "Red"}) {
		t.Fatalf("got %+v %v", r, ok)
	}
	for _, s := range []string{``, `"text"`, `{"stdout":"x"}`, `{"questions":[],"answers":{}}`, `{"questions":[{"question":"Q?","options":[{"label":"A"}]}]}`} {
		if _, ok := ParseResult(json.RawMessage(s)); ok {
			t.Errorf("%q was taken for an ask result", s)
		}
	}
}
