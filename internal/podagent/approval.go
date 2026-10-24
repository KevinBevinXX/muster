package podagent

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The approval-queue client: a permission prompt filed with muster's approval
// relay (internal/api/approvals.go), which puts a card on the operator's phone,
// then polled until it is decided. ccd's PermissionRequest hook uses this core
// and keeps its OWN fallback for everything that is not an explicit answer (the
// terminal prompt).
//
// 🔴 ONLY AN EXPLICIT "approve" (or "allow") FROM muster IS ApprovalApproved.
// Every other exit — muster unreachable, a refused credential, nobody answering
// before the deadline, an id muster no longer tracks, a dismissed card, an
// unknown answer, ctx ending — is ApprovalUndecided or ApprovalRejected, never
// an allow.
//
// The pod holds no credential for the approval queue itself; it authenticates
// with its OWN per-agent token, and muster can only file requests and read back
// the ones this agent filed.

// ApprovalDecision is what the relay concluded.
type ApprovalDecision int

const (
	// ApprovalUndecided: no answer (the caller's fallback applies).
	ApprovalUndecided ApprovalDecision = iota
	ApprovalApproved
	ApprovalRejected
)

// ApprovalRelay files one prompt and waits for its answer.
type ApprovalRelay struct {
	Base, Token string
	// Deadline bounds the wait for an answer.
	Deadline time.Duration
	Poll     time.Duration
	Client   *http.Client
	Logf     func(format string, args ...any)
	// Fallback names what the caller does with ApprovalUndecided, for the log
	// ("deferring", "denying"); TimeoutFallback is the same for the deadline
	// ("deferring to the terminal prompt").
	Fallback, TimeoutFallback string
}

// ApprovalResponse maps the router's decision vocabulary onto a decision.
// 🔴 ONLY THE TWO SPELLINGS A HUMAN'S APPROVE CAN PRODUCE MAP TO APPROVED.
// Anything else, including "ignore" (a dismissed card) and an empty value, is
// undecided.
func ApprovalResponse(resp string) ApprovalDecision {
	switch strings.ToLower(strings.TrimSpace(resp)) {
	case "approve", "allow":
		return ApprovalApproved
	case "reject", "deny":
		return ApprovalRejected
	}
	return ApprovalUndecided
}

// Decide files req (the body muster's POST /agent/approvals takes) and waits
// for the answer, clearing the card on every exit that is not a decision.
func (r ApprovalRelay) Decide(ctx context.Context, req map[string]any) ApprovalDecision {
	var created struct {
		RequestID string `json:"requestId"`
	}
	if status, _, err := MusterCall(ctx, r.Client, http.MethodPost, r.Base, r.Token, "/agent/approvals", req, &created); err != nil ||
		status != http.StatusCreated || created.RequestID == "" {
		r.Logf("filing the request failed (status %d, err %v); %s", status, err, r.Fallback)
		return ApprovalUndecided
	}
	id := created.RequestID
	decided := false
	defer func() {
		if decided {
			return
		}
		// Clear the card on every non-decision exit, including a signal: the
		// prompt was answered elsewhere, timed out, or the session ended.
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _, _ = MusterCall(cctx, r.Client, http.MethodDelete, r.Base, r.Token, "/agent/approvals/"+url.PathEscape(id), nil, nil)
	}()

	stopAt := time.Now().Add(r.Deadline)
	for {
		if ctx.Err() != nil {
			r.Logf("interrupted; %s", r.Fallback)
			return ApprovalUndecided
		}
		if time.Now().After(stopAt) {
			r.Logf("no answer within %s; %s", r.Deadline, r.TimeoutFallback)
			return ApprovalUndecided
		}
		var st struct {
			State    string `json:"state"`
			Response string `json:"response"`
		}
		status, _, err := MusterCall(ctx, r.Client, http.MethodGet, r.Base, r.Token, "/agent/approvals/"+url.PathEscape(id), nil, &st)
		switch {
		case err != nil || status == http.StatusBadGateway || status == http.StatusServiceUnavailable:
			// transient: keep polling until the deadline
		case status == http.StatusNotFound:
			r.Logf("muster no longer tracks %s; %s", id, r.Fallback)
			return ApprovalUndecided
		case status != http.StatusOK:
			r.Logf("unexpected status %d reading %s; %s", status, id, r.Fallback)
			return ApprovalUndecided
		case st.State == "decided":
			decided = true
			r.Logf("%s decided %q", id, st.Response)
			return ApprovalResponse(st.Response)
		case st.State == "gone":
			// No DELETE: muster answers "gone" only after it has confirmed the
			// card over several reads, forgotten the id and cleared the card
			// itself, so a DELETE from here could only 404.
			decided = true
			r.Logf("%s is gone; %s", id, r.Fallback)
			return ApprovalUndecided
		case st.State != "pending":
			r.Logf("unknown state %q for %s; %s", st.State, id, r.Fallback)
			return ApprovalUndecided
		}
		select {
		case <-ctx.Done():
		case <-time.After(r.Poll):
		}
	}
}
