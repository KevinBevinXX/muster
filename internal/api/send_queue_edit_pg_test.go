package api

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sendqueue"
	"github.com/ZacxDev/muster/internal/stepup"
	"github.com/ZacxDev/muster/internal/ui"
)

// EDITING A QUEUED MESSAGE through the real server against a real Postgres:
// Edit holds the row (and draws it open), Save replaces the text in place and
// the dispatcher sends the NEW text, in the original order; Cancel releases it
// unchanged; both Edit and Save need a fresh sign-in (step-up), and the
// delivery checks the Save's own sign-in again from the row.

// queueListPath is user 1's queue list for queueTarget, with row edit open.
func queueListPath(edit int64) string {
	v := url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@1"}}
	if edit > 0 {
		v.Set(ui.SendQueueEditParam, strconv.FormatInt(edit, 10))
	}
	return ui.SendQueuePath + "?" + v.Encode()
}

// TestAnEditHoldsTheRowSavesInPlaceAndTheNewTextIsSent is the whole cycle: two
// messages queued and the window idle long enough to send; Edit holds the
// first, and the dispatcher sends NEITHER (the second waits behind it); the
// list draws the first open for editing only when asked; Save replaces its
// text, and the dispatcher sends the EDITED text first, then the second.
func TestAnEditHoldsTheRowSavesInPlaceAndTheNewTextIsSent(t *testing.T) {
	f := newSQFixture(t)
	first := f.enqueue(t, "first, as queued", 0, 0)
	second := f.enqueue(t, "second", 0, 0)
	cookie := sessionCookieFor(f.srv)

	resp, body := f.retry(t, ui.SendQueueEditPath(first.ID), cookie, true, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Edit: %d %q", resp.StatusCode, body)
	}
	held := f.row(t, first.ID)
	if !held.Held || held.HoldToken == "" || held.Body != "first, as queued" {
		t.Fatalf("after Edit the row is held %t, token %q, body %q", held.Held, held.HoldToken, held.Body)
	}
	if body != held.HoldToken {
		t.Fatalf("Edit answered %q to the script; want the hold it took, %q", body, held.HoldToken)
	}
	f.tick(t)
	f.tick(t)
	if p := f.pasted(); len(p) != 0 {
		t.Fatalf("pasted %q while the head was held", p)
	}

	// The list draws the row open only when asked, with the hold's token.
	_, list := mirrorGet(t, f.srv, f.ts, queueListPath(0))
	if strings.Contains(list, `data-queue-edit-form`) || !strings.Contains(list, ui.SendQueueHeldText) {
		t.Fatalf("the plain list draws an edit form, or does not say the row is held: %s", list)
	}
	_, list = mirrorGet(t, f.srv, f.ts, queueListPath(first.ID)+"&hold="+url.QueryEscape(held.HoldToken))
	if !strings.Contains(list, `action="`+ui.SendQueueEditSavePath(first.ID)+`"`) || !strings.Contains(list, `value="`+held.HoldToken+`"`) ||
		!strings.Contains(list, ">first, as queued</textarea>") {
		t.Fatalf("?edit= does not draw the held row open with its hold and its text: %s", list)
	}
	_, list = mirrorGet(t, f.srv, f.ts, queueListPath(second.ID)+"&hold="+url.QueryEscape(held.HoldToken))
	if strings.Contains(list, `data-queue-edit-form`) {
		t.Fatalf("?edit= drew a row that is NOT held open: %s", list)
	}

	resp, body = f.retry(t, ui.SendQueueEditSavePath(first.ID), cookie, true, url.Values{"hold": {held.HoldToken}, "text": {"first, as edited"}})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("Save: %d %q", resp.StatusCode, body)
	}
	saved := f.row(t, first.ID)
	if saved.Held || saved.Body != "first, as edited" || time.Since(saved.EditStepUpAt) > time.Minute || saved.EditedAt.IsZero() {
		t.Fatalf("after Save: held %t, body %q, edit sign-in %s, edited %s", saved.Held, saved.Body, saved.EditStepUpAt, saved.EditedAt)
	}
	f.tick(t)
	if p := f.pasted(); len(p) != 1 || p[0] != "first, as edited" {
		t.Fatalf("after Save the dispatcher pasted %q; want the edited text, first", p)
	}
	// The second's turn: a newer status time releases it.
	f.mirror.agentState = &mirror.AgentState{Status: mirror.StatusIdle, Since: time.Now().UnixMilli() + 1000,
		Observed: time.Now().Add(-time.Hour)}
	f.tick(t)
	if p := f.pasted(); len(p) != 2 || p[1] != "second" {
		t.Fatalf("pasted %q; want the edited first, then the second", p)
	}
}

// TestCancellingAnEditLeavesTheMessageAsItWas: Cancel from a browser form
// releases the hold and comes back to the view saying so; the text is the
// original, and the dispatcher then sends it.
func TestCancellingAnEditLeavesTheMessageAsItWas(t *testing.T) {
	f := newSQFixture(t)
	it := f.enqueue(t, "keep me", 0, 0)
	cookie := sessionCookieFor(f.srv)
	resp, body := f.retry(t, ui.SendQueueEditPath(it.ID), cookie, false, nil)
	if resp.StatusCode != http.StatusSeeOther || !strings.Contains(resp.Header.Get("Location"), "&edit="+strconv.FormatInt(it.ID, 10)) ||
		!strings.Contains(resp.Header.Get("Location"), "notice="+noticeEditing) {
		t.Fatalf("a browser-form Edit: %d %q (Location %q); want back to the view with the row open", resp.StatusCode, body, resp.Header.Get("Location"))
	}
	// The view it comes back to draws the row open (the no-script path).
	_, page := mirrorGet(t, f.srv, f.ts, resp.Header.Get("Location"))
	if !strings.Contains(page, `action="`+ui.SendQueueEditSavePath(it.ID)+`"`) {
		t.Fatalf("the view with ?edit= does not draw the edit form: %s", page)
	}
	tok := f.row(t, it.ID).HoldToken
	resp, body = f.retry(t, ui.SendQueueEditCancelPath(it.ID), cookie, false, url.Values{"hold": {tok}, "text": {"typed, then thrown away"}})
	if resp.StatusCode != http.StatusSeeOther || !strings.Contains(resp.Header.Get("Location"), "notice="+noticeEditCancelled) {
		t.Fatalf("Cancel: %d %q (Location %q)", resp.StatusCode, body, resp.Header.Get("Location"))
	}
	if got := f.row(t, it.ID); got.Held || got.Body != "keep me" || !got.EditedAt.IsZero() {
		t.Fatalf("after Cancel: held %t, body %q, edited %s", got.Held, got.Body, got.EditedAt)
	}
	f.tick(t)
	if p := f.pasted(); len(p) != 1 || p[0] != "keep me" {
		t.Fatalf("pasted %q; want the original text", p)
	}
}

// TestEditAndSaveNeedAFreshSignIn: under a stale sign-in, Edit holds nothing
// and Save changes nothing — 403 + X-Muster-Step-Up to the script, the plain
// confirm page to a browser form, which carries the hold and the text — and
// the confirm page's password completes each under the fresh sign-in, which
// Save stores on the row.
func TestEditAndSaveNeedAFreshSignIn(t *testing.T) {
	f := newSQFixture(t)
	f.mirror.agentState = &mirror.AgentState{Status: mirror.StatusBusy, Since: time.Now().UnixMilli()}
	it := f.enqueue(t, "before", 0, 0)
	stale, fresh := staleCookieFor(f.srv), sessionCookieFor(f.srv)

	resp, body := f.retry(t, ui.SendQueueEditPath(it.ID), stale, true, nil)
	if resp.StatusCode != http.StatusForbidden || resp.Header.Get("X-Muster-Step-Up") != "required" {
		t.Fatalf("a stale Edit from the script: %d %q", resp.StatusCode, body)
	}
	if f.row(t, it.ID).Held {
		t.Fatal("a stale Edit held the row")
	}
	resp, body = f.retry(t, ui.SendQueueEditPath(it.ID), stale, false, nil)
	confirm := ui.SendQueueEditPath(it.ID) + "/confirm"
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `id="stepup-form"`) || !strings.Contains(body, `action="`+confirm+`"`) {
		t.Fatalf("a stale Edit from a browser form: %d; want the confirm page posting to %s: %s", resp.StatusCode, confirm, body)
	}
	resp, _ = f.retry(t, confirm, stale, false, url.Values{"password": {"not it"}})
	if resp.StatusCode == http.StatusSeeOther || f.row(t, it.ID).Held {
		t.Fatalf("a wrong password: %d, held %t", resp.StatusCode, f.row(t, it.ID).Held)
	}
	resp, _ = f.retry(t, confirm, stale, false, url.Values{"password": {connTestPassword}})
	if resp.StatusCode != http.StatusSeeOther || !f.row(t, it.ID).Held {
		t.Fatalf("the right password: %d (Location %q), held %t", resp.StatusCode, resp.Header.Get("Location"), f.row(t, it.ID).Held)
	}
	tok := f.row(t, it.ID).HoldToken

	form := url.Values{"hold": {tok}, "text": {"after"}}
	resp, body = f.retry(t, ui.SendQueueEditSavePath(it.ID), stale, true, form)
	if resp.StatusCode != http.StatusForbidden || resp.Header.Get("X-Muster-Step-Up") != "required" {
		t.Fatalf("a stale Save from the script: %d %q", resp.StatusCode, body)
	}
	if got := f.row(t, it.ID); got.Body != "before" || !got.Held {
		t.Fatalf("a stale Save changed the row: body %q, held %t", got.Body, got.Held)
	}
	resp, body = f.retry(t, ui.SendQueueEditSavePath(it.ID), stale, false, form)
	saveConfirm := ui.SendQueueEditSavePath(it.ID) + "/confirm"
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `action="`+saveConfirm+`"`) ||
		!strings.Contains(body, `name="hold" value="`+tok+`"`) || !strings.Contains(body, `value="after"`) {
		t.Fatalf("a stale Save from a browser form: %d; want the confirm page carrying the hold and the text: %s", resp.StatusCode, body)
	}
	form.Set("password", connTestPassword)
	resp, _ = f.retry(t, saveConfirm, stale, false, form)
	if resp.StatusCode != http.StatusSeeOther || !strings.Contains(resp.Header.Get("Location"), "notice="+noticeEdited) {
		t.Fatalf("Save after the confirm: %d (Location %q)", resp.StatusCode, resp.Header.Get("Location"))
	}
	got := f.row(t, it.ID)
	if got.Body != "after" || got.Held || time.Since(got.EditStepUpAt) > time.Minute {
		t.Fatalf("after the confirm: body %q, held %t, edit sign-in %s", got.Body, got.Held, got.EditStepUpAt)
	}
	// CONTROL: a fresh session's Edit is not asked to confirm.
	if resp, body := f.retry(t, ui.SendQueueEditPath(it.ID), fresh, true, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("CONTROL: a fresh Edit: %d %q", resp.StatusCode, body)
	}
}

// TestEditRefusals: a row being sent is refused, saying so; a routed row has
// no Edit in its menu and is refused if posted anyway; a Save under a hold
// that ran out is refused, saying so, and the row keeps its text; a Save after
// the hold ran out AND the message was sent says it was sent with the
// original; Send now on a held row is refused, saying it is being edited.
func TestEditRefusals(t *testing.T) {
	f := newSQFixture(t)
	f.mirror.agentState = &mirror.AgentState{Status: mirror.StatusBusy, Since: time.Now().UnixMilli()}
	cookie := sessionCookieFor(f.srv)
	sys := context.Background()

	sending := f.enqueue(t, "being sent", 0, 0)
	if _, ok, err := f.queue.Claim(sys, sending.ID, time.Now(), sendqueue.Claim{By: "test", Lease: time.Minute}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if resp, body := f.retry(t, ui.SendQueueEditPath(sending.ID), cookie, true, nil); resp.StatusCode != http.StatusConflict || body != SendQueueEditSending+"\n" {
		t.Fatalf("Edit of a row being sent: %d %q", resp.StatusCode, body)
	}
	if err := f.queue.Finish(sys, sending.ID, "test", sendqueue.Outcome{Sent: true}, time.Now()); err != nil {
		t.Fatal(err)
	}

	routed, err := f.queue.Enqueue(asQueueUser(1, time.Now()), sendqueue.Item{Target: queueTarget, Body: "the agent's answer",
		StepUpAt: time.Now(), RouteID: "rt_0123456789abcdef01234567"})
	if err != nil {
		t.Fatal(err)
	}
	own := f.enqueue(t, "my own", 0, 0)
	_, list := mirrorGet(t, f.srv, f.ts, queueListPath(0))
	if strings.Contains(list, `action="`+ui.SendQueueEditPath(routed.ID)+`"`) {
		t.Fatalf("a routed row's menu has Edit: %s", list)
	}
	if !strings.Contains(list, `action="`+ui.SendQueueEditPath(own.ID)+`"`) {
		t.Fatalf("CONTROL: a person's queued row's menu has no Edit: %s", list)
	}
	if resp, body := f.retry(t, ui.SendQueueEditPath(routed.ID), cookie, true, nil); resp.StatusCode != http.StatusConflict ||
		body != SendQueueRoutedNotEditable+"\n" || f.row(t, routed.ID).Held {
		t.Fatalf("Edit of a routed row: %d %q", resp.StatusCode, body)
	}

	// A hold that ran out (moved into the past on the database's clock).
	if resp, body := f.retry(t, ui.SendQueueEditPath(own.ID), cookie, true, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("Edit: %d %q", resp.StatusCode, body)
	}
	tok := f.row(t, own.ID).HoldToken
	// Send now on the held row is refused, saying why.
	if resp, body := f.retry(t, ui.SendQueueSendNowPath(own.ID), cookie, true, nil); resp.StatusCode != http.StatusConflict || body != SendQueueHeldNow+"\n" {
		t.Fatalf("Send now of a held row: %d %q", resp.StatusCode, body)
	}
	if _, err := f.pool.Exec(authz.System(sys), `UPDATE send_queue SET held_until = now() - interval '1 second' WHERE id = $1`, own.ID); err != nil {
		t.Fatal(err)
	}
	_, list = mirrorGet(t, f.srv, f.ts, queueListPath(own.ID))
	if strings.Contains(list, `data-queue-edit-form`) || !strings.Contains(list, ui.SendQueueLapsedText) {
		t.Fatalf("a lapsed hold: the list still draws the form, or does not say it ran out: %s", list)
	}
	resp, body := f.retry(t, ui.SendQueueEditSavePath(own.ID), cookie, true, url.Values{"hold": {tok}, "text": {"too late"}})
	if resp.StatusCode != http.StatusConflict || body != SendQueueEditLapsed+"\n" || f.row(t, own.ID).Body != "my own" {
		t.Fatalf("Save after the hold ran out: %d %q, body %q", resp.StatusCode, body, f.row(t, own.ID).Body)
	}
	// It ran out and the message was sent with its original text.
	if _, err := f.queue.Hold(asQueueUser(1, time.Now()), own.ID); err != nil {
		t.Fatal(err)
	}
	tok = f.row(t, own.ID).HoldToken
	if _, err := f.pool.Exec(authz.System(sys), `UPDATE send_queue SET held_until = now() - interval '1 second' WHERE id = $1`, own.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := f.queue.Claim(sys, own.ID, time.Now(), sendqueue.Claim{By: "test", Lease: time.Minute}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := f.queue.Finish(sys, own.ID, "test", sendqueue.Outcome{Sent: true}, time.Now()); err != nil {
		t.Fatal(err)
	}
	resp, body = f.retry(t, ui.SendQueueEditSavePath(own.ID), cookie, false, url.Values{"hold": {tok}, "text": {"too late"}})
	if resp.StatusCode != http.StatusSeeOther || !strings.Contains(resp.Header.Get("Location"), "notice="+noticeEditTooLateSent) {
		t.Fatalf("Save after it was sent: %d %q (Location %q)", resp.StatusCode, body, resp.Header.Get("Location"))
	}
}

// TestTheDeliveryChecksTheEditsOwnSignIn: an edited message whose Save's
// sign-in was not fresh when it saved is refused at delivery, pasting nothing;
// the control is the same row with a fresh edit sign-in.
func TestTheDeliveryChecksTheEditsOwnSignIn(t *testing.T) {
	now := time.Now()
	it := sendqueue.Item{StepUpAt: now.Add(-time.Minute), CreatedAt: now, EditedAt: now, EditStepUpAt: now.Add(-stepup.Window() - time.Minute)}
	if got := sendQueueAuthorization(it, now); got != SendQueueNotFreshWhenEdited {
		t.Fatalf("an edit saved under a stale sign-in: %q, want %q", got, SendQueueNotFreshWhenEdited)
	}
	it.EditStepUpAt = now.Add(-time.Minute)
	if got := sendQueueAuthorization(it, now); got != "" {
		t.Fatalf("CONTROL: an edit saved under a fresh sign-in: %q", got)
	}
	// Never edited: the edit's (zero) sign-in is not checked.
	it.EditedAt, it.EditStepUpAt = time.Time{}, time.Time{}
	if got := sendQueueAuthorization(it, now); got != "" {
		t.Fatalf("CONTROL: an unedited row: %q", got)
	}
}

// TestADeliveryRefusesAnEditSavedAtOrBeforeTheCutoff: the sign-in that SAVED
// an edit is held to the account's cutoffs too — a row queued after the
// cutoff but edited under a sign-in before it is refused, pasting nothing;
// the control is the same row edited after the cutoff.
func TestADeliveryRefusesAnEditSavedAtOrBeforeTheCutoff(t *testing.T) {
	r := newQueueAccountRig(t)
	cut := time.Now().Add(-time.Minute).Truncate(time.Second)
	u, _ := r.store.ByID(context.Background(), 1)
	u.SessionsNotBefore = &cut
	r.store.add(u)
	after := cut.Add(2 * time.Second)
	item := sendqueue.Item{ID: 1, Owner: 1, Target: queueTarget, Body: "edited", StepUpAt: after, CreatedAt: after,
		EditedAt: after, EditStepUpAt: cut.Add(-time.Second)}
	if o := r.deliver(item); o.Sent || o.Detail != SendQueueOwnerCutoff || r.pastes() != 0 {
		t.Fatalf("an edit saved before the cutoff: %+v, %d paste(s); want refused, %q", o, r.pastes(), SendQueueOwnerCutoff)
	}
	item.ID, item.EditStepUpAt = 2, after
	if o := r.deliver(item); !o.Sent || r.pastes() != 1 {
		t.Fatalf("CONTROL: an edit saved after the cutoff: %+v, %d paste(s)", o, r.pastes())
	}
}

// TestTheEditHoldIsOneNumber: what the page says the hold lasts is what the
// store holds for.
func TestTheEditHoldIsOneNumber(t *testing.T) {
	if time.Duration(ui.SendQueueEditHoldMinutes)*time.Minute != sendqueue.EditHold {
		t.Fatalf("ui.SendQueueEditHoldMinutes = %d, sendqueue.EditHold = %s", ui.SendQueueEditHoldMinutes, sendqueue.EditHold)
	}
}

// TestAReloadAfterATakeoverDoesNotCarryTheOtherDevicesHold: device A opens a
// message for editing (a browser form: it comes back to the view with the row
// open), device B opens it too and so takes it over, and A reloads the page it
// was sent to. A's page must not draw the form with B's hold — it says the
// message is being edited on another device — and neither may a bare ?edit=;
// A's Save, under the hold A was given, is refused and changes nothing. The
// control is B's own page, which draws B's form, and B's Save, which saves.
func TestAReloadAfterATakeoverDoesNotCarryTheOtherDevicesHold(t *testing.T) {
	f := newSQFixture(t)
	f.mirror.agentState = &mirror.AgentState{Status: mirror.StatusBusy, Since: time.Now().UnixMilli()}
	it := f.enqueue(t, "contested text", 0, 0)
	cookie := sessionCookieFor(f.srv)

	resp, body := f.retry(t, ui.SendQueueEditPath(it.ID), cookie, false, nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("A's Edit: %d %q", resp.StatusCode, body)
	}
	pageA := resp.Header.Get("Location")
	tokA := f.row(t, it.ID).HoldToken
	if resp, body := f.retry(t, ui.SendQueueEditPath(it.ID), cookie, true, nil); resp.StatusCode >= 300 {
		t.Fatalf("B's Edit: %d %q", resp.StatusCode, body)
	}
	tokB := f.row(t, it.ID).HoldToken
	if tokB == "" || tokB == tokA {
		t.Fatalf("B's Edit did not take the row over: %q, %q", tokA, tokB)
	}

	_, reloaded := mirrorGet(t, f.srv, f.ts, pageA)
	if strings.Contains(reloaded, tokB) {
		t.Fatalf("A's reloaded page carries B's hold %q: %s", tokB, reloaded)
	}
	if strings.Contains(reloaded, `action="`+ui.SendQueueEditSavePath(it.ID)+`"`) || !strings.Contains(reloaded, "Being edited on another device") {
		t.Fatalf("A's reloaded page draws an edit form, or does not say another device is editing: %s", reloaded)
	}
	if _, bare := mirrorGet(t, f.srv, f.ts, queueListPath(it.ID)); strings.Contains(bare, tokB) {
		t.Fatalf("a bare ?edit= carries B's hold: %s", bare)
	}
	resp, body = f.retry(t, ui.SendQueueEditSavePath(it.ID), cookie, true, url.Values{"hold": {tokA}, "text": {"A's stale edit"}})
	if resp.StatusCode != http.StatusConflict || f.row(t, it.ID).Body != "contested text" {
		t.Fatalf("A's Save after the takeover: %d %q, body %q", resp.StatusCode, body, f.row(t, it.ID).Body)
	}

	// CONTROL: B's own page draws B's form, and B's Save saves.
	_, pageB := mirrorGet(t, f.srv, f.ts, queueListPath(it.ID)+"&hold="+url.QueryEscape(tokB))
	if !strings.Contains(pageB, `value="`+tokB+`"`) || !strings.Contains(pageB, `data-queue-edit-form`) {
		t.Fatalf("CONTROL: B's page does not draw B's form: %s", pageB)
	}
	if resp, body := f.retry(t, ui.SendQueueEditSavePath(it.ID), cookie, true, url.Values{"hold": {tokB}, "text": {"B's edit"}}); resp.StatusCode >= 300 ||
		f.row(t, it.ID).Body != "B's edit" {
		t.Fatalf("CONTROL: B's Save: %d %q, body %q", resp.StatusCode, body, f.row(t, it.ID).Body)
	}
}
