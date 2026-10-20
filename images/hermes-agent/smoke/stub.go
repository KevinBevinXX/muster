package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// stubModel is a scripted OpenAI-compatible model for the smoke test, on the
// host's loopback (the container runs with the host's network). It is the Go
// port of the Phase 0 spikes' stub. Directives in the LAST user message, only
// honoured when the request offers tools:
//
//	RUN:<cmd>   one `terminal` tool call with {"command": <cmd>}
//	SLEEP:<n>   wait n seconds before answering
//	else        a plain reply, "STUB_REPLY to: <text>"
//
// When the last message is a tool result, the reply quotes it:
// "TOOL_RESULT_SEEN: <result>". Requests without tools (titles, summaries) get
// a short reply. Every request body is kept, for the checks.
type stubModel struct {
	mu   sync.Mutex
	reqs []map[string]any
	// seen is closed and replaced on every request.
	seen chan struct{}
}

func newStubModel() *stubModel { return &stubModel{seen: make(chan struct{})} }

func textOf(c any) string {
	switch v := c.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, p := range v {
			if m, ok := p.(map[string]any); ok {
				if t, ok := m["text"].(string); ok {
					b.WriteString(t)
				}
			}
		}
		return b.String()
	}
	return ""
}

// requests returns the chat requests that offered tools: the agent's turns.
func (s *stubModel) requests() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, r := range s.reqs {
		if tools, _ := r["tools"].([]any); len(tools) > 0 {
			out = append(out, r)
		}
	}
	return out
}

func (s *stubModel) waitFor(pred func([]map[string]any) bool, d time.Duration) bool {
	deadline := time.After(d)
	for {
		s.mu.Lock()
		ch := s.seen
		s.mu.Unlock()
		if pred(s.requests()) {
			return true
		}
		select {
		case <-ch:
		case <-deadline:
			return false
		}
	}
}

func (s *stubModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && strings.HasSuffix(strings.TrimRight(r.URL.Path, "/"), "/models") {
		writeJSON(w, 200, map[string]any{"object": "list", "data": []any{
			map[string]any{"id": "stub-model", "object": "model", "owned_by": "stub", "context_length": 200000}}})
		return
	}
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/chat/completions") {
		writeJSON(w, 404, map[string]any{"error": map[string]any{"message": "the stub serves chat/completions only"}})
		return
	}
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	s.mu.Lock()
	s.reqs = append(s.reqs, body)
	close(s.seen)
	s.seen = make(chan struct{})
	s.mu.Unlock()

	kind, payload, delay := decide(body)
	if delay > 0 {
		time.Sleep(delay)
	}
	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	model, _ := body["model"].(string)
	var msg map[string]any
	finish := "stop"
	var toolCall map[string]any
	if kind == "tool" {
		args, _ := json.Marshal(map[string]string{"command": payload})
		toolCall = map[string]any{"id": "call_" + strconv.FormatInt(time.Now().UnixNano(), 36), "type": "function",
			"function": map[string]any{"name": "terminal", "arguments": string(args)}}
		msg = map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{toolCall}}
		finish = "tool_calls"
	} else {
		msg = map[string]any{"role": "assistant", "content": payload}
	}
	usage := map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
	if stream, _ := body["stream"].(bool); !stream {
		writeJSON(w, 200, map[string]any{"id": id, "object": "chat.completion", "created": time.Now().Unix(), "model": model,
			"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}}, "usage": usage})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	fl, _ := w.(http.Flusher)
	chunk := func(delta map[string]any, fin any, extra map[string]any) {
		c := map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": fin}}}
		for k, v := range extra {
			c[k] = v
		}
		b, _ := json.Marshal(c)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if fl != nil {
			fl.Flush()
		}
	}
	chunk(map[string]any{"role": "assistant"}, nil, nil)
	if kind == "tool" {
		tc := map[string]any{"index": 0}
		for k, v := range toolCall {
			tc[k] = v
		}
		chunk(map[string]any{"tool_calls": []any{tc}}, nil, nil)
	} else {
		for i := 0; i < len(payload); i += 40 {
			chunk(map[string]any{"content": payload[i:min(i+40, len(payload))]}, nil, nil)
		}
	}
	chunk(map[string]any{}, finish, nil)
	b, _ := json.Marshal(map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(),
		"model": model, "choices": []any{}, "usage": usage})
	fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
	if fl != nil {
		fl.Flush()
	}
}

func decide(body map[string]any) (kind, payload string, delay time.Duration) {
	msgs, _ := body["messages"].([]any)
	tools, _ := body["tools"].([]any)
	if len(tools) == 0 {
		return "text", "Stub title", 0
	}
	var last map[string]any
	if len(msgs) > 0 {
		last, _ = msgs[len(msgs)-1].(map[string]any)
	}
	if last["role"] == "tool" {
		t := textOf(last["content"])
		if len(t) > 400 {
			t = t[:400]
		}
		return "text", "TOOL_RESULT_SEEN: " + t, 0
	}
	user := ""
	if last["role"] == "user" {
		user = textOf(last["content"])
	}
	for _, line := range strings.Split(user, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "SLEEP:"); ok {
			if n, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
				delay = time.Duration(n * float64(time.Second))
			}
		}
	}
	for _, line := range strings.Split(user, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "RUN:"); ok {
			return "tool", strings.TrimSpace(v), delay
		}
	}
	u := user
	if len(u) > 120 {
		u = u[:120]
	}
	return "text", "STUB_REPLY to: " + u, delay
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// fakeMuster is muster's agent-facing surface as the pod reaches it: the
// approval relay (answering from a queue of decisions) and an empty connector
// list. It checks the agent's token.
type fakeMuster struct {
	token string

	mu        sync.Mutex
	decisions []string // answered in order, then "approve"
	filed     []map[string]any
	deleted   int
	answered  map[string]string
}

func (m *fakeMuster) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Muster-Token") != m.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/agent/connectors":
		// One approved connector, so hermesd writes git's routing through
		// muster's proxy and the agent's shell can be asked to show it.
		writeJSON(w, 200, map[string]any{"connectors": []any{map[string]any{
			"name": "github", "exists": true, "grant": "approved", "hosts": []string{"github.com"}}}})
	case r.Method == http.MethodPost && r.URL.Path == "/agent/approvals":
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		m.filed = append(m.filed, b)
		id := fmt.Sprintf("req-%d", len(m.filed))
		d := "approve"
		if len(m.decisions) > 0 {
			d, m.decisions = m.decisions[0], m.decisions[1:]
		}
		if m.answered == nil {
			m.answered = map[string]string{}
		}
		m.answered[id] = d
		writeJSON(w, http.StatusCreated, map[string]string{"requestId": id})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/agent/approvals/"):
		id := strings.TrimPrefix(r.URL.Path, "/agent/approvals/")
		d, ok := m.answered[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, 200, map[string]string{"state": "decided", "response": d})
	case r.Method == http.MethodDelete:
		m.deleted++
		w.WriteHeader(200)
	default:
		http.NotFound(w, r)
	}
}
