package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ZacxDev/muster/internal/podagent"
)

// `ccd approve` is the claude-code pod's PermissionRequest hook: it files the
// prompt with muster's approval relay (internal/api/approvals.go), which puts a
// card on the operator's phone, and waits for the answer: a human's, or the
// approval queue's own auto-approve when the operator has a window open that
// covers this pod (then the first poll already reads "approve").
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

// approvalResponse maps the router's decision vocabulary onto the hook's,
// through podagent.ApprovalResponse: 🔴 only the two spellings a human's approve
// can produce map to allow; anything else, including "ignore" (a dismissed card)
// and an empty value, defers.
func approvalResponse(resp string) approvalDecision {
	return fromRelay(podagent.ApprovalResponse(resp))
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

	// The file → poll → clear-on-non-decision core is podagent's; ccd's fallback for everything undecided is the terminal prompt.
	return fromRelay(podagent.ApprovalRelay{
		Base: a.base, Token: a.token, Deadline: a.deadline, Poll: a.poll, Client: a.client, Logf: a.logf,
		Fallback: "deferring", TimeoutFallback: "deferring to the terminal prompt",
	}.Decide(ctx, req))
}

// fromRelay maps the core's decision onto the hook's: undecided defers.
func fromRelay(d podagent.ApprovalDecision) approvalDecision {
	switch d {
	case podagent.ApprovalApproved:
		return decisionAllow
	case podagent.ApprovalRejected:
		return decisionDeny
	}
	return decisionDefer
}
