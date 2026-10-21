package podagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/ZacxDev/muster/internal/cctranscript"
)

// muster's agent wire as an in-pod supervisor answers it: the OpenAI-Responses
// shape muster's client parses (internal/agents.parseResponsesSSE and
// RuntimeError). ccd and hermesd both answer with this one writer; each drives
// it with muster's own client in its contract test.
//
// 🔴 A FAILED TURN IS ALWAYS A NON-200 WITH A JSON BODY NAMING A STABLE TYPE —
// NEVER A 200 WITH AN EMPTY REPLY. muster's client (RunToollessTurn) assembles
// the reply from the stream and returns it with a nil error; a 200 whose stream
// carries no text is, to every caller, "the agent answered nothing" — a kickoff
// recorded as delivered, a chat bubble left blank.
//
// 🔴 AND NEVER A 404. muster's client maps exactly 404 to "this runtime has no
// /v1/responses" and retries the turn on /v1/chat/completions.
//
// 🔴 response.completed IS THE LOAD-BEARING EVENT, NOT THE DELTAS. muster
// assembles the reply text ONLY from response.completed's `response.output`;
// the output_text deltas are forwarded to a live UI and are otherwise ignored.
//
// ⚠ THE STATUS IS DECIDED BEFORE THE FIRST BYTE, WHICH IS WHY NOTHING STREAMS
// DURING A TURN: a failure has to be a non-200, and an HTTP status cannot be
// changed once the body has started — so a supervisor opens the stream only
// once the turn has ended and its outcome is known.

// Failure is a typed failed turn: `{"error":{type,code,upstream_status,message}}`.
type Failure struct {
	Status   int    `json:"-"`
	Type     string `json:"type"`
	Code     string `json:"code,omitempty"`            // the runtime's own error code, when there is one
	Upstream int    `json:"upstream_status,omitempty"` // the API's HTTP status, when known
	Message  string `json:"message"`
}

func (f *Failure) Error() string { return f.Type + ": " + f.Message }

// WriteFailure writes f as the response.
func WriteFailure(w http.ResponseWriter, f *Failure) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(f.Status)
	_ = json.NewEncoder(w).Encode(map[string]*Failure{"error": f})
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

// NativeToolItem is one tool call the agent ran itself during the turn,
// streamed as a cctranscript.NativeToolItem output item so muster draws it as a
// collapsed row and never dispatches it. It never appears in
// response.completed's output.
type NativeToolItem struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	// Status: "completed", "failed", or "in_progress" (no result before the
	// turn ended).
	Status string `json:"status"`
	Output string `json:"output,omitempty"`
}

// NewNativeToolItem is a NativeToolItem for call id with the given fields.
func NewNativeToolItem(id, name, arguments, status, output string) NativeToolItem {
	return NativeToolItem{Type: cctranscript.NativeToolItem, ID: "ntc_" + id, CallID: id,
		Name: name, Arguments: arguments, Status: status, Output: output}
}

// ReplyItem is one piece of a settled turn, in order: a text segment, or one
// tool call.
type ReplyItem struct {
	Text string
	Tool *NativeToolItem
}

func buildResponse(id string, items []ReplyItem) responseObject {
	resp := responseObject{ID: "resp_" + id, Object: "response", Status: "completed", Output: []outputItem{}}
	for _, it := range items {
		if it.Tool != nil || strings.TrimSpace(it.Text) == "" {
			continue
		}
		resp.Output = append(resp.Output, outputItem{
			Type: "message", ID: fmt.Sprintf("msg_%s_%d", id, len(resp.Output)),
			Role: "assistant", Status: "completed",
			Content: []outputPart{{Type: "output_text", Text: it.Text}},
		})
	}
	return resp
}

// WriteResponse writes a settled turn. The REPLY is the text segments (each its
// own message item; muster joins items with a blank line); tool calls are
// streamed as native tool items.
func WriteResponse(w http.ResponseWriter, id string, items []ReplyItem, stream bool) {
	resp := buildResponse(id, items)
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
	// Text deltas and tool items in turn order. A text item's output_index is
	// its index in resp.Output (the completed event's); tool items take the
	// indices after those, so neither collides.
	text, tool := 0, len(resp.Output)
	for _, it := range items {
		if it.Tool != nil {
			emit("response.output_item.done", map[string]any{
				"type": "response.output_item.done", "output_index": tool, "item": *it.Tool,
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

type inputItem struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// PromptFrom extracts the LAST user message's text from `input`, which may be a
// bare string or an array of items whose content is a string or an array of
// {type: input_text|text, text} parts.
func PromptFrom(raw json.RawMessage) (string, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var items []inputItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return "", errors.New("`input` is neither a string nor an array of items")
	}
	for i := len(items) - 1; i >= 0; i-- {
		it := items[i]
		if it.Role != "user" || (it.Type != "" && it.Type != "message") {
			continue
		}
		if json.Unmarshal(it.Content, &s) == nil {
			return s, nil
		}
		var parts []contentBlock
		if json.Unmarshal(it.Content, &parts) != nil {
			return "", errors.New("user message content is neither a string nor an array of parts")
		}
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "input_text" || p.Type == "text" {
				b.WriteString(p.Text)
			}
		}
		return b.String(), nil
	}
	return "", errors.New("`input` carries no user message")
}
