package push

import (
	"bytes"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"context"
	"github.com/ZacxDev/muster/internal/metrics"
	"io"
	"log"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"
)

// The 4096-byte cap on a paired phone's push: a self-hosted ntfy accepts a
// larger `up*` message with HTTP 200 and silently truncates it, and the phone
// then cannot decrypt it (Phase 0 spike, ntfy 2.31).

// bodyCapture is a webpush.HTTPClient that keeps the encrypted body it was
// handed.
type bodyCapture struct{ body []byte }

func (c *bodyCapture) Do(req *http.Request) (*http.Response, error) {
	b, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	c.body = b
	return &http.Response{StatusCode: http.StatusCreated, Body: io.NopCloser(bytes.NewReader(nil)), Header: http.Header{}}, nil
}

// TestTheEncryptedNativeBodyIsAtMost4096Bytes MEASURES the encryption
// overhead through the production pusher (real aes128gcm, real keys): a
// plaintext of exactly NativeMaxPlaintext bytes encrypts to a body of 4096
// bytes, a one-byte plaintext also to 4096 (webpush-go pads every message to
// the record), and one byte past the maximum cannot be sent at all. So the
// constant is the measured limit, not a guess.
func TestTheEncryptedNativeBodyIsAtMost4096Bytes(t *testing.T) {
	send := func(n int) (*bodyCapture, SendResult) {
		c := &bodyCapture{}
		p := NewWebpushPusher(realVAPID(t, "mailto:ops@example.com"))
		p.client = c
		return c, p.Send(context.Background(), realSubscription(t, "https://ntfy.example.org/upPhone"), bytes.Repeat([]byte("x"), n))
	}
	for _, n := range []int{1, 1000, NativeMaxPlaintext} {
		c, res := send(n)
		if res.Err != nil {
			t.Fatalf("a %d-byte plaintext: %v", n, res.Err)
		}
		if len(c.body) != ntfyMaxBody {
			t.Fatalf("a %d-byte plaintext encrypted to %d bytes; want exactly %d", n, len(c.body), ntfyMaxBody)
		}
	}
	if c, res := send(NativeMaxPlaintext + 1); res.Err == nil {
		t.Fatalf("a plaintext one byte over the maximum was sent (%d encrypted bytes)", len(c.body))
	}
	if NativeMaxPlaintext != 3993 {
		t.Fatalf("NativeMaxPlaintext = %d; the arithmetic says 4096 - 86 - 16 - 1 = 3993", NativeMaxPlaintext)
	}
}

// TestAnOversizeNativePushIsRefusedLoudly: a payload v2 over the limit is
// never handed to the pusher for a native device — it is refused, logged and
// counted as failed — while a browser on the same fan-out (no such limit) is
// still sent the notification. The CONTROL is the same fan-out with a payload
// at the limit, which reaches the phone.
func TestAnOversizeNativePushIsRefusedLoudly(t *testing.T) {
	fan := func(tag string) (*bodyPusher, Report, string) {
		var logs strings.Builder
		b := dev(1, 7, "https://fcm.googleapis.com/fcm/send/b")
		b.Kind = KindBrowser
		store := &memStore{devs: []Device{b, native(2, 7, epNtfy)}, allow: NewAllowlist([]string{"ntfy.example.org"})}
		pusher := &bodyPusher{}
		rep := NewService(store, pusher, VAPIDConfig{}, log.New(&logs, "", 0)).SendTo(context.Background(), 7,
			Payload{Type: "review", ID: "1", Title: "agent", Tag: tag})
		return pusher, rep, logs.String()
	}
	// The tag is the one field v2 carries unshortened; size it to land just
	// over, and exactly at, the limit.
	base, _ := nativeBody(Payload{Type: "review", ID: "1", Title: "agent", Tag: ""})
	room := NativeMaxPlaintext - len(base) - len(`,"tag":""`)

	pusher, rep, logs := fan(strings.Repeat("t", room+1))
	if _, sent := pusher.sent[epNtfy]; sent {
		t.Fatalf("an oversize native payload (%d bytes) was handed to the pusher", len(pusher.sent[epNtfy]))
	}
	if rep.Failed != 1 || rep.Delivered != 1 {
		t.Fatalf("report = %+v; want the phone failed and the browser delivered", rep)
	}
	if !strings.Contains(logs, "REFUSED") || !strings.Contains(logs, "larger than 3993 bytes") {
		t.Fatalf("the refusal was not logged loudly:\n%s", logs)
	}

	pusher, rep, _ = fan(strings.Repeat("t", room))
	got, sent := pusher.sent[epNtfy]
	if !sent || len(got) != NativeMaxPlaintext || rep.Delivered != 2 {
		t.Fatalf("CONTROL: a payload at the limit: sent=%v (%d bytes), report %+v", sent, len(got), rep)
	}
}

// TestTheNativeLinesAreShortAndCutOnACharacter: the title and body are cut
// to MaxNativeTitle and MaxNativeBody characters, with an ellipsis, and never
// in the middle of what a reader sees as one character — a combining accent,
// a skin-tone modifier, a joined emoji or a flag stays whole or goes whole.
// The controls: short lines are untouched, and a plain ASCII line is cut at
// exactly the limit.
func TestTheNativeLinesAreShortAndCutOnACharacter(t *testing.T) {
	v := Payload{Type: "attention", Title: strings.Repeat("a", 500), Body: strings.Repeat("b", 500)}.V2()
	if n := utf8.RuneCountInString(v.Title); n != MaxNativeTitle || !strings.HasSuffix(v.Title, "…") {
		t.Fatalf("title is %d characters", n)
	}
	if n := utf8.RuneCountInString(v.Body); n != MaxNativeBody || !strings.HasSuffix(v.Body, "…") {
		t.Fatalf("body is %d characters", n)
	}
	if got := (Payload{Type: "attention", Title: "agent-7", Body: "repo"}.V2()); got.Title != "agent-7" || got.Body != "repo" {
		t.Fatalf("CONTROL: short lines were changed: %+v", got)
	}
	pad := strings.Repeat("x", MaxNativeTitle-2) // the cut falls on the next rune
	for name, c := range map[string]struct{ cluster, want string }{
		"combining accent":   {"e\u0301", pad + "…"},
		"skin-tone modifier": {"\U0001F44D\U0001F3FD", pad + "…"},
		"ZWJ family":         {"\U0001F469\u200d\U0001F467", pad + "…"},
		"flag":               {"\U0001F1EB\U0001F1F7", pad + "…"},
		"variation selector": {"\u2764\ufe0f", pad + "…"},
	} {
		got := cutClusters(pad+c.cluster+"tail", MaxNativeTitle)
		if got != c.want {
			t.Errorf("%s: cut to %q; want the whole cluster dropped (%q)", name, []rune(got)[len([]rune(got))-3:], c.want[len(c.want)-6:])
		}
	}
}

// TestTheOversizeCounterMoves: a refused native push counts as
// muster_push_delivery_total{result="oversize"} — once per refused device —
// and not as failed; the browser on the same fan-out counts as ok.
func TestTheOversizeCounterMoves(t *testing.T) {
	before := map[string]float64{}
	for _, r := range []string{ResultOK, ResultFailed, ResultOversize} {
		before[r] = testutil.ToFloat64(metrics.PushDelivery.WithLabelValues(r))
	}
	b := dev(1, 7, "https://fcm.googleapis.com/fcm/send/b")
	b.Kind = KindBrowser
	store := &memStore{devs: []Device{b, native(2, 7, epNtfy), native(3, 7, epNtfy+"2")}, allow: NewAllowlist([]string{"ntfy.example.org"})}
	NewService(store, &bodyPusher{}, VAPIDConfig{}, log.New(io.Discard, "", 0)).SendTo(context.Background(), 7,
		Payload{Type: "review", ID: strings.Repeat("9", NativeMaxPlaintext), Tag: "t"})
	for r, want := range map[string]float64{ResultOK: 1, ResultFailed: 0, ResultOversize: 2} {
		if got := testutil.ToFloat64(metrics.PushDelivery.WithLabelValues(r)) - before[r]; got != want {
			t.Errorf("muster_push_delivery_total{result=%q} moved %v, want %v", r, got, want)
		}
	}
	if len(store.failed) != 0 {
		t.Errorf("an oversize refusal moved the device's fail_count: %v", store.failed)
	}
}
