package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// `ccd approve` is the claude-code pod's PermissionRequest hook: it files the
// prompt with muster's approval relay (internal/api/approvals.go), which puts a
// card on the operator's phone, and waits for the human's answer.
//
// 🔴 THE ONLY OUTPUT THAT ALLOWS A TOOL CALL IS approveAllow, AND IT IS PRINTED
// ONLY FOR AN EXPLICIT "approve" READ FROM muster. Every other exit — no relay
// configured, muster unreachable, a refused credential, a request nobody
// answered before the deadline, an id muster no longer tracks, a dismissed
// card, an unknown answer, a signal, a panic — prints NOTHING and exits 0, which
// the CLI treats as "no decision": the normal in-terminal prompt is shown (the
// operator answers it in muster's terminal view). Measured contract of the
// pinned CLI's hook docs: exit 2 is NOT honoured for PermissionRequest and any
// other failure is a non-blocking error, so no failure mode of this command can
// turn into an allow. A timed-out hook's output is discarded.
//
// The pod holds no credential for the approval queue itself; it authenticates
// to muster with its OWN per-agent token (MUSTER_HOOK_TOKEN), and muster can
// only file requests and read back the ones this agent filed.

const (
	approveAllow = `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}`
	approveDeny  = `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"deny"}}}`

	// defaultApprovalDeadline stays under the hook's configured timeout (180s in
	// images/claude-code-agent/settings.json), so the hook defers by itself and
	// clears its card instead of being killed.
	defaultApprovalDeadline = 170 * time.Second
)

// permissionEvent is the subset of the PermissionRequest hook payload read here.
type permissionEvent struct {
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
	PermissionMode string          `json:"permission_mode"`
	Cwd            string          `json:"cwd"`
	SessionID      string          `json:"session_id"`
}

// approvalDecision is what the hook concluded.
type approvalDecision int

const (
	decisionDefer approvalDecision = iota // print nothing: the terminal prompt
	decisionAllow
	decisionDeny
)

type approver struct {
	base, token string
	deadline    time.Duration
	poll        time.Duration
	client      *http.Client
	logf        func(format string, args ...any)
}

func runApprove(args []string, stdin io.Reader, stdout, stderr io.Writer) (code int) {
	// A panic must never escape as a non-zero exit with partial output; it is a
	// deferral like any other failure.
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintln(stderr, "ccd approve: internal error, deferring to the terminal prompt:", r)
			code = 0
		}
	}()
	if len(args) != 0 {
		fmt.Fprintln(stderr, "usage: ccd approve  (the PermissionRequest hook; reads the hook payload on stdin)")
		return 0
	}
	deadline, err := envDuration("CCD_APPROVAL_DEADLINE", defaultApprovalDeadline)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 0
	}
	a := approver{
		base:     strings.TrimRight(os.Getenv(envConnectURL), "/"),
		token:    os.Getenv(envAgentToken),
		deadline: deadline,
		poll:     time.Second,
		client:   &http.Client{Timeout: 10 * time.Second, CheckRedirect: noRedirects.CheckRedirect},
		logf:     func(f string, v ...any) { fmt.Fprintf(stderr, "ccd approve: "+f+"\n", v...) },
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer stop()
	body, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
	if err != nil {
		a.logf("read stdin: %v; deferring", err)
		return 0
	}
	var ev permissionEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		a.logf("payload is not JSON; deferring")
		return 0
	}
	switch a.decide(ctx, ev) {
	case decisionAllow:
		fmt.Fprint(stdout, approveAllow)
	case decisionDeny:
		fmt.Fprint(stdout, approveDeny)
	}
	return 0
}

// approvalResponse maps the router's decision vocabulary onto the hook's.
// 🔴 ONLY THE TWO SPELLINGS A HUMAN'S APPROVE CAN PRODUCE MAP TO ALLOW. Anything
// else, including "ignore" (a dismissed card) and an empty value, defers.
func approvalResponse(resp string) approvalDecision {
	switch strings.ToLower(strings.TrimSpace(resp)) {
	case "approve", "allow":
		return decisionAllow
	case "reject", "deny":
		return decisionDeny
	}
	return decisionDefer
}

// decide files the request and waits for the answer.
func (a approver) decide(ctx context.Context, ev permissionEvent) approvalDecision {
	if a.base == "" || a.token == "" {
		a.logf("%s or %s is not set; this pod is not offered muster's approval relay. Deferring.", envConnectURL, envAgentToken)
		return decisionDefer
	}
	switch ev.PermissionMode {
	case "bypassPermissions", "plan":
		a.logf("permission_mode=%s; deferring", ev.PermissionMode)
		return decisionDefer
	}
	// An interactive question asks the user to PICK an answer; allow/deny cannot
	// carry one, so it must be answered at the terminal.
	if ev.ToolName == "AskUserQuestion" || ev.ToolName == "" {
		return decisionDefer
	}

	req := map[string]any{"tool": ev.ToolName, "cwd": ev.Cwd, "session": ev.SessionID}
	var in struct {
		Command     string `json:"command"`
		Description string `json:"description"`
	}
	_ = json.Unmarshal(ev.ToolInput, &in)
	if in.Command != "" {
		req["command"] = in.Command
	} else {
		raw := bytes.TrimSpace(ev.ToolInput)
		if len(raw) > 1500 {
			raw = raw[:1500]
		}
		req["input"] = string(raw)
	}
	if in.Description != "" {
		req["context"] = []string{in.Description}
	}

	var created struct {
		RequestID string `json:"requestId"`
	}
	if status, err := a.call(ctx, http.MethodPost, "/agent/approvals", req, &created); err != nil || status != http.StatusCreated || created.RequestID == "" {
		a.logf("filing the request failed (status %d, err %v); deferring", status, err)
		return decisionDefer
	}
	id := created.RequestID
	decided := false
	defer func() {
		if decided {
			return
		}
		// Clear the card on every non-decision exit, including a signal: the
		// prompt was answered in the terminal, or the session ended.
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = a.call(cctx, http.MethodDelete, "/agent/approvals/"+url.PathEscape(id), nil, nil)
	}()

	stopAt := time.Now().Add(a.deadline)
	for {
		if ctx.Err() != nil {
			a.logf("interrupted; deferring")
			return decisionDefer
		}
		if time.Now().After(stopAt) {
			a.logf("no answer within %s; deferring to the terminal prompt", a.deadline)
			return decisionDefer
		}
		var st struct {
			State    string `json:"state"`
			Response string `json:"response"`
		}
		status, err := a.call(ctx, http.MethodGet, "/agent/approvals/"+url.PathEscape(id), nil, &st)
		switch {
		case err != nil || status == http.StatusBadGateway || status == http.StatusServiceUnavailable:
			// transient: keep polling until the deadline
		case status == http.StatusNotFound:
			a.logf("muster no longer tracks %s; deferring", id)
			return decisionDefer
		case status != http.StatusOK:
			a.logf("unexpected status %d reading %s; deferring", status, id)
			return decisionDefer
		case st.State == "decided":
			decided = true
			d := approvalResponse(st.Response)
			a.logf("%s decided %q", id, st.Response)
			return d
		case st.State == "gone":
			// No DELETE: muster answers "gone" only after it has confirmed the card
			// over several reads, forgotten the id and cleared the card itself, so a
			// DELETE from here could only 404.
			decided = true
			a.logf("%s is gone; deferring", id)
			return decisionDefer
		case st.State != "pending":
			a.logf("unknown state %q for %s; deferring", st.State, id)
			return decisionDefer
		}
		select {
		case <-ctx.Done():
		case <-time.After(a.poll):
		}
	}
}

// call sends one request to muster with this agent's own token.
func (a approver) call(ctx context.Context, method, path string, in, out any) (int, error) {
	var rd io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-Muster-Token", a.token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client.Do(req)
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
