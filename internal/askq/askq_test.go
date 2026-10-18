package askq

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
)

// The testdata payload was captured from a real Claude Code 2.1.289 session
// (interactive, a scratch directory, a PreToolUse hook on AskUserQuestion; the
// claude config directory in transcript_path replaced).

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
	cs, got, err := FromForm(qs, form("a0", "Red", "a1", "Fish", "a1", "Cat"))
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"Which color do you prefer?": "Red", "Which pets do you have?": "Cat, Fish"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
	// The same answers as the picker takes them: option indexes in the
	// question's order.
	if want := []Choice{{Picks: []int{0}}, {Picks: []int{0, 2}}}; !reflect.DeepEqual(cs, want) {
		t.Fatalf("choices %+v", cs)
	}
	cs, got, err = FromForm(qs, form("a0", OtherValue, "o0", "  teal, mostly  ", "a1", "Dog", "a1", OtherValue, "o1", "a hamster"))
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"Which color do you prefer?": "teal, mostly", "Which pets do you have?": "Dog, a hamster"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
	if want := []Choice{{Other: "teal, mostly"}, {Picks: []int{1}, Other: "a hamster"}}; !reflect.DeepEqual(cs, want) {
		t.Fatalf("choices %+v", cs)
	}
	// Text typed beside a chosen option is not the answer unless Other is chosen.
	_, got, err = FromForm(qs, form("a0", "Blue", "o0", "ignored", "a1", "Cat"))
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
		// Typed into the picker's one-line text row: no line break, no tab.
		"other text on two lines": form("a0", OtherValue, "o0", "two\nlines", "a1", "Cat"),
		"other text with a tab":   form("a0", OtherValue, "o0", "a\tb", "a1", "Cat"),
	}
	for name, v := range cases {
		if _, _, err := FromForm(qs, v); !errors.Is(err, ErrInvalid) {
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
