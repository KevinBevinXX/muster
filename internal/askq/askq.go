// Package askq is Claude Code's AskUserQuestion tool as muster shows it: the
// question set's shape and bounds, the answers a person may give to it in
// muster, and the store of the questions sessions are asking.
//
// It is SURFACE-NEUTRAL. Nothing here knows about mirrored hosts or dispatched
// agents: a question is keyed by (owner, source, scope, Claude session,
// tool_use id) — Key — and the mirror plugin's hook (`muster-mirror ask`) is
// only the first source.
//
// A question is shown IN BOTH PLACES AT ONCE (an operator decision): the
// terminal draws its own picker the moment the call is made, and muster draws
// the same question. Whichever is answered first wins. An answer given in
// muster is TYPED into the terminal's picker by the host (internal/askdrive),
// never handed to the CLI any other way; the CLI's own record of the call's
// result (the transcript's toolUseResult) is what says which answer won.
package askq

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ToolName is the tool this package answers.
const ToolName = "AskUserQuestion"

// The bounds of a question set muster will show. The CLI's own schema asks for
// 1-4 questions of 2-4 options; a little slack is allowed on the options, and
// anything past these is refused — the hook then leaves the question to the
// terminal, which shows it whatever its size.
const (
	MaxQuestions   = 4
	MaxOptions     = 6
	MaxHeader      = 128
	MaxQuestion    = 2048
	MaxLabel       = 256
	MaxDescription = 2048
	// MaxPreview bounds an option's preview (Option.Preview).
	MaxPreview = 4096
	// MaxAnswer bounds one question's answer (labels joined, or typed text).
	MaxAnswer = 4096
	// MaxFreeText bounds what a person types as their own answer.
	MaxFreeText = 2048
)

// OtherValue is the form value of a question's "Other" choice: the person's own
// text (field o<i>) is the answer, or part of it.
const OtherValue = "__other__"

// MultiJoin is how a multi-select answer joins its parts.
const MultiJoin = ", "

// Option is one choice of a question.
type Option struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	// Preview is the option's preview text, when the call gave one: the CLI
	// draws it in a box beside the options of a single-select question
	// (Question.Previews).
	Preview string `json:"preview,omitempty"`
}

// Question is one question of an AskUserQuestion call.
type Question struct {
	Question    string   `json:"question"`
	Header      string   `json:"header,omitempty"`
	Options     []Option `json:"options"`
	MultiSelect bool     `json:"multiSelect,omitempty"`
}

// Previews reports whether the terminal draws q as a PREVIEW picker: a
// single-select question any of whose options has a preview. Measured
// (Claude Code 2.1.289): the options are drawn in a column with the cursor's
// preview boxed beside them, there is NO "Type something" row (so no answer of
// one's own), a digit only moves the cursor to its row, and Enter takes the
// option under the cursor. A multi-select question is drawn as usual.
func (q Question) Previews() bool {
	if q.MultiSelect {
		return false
	}
	for _, o := range q.Options {
		if o.Preview != "" {
			return true
		}
	}
	return false
}

// ErrInvalid wraps every refusal of a question set or of answers.
var ErrInvalid = errors.New("askq")

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, a...)...)
}

// textOK reports whether s is valid UTF-8 with no control character but
// newline and tab.
func textOK(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r == '\n' || r == '\t' {
			continue
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return false
		}
	}
	return true
}

// rawQuestion is a question as the call carries it, with the fields that make
// it one muster cannot show (Kind: a form question of another kind).
type rawQuestion struct {
	Question
	Kind string `json:"kind"`
}

// ParseQuestions reads an AskUserQuestion call's `questions` and checks them
// against the bounds: 1..MaxQuestions questions, each with a non-empty text
// unique in the set, a header, and 1..MaxOptions options whose labels are
// non-empty and unique in their question. A question of any kind but a choice
// is refused (there is nothing to pick from).
func ParseQuestions(raw json.RawMessage) ([]Question, error) {
	var rq []rawQuestion
	if err := json.Unmarshal(raw, &rq); err != nil {
		return nil, invalid("questions are not a JSON array of questions: %v", err)
	}
	if len(rq) == 0 || len(rq) > MaxQuestions {
		return nil, invalid("a call has 1-%d questions, not %d", MaxQuestions, len(rq))
	}
	seen := map[string]bool{}
	out := make([]Question, 0, len(rq))
	for i, q := range rq {
		if q.Kind != "" && q.Kind != "choice" {
			return nil, invalid("question %d is a %q question, which muster does not show", i+1, q.Kind)
		}
		switch {
		case strings.TrimSpace(q.Question.Question) == "" || len(q.Question.Question) > MaxQuestion || !textOK(q.Question.Question):
			return nil, invalid("question %d's text must be 1-%d bytes of text", i+1, MaxQuestion)
		case seen[q.Question.Question]:
			return nil, invalid("question %d repeats an earlier question's text", i+1)
		case len(q.Header) > MaxHeader || !textOK(q.Header):
			return nil, invalid("question %d's header must be at most %d bytes of text", i+1, MaxHeader)
		case len(q.Options) == 0 || len(q.Options) > MaxOptions:
			return nil, invalid("question %d has 1-%d options, not %d", i+1, MaxOptions, len(q.Options))
		}
		seen[q.Question.Question] = true
		labels := map[string]bool{}
		for j, o := range q.Options {
			switch {
			case strings.TrimSpace(o.Label) == "" || len(o.Label) > MaxLabel || !textOK(o.Label) || o.Label == OtherValue:
				return nil, invalid("question %d option %d's label must be 1-%d bytes of text", i+1, j+1, MaxLabel)
			case labels[o.Label]:
				return nil, invalid("question %d repeats the option %q", i+1, o.Label)
			case len(o.Description) > MaxDescription || !textOK(o.Description):
				return nil, invalid("question %d option %d's description must be at most %d bytes of text", i+1, j+1, MaxDescription)
			case len(o.Preview) > MaxPreview || !textOK(o.Preview):
				return nil, invalid("question %d option %d's preview must be at most %d bytes of text", i+1, j+1, MaxPreview)
			}
			labels[o.Label] = true
		}
		out = append(out, q.Question)
	}
	return out, nil
}

// ParseInput reads an AskUserQuestion call's whole input (the hook's
// tool_input) and returns its questions.
func ParseInput(input json.RawMessage) ([]Question, error) {
	var in struct {
		Questions json.RawMessage `json:"questions"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, invalid("the input is not a JSON object: %v", err)
	}
	return ParseQuestions(in.Questions)
}

// Choice is one question's answer as the picker takes it: the options chosen
// (0-based indexes into its Options) and the person's own text ("" for none).
type Choice struct {
	Picks []int  `json:"picks,omitempty"`
	Other string `json:"other,omitempty"`
}

// FromForm builds the answers to qs from a submitted form: for question i (from
// 0), the values a<i> are the chosen option labels — or OtherValue, whose text
// is o<i> — and o<i> is the person's own answer. It returns them both as the
// picker takes them (choices) and as the CLI records them (answers, keyed by
// question text).
//
// Every question must be answered. A single-select question takes exactly one
// choice; a multi-select one at least one, its answer being the chosen labels in
// the question's own order, joined by MultiJoin, then the person's own text. A
// field naming a question qs does not have, a label that is not one of the
// question's options, a repeated choice, OtherValue with no text, text past
// MaxFreeText, and text with a line break or a tab (the terminal's picker takes
// one line, and the answer is typed into it) are refused.
func FromForm(qs []Question, form url.Values) ([]Choice, map[string]string, error) {
	for k := range form {
		var n string
		switch {
		case strings.HasPrefix(k, "a"):
			n = k[1:]
		case strings.HasPrefix(k, "o"):
			n = k[1:]
		default:
			continue
		}
		i, err := strconv.Atoi(n)
		if err != nil || strconv.Itoa(i) != n {
			continue
		}
		if i < 0 || i >= len(qs) {
			return nil, nil, invalid("the answer names question %d, which this ask does not have", i+1)
		}
	}
	answers := make(map[string]string, len(qs))
	choices := make([]Choice, 0, len(qs))
	for i, q := range qs {
		idx := strconv.Itoa(i)
		picks := form["a"+idx]
		other := strings.TrimSpace(form.Get("o" + idx))
		chosen := map[string]bool{}
		otherOn := false
		for _, p := range picks {
			if chosen[p] || (p == OtherValue && otherOn) {
				return nil, nil, invalid("question %d: %q is chosen twice", i+1, p)
			}
			if p == OtherValue {
				otherOn = true
				continue
			}
			if !q.hasLabel(p) {
				return nil, nil, invalid("question %d: %q is not one of its options", i+1, p)
			}
			chosen[p] = true
		}
		n := len(chosen)
		if otherOn {
			n++
			switch {
			case q.Previews():
				return nil, nil, invalid("question %d: the terminal's picker has no row for an answer of your own on this question — choose one of its options", i+1)
			case other == "":
				return nil, nil, invalid("question %d: type your own answer, or choose an option", i+1)
			case len(other) > MaxFreeText || !textOK(other):
				return nil, nil, invalid("question %d: your own answer must be at most %d bytes of text", i+1, MaxFreeText)
			case strings.ContainsAny(other, "\n\t"):
				return nil, nil, invalid("question %d: write your own answer on one line — it is typed into the terminal's picker, which takes one", i+1)
			}
		}
		switch {
		case n == 0:
			return nil, nil, invalid("question %d is not answered", i+1)
		case !q.MultiSelect && n > 1:
			return nil, nil, invalid("question %d takes one answer", i+1)
		}
		var parts []string
		var c Choice
		for j, o := range q.Options {
			if chosen[o.Label] {
				parts = append(parts, o.Label)
				c.Picks = append(c.Picks, j)
			}
		}
		if otherOn {
			parts = append(parts, other)
			c.Other = other
		}
		a := strings.Join(parts, MultiJoin)
		if len(a) > MaxAnswer {
			return nil, nil, invalid("question %d's answer is longer than %d bytes", i+1, MaxAnswer)
		}
		answers[q.Question] = a
		choices = append(choices, c)
	}
	return choices, answers, nil
}

func (q Question) hasLabel(l string) bool {
	for _, o := range q.Options {
		if o.Label == l {
			return true
		}
	}
	return false
}

// CheckAnswers verifies answers against qs: exactly one non-empty answer per
// question, keyed by its text, each within MaxAnswer and free of control
// characters.
func CheckAnswers(qs []Question, answers map[string]string) error {
	if len(answers) != len(qs) {
		return invalid("%d answers for %d questions", len(answers), len(qs))
	}
	for i, q := range qs {
		a, ok := answers[q.Question]
		if !ok {
			return invalid("question %d is not answered", i+1)
		}
		if strings.TrimSpace(a) == "" || len(a) > MaxAnswer || !textOK(a) {
			return invalid("question %d's answer must be 1-%d bytes of text", i+1, MaxAnswer)
		}
	}
	return nil
}

// Result is a call's outcome as the transcript records it (the user record's
// toolUseResult): the questions and the answers given.
type Result struct {
	Questions []Question        `json:"questions"`
	Answers   map[string]string `json:"answers"`
}

// ParseResult reads a transcript record's toolUseResult as an AskUserQuestion
// outcome: ok only when it has a valid question set and an answers object.
// Answers are capped, not refused — this is drawing history, not accepting
// input — and answers to questions the set does not have are dropped.
func ParseResult(raw json.RawMessage) (Result, bool) {
	var r struct {
		Questions json.RawMessage   `json:"questions"`
		Answers   map[string]string `json:"answers"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &r) != nil || r.Answers == nil {
		return Result{}, false
	}
	qs, err := ParseQuestions(r.Questions)
	if err != nil {
		return Result{}, false
	}
	out := Result{Questions: qs, Answers: map[string]string{}}
	for _, q := range qs {
		if a, ok := r.Answers[q.Question]; ok {
			out.Answers[q.Question] = capText(a, MaxAnswer)
		}
	}
	return out, true
}

// capText shortens s to at most n bytes on a rune boundary, marking a cut.
func capText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
