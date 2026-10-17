package askq

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// THE HOOK CLIENT: what a PreToolUse hook on AskUserQuestion runs (today
// `muster-mirror ask`). It registers the call's questions with muster, holds the
// call while it long-polls for answers, and returns HookOutput for answers a
// person gave — or NOTHING.
//
// 🔴 NOTHING IS THE ANSWER TO EVERY FAILURE, AND IT IS THE FAIL-OPEN PATH. A hook
// that prints nothing leaves the call to the CLI's own picker (measured: the
// picker is drawn the moment the hook returns). So: muster unreachable, a
// refused registration (an unknown session, a read-only host, a question set
// muster will not show), the hold's deadline, a withdrawn or forgotten question,
// answers that do not fit the questions, a signal — each returns nil, and
// withdraws the question when one was registered so muster stops offering it.
// The hold is bounded by Hold; the CLI's own hook timeout must exceed it, or the
// CLI kills the hook first (whose output it then discards — the picker again).

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
	// Base is muster's base URL; Path the ask routes' prefix ("/mirror/ask").
	Base, Path string
	// Auth sets the request's credential.
	Auth func(*http.Request)
	// Hold bounds how long the call is held for an answer.
	Hold time.Duration
	// Wait is one long-poll's wait; GiveUpAfter how long muster may stay
	// unreachable mid-hold before the hook lets go.
	Wait, GiveUpAfter time.Duration
	Client            *http.Client
	Logf              func(format string, a ...any)
	// Now is the clock (tests).
	Now func() time.Time
}

func (h Hook) logf(format string, a ...any) {
	if h.Logf != nil {
		h.Logf(format, a...)
	}
}

func (h Hook) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

type hookState struct {
	ID      string            `json:"id"`
	State   State             `json:"state"`
	Answers map[string]string `json:"answers"`
}

// Run handles one hook payload: the stdout to print, or nil for none.
func (h Hook) Run(ctx context.Context, payload []byte) []byte {
	var p HookPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		h.logf("the payload is not JSON; leaving the question to the terminal")
		return nil
	}
	if p.ToolName != ToolName || (p.HookEventName != "" && p.HookEventName != "PreToolUse") {
		return nil
	}
	qs, err := ParseInput(p.ToolInput)
	if err != nil {
		h.logf("%v; leaving the question to the terminal", err)
		return nil
	}
	if h.Base == "" || h.Auth == nil {
		h.logf("no muster URL or credential; leaving the question to the terminal")
		return nil
	}
	hold := h.Hold
	if hold <= 0 || hold > MaxHold {
		hold = MaxHold
	}
	deadline := h.now().Add(hold)
	var st hookState
	status, err := h.call(ctx, http.MethodPost, h.Path, map[string]any{
		"sessionId": p.SessionID, "toolUseId": p.ToolUseID, "toolInput": p.ToolInput,
		"holdSeconds": int(hold / time.Second),
	}, &st)
	if err != nil || status != http.StatusCreated || st.ID == "" {
		h.logf("muster did not take the question (status %d, %v); leaving it to the terminal", status, err)
		return nil
	}
	id := st.ID
	answered := false
	defer func() {
		if answered {
			return
		}
		// Let muster know the terminal has it now — including on a signal.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, _ = h.call(cctx, http.MethodDelete, h.Path+"/"+url.PathEscape(id), nil, nil)
	}()
	wait := h.Wait
	if wait <= 0 {
		wait = 20 * time.Second
	}
	giveUp := h.GiveUpAfter
	if giveUp <= 0 {
		giveUp = 30 * time.Second
	}
	var failingSince time.Time
	for {
		if ctx.Err() != nil {
			h.logf("interrupted; leaving the question to the terminal")
			return nil
		}
		left := deadline.Sub(h.now())
		if left <= 0 {
			h.logf("no answer within %s; leaving the question to the terminal", hold)
			return nil
		}
		w := min(wait, left)
		secs := int((w + time.Second - 1) / time.Second)
		var got hookState
		status, err := h.call(ctx, http.MethodGet, fmt.Sprintf("%s/%s?wait=%d", h.Path, url.PathEscape(id), secs), nil, &got)
		switch {
		case err != nil || status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusTooManyRequests:
			if failingSince.IsZero() {
				failingSince = h.now()
			}
			if h.now().Sub(failingSince) > giveUp {
				h.logf("muster unreachable for %s (%v, status %d); leaving the question to the terminal", giveUp, err, status)
				return nil
			}
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
			continue
		case status == http.StatusNotFound:
			answered = true // nothing to withdraw
			h.logf("muster no longer has the question; leaving it to the terminal")
			return nil
		case status != http.StatusOK:
			h.logf("unexpected status %d; leaving the question to the terminal", status)
			return nil
		}
		failingSince = time.Time{}
		switch got.State {
		case Pending:
			continue
		case Answered:
			// 🔴 Checked HERE, against the questions this hook holds, before
			// anything reaches the CLI.
			if err := CheckAnswers(qs, got.Answers); err != nil {
				h.logf("muster's answers do not fit the questions (%v); leaving the question to the terminal", err)
				return nil
			}
			out, err := HookOutput(p.ToolInput, got.Answers)
			if err != nil {
				h.logf("%v; leaving the question to the terminal", err)
				return nil
			}
			answered = true
			h.logf("answered in muster")
			return out
		default:
			answered = got.State == Withdrawn
			h.logf("the question is %s; leaving it to the terminal", got.State)
			return nil
		}
	}
}

// call sends one request; out is decoded from a 2xx answer.
func (h Hook) call(ctx context.Context, method, path string, in, out any) (int, error) {
	var rd io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(h.Base, "/")+path, rd)
	if err != nil {
		return 0, err
	}
	h.Auth(req)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := h.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if out != nil && resp.StatusCode/100 == 2 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("muster's answer is not JSON: %w", err)
		}
	}
	return resp.StatusCode, nil
}
