package api

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/auth"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sendqueue"
	"github.com/ZacxDev/muster/internal/stepup"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// THE SEND QUEUE's routes and delivery (send_queue.go). The store's own
// guarantees — row-level security, the bound, claim-once, recovery — are
// pinned by internal/sendqueue's Postgres tests, and the isolation walk drives
// these routes against the real store; here the queue is in memory, per
// principal, and what is pinned is THIS package's half: the gates, the step-up
// decision, the fail-closed probe, and that delivery is the send path.
// ---------------------------------------------------------------------------

// memQueue is SendQueue in memory, scoped by the principal's user as the
// table's row-level security scopes it.
type memQueue struct {
	sendqueue.DispatchStore // unused here; nil
	mu                      sync.Mutex
	items                   []sendqueue.Item
	// cancelOwnerErr, when set, fails every CancelOwner, cancelling nothing.
	cancelOwnerErr error
}

func queueUser(ctx context.Context) int64 {
	if p, ok := authz.From(ctx); ok && !p.All() {
		return p.UserID
	}
	return 0
}

func (m *memQueue) Enqueue(ctx context.Context, it sendqueue.Item) (sendqueue.Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	it.ID, it.Owner, it.State, it.CreatedAt = int64(len(m.items)+1), queueUser(ctx), sendqueue.Queued, time.Now()
	m.items = append(m.items, it)
	return it, nil
}

func (m *memQueue) List(ctx context.Context, t sendqueue.Target) ([]sendqueue.Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []sendqueue.Item
	for _, it := range m.items {
		if it.Owner == queueUser(ctx) && it.Target == t {
			out = append(out, it)
		}
	}
	return out, nil
}

func (m *memQueue) Get(ctx context.Context, id int64) (sendqueue.Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, it := range m.items {
		if it.ID == id && it.Owner == queueUser(ctx) {
			return it, nil
		}
	}
	return sendqueue.Item{}, sendqueue.ErrNotFound
}

func (m *memQueue) Cancel(ctx context.Context, id int64, _ time.Time) (sendqueue.Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, it := range m.items {
		if it.ID == id && it.Owner == queueUser(ctx) {
			if it.State != sendqueue.Queued {
				return sendqueue.Item{}, sendqueue.ErrNotCancellable
			}
			m.items[i].State = sendqueue.Cancelled
			return m.items[i], nil
		}
	}
	return sendqueue.Item{}, sendqueue.ErrNotFound
}

func (m *memQueue) HeadOf(ctx context.Context, t sendqueue.Target) (sendqueue.Head, bool, error) {
	items, _ := m.List(ctx, t)
	for _, it := range items {
		if it.State.Open() {
			return sendqueue.Head{Item: it}, true, nil
		}
	}
	return sendqueue.Head{}, false, nil
}

func (m *memQueue) NotePaste(ctx context.Context, t sendqueue.Target, since int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, it := range m.items {
		if it.Owner == queueUser(ctx) && it.Target == t && it.State.Open() && it.AfterSince < since {
			m.items[i].AfterSince = since
		}
	}
	return nil
}

func (m *memQueue) CancelOwner(_ context.Context, owner int64, _ time.Time, detail string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancelOwnerErr != nil {
		return 0, m.cancelOwnerErr
	}
	n := 0
	for i, it := range m.items {
		if it.Owner == owner && it.State == sendqueue.Queued {
			m.items[i].State, m.items[i].Detail = sendqueue.Cancelled, detail
			n++
		}
	}
	return n, nil
}

func (m *memQueue) Retry(ctx context.Context, id int64, stepUpAt time.Time, afterSince int64) (sendqueue.Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, it := range m.items {
		if it.ID != id || it.Owner != queueUser(ctx) {
			continue
		}
		if it.State != sendqueue.Failed || it.RetriedAs != 0 {
			return sendqueue.Item{}, sendqueue.ErrNotRetryable
		}
		n := sendqueue.Item{ID: int64(len(m.items) + 1), Owner: it.Owner, Target: it.Target, Body: it.Body, StepUpAt: stepUpAt,
			AfterSince: afterSince, State: sendqueue.Queued, CreatedAt: time.Now()}
		m.items = append(m.items, n)
		m.items[i].RetriedAs = n.ID
		return n, nil
	}
	return sendqueue.Item{}, sendqueue.ErrNotFound
}

func (m *memQueue) Delete(ctx context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, it := range m.items {
		if it.ID == id && it.Owner == queueUser(ctx) {
			if !it.State.Finished() {
				return sendqueue.ErrNotDeletable
			}
			m.items = append(m.items[:i], m.items[i+1:]...)
			return nil
		}
	}
	return sendqueue.ErrNotFound
}

func (m *memQueue) all() []sendqueue.Item {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]sendqueue.Item(nil), m.items...)
}

// queueWindows is user 1's per-window host "ws": main:@1 runs Claude Code,
// chief:@2 is a shell.
func queueWindows() []mirror.Window {
	return []mirror.Window{
		{Session: "main", ID: "@1", Index: 0, Active: true, Claude: true},
		{Session: "chief", ID: "@2", Index: 0, Active: true, Command: "zsh"},
	}
}

func queueMirror() *fakeMirror {
	return &fakeMirror{online: true, interactive: true, statusReport: true, v2: &fakeMirrorV2{}, windows: queueWindows()}
}

func queueServer(t *testing.T, fm *fakeMirror, q *memQueue, gw Gateway) (*Server, *httptest.Server) {
	t.Helper()
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	ext := Extensions{Notes: stubNotesStore{}, Agents: newSendAgents(), SessionLiveness: stubLiveness{},
		Provisioner: newFakeTerminal(), SendQueue: q, Gateway: gw}
	if fm != nil {
		ext.Mirror = fm
	}
	s.UseExtensions(ext)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

// postQueue posts form to the queue route as the composer's script would, with
// cookie as the session.
func postQueue(t *testing.T, ts *httptest.Server, cookie string, form url.Values) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+ui.SendQueuePath, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Origin", ts.URL)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func mirrorQueueForm(session, window, text string) url.Values {
	f := url.Values{"host": {"ws"}, "session": {session}, "text": {text}}
	if window != "" {
		f.Set("window", window)
	}
	return f
}

// TestQueueingForAMirrorIsGatedLikeASendAndFailsClosed: through the real mux
// and real session cookies, a queue post for a mirrored window passes the SAME
// gates as a send — a fresh sign-in (403 + X-Muster-Step-Up), an -interactive
// host — and then the queue's own: the host must report Claude Code's status
// (an older muster-mirror is refused, saying so) and the window must run it.
// Each refusal stores nothing. The one that passes stores the message with the
// sign-in time of the session that queued it: the step-up decision.
func TestQueueingForAMirrorIsGatedLikeASendAndFailsClosed(t *testing.T) {
	type tc struct {
		name   string
		fm     func() *fakeMirror
		stale  bool
		form   url.Values
		status int
		says   string
	}
	for _, c := range []tc{
		{"stale sign-in", queueMirror, true, mirrorQueueForm("main", "@1", "hi"), http.StatusForbidden, stepup.ErrStale.Error()},
		{"an offline host", func() *fakeMirror { f := queueMirror(); f.online = false; return f }, false,
			mirrorQueueForm("main", "@1", "hi"), http.StatusConflict, "offline"},
		{"read-only host", func() *fakeMirror { f := queueMirror(); f.interactive = false; return f }, false,
			mirrorQueueForm("main", "@1", "hi"), http.StatusConflict, "read-only"},
		{"an older muster-mirror", func() *fakeMirror { f := queueMirror(); f.statusReport = false; return f }, false,
			mirrorQueueForm("main", "@1", "hi"), http.StatusConflict, "does not report whether Claude Code is busy"},
		{"a shell window", queueMirror, false, mirrorQueueForm("chief", "@2", "hi"), http.StatusConflict, SendQueueNotClaude},
		{"a window not listed", queueMirror, false, mirrorQueueForm("main", "@9", "hi"), http.StatusConflict, "no longer lists this tmux window"},
		{"a control character", queueMirror, false, mirrorQueueForm("main", "@1", "a\x1bb"), http.StatusBadRequest, "control character"},
		{"nothing typed", queueMirror, false, mirrorQueueForm("main", "@1", "  "), http.StatusBadRequest, MirrorSendEmpty},
	} {
		q := &memQueue{}
		s, ts := queueServer(t, c.fm(), q, &turnGateway{})
		cookie := sessionCookieFor(s)
		if c.stale {
			cookie = staleCookieFor(s)
		}
		resp, body := postQueue(t, ts, cookie, c.form)
		if resp.StatusCode != c.status || !strings.Contains(body, c.says) {
			t.Errorf("%s: %d %q; want %d saying %q", c.name, resp.StatusCode, body, c.status, c.says)
		}
		if c.stale && resp.Header.Get("X-Muster-Step-Up") != "required" {
			t.Errorf("%s: no X-Muster-Step-Up", c.name)
		}
		if got := q.all(); len(got) != 0 {
			t.Errorf("%s: stored %+v", c.name, got)
		}
	}

	// The control: the same post, every gate passed — from a session signed in
	// an hour ago (fresh: the window is 12 h), so the stored time can be told
	// apart from "now".
	q := &memQueue{}
	s, ts := queueServer(t, queueMirror(), q, &turnGateway{})
	signedInAt := time.Now().Add(-time.Hour).Truncate(time.Second)
	cookie := sessionCookieName + "=" + auth.SignSession(s.auth.uiSessionSecret(), signedInAt, sessionTTL)
	resp, body := postQueue(t, ts, cookie, mirrorQueueForm("main", "@1", "later please"))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("a queue post that passes every gate: %d %q", resp.StatusCode, body)
	}
	got := q.all()
	if len(got) != 1 {
		t.Fatalf("stored %d messages, want 1", len(got))
	}
	want := sendqueue.Target{Host: "ws", Session: "main", Window: "@1"}
	if it := got[0]; it.Owner != 1 || it.Target != want || it.Body != "later please" {
		t.Fatalf("stored %+v", it)
	}
	if at := got[0].StepUpAt; !at.Equal(signedInAt) {
		t.Fatalf("the stored step-up time is %v; want the queueing session's sign-in, %v", at, signedInAt)
	}
	// A cross-site post is refused before anything (requireOperatorWrite).
	req, _ := http.NewRequest(http.MethodPost, ts.URL+ui.SendQueuePath, strings.NewReader(mirrorQueueForm("main", "@1", "x").Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", sessionCookieFor(s))
	req.Header.Set("Origin", "https://evil.example")
	if r, err := http.DefaultClient.Do(req); err != nil || r.StatusCode != http.StatusForbidden {
		t.Fatalf("a cross-site queue post: %v %v", r.StatusCode, err)
	}
	if len(q.all()) != 1 {
		t.Fatal("a cross-site post was stored")
	}
}

// asQueueUser is user uid's request principal, signed in at.
func asQueueUser(uid int64, at time.Time) context.Context {
	return stepup.WithAuthTime(authz.WithPrincipal(context.Background(),
		authz.Principal{UserID: uid, Role: authz.RoleMember, Via: authz.ViaSession}), at)
}

// TestAnotherOwnerCannotSeeOrCancelAQueue: user 1's queued message is not in
// user 2's list for the same address (404 — not "exists, not yours"), and user
// 2's Cancel of it is 404 and leaves it queued. The control is user 1's own
// list (the message is there) and Cancel (204, cancelled).
func TestAnotherOwnerCannotSeeOrCancelAQueue(t *testing.T) {
	q := &memQueue{}
	s, _ := queueServer(t, queueMirror(), q, &turnGateway{})
	it, _ := q.Enqueue(asQueueUser(1, time.Now()), sendqueue.Item{Target: sendqueue.Target{
		Host: "ws", Session: "main", Window: "@1"}, Body: "user one's secret plan", StepUpAt: time.Now()})
	list := func(uid int64) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, ui.SendQueuePath+"?"+url.Values{
			"host": {"ws"}, "session": {"main"}, "window": {"@1"}}.Encode(), nil)
		rec := httptest.NewRecorder()
		s.handleSendQueueList(rec, req.WithContext(asQueueUser(uid, time.Now())))
		return rec
	}
	cancel := func(uid int64) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, ui.SendQueueCancelPath(it.ID), nil)
		req.SetPathValue("id", strconv.FormatInt(it.ID, 10))
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		rec := httptest.NewRecorder()
		s.handleSendQueueCancel(rec, req.WithContext(asQueueUser(uid, time.Now())))
		return rec
	}
	if rec := list(2); rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "secret plan") {
		t.Fatalf("user 2's list of user 1's window: %d %q", rec.Code, rec.Body.String())
	}
	if rec := cancel(2); rec.Code != http.StatusNotFound {
		t.Fatalf("user 2's cancel of user 1's message: %d", rec.Code)
	}
	if got := q.all()[0]; got.State != sendqueue.Queued {
		t.Fatalf("user 2's cancel moved user 1's message to %s", got.State)
	}
	if rec := list(1); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "user one&#39;s secret plan") {
		t.Fatalf("CONTROL: user 1's own list: %d %q", rec.Code, rec.Body.String())
	}
	if rec := cancel(1); rec.Code != http.StatusNoContent || q.all()[0].State != sendqueue.Cancelled {
		t.Fatalf("CONTROL: user 1's own cancel: %d, state %s", rec.Code, q.all()[0].State)
	}
}

// TestAQueuedMirrorMessageIsSentThroughTheSendPathWithItsOwnStepUp: delivery
// pastes through the plugin exactly as a Send does — as the message's owner,
// into the window's CLAUDE CODE pane (the pane whose idle the queue waited on,
// never whatever else is active) — and is authorized by the sign-in the message
// was QUEUED with: one that was inside the window when the message was queued
// is pasted; one that was already past it fails saying so and NOTHING is
// pasted, however fresh anything else is. (Its 7-day bound is
// TestAQueuedMessageStaysAuthorizedForSevenDays.)
func TestAQueuedMirrorMessageIsSentThroughTheSendPathWithItsOwnStepUp(t *testing.T) {
	fm := queueMirror()
	s, _ := queueServer(t, fm, &memQueue{}, &turnGateway{})
	item := func(id int64, at time.Time) sendqueue.Item {
		return sendqueue.Item{ID: id, Owner: 1, Target: sendqueue.Target{Host: "ws", Session: "main", Window: "@1"},
			Body: "queued text " + strconv.FormatInt(id, 10), StepUpAt: at, CreatedAt: time.Now()}
	}
	// The owner's context, as the dispatcher hands it (queuedPrincipal), and
	// itself FRESH: what is presented must be the item's time, not anything
	// the caller carries.
	ctx := stepup.WithAuthTime(s.queuedPrincipal(context.Background(), 1), time.Now())
	// The state the dispatcher decided on: the paste must carry exactly it.
	decided := sendqueue.AgentState{Idle: true, Since: 1792000001111, Pasted: 1792000000222, Observed: time.Now()}

	if o := s.sendQueueSend(ctx, item(1, time.Now().Add(-stepup.Window()-time.Minute)), decided); o.Sent || o.MayHaveDelivered ||
		o.Detail != SendQueueNotFreshWhenQueued {
		t.Fatalf("a message queued under a sign-in past the window: %+v", o)
	}
	if _, pastes := fm.v2.snapshot(); len(pastes) != 0 {
		t.Fatalf("a message past its step-up was pasted: %+v", pastes)
	}
	if o := s.sendQueueSend(ctx, item(2, time.Now()), decided); !o.Sent {
		t.Fatalf("a message inside its window: %+v", o)
	}
	_, pastes := fm.v2.snapshot()
	if len(pastes) != 1 || pastes[0].expect == nil ||
		*pastes[0].expect != (mirror.Turn{Since: 1792000001111, Pasted: 1792000000222}) ||
		pastes[0] != (pasteCall{1, "ws", "main", "queued text 2", mirror.PasteClaude, pastes[0].expect}) {
		t.Fatalf("pasted %+v; want one paste as user 1 into the Claude Code pane", pastes)
	}
	// The same message again is the send path's repeat: not pasted twice.
	if o := s.sendQueueSend(ctx, item(2, time.Now()), decided); !o.Sent {
		t.Fatalf("a repeat: %+v", o)
	}
	if _, pastes := fm.v2.snapshot(); len(pastes) != 1 {
		t.Fatalf("a repeated delivery was pasted again: %+v", pastes)
	}
	// A paste the host never confirmed may have landed.
	fm.v2.pasteErr = fmtUnconfirmed()
	if o := s.sendQueueSend(ctx, item(3, time.Now()), decided); o.Sent || !o.MayHaveDelivered {
		t.Fatalf("an unconfirmed paste: %+v; want failed, may have been delivered", o)
	}
	fm.v2.pasteErr = mirror.ErrNotClaude
	if o := s.sendQueueSend(ctx, item(4, time.Now()), decided); o.Sent || o.MayHaveDelivered {
		t.Fatalf("a paste refused before the pane: %+v; want failed, not delivered", o)
	}
}

// fmtUnconfirmed is an error marked mirror.ErrPasteUnconfirmed, as the hub
// returns one.
func fmtUnconfirmed() error { return errors.Join(mirror.ErrPasteTimeout, mirror.ErrPasteUnconfirmed) }

// TestTheProbeFailsClosed: the view of a mirrored window is idle ONLY when the
// host reports Claude Code idle, and idle since exactly the host's time; busy,
// waiting on the person and every unknown (an older muster-mirror, a status
// without its time, a shell, the host offline) hold the queue, each saying why.
// The probe reads as ctx's principal: another user's ctx sees no such host.
func TestTheProbeFailsClosed(t *testing.T) {
	fm := queueMirror()
	s, _ := queueServer(t, fm, &memQueue{}, &turnGateway{})
	h := sendqueue.Head{Item: sendqueue.Item{Owner: 1, Target: sendqueue.Target{Host: "ws", Session: "main", Window: "@1"}}}
	user1 := asQueueUser(1, time.Now())
	since := time.Now().Add(-time.Hour).UnixMilli() // the host's clock
	observed := time.Now().Add(-time.Minute)        // muster's
	const pasted = int64(1792000004444)
	for _, c := range []struct {
		st   *mirror.AgentState
		idle bool
		says string
	}{
		{nil, false, "does not report whether Claude Code is busy"},
		{&mirror.AgentState{Status: mirror.StatusBusy, Since: since}, false, "Claude Code is working."},
		// 🔴 A status muster does not know is NAMED and still holds — never
		// called "working" (a queue once sat held for hours under that, behind
		// a status that was really an idle prompt).
		{&mirror.AgentState{Status: "compacting", Since: since}, false, `Claude Code reports a status muster does not know: "compacting"`},
		{&mirror.AgentState{Status: mirror.StatusWaiting, Since: since}, false, "waiting for you"},
		{&mirror.AgentState{Unknown: mirror.ErrNotClaude}, false, "no Claude Code session"},
		{&mirror.AgentState{Unknown: mirror.ErrNoStatusSince}, false, "does not report since when"},
		{&mirror.AgentState{Status: mirror.StatusIdle, Since: since, Observed: observed, Pasted: pasted}, true, ""},
	} {
		fm.mu.Lock()
		fm.agentState = c.st
		fm.mu.Unlock()
		got := s.sendQueueProbe(user1, h)
		if got.Idle != c.idle || (c.idle && (got.Since != since || !got.Observed.Equal(observed) || got.Pasted != pasted)) ||
			!strings.Contains(got.Why, c.says) || (c.st != nil && c.st.Status != mirror.StatusBusy && strings.Contains(got.Why, "working")) {
			t.Errorf("host state %+v → probe %+v; want idle=%v saying %q", c.st, got, c.idle, c.says)
		}
	}
	// Read as another user, the same address is no host of theirs.
	fm.agentState = &mirror.AgentState{Status: mirror.StatusIdle, Since: since}
	if got := s.sendQueueProbe(asQueueUser(2, time.Now()), h); got.Idle {
		t.Fatalf("user 2's context reads user 1's host: %+v", got)
	}
}

// TestTheQueueListReadsAsTheRequestNotAsTheQueue (D4): the list's "waiting
// for" line probes the window as the REQUEST's own principal — the session
// that asked — and never mints a send-queue principal on the request path;
// only the dispatcher does (queuedPrincipal is its AsOwner). The control is the
// dispatcher's own probe of the same head, which is ViaQueue.
func TestTheQueueListReadsAsTheRequestNotAsTheQueue(t *testing.T) {
	fm := queueMirror()
	fm.agentState = &mirror.AgentState{Status: mirror.StatusBusy, Since: time.Now().UnixMilli()}
	q := &memQueue{}
	s, ts := queueServer(t, fm, q, &turnGateway{})
	if _, err := q.Enqueue(asQueueUser(1, time.Now()), sendqueue.Item{Target: sendqueue.Target{Host: "ws", Session: "main", Window: "@1"},
		Body: "waiting", StepUpAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	_, page := mirrorGet(t, s, ts, ui.SendQueuePath+"?"+url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@1"}}.Encode())
	if !strings.Contains(page, "Claude Code is working.") {
		t.Fatalf("the list did not probe the window: %q", page)
	}
	fm.mu.Lock()
	vias := append([]authz.Via(nil), fm.agentStateVias...)
	fm.agentStateVias = nil
	fm.mu.Unlock()
	if len(vias) == 0 {
		t.Fatal("the list never probed")
	}
	for _, v := range vias {
		if v != authz.ViaSession {
			t.Fatalf("the list probed through %q; want the request's own door, %q (all: %v)", v, authz.ViaSession, vias)
		}
	}
	// CONTROL: the dispatcher's probe, through its AsOwner.
	s.sendQueueProbe(s.queuedPrincipal(context.Background(), 1), sendqueue.Head{Item: q.all()[0]})
	fm.mu.Lock()
	defer fm.mu.Unlock()
	if len(fm.agentStateVias) != 1 || fm.agentStateVias[0] != authz.ViaQueue {
		t.Fatalf("CONTROL: the dispatcher's probe went through %v; want %q", fm.agentStateVias, authz.ViaQueue)
	}
}

// TestTheComposersDrawQueueOnlyWhereItCanWork: the Rendered view of a Claude
// window on a reporting host draws Queue enabled; the same view on a host that
// does not report status draws it disabled, saying why; no store, no Queue.
// And ONLY the Rendered view: the same window's Terminal view (whose composer
// pastes into whatever pane is active) and a dispatched agent's Terminal draw
// none, with the store wired and the host reporting.
func TestTheComposersDrawQueueOnlyWhereItCanWork(t *testing.T) {
	for _, c := range []struct {
		name     string
		fm       *fakeMirror
		q        *memQueue
		queue    bool
		disabled string
	}{
		{"a reporting host", queueMirror(), &memQueue{}, true, ""},
		{"an older muster-mirror", func() *fakeMirror { f := queueMirror(); f.statusReport = false; return f }(), &memQueue{}, true, "does not report"},
		{"no queue store", queueMirror(), nil, false, ""},
		// A disabled composer (a read-only host) draws no Queue at all.
		{"a read-only host", func() *fakeMirror { f := queueMirror(); f.interactive = false; return f }(), &memQueue{}, false, ""},
	} {
		s, ts := queueServer(t, c.fm, c.q, &turnGateway{})
		if c.q == nil {
			s.ext.SendQueue = nil
		}
		_, page := mirrorGet(t, s, ts, ui.MirrorViewPath("ws", "main")+"&window=%401")
		btn := strings.Contains(page, `id="mv-queue-send"`)
		if btn != c.queue {
			t.Errorf("%s: Queue drawn = %v, want %v", c.name, btn, c.queue)
		}
		if btn {
			tag := page[strings.Index(page, `id="mv-queue-send"`):]
			tag = tag[:strings.Index(tag, ">")]
			// The attribute, not the class `disabled:opacity-40`.
			if disabled := regexp.MustCompile(`\sdisabled(\s|=|$)`).MatchString(tag); disabled != (c.disabled != "") {
				t.Errorf("%s: Queue disabled = %v, want %v (%s)", c.name, disabled, c.disabled != "", tag)
			}
		}
		off := strings.Contains(page, `id="mv-queue-off"`)
		if off != (c.disabled != "") || (c.disabled != "" && !strings.Contains(page, c.disabled)) {
			t.Errorf("%s: the off note drawn = %v; want it saying %q", c.name, off, c.disabled)
		}
	}
	s, ts := queueServer(t, queueMirror(), &memQueue{}, &turnGateway{})
	for name, path := range map[string]string{
		"the mirror Terminal view":      ui.MirrorRef{Host: "ws", Session: "main", Window: "@1"}.TerminalPath(),
		"a dispatched agent's Terminal": ui.AgentTerminalPath("keen-wren"),
	} {
		resp, page := mirrorGet(t, s, ts, path)
		if resp.StatusCode != http.StatusOK || !strings.Contains(page, `id="mv-form"`) {
			t.Fatalf("%s: %d, composer drawn = %v; the page must draw its composer for this to mean anything",
				name, resp.StatusCode, strings.Contains(page, `id="mv-form"`))
		}
		if strings.Contains(page, `id="mv-queue-send"`) || strings.Contains(page, `id="mv-queue"`) {
			t.Errorf("%s draws a Queue", name)
		}
	}
	// CONTROL: the Rendered view of the same window, same server, does.
	if _, page := mirrorGet(t, s, ts, ui.MirrorViewPath("ws", "main")+"&window=%401"); !strings.Contains(page, `id="mv-queue-send"`) {
		t.Error("CONTROL: the Rendered view draws no Queue")
	}
}

// TestASendBetweenTheDecisionAndTheQueuedPasteSupersedesIt (N1): the delivery
// carries the state the dispatcher decided on (mirror.ExpectTurn); a person's
// Send that lands after that decision moves the window's Pasted, and the
// delivery is then refused before anything is pasted — Superseded, waiting
// behind that Send's status time, nothing delivered — instead of being pasted
// into the Send's turn. The control is the same delivery decided on the state
// as it is after the Send, which pastes. (The hub's own refusal, under its
// lock, is TestAQueuedPasteIsRefusedWhenAnotherPasteGotInFirst.)
func TestASendBetweenTheDecisionAndTheQueuedPasteSupersedesIt(t *testing.T) {
	fm := queueMirror()
	s, _ := queueServer(t, fm, &memQueue{}, &turnGateway{})
	const since = int64(1792000006666)
	fm.agentState = &mirror.AgentState{Status: mirror.StatusIdle, Since: since, Observed: time.Now().Add(-time.Hour), Pasted: since - 1000}
	user1 := stepup.WithAuthTime(asQueueUser(1, time.Now()), time.Now())
	it := sendqueue.Item{ID: 7, Owner: 1, Target: queueTarget, Body: "queued", StepUpAt: time.Now(), CreatedAt: time.Now()}
	h := sendqueue.Head{Item: it}
	st := s.sendQueueProbe(user1, h)
	if d := s.sendQueueDecide(h, st); !d.Send {
		t.Fatalf("CONTROL: the dispatcher's decision = %+v; want send", d)
	}
	// The person's Send, between the decision and the paste.
	fm.onPaste = func() { fm.agentState.Pasted = since }
	if _, err := s.mirrorPaste(user1, "ws", "main", "@1", "sent by hand", "manual-send-nonce-0002", mirror.PasteClaude); err != nil {
		t.Fatal(err)
	}
	fm.onPaste = nil
	ctx := stepup.WithAuthTime(s.queuedPrincipal(context.Background(), 1), time.Now())
	o := s.sendQueueSend(ctx, it, st)
	if !o.Superseded || o.Sent || o.MayHaveDelivered || o.AfterSince != since {
		t.Fatalf("a delivery decided before a Send = %+v; want superseded behind %d, nothing delivered", o, since)
	}
	if _, pastes := fm.v2.snapshot(); len(pastes) != 1 || pastes[0].text != "sent by hand" {
		t.Fatalf("pastes %+v; want only the person's Send", pastes)
	}
	// CONTROL: decided on the state after the Send, it is pasted.
	if o := s.sendQueueSend(ctx, it, s.sendQueueProbe(user1, h)); !o.Sent {
		t.Fatalf("CONTROL: a delivery decided after the Send = %+v; want sent", o)
	}
}

// TestASendToTheActiveWindowHoldsThatWindowsQueue (N5): a Send that names no
// window (the session's active window) is recorded on the queue rows of the
// window it RESOLVED to, so they wait for its turn after a restart too. The
// control is a row for another window, which it leaves alone.
func TestASendToTheActiveWindowHoldsThatWindowsQueue(t *testing.T) {
	fm := queueMirror()
	q := &memQueue{}
	s, _ := queueServer(t, fm, q, &turnGateway{})
	const before = int64(1792000007777)
	fm.agentState = &mirror.AgentState{Status: mirror.StatusIdle, Since: before, Observed: time.Now().Add(-time.Hour)}
	user1 := stepup.WithAuthTime(asQueueUser(1, time.Now()), time.Now())
	active, err := q.Enqueue(user1, sendqueue.Item{Target: queueTarget, Body: "queued for @1", StepUpAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	other := queueTarget
	other.Window = "@3"
	elsewhere, err := q.Enqueue(user1, sendqueue.Item{Target: other, Body: "queued for @3", StepUpAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	fm.onPaste = func() { fm.agentState.Pasted = before }
	if _, err := s.mirrorPaste(user1, "ws", "main", "", "sent to the active window", "manual-send-nonce-0003", mirror.PasteClaude); err != nil {
		t.Fatal(err)
	}
	for _, it := range q.all() {
		switch {
		case it.ID == active.ID && it.AfterSince != before:
			t.Fatalf("the active window's queued row has AfterSince %d after a Send to it, want %d", it.AfterSince, before)
		case it.ID == elsewhere.ID && it.AfterSince != 0:
			t.Fatalf("CONTROL: another window's row was stamped %d", it.AfterSince)
		}
	}
}

// TestEachDispatcherHasItsOwnInstanceID (N2): the dispatcher production runs
// (newSendQueueDispatcher, what RunSendQueue starts) names itself; two of
// them — two pods — never share a name, since a shared one would let each
// renew and finish the other's claims. Both carry the host name.
func TestEachDispatcherHasItsOwnInstanceID(t *testing.T) {
	s, _ := queueServer(t, queueMirror(), &memQueue{}, &turnGateway{})
	a, b := s.newSendQueueDispatcher(), s.newSendQueueDispatcher()
	ida, idb := a.InstanceID(), b.InstanceID()
	if ida == "" || ida == idb {
		t.Fatalf("two dispatchers are named %q and %q; want two distinct names", ida, idb)
	}
	if again := a.InstanceID(); again != ida {
		t.Fatalf("a dispatcher's name changed from %q to %q", ida, again)
	}
	if host, err := os.Hostname(); err == nil && !strings.HasPrefix(ida, host+"/") {
		t.Fatalf("the name %q does not carry the host name %q", ida, host)
	}
}
