package askq

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// THE HOOK CLIENT: what a PreToolUse hook on AskUserQuestion runs (today
// `muster-mirror ask`). It tells muster the call's questions — so muster shows
// them at once — and RETURNS AT ONCE, PRINTING NOTHING, so the terminal draws
// its own picker at once too. It never holds the call and never answers it: an
// answer given in muster reaches the picker as keys (internal/askdrive).
//
// 🔴 IT NEVER PRINTS, WHATEVER HAPPENS. A PreToolUse hook's stdout is the
// CLI's to act on; this hook has nothing to say to it. Every failure (muster
// unreachable, a refused registration) costs only the muster card — the
// picker is unaffected.

// HookPayload is the subset of Claude Code's PreToolUse hook payload read here.
type HookPayload struct {
	HookEventName string          `json:"hook_event_name"`
	ToolName      string          `json:"tool_name"`
	ToolInput     json.RawMessage `json:"tool_input"`
	SessionID     string          `json:"session_id"`
	ToolUseID     string          `json:"tool_use_id"`
}

// Hook is one hook run's configuration.
type Hook struct {
	// Base is muster's base URL; Path the registration route ("/mirror/ask").
	Base, Path string
	// Auth sets the request's credential.
	Auth func(*http.Request)
	// Client makes the one request; its Timeout bounds the hook (the picker
	// is drawn once the hook returns).
	Client *http.Client
	Logf   func(format string, a ...any)
}

func (h Hook) logf(format string, a ...any) {
	if h.Logf != nil {
		h.Logf(format, a...)
	}
}

// Run registers payload's call with muster. It returns whether muster took it;
// either way the hook prints nothing.
func (h Hook) Run(ctx context.Context, payload []byte) bool {
	var p HookPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		h.logf("the payload is not JSON")
		return false
	}
	if p.ToolName != ToolName || (p.HookEventName != "" && p.HookEventName != "PreToolUse") {
		return false
	}
	if _, err := ParseInput(p.ToolInput); err != nil {
		h.logf("%v; muster will not show this question", err)
		return false
	}
	if h.Base == "" || h.Auth == nil {
		h.logf("no muster URL or credential; muster will not show this question")
		return false
	}
	body, _ := json.Marshal(map[string]any{"id": NewID(), "sessionId": p.SessionID, "toolUseId": p.ToolUseID, "toolInput": p.ToolInput})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(h.Base, "/")+h.Path, bytes.NewReader(body))
	if err != nil {
		h.logf("%v", err)
		return false
	}
	h.Auth(req)
	req.Header.Set("Content-Type", "application/json")
	client := h.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		h.logf("muster unreachable (%v); the question is the terminal's only", err)
		return false
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode != http.StatusCreated {
		h.logf("muster did not take the question (%s: %s); it is the terminal's only", resp.Status, strings.TrimSpace(string(msg)))
		return false
	}
	return true
}
