package push

import (
	"bytes"
	"context"
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
		store := &memStore{devs: []Device{b, native(2, 7, epNtfy)}}
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

// TestTheNativeTitleIsShort: the title is cut to MaxNativeTitle characters.
func TestTheNativeTitleIsShort(t *testing.T) {
	v := Payload{Type: "attention", Title: strings.Repeat("é", 500)}.V2()
	if n := utf8.RuneCountInString(v.Title); n != MaxNativeTitle || !strings.HasSuffix(v.Title, "…") {
		t.Fatalf("title is %d characters", n)
	}
}
