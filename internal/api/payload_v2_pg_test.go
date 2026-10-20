package api

import (
	"net/http"
	"testing"

	"github.com/ZacxDev/muster/internal/push"
)

// TestAnExistingEventReachesAPairedPhoneAsPayloadV2: the notification path every
// event takes (goNotify → sendPush → push.Service) delivers payload v2 to a
// phone's registered endpoint — and the task's withdrawal as the bare v2
// "resolved" — keyed on the subscription the device registered.
func TestAnExistingEventReachesAPairedPhoneAsPayloadV2(t *testing.T) {
	r := newDeviceRig(t)
	at, rt := r.login()
	tok, _ := r.pair(at, rt, "Pixel")
	ep := "https://" + devNtfyHost + "/upPhone"
	if w := r.call(r.edge, "PUT", devicePushPath, tok, `{"endpoint":"`+ep+`","p256dh":"BPk","auth":"au"}`); w.Code != http.StatusOK {
		t.Fatalf("register: %d %s", w.Code, w.Body.String())
	}
	last := func() string {
		r.pusher.mu.Lock()
		defer r.pusher.mu.Unlock()
		if n := len(r.pusher.sent); n == 0 || r.pusher.sent[n-1] != ep {
			t.Fatalf("the phone's endpoint was not the last sent to: %v", r.pusher.sent)
		}
		return r.pusher.bodies[len(r.pusher.bodies)-1]
	}
	r.s.goNotify(r.uid, push.EventReview, RouterNotification{Type: "review", ID: "9", Title: "agent-9", Body: "repo: ready for review",
		Tag: "task-9", Data: map[string]string{"url": "/tasks/9"}}, nil)
	r.s.WaitForPushes()
	if got, want := last(), `{"v":2,"type":"review","id":"9","tag":"task-9","title":"agent-9","body":"repo: ready for review","url":"/tasks/9"}`; got != want {
		t.Fatalf("the phone got %s; want %s", got, want)
	}
	r.s.goNotify(r.uid, push.EventResolved, RouterNotification{Type: push.TypeResolved, ID: "9", Tag: "task-9"}, nil)
	r.s.WaitForPushes()
	if got, want := last(), `{"v":2,"type":"resolved","id":"9","tag":"task-9"}`; got != want {
		t.Fatalf("the phone got %s; want %s", got, want)
	}
}
