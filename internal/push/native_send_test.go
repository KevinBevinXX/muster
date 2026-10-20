package push

import (
	"context"
	"io"
	"log"
	"testing"
)

// TestAPhoneOnARemovedOperatorHostIsPrunedAtSendTime: a paired phone's row on
// an operator host is sent to while the host is configured, and once the host
// leaves MUSTER_PUSH_EXTRA_HOSTS the next send POSTs nothing to it and prunes
// it. A browser row on the same host is never sent to at all: operator hosts
// are for phones only.
func TestAPhoneOnARemovedOperatorHostIsPrunedAtSendTime(t *testing.T) {
	const ep = "https://ntfy.example.org/upPhone"
	phone := dev(1, 7, ep)
	phone.Kind, phone.DeviceID = KindAndroid, 1
	browserOnNtfy := dev(2, 7, "https://ntfy.example.org/upBrowser")
	browserOnNtfy.Kind = KindBrowser
	store := &memStore{devs: []Device{phone, browserOnNtfy}, allow: NewAllowlist([]string{"ntfy.example.org"})}
	pusher := &scriptPusher{}
	svc := NewService(store, pusher, VAPIDConfig{}, log.New(io.Discard, "", 0))

	svc.SendTo(context.Background(), 7, Payload{Type: "review", Tag: "t1"})
	if len(pusher.asked) != 1 || pusher.asked[0] != ep {
		t.Fatalf("with the host configured the pusher reached %v; want only the phone", pusher.asked)
	}
	if len(store.pruned) != 1 || store.pruned[0] != 2 {
		t.Fatalf("pruned %v; want the browser row on the operator host", store.pruned)
	}

	store.allow = NewAllowlist(nil) // the operator took the host out
	pusher.asked, store.pruned = nil, nil
	svc.SendTo(context.Background(), 7, Payload{Type: "review", Tag: "t2"})
	if len(pusher.asked) != 0 {
		t.Fatalf("after the host was removed the pusher reached %v; want nothing", pusher.asked)
	}
	if len(store.pruned) != 2 {
		t.Fatalf("pruned %v; want the phone (and the browser row, again)", store.pruned)
	}
}
