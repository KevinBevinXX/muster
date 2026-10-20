package hermeshost

import (
	"encoding/json"
	"strings"

	"github.com/ZacxDev/muster/internal/cctranscript"
)

// RENDERING (docs/design/hermes.md, "Rendering into cctranscript.Event"): a
// Hermes messages row becomes the rendered view's own events, so the page
// draws a Hermes conversation with the Claude Code view's components.
//
//	role user, no display_kind          → prompt (Line = messages.id)
//	role assistant, content             → text
//	each tool_calls[] entry             → tool_call
//	role tool with tool_call_id         → tool_result
//	display_kind failed_turn, or an
//	  error finish_reason               → error
//	a known notice display_kind         → notice naming the kind
//	an UNKNOWN display_kind             → notice naming it, never user text
//	display_kind hidden, role system,
//	  role session_meta, reasoning only → nothing
//
// The fields are capped here (cctranscript's caps) and REDACTED by the
// caller (internal/mirrorhost's redactLines) before anything is framed.

// noticeKinds are the display kinds drawn as a notice: rows Hermes writes
// into the conversation that the person did not type.
var noticeKinds = map[string]string{
	"process_complete":          "process complete",
	"async_delegation_complete": "delegation complete",
	"model_switch":              "model switch",
	"steer":                     "steer",
	"auto_continue":             "auto-continue",
	"skill_invocation":          "skill",
	"personality_switch":        "personality switch",
	"internal_notification":     "notification",
}

// errorFinish are the finish_reason values that mean the turn failed.
var errorFinish = map[string]bool{"error": true, "failed": true}

// toolCall is one entry of a tool_calls column (the OpenAI shape Hermes
// stores).
type toolCall struct {
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments any    `json:"arguments"`
	} `json:"function"`
}

// arguments is a call's arguments as JSON text: Hermes stores them as a JSON
// string (OpenAI's shape), sometimes as an object.
func (c toolCall) arguments() string {
	switch a := c.Function.Arguments.(type) {
	case string:
		return a
	case nil:
		return ""
	default:
		b, _ := json.Marshal(a)
		return string(b)
	}
}

// Render is r's events. A row it draws nothing for has none.
func Render(r Row) []cctranscript.Event {
	if r.Role == "system" || r.Role == "session_meta" || r.DisplayKind == "hidden" {
		return nil
	}
	if r.DisplayKind == "failed_turn" || errorFinish[r.FinishReason] {
		text := strings.TrimSpace(r.Content)
		if text == "" {
			text = "The turn failed."
		}
		return []cctranscript.Event{{Kind: cctranscript.KindError, Text: cctranscript.Cap(text, cctranscript.MaxText)}}
	}
	if r.DisplayKind != "" {
		label, ok := noticeKinds[r.DisplayKind]
		if !ok {
			// 🔴 An unknown kind is a notice naming it — never the person's
			// bubble, whatever role the row has.
			label = r.DisplayKind
		}
		text := label
		if line := firstLine(r.Content); line != "" {
			text += ": " + line
		}
		return []cctranscript.Event{{Kind: cctranscript.KindNotice, Notice: cctranscript.NoticeHermes,
			Text: cctranscript.Cap(text, cctranscript.MaxTarget*4), Output: cctranscript.Cap(r.Content, cctranscript.MaxOutput)}}
	}
	switch r.Role {
	case "user":
		if strings.TrimSpace(r.Content) == "" {
			return nil
		}
		return []cctranscript.Event{{Kind: cctranscript.KindPrompt, Text: cctranscript.Cap(r.Content, cctranscript.MaxText), Line: r.ID}}
	case "assistant":
		var out []cctranscript.Event
		if strings.TrimSpace(r.Content) != "" {
			out = append(out, cctranscript.Event{Kind: cctranscript.KindText, Text: cctranscript.Cap(r.Content, cctranscript.MaxText)})
		}
		var calls []toolCall
		if r.ToolCalls != "" && json.Unmarshal([]byte(r.ToolCalls), &calls) == nil {
			for _, c := range calls {
				if c.ID == "" || c.Function.Name == "" {
					continue
				}
				args := c.arguments()
				out = append(out, cctranscript.Event{Kind: cctranscript.KindToolCall, ToolID: cctranscript.Cap(c.ID, 128),
					Tool: cctranscript.Cap(c.Function.Name, 128), Target: cctranscript.ToolTarget(c.Function.Name, args),
					Input: cctranscript.CapInput(args)})
			}
		}
		return out
	case "tool":
		if r.ToolCallID == "" {
			return nil
		}
		return []cctranscript.Event{{Kind: cctranscript.KindToolResult, ToolID: cctranscript.Cap(r.ToolCallID, 128),
			OK: toolOK(r.Content), Output: cctranscript.Cap(r.Content, cctranscript.MaxOutput)}}
	}
	return nil
}

// toolOK reports a tool result as a success unless its content is a JSON
// object that says otherwise: a non-empty "error", or a non-zero
// "exit_code" (Hermes's terminal tool). Anything else — plain text — is a
// success: Hermes records no is_error flag.
func toolOK(content string) bool {
	var m map[string]any
	if json.Unmarshal([]byte(strings.TrimSpace(content)), &m) != nil {
		return true
	}
	if e, ok := m["error"]; ok && e != nil && e != "" && e != false {
		return false
	}
	if c, ok := m["exit_code"].(float64); ok && c != 0 {
		return false
	}
	return true
}

// firstLine is s's first non-blank line, trimmed.
func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}
