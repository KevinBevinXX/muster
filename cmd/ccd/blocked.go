package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// "Blocked on approval": the session's TUI is showing a permission prompt.
//
// The signal is the CLI's Notification hook with notification_type
// "permission_prompt" (images/claude-code-agent/settings.json registers it with
// that matcher). It is CLEARED by any later hook that proves the session moved
// on: PostToolUse (the tool ran, so it was allowed), Stop / StopFailure (the
// turn ended, e.g. after a deny), UserPromptSubmit, SessionStart and SessionEnd.
//
// ccd reports the state to muster (POST /agent/blocked with the agent's own
// token), which shows an "awaiting approval" badge on the agent. Reporting is
// best-effort and never blocks a hook: a missed report only costs the badge.
// While blocked, ccd re-reports every blockedHeartbeat so a muster restart does
// not lose the badge and a pod that died blocked stops showing it (muster
// expires a report it has not seen refreshed).

// blockedHeartbeat is how often a still-blocked state is re-reported.
const blockedHeartbeat = time.Minute

// trackApprovalLocked updates awaitingApproval from one hook and reports a
// change. Caller holds s.mu.
func (s *server) trackApprovalLocked(ev hookEvent) {
	was := s.awaitingApproval
	switch strings.ToLower(ev.Event) {
	case "notification":
		if ev.NotificationType == "permission_prompt" {
			s.awaitingApproval = true
		}
	case "posttooluse", "stop", "stopfailure", "userpromptsubmit", "sessionstart", "sessionend":
		s.awaitingApproval = false
	}
	if s.awaitingApproval != was {
		if s.awaitingApproval {
			s.approvalSince = s.clock()
		} else {
			s.approvalSince = time.Time{}
		}
		if s.blocked != nil {
			s.blocked.setFrom(s.blockedSrc, s.awaitingApproval)
		}
	}
}

// awaitingSince is when the session's current permission prompt appeared, and
// whether one is showing.
func (s *server) awaitingSince() (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.approvalSince, s.awaitingApproval
}

// blockedReporter posts the latest blocked state to muster from one goroutine.
//
// 🔴 THE STATE IS THE AGENT'S, SO IT IS THE OR OF EVERY CLI IN THE POD: the
// primary and each lane (lanes.go) is a SOURCE (setFrom), and muster's one
// "awaiting approval" badge shows while any of them is at a permission prompt.
// Before, only the primary reported, so a lane stuck at a prompt was invisible
// — and, being busy, unstoppable — while it refused new sessions at the cap.
type blockedReporter struct {
	base, token string
	client      *http.Client
	heartbeat   time.Duration
	ch          chan bool // capacity 1: the latest state wins
	logf        func(format string, args ...any)

	mu      sync.Mutex
	sources map[string]bool // source ("" the primary, else a lane id) -> blocked
	any     bool            // the OR last sent
}

// setFrom records one source's state and reports the OR when it changes. A
// source that goes away (a stopped lane) reports false.
func (r *blockedReporter) setFrom(src string, blocked bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sources == nil {
		r.sources = map[string]bool{}
	}
	if blocked {
		r.sources[src] = true
	} else {
		delete(r.sources, src)
	}
	if now := len(r.sources) > 0; now != r.any {
		r.any = now
		r.set(now)
	}
}

func newBlockedReporter(base, token string) *blockedReporter {
	return &blockedReporter{
		base: strings.TrimRight(base, "/"), token: token,
		client:    &http.Client{Timeout: 5 * time.Second, CheckRedirect: noRedirects.CheckRedirect},
		heartbeat: blockedHeartbeat,
		ch:        make(chan bool, 1),
		logf:      log.Printf,
	}
}

// set records the latest state without blocking. Its one caller, setFrom, holds
// r.mu, so drain-then-send cannot block.
func (r *blockedReporter) set(blocked bool) {
	select {
	case <-r.ch:
	default:
	}
	r.ch <- blocked
}

func (r *blockedReporter) run(ctx context.Context) {
	cur := false
	t := time.NewTicker(r.heartbeat)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case cur = <-r.ch:
			r.post(ctx, cur)
		case <-t.C:
			if cur {
				r.post(ctx, cur)
			}
		}
	}
}

func (r *blockedReporter) post(ctx context.Context, blocked bool) {
	b, _ := json.Marshal(map[string]bool{"blocked": blocked})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.base+"/agent/blocked", bytes.NewReader(b))
	if err != nil {
		return
	}
	req.Header.Set("X-Muster-Token", r.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		r.logf("ccd: reporting blocked=%v to muster: %v", blocked, err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		r.logf("ccd: reporting blocked=%v to muster: status %d", blocked, resp.StatusCode)
	}
}
