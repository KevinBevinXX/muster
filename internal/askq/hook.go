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
	// 🔴 THE HOOK NAMES ITS QUESTION ITSELF, so a registration whose answer
	// was lost (a timeout after muster created it) can still be withdrawn —
	// and a retry is the same question, not a second one.
	id := NewID()
	out := []byte(nil)
	settled := false
	// giveUp withdraws the question and reads what the withdraw returns: an
	// answer a person gave in the moment before is delivered after all, so
	// muster never says "sent to Claude" for answers dropped here. Every path
	// that does not end in a delivered answer or a question muster no longer
	// has runs it — a signal included.
	giveUp := func(why string, a ...any) []byte {
		settled = true
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		var got hookState
		status, err := h.call(cctx, http.MethodDelete, h.Path+"/"+url.PathEscape(id), nil, &got)
		if err == nil && status == http.StatusOK && got.State == Answered {
			if o := h.deliver(qs, p.ToolInput, got.Answers); o != nil {
				h.logf("answered in muster just before the hook let go")
				return o
			}
		}
		h.logf(why+"; leaving the question to the terminal", a...)
		return nil
	}
	defer func() {
		if !settled {
			giveUp("interrupted")
		}
	}()
	var st hookState
	status, err := h.call(ctx, http.MethodPost, h.Path, map[string]any{
		"id": id, "sessionId": p.SessionID, "toolUseId": p.ToolUseID, "toolInput": p.ToolInput,
		"holdSeconds": int(hold / time.Second),
	}, &st)
	switch {
	case err != nil || transient(status):
		// muster may have created it before the answer was lost.
		return giveUp("muster did not confirm the question (status %d, %v)", status, err)
	case status != http.StatusCreated || st.ID != id:
		settled = true // refused: nothing was created
		h.logf("muster did not take the question (status %d); leaving it to the terminal", status)
		return nil
	}
	wait := h.Wait
	if wait <= 0 {
		wait = HookWait
	}
	giveUpAfter := h.GiveUpAfter
	if giveUpAfter <= 0 {
		giveUpAfter = 30 * time.Second
	}
	var failingSince time.Time
	for {
		if ctx.Err() != nil {
			return giveUp("interrupted")
		}
		left := deadline.Sub(h.now())
		if left <= 0 {
			return giveUp("no answer within %s", hold)
		}
		w := min(wait, left)
		secs := int((w + time.Second - 1) / time.Second)
		var got hookState
		status, err := h.call(ctx, http.MethodGet, fmt.Sprintf("%s/%s?wait=%d", h.Path, url.PathEscape(id), secs), nil, &got)
		switch {
		case err != nil || transient(status):
			if failingSince.IsZero() {
				failingSince = h.now()
			}
			if h.now().Sub(failingSince) > giveUpAfter {
				return giveUp("muster unreachable for %s (%v, status %d)", giveUpAfter, err, status)
			}
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
			continue
		case status == http.StatusNotFound:
			settled = true // nothing to withdraw
			h.logf("muster no longer has the question; leaving it to the terminal")
			return nil
		case status != http.StatusOK:
			return giveUp("unexpected status %d", status)
		}
		failingSince = time.Time{}
		switch got.State {
		case Pending:
			continue
		case Answered:
			settled = true
			if out = h.deliver(qs, p.ToolInput, got.Answers); out != nil {
				h.logf("answered in muster")
			}
			return out
		default:
			settled = true
			h.logf("the question is %s; leaving it to the terminal", got.State)
			return nil
		}
	}
}

// deliver is the hook output for answers muster says a person gave — checked
// HERE, against the questions this hook holds, before anything reaches the
// CLI — or nil when they do not fit.
func (h Hook) deliver(qs []Question, toolInput json.RawMessage, answers map[string]string) []byte {
	if err := CheckAnswers(qs, answers); err != nil {
		h.logf("muster's answers do not fit the questions (%v)", err)
		return nil
	}
	out, err := HookOutput(toolInput, answers)
	if err != nil {
		h.logf("%v", err)
		return nil
	}
	return out
}

// transient reports whether a status is worth retrying: muster (or a proxy in
// front of it) is restarting, overloaded or briefly unreachable. 500 is
// included: behind the public path a proxy answers it for an upstream that is
// down, and muster's own 500 (a panic recovered) on a poll is safe to retry —
// a poll changes nothing but the hook's liveness.
func transient(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
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
