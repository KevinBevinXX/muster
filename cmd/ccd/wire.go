package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/ZacxDev/muster/internal/cctranscript"
)

// The success wire: OpenAI-Responses-style SSE, in the shape muster's client
// parses (internal/agents.parseResponsesSSE).
//
// 🔴 response.completed IS THE LOAD-BEARING EVENT, NOT THE DELTAS. muster
// assembles the reply text ONLY from response.completed's `response.output`; the
// output_text deltas are forwarded to a live UI and are otherwise ignored. A
// stream with deltas and no completed event assembles to "" with a nil error —
// the exact empty success failure.go exists to prevent. The contract test drives
// this writer with muster's own client to hold that.
//
// ⚠ THE STATUS IS DECIDED BEFORE THE FIRST BYTE, WHICH IS WHY NOTHING STREAMS
// DURING A TURN. A failure has to be a non-200 (failure.go), and an HTTP status
// cannot be changed once the body has started — so ccd only opens the stream once
// the turn has ended and its outcome is known. The transcript is written per
// content block anyway, so the best a live stream could have offered is block-
// sized chunks; what it would have cost is a 200 that turns out to be a failure.
//
// Each text segment becomes its own message item with an increasing
// output_index; muster joins items with a blank line, which is also how
// turnResult.Text joins them, so the two sides render the same paragraphs.

type outputPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type outputItem struct {
	Type    string       `json:"type"`
	ID      string       `json:"id"`
	Role    string       `json:"role"`
	Status  string       `json:"status"`
	Content []outputPart `json:"content"`
}

type responseObject struct {
	ID     string       `json:"id"`
	Object string       `json:"object"`
	Status string       `json:"status"`
	Output []outputItem `json:"output"`
}

func buildResponse(promptID string, segments []string) responseObject {
	resp := responseObject{ID: "resp_" + promptID, Object: "response", Status: "completed", Output: []outputItem{}}
	for _, seg := range segments {
		if strings.TrimSpace(seg) == "" {
			continue
		}
		resp.Output = append(resp.Output, outputItem{
			Type: "message", ID: fmt.Sprintf("msg_%s_%d", promptID, len(resp.Output)),
			Role: "assistant", Status: "completed",
			Content: []outputPart{{Type: "output_text", Text: seg}},
		})
	}
	return resp
}

// nativeToolItem is one tool call the CLI ran during the turn, streamed as a
// cctranscript.NativeToolItem output item so muster draws it as a collapsed row.
// It never appears in response.completed's output: the reply muster assembles
// from that event is the text segments, exactly as before.
type nativeToolItem struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	// Status: "completed" (the tool returned ok), "failed" (is_error), or
	// "in_progress" (no result before the turn ended — an interrupted turn).
	Status string `json:"status"`
	Output string `json:"output,omitempty"`
}

func toolItem(it turnItem) nativeToolItem {
	n := nativeToolItem{Type: cctranscript.NativeToolItem, ID: "ntc_" + it.Tool.ToolID, CallID: it.Tool.ToolID,
		Name: it.Tool.Tool, Arguments: it.Tool.Input, Status: "in_progress"}
	if it.Result != nil {
		n.Status, n.Output = "completed", it.Result.Output
		if !it.Result.OK {
			n.Status = "failed"
		}
	}
	return n
}

// writeResponse writes the turn. items is turnResult.Items (text segments and
// tool calls in transcript order); the REPLY is its text segments.
func writeResponse(w http.ResponseWriter, promptID string, items []turnItem, stream bool) {
	var segments []string
	for _, it := range items {
		if it.Tool == nil {
			segments = append(segments, it.Text)
		}
	}
	resp := buildResponse(promptID, segments)
	if !stream {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	emit := func(name string, data any) {
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
	}
	inProgress := resp
	inProgress.Status, inProgress.Output = "in_progress", []outputItem{}
	emit("response.created", map[string]any{"type": "response.created", "response": inProgress})
	// Text deltas and tool items in TRANSCRIPT order, so the chat shows the
	// narration with each call where it happened. A text item's output_index is
	// its index in resp.Output (the completed event's), as before; tool items
	// take the indices after those, so neither collides.
	text, tool := 0, len(resp.Output)
	for _, it := range items {
		if it.Tool != nil {
			emit("response.output_item.done", map[string]any{
				"type": "response.output_item.done", "output_index": tool, "item": toolItem(it),
			})
			tool++
			continue
		}
		if strings.TrimSpace(it.Text) == "" {
			continue
		}
		item := resp.Output[text]
		emit("response.output_text.delta", map[string]any{
			"type": "response.output_text.delta", "item_id": item.ID,
			"output_index": text, "content_index": 0, "delta": item.Content[0].Text,
		})
		text++
	}
	emit("response.completed", map[string]any{"type": "response.completed", "response": resp})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
