package api

import (
	"context"
	"io"
	"log"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/plugins"
)

// THE ATTENTION QUEUE's bounds and hygiene: the long-poll's concurrency caps,
// the per-owner wake-up, the store-less dispatch, and a reply draft's
// notification withdrawn when it is superseded or expires.

// waitsOf is how many waits asker has open now.
func (f *attnFixture) waitsOf(asker string) int {
	f.srv.attention.mu.Lock()
	defer f.srv.attention.mu.Unlock()
	return f.srv.attention.waitsByAsker[asker]
}

// TestAttentionWaitsAreBoundedPerAskerAndPerOwner: an asker's third concurrent
// wait is refused at once (two run); an owner's waits past its cap are refused
// for any of its askers; an answer releases them all.
func TestAttentionWaitsAreBoundedPerAskerAndPerOwner(t *testing.T) {
	f := newAttnFixture(t)
	oldA, oldO := attentionWaitsPerAsker, attentionWaitsPerOwner
	attentionWaitsPerAsker, attentionWaitsPerOwner = 2, 3
	t.Cleanup(func() { attentionWaitsPerAsker, attentionWaitsPerOwner = oldA, oldO })
	id := f.ask(t, f.opTok, "Bounded?")
	id2 := f.ask(t, f.op2Tok, "Bounded too?")
	done := make(chan string, 4)
	wait := func(tok string, ask int64) {
		go func() {
			text, _, _ := f.tool(t, tok, attentionWaitTool, map[string]any{"id": ask, "wait_seconds": 30})
			done <- text
		}()
	}
	opKey, op2Key := "agent:"+strconv.FormatInt(f.op.ID, 10), "agent:"+strconv.FormatInt(f.op2.ID, 10)
	wait(f.opTok, id)
	wait(f.opTok, id)
	waitFor(t, func() bool { return f.waitsOf(opKey) == 2 })
	start := time.Now()
	text, isErr, _ := f.tool(t, f.opTok, attentionWaitTool, map[string]any{"id": id, "wait_seconds": 30})
	if !isErr || !strings.Contains(text, "too many attention_wait") || time.Since(start) > 2*time.Second {
		t.Fatalf("the asker's third wait: %s (after %s)", text, time.Since(start))
	}
	wait(f.op2Tok, id2) // the owner's third
	waitFor(t, func() bool { return f.waitsOf(op2Key) == 1 })
	if text, isErr, _ := f.tool(t, f.op2Tok, attentionWaitTool, map[string]any{"id": id2, "wait_seconds": 30}); !isErr || !strings.Contains(text, "too many attention_wait") {
		t.Fatalf("the owner's fourth wait: %s", text)
	}
	// A read with no wait takes no slot.
	if text, _, _ := f.tool(t, f.op2Tok, attentionWaitTool, map[string]any{"id": id2, "wait_seconds": 0}); !strings.Contains(text, `"status":"pending"`) {
		t.Fatalf("a no-wait read: %s", text)
	}
	for _, ask := range []int64{id, id2} {
		if _, err := f.srv.answerAsk(asUser(1), ask, "done"); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		select {
		case text := <-done:
			if !strings.Contains(text, `"status":"answered"`) {
				t.Fatalf("a released wait: %s", text)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a wait did not return after the answer")
		}
	}
	waitFor(t, func() bool { return f.waitsOf(opKey) == 0 && f.waitsOf(op2Key) == 0 })
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("condition not reached")
}

// TestAWakeIsPerOwner: a change to one owner's queue wakes that owner's
// waiters and no one else's.
func TestAWakeIsPerOwner(t *testing.T) {
	s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	a, b := s.attention.wakeFor(1).wait(), s.attention.wakeFor(2).wait()
	s.attentionChanged(2, "")
	select {
	case <-b:
	default:
		t.Fatal("owner 2's waiter was not woken by owner 2's change")
	}
	select {
	case <-a:
		t.Fatal("owner 1's waiter was woken by owner 2's change")
	default:
	}
	s.attentionChanged(1, "")
	select {
	case <-a:
	default:
		t.Fatal("owner 1's waiter was not woken by owner 1's change")
	}
}

// TestTheAttentionToolsRunNothingWithoutTheirStore: with no attention store
// the tools are unknown — refused before anything touches the (nil) store.
func TestTheAttentionToolsRunNothingWithoutTheirStore(t *testing.T) {
	s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Notes: stubNotesStore{}, SessionLiveness: stubLiveness{}})
	a := agents.Agent{ID: 7, OwnerID: 1, Name: "storeless"}
	for _, n := range []string{attentionAskTool, attentionWaitTool, attentionCancelTool, tasksListTool, taskGetTool} {
		if got := s.AgentToolDispatch(asUser(1), a)(n, `{"question":"q","id":1}`); !strings.Contains(got, "unknown tool") {
			t.Fatalf("%s with no store: %s", n, got)
		}
	}
}

// pendingDraft files a pending reply draft of the operator's to a fresh
// message delivered to the operator's agent.
func (f *attnFixture) pendingDraft(t *testing.T, body string) (int64, plugins.Draft) {
	t.Helper()
	ctx := asUser(1)
	ps, err := f.plugins.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var p plugins.Plugin
	if len(ps) > 0 {
		p = ps[0]
	} else if _, p, err = f.plugins.Create(ctx, "attn-inbox", "Attention inbox", "attn-inbox"); err != nil {
		t.Fatal(err)
	}
	m, _, err := f.plugins.Submit(ctx, p.ID, plugins.MessageIn{ExternalID: "attn-" + strconv.FormatInt(time.Now().UnixNano(), 36), Sender: "s@example.test", Body: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM plugin_messages WHERE id = $1`, m.ID)
	})
	if ok, err := f.plugins.BeginDelivery(ctx, m.ID, f.op.ID); err != nil || !ok {
		t.Fatalf("begin delivery: %v %v", ok, err)
	}
	if ok, err := f.plugins.RecordDelivery(ctx, m.ID, f.op.ID, plugins.StatusDelivered); err != nil || !ok {
		t.Fatalf("record delivery: %v %v", ok, err)
	}
	d, _, err := f.plugins.FileDraft(ctx, f.op.ID, 1, f.op.Name, m.ID, body)
	if err != nil {
		t.Fatal(err)
	}
	return m.ID, d
}

// TestADraftsNotificationIsWithdrawnWhenSupersededOrExpired: a newer draft for
// the same message withdraws the older one's notification; a draft past its
// expiry has its notification withdrawn by the sweep — including one notified
// before this process started (seeded from the store).
func TestADraftsNotificationIsWithdrawnWhenSupersededOrExpired(t *testing.T) {
	f := newAttnFixture(t)
	resolved := func() []string {
		var out []string
		for _, p := range strings.Split(strings.Join(f.pushes(), "\n"), "\n") {
			if strings.HasPrefix(p, "resolved plugin-draft-") {
				out = append(out, strings.TrimPrefix(p, "resolved "))
			}
		}
		return out
	}
	msg, d1 := f.pendingDraft(t, "first reply")
	f.srv.draftFiled(1, d1)
	d2, _, err := f.plugins.FileDraft(asUser(1), f.op.ID, 1, f.op.Name, msg, "second reply")
	if err != nil {
		t.Fatal(err)
	}
	f.srv.draftFiled(1, d2)
	if got := resolved(); strings.Join(got, ",") != draftTag(d1.ID) {
		t.Fatalf("superseding withdrew %v, want %s", got, draftTag(d1.ID))
	}
	// Not yet expired: nothing more.
	f.srv.expireDraftNotes(context.Background())
	if got := resolved(); len(got) != 1 {
		t.Fatalf("an unexpired draft was withdrawn: %v", got)
	}
	// Past its expiry: withdrawn, once.
	f.srv.now = func() time.Time { return d2.ExpiresAt.Add(time.Second) }
	f.srv.expireDraftNotes(context.Background())
	f.srv.expireDraftNotes(context.Background())
	if got := resolved(); strings.Join(got, ",") != draftTag(d1.ID)+","+draftTag(d2.ID) {
		t.Fatalf("expiry withdrew %v", got)
	}
	// A restart: a draft notified before it is still withdrawn at its expiry.
	_, d3 := f.pendingDraft(t, "third reply")
	f.srv.attention = attentionState{}
	f.srv.now = func() time.Time { return d3.ExpiresAt.Add(time.Second) }
	f.srv.expireDraftNotes(context.Background())
	if got := resolved(); !containsStr(got, draftTag(d3.ID)) {
		t.Fatalf("after a restart the expired draft was not withdrawn: %v", got)
	}
}
