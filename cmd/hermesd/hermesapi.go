package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// hermesAPI is Hermes's API server as hermesd uses it, at the pin (S6;
// docs/design/hermes.md, "Mode: API server, driven by a supervisor"). It is on
// 127.0.0.1 and every call carries the API key.
type hermesAPI struct {
	base string
	key  string
	// client carries every request/response call, bounded whole
	// (apiCallTimeout): a gateway that accepts a connection and never answers
	// cannot hang a turn, recovery or a health probe.
	client *http.Client
	// stream carries the SSE events stream, which lives as long as the run: it
	// bounds the wait for the response HEADERS only; the turn's own deadline
	// bounds the rest.
	stream *http.Client
}

// apiCallTimeout bounds one non-streaming call to Hermes's API server.
const apiCallTimeout = 30 * time.Second

func newHermesAPI(base, key string) *hermesAPI {
	return &hermesAPI{
		base: base, key: key,
		client: &http.Client{Timeout: apiCallTimeout},
		stream: &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: apiCallTimeout}},
	}
}

// apiError is a non-2xx answer: Hermes's OpenAI-style error envelope
// {"error":{"message","type","code"}}, or the raw body when it is not one.
type apiError struct {
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("hermes API %d %s: %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("hermes API %d: %s", e.Status, e.Message)
}

// isAPIError reports whether err is an apiError with the given status (and code,
// when code is non-empty).
func isAPIError(err error, status int, code string) bool {
	var e *apiError
	return errors.As(err, &e) && e.Status == status && (code == "" || e.Code == code)
}

func (h *hermesAPI) do(ctx context.Context, method, path string, body []byte, header http.Header, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+h.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		return decodeAPIError(resp.StatusCode, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("hermes API %s %s: the answer is not JSON: %w", method, path, err)
		}
	}
	return nil
}

func decodeAPIError(status int, raw []byte) error {
	e := &apiError{Status: status}
	var env struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &env) == nil && (env.Error.Message != "" || env.Error.Code != "") {
		e.Code, e.Message = env.Error.Code, env.Error.Message
	} else {
		e.Message = strings.TrimSpace(string(raw))
		if len(e.Message) > 300 {
			e.Message = e.Message[:300]
		}
	}
	return e
}

// health is GET /health: 200 once the API server answers (about 2 s after
// start, S8).
func (h *hermesAPI) health(ctx context.Context) error {
	return h.do(ctx, http.MethodGet, "/health", nil, nil, nil)
}

// postRun is POST /v1/runs with body exactly as given and the Idempotency-Key
// header. A replay of a key Hermes already admitted answers 202 with the
// ORIGINAL run id and replayed=true; the same key with a different body is 409
// idempotency_key_conflict (P:gateway/platforms/api_server_runs.py:508-517).
func (h *hermesAPI) postRun(ctx context.Context, idemKey string, body []byte) (runID string, replayed bool, err error) {
	var out struct {
		RunID    string `json:"run_id"`
		Status   string `json:"status"`
		Replayed bool   `json:"replayed"`
	}
	if err := h.do(ctx, http.MethodPost, "/v1/runs", body, http.Header{"Idempotency-Key": {idemKey}}, &out); err != nil {
		return "", false, err
	}
	if out.RunID == "" {
		return "", false, errors.New("hermes API POST /v1/runs: the 202 names no run_id")
	}
	return out.RunID, out.Replayed, nil
}

// runStatus is GET /v1/runs/{id} at the pin: the session authority's
// projection (P:gateway/platforms/api_server_authority_runs.py:68-97).
// `unknown` is NOT terminal: later runs on the session wait behind it until
// resolve-unknown.
type runStatus struct {
	RunID               string      `json:"run_id"`
	Status              string      `json:"status"`
	SessionID           string      `json:"session_id"`
	AdmissionID         string      `json:"admission_id"`
	ExecutionGeneration json.Number `json:"execution_generation"`
	Output              string      `json:"output"`
	Error               string      `json:"error"`
}

// terminalStatus reports whether a run in this status will not change again.
func terminalStatus(s string) bool {
	switch s {
	case "completed", "failed", "cancelled", "interrupted":
		return true
	}
	return false
}

func (h *hermesAPI) getRun(ctx context.Context, runID string) (runStatus, error) {
	var st runStatus
	err := h.do(ctx, http.MethodGet, "/v1/runs/"+url.PathEscape(runID), nil, nil, &st)
	return st, err
}

// resolveUnknown is POST /v1/runs/{id}/resolve-unknown with EXACTLY
// {admission_id, execution_generation} (any other key set is invalid_params,
// P:gateway/platforms/api_server_authority_runs.py:176).
func (h *hermesAPI) resolveUnknown(ctx context.Context, runID, admissionID string, generation json.Number) (string, error) {
	body, err := json.Marshal(struct {
		AdmissionID         string      `json:"admission_id"`
		ExecutionGeneration json.Number `json:"execution_generation"`
	}{admissionID, generation})
	if err != nil {
		return "", err
	}
	var out struct {
		Outcome string `json:"outcome"`
		Status  string `json:"status"`
	}
	if err := h.do(ctx, http.MethodPost, "/v1/runs/"+url.PathEscape(runID)+"/resolve-unknown", body, nil, &out); err != nil {
		return "", err
	}
	return out.Outcome, nil
}

// approvalBody is the EXACT body the pin's approval route accepts:
// set(body) == {request_id, execution_generation, choice}, anything else is
// 400 invalid_params (P:gateway/platforms/api_server_authority_runs.py:113-133,
// measured in S6). request_id is the approval.request event's prompt_id.
type approvalBody struct {
	RequestID           string      `json:"request_id"`
	ExecutionGeneration json.Number `json:"execution_generation"`
	Choice              string      `json:"choice"`
}

func (h *hermesAPI) answerApproval(ctx context.Context, runID string, a approvalBody) error {
	body, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return h.do(ctx, http.MethodPost, "/v1/runs/"+url.PathEscape(runID)+"/approval", body, nil, nil)
}

// stopRun is POST /v1/runs/{id}/stop: interrupt the run (the session
// authority's stop, P:gateway/platforms/api_server_runs.py _handle_stop_run).
func (h *hermesAPI) stopRun(ctx context.Context, runID string) error {
	return h.do(ctx, http.MethodPost, "/v1/runs/"+url.PathEscape(runID)+"/stop", []byte("{}"), nil, nil)
}

// sessionMessage is one row of GET /api/sessions/{id}/messages.
type sessionMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// sessionMessages is the newest page of a session's messages. A session Hermes
// does not know is 404.
func (h *hermesAPI) sessionMessages(ctx context.Context, sessionID string) ([]sessionMessage, error) {
	var out struct {
		Data []sessionMessage `json:"data"`
	}
	err := h.do(ctx, http.MethodGet, "/api/sessions/"+url.PathEscape(sessionID)+"/messages", nil, nil, &out)
	return out.Data, err
}

// runEvent is one event of GET /v1/runs/{id}/events: `data: {"event": <name>,
// "run_id", "timestamp", …fields}` (P:gateway/platforms/api_server_runs.py:181).
type runEvent struct {
	Event string
	Raw   json.RawMessage
}

// streamEvents opens the run's event stream and calls fn for each event until
// the stream ends, fn returns false, or ctx ends. It returns nil when the
// stream closed (Hermes closes it after the terminal event), or the read error.
func (h *hermesAPI) streamEvents(ctx context.Context, runID string, fn func(runEvent) bool) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.base+"/v1/runs/"+url.PathEscape(runID)+"/events", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+h.key)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := h.stream.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return decodeAPIError(resp.StatusCode, raw)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	var data strings.Builder
	flush := func() bool {
		if data.Len() == 0 {
			return true
		}
		raw := json.RawMessage(data.String())
		data.Reset()
		var head struct {
			Event string `json:"event"`
		}
		if json.Unmarshal(raw, &head) != nil || head.Event == "" {
			return true
		}
		return fn(runEvent{Event: head.Event, Raw: raw})
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if !flush() {
				return nil
			}
		case strings.HasPrefix(line, ":"):
			// a comment: `: open`, `: keepalive`, `: stream closed`
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	flush()
	return nil
}
