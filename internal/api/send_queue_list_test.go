package api

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/sendqueue"
	"github.com/ZacxDev/muster/internal/ui"
)

// TestTheQueueListOfAnEmptyWindowIsAnswered (2026-10-19, "PANIC GET
// /send-queue"): the composer's script re-reads GET /send-queue every 15 s for
// a window with nothing queued, and the empty list rendered a nil node — a
// panic on every such read. Through the real mux: an empty window's list is
// 200 with nothing in it; the control is the same window with a message
// queued, whose list draws it.
func TestTheQueueListOfAnEmptyWindowIsAnswered(t *testing.T) {
	q := &memQueue{}
	s, ts := queueServer(t, queueMirror(), q, &turnGateway{})
	path := ui.SendQueuePath + "?" + url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@1"}}.Encode()
	resp, page := mirrorGet(t, s, ts, path)
	if resp.StatusCode != http.StatusOK || strings.Contains(page, "data-queue-id") {
		t.Fatalf("an empty window's queue list: %d %q; want 200 and no rows", resp.StatusCode, page)
	}
	if _, err := q.Enqueue(asQueueUser(1, time.Now()), sendqueue.Item{Target: queueTarget, Body: "now one", StepUpAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if resp, page := mirrorGet(t, s, ts, path); resp.StatusCode != http.StatusOK || !strings.Contains(page, "now one") {
		t.Fatalf("CONTROL: a window with a queued message: %d %q", resp.StatusCode, page)
	}
}

// TestAFailedRowDrawsRetryOnce: a failed message's row draws Retry (a form
// posting to its retry route); one that may already have been sent says so
// and its Retry asks first (data-confirm); a retried one, a queued one and a
// sent one draw none.
func TestAFailedRowDrawsRetryOnce(t *testing.T) {
	page := func(it ui.SendQueueItem) string {
		var b strings.Builder
		if err := ui.SendQueueList(ui.SendQueueListView{Items: []ui.SendQueueItem{it}}).Render(&b); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	retry := `action="` + ui.SendQueueRetryPath(9) + `"`
	if p := page(ui.SendQueueItem{ID: 9, State: "failed", Retryable: true}); !strings.Contains(p, retry) ||
		strings.Contains(p, "data-confirm") || strings.Contains(p, ui.SendQueueMayHaveSentText) {
		t.Fatalf("a failed row: %s", p)
	}
	if p := page(ui.SendQueueItem{ID: 9, State: "failed", Retryable: true, MayHaveDelivered: true}); !strings.Contains(p, retry) ||
		!strings.Contains(p, "data-confirm") || !strings.Contains(p, ui.SendQueueMayHaveSentText) {
		t.Fatalf("a failed row that may have been sent: %s", p)
	}
	for _, it := range []ui.SendQueueItem{{ID: 9, State: "failed"}, {ID: 9, State: "queued", Cancellable: true}, {ID: 9, State: "sent"}} {
		if p := page(it); strings.Contains(p, "/retry") {
			t.Fatalf("%+v draws a Retry: %s", it, p)
		}
	}
	// The list view sets Retryable from the row: failed and not yet retried.
	for _, c := range []struct {
		it   sendqueue.Item
		want bool
	}{
		{sendqueue.Item{State: sendqueue.Failed}, true},
		{sendqueue.Item{State: sendqueue.Failed, RetriedAs: 4}, false},
		// A routed row is never retried as the person's own: its failed
		// route goes back to the agent.
		{sendqueue.Item{State: sendqueue.Failed, RouteID: "rt_0123456789abcdef01234567"}, false},
		{sendqueue.Item{State: sendqueue.Sent}, false},
		{sendqueue.Item{State: sendqueue.Cancelled}, false},
		{sendqueue.Item{State: sendqueue.Queued}, false},
	} {
		if got := sendQueueRetryable(c.it); got != c.want {
			t.Errorf("retryable(%+v) = %v, want %v", c.it, got, c.want)
		}
	}
}

// TestDeleteIsDrawnOnExactlyTheFinishedStates: through the real list route, a
// row in each state the schema allows (0019's send_queue_state_check) draws a
// Delete exactly when it is finished — sent, failed, cancelled — and never
// when it is queued (that is Cancel) or being sent.
func TestDeleteIsDrawnOnExactlyTheFinishedStates(t *testing.T) {
	q := &memQueue{}
	s, ts := queueServer(t, queueMirror(), q, &turnGateway{})
	states := []sendqueue.State{sendqueue.Queued, sendqueue.Sending, sendqueue.Sent, sendqueue.Failed, sendqueue.Cancelled}
	for _, st := range states {
		it, err := q.Enqueue(asQueueUser(1, time.Now()), sendqueue.Item{Target: queueTarget, Body: "row " + string(st), StepUpAt: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		q.mu.Lock()
		q.items[it.ID-1].State = st
		q.mu.Unlock()
	}
	_, page := mirrorGet(t, s, ts, ui.SendQueuePath+"?"+url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@1"}}.Encode())
	want := map[sendqueue.State]bool{sendqueue.Sent: true, sendqueue.Failed: true, sendqueue.Cancelled: true}
	for i, st := range states {
		drawn := strings.Contains(page, `action="`+ui.SendQueueDeletePath(int64(i+1))+`"`)
		if drawn != want[st] {
			t.Errorf("a %s row draws Delete: %v; want %v", st, drawn, want[st])
		}
	}
	if !strings.Contains(page, "row sending") {
		t.Fatalf("CONTROL: the list drew no rows: %s", page)
	}
}
