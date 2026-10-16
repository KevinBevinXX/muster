// Package cctranscript reads Claude Code's session transcript: the JSONL file the
// CLI appends one record to per message block, under
// ~/.claude/projects/<cwd-slug>/<session-id>.jsonl.
//
// It is the ONE decoder of that format in muster. Two readers use it:
//
//   - ccd (cmd/ccd), which locates one turn by its promptId and returns the
//     reply a provisioned claude-code agent gave, plus — now — the tool calls the
//     CLI ran during it, so the agent's chat shows them as collapsed rows;
//   - muster-mirror (internal/mirrorhost), which tails a mirrored tmux pane's
//     transcript and streams it as Events to muster's rendered view.
//
// 🔴 THINKING IS NEVER AN EVENT. A thinking block is the model's private
// reasoning; rendering it beside the reply presents it as if it were the
// answer. Every reader here drops it, and the tests pin that.
//
// The record shapes relied on, all observed in real transcripts (CLI 2.1.289 /
// 2.1.296; cmd/ccd/testdata holds real records with their content replaced):
//
//   - a human prompt: {"type":"user","promptId":P,"message":{"content":"<text>"}}
//   - tool results in that turn: {"type":"user","promptId":P,"message":{"content":
//     [{"type":"tool_result","tool_use_id":T,"content":<string|blocks>,"is_error":B}]}}
//   - assistant output, ONE block per record: {"type":"assistant","message":
//     {"content":[<thinking | text | tool_use{id,name,input}>]}}
//   - an API failure: an assistant record with "isApiErrorMessage":true
//   - the end of a turn: {"type":"system","subtype":"turn_duration"}
//   - injected context (skill bodies, caveats): a user record with "isMeta":true
//   - a harness notice (a background task finishing, a local command's
//     output): a user record with a prompt's shape whose text is wholly
//     <tag>…</tag> blocks, or whose "origin" is {"kind":"task-notification"}
//     (notice.go). A slash command has the first shape but is the human's.
//   - a subagent's records: "isSidechain":true
package cctranscript

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Record is one decoded transcript line, the fields muster reads and no others.
type Record struct {
	Type             string `json:"type"`
	Subtype          string `json:"subtype"`
	PromptID         string `json:"promptId"`
	IsSidechain      bool   `json:"isSidechain"`
	IsMeta           bool   `json:"isMeta"`
	IsCompactSummary bool   `json:"isCompactSummary"`
	IsAPIError       bool   `json:"isApiErrorMessage"`
	Error            string `json:"error"`
	APIErrorStatus   int    `json:"apiErrorStatus"`
	// Origin is who wrote a user record: "human", or "task-notification" for
	// one the harness injected (notice.go). Absent before CLI 2.1.22.
	Origin struct {
		Kind string `json:"kind"`
	} `json:"origin"`
	Message struct {
		Content json.RawMessage `json:"content"`
		// Model and Usage are an assistant record's (usage.go): its model id
		// only marks the CLI's own "<synthetic>" records.
		Model string `json:"model"`
		Usage *struct {
			InputTokens              int64 `json:"input_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// Block is one content block of a record's message.
type Block struct {
	Type string `json:"type"`
	// text
	Text string `json:"text"`
	// tool_use
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
	// tool_result
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// DecodeLine decodes one JSONL line. A line that is not a JSON object is not a
// record (false), which a reader skips: one corrupt line must not end a read.
func DecodeLine(line []byte) (Record, bool) {
	var r Record
	if len(bytes.TrimSpace(line)) == 0 || json.Unmarshal(line, &r) != nil {
		return Record{}, false
	}
	return r, true
}

// Blocks decodes an array content; a string content (a prompt) yields nil.
func (r Record) Blocks() []Block {
	c := bytes.TrimSpace(r.Message.Content)
	if len(c) == 0 || c[0] != '[' {
		return nil
	}
	var bs []Block
	if json.Unmarshal(c, &bs) != nil {
		return nil
	}
	return bs
}

// PromptText is a prompt-shaped record's text, and whether r is one at all.
//
// ⚠ PROMPT-SHAPED, NOT NECESSARILY HUMAN: a record the harness injected (a
// task notification, a local command's output) has exactly this shape. Events tells the
// two apart (notice.go) and draws only a human's as a prompt.
//
// A prompt is a main-conversation user record that is not injected context
// (isMeta, a compaction summary) and carries text rather than tool results: a
// string content, or an array of text blocks with no tool_result among them (a
// prompt with a pasted image is written that way).
func (r Record) PromptText() (string, bool) {
	if r.Type != "user" || r.IsSidechain || r.IsMeta || r.IsCompactSummary {
		return "", false
	}
	c := bytes.TrimSpace(r.Message.Content)
	if len(c) > 0 && c[0] == '"' {
		var s string
		if json.Unmarshal(c, &s) != nil {
			return "", false
		}
		return s, true
	}
	var parts []string
	for _, b := range r.Blocks() {
		switch b.Type {
		case "tool_result":
			return "", false
		case "text":
			parts = append(parts, b.Text)
		}
	}
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, "\n\n"), true
}

// ResultText is a tool_result block's content as text: the string itself, or
// the text of its text blocks (an image block contributes "[image]").
func (b Block) ResultText() string {
	c := bytes.TrimSpace(b.Content)
	if len(c) == 0 {
		return ""
	}
	if c[0] == '"' {
		var s string
		_ = json.Unmarshal(c, &s)
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(c, &parts) != nil {
		return ""
	}
	var out []string
	for _, p := range parts {
		switch p.Type {
		case "text":
			out = append(out, p.Text)
		case "image":
			out = append(out, "[image]")
		}
	}
	return strings.Join(out, "\n")
}
