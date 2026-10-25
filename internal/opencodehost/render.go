package opencodehost

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/cctranscript"
)

// RENDERING (docs/design/opencode.md): an opencode message becomes the
// rendered view's own events, so the page draws an opencode session with the
// Claude Code view's components.
//
//	user (text)                       → prompt (Line = its time.created)
//	assistant text                    → text, once the item is FINAL
//	assistant reasoning               → nothing: thinking is never drawn
//	                                    (cctranscript's rule; it may be opaque)
//	assistant tool, running or later  → tool_call: the tool's NAME and TARGET
//	                                    (the file, the command), never its input
//	assistant tool, completed / error → tool_result: ok or not, never its output
//	assistant finish error            → error
//	idle, outcome failed              → error; interrupted → notice
//	synthetic, system, skill, shell,
//	  compaction, a switch            → notice naming it
//	a type this does not know         → notice naming it, never the person's
//	                                    bubble
//
// 🔴 WHAT IS EMITTED NEVER CHANGES. The view's protocol appends (or starts
// over), and an assistant message is rewritten in place while it streams. So
// each event has a KEY, and an event is rendered only once it is final: a
// text item when a later item exists or the message is complete; a tool call
// once its input is parsed (running), its result once it has one. The tail
// sends each key once, and starts over if a key's event ever changes.
//
// The fields are capped here (cctranscript's caps) and REDACTED by the
// caller (internal/mirrorhost's redactLines) before anything is framed.

// Keyed is one event of a message and its key within the message.
type Keyed struct {
	Key   string
	Event cctranscript.Event
}

// Render is m's settled events, in order.
func Render(m Message) []Keyed {
	switch m.Type {
	case "user":
		if strings.TrimSpace(m.Text) == "" {
			return nil
		}
		return []Keyed{{"u", cctranscript.Event{Kind: cctranscript.KindPrompt, Text: cctranscript.Cap(m.Text, cctranscript.MaxText),
			Line: int64(m.Time.Created)}}}
	case "assistant":
		return renderAssistant(m)
	case "idle":
		switch m.Outcome {
		case "failed":
			return []Keyed{{"i", cctranscript.Event{Kind: cctranscript.KindError, Text: cctranscript.Cap(errorText(m.Error, "The turn failed."), cctranscript.MaxText)}}}
		case "interrupted":
			return []Keyed{notice("i", "turn interrupted", "")}
		}
		return nil
	case "synthetic":
		return []Keyed{notice("n", labelled("synthetic", m.Description, m.Text), m.Text)}
	case "system":
		return []Keyed{notice("n", labelled("system", m.Description, m.Text), m.Text)}
	case "skill":
		return []Keyed{notice("n", labelled("skill", m.Name, ""), m.Text)}
	case "shell":
		if m.Status == "" || m.Status == "running" {
			return nil
		}
		return []Keyed{notice("n", "shell "+m.Status+": "+oneLine(m.Command), "")}
	case "compaction":
		if m.Status != "" && m.Status != "completed" && m.Status != "failed" {
			return nil
		}
		return []Keyed{notice("n", "compaction", "")}
	case "agent-switched", "agent-selected":
		return []Keyed{notice("n", "agent switched", "")}
	case "model-switched", "model-selected":
		return []Keyed{notice("n", "model switched", "")}
	case "location-switched":
		return []Keyed{notice("n", "moved to another directory", "")}
	case "":
		return nil
	}
	// 🔴 An unknown type is a notice naming it — never the person's bubble.
	return []Keyed{notice("n", cctranscript.Cap(m.Type, 64), "")}
}

func renderAssistant(m Message) []Keyed {
	var out []Keyed
	complete := m.Time.Completed > 0 || m.Finish != ""
	for i, c := range m.Content {
		final := complete || i < len(m.Content)-1
		switch c.Type {
		case "text":
			if final && strings.TrimSpace(c.Text) != "" {
				out = append(out, Keyed{"t" + strconv.Itoa(i), cctranscript.Event{Kind: cctranscript.KindText, Text: cctranscript.Cap(c.Text, cctranscript.MaxText)}})
			}
		case "tool":
			if c.ID == "" || c.Name == "" {
				continue
			}
			id := cctranscript.Cap(c.ID, 128)
			switch c.State.Status {
			case "running", "completed", "error":
			default:
				continue // streaming: its input is not parsed yet
			}
			out = append(out, Keyed{"c:" + c.ID, cctranscript.Event{Kind: cctranscript.KindToolCall, ToolID: id,
				Tool: cctranscript.Cap(c.Name, 128), Target: target(c.Name, c.State.Input)}})
			if c.State.Status != "running" {
				out = append(out, Keyed{"r:" + c.ID, cctranscript.Event{Kind: cctranscript.KindToolResult, ToolID: id, OK: c.State.Status == "completed"}})
			}
		}
	}
	if complete && (m.Finish == "error" || hasError(m.Error)) {
		out = append(out, Keyed{"e", cctranscript.Event{Kind: cctranscript.KindError, Text: cctranscript.Cap(errorText(m.Error, "The turn failed."), cctranscript.MaxText)}})
	}
	return out
}

// targetKeys is, per opencode tool, the input field that names what the call
// acts on (opencode's tools at 6ec5682: shell, read, edit, write, grep, glob,
// webfetch, websearch, patch, skill, subagent, question, …). A tool not
// listed falls back to cctranscript.ToolTarget's own keys.
var targetKeys = map[string][]string{
	"shell":     {"command"},
	"read":      {"filePath", "path"},
	"edit":      {"filePath", "path"},
	"write":     {"filePath", "path"},
	"patch":     {"filePath", "path"},
	"grep":      {"pattern"},
	"glob":      {"pattern"},
	"webfetch":  {"url"},
	"websearch": {"query"},
	"skill":     {"name"},
	"subagent":  {"description"},
	"question":  {"header", "question"},
}

// target is a tool call's one-line summary from its input.
func target(tool string, input json.RawMessage) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(input, &m) != nil {
		return ""
	}
	for _, k := range targetKeys[tool] {
		var s string
		if raw, ok := m[k]; ok && json.Unmarshal(raw, &s) == nil {
			if one := oneLine(s); one != "" {
				return one
			}
		}
	}
	return cctranscript.ToolTarget(tool, string(input))
}

// notice is a notice event keyed key.
func notice(key, text, detail string) Keyed {
	return Keyed{key, cctranscript.Event{Kind: cctranscript.KindNotice, Notice: cctranscript.NoticeOpenCode,
		Text: cctranscript.Cap(text, cctranscript.MaxTarget*4), Output: cctranscript.Cap(detail, cctranscript.MaxOutput)}}
}

// labelled is "<kind>: <name>", or "<kind>: <first line of fallback>", or
// "<kind>".
func labelled(kind, name, fallback string) string {
	if l := firstLine(name); l != "" {
		return kind + ": " + l
	}
	if l := firstLine(fallback); l != "" {
		return kind + ": " + l
	}
	return kind
}

// hasError reports whether raw is a non-null error.
func hasError(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null"
}

// errorText is a structured error's message, else def.
func errorText(raw json.RawMessage, def string) string {
	var e struct {
		Message string `json:"message"`
		Data    struct {
			Message string `json:"message"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &e) == nil {
		if strings.TrimSpace(e.Message) != "" {
			return e.Message
		}
		if strings.TrimSpace(e.Data.Message) != "" {
			return e.Data.Message
		}
	}
	return def
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

// oneLine is s's first non-blank line, whitespace-collapsed, capped at
// cctranscript.MaxTarget runes (a cut is marked "…"): cctranscript's rule for
// a tool row's target.
func oneLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.Join(strings.Fields(line), " ")
		if line == "" {
			continue
		}
		if r := []rune(line); len(r) > cctranscript.MaxTarget {
			line = string(r[:cctranscript.MaxTarget-1]) + "…"
		}
		return line
	}
	return ""
}
