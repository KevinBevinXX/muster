package push

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ZacxDev/muster/internal/metrics"
)

// memStore is Store in memory, with the owner filter played honestly.
type memStore struct {
	mu        sync.Mutex
	devs      []Device
	delivered []int64
	failed    []int64
	pruned    []int64
	errOn     string // "mark" | "prune": that call fails with an error quoting the endpoint
	allow     Allowlist
}

func (m *memStore) Claim(context.Context, int64, Subscription, string) (int64, error)     { return 0, nil }
func (m *memStore) Devices(context.Context) ([]Device, error)                             { return nil, nil }
func (m *memStore) Remove(context.Context, int64) (bool, error)                           { return false, nil }
func (m *memStore) Unsubscribe(context.Context, string) (bool, error)                     { return false, nil }
func (m *memStore) ForgetDevice(context.Context, int64, string) (bool, error)             { return false, nil }
func (m *memStore) ClaimNative(context.Context, int64, int64, Subscription, string) error { return nil }
func (m *memStore) ForgetNative(context.Context, int64, int64) (bool, error)              { return false, nil }
func (m *memStore) Muted(context.Context, int64) ([]string, error)                        { return nil, nil }
func (m *memStore) MyMuted(context.Context) ([]string, error)                             { return nil, nil }
func (m *memStore) SetMuted(context.Context, []string) error                              { return nil }
func (m *memStore) Allowlist() Allowlist                                                  { return m.allow }

func (m *memStore) ForOwner(_ context.Context, owner int64) ([]Device, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Device
	for _, d := range m.devs {
		if d.OwnerID == owner {
			out = append(out, d)
		}
	}
	return out, nil
}

func (m *memStore) MarkDelivered(_ context.Context, d Device) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.errOn == "mark" {
		return fmt.Errorf("update failed for endpoint %s", d.Sub.Endpoint)
	}
	m.delivered = append(m.delivered, d.ID)
	return nil
}

func (m *memStore) MarkFailed(_ context.Context, d Device) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.errOn == "mark" {
		return fmt.Errorf("update failed for endpoint %s", d.Sub.Endpoint)
	}
	m.failed = append(m.failed, d.ID)
	return nil
}

func (m *memStore) Prune(_ context.Context, d Device) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.errOn == "prune" {
		return false, fmt.Errorf("delete failed: ERROR near %q", d.Sub.Endpoint)
	}
	m.pruned = append(m.pruned, d.ID)
	return true, nil
}

// scriptPusher answers per endpoint and records who it was asked to reach.
type scriptPusher struct {
	mu    sync.Mutex
	by    map[string]SendResult
	asked []string
}

func (p *scriptPusher) Send(_ context.Context, sub Subscription, _ []byte) SendResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.asked = append(p.asked, sub.Endpoint)
	if r, ok := p.by[sub.Endpoint]; ok {
		return r
	}
	return SendResult{StatusCode: 201}
}

func dev(id, owner int64, ep string) Device {
	d := Device{ID: id, OwnerID: owner}
	d.Sub.Endpoint = ep
	d.Sub.Keys.P256dh, d.Sub.Keys.Auth = "k", "a"
	return d
}

// TestSendToSettlesEachDeviceAndReachesOnlyTheOwner: SendTo asks the pusher
// for the owner's devices and no one else's; a 410 prunes, a 201 stamps
// delivery, a 500 and a transport error count a failure; the counter moves by
// exactly those results.
func TestSendToSettlesEachDeviceAndReachesOnlyTheOwner(t *testing.T) {
	const (
		ok    = "https://fcm.googleapis.com/fcm/send/owner7-ok"
		gone  = "https://fcm.googleapis.com/fcm/send/owner7-gone"
		bad   = "https://fcm.googleapis.com/fcm/send/owner7-500"
		down  = "https://fcm.googleapis.com/fcm/send/owner7-down"
		else_ = "https://fcm.googleapis.com/fcm/send/owner8-other"
	)
	store := &memStore{devs: []Device{dev(1, 7, ok), dev(2, 7, gone), dev(3, 7, bad), dev(4, 7, down), dev(5, 8, else_)}}
	pusher := &scriptPusher{by: map[string]SendResult{
		gone: {StatusCode: 410, Expired: true},
		bad:  {StatusCode: 500},
		down: {Err: errors.New("dial tcp: refused")},
	}}
	before := map[string]float64{}
	for _, r := range []string{ResultOK, ResultExpired, ResultFailed} {
		before[r] = testutil.ToFloat64(metrics.PushDelivery.WithLabelValues(r))
	}
	var logs bytes.Buffer
	s := NewService(store, pusher, VAPIDConfig{PublicKey: "pk"}, log.New(&logs, "", 0))

	rep := s.SendTo(context.Background(), 7, Payload{Type: "task", Title: "t", Tag: "task-1-done"})

	if rep != (Report{Devices: 4, Delivered: 1, Expired: 1, Failed: 2}) {
		t.Fatalf("report = %+v", rep)
	}
	sort.Strings(pusher.asked)
	for _, ep := range pusher.asked {
		if ep == else_ {
			t.Fatal("SendTo(7) reached owner 8's device")
		}
	}
	if len(pusher.asked) != 4 {
		t.Fatalf("asked %d devices, want 4", len(pusher.asked))
	}
	if fmt.Sprint(store.delivered) != "[1]" || fmt.Sprint(store.pruned) != "[2]" {
		t.Errorf("delivered %v pruned %v", store.delivered, store.pruned)
	}
	sort.Slice(store.failed, func(i, j int) bool { return store.failed[i] < store.failed[j] })
	if fmt.Sprint(store.failed) != "[3 4]" {
		t.Errorf("failed %v, want [3 4]", store.failed)
	}
	for r, want := range map[string]float64{ResultOK: 1, ResultExpired: 1, ResultFailed: 2} {
		if got := testutil.ToFloat64(metrics.PushDelivery.WithLabelValues(r)) - before[r]; got != want {
			t.Errorf("muster_push_delivery_total{result=%q} moved %v, want %v", r, got, want)
		}
	}
	if !strings.Contains(logs.String(), "push: delivered tag=task-1-done owner=7 to 1/4 device(s)") {
		t.Errorf("no per-owner delivery line:\n%s", logs.String())
	}

	// No devices: nothing is sent and nothing counted.
	pusher.asked = nil
	if rep := s.SendTo(context.Background(), 99, Payload{Type: "task"}); rep != (Report{}) || len(pusher.asked) != 0 {
		t.Errorf("an owner with no devices: %+v, asked %v", rep, pusher.asked)
	}
}

// TestNoLogLineCarriesAnEndpoint: an endpoint is a capability. Every failure
// path SendTo logs — a *url.Error from the transport (which stringifies the
// WHOLE request URL), a store error quoting the endpoint, a non-2xx status —
// must log the device as Short() and never the endpoint's path.
//
// Measured red: with Redact replaced by err.Error() at the transport line, the
// url.Error leg fails.
func TestNoLogLineCarriesAnEndpoint(t *testing.T) {
	const secretTail = "dQw4w9WgXcQ-capability-tail-0123456789"
	ep := "https://fcm.googleapis.com/fcm/send/" + secretTail
	for _, leg := range []struct {
		name   string
		res    SendResult
		errOn  string
		expect string
	}{
		{"transport url.Error", SendResult{Err: &url.Error{Op: "Post", URL: ep, Err: errors.New("dial tcp: no such host")}}, "", "failed"},
		{"store error on mark", SendResult{StatusCode: 201}, "mark", "record delivery"},
		{"store error on prune", SendResult{StatusCode: 410, Expired: true}, "prune", "prune"},
		{"status", SendResult{StatusCode: 403}, "", "status 403"},
	} {
		t.Run(leg.name, func(t *testing.T) {
			var logs bytes.Buffer
			store := &memStore{devs: []Device{dev(1, 3, ep)}, errOn: leg.errOn}
			s := NewService(store, &scriptPusher{by: map[string]SendResult{ep: leg.res}}, VAPIDConfig{}, log.New(&logs, "", 0))
			s.SendTo(context.Background(), 3, Payload{Type: "task", Tag: "x"})
			out := logs.String()
			// Positive control: the failure WAS logged, naming the device.
			if !strings.Contains(out, leg.expect) || !strings.Contains(out, Short(ep)) {
				t.Fatalf("the failure was not logged as expected (%q, %q):\n%s", leg.expect, Short(ep), out)
			}
			if strings.Contains(out, secretTail) || strings.Contains(out, secretTail[:12]) {
				t.Fatalf("a log line carries the endpoint's capability:\n%s", out)
			}
		})
	}
}

func TestShortAndRedact(t *testing.T) {
	ep := "https://updates.push.services.mozilla.com/wpush/v2/gAAAAABsecretsecretsecret"
	if s := Short(ep); strings.Contains(s, "gAAAA") || !strings.HasPrefix(s, "updates.push.services.mozilla.com/…#") {
		t.Errorf("Short = %q", s)
	}
	if Short(ep) == Short(ep+"x") {
		t.Error("two endpoints share a Short form; the fingerprint is not distinguishing them")
	}
	// The re-serialised form (url.Error prints u.String()) and the bare path.
	weird := "https://fcm.googleapis.com/fcm/send/a%2Fb-tail-secret-value"
	u, _ := url.Parse(weird)
	for _, msg := range []string{"x " + u.String() + " y", "near " + u.Path} {
		if got := Redact(errors.New(msg), weird); strings.Contains(got, "tail-secret-value") {
			t.Errorf("Redact(%q) = %q", msg, got)
		}
	}
	if Redact(nil, ep) != "<nil>" {
		t.Error("nil error")
	}
}
