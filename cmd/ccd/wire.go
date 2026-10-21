package main

import (
	"net/http"

	"github.com/ZacxDev/muster/internal/podagent"
)

// The success wire is internal/podagent's (WriteResponse): the OpenAI-Responses
// SSE muster's client parses, shared with hermesd. Its rules — response.completed
// is the load-bearing event, and nothing streams during a turn because the
// status is decided before the first byte — are stated there. ccd's own part is
// turning a turn's transcript items into reply items.

// toolItem is one tool call the CLI ran during the turn, as a native tool item:
// "completed" (the tool returned ok), "failed" (is_error), or "in_progress" (no
// result before the turn ended — an interrupted turn).
func toolItem(it turnItem) podagent.NativeToolItem {
	status, output := "in_progress", ""
	if it.Result != nil {
		status, output = "completed", it.Result.Output
		if !it.Result.OK {
			status = "failed"
		}
	}
	return podagent.NewNativeToolItem(it.Tool.ToolID, it.Tool.Tool, it.Tool.Input, status, output)
}

// writeResponse writes the turn. items is turnResult.Items (text segments and
// tool calls in transcript order); the REPLY is its text segments.
func writeResponse(w http.ResponseWriter, promptID string, items []turnItem, stream bool) {
	reply := make([]podagent.ReplyItem, 0, len(items))
	for _, it := range items {
		if it.Tool != nil {
			t := toolItem(it)
			reply = append(reply, podagent.ReplyItem{Tool: &t})
			continue
		}
		reply = append(reply, podagent.ReplyItem{Text: it.Text})
	}
	podagent.WriteResponse(w, promptID, reply, stream)
}
