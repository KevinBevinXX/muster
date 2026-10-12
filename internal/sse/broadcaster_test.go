package sse

import (
	"sync"
	"testing"
	"time"
)

func recv(t *testing.T, ch <-chan Event) (Event, bool) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		return ev, ok
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
		return Event{}, false
	}
}

func TestSubscriberReceivesEvents(t *testing.T) {
	b := New(4)
	id, ch := b.Subscribe(1)
	defer b.Unsubscribe(id)

	if b.Count() != 1 {
		t.Fatalf("Count = %d, want 1", b.Count())
	}

	b.Broadcast(Event{Name: "request.created", Data: `{"id":"req-1"}`, OwnerID: 1})
	ev, _ := recv(t, ch)
	if ev.Name != "request.created" || ev.Data != `{"id":"req-1"}` {
		t.Fatalf("got %+v", ev)
	}

	b.Broadcast(Event{Name: "request.resolved", Data: `{"id":"req-1"}`, OwnerID: 1})
	ev, _ = recv(t, ch)
	if ev.Name != "request.resolved" {
		t.Fatalf("got %+v, want resolved", ev)
	}
}

func TestFanOutToMultipleSubscribers(t *testing.T) {
	b := New(4)
	_, ch1 := b.Subscribe(1)
	_, ch2 := b.Subscribe(1)
	if b.Count() != 2 {
		t.Fatalf("Count = %d, want 2", b.Count())
	}

	b.Broadcast(Event{Name: "e", Data: "d", OwnerID: 1})
	for i, ch := range []<-chan Event{ch1, ch2} {
		ev, _ := recv(t, ch)
		if ev.Name != "e" {
			t.Fatalf("subscriber %d got %+v", i, ev)
		}
	}
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	b := New(4)
	id, ch := b.Subscribe(1)

	b.Unsubscribe(id)
	if b.Count() != 0 {
		t.Fatalf("Count = %d, want 0 after unsubscribe", b.Count())
	}
	// Channel must be closed.
	if _, ok := <-ch; ok {
		t.Fatal("expected channel closed after unsubscribe")
	}
	// Broadcasting after unsubscribe must not panic (no send on closed chan).
	b.Broadcast(Event{Name: "e", Data: "d", OwnerID: 1})

	// Unsubscribe is idempotent.
	b.Unsubscribe(id)
}

func TestBroadcastNonBlockingOnFullBuffer(t *testing.T) {
	b := New(1) // buffer of 1
	id, ch := b.Subscribe(1)
	defer b.Unsubscribe(id)

	// Fill the buffer then broadcast more; must not block.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 10; i++ {
			b.Broadcast(Event{Name: "e", Data: "d", OwnerID: 1})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Broadcast blocked on full subscriber buffer")
	}
	// At least the first buffered event is retrievable.
	if _, ok := recv(t, ch); !ok {
		t.Fatal("expected at least one buffered event")
	}
}

func TestConcurrentSubscribeBroadcast(t *testing.T) {
	b := New(8)
	stop := make(chan struct{})
	var broadcaster sync.WaitGroup

	// Continuously broadcast until stopped.
	broadcaster.Add(1)
	go func() {
		defer broadcaster.Done()
		for {
			select {
			case <-stop:
				return
			default:
				b.Broadcast(Event{Name: "e", Data: "d", OwnerID: 1})
			}
		}
	}()

	// Churn subscribers concurrently with broadcasts.
	for i := 0; i < 500; i++ {
		id, ch := b.Subscribe(1)
		go func() {
			for range ch { // drain until closed by Unsubscribe
			}
		}()
		b.Unsubscribe(id)
	}

	close(stop)
	broadcaster.Wait()
	// All churned subscribers were unsubscribed, so none should remain.
	if b.Count() != 0 {
		t.Fatalf("Count = %d, want 0 after churn", b.Count())
	}
}

// --- ValidData ---------------------------------------------------------------

// TestValidDataRefusesEveryLineTerminatorAndAcceptsOrdinaryPayloads pins the
// predicate in BOTH directions.
//
// 🔴 THE REFUSE SIDE IS WHAT THE FUNCTION IS FOR, BUT THE ACCEPT SIDE IS WHAT STOPS
// IT BEING `return false`. A predicate that refused everything would satisfy every
// injection case here and would break every publish in production.
func TestValidDataRefusesEveryLineTerminatorAndAcceptsOrdinaryPayloads(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
	}{
		{"a bare LF", "{}\n"},
		{"a bare CR", "{}\r"},
		{"CRLF", "{}\r\n"},
		{"a blank line then a forged frame", "{}\n\nevent: request.resolved\ndata: {}"},
		{"a CR-only forged frame", "{}\r\revent: request.created\rdata: {}"},
		{"a newline INSIDE otherwise valid JSON", "{\"a\":\n1}"},
		{"a newline at the very start", "\n{}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if ValidData(tc.data) {
				t.Errorf("ValidData(%q) = true.\nThe api package writes one frame as "+
					"\"event: %%s\\ndata: %%s\\n\\n\", so this payload terminates that frame early "+
					"and emits a SECOND event under a name no allowlist had to admit.", tc.data)
			}
		})
	}
	// ACCEPT. The fixtures are the shapes real producers emit.
	for _, ok := range []string{
		`{"id":"abc"}`,
		`{"agent":"chief","kind":"text","text":"a reply with \n escaped, which is two characters"}`,
		"{}",
		"",
		`{"nested":{"a":[1,2,3]}}`,
	} {
		if !ValidData(ok) {
			t.Errorf("ValidData(%q) = false; this is a payload an in-process producer emits, so "+
				"refusing it would break the bus for everyone", ok)
		}
	}
}

// TestValidDataDoesNotSubsumeAJSONCheck records the relationship two callers depend
// on: neither predicate implies the other, so applying only one leaves a hole.
//
// ⚠ NAME THE HOLES, BECAUSE THEY ARE NOT THE SAME SIZE AND AN EARLIER READING OF THIS
// TEST TREATED THEM AS IF THEY WERE. json.Valid alone leaves raw-newline CORRUPTION of
// the data field; ValidData alone leaves an UNPARSEABLE payload on the wire. Neither
// hole is an allowlist bypass: FORGING an event name needs the bare token `event` at
// the start of a line, which no valid JSON can produce, so either check alone already
// closes that. See api.handleEventPublish's header for the measurement.
func TestValidDataDoesNotSubsumeAJSONCheck(t *testing.T) {
	// Valid JSON that ValidData must REFUSE (whitespace between tokens may be a
	// newline), so a caller that checked only json.Valid would forge a frame.
	if ValidData("{\"a\":\n1}") {
		t.Error("ValidData accepts JSON containing a raw newline; a json.Valid-only caller would " +
			"then be the whole guard, and it does not guard framing")
	}
	// A payload ValidData ACCEPTS that is not JSON at all, so a caller that checked
	// only ValidData would put an unparseable payload on the wire.
	if !ValidData("this is prose, not json") {
		t.Error("ValidData refuses ordinary prose, which would make the JSON check unreachable " +
			"and the relationship this test pins untestable")
	}
}

// --- owner filtering (accounts PR 3b) ------------------------------------------

// TestAnEventReachesOnlyItsOwnersSubscribers is the bus-level half of the
// SSE isolation guarantee: user 2's subscriber never receives user 1's event,
// while user 1's subscribers (both of them) do. RED when Broadcast ignores
// the owner (the pre-3b fan-out to every subscriber).
func TestAnEventReachesOnlyItsOwnersSubscribers(t *testing.T) {
	b := New(4)
	_, one := b.Subscribe(1)
	_, oneAgain := b.Subscribe(1)
	_, two := b.Subscribe(2)

	if !b.Broadcast(Event{Name: "task.changed", Data: `{"id":"7"}`, OwnerID: 1}) {
		t.Fatal("Broadcast refused an event that names an owner")
	}
	for i, ch := range []<-chan Event{one, oneAgain} {
		if ev, _ := recv(t, ch); ev.Data != `{"id":"7"}` {
			t.Fatalf("owner subscriber %d got %+v", i, ev)
		}
	}
	select {
	case ev := <-two:
		t.Fatalf("user 2's subscriber received user 1's event %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
	// Positive control on the second subscriber: it is live, and receives its
	// own user's event.
	b.Broadcast(Event{Name: "task.changed", Data: `{"id":"8"}`, OwnerID: 2})
	if ev, _ := recv(t, two); ev.Data != `{"id":"8"}` {
		t.Fatalf("user 2's subscriber got %+v for its own event", ev)
	}
}

// TestAnEventWithNoOwnerReachesNobody: a zero owner is a wiring bug and fails
// closed — reported false, delivered to no subscriber (not even one that
// itself registered with no owner).
func TestAnEventWithNoOwnerReachesNobody(t *testing.T) {
	b := New(4)
	_, one := b.Subscribe(1)
	_, nobody := b.Subscribe(0)
	if b.Broadcast(Event{Name: "task.changed", Data: "{}"}) {
		t.Fatal("Broadcast reported delivering an event with no owner")
	}
	for i, ch := range []<-chan Event{one, nobody} {
		select {
		case ev := <-ch:
			t.Fatalf("subscriber %d received an owner-less event %+v", i, ev)
		case <-time.After(50 * time.Millisecond):
		}
	}
}
