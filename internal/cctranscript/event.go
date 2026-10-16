package cctranscript

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// The Event kinds: what the rendered view draws. There is no "thinking" kind,
// by design (see the package doc).
const (
	KindPrompt     = "prompt"
	KindText       = "text"
	KindToolCall   = "tool_call"
	KindToolResult = "tool_result"
	// KindError is an API failure the CLI recorded in place of a reply (rate
	// limit, auth failure); Text is the CLI's own message.
	KindError = "error"
	// KindNotice is a user record the harness wrote, not the human (a
	// background task finishing, a local command's output):
	// notice.go. Notice names its source, Status a task's status, Text its
	// one-line summary and Output the detail shown when the row is expanded.
	KindNotice = "notice"
)

// The size caps. An Event never carries a field past these, so a batch of
// events is bounded by its count, whatever the transcript holds: a 30 MiB tool
// result costs MaxOutput bytes on the wire, not 30 MiB.
const (
	// MaxText bounds a prompt, a reply segment or an error message.
	MaxText = 32 << 10
	// MaxInput bounds a tool call's input JSON (shown when the row is expanded).
	MaxInput = 2 << 10
	// MaxOutput bounds a tool result's output (shown when the row is expanded).
	MaxOutput = 4 << 10
)

// NativeToolItem is the Responses-stream output item type ccd uses to carry a
// tool call the CLI RAN ITSELF (Bash, Read, Edit…) to muster, which draws it as a
// collapsed row (internal/agents.handleResponsesEvent).
//
// 🔴 muster DISPATCHES every function_call in a COMPLETED response as one of
// ITS tools and feeds the output back; a call the CLI already ran must never be
// run again. Two things keep it from that, and the first is the one that
// holds: ccd streams these items as output_item.done events and NEVER puts them
// in response.completed's output, which is the only place muster reads calls to
// dispatch from (mutation-checked: adding them there makes muster dispatch
// them). The distinct type name is the second, so a future reader of the
// completed output could not mistake one for a call either.
const NativeToolItem = "native_tool_call"

// Event is one thing the rendered view draws, in transcript order.
//
// A tool call and its result are TWO events linked by ToolID, because they are
// two records written at different times — a call is on screen (status
// running) for as long as the tool runs, and its result can arrive many
// records later (Claude Code issues parallel calls before any result).
type Event struct {
	Kind string `json:"kind"`
	// Text is a prompt's, a reply segment's or an error's text.
	Text string `json:"text,omitempty"`
	// ToolID links a tool_call to its tool_result.
	ToolID string `json:"toolId,omitempty"`
	// Tool is the tool's name (tool_call only: a result record does not name it).
	Tool string `json:"tool,omitempty"`
	// Target is the call's one-line summary: the file, the command, the pattern.
	Target string `json:"target,omitempty"`
	// Input is the call's input JSON, capped at MaxInput.
	Input string `json:"input,omitempty"`
	// OK is a result's status (is_error false).
	OK bool `json:"ok,omitempty"`
	// Output is a result's text, capped at MaxOutput (and a notice's detail).
	Output string `json:"output,omitempty"`
	// Notice is a notice's source (NoticeTask, NoticeCommandOutput); Status a
	// task notice's status as the harness wrote it ("completed", "failed",
	// "killed"…), "" when it gave none.
	Notice string `json:"notice,omitempty"`
	Status string `json:"status,omitempty"`
}

// Events is what one record contributes to the rendered view, in order. It is
// STATELESS — a record's events depend on that record alone — which is what lets
// a reader start at any line boundary (a backfill window, a resume offset)
// and still produce the same events for every line after it.
//
// Dropped: thinking blocks, subagent (sidechain) records, injected context
// (isMeta, compaction summaries), and every record type that is not a message
// (attachments, hook summaries, mode changes, titles).
func Events(r Record) []Event {
	if r.IsSidechain {
		return nil
	}
	switch r.Type {
	case "user":
		if text, ok := r.PromptText(); ok {
			if strings.TrimSpace(text) == "" {
				return nil
			}
			if n, ok := noticeOf(r, text); ok {
				return []Event{n}
			}
			// A slash command is the human's: their bubble shows what they typed.
			if cmd, ok := slashCommand(text); ok {
				text = cmd
			}
			return []Event{{Kind: KindPrompt, Text: Cap(text, MaxText)}}
		}
		if r.IsMeta || r.IsCompactSummary {
			return nil
		}
		var out []Event
		for _, b := range r.Blocks() {
			if b.Type == "tool_result" && b.ToolUseID != "" {
				out = append(out, Event{Kind: KindToolResult, ToolID: b.ToolUseID, OK: !b.IsError, Output: Cap(b.ResultText(), MaxOutput)})
			}
		}
		return out
	case "assistant":
		var out []Event
		if r.IsAPIError {
			var msg []string
			for _, b := range r.Blocks() {
				if b.Type == "text" {
					msg = append(msg, b.Text)
				}
			}
			return []Event{{Kind: KindError, Text: Cap(strings.Join(msg, ""), MaxText)}}
		}
		for _, b := range r.Blocks() {
			switch b.Type {
			case "text":
				if strings.TrimSpace(b.Text) != "" {
					out = append(out, Event{Kind: KindText, Text: Cap(b.Text, MaxText)})
				}
			case "tool_use":
				out = append(out, Event{Kind: KindToolCall, ToolID: b.ID, Tool: b.Name,
					Target: ToolTarget(b.Name, string(b.Input)), Input: CapInput(string(b.Input))})
			}
			// "thinking" (and "redacted_thinking"): dropped, always.
		}
		return out
	}
	return nil
}

// CapInput bounds a tool call's input JSON at MaxInput WHILE KEEPING IT JSON
// where it can: an oversized input (an Edit's old_string, a Write's content)
// has each string field shortened instead of the document being cut mid-token,
// so ToolTarget can still read the file path back out of what was stored. Only
// an input that is still too big after that is cut raw.
func CapInput(raw string) string {
	if len(raw) <= MaxInput {
		return raw
	}
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &m) == nil {
		for k, v := range m {
			var s string
			if json.Unmarshal(v, &s) == nil {
				if len(s) > 256 {
					b, _ := json.Marshal(Cap(s, 256))
					m[k] = b
				}
			} else if len(v) > 256 {
				m[k] = json.RawMessage(`"…"`)
			}
		}
		if b, err := json.Marshal(m); err == nil && len(b) <= MaxInput {
			return string(b)
		}
	}
	return Cap(raw, MaxInput)
}

// Cap shortens s to at most n bytes, on a rune boundary, marking a cut with "…".
func Cap(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const mark = "…"
	cut := n - len(mark)
	if cut < 0 {
		cut = 0
	}
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + mark
}
