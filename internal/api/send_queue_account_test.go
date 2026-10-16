package api

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/gotrue/gotruetest"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sendqueue"
	"github.com/ZacxDev/muster/internal/stepup"
	"github.com/ZacxDev/muster/internal/users"
)

// R1: A QUEUED MESSAGE DOES NOT OUTLIVE THE ACCOUNT THAT QUEUED IT. A disable
// and a sign-out everywhere cancel the owner's queued messages, and the
// delivery itself refuses — pasting nothing — for an owner who is disabled or
// whose recorded sign-in is at or before the account's session cutoff.

type queueAccountRig struct {
	*gtFixture
	mirror *fakeMirror
	queue  *memQueue
	owner  string // user 1's GoTrue subject; fakeMirror's host "ws" is user 1's
}

const queueOwnerPW = "the queue owner's password"

func newQueueAccountRig(t *testing.T) *queueAccountRig {
	t.Helper()
	fake := gotruetest.New(t, gtSecret)
	f := &gtFixture{fake: fake, store: newMemUsers()}
	f.s = New(nil, AuthConfig{Mode: AuthModeGoTrue, HookToken: gtHook, PublicURL: "https://muster.example.com"},
		log.New(io.Discard, "", 0))
	fm, q := queueMirror(), &memQueue{}
	f.s.UseExtensions(Extensions{Notes: stubNotesStore{}, Agents: newSendAgents(), SessionLiveness: stubLiveness{},
		Provisioner: newFakeTerminal(), Mirror: fm, SendQueue: q, Gateway: &turnGateway{}})
	f.s.UseGoTrue(GoTrueAuth{Client: fake.Client(), Verifier: fake.Verifier(), Users: f.store})
	f.edge = BrowserListener(f.s.Handler())
	owner := fake.AddUser("owner@example.com", queueOwnerPW)
	f.store.add(users.User{ID: 1, GotrueSub: owner, Email: "owner@example.com"})
	return &queueAccountRig{gtFixture: f, mirror: fm, queue: q, owner: owner}
}

var queueTarget = sendqueue.Target{Host: "ws", Session: "main", Window: "@1"}

// queued is user 1's message, queued now under a fresh sign-in.
func (r *queueAccountRig) queued(t *testing.T, body string) sendqueue.Item {
	t.Helper()
	it, err := r.queue.Enqueue(asQueueUser(1, time.Now()), sendqueue.Item{Target: queueTarget, Body: body, StepUpAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	return it
}

// deliver is the dispatcher's delivery of it, as its owner.
func (r *queueAccountRig) deliver(it sendqueue.Item) sendqueue.Outcome {
	return r.s.sendQueueSend(r.s.queuedPrincipal(context.Background(), it.Owner), it)
}

func (r *queueAccountRig) pastes() int {
	_, p := r.mirror.v2.snapshot()
	return len(p)
}

func (r *queueAccountRig) stateOf(t *testing.T, id int64) sendqueue.Item {
	t.Helper()
	for _, it := range r.queue.all() {
		if it.ID == id {
			return it
		}
	}
	t.Fatalf("no message %d", id)
	return sendqueue.Item{}
}

// TestDisablingTheOwnerStopsTheirQueue: queued, then the owner is disabled —
// every queued message is cancelled, saying why, and a message the dispatcher
// had already claimed is refused at delivery, saying why, with NOTHING pasted.
// The control is the same delivery before the disable, which pastes.
func TestDisablingTheOwnerStopsTheirQueue(t *testing.T) {
	r := newQueueAccountRig(t)
	if o := r.deliver(r.queued(t, "control")); !o.Sent || r.pastes() != 1 {
		t.Fatalf("CONTROL: before the disable, delivery = %+v with %d paste(s); want sent, 1", o, r.pastes())
	}
	waiting := r.queued(t, "still queued at the disable")
	claimed := r.queued(t, "claimed just before the disable")
	if err := r.s.DisableUserAs(context.Background(), users.Actor{Via: "test"}, 1, true); err != nil {
		t.Fatal(err)
	}
	if got := r.stateOf(t, waiting.ID); got.State != sendqueue.Cancelled || got.Detail != SendQueueCancelDisabled {
		t.Fatalf("a message queued at the disable = %+v; want cancelled, %q", got, SendQueueCancelDisabled)
	}
	if o := r.deliver(claimed); o.Sent || o.MayHaveDelivered || o.Detail != SendQueueOwnerDisabled {
		t.Fatalf("delivery for a disabled owner = %+v; want failed, %q", o, SendQueueOwnerDisabled)
	}
	if n := r.pastes(); n != 1 {
		t.Fatalf("%d paste(s) after the disable; want only the control's", n)
	}
}

// TestSigningOutEverywhereStopsTheQueue: queued, then the owner signs out
// everywhere (POST /logout scope=global) — every queued message is cancelled,
// saying why, and nothing is pasted. The control is a plain sign-out of one
// session, which leaves the queue alone.
func TestSigningOutEverywhereStopsTheQueue(t *testing.T) {
	r := newQueueAccountRig(t)
	login := func() (string, string) {
		w := do(r.edge, "POST", "/login", url.Values{"email": {"owner@example.com"}, "password": {queueOwnerPW}})
		ck := setCookies(w)
		if w.Code != http.StatusSeeOther || ck[accessCookieName] == nil {
			t.Fatalf("sign-in: %d %v", w.Code, ck)
		}
		return ck[accessCookieName].Value, ck[refreshCookieName].Value
	}
	m := r.queued(t, "queued before the sign-out")
	at, rt := login()
	if w := do(r.edge, "POST", "/logout", url.Values{}, withCookies(at, rt)); w.Code != http.StatusSeeOther {
		t.Fatalf("logout: %d", w.Code)
	}
	if got := r.stateOf(t, m.ID); got.State != sendqueue.Queued {
		t.Fatalf("CONTROL: a sign-out of one session moved the queue to %+v", got)
	}
	at, rt = login()
	if w := do(r.edge, "POST", "/logout", url.Values{"scope": {"global"}}, withCookies(at, rt)); w.Code != http.StatusSeeOther {
		t.Fatalf("global logout: %d", w.Code)
	}
	if got := r.stateOf(t, m.ID); got.State != sendqueue.Cancelled || got.Detail != SendQueueCancelSignOut {
		t.Fatalf("after sign-out everywhere: %+v; want cancelled, %q", got, SendQueueCancelSignOut)
	}
	if n := r.pastes(); n != 0 {
		t.Fatalf("%d paste(s) after sign-out everywhere", n)
	}
}

// TestADeliveryRefusesASignInAtOrBeforeTheCutoff: the account's session
// cutoff (users.sessions_not_before — a disable or a password reset moves it)
// refuses a message queued under a sign-in at or before it, in whole seconds
// as the session tier refuses a token (the same second is refused), and
// pastes nothing. A sign-in the second after it is delivered: the control.
func TestADeliveryRefusesASignInAtOrBeforeTheCutoff(t *testing.T) {
	r := newQueueAccountRig(t)
	cut := time.Now().Add(-time.Minute).Truncate(time.Second)
	u, _ := r.store.ByID(context.Background(), 1)
	u.SessionsNotBefore = &cut
	r.store.add(u)
	item := func(id int64, at time.Time) sendqueue.Item {
		return sendqueue.Item{ID: id, Owner: 1, Target: queueTarget, Body: "at the cutoff", StepUpAt: at}
	}
	for _, c := range []struct {
		name string
		at   time.Time
	}{
		{"a second before", cut.Add(-time.Second)},
		{"the cutoff's own second", cut.Add(500 * time.Millisecond)},
	} {
		if o := r.deliver(item(1, c.at)); o.Sent || o.Detail != SendQueueOwnerCutoff {
			t.Fatalf("%s: delivery = %+v; want failed, %q", c.name, o, SendQueueOwnerCutoff)
		}
	}
	if n := r.pastes(); n != 0 {
		t.Fatalf("%d paste(s) for sign-ins at or before the cutoff", n)
	}
	if o := r.deliver(item(2, cut.Add(time.Second))); !o.Sent || r.pastes() != 1 {
		t.Fatalf("CONTROL: a sign-in after the cutoff = %+v, %d paste(s)", o, r.pastes())
	}
	// And an owner the account store cannot find sends nothing.
	if o := r.deliver(sendqueue.Item{ID: 3, Owner: 99, Target: queueTarget, Body: "x", StepUpAt: time.Now()}); o.Sent ||
		o.Detail != SendQueueOwnerUnchecked {
		t.Fatalf("an unknown owner = %+v; want %q", o, SendQueueOwnerUnchecked)
	}
}

// TestAManualSendHoldsTheQueueForItsTurn (Y1): a person's Send into the window
// records the status time the window held then — on the hub (Pasted) and on
// the window's open queue rows (NotePaste) — so a queued message is NOT
// released on the status it was sent in, only on a newer one. The queue
// probe's view is the hub's; the row's is what survives a restart.
func TestAManualSendHoldsTheQueueForItsTurn(t *testing.T) {
	fm := queueMirror()
	q := &memQueue{}
	s, ts := queueServer(t, fm, q, &turnGateway{})
	const before = int64(1792000005555)
	observed := time.Now().Add(-time.Hour)
	fm.agentState = &mirror.AgentState{Status: mirror.StatusIdle, Since: before, Observed: observed}
	waiting, err := q.Enqueue(asQueueUser(1, time.Now()), sendqueue.Item{Target: queueTarget, Body: "queued", StepUpAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	head := sendqueue.Head{Item: waiting}
	user1 := stepup.WithAuthTime(asQueueUser(1, time.Now()), time.Now())
	decide := func() sendqueue.Decision { return s.sendQueueDecide(head, s.sendQueueProbe(user1, head)) }
	if d := decide(); !d.Send {
		t.Fatalf("CONTROL: idle an hour, nothing pasted: %+v; want send", d)
	}
	// The person's Send. The fake hub records what the real one does.
	fm.onPaste = func() { fm.agentState.Pasted = before }
	if _, err := s.mirrorPaste(user1, "ws", "main", "@1", "sent by hand", "manual-send-nonce-0001", mirror.PasteClaude); err != nil {
		t.Fatal(err)
	}
	if got := q.all()[0].AfterSince; got != before {
		t.Fatalf("the queued row's AfterSince after a Send = %d, want %d", got, before)
	}
	if d := decide(); d.Send {
		t.Fatalf("a queued message released on the status the Send was made in: %+v", d)
	}
	// A message queued AFTER the Send is stored with the hub's paste time.
	if resp, body := postQueue(t, ts, sessionCookieFor(s), mirrorQueueForm("main", "@1", "queued after the Send")); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("queue after the Send: %d %q", resp.StatusCode, body)
	}
	if got := q.all()[1].AfterSince; got != before {
		t.Fatalf("a message queued after the Send has AfterSince %d, want %d", got, before)
	}
	// The row alone holds it too (the hub forgot: a restart).
	fm.agentState.Pasted = 0
	head.Item = q.all()[0]
	head.TurnAfter = head.AfterSince
	if d := decide(); d.Send {
		t.Fatalf("after a restart, a queued message released on the Send's status: %+v", d)
	}
	// The Send's turn ends: a newer status, observed IdleFor ago.
	fm.agentState.Since, fm.agentState.Observed = before+1, time.Now().Add(-s.sendQueueIdle())
	if d := decide(); !d.Send {
		t.Fatalf("after the Send's turn ended: %+v; want send", d)
	}
}
