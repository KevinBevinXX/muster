package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sendqueue"
)

// TestSendNowIsRefusedWhileClaudeCodeIsWaitingOnThePerson: a Send now into a
// window whose Claude Code is WAITING (a permission prompt, a question) is 409
// saying so — the paste's Enter would answer the prompt — and the row stays
// queued with nothing pasted; a browser form is sent back with the notice. The
// control is the same row once the window is busy (a Send now goes in while
// busy): sent.
func TestSendNowIsRefusedWhileClaudeCodeIsWaitingOnThePerson(t *testing.T) {
	f := newSQFixture(t)
	f.mirror.agentState = &mirror.AgentState{Status: mirror.StatusWaiting, Since: time.Now().UnixMilli()}
	it := f.enqueue(t, "not into a prompt", time.Minute, 0)
	cookie := sessionCookieFor(f.srv)
	resp, body := f.retry(t, sendNowPath(it.ID), cookie, true, nil)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body, "Claude Code is waiting for you") {
		t.Fatalf("a Send now while waiting: %d %q; want 409 saying it is waiting", resp.StatusCode, body)
	}
	if got := f.row(t, it.ID); got.State != sendqueue.Queued || len(f.pasted()) != 0 {
		t.Fatalf("after the refusal: row %s, pasted %q; want queued, nothing pasted", got.State, f.pasted())
	}
	resp, _ = f.retry(t, sendNowPath(it.ID), cookie, false, nil)
	if resp.StatusCode != http.StatusSeeOther || !strings.Contains(resp.Header.Get("Location"), "notice=waiting-now") {
		t.Fatalf("a Send now while waiting from a browser form: %d (Location %q)", resp.StatusCode, resp.Header.Get("Location"))
	}
	if got := f.row(t, it.ID); got.State != sendqueue.Queued || len(f.pasted()) != 0 {
		t.Fatalf("after the form's refusal: row %s, pasted %q", got.State, f.pasted())
	}
	f.mirror.mu.Lock()
	f.mirror.agentState = &mirror.AgentState{Status: mirror.StatusBusy, Since: time.Now().UnixMilli()}
	f.mirror.mu.Unlock()
	if resp, body := f.retry(t, sendNowPath(it.ID), cookie, true, nil); resp.StatusCode != http.StatusNoContent || len(f.pasted()) != 1 {
		t.Fatalf("CONTROL, busy: %d %q, pasted %q", resp.StatusCode, body, f.pasted())
	}
}

// TestASendNowWhoseBrowserLeavesMidPasteStillEndsSent: the request's context
// is cancelled while the paste is in flight (a phone locked, a tab switched).
// Once claimed, the send does not depend on the browser: the row ends SENT —
// not failed, which would spend its one Retry on a message on its way.
func TestASendNowWhoseBrowserLeavesMidPasteStillEndsSent(t *testing.T) {
	f := newSQFixture(t)
	f.mirror.agentState = &mirror.AgentState{Status: mirror.StatusBusy, Since: time.Now().UnixMilli()}
	it := f.enqueue(t, "sent though the page went away", time.Minute, 0)
	ctx, cancel := context.WithCancel(asQueueUser(1, time.Now()))
	defer cancel()
	f.mirror.mu.Lock()
	f.mirror.inPaste = cancel
	f.mirror.mu.Unlock()
	req := httptest.NewRequest(http.MethodPost, sendNowPath(it.ID), nil)
	req.SetPathValue("id", strconv.FormatInt(it.ID, 10))
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	rec := httptest.NewRecorder()
	f.srv.handleSendQueueSendNow(rec, req.WithContext(ctx))
	if ctx.Err() == nil {
		t.Fatal("instrument: the request context was never cancelled during the paste")
	}
	if got := f.row(t, it.ID); got.State != sendqueue.Sent || len(f.pasted()) != 1 {
		t.Fatalf("after the browser left mid-paste: row %s %q, pasted %q (answer %d); want sent", got.State, got.Detail, f.pasted(), rec.Code)
	}
}
