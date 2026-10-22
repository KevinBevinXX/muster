package api

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/push"
)

// rawPusher records each send's body, by endpoint, undecoded: the browser and
// the native device get DIFFERENT payloads (push.Payload vs push.PayloadV2).
type rawPusher struct {
	mu  sync.Mutex
	got map[string][][]byte
}

func (p *rawPusher) Send(_ context.Context, sub push.Subscription, body []byte) push.SendResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.got == nil {
		p.got = map[string][][]byte{}
	}
	p.got[sub.Endpoint] = append(p.got[sub.Endpoint], append([]byte(nil), body...))
	return push.SendResult{StatusCode: 201}
}

func (p *rawPusher) one(t *testing.T, ep string, into any) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.got[ep]) != 1 {
		t.Fatalf("%s received %d push(es), want 1", ep, len(p.got[ep]))
	}
	if err := json.Unmarshal(p.got[ep][0], into); err != nil {
		t.Fatal(err)
	}
}

// TestGoNotifyPlainsTheTitleOnly: a notification's TITLE reaches both kinds of
// device — the browser's payload and the native device's payload v2 — as plain
// text, and its BODY as the caller built it. goNotify is the one push title
// sink (the ledger in internal/textplain pins that nothing else sends); bodies
// are plained upstream where they carry a title (taskTitle,
// commentNotificationText) and are sent as written where they do not.
func TestGoNotifyPlainsTheTitleOnly(t *testing.T) {
	store := &memPush{}
	web, native := ep("web-plain"), ep("native-plain")
	claim(t, store, 7, web)
	store.mu.Lock()
	store.nextID++
	var sub push.Subscription
	sub.Endpoint, sub.Keys.P256dh, sub.Keys.Auth = native, "k", "a"
	store.devs = append(store.devs, push.Device{ID: store.nextID, OwnerID: 7, Sub: sub, Kind: push.KindAndroid})
	store.mu.Unlock()
	pusher := &rawPusher{}
	s := eventServer(store, pusher)

	s.goNotify(7, push.EventReview, RouterNotification{Type: taskNotificationType, ID: "31",
		Title: "Task #31: **Ship** [the fix](https://example.com/pr/31)",
		Body:  "`muster` ~~v1~~ <b>v2</b> for snake_case_names", Tag: "t31"}, nil)
	s.WaitForPushes()

	const wantTitle, wantBody = "Task #31: Ship the fix", "`muster` ~~v1~~ <b>v2</b> for snake_case_names"
	var b push.Payload
	pusher.one(t, web, &b)
	if b.Title != wantTitle || b.Body != wantBody {
		t.Errorf("browser payload title=%q body=%q\nwant title=%q body=%q", b.Title, b.Body, wantTitle, wantBody)
	}
	var n push.PayloadV2
	pusher.one(t, native, &n)
	if n.V != 2 || n.Title != wantTitle || n.Body != wantBody {
		t.Errorf("native payload v2 = %+v\nwant title=%q body=%q", n, wantTitle, wantBody)
	}
}

// TestATaskPushBodyKeepsSnakeCase: the realistic path — a task entering review
// pushes its body's first line — through the real helper. The previous
// notification stripper unwrapped `_x_` emphasis INSIDE identifiers, so the
// phone read "snakecasenames".
func TestATaskPushBodyKeepsSnakeCase(t *testing.T) {
	store := &memPush{}
	claim(t, store, 7, ep("review-plain"))
	pusher := &payloadPusher{}
	s := eventServer(store, pusher)

	s.notifyTaskDone(notes.Note{ID: 32, OwnerID: 7, Status: notes.StatusReadyForReview,
		Body: "**Goal:** rename `config` in snake_case_names and [the doc](https://example.com/d)\n\nmore"})
	s.WaitForPushes()
	got := pusher.to(ep("review-plain"))
	if len(got) != 1 {
		t.Fatalf("got %d pushes, want 1", len(got))
	}
	if want := "Goal: rename config in snake_case_names and the doc"; got[0].Body != want {
		t.Errorf("body = %q, want %q", got[0].Body, want)
	}
}

// TestTheAttentionQueueShowsPlainTitles: the attention page's hand-written
// HTML draws an item's source (an agent's or window's name) and its focus (an
// LLM summary) — both titles — as plain text, and an unrouted task's label too
// (while the label that is stored keeps the raw title).
func TestTheAttentionQueueShowsPlainTitles(t *testing.T) {
	out := attentionItemsHTML([]attentionItem{{
		Key: "k33", Kind: attentionKindSession, Since: time.Now(),
		Source: "**Hermes** [chat](https://example.com/h) @ host-a",
		Focus:  "Fixing ~~old~~ `auth` for C#",
		Text:   "waiting",
	}}, time.Now())
	for _, want := range []string{
		"<span data-attention-source>Hermes chat @ host-a</span>",
		">Fixing old auth for C#</span>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("attention HTML lacks %q:\n%s", want, out)
		}
	}
	// The queue's label is plained (taskLabelPlain); the label that is STORED
	// and quoted to the chief (taskLabel) keeps the title as written.
	n := notes.Note{ID: 34, Title: "1. **Route** [it](https://example.com/r) to #34"}
	if got, want := taskLabelPlain(n), "Route it to #34"; got != want {
		t.Errorf("taskLabelPlain = %q, want %q", got, want)
	}
	if got := taskLabel(n); got != n.Title {
		t.Errorf("taskLabel = %q, want the raw title %q", got, n.Title)
	}
}

// TestAConnectorReasonIsSentVerbatim: a connector request's body is the
// connector and the agent's stated reason — not a title — so markdown-looking
// text in it reaches the phone as written.
func TestAConnectorReasonIsSentVerbatim(t *testing.T) {
	store := &memPush{}
	claim(t, store, authz.OperatorUserID, ep("connector-verbatim"))
	pusher := &payloadPusher{}
	s := eventServer(store, pusher)
	ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: authz.OperatorUserID, Role: authz.RoleAdmin, Via: authz.ViaConnect})
	s.onConnectorRequested(ctx, connectors.Grant{ID: 35, AgentName: "a35", Connector: "github",
		Reason: "read `repo_a` **and** issue #12 [docs](x)"})
	s.WaitForPushes()
	got := pusher.to(ep("connector-verbatim"))
	if len(got) != 1 {
		t.Fatalf("got %d pushes, want 1", len(got))
	}
	if want := "github — read `repo_a` **and** issue #12 [docs](x)"; got[0].Body != want {
		t.Errorf("body = %q, want it verbatim: %q", got[0].Body, want)
	}
}

// TestATaskPushBodyIsPlainedExactlyOnce: on an input where the rule is not
// idempotent — escaped markers — the body is plained once (by taskTitle) and
// not again at emit: `\*\*x\*\*` reads "**x**", a second pass would read "x".
func TestATaskPushBodyIsPlainedExactlyOnce(t *testing.T) {
	store := &memPush{}
	claim(t, store, 7, ep("review-once"))
	pusher := &payloadPusher{}
	s := eventServer(store, pusher)
	s.notifyTaskDone(notes.Note{ID: 36, OwnerID: 7, Status: notes.StatusReadyForReview, Body: `Done: \*\*x\*\* shipped`})
	s.WaitForPushes()
	got := pusher.to(ep("review-once"))
	if len(got) != 1 {
		t.Fatalf("got %d pushes, want 1", len(got))
	}
	if want := "Done: **x** shipped"; got[0].Body != want {
		t.Errorf("body = %q, want %q (plained exactly once)", got[0].Body, want)
	}
}
