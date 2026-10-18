package api

import (
	"encoding/json"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/ui"
)

// TestTheAttentionPayloadsAreWhatTheWorkerReads pins the two JSON documents
// an ask's notification is made of, byte for byte. e2e/tests/push.spec.ts
// delivers these SAME two documents to the real service worker and asserts it
// shows the first (tag, link) and closes it on the second — so a change here
// must be made there too, or the browser half tests a payload the server no
// longer sends.
func TestTheAttentionPayloadsAreWhatTheWorkerReads(t *testing.T) {
	store, pusher := &memPush{}, &payloadPusher{}
	claim(t, store, 1, ep("payload"))
	s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Users: stubUsers{}, Push: push.NewService(store, pusher, push.VAPIDConfig{PublicKey: "k"}, log.New(io.Discard, "", 0))})
	a := attention.Ask{ID: 41, Owner: 1, Asker: attention.Asker{AgentID: 7, Label: "worker-7"}, Question: "Deploy the importer now?\nMore detail below.",
		CreatedAt: time.Now()}
	s.attentionNotify(1, askItem(a))
	s.WaitForPushes()
	s.attentionClear(1, attentionAskKey(41))
	s.WaitForPushes()
	var got []string
	for _, p := range pusher.to(ep("payload")) {
		b, _ := json.Marshal(p)
		got = append(got, string(b))
	}
	want := []string{
		`{"type":"attention","id":"ask-41","title":"An agent needs your input","body":"worker-7 is asking","tag":"attention-ask-41","data":{"url":"/attention#ask-41"}}`,
		`{"type":"resolved","id":"ask-41","tag":"attention-ask-41"}`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("payloads:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestTheAttentionMuteSilencesTheItemButNotItsClear: a user who muted
// `attention` gets no new-item notification; the clear rides `resolved`.
func TestTheAttentionMuteSilencesTheItemButNotItsClear(t *testing.T) {
	store, pusher := &memPush{muted: map[int64][]string{1: {push.EventAttention}}}, &payloadPusher{}
	claim(t, store, 1, ep("muted"))
	s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Users: stubUsers{}, Push: push.NewService(store, pusher, push.VAPIDConfig{PublicKey: "k"}, log.New(io.Discard, "", 0))})
	s.attentionNotify(1, askItem(attention.Ask{ID: 5, Owner: 1, Asker: attention.Asker{AgentID: 7}, Question: "q"}))
	s.attentionClear(1, attentionAskKey(5))
	s.WaitForPushes()
	got := pusher.to(ep("muted"))
	if len(got) != 1 || got[0].Type != push.TypeResolved {
		t.Fatalf("muted attention delivered %+v", got)
	}
}

// TestTheBadgeListensForTheEventTheServerSends: the shell badge's trigger
// (internal/ui) names the SSE event this package publishes. Two spellings in
// two packages: a drift would leave the badge refreshing only once a minute.
func TestTheBadgeListensForTheEventTheServerSends(t *testing.T) {
	var page strings.Builder
	if err := ui.RenderPage(&page, "tasks", ui.Features{Attention: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(page.String(), "sse:"+EventAttentionChanged+" ") {
		t.Fatalf("the badge does not listen for %q", EventAttentionChanged)
	}
	if !strings.Contains(attentionPageScript, "'"+EventAttentionChanged+"'") {
		t.Fatal("the page's script does not listen for the queue's event")
	}
}
