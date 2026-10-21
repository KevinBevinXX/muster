package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/ZacxDev/muster/internal/podagent"
)

// The approval relay: a Hermes approval.request filed with muster's approval
// queue through podagent.ApprovalRelay (the core `ccd approve` uses), then the
// answer sent back to Hermes.
//
// 🔴 hermesd's FALLBACK IS DENY, NOT A TERMINAL PROMPT. Only an explicit
// approve from muster becomes `once`. A reject, no relay configured, muster
// unreachable, nobody answering within the relay timeout, a dismissed card, the
// turn ending — all become `deny`, sent by hermesd itself. An approval hermesd
// does not answer is denied by Hermes at approvals.timeout and the run is
// reported as completed (S6), so hermesd's own deny, BEFORE that timeout, is
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

// approvalRequest is what an approval prompt carries that the relay shows —
// the approval.request event's fields, and a pending_controls entry's (the same
// dict at the pin, P:gateway/session_pending_controls.py register). The command
// is already redacted by Hermes before egress.
type approvalRequest struct {
	PromptID            string      `json:"prompt_id"`
	ExecutionGeneration json.Number `json:"execution_generation"`
	Command             string      `json:"command"`
	Description         string      `json:"description"`
}

// decide files req and waits until muster answers, the relay times out (its
// window counted from started, the moment hermesd first saw the prompt), or
// ctx ends (the approval settled elsewhere, or the turn ended). cwd and
// session label the card.
func (r *relay) decide(ctx context.Context, req approvalRequest, started time.Time, cwd, session string) relayDecision {
	if r.base == "" || r.token == "" {
		r.logf("hermesd: approval %s: this pod is not offered muster's approval relay; denying", req.PromptID)
		return relayDeny
	}
	window := r.timeout - time.Since(started)
	if window <= 0 {
		r.logf("hermesd: approval %s: its relay window (%s) has passed; denying", req.PromptID, r.timeout)
		return relayDeny
	}
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
	d := podagent.ApprovalRelay{
		Base: r.base, Token: r.token, Deadline: window, Poll: r.poll, Client: r.client,
		Logf:     func(f string, v ...any) { r.logf("hermesd: approval "+req.PromptID+": "+f, v...) },
		Fallback: "denying", TimeoutFallback: "denying",
	}.Decide(ctx, body)
	if d == podagent.ApprovalApproved {
		return relayApprove
	}
	return relayDeny
}
