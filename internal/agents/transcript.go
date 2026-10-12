package agents

import "strings"

// TurnPart is one ORDERED piece of an assistant turn: a text segment, a tool
// call, or a tool result. A turn is persisted as a sequence of these so the
// canonical transcript (reload / reconcile) reproduces the live stream — full
// narration with tool calls interleaved — instead of only the final text.
type TurnPart struct {
	Kind     string // "text" | "tool_call" | "tool_result"
	Content  string // text body, or tool args (tool_call) / output (tool_result)
	ToolID   string // links a tool_call to its tool_result
	ToolName string
	ToolOK   bool // tool_result: dispatch success
}

// PartCollector reconstructs a turn's ordered parts by OBSERVING the same stream
// events that drive the live UI. Text deltas accumulate into a running segment
// that is flushed (as a text part) at each tool boundary, so a turn like
// text→tool→text yields three parts in arrival order — with NO change to the
// tool-loop internals (the caller just wraps its emit with Observe). 'user',
// 'thinking', and 'done' events are not persisted as parts (the user message is
// stored separately; thinking is ephemeral), but 'thinking' flushes pending text
// so a text→thinking→text turn keeps its ordering.
type PartCollector struct {
	parts []TurnPart
	buf   strings.Builder
}

// Observe folds one stream event into the collector.
func (pc *PartCollector) Observe(ev StreamEvent) {
	switch ev.Kind {
	case "text":
		pc.buf.WriteString(ev.Text)
	case "thinking":
		pc.flush()
	case "tool_call":
		pc.flush()
		pc.parts = append(pc.parts, TurnPart{Kind: "tool_call", ToolID: ev.ToolID, ToolName: ev.ToolName, Content: ev.ToolArgs})
	case "tool_result":
		pc.parts = append(pc.parts, TurnPart{Kind: "tool_result", ToolID: ev.ToolID, ToolName: ev.ToolName, ToolOK: ev.ToolOK, Content: ev.ToolOutput})
	}
}

func (pc *PartCollector) flush() {
	// Drop empty + gateway "no reply" sentinel segments so they aren't persisted as
	// noise bubbles (the gateway emits a marker like NO_REPLY when a turn ends via
	// tool calls with no closing narration).
	// A segment that follows a tool row begins with the blank-line separator a
	// text-only reader needs between segments (handleResponsesEvent); as its own
	// part it is a separate bubble, and the separator is just leading space.
	if s := strings.TrimLeft(pc.buf.String(), "\n"); !isSentinelReply(s) {
		pc.parts = append(pc.parts, TurnPart{Kind: "text", Content: s})
	}
	pc.buf.Reset()
}

// isSentinelReply reports whether an assistant text segment is empty/whitespace or
// a gateway "no textual reply" marker that should not be persisted or rendered.
func isSentinelReply(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" {
		return true
	}
	switch strings.ToUpper(t) {
	case "NO_REPLY", "NO_REPL", "NO REPLY", "(NO REPLY)":
		return true
	}
	return false
}

// Parts flushes any trailing text and returns the ordered parts of the turn.
func (pc *PartCollector) Parts() []TurnPart {
	pc.flush()
	return pc.parts
}
