package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/sendqueue"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE SEND QUEUE'S DELETE (POST /send-queue/{id}/delete), through the real
// mux and a real Postgres (row-level security included): a FINISHED message —
// sent, failed (expired included), cancelled — is deleted and leaves the
// list; a queued one is refused (Cancel it), and so is one being sent (its
// claim still holds the row); another user's is 404 and untouched.
//
// RED AT origin/main (0405925): the route is not registered, so every Delete
// below answers 405 (the mux knows POST /send-queue/{id}/… for cancel and
// retry only) and every finished row is still there.

// exists is whether user 1's message id still exists, and its state.
func (f *sqFixture) exists(t *testing.T, id int64) (sendqueue.State, bool) {
	t.Helper()
	it, err := f.queue.Get(asQueueUser(1, time.Now()), id)
	if err != nil {
		return "", false
	}
	return it.State, true
}

// sent is user 1's message, sent by the dispatcher.
func (f *sqFixture) sent(t *testing.T, body string) sendqueue.Item {
	t.Helper()
	it := f.enqueue(t, body, time.Hour, 0)
	if _, ok, err := f.queue.Claim(context.Background(), it.ID, time.Now(), sendqueue.Claim{By: "test", Lease: time.Minute, Since: 1234}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := f.queue.Finish(context.Background(), it.ID, "test", sendqueue.Outcome{Sent: true, Detail: "Sent."}, time.Now()); err != nil {
		t.Fatal(err)
	}
	return it
}

func TestDeleteRemovesAFinishedMessageAndRefusesAnOpenOne(t *testing.T) {
	f := newSQFixture(t)
	f.mirror.agentState = &mirror.AgentState{Status: mirror.StatusBusy, Since: time.Now().UnixMilli()}
	cookie := sessionCookieFor(f.srv)

	failed := f.expired(t, "failed and stuck")
	cancelled := f.enqueue(t, "cancelled one", time.Hour, 0)
	if _, err := f.queue.Cancel(asQueueUser(1, time.Now()), cancelled.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	sent := f.sent(t, "sent one")
	queued := f.enqueue(t, "still queued", time.Hour, 0)
	sending := sendqueue.Item{}
	{
		// A second window's head, so its claim does not collide with the
		// queued row's target (one sending row per window).
		other := sendqueue.Target{Host: "ws", Session: "main", Window: "@9"}
		it, err := f.queue.Enqueue(asQueueUser(1, time.Now()), sendqueue.Item{Target: other, Body: "being sent", StepUpAt: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok, err := f.queue.Claim(context.Background(), it.ID, time.Now(), sendqueue.Claim{By: "a live pod", Lease: time.Hour}); err != nil || !ok {
			t.Fatal(ok, err)
		}
		sending = it
	}

	for _, c := range []struct {
		name string
		id   int64
	}{{"failed", failed.ID}, {"cancelled", cancelled.ID}, {"sent", sent.ID}} {
		resp, body := f.retry(t, ui.SendQueueDeletePath(c.id), cookie, true, nil)
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("Delete of a %s message: %d %q; want 204", c.name, resp.StatusCode, body)
		}
		if st, ok := f.exists(t, c.id); ok {
			t.Fatalf("the %s message is still there after its Delete (%s)", c.name, st)
		}
	}
	for _, c := range []struct {
		name string
		id   int64
		want sendqueue.State
	}{{"queued", queued.ID, sendqueue.Queued}, {"sending", sending.ID, sendqueue.Sending}} {
		resp, body := f.retry(t, ui.SendQueueDeletePath(c.id), cookie, true, nil)
		if resp.StatusCode != http.StatusConflict || !strings.Contains(body, SendQueueNotDeleted) {
			t.Fatalf("Delete of a %s message: %d %q; want 409 saying why", c.name, resp.StatusCode, body)
		}
		if st, ok := f.exists(t, c.id); !ok || st != c.want {
			t.Fatalf("a refused Delete moved the %s message: %s (exists %v)", c.name, st, ok)
		}
	}
	// The browser's own form (no script): a redirect back with a notice.
	again := f.expired(t, "deleted by a form")
	resp, body := f.retry(t, ui.SendQueueDeletePath(again.ID), cookie, false, nil)
	if resp.StatusCode != http.StatusSeeOther || !strings.Contains(resp.Header.Get("Location"), "notice="+noticeDeleted) {
		t.Fatalf("a Delete from a form: %d %q (Location %q)", resp.StatusCode, body, resp.Header.Get("Location"))
	}
	if _, ok := f.exists(t, again.ID); ok {
		t.Fatal("a form's Delete left the message")
	}
	// A fresh queued row: expired() runs an idle dispatcher pass, which sent
	// the earlier one.
	queued2 := f.enqueue(t, "queued after", time.Hour, 0)
	resp, _ = f.retry(t, ui.SendQueueDeletePath(queued2.ID), cookie, false, nil)
	if resp.StatusCode != http.StatusSeeOther || !strings.Contains(resp.Header.Get("Location"), "notice="+noticeNotDeleted) {
		t.Fatalf("a refused Delete from a form: %d (Location %q)", resp.StatusCode, resp.Header.Get("Location"))
	}
	if st, ok := f.exists(t, queued2.ID); !ok || st != sendqueue.Queued {
		t.Fatalf("a refused form Delete moved the queued message: %s (exists %v)", st, ok)
	}
}

// TestAnotherOwnersDeleteIsNotFound: user 2's Delete of user 1's FINISHED
// message is 404 — as for a message that does not exist — and the message is
// still there; the control is user 1's own Delete of it (204, gone).
func TestAnotherOwnersDeleteIsNotFound(t *testing.T) {
	f := newSQFixture(t)
	f.mirror.agentState = &mirror.AgentState{Status: mirror.StatusBusy, Since: time.Now().UnixMilli()}
	it := f.expired(t, "user one's failed message")
	call := func(uid int64) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, ui.SendQueueDeletePath(it.ID), nil)
		req.SetPathValue("id", strconv.FormatInt(it.ID, 10))
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		rec := httptest.NewRecorder()
		f.srv.handleSendQueueDelete(rec, req.WithContext(asQueueUser(uid, time.Now())))
		return rec
	}
	if rec := call(2); rec.Code != http.StatusNotFound {
		t.Fatalf("user 2's Delete of user 1's message: %d %q; want 404", rec.Code, rec.Body.String())
	}
	if st, ok := f.exists(t, it.ID); !ok || st != sendqueue.Failed {
		t.Fatalf("user 2's Delete moved user 1's message: %s (exists %v)", st, ok)
	}
	if rec := call(1); rec.Code != http.StatusNoContent {
		t.Fatalf("CONTROL: user 1's own Delete: %d %q", rec.Code, rec.Body.String())
	}
	if _, ok := f.exists(t, it.ID); ok {
		t.Fatal("CONTROL: user 1's own Delete left the message")
	}
}

// TestTheListDrawsDeleteOnFinishedRowsOnly: through GET /send-queue, the
// finished rows (failed, cancelled, sent) each draw a Delete posting to their
// own route, and the queued row draws none.
func TestTheListDrawsDeleteOnFinishedRowsOnly(t *testing.T) {
	f := newSQFixture(t)
	f.mirror.agentState = &mirror.AgentState{Status: mirror.StatusBusy, Since: time.Now().UnixMilli()}
	failed := f.expired(t, "a failed one")
	cancelled := f.enqueue(t, "a cancelled one", time.Hour, 0)
	if _, err := f.queue.Cancel(asQueueUser(1, time.Now()), cancelled.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	sent := f.sent(t, "a sent one")
	queued := f.enqueue(t, "a queued one", time.Hour, 0)
	_, page := mirrorGet(t, f.srv, f.ts, ui.SendQueuePath+"?host=ws&session=main&window=%401")
	for _, id := range []int64{failed.ID, cancelled.ID, sent.ID} {
		if !strings.Contains(page, `action="`+ui.SendQueueDeletePath(id)+`"`) {
			t.Fatalf("finished row %d draws no Delete: %s", id, page)
		}
	}
	if strings.Contains(page, ui.SendQueueDeletePath(queued.ID)) {
		t.Fatalf("the queued row draws a Delete: %s", page)
	}
	if got := strings.Count(page, `aria-label="`+ui.SendQueueDeleteLabel+`"`); got != 3 {
		t.Fatalf("%d Delete buttons; want 3 (one per finished row): %s", got, page)
	}
}

// TestDeletingASentMessageKeepsTheNextOneWaitingForItsTurn: the window's next
// message waits behind the turn a sent message started (heads' TurnAfter
// reads the sent row's after_since). Deleting the sent row must not release
// the next one into that turn: its turn is carried onto the open row.
func TestDeletingASentMessageKeepsTheNextOneWaitingForItsTurn(t *testing.T) {
	f := newSQFixture(t)
	f.mirror.agentState = &mirror.AgentState{Status: mirror.StatusBusy, Since: time.Now().UnixMilli()}
	sent := f.sent(t, "the one that started a turn") // claimed at status time 1234
	next := f.enqueue(t, "the next one", time.Hour, 0)
	head := func() int64 {
		h, ok, err := f.queue.HeadOf(asQueueUser(1, time.Now()), queueTarget)
		if err != nil || !ok || h.ID != next.ID {
			t.Fatalf("head: %+v %v %v", h, ok, err)
		}
		return h.TurnAfter
	}
	if got := head(); got != 1234 {
		t.Fatalf("fixture: the next message waits behind %d; want the sent one's 1234", got)
	}
	if resp, body := f.retry(t, ui.SendQueueDeletePath(sent.ID), sessionCookieFor(f.srv), true, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("Delete: %d %q", resp.StatusCode, body)
	}
	if got := head(); got != 1234 {
		t.Fatalf("after the sent message was deleted the next waits behind %d; want 1234 (its turn kept)", got)
	}
}

// TestDeletingAFailedRoutedMessageLeavesItsRouteToGoBackToTheChief: a routed
// injection that failed is deleted by its owner (the operator's case: a
// agent-routed row offers no Retry and used to sit in the list until it was
// pruned). Its route still names the row (send_queue_id, no foreign key);
// when the route times out, the sweep finds the row gone and STILL hands the
// message back to the agent.
func TestDeletingAFailedRoutedMessageLeavesItsRouteToGoBackToTheChief(t *testing.T) {
	f := newRouteFixture(t)
	m := f.deliveredMessage(t, f.owner, f.agent.ID, "route-deleted")
	ps, _ := f.plugins.List(f.asOwner)
	sess, err := f.srv.ext.Agents.CreateSession(f.asOwner, f.agent.ID, f.agent.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.plugins.SetDeliverySession(f.asOwner, ps[0].ID, f.agent.ID, sess.ID); err != nil {
		t.Fatal(err)
	}
	id := f.route(t, m)
	items := f.queued(t)
	if len(items) != 1 || items[0].RouteID != id {
		t.Fatalf("fixture: the injection: %+v", items)
	}
	row := items[0]
	if _, ok, err := f.queue.Claim(context.Background(), row.ID, time.Now(), sendqueue.Claim{By: "test", Lease: time.Minute}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := f.queue.Finish(context.Background(), row.ID, "test", sendqueue.Outcome{Detail: SendQueueRouteUnknown}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := f.queue.Delete(f.asOwner, row.ID); err != nil {
		t.Fatalf("the owner's Delete of the failed routed row: %v", err)
	}
	if items := f.queued(t); len(items) != 0 {
		t.Fatalf("the failed routed row is still listed: %+v", items)
	}
	if r, _ := f.plugins.Route(f.asOwner, id); r.State != plugins.RouteRouted || r.SendQueueID == nil || *r.SendQueueID != row.ID {
		t.Fatalf("the route after the delete: %+v", r)
	}
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE session_routes SET timeout_at = now() - interval '1 minute' WHERE route_id = '`+id+`'`)
	if n, err := f.srv.RouteTimeoutPass(context.Background()); err != nil || n < 1 {
		t.Fatalf("past the deadline: %d timed out (%v)", n, err)
	}
	if r, _ := f.plugins.Route(f.asOwner, id); r.State != plugins.RouteTimedOut {
		t.Fatalf("route state %s, want timed_out", r.State)
	}
	msgs, err := f.srv.ext.Agents.ListChatMessages(f.asOwner, sess.ID)
	if err != nil || len(msgs) != 1 || !strings.Contains(msgs[0].Content, "Route "+id) || !strings.Contains(msgs[0].Content, "back with you") {
		t.Fatalf("the agent was not handed the message back: %+v %v", msgs, err)
	}
}
