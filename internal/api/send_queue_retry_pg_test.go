package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/auth"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sendqueue"
	"github.com/ZacxDev/muster/internal/stepup"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE SEND QUEUE'S AUTHORIZATION AND RETRY, through the real server against a
// real Postgres (internal/sendqueue's PGStore, its row-level security
// included): a message authorized when it was queued is sent for up to
// sendqueue.AuthorizedFor after, whatever the step-up window; past that it
// fails as expired; a failed message's Retry queues it again under the
// retrying request's own fresh sign-in.

type sqFixture struct {
	*connFixture
	mirror *fakeMirror
	queue  *sendqueue.PGStore
	ts     *httptest.Server
}

func newSQFixture(t *testing.T) *sqFixture {
	t.Helper()
	f := &sqFixture{mirror: queueMirror()}
	f.connFixture = newConnFixtureWith(t, func(ext *Extensions, pool *pgxpool.Pool) {
		f.queue = sendqueue.NewPG(pool)
		ext.Notes, ext.SessionLiveness, ext.Provisioner, ext.Gateway = stubNotesStore{}, stubLiveness{}, newFakeTerminal(), &turnGateway{}
		ext.Mirror, ext.SendQueue = f.mirror, f.queue
	})
	dbtest.OwnerExec(t, dbtest.DSN(t), `TRUNCATE send_queue`)
	// Idle since an hour ago, observed then: a head is sent at once.
	f.mirror.agentState = &mirror.AgentState{Status: mirror.StatusIdle, Since: time.Now().Add(-time.Hour).UnixMilli(),
		Observed: time.Now().Add(-time.Hour)}
	f.ts = httptest.NewServer(f.h)
	t.Cleanup(f.ts.Close)
	return f
}

// enqueue is user 1's message for queueTarget, queued `ago` in the past under
// a sign-in `signedIn` before it was queued: the row is written now and both
// its times are then moved back by ago (Postgres's own created_at).
func (f *sqFixture) enqueue(t *testing.T, body string, signedIn, ago time.Duration) sendqueue.Item {
	t.Helper()
	it, err := f.queue.Enqueue(asQueueUser(1, time.Now()), sendqueue.Item{Target: queueTarget, Body: body, StepUpAt: time.Now().Add(-signedIn)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE send_queue SET created_at = created_at - $2 * interval '1 second',
		stepup_at = stepup_at - $2 * interval '1 second' WHERE id = $1`, it.ID, int64(ago/time.Second)); err != nil {
		t.Fatal(err)
	}
	return it
}

// tick is one dispatcher pass, as production runs it.
func (f *sqFixture) tick(t *testing.T) {
	t.Helper()
	if _, err := f.srv.newSendQueueDispatcher().Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (f *sqFixture) row(t *testing.T, id int64) sendqueue.Item {
	t.Helper()
	it, err := f.queue.Get(asQueueUser(1, time.Now()), id)
	if err != nil {
		t.Fatal(err)
	}
	return it
}

func (f *sqFixture) pasted() []string {
	_, ps := f.mirror.v2.snapshot()
	var out []string
	for _, p := range ps {
		out = append(out, p.text)
	}
	return out
}

func (f *sqFixture) open(t *testing.T) []sendqueue.Item {
	t.Helper()
	items, err := f.queue.List(asQueueUser(1, time.Now()), queueTarget)
	if err != nil {
		t.Fatal(err)
	}
	var out []sendqueue.Item
	for _, it := range items {
		if it.State.Open() {
			out = append(out, it)
		}
	}
	return out
}

// retry posts message id's Retry with cookie: as the list's script does (xhr)
// or as a browser form (a navigation), with form.
func (f *sqFixture) retry(t *testing.T, path, cookie string, xhr bool, form url.Values) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, f.ts.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Origin", f.ts.URL)
	if xhr {
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
	} else {
		req.Header.Set("Sec-Fetch-Dest", "document")
		req.Header.Set("Sec-Fetch-Mode", "navigate")
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// TestAQueuedMessageStaysAuthorizedForSevenDays: queued under a FRESH sign-in
// (an hour old), the message is sent by the dispatcher 13 hours later — past
// the 12-hour step-up window, which used to fail it — and 6 days 23 hours
// later; 7 days and a minute later it fails as expired, pasting nothing, and
// its row offers Retry.
func TestAQueuedMessageStaysAuthorizedForSevenDays(t *testing.T) {
	for _, c := range []struct {
		name string
		ago  time.Duration
		sent bool
	}{
		// Literal durations, never derived from sendqueue.AuthorizedFor: a
		// bound moved in the code must move a case from sent to failed here.
		{"13 hours", 13 * time.Hour, true},
		{"6 days 23 hours", (6*24 + 23) * time.Hour, true},
		{"7 days and a minute", 7*24*time.Hour + time.Minute, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newSQFixture(t)
			it := f.enqueue(t, "queued "+c.name+" ago", time.Hour, c.ago)
			f.tick(t)
			got := f.row(t, it.ID)
			if c.sent {
				if got.State != sendqueue.Sent || len(f.pasted()) != 1 || f.pasted()[0] != it.Body {
					t.Fatalf("queued %s ago under a fresh sign-in: %s %q, pasted %q; want sent", c.name, got.State, got.Detail, f.pasted())
				}
				return
			}
			if got.State != sendqueue.Failed || got.Detail != SendQueueExpired || got.MayHaveDelivered || len(f.pasted()) != 0 {
				t.Fatalf("queued %s ago: %s %q (may have: %v), pasted %q; want failed as expired, nothing pasted",
					c.name, got.State, got.Detail, got.MayHaveDelivered, f.pasted())
			}
			_, page := mirrorGet(t, f.srv, f.ts, ui.SendQueuePath+"?"+url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@1"}}.Encode())
			if !strings.Contains(page, `action="`+ui.SendQueueRetryPath(it.ID)+`"`) {
				t.Fatalf("the expired row's list draws no Retry: %s", page)
			}
		})
	}
}

// TestTheSignInMustHaveBeenFreshWhenTheMessageWasQueued: the queue route still
// refuses a stale sign-in (403 + X-Muster-Step-Up), storing nothing; and a row
// whose recorded sign-in was already past the window when it was created
// (written around the route) is refused at delivery, pasting nothing. The
// control is a row queued under a sign-in inside the window, which is sent.
func TestTheSignInMustHaveBeenFreshWhenTheMessageWasQueued(t *testing.T) {
	f := newSQFixture(t)
	resp, body := postQueue(t, f.ts, staleCookieFor(f.srv), mirrorQueueForm("main", "@1", "stale queue"))
	if resp.StatusCode != http.StatusForbidden || resp.Header.Get("X-Muster-Step-Up") != "required" {
		t.Fatalf("a queue post under a stale sign-in: %d %q", resp.StatusCode, body)
	}
	if got := f.open(t); len(got) != 0 {
		t.Fatalf("a stale queue post stored %+v", got)
	}
	stale := f.enqueue(t, "stale when queued", stepup.Window()+time.Minute, 0)
	f.tick(t)
	if got := f.row(t, stale.ID); got.State != sendqueue.Failed || got.Detail != SendQueueNotFreshWhenQueued || len(f.pasted()) != 0 {
		t.Fatalf("a row queued under a stale sign-in: %s %q, pasted %q; want refused", got.State, got.Detail, f.pasted())
	}
	fresh := f.enqueue(t, "fresh when queued", stepup.Window()-time.Minute, 0)
	f.tick(t)
	if got := f.row(t, fresh.ID); got.State != sendqueue.Sent {
		t.Fatalf("CONTROL: a row queued inside the window: %s %q", got.State, got.Detail)
	}
}

// expired is a message that failed as expired, as the dispatcher fails one.
func (f *sqFixture) expired(t *testing.T, body string) sendqueue.Item {
	t.Helper()
	// Idle for this tick only: the delivery fails it, then the window is busy
	// again, so what is queued after is held.
	it := f.enqueue(t, body, time.Hour, sendqueue.AuthorizedFor+time.Hour)
	f.mirror.agentState = &mirror.AgentState{Status: mirror.StatusIdle, Since: time.Now().Add(-time.Hour).UnixMilli(),
		Observed: time.Now().Add(-time.Hour)}
	f.tick(t)
	f.mirror.agentState = &mirror.AgentState{Status: mirror.StatusBusy, Since: time.Now().UnixMilli()}
	if got := f.row(t, it.ID); got.State != sendqueue.Failed || got.Detail != SendQueueExpired {
		t.Fatalf("fixture: %s %q; want failed as expired", got.State, got.Detail)
	}
	return it
}

// TestRetryQueuesAFailedMessageUnderTheCurrentSignIn: the Retry of an expired
// message queues the same text for the same window under the REQUEST's sign-in
// (the cookie's time, not the old row's), and the dispatcher sends it. A
// second Retry is 409 and queues nothing more.
func TestRetryQueuesAFailedMessageUnderTheCurrentSignIn(t *testing.T) {
	f := newSQFixture(t)
	f.mirror.agentState = &mirror.AgentState{Status: mirror.StatusBusy, Since: time.Now().UnixMilli()}
	old := f.expired(t, "send me after all")
	signedIn := time.Now().Add(-time.Hour).Truncate(time.Second)
	cookie := sessionCookieName + "=" + auth.SignSession(f.srv.auth.uiSessionSecret(), signedIn, sessionTTL)
	if resp, body := f.retry(t, ui.SendQueueRetryPath(old.ID), cookie, true, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("Retry: %d %q", resp.StatusCode, body)
	}
	open := f.open(t)
	if len(open) != 1 || open[0].Body != old.Body || open[0].Target != queueTarget || open[0].ID == old.ID || !open[0].StepUpAt.Equal(signedIn) {
		t.Fatalf("after Retry the open rows are %+v; want one new row of %q signed in at %v", open, old.Body, signedIn)
	}
	if got := f.row(t, old.ID); got.State != sendqueue.Failed || got.RetriedAs != open[0].ID {
		t.Fatalf("the failed row after Retry: %s retried as %d", got.State, got.RetriedAs)
	}
	if resp, body := f.retry(t, ui.SendQueueRetryPath(old.ID), cookie, true, nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("a second Retry: %d %q; want 409", resp.StatusCode, body)
	}
	if got := f.open(t); len(got) != 1 {
		t.Fatalf("a second Retry queued another copy: %+v", got)
	}
	f.mirror.agentState = &mirror.AgentState{Status: mirror.StatusIdle, Since: time.Now().UnixMilli(), Observed: time.Now().Add(-time.Hour)}
	f.tick(t)
	if got := f.row(t, open[0].ID); got.State != sendqueue.Sent || len(f.pasted()) != 1 || f.pasted()[0] != old.Body {
		t.Fatalf("the retried message: %s %q, pasted %q; want sent", got.State, got.Detail, f.pasted())
	}
}

// mayHaveSent is a message muster stopped while sending: claimed, its lease
// run out, and recovered as failed, may have been delivered.
func (f *sqFixture) mayHaveSent(t *testing.T, body string) sendqueue.Item {
	t.Helper()
	it := f.enqueue(t, body, time.Hour, 0)
	if _, ok, err := f.queue.Claim(context.Background(), it.ID, time.Now(), sendqueue.Claim{By: "a stopped pod", Lease: time.Millisecond}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := f.queue.Recover(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := f.row(t, it.ID); got.State != sendqueue.Failed || !got.MayHaveDelivered {
		t.Fatalf("fixture: %s may have: %v", got.State, got.MayHaveDelivered)
	}
	return it
}

// TestRetryOfAMessageThatMayHaveBeenSentNeedsAConfirm: Retry of a message
// that may already have been sent is refused until it says the person
// confirmed — 409 saying why to the script, a confirm page to a browser form —
// queueing nothing; with the confirm it is queued.
func TestRetryOfAMessageThatMayHaveBeenSentNeedsAConfirm(t *testing.T) {
	f := newSQFixture(t)
	f.mirror.agentState = &mirror.AgentState{Status: mirror.StatusBusy, Since: time.Now().UnixMilli()}
	it := f.mayHaveSent(t, "did it land")
	// The control's row, failed before anything is queued again (one at a
	// time: a queued retry would be the window's head).
	plain := f.expired(t, "plain failure")
	cookie := sessionCookieFor(f.srv)
	resp, body := f.retry(t, ui.SendQueueRetryPath(it.ID), cookie, true, nil)
	if resp.StatusCode != http.StatusConflict || resp.Header.Get("X-Muster-Confirm") == "" || !strings.Contains(body, "may already have been sent") {
		t.Fatalf("an unconfirmed Retry from the script: %d %q", resp.StatusCode, body)
	}
	resp, body = f.retry(t, ui.SendQueueRetryPath(it.ID), cookie, false, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `id="sq-retry-confirm"`) ||
		!strings.Contains(body, `name="`+SendQueueRetryConfirmField+`" value="1"`) {
		t.Fatalf("an unconfirmed Retry from a browser form: %d %q; want the confirm page", resp.StatusCode, body)
	}
	if got := f.open(t); len(got) != 0 {
		t.Fatalf("an unconfirmed Retry queued %+v", got)
	}
	if resp, body := f.retry(t, ui.SendQueueRetryPath(it.ID), cookie, true, url.Values{SendQueueRetryConfirmField: {"1"}}); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("a confirmed Retry: %d %q", resp.StatusCode, body)
	}
	if got := f.open(t); len(got) != 1 || got[0].Body != it.Body {
		t.Fatalf("a confirmed Retry queued %+v", got)
	}
	// CONTROL: a failure that did not deliver needs no confirm.
	if resp, body := f.retry(t, ui.SendQueueRetryPath(plain.ID), cookie, true, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("CONTROL: a Retry of a plain failure: %d %q", resp.StatusCode, body)
	}
}

// TestRetryUnderAStaleSignInAsksToStepUp: a Retry from a session whose sign-in
// is stale queues nothing — 403 + X-Muster-Step-Up to the script, the confirm
// page (posting to the Retry's confirm route) to a browser form — and the
// confirm page's password completes it under the fresh sign-in.
func TestRetryUnderAStaleSignInAsksToStepUp(t *testing.T) {
	f := newSQFixture(t)
	f.mirror.agentState = &mirror.AgentState{Status: mirror.StatusBusy, Since: time.Now().UnixMilli()}
	it := f.expired(t, "after a confirm")
	stale := staleCookieFor(f.srv)
	resp, body := f.retry(t, ui.SendQueueRetryPath(it.ID), stale, true, nil)
	if resp.StatusCode != http.StatusForbidden || resp.Header.Get("X-Muster-Step-Up") != "required" {
		t.Fatalf("a stale Retry from the script: %d %q", resp.StatusCode, body)
	}
	resp, body = f.retry(t, ui.SendQueueRetryPath(it.ID), stale, false, nil)
	confirm := ui.SendQueueRetryPath(it.ID) + "/confirm"
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `id="stepup-form"`) || !strings.Contains(body, `action="`+confirm+`"`) {
		t.Fatalf("a stale Retry from a browser form: %d %q; want the confirm page posting to %s", resp.StatusCode, body, confirm)
	}
	if got := f.open(t); len(got) != 0 {
		t.Fatalf("a stale Retry queued %+v", got)
	}
	// A wrong password shows the page again; the right one retries.
	if resp, body := f.retry(t, confirm, stale, false, url.Values{"password": {"not it"}}); resp.StatusCode == http.StatusSeeOther || len(f.open(t)) != 0 {
		t.Fatalf("a wrong password: %d %q, open %d", resp.StatusCode, body, len(f.open(t)))
	}
	resp, body = f.retry(t, confirm, stale, false, url.Values{"password": {connTestPassword}})
	if resp.StatusCode != http.StatusSeeOther || !strings.Contains(resp.Header.Get("Location"), "notice="+noticeQueued) {
		t.Fatalf("the right password: %d %q (Location %q)", resp.StatusCode, body, resp.Header.Get("Location"))
	}
	got := f.open(t)
	if len(got) != 1 || got[0].Body != it.Body || time.Since(got[0].StepUpAt) > time.Minute {
		t.Fatalf("after the confirm the open rows are %+v; want the retry, signed in just now", got)
	}
}

// TestAnotherOwnersRetryIsNotFound: user 2's Retry of user 1's failed message
// is 404 — not "exists, not yours" — and leaves it unretried; user 1's own is
// the control.
func TestAnotherOwnersRetryIsNotFound(t *testing.T) {
	f := newSQFixture(t)
	f.mirror.agentState = &mirror.AgentState{Status: mirror.StatusBusy, Since: time.Now().UnixMilli()}
	it := f.expired(t, "user one's")
	call := func(uid int64) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, ui.SendQueueRetryPath(it.ID), nil)
		req.SetPathValue("id", strconv.FormatInt(it.ID, 10))
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		rec := httptest.NewRecorder()
		f.srv.handleSendQueueRetry(rec, req.WithContext(asQueueUser(uid, time.Now())))
		return rec
	}
	if rec := call(2); rec.Code != http.StatusNotFound {
		t.Fatalf("user 2's Retry of user 1's message: %d %q", rec.Code, rec.Body.String())
	}
	if got := f.row(t, it.ID); got.RetriedAs != 0 || len(f.open(t)) != 0 {
		t.Fatalf("user 2's Retry moved user 1's message: retried as %d, open %d", got.RetriedAs, len(f.open(t)))
	}
	if rec := call(1); rec.Code != http.StatusNoContent {
		t.Fatalf("CONTROL: user 1's own Retry: %d %q", rec.Code, rec.Body.String())
	}
}
