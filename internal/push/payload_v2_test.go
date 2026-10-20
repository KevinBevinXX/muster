package push

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"sync"
	"testing"
)

// bodyPusher records the payload each endpoint was sent.
type bodyPusher struct {
	mu   sync.Mutex
	sent map[string]string
}

func (p *bodyPusher) Send(_ context.Context, sub Subscription, payload []byte) SendResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sent == nil {
		p.sent = map[string]string{}
	}
	p.sent[sub.Endpoint] = string(payload)
	return SendResult{StatusCode: 201}
}

func native(id, owner int64, ep string) Device {
	d := dev(id, owner, ep)
	d.Kind, d.DeviceID = KindAndroid, id
	return d
}

const (
	epFCM    = "https://fcm.googleapis.com/fcm/send/b"
	epApple  = "https://web.push.apple.com/QHapple"
	epNtfy   = "https://ntfy.example.org/upPhone"
	epNtfyOn = "https://web.push.apple.com/QHnativeOnApple"
)

func fanOut(t *testing.T, p Payload) map[string]string {
	t.Helper()
	b := dev(1, 7, epFCM)
	b.Kind = KindBrowser
	a := dev(2, 7, epApple)
	a.Kind = KindBrowser
	store := &memStore{devs: []Device{b, a, native(3, 7, epNtfy), native(4, 7, epNtfyOn)}}
	pusher := &bodyPusher{}
	NewService(store, pusher, VAPIDConfig{}, log.New(io.Discard, "", 0)).SendTo(context.Background(), 7, p)
	return pusher.sent
}

// TestANativeDeviceGetsPayloadV2: for an existing event a native device gets
// payload v2 — v:2, the same type, id and tag, the SAME title and body (WHO and
// WHERE, nothing added) and the tap URL — while a browser on the same fan-out
// gets the payload its service worker reads, unchanged.
func TestANativeDeviceGetsPayloadV2(t *testing.T) {
	p := Payload{Type: "review", ID: "42", Title: "agent-7", Body: "repo/thing: ready for review", Tag: "task-42",
		Data: map[string]string{"url": "/tasks/42"}}
	sent := fanOut(t, p)
	var got PayloadV2
	if err := json.Unmarshal([]byte(sent[epNtfy]), &got); err != nil {
		t.Fatalf("native body %q: %v", sent[epNtfy], err)
	}
	want := PayloadV2{V: 2, Type: "review", ID: "42", Tag: "task-42", Title: "agent-7", Body: "repo/thing: ready for review", URL: "/tasks/42"}
	if got != want {
		t.Fatalf("native payload = %+v; want %+v", got, want)
	}
	var raw map[string]any
	_ = json.Unmarshal([]byte(sent[epNtfy]), &raw)
	for _, k := range []string{"actions", "state", "conv", "data"} {
		if _, ok := raw[k]; ok {
			t.Errorf("the Phase 1 native payload carries %q", k)
		}
	}
	browser, _ := json.Marshal(p)
	if sent[epFCM] != string(browser) || sent[epApple] != string(browser) {
		t.Fatalf("a browser got %q / %q; want the unchanged v1 %q", sent[epFCM], sent[epApple], browser)
	}
}

// TestTheResolvedClearReachesNativeDevicesSilently: a "resolved" reaches every
// native device as a bare v2 withdrawal (type, id, tag — no title, no body),
// keyed on the KIND: a native subscription is sent it even on Apple's host,
// while a Safari BROWSER is still spared it and a non-Apple browser gets v1.
func TestTheResolvedClearReachesNativeDevicesSilently(t *testing.T) {
	sent := fanOut(t, Payload{Type: TypeResolved, ID: "42", Tag: "task-42"})
	for _, ep := range []string{epNtfy, epNtfyOn} {
		if sent[ep] != `{"v":2,"type":"resolved","id":"42","tag":"task-42"}` {
			t.Errorf("native %s got %q; want the bare v2 withdrawal", ep, sent[ep])
		}
	}
	if _, ok := sent[epApple]; ok {
		t.Error("a Safari browser was sent the silent resolved")
	}
	if sent[epFCM] != `{"type":"resolved","id":"42","tag":"task-42"}` {
		t.Errorf("CONTROL: the FCM browser got %q; want the v1 resolved", sent[epFCM])
	}
}
