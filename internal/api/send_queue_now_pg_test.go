package api

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sendqueue"
	"github.com/ZacxDev/muster/internal/stepup"
	"github.com/ZacxDev/muster/internal/ui"
)

// SEND NOW (send_queue_now.go), through the real server against a real
// Postgres: a queued message is sent at once, busy or not, through the send
// path; it is claimed first, so it and the dispatcher never both send one
// message; the next queued message still waits for its turn; and it is gated
// like a Send (a fresh sign-in, an -interactive host) and like the
// dispatcher's delivery (the row's own authorization, a routed row's route).

// sendNowPath is message id's Send now, spelled literally: these tests are
// read against a tree without the route (where they must fail), so they do not
// lean on its helper.
func sendNowPath(id int64) string { return "/send-queue/" + strconv.FormatInt(id, 10) + "/send-now" }

// TestSendNowSendsAQueuedMessageAtOnceAndTheNextStillWaitsForItsTurn: with the
// window BUSY, the second of two queued messages is sent now — pasted once,
// with no turn expected (a Send's paste) — and ends sent. The first, still
// queued, then waits for the turn the Send now started: idle since the time
// the window held at that paste does not release it; a newer one does.
func TestSendNowSendsAQueuedMessageAtOnceAndTheNextStillWaitsForItsTurn(t *testing.T) {
	f := newSQFixture(t)
	held := time.Now().Add(-time.Minute).UnixMilli()
	f.mirror.agentState = &mirror.AgentState{Status: mirror.StatusBusy, Since: held}
	first := f.enqueue(t, "the first, queued", time.Minute, 0)
	second := f.enqueue(t, "the second, sent now", time.Minute, 0)
	cookie := sessionCookieFor(f.srv)
	if resp, body := f.retry(t, sendNowPath(second.ID), cookie, true, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("Send now: %d %q", resp.StatusCode, body)
	}
	if got := f.pasted(); len(got) != 1 || got[0] != second.Body {
		t.Fatalf("pasted %q; want the second message, once", got)
	}
	if _, ps := f.mirror.v2.snapshot(); ps[0].expect != nil || ps[0].target != mirror.PasteClaude {
		t.Fatalf("the Send now paste = %+v; want a Send's paste (no turn expected) into the Claude Code pane", ps[0])
	}
	if got := f.row(t, second.ID); got.State != sendqueue.Sent {
		t.Fatalf("the sent-now row: %s %q; want sent", got.State, got.Detail)
	}
	if got := f.row(t, first.ID); got.State != sendqueue.Queued {
		t.Fatalf("the other row: %s; want still queued", got.State)
	}
	// Idle, but since the time the window held at the Send now: its turn has
	// not ended, so the first waits.
	f.mirror.agentState = &mirror.AgentState{Status: mirror.StatusIdle, Since: held, Observed: time.Now().Add(-time.Hour)}
	f.tick(t)
	if got := f.row(t, first.ID); got.State != sendqueue.Queued || len(f.pasted()) != 1 {
		t.Fatalf("after the Send now, idle since the same time: first %s, pasted %q; want it held for the turn", got.State, f.pasted())
	}
	// CONTROL: the turn ends (a newer status time): the first is sent.
	f.mirror.agentState = &mirror.AgentState{Status: mirror.StatusIdle, Since: held + 1, Observed: time.Now().Add(-time.Hour)}
	f.tick(t)
	if got := f.row(t, first.ID); got.State != sendqueue.Sent || len(f.pasted()) != 2 || f.pasted()[1] != first.Body {
		t.Fatalf("after the turn: first %s, pasted %q; want it sent", got.State, f.pasted())
	}
}

// TestSendNowAndTheDispatcherNeverBothSendOneMessage: the double-send race,
// both ways round, with the second sender run WHILE the first one's paste is
// in flight. A dispatcher pass during a Send now's paste finds the row no
// longer queued and pastes nothing; a Send now during the dispatcher's paste
// of the same row is 409 and pastes nothing. Each message is pasted exactly
// once. And a Send now of ANOTHER row while one is being sent is refused,
// saying so: one at a time per window.
func TestSendNowAndTheDispatcherNeverBothSendOneMessage(t *testing.T) {
	f := newSQFixture(t)
	idle := &mirror.AgentState{Status: mirror.StatusIdle, Since: time.Now().Add(-time.Hour).UnixMilli(), Observed: time.Now().Add(-time.Hour)}
	f.mirror.agentState = idle
	cookie := sessionCookieFor(f.srv)

	// 1. Send now first; the dispatcher runs during its paste.
	a := f.enqueue(t, "sent now, raced by the dispatcher", time.Minute, 0)
	var once sync.Once
	f.mirror.mu.Lock()
	f.mirror.inPaste = func() { once.Do(func() { f.tick(t) }) }
	f.mirror.mu.Unlock()
	if resp, body := f.retry(t, sendNowPath(a.ID), cookie, true, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("Send now: %d %q", resp.StatusCode, body)
	}
	if got := f.pasted(); len(got) != 1 || got[0] != a.Body {
		t.Fatalf("pasted %q; want the message exactly once", got)
	}
	if got := f.row(t, a.ID); got.State != sendqueue.Sent {
		t.Fatalf("row a: %s %q; want sent", got.State, got.Detail)
	}

	// 2. The dispatcher first; a Send now of the same row — and of another —
	// runs during its paste. (A newer idle: a's turn has ended.)
	b := f.enqueue(t, "sent by the dispatcher, raced by a Send now", time.Minute, 0)
	c := f.enqueue(t, "another, sent now while b is being sent", time.Minute, 0)
	f.mirror.mu.Lock()
	f.mirror.agentState = &mirror.AgentState{Status: mirror.StatusIdle, Since: time.Now().UnixMilli(), Observed: time.Now().Add(-time.Hour)}
	var same, other struct {
		code int
		body string
	}
	once2 := sync.Once{}
	f.mirror.inPaste = func() {
		once2.Do(func() {
			resp, body := f.retry(t, sendNowPath(b.ID), cookie, true, nil)
			same.code, same.body = resp.StatusCode, body
			resp, body = f.retry(t, sendNowPath(c.ID), cookie, true, nil)
			other.code, other.body = resp.StatusCode, body
		})
	}
	f.mirror.mu.Unlock()
	f.tick(t)
	if same.code != http.StatusConflict || !strings.Contains(same.body, "no longer queued") {
		t.Fatalf("a Send now of the row the dispatcher is sending: %d %q; want 409, no longer queued", same.code, same.body)
	}
	if other.code != http.StatusConflict || !strings.Contains(other.body, "another message for this window is being sent") {
		t.Fatalf("a Send now of another row while one is sent: %d %q; want 409, another is being sent", other.code, other.body)
	}
	if got := f.pasted(); len(got) != 2 || got[1] != b.Body {
		t.Fatalf("pasted %q; want b exactly once after a", got)
	}
	if got := f.row(t, b.ID); got.State != sendqueue.Sent {
		t.Fatalf("row b: %s; want sent", got.State)
	}
	if got := f.row(t, c.ID); got.State != sendqueue.Queued {
		t.Fatalf("row c, refused: %s; want still queued", got.State)
	}
}

// TestSendNowUnderAStaleSignInAsksToStepUp: a Send now from a session whose
// sign-in is stale sends nothing — 403 + X-Muster-Step-Up to the script, the
// plain confirm page (posting to the Send now's confirm route) to a browser
// form — and the confirm page's password completes it, once.
func TestSendNowUnderAStaleSignInAsksToStepUp(t *testing.T) {
	f := newSQFixture(t)
	f.mirror.agentState = &mirror.AgentState{Status: mirror.StatusBusy, Since: time.Now().UnixMilli()}
	it := f.enqueue(t, "sent after a confirm", time.Minute, 0)
	stale := staleCookieFor(f.srv)
	resp, body := f.retry(t, sendNowPath(it.ID), stale, true, nil)
	if resp.StatusCode != http.StatusForbidden || resp.Header.Get("X-Muster-Step-Up") != "required" {
		t.Fatalf("a stale Send now from the script: %d %q", resp.StatusCode, body)
	}
	resp, body = f.retry(t, sendNowPath(it.ID), stale, false, nil)
	confirm := sendNowPath(it.ID) + "/confirm"
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `id="stepup-form"`) || !strings.Contains(body, `action="`+confirm+`"`) {
		t.Fatalf("a stale Send now from a browser form: %d %q; want the confirm page posting to %s", resp.StatusCode, body, confirm)
	}
	if got := f.pasted(); len(got) != 0 || f.row(t, it.ID).State != sendqueue.Queued {
		t.Fatalf("a stale Send now pasted %q / moved the row to %s", got, f.row(t, it.ID).State)
	}
	if resp, body := f.retry(t, confirm, stale, false, url.Values{"password": {"not it"}}); resp.StatusCode == http.StatusSeeOther || len(f.pasted()) != 0 {
		t.Fatalf("a wrong password: %d %q, pasted %q", resp.StatusCode, body, f.pasted())
	}
	resp, body = f.retry(t, confirm, stale, false, url.Values{"password": {connTestPassword}})
	if resp.StatusCode != http.StatusSeeOther || !strings.Contains(resp.Header.Get("Location"), "notice="+noticeSent) {
		t.Fatalf("the right password: %d %q (Location %q)", resp.StatusCode, body, resp.Header.Get("Location"))
	}
	if got := f.pasted(); len(got) != 1 || got[0] != it.Body || f.row(t, it.ID).State != sendqueue.Sent {
		t.Fatalf("after the confirm: pasted %q, row %s; want sent once", got, f.row(t, it.ID).State)
	}
}

// TestSendNowIsRefusedWhereASendIsAndWhereTheDispatchersDeliveryIs: each
// refusal pastes nothing. Before the claim (the row stays queued): a read-only
// host, an offline one, a message no longer queued (another user's message is
// TestAnotherOwnersSendNowIsNotFound). After it, the dispatcher's own delivery checks fail the row saying
// why: a sign-in that was not fresh when the message was queued, and a routed
// row whose route cannot be read. The control is a plain Send now, sent.
func TestSendNowIsRefusedWhereASendIsAndWhereTheDispatchersDeliveryIs(t *testing.T) {
	f := newSQFixture(t)
	f.mirror.agentState = &mirror.AgentState{Status: mirror.StatusBusy, Since: time.Now().UnixMilli()}
	cookie := sessionCookieFor(f.srv)
	post := func(id int64) (int, string) {
		resp, body := f.retry(t, sendNowPath(id), cookie, true, nil)
		return resp.StatusCode, body
	}
	it := f.enqueue(t, "refused, then sent", time.Minute, 0)
	set := func(online, interactive bool) {
		f.mirror.mu.Lock()
		f.mirror.online, f.mirror.interactive = online, interactive
		f.mirror.mu.Unlock()
	}
	set(true, false)
	if code, body := post(it.ID); code != http.StatusConflict || !strings.Contains(body, "read-only") {
		t.Fatalf("a read-only host: %d %q", code, body)
	}
	set(false, false)
	if code, body := post(it.ID); code != http.StatusConflict && code != http.StatusNotFound {
		t.Fatalf("an offline host: %d %q", code, body)
	}
	set(true, true)
	if got := f.row(t, it.ID); got.State != sendqueue.Queued || len(f.pasted()) != 0 {
		t.Fatalf("after the host refusals: %s, pasted %q", got.State, f.pasted())
	}
	// CONTROL: the owner's own, sent.
	if code, body := post(it.ID); code != http.StatusNoContent {
		t.Fatalf("CONTROL: %d %q", code, body)
	}
	// No longer queued: refused, nothing more pasted.
	if code, body := post(it.ID); code != http.StatusConflict || !strings.Contains(body, "no longer queued") || len(f.pasted()) != 1 {
		t.Fatalf("a Send now of a sent message: %d %q, pasted %q", code, body, f.pasted())
	}
	// Queued under a sign-in that was already stale then: the dispatcher's
	// delivery check fails it, pasting nothing.
	stale := f.enqueue(t, "queued under a stale sign-in", stepup.Window()+time.Hour, 0)
	if code, body := post(stale.ID); code != http.StatusBadGateway || body != SendQueueNotFreshWhenQueued+"\n" {
		t.Fatalf("a row not authorized when queued: %d %q", code, body)
	}
	if got := f.row(t, stale.ID); got.State != sendqueue.Failed || got.Detail != SendQueueNotFreshWhenQueued || len(f.pasted()) != 1 {
		t.Fatalf("the unauthorized row: %s %q, pasted %q; want failed saying why, nothing pasted", got.State, got.Detail, f.pasted())
	}
	// A routed row keeps its route gate.
	routed, err := f.queue.Enqueue(asQueueUser(1, time.Now()), sendqueue.Item{Target: queueTarget, Body: "a routed answer",
		StepUpAt: time.Now(), RouteID: "rt_0123456789abcdef01234567"})
	if err != nil {
		t.Fatal(err)
	}
	if code, body := post(routed.ID); code != http.StatusBadGateway || body != SendQueueRouteUnknown+"\n" {
		t.Fatalf("a routed row with no readable route: %d %q", code, body)
	}
	if got := f.row(t, routed.ID); got.State != sendqueue.Failed || len(f.pasted()) != 1 {
		t.Fatalf("the routed row: %s %q, pasted %q; want failed by its route, nothing pasted", got.State, got.Detail, f.pasted())
	}
}

// TestTheListDrawsSendNowAndCopyOnQueuedRowsOnly: a queued row's ⋯ menu holds
// Send now (a real form posting to its route), Copy message (carrying the
// whole message, not the preview) and Cancel; a finished row has no menu and
// keeps its own buttons.
func TestTheListDrawsSendNowAndCopyOnQueuedRowsOnly(t *testing.T) {
	f := newSQFixture(t)
	f.mirror.agentState = &mirror.AgentState{Status: mirror.StatusBusy, Since: time.Now().UnixMilli()}
	long := strings.Repeat("a long queued message ", 20) + "THE END"
	q := f.enqueue(t, long, time.Minute, 0)
	sent := f.sent(t, "already sent")
	_, page := mirrorGet(t, f.srv, f.ts, ui.SendQueuePath+"?"+url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@1"}}.Encode())
	rowOf := func(id int64) string {
		i := strings.Index(page, `data-queue-id="`+strconv.FormatInt(id, 10)+`"`)
		if i < 0 {
			t.Fatalf("no row %d in %s", id, page)
		}
		j := strings.Index(page[i:], "</li>")
		return page[i : i+j]
	}
	qr, sr := rowOf(q.ID), rowOf(sent.ID)
	for _, want := range []string{`aria-haspopup="menu"`, `action="` + sendNowPath(q.ID) + `"`, `action="` + ui.SendQueueCancelPath(q.ID) + `"`,
		`data-queue-copy`, "THE END"} {
		if !strings.Contains(qr, want) {
			t.Errorf("the queued row lacks %q: %s", want, qr)
		}
	}
	for _, not := range []string{`aria-haspopup="menu"`, "send-now", "data-queue-copy"} {
		if strings.Contains(sr, not) {
			t.Errorf("the sent row has %q: %s", not, sr)
		}
	}
	if !strings.Contains(sr, `action="`+ui.SendQueueDeletePath(sent.ID)+`"`) {
		t.Errorf("CONTROL: the sent row lost its Delete: %s", sr)
	}
}
