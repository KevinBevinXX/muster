package hermeshost

import (
	"context"
	"encoding/json"
)

// PENDING TOOL CALLS, FROM THE DATABASE. Hermes writes the assistant row that
// calls a tool BEFORE the tool runs (agent/turn_tool_round.py), and the tool's
// result row after; a call with no result row yet is pending. A terminal
// command waiting on an approval is such a call, and its `command` argument is
// the RAW command — what will run, before any redaction.

// pendingCallRows is how many of the conversation's last rows are read for
// its pending calls: the call is the newest row but one or two.
const pendingCallRows = 64

// pendingCallArgs is the arguments of every call named name in rows (oldest
// first) that no tool row answers.
func pendingCallArgs(rows []Row, name string) []string {
	answered := map[string]bool{}
	for _, r := range rows {
		if r.Role == "tool" && r.ToolCallID != "" {
			answered[r.ToolCallID] = true
		}
	}
	var out []string
	for _, r := range rows {
		if r.Role != "assistant" || r.ToolCalls == "" {
			continue
		}
		var calls []struct {
			ID       string `json:"id"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		}
		if json.Unmarshal([]byte(r.ToolCalls), &calls) != nil {
			continue
		}
		for _, c := range calls {
			if c.Function.Name == name && c.ID != "" && !answered[c.ID] {
				out = append(out, c.Function.Arguments)
			}
		}
	}
	return out
}

// PendingTerminalCommandsOf is the `command` of every pending `terminal`
// call in rows.
func PendingTerminalCommandsOf(rows []Row) []string {
	var out []string
	for _, a := range pendingCallArgs(rows, "terminal") {
		var args struct {
			Command string `json:"command"`
		}
		if json.Unmarshal([]byte(a), &args) == nil && args.Command != "" {
			out = append(out, args.Command)
		}
	}
	return out
}

// PendingTerminalCommands is PendingTerminalCommandsOf the last rows of root's
// conversation.
func (s *State) PendingTerminalCommands(ctx context.Context, root string) ([]string, error) {
	p, err := s.Backfill(ctx, root, pendingCallRows)
	if err != nil {
		return nil, err
	}
	return PendingTerminalCommandsOf(p.Rows), nil
}
