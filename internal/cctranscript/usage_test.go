package cctranscript

import (
	"reflect"
	"testing"
)

// The record shapes are the CLI's (2.1.289, as in cmd/ccd/testdata) with
// invented numbers; the four counters are pairwise distinct so a sum that
// drops or doubles one cannot match.
func TestEventsDecodesContextUsage(t *testing.T) {
	const usage = `"usage":{"input_tokens":3,"cache_creation_input_tokens":500,"cache_read_input_tokens":120000,"output_tokens":7000}`
	cases := []struct {
		name, line string
		want       []Event
	}{
		{"a text turn: input + cache read + cache creation, output excluded",
			`{"type":"assistant","message":{"model":"claude-opus-5-5","content":[{"type":"text","text":"hi"}],` + usage + `}}`,
			[]Event{{Kind: KindText, Text: "hi", Usage: &Usage{Model: "claude-opus-5-5", Tokens: 120503}}}},
		{"a tool call carries it the same",
			`{"type":"assistant","message":{"model":"claude-haiku-4-5","content":[{"type":"tool_use","id":"t","name":"Bash","input":{"command":"ls"}}],` + usage + `}}`,
			[]Event{{Kind: KindToolCall, ToolID: "t", Tool: "Bash", Target: "ls", Input: `{"command":"ls"}`, Usage: &Usage{Model: "claude-haiku-4-5", Tokens: 120503}}}},
		{"a synthetic record (the CLI's own message) has none",
			`{"type":"assistant","message":{"model":"<synthetic>","content":[{"type":"text","text":"x"}],` + usage + `}}`,
			[]Event{{Kind: KindText, Text: "x"}}},
		{"zero tokens is none",
			`{"type":"assistant","message":{"model":"claude-opus-5-5","content":[{"type":"text","text":"x"}],"usage":{"input_tokens":0,"output_tokens":0}}}`,
			[]Event{{Kind: KindText, Text: "x"}}},
		{"an API failure has none",
			`{"type":"assistant","isApiErrorMessage":true,"message":{"model":"claude-opus-5-5","content":[{"type":"text","text":"rate limited"}],` + usage + `}}`,
			[]Event{{Kind: KindError, Text: "rate limited"}}},
		{"a subagent's turn is not this conversation's context",
			`{"type":"assistant","isSidechain":true,"message":{"model":"claude-opus-5-5","content":[{"type":"text","text":"x"}],` + usage + `}}`,
			nil},
		{"the session's model attachment says the window",
			`{"type":"attachment","attachment":{"type":"model","identity":{"modelId":"claude-opus-5-5[1m]","marketingName":"Example"},"text":"…"}}`,
			[]Event{{Kind: KindModel, Model: "claude-opus-5-5[1m]"}}},
		{"any other attachment is nothing",
			`{"type":"attachment","attachment":{"type":"skill_listing","content":"…"}}`,
			nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Events(rec(t, c.line)); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("events:\n got %+v\nwant %+v", got, c.want)
			}
		})
	}
}

// Usage rides on the FIRST event of a multi-block record and on no other.
func TestUsageIsOnTheFirstEventOnly(t *testing.T) {
	got := Events(rec(t, `{"type":"assistant","message":{"model":"m","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}],"usage":{"input_tokens":5}}}`))
	if len(got) != 2 || got[0].Usage == nil || got[0].Usage.Tokens != 5 || got[1].Usage != nil {
		t.Fatalf("events: %+v", got)
	}
}

func TestContextWindow(t *testing.T) {
	cases := []struct {
		model string
		want  int64
		known bool
	}{
		{"claude-opus-5-5[1m]", 1_000_000, true},
		{"claude-sonnet-5[1m]", 1_000_000, true},
		{"claude-haiku-4-5", 200_000, true},
		{"claude-haiku-4-5-20251001", 200_000, true},
		// 🔴 A model with a 1M variant and no marker: NOT guessed.
		{"claude-opus-5-5", 0, false},
		{"claude-opus-5", 0, false},
		{"claude-sonnet-5-5", 0, false},
		{"claude-fable-5-1", 0, false},
		{"", 0, false},
		{"some-other-model", 0, false},
	}
	for _, c := range cases {
		if got, ok := ContextWindow(c.model); got != c.want || ok != c.known {
			t.Errorf("ContextWindow(%q) = %d, %v; want %d, %v", c.model, got, ok, c.want, c.known)
		}
	}
	if BaseModel("claude-opus-5-5[1m]") != "claude-opus-5-5" || BaseModel("claude-haiku-4-5") != "claude-haiku-4-5" {
		t.Error("BaseModel")
	}
}
