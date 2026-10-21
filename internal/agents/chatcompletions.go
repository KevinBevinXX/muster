package agents

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/ZacxDev/muster/internal/provision"
)

// The OpenAI-compatible /v1/chat/completions transport: the TOOL-LESS half of
// the two an agent runtime exposes.
//
// 🔴 IT EXISTS BECAUSE /v1/responses IS NOT UNIVERSAL, AND THAT IS NOW ITS ONLY
// REASON TO BE REACHED AT ALL. responses.go's own header records it: older runtime
// builds answer 404 there. A caller that had only the responses endpoint would lose
// the whole turn on such a build — not degrade to a text answer, lose it — so this
// path is the fallback ErrResponsesUnsupported is for. It carries NO tools, and a
// caller must not pretend otherwise: a turn that silently ran here produced an
// answer the model composed with no ability to read or write anything.
//
// 🔴 AND IT IS NO LONGER THE DEFAULT TOOL-LESS PATH, BECAUSE STREAMING HERE LOSES A
// REASONING MODEL'S ENTIRE ANSWER. Measured against a live agent runtime, request
// and credential held identical and only `stream` changed: stream:true produced a
// role chunk, an EMPTY content delta and finish_reason=stop, so the loop below wrote
// nothing into `full` and returned "" with a nil error; stream:false returned the
// text. The runtime had in fact answered — its log shows it refusing the first
// attempt because reasoning cannot be disabled on that endpoint and retrying
// internally — but only AFTER this request had already returned. Nothing in this
// function can tell that apart from a model that said nothing, which is why the fix
// is a transport choice at the caller (agentgateway.Gateway.Chat) and not a check
// here. Do NOT re-point a tool-less turn at this path for tidiness.
//
// ⚠ IT IS A SEPARATE FILE FROM responses.go BECAUSE THE TWO WIRE FORMATS ARE
// UNRELATED. Same host, same bearer, same session-key header, and entirely
// different request and event shapes — chat-completions streams
// `choices[].delta.content` chunks with a `[DONE]` sentinel, Responses streams
// named SSE events assembled from `response.completed`. Sharing a file invited
// the assumption that a fix to one applies to the other.

// ChatMessageIn is one message in a chat-completions request.
type ChatMessageIn struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatCompletionsURL is the agent runtime's /v1/chat/completions URL, derived
// from wherever the provisioner says that instance is reachable.
//
// It is the sibling of [ResponsesURL] and shares its reasoning about taking an
// endpoint rather than a name, including the trailing-slash trim: Endpoint.Path
// is a BASE path, and a caller that spelled it "/agent/" would otherwise produce
// a double slash some routers treat as a different route.
func ChatCompletionsURL(ep provision.Endpoint) string {
	return strings.TrimSuffix(ep.URL(), "/") + "/v1/chat/completions"
}

// ChatStream POSTs a streaming chat-completions turn to the agent runtime and
// invokes emit for each assistant content delta, returning the full assembled
// assistant text. A nil emit just assembles.
//
// model is the runtime's passthrough sentinel and is REQUIRED on the wire — the
// same input [RunToolLoop] takes, for the same reason: the value belongs to the
// attached runtime, not to this package. See agentgateway for where it comes
// from.
//
// 🔴 A MALFORMED DATA LINE IS SKIPPED, NOT FATAL, AND THAT IS A DELIBERATE
// ASYMMETRY WITH THE STATUS CHECK ABOVE IT. A stream that begins is a stream the
// runtime accepted; one unparseable chunk in the middle of a good turn would
// otherwise discard every delta that already arrived. A bad STATUS, by contrast,
// means the turn never started and is an error.
func ChatStream(ctx context.Context, client *http.Client, url, token, sessionKey, model string, messages []ChatMessageIn, emit func(delta string)) (string, error) {
	reqBody, err := json.Marshal(map[string]any{
		"model":    model,
		"messages": messages,
		"stream":   true,
	})
	if err != nil {
		return "", fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if sessionKey != "" {
		req.Header.Set(sessionKeyHeader, sessionKey)
	}
	applyTurnRoute(req)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	observeTurnRoute(req, resp)
	if resp.StatusCode != http.StatusOK {
		// 🔴 THE BODY IS CARRIED, AND AN EARLIER REVISION DROPPED IT WHILE THREE OTHER
		// PLACES ARGUED FROM IT. config.validateProvisioner's sentinel refusal, and the
		// tests that pin it, all justify a BOOT-time refusal by saying the alternative
		// is "an HTTP 400 with a message about the model field" from inside a turn —
		// and on this path the error read `chat completions HTTP 400`, with the message
		// discarded. The machine route POST /api/agents/{name}/messages goes through
		// here rather than through the tool loop, so this was the path that argument
		// was written about. responses.go's own status branch has carried a snippet all
		// along; the asymmetry was the defect.
		// 🔴 BOUNDED: the 512 below caps the MESSAGE, not the READ, and an unbounded
		// ReadAll buffers whatever a non-200 runtime sends before truncating it.
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, errBodyReadLimit))
		snippet := strings.TrimSpace(string(respBody))
		if len(snippet) > 512 {
			snippet = snippet[:512] + "…"
		}
		if snippet == "" {
			return "", fmt.Errorf("chat completions HTTP %d (empty body)", resp.StatusCode)
		}
		return "", fmt.Errorf("chat completions HTTP %d: %s", resp.StatusCode, snippet)
	}

	var full strings.Builder
	sc := bufio.NewScanner(resp.Body)
	// The cap matches responses.go's deliberately: an SSE "line" here is one delta and
	// is normally tiny, but a runtime is free to emit a large one, and a line over the
	// cap is a SCANNER ERROR rather than a skipped line — it ends the stream and is
	// returned below, after deltas have already reached emit. A cap smaller than the
	// other transport's was a difference nothing justified.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		for _, ch := range chunk.Choices {
			if ch.Delta.Content == "" {
				continue
			}
			full.WriteString(ch.Delta.Content)
			if emit != nil {
				emit(ch.Delta.Content)
			}
		}
	}
	// ⚠ BOTH VALUES ARE RETURNED ON PURPOSE, AND A CALLER MUST NOT ASSUME THE TEXT IS
	// EMPTY WHEN THE ERROR IS NON-NIL. Deltas already handed to emit cannot be taken
	// back — a client has rendered them — so a mid-stream scanner failure yields the
	// text that did arrive ALONGSIDE the error, and the caller decides which to honour.
	return full.String(), sc.Err()
}
