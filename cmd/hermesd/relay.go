package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/podagent"
)

// The approval relay: a Hermes approval.request filed with muster's approval
// queue (internal/api/approvals.go, the route `ccd approve` uses), which puts a
// card on the operator's phone, then the answer sent back to Hermes.
//
// 🔴 ONLY AN EXPLICIT "approve" FROM muster BECOMES `once`. Everything else —
// no relay configured, muster unreachable, a refused credential, nobody answering
// within the relay timeout, an id muster no longer tracks, a dismissed card, an
// unknown answer, the turn ending — becomes `deny`, sent by hermesd itself.
// Unlike ccd's hook, there is no terminal prompt to fall back to: an approval
// hermesd does not answer is denied by Hermes at approvals.timeout and the run
// is reported as completed (S6), so hermesd's own deny, BEFORE that timeout, is
// the only answer that leaves the outcome visible. muster only ever offers
// `once` and `deny` (open question 1 of the design).

type relayDecision int

const (
	relayDeny relayDecision = iota
	relayApprove
)

// relay files approval requests with muster and waits for the answer.
type relay struct {
	base, token string // "" when the pod is not offered muster's relay
	timeout     time.Duration
	poll        time.Duration
	client      *http.Client
	logf        func(string, ...any)
}

func newRelay(c config) *relay {
	return &relay{
		base: c.ConnectURL, token: c.AgentToken, timeout: c.RelayTimeout, poll: time.Second,
		client: &http.Client{Timeout: 10 * time.Second, CheckRedirect: podagent.NoRedirects.CheckRedirect},
		logf:   log.Printf,
	}
}

// approvalRequest is what an approval.request event carries that the relay
// shows (the command is already redacted by Hermes before egress).
type approvalRequest struct {
	PromptID            string      `json:"prompt_id"`
	ExecutionGeneration json.Number `json:"execution_generation"`
	Command             string      `json:"command"`
	Description         string      `json:"description"`
}

// decide files req and waits until muster answers, the relay times out, or ctx
// ends (the approval settled elsewhere, or the turn ended). cwd and session
// label the card.
func (r *relay) decide(ctx context.Context, req approvalRequest, cwd, session string) relayDecision {
	if r.base == "" || r.token == "" {
		r.logf("hermesd: approval %s: this pod is not offered muster's approval relay; denying", req.PromptID)
		return relayDeny
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	body := map[string]any{"tool": "terminal", "cwd": cwd, "session": session}
	cmd := req.Command
	if len(cmd) > 1500 {
		cmd = cmd[:1500]
	}
	if cmd != "" {
		body["command"] = cmd
	}
	if req.Description != "" {
		body["context"] = []string{req.Description}
	}
	var created struct {
		RequestID string `json:"requestId"`
	}
	if status, err := r.call(ctx, http.MethodPost, "/agent/approvals", body, &created); err != nil || status != http.StatusCreated || created.RequestID == "" {
		r.logf("hermesd: approval %s: filing it with muster failed (status %d, err %v); denying", req.PromptID, status, err)
		return relayDeny
	}
	id := created.RequestID
	decided := false
	defer func() {
		if decided {
			return
		}
		// Clear the card on every non-decision exit: it was answered by the
		// timeout, settled elsewhere, or the turn ended.
		cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer ccancel()
		_, _ = r.call(cctx, http.MethodDelete, "/agent/approvals/"+url.PathEscape(id), nil, nil)
	}()
	for {
		var st struct {
			State    string `json:"state"`
			Response string `json:"response"`
		}
		status, err := r.call(ctx, http.MethodGet, "/agent/approvals/"+url.PathEscape(id), nil, &st)
		switch {
		case ctx.Err() != nil:
			r.logf("hermesd: approval %s: no answer within %s (or the turn ended); denying", req.PromptID, r.timeout)
			return relayDeny
		case err != nil || status == http.StatusBadGateway || status == http.StatusServiceUnavailable:
			// transient: keep polling until the timeout
		case status == http.StatusNotFound:
			r.logf("hermesd: approval %s: muster no longer tracks %s; denying", req.PromptID, id)
			return relayDeny
		case status != http.StatusOK:
			r.logf("hermesd: approval %s: unexpected status %d reading %s; denying", req.PromptID, status, id)
			return relayDeny
		case st.State == "decided":
			decided = true
			r.logf("hermesd: approval %s: muster decided %q", req.PromptID, st.Response)
			if a := strings.ToLower(strings.TrimSpace(st.Response)); a == "approve" || a == "allow" {
				return relayApprove
			}
			return relayDeny
		case st.State == "gone":
			decided = true
			return relayDeny
		case st.State != "pending":
			r.logf("hermesd: approval %s: unknown state %q; denying", req.PromptID, st.State)
			return relayDeny
		}
		select {
		case <-ctx.Done():
		case <-time.After(r.poll):
		}
	}
}

func (r *relay) call(ctx context.Context, method, path string, in, out any) (int, error) {
	var rd io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.base+path, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-Muster-Token", r.token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if out != nil && resp.StatusCode/100 == 2 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("muster's answer is not JSON: %w", err)
		}
	}
	return resp.StatusCode, nil
}
