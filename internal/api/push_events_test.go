package api

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/push"
)

// payloadPusher records every payload, by endpoint.
type payloadPusher struct {
	mu  sync.Mutex
	got map[string][]push.Payload
}

func (p *payloadPusher) Send(_ context.Context, sub push.Subscription, body []byte) push.SendResult {
	var pl push.Payload
	_ = json.Unmarshal(body, &pl)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.got == nil {
		p.got = map[string][]push.Payload{}
	}
	p.got[sub.Endpoint] = append(p.got[sub.Endpoint], pl)
	return push.SendResult{StatusCode: 201}
}

func (p *payloadPusher) to(ep string) []push.Payload {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]push.Payload(nil), p.got[ep]...)
}

func (p *payloadPusher) endpoints() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for ep, v := range p.got {
		if len(v) > 0 {
			out = append(out, ep)
		}
	}
	sort.Strings(out)
	return out
}

func (p *payloadPusher) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.got = nil
}

// manualTimers is the afterFunc seam: a timer fires only when the test says.
type manualTimers struct {
	mu sync.Mutex
	fs []func()
}

type manualTimer struct{}

func (manualTimer) Reset(time.Duration) bool { return true }
func (manualTimer) Stop() bool               { return true }

func (m *manualTimers) after(_ time.Duration, f func()) resettableTimer {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fs = append(m.fs, f)
	return manualTimer{}
}

func (m *manualTimers) fireAll() int {
	m.mu.Lock()
	fs := m.fs
	m.fs = nil
	m.mu.Unlock()
	for _, f := range fs {
		f()
	}
	return len(fs)
}

func ep(tail string) string { return "https://fcm.googleapis.com/fcm/send/" + tail }

func claim(t *testing.T, store push.Store, owner int64, endpoint string) {
	t.Helper()
	var sub push.Subscription
	sub.Endpoint, sub.Keys.P256dh, sub.Keys.Auth = endpoint, "k", "a"
	if _, err := store.Claim(context.Background(), owner, sub, "ua"); err != nil {
		t.Fatal(err)
	}
}

func eventServer(store push.Store, pusher push.Pusher) *Server {
	s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Users: stubUsers{}, Push: push.NewService(store, pusher, push.VAPIDConfig{PublicKey: "k"}, log.New(io.Discard, "", 0))})
	return s
}

// TestAMembersEventReachesOnlyTheMembersDevices runs over the REAL store
// (Postgres, row-level security on): a member's task entering review, its
// leaving review, and its connector request each reach that member's device
// and no one else's — not the admin's, not another member's. The admin's own
// event reaches the admin only (the control: the pipeline is not simply
// silent).
//
// Measured red with the owner filter dropped from PGStore.ForOwner
// (`WHERE $1::bigint > 0`): every device is asked.
func TestAMembersEventReachesOnlyTheMembersDevices(t *testing.T) {
	dsn := dbtest.DSN(t)
	ctx := context.Background()
	p, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	sys := authz.System(ctx)
	var m, x int64
	for _, dst := range []*int64{&m, &x} {
		if err := p.QueryRow(sys, `INSERT INTO users (role) VALUES ('member') RETURNING id`).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	// Other tests in this package write the admin's preferences (the isolation
	// walk posts the toggle form as user 1); start from "everything on".
	if _, err := p.Exec(sys, `DELETE FROM push_preferences WHERE owner_id = ANY($1)`, []int64{authz.OperatorUserID, m, x}); err != nil {
		t.Fatal(err)
	}
	store := push.NewPG(p)
	tag := strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	mine, admin, other := ep("member-"+tag), ep("admin-"+tag), ep("other-"+tag)
	claim(t, store, m, mine)
	claim(t, store, authz.OperatorUserID, admin)
	claim(t, store, x, other)
	t.Cleanup(func() {
		_, _ = p.Exec(sys, `DELETE FROM push_subscriptions WHERE endpoint = ANY($1)`, []string{mine, admin, other})
		_, _ = p.Exec(sys, `DELETE FROM users WHERE id = ANY($1)`, []int64{m, x})
	})
	pusher := &payloadPusher{}
	s := eventServer(store, pusher)

	s.notifyTaskDone(notes.Note{ID: 501, OwnerID: m, Status: notes.StatusReadyForReview, Body: "member's task"})
	s.WaitForPushes()
	if got := pusher.endpoints(); len(got) != 1 || got[0] != mine {
		t.Fatalf("a member's ready-for-review reached %v; want only the member's device", got)
	}
	if pl := pusher.to(mine); pl[0].Title != "Task #501 ready for review" || pl[0].Data["url"] != "/tasks/501" {
		t.Errorf("payload = %+v", pl[0])
	}

	pusher.reset()
	s.notifyTaskDone(notes.Note{ID: 501, OwnerID: m, Status: notes.StatusComplete})
	s.WaitForPushes()
	if got := pusher.endpoints(); len(got) != 1 || got[0] != mine || pusher.to(mine)[0].Type != push.TypeResolved {
		t.Fatalf("leaving review: %v %+v", got, pusher.to(mine))
	}

	pusher.reset()
	mctx := authz.WithPrincipal(ctx, authz.Principal{UserID: m, Role: authz.RoleMember, Via: authz.ViaConnect})
	s.onConnectorRequested(mctx, connectors.Grant{ID: 9, AgentName: "a", Connector: "github"})
	s.WaitForPushes()
	if got := pusher.endpoints(); len(got) != 1 || got[0] != mine {
		t.Fatalf("a member's connector request reached %v", got)
	}

	// Control: the admin's own event reaches the admin's device.
	pusher.reset()
	s.notifyTaskDone(notes.Note{ID: 502, OwnerID: authz.OperatorUserID, Status: notes.StatusReadyForReview})
	s.WaitForPushes()
	if got := pusher.endpoints(); len(got) != 1 || got[0] != admin {
		t.Fatalf("the admin's event reached %v", got)
	}
}

// TestTwoOwnersBurstsCoalesceSeparately: two owners' machine-created bursts
// land in two buffers on two timers — owner 7 gets "3 new tasks" naming only
// its titles, owner 8 gets its own single "New task". The same for comments.
//
// Measured red with ownerBuffer.add keying every owner to one buffer.
func TestTwoOwnersBurstsCoalesceSeparately(t *testing.T) {
	store := &memPush{}
	claim(t, store, 7, ep("seven"))
	claim(t, store, 8, ep("eight"))
	pusher := &payloadPusher{}
	s := eventServer(store, pusher)
	timers := &manualTimers{}
	s.taskCreateAfter, s.taskCommentAfter = timers.after, timers.after

	for i, title := range []string{"seven-a", "seven-b", "seven-c"} {
		s.notifyTaskCreated(notes.Note{ID: int64(10 + i), OwnerID: 7, Body: title})
	}
	s.notifyTaskCreated(notes.Note{ID: 20, OwnerID: 8, Body: "eight-only"})
	if n := timers.fireAll(); n != 2 {
		t.Fatalf("%d debounce timers armed for two owners, want 2", n)
	}
	s.WaitForPushes()
	seven, eight := pusher.to(ep("seven")), pusher.to(ep("eight"))
	if len(seven) != 1 || seven[0].Title != "3 new tasks" || strings.Contains(seven[0].Body, "eight") {
		t.Fatalf("owner 7 got %+v", seven)
	}
	if len(eight) != 1 || eight[0].Title != "New task" || eight[0].Body != "eight-only" {
		t.Fatalf("owner 8 got %+v", eight)
	}

	pusher.reset()
	s.notifyTaskCommented(notes.Note{ID: 10, OwnerID: 7}, notes.Comment{Author: "bot", Body: "seven note"})
	s.notifyTaskCommented(notes.Note{ID: 20, OwnerID: 8}, notes.Comment{Author: "bot", Body: "eight note"})
	s.notifyTaskCommented(notes.Note{ID: 20, OwnerID: 8}, notes.Comment{Author: "bot", Body: "eight again"})
	timers.fireAll()
	s.WaitForPushes()
	if got := pusher.to(ep("seven")); len(got) != 1 || !strings.Contains(got[0].Body, "seven note") {
		t.Fatalf("owner 7's comments: %+v", got)
	}
	if got := pusher.to(ep("eight")); len(got) != 1 || got[0].Title != "2 new comments" {
		t.Fatalf("owner 8's comments: %+v", got)
	}
}

// TestLeavingReviewSendsExactlyOneResolved: ready → complete emits ONE
// "resolved" for that task's tag; a second save of complete emits nothing; a
// task that never buzzed emits nothing; and Safari devices are skipped (a
// silent push is penalised there).
func TestLeavingReviewSendsExactlyOneResolved(t *testing.T) {
	store := &memPush{}
	android, apple := ep("android"), "https://web.push.apple.com/QHd-apple-device"
	claim(t, store, 7, android)
	claim(t, store, 7, apple)
	pusher := &payloadPusher{}
	s := eventServer(store, pusher)

	s.notifyTaskDone(notes.Note{ID: 30, OwnerID: 7, Status: notes.StatusReadyForReview})
	s.notifyTaskDone(notes.Note{ID: 30, OwnerID: 7, Status: notes.StatusReadyForReview}) // re-save: deduped
	s.notifyTaskDone(notes.Note{ID: 30, OwnerID: 7, Status: notes.StatusComplete})
	s.notifyTaskDone(notes.Note{ID: 30, OwnerID: 7, Status: notes.StatusComplete}) // re-save: nothing
	s.notifyTaskDone(notes.Note{ID: 31, OwnerID: 7, Status: notes.StatusComplete}) // never buzzed
	s.WaitForPushes()

	var resolved, shown int
	for _, pl := range pusher.to(android) {
		switch pl.Type {
		case push.TypeResolved:
			resolved++
			if pl.Tag != "task-30-done" {
				t.Errorf("resolved tag %q, want the review notification's task-30-done", pl.Tag)
			}
		default:
			shown++
		}
	}
	if shown != 1 || resolved != 1 {
		t.Fatalf("android got %d shown / %d resolved, want 1 / 1: %+v", shown, resolved, pusher.to(android))
	}
	for _, pl := range pusher.to(apple) {
		if pl.Type == push.TypeResolved {
			t.Fatal("a resolved (silent) push was sent to a web.push.apple.com device")
		}
	}
	if len(pusher.to(apple)) != 1 {
		t.Fatalf("the apple device did not get the shown notification: %+v", pusher.to(apple))
	}
}

// TestMutedEventsAndTheRateCap: an owner who muted "review" gets no review
// push (and nothing else of theirs is muted); past the per-owner cap nothing
// more is sent to that owner while another owner is unaffected; "resolved"
// is never throttled.
func TestMutedEventsAndTheRateCap(t *testing.T) {
	store := &memPush{}
	claim(t, store, 7, ep("seven"))
	claim(t, store, 8, ep("eight"))
	pusher := &payloadPusher{}
	s := eventServer(store, pusher)
	now := time.Unix(1_000_000, 0)
	s.now = func() time.Time { return now }
	as7 := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 7, Role: authz.RoleMember, Via: authz.ViaSession})
	if err := store.SetMuted(as7, []string{push.EventReview}); err != nil {
		t.Fatal(err)
	}
	s.notifyTaskDone(notes.Note{ID: 40, OwnerID: 7, Status: notes.StatusReadyForReview})
	s.pushTask(7, push.EventComment, RouterNotification{Type: "task", ID: "40", Title: "c", Tag: "c"})
	s.WaitForPushes()
	if got := pusher.to(ep("seven")); len(got) != 1 || got[0].Title != "c" {
		t.Fatalf("with review muted, owner 7 got %+v (want only the comment)", got)
	}
	_ = store.SetMuted(as7, nil)

	// A fresh window for the cap: the comment above used one slot of the last.
	pusher.reset()
	now = now.Add(pushOwnerWindow)
	for i := 0; i < pushOwnerBurst+5; i++ {
		s.pushTask(7, push.EventComment, RouterNotification{Type: "task", Title: "burst", Tag: "b"})
	}
	s.pushTask(8, push.EventComment, RouterNotification{Type: "task", Title: "eight", Tag: "e"})
	s.goNotify(7, push.EventResolved, RouterNotification{Type: push.TypeResolved, Tag: "task-1-done"}, nil)
	s.WaitForPushes()
	var burst, cleared int
	for _, pl := range pusher.to(ep("seven")) {
		if pl.Type == push.TypeResolved {
			cleared++
		} else {
			burst++
		}
	}
	if burst != pushOwnerBurst || cleared != 1 {
		t.Fatalf("owner 7: %d shown (want the cap, %d), %d resolved (want 1, never throttled)", burst, pushOwnerBurst, cleared)
	}
	if len(pusher.to(ep("eight"))) != 1 {
		t.Fatal("owner 7's burst spent owner 8's budget")
	}
	now = now.Add(pushOwnerWindow)
	s.pushTask(7, push.EventComment, RouterNotification{Type: "task", Title: "after", Tag: "a"})
	s.WaitForPushes()
	if got := pusher.to(ep("seven")); got[len(got)-1].Title != "after" {
		t.Fatal("the cap did not reset when the window rolled")
	}
}

// TestThePreferencesFormSavesUncheckedAsMuted: the /account form's unchecked
// boxes are the muted events; the page reflects them.
func TestThePreferencesFormSavesUncheckedAsMuted(t *testing.T) {
	store := &memPush{}
	_, h := pushServer(t, store, &recPusher{}, 7)
	form := url.Values{"on": {push.EventReview, push.EventConnector, "not-an-event"}}
	if rec := pushDo(t, h, "POST", accountPushPrefsPath, form.Encode()); rec.Code != 303 {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	got, _ := store.Muted(context.Background(), 7)
	sort.Strings(got)
	want := []string{push.EventAgent, push.EventComment, push.EventCreated, push.EventPlugin, push.EventResolved}
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("muted = %v, want %v", got, want)
	}
	page := pushDo(t, h, "GET", "/account", "").Body.String()
	if !strings.Contains(page, `value="review" checked`) || strings.Contains(page, `value="comment" checked`) {
		t.Errorf("the form does not reflect the saved toggles")
	}
}

// TestNativePushHasOneSendSite: PushService.SendTo is called from sendPush
// alone, and sendPush only from goNotify (server.go) and the account page's
// send test (push_routes.go) — so who receives a notification is decided in
// one place. Positive control: both lists are non-empty.
func TestNativePushHasOneSendSite(t *testing.T) {
	send := callSitesOf(t, "SendTo")
	if len(send) != 1 || !strings.Contains(send[0], "push_routes.go") {
		t.Fatalf("SendTo call sites %v; want exactly one, in sendPush (push_routes.go)", send)
	}
	callers := callSitesOf(t, "sendPush")
	if len(callers) != 2 {
		t.Fatalf("sendPush call sites %v; want 2 (goNotify, handlePushTest)", callers)
	}
	var files []string
	for _, c := range callers {
		files = append(files, c[:strings.IndexByte(c, ':')])
	}
	sort.Strings(files)
	if strings.Join(files, ",") != "push_routes.go,server.go" {
		t.Fatalf("sendPush is called from %v", callers)
	}
}
