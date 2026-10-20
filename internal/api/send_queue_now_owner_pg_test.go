package api

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sendqueue"
)

// TestAnotherOwnersSendNowIsNotFound: user 2's Send now of user 1's queued
// message is 404 — not "exists, not yours" — and leaves it queued and
// unpasted; user 1's own is the control. (It calls the handler directly, with
// each user's principal: the password-mode fixture has one user's cookie.)
func TestAnotherOwnersSendNowIsNotFound(t *testing.T) {
	f := newSQFixture(t)
	f.mirror.agentState = &mirror.AgentState{Status: mirror.StatusBusy, Since: time.Now().UnixMilli()}
	it := f.enqueue(t, "user one's, sent now", time.Minute, 0)
	call := func(uid int64) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, sendNowPath(it.ID), nil)
		req.SetPathValue("id", strconv.FormatInt(it.ID, 10))
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		rec := httptest.NewRecorder()
		f.srv.handleSendQueueSendNow(rec, req.WithContext(asQueueUser(uid, time.Now())))
		return rec
	}
	if rec := call(2); rec.Code != http.StatusNotFound {
		t.Fatalf("user 2's Send now of user 1's message: %d %q", rec.Code, rec.Body.String())
	}
	if got := f.row(t, it.ID); got.State != sendqueue.Queued || len(f.pasted()) != 0 {
		t.Fatalf("user 2's Send now moved user 1's message: %s, pasted %q", got.State, f.pasted())
	}
	if rec := call(1); rec.Code != http.StatusNoContent {
		t.Fatalf("CONTROL: user 1's own Send now: %d %q", rec.Code, rec.Body.String())
	}
	if got := f.row(t, it.ID); got.State != sendqueue.Sent || len(f.pasted()) != 1 {
		t.Fatalf("CONTROL: user 1's own: %s, pasted %q", got.State, f.pasted())
	}
}
