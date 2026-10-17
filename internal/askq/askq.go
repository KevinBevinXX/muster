// Package askq is Claude Code's AskUserQuestion tool as muster answers it: the
// question set's shape and bounds, the answers a person may give to it, and the
// PreToolUse hook output that hands those answers to the CLI.
//
// It is SURFACE-NEUTRAL. Nothing here knows about mirrored hosts or dispatched
// agents: a pending question is keyed by (owner, source, scope, Claude session,
// tool_use id) — Key — and the mirror plugin's hook (`muster-mirror ask`) is
// only the first source. A later `ccd ask` reuses the same store and the same
// payload.
//
// THE PAYLOAD IS MEASURED, NOT ASSUMED (Claude Code 2.1.289, an interactive
// session in a scratch directory, PR "ask-user-question"): a PreToolUse hook on
// AskUserQuestion that prints
//
//	{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow",
//	 "updatedInput":{"questions":<the call's own questions>,"answers":{"<question text>":"<answer>"}}}}
//
// answers the call with no picker on screen, in both the default and the auto
// permission modes; the model's tool result reads `Your questions have been
// answered: "Q"="A", …`. A multi-select answer is its labels joined by ", " (the
// CLI's own schema: "multi-select answers are comma-separated"), and an answer
// that is none of the options (the picker's "Type something") is passed as
// typed. A hook that prints nothing leaves the picker on screen — which is how
// every failure here falls back to the terminal (HookOutput is printed only for
// answers a person gave).
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
}

// Question is one question of an AskUserQuestion call.
type Question struct {
	Question    string   `json:"question"`
	Header      string   `json:"header,omitempty"`
	Options     []Option `json:"options"`
	MultiSelect bool     `json:"multiSelect,omitempty"`
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

// FromForm builds the answers to qs from a submitted form: for question i (from
// 0), the values a<i> are the chosen option labels — or OtherValue, whose text
// is o<i> — and o<i> is the person's own answer.
//
// Every question must be answered. A single-select question takes exactly one
// choice; a multi-select one at least one, its answer being the chosen labels in
// the question's own order, joined by MultiJoin, then the person's own text. A
// field naming a question qs does not have, a label that is not one of the
// question's options, a repeated choice, OtherValue with no text, and text past
// MaxFreeText are refused.
func FromForm(qs []Question, form url.Values) (map[string]string, error) {
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
			return nil, invalid("the answer names question %d, which this ask does not have", i+1)
		}
	}
	answers := make(map[string]string, len(qs))
	for i, q := range qs {
		idx := strconv.Itoa(i)
		picks := form["a"+idx]
		other := strings.TrimSpace(strings.ReplaceAll(form.Get("o"+idx), "\r\n", "\n"))
		chosen := map[string]bool{}
		otherOn := false
		for _, p := range picks {
			if chosen[p] || (p == OtherValue && otherOn) {
				return nil, invalid("question %d: %q is chosen twice", i+1, p)
			}
			if p == OtherValue {
				otherOn = true
				continue
			}
			if !q.hasLabel(p) {
				return nil, invalid("question %d: %q is not one of its options", i+1, p)
			}
			chosen[p] = true
		}
		n := len(chosen)
		if otherOn {
			n++
			if other == "" {
				return nil, invalid("question %d: type your own answer, or choose an option", i+1)
			}
			if len(other) > MaxFreeText || !textOK(other) {
				return nil, invalid("question %d: your own answer must be at most %d bytes of text", i+1, MaxFreeText)
			}
		}
		switch {
		case n == 0:
			return nil, invalid("question %d is not answered", i+1)
		case !q.MultiSelect && n > 1:
			return nil, invalid("question %d takes one answer", i+1)
		}
		var parts []string
		for _, o := range q.Options {
			if chosen[o.Label] {
				parts = append(parts, o.Label)
			}
		}
		if otherOn {
			parts = append(parts, other)
		}
		a := strings.Join(parts, MultiJoin)
		if len(a) > MaxAnswer {
			return nil, invalid("question %d's answer is longer than %d bytes", i+1, MaxAnswer)
		}
		answers[q.Question] = a
	}
	return answers, nil
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
// characters. It is what the hook checks before it hands anything to the CLI.
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

// HookOutput is the PreToolUse hook's stdout that answers the call whose input
// was toolInput: the input itself, every field kept, with `answers` set — the
// shape measured against the CLI (see the package doc).
func HookOutput(toolInput json.RawMessage, answers map[string]string) ([]byte, error) {
	var in map[string]json.RawMessage
	if err := json.Unmarshal(toolInput, &in); err != nil || in == nil {
		return nil, invalid("the tool input is not a JSON object")
	}
	a, err := json.Marshal(answers)
	if err != nil {
		return nil, err
	}
	in["answers"] = a
	return json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":      "PreToolUse",
		"permissionDecision": "allow",
		"updatedInput":       in,
	}})
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
