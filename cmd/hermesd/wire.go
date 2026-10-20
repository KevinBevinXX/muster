package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/ZacxDev/muster/internal/cctranscript"
)

// muster's agent wire, as hermesd answers it: the shapes ccd answers
// (cmd/ccd/wire.go, failure.go), which muster's client parses
// (internal/agents.parseResponsesSSE, RuntimeError). contract_test.go drives
// this file with muster's own client.
//
// 🔴 A FAILED TURN IS ALWAYS A NON-200 WITH A TYPED JSON BODY — NEVER A 200 WITH
// AN EMPTY REPLY (every muster caller reads that as "the agent answered
// nothing") AND NEVER A 404 (muster's client maps exactly 404 to "this runtime
// has no /v1/responses" and retries on another transport). So, like ccd,
// hermesd decides the status only once the run has ended, and only then opens
// the stream: nothing streams during a turn.

const (
	failUnauthorized = "unauthorized"  // 401: the caller's bearer is wrong
	failBadRequest   = "bad_request"   // 400
	failBusy         = "busy"          // 409: a turn on this session has not settled
	failNotReady     = "not_ready"     // 503: the gateway is not started, recovering, or the checkout is not there
	failTurnTimeout  = "turn_timeout"  // 504: no terminal event within the turn budget
	failRuntime      = "runtime_error" // 502: run.failed
	failInterrupted  = "interrupted"   // 502: run.interrupted / run.cancelled (a gateway shutdown); not retried
	failRateLimited  = "rate_limited"  // 429: the provider refused the turn for a rate limit
	failEmptyReply   = "empty_reply"   // 502: the run completed with no answer text
	// failMayHaveRun is a turn whose outcome hermesd cannot know: the POST's
	// answer was lost, or Hermes said the stored idempotency key was used with a
	// different body. 🔴 It is deliberately NOT not_ready, which muster re-sends:
	// a re-send of a turn that did run runs it twice.
	failMayHaveRun = "may_have_run" // 502
)

// mayHaveRunText is what a person reads for failMayHaveRun.
const mayHaveRunText = "the turn may have run; check the conversation before sending it again"

type failure struct {
	Status  int    `json:"-"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

func (f *failure) Error() string { return f.Type + ": " + f.Message }

func writeFailure(w http.ResponseWriter, f *failure) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(f.Status)
	_ = json.NewEncoder(w).Encode(map[string]*failure{"error": f})
}

func notReady(why string) *failure {
	return &failure{Status: http.StatusServiceUnavailable, Type: failNotReady, Message: why}
}

// classifyRunFailure maps a run.failed error text to a failure. A provider rate
// limit is its own type; muster keys nothing on it for this kind (it marks only
// a Claude account), but the card names it.
func classifyRunFailure(errText string) *failure {
	if errText == "" {
		errText = "the Hermes run failed with no message"
	}
	low := strings.ToLower(errText)
	if strings.Contains(low, "429") || strings.Contains(low, "rate limit") || strings.Contains(low, "rate_limit") {
		return &failure{Status: http.StatusTooManyRequests, Type: failRateLimited, Message: errText}
	}
	return &failure{Status: http.StatusBadGateway, Type: failRuntime, Message: errText}
}

// turnItem is one piece of a turn's reply, in order: a text segment, or one
// tool call Hermes ran.
type turnItem struct {
	Text string
	Tool *toolCall
}

type toolCall struct {
	ID     string
	Name   string
	Input  string
	Status string // in_progress | completed | failed
}

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

type nativeToolItem struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Status    string `json:"status"`
}

// writeResponse writes a settled turn. The REPLY is the text segments; tool
// calls are streamed as native tool items, which muster draws and never
// dispatches, and never appear in response.completed.
//
// 🔴 response.completed IS THE LOAD-BEARING EVENT: muster assembles the reply
// from its `response.output` only.
func writeResponse(w http.ResponseWriter, id string, items []turnItem, stream bool) {
	resp := responseObject{ID: "resp_" + id, Object: "response", Status: "completed", Output: []outputItem{}}
	for _, it := range items {
		if it.Tool != nil || strings.TrimSpace(it.Text) == "" {
			continue
		}
		resp.Output = append(resp.Output, outputItem{
			Type: "message", ID: fmt.Sprintf("msg_%s_%d", id, len(resp.Output)), Role: "assistant", Status: "completed",
			Content: []outputPart{{Type: "output_text", Text: it.Text}},
		})
	}
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
	text, tool := 0, len(resp.Output)
	for _, it := range items {
		if it.Tool != nil {
			emit("response.output_item.done", map[string]any{
				"type": "response.output_item.done", "output_index": tool,
				"item": nativeToolItem{Type: cctranscript.NativeToolItem, ID: "ntc_" + it.Tool.ID, CallID: it.Tool.ID,
					Name: it.Tool.Name, Arguments: it.Tool.Input, Status: it.Tool.Status},
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
