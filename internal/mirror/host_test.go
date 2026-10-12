package mirror_test

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
)

// drivenResult is what a hand-driven muster observed of one open.
type drivenResult struct {
	backend  *fakeBackend
	closed   string // the host's closed.error for the stream ("" = none / clean)
	gotClose bool
	screen   bool // the stream produced screen bytes (the terminal attached)
}

// drive runs a REAL muster-mirror client (interactive or not, listing
// sessions) against a HAND-DRIVEN muster that sends `open` and, once the
// terminal draws, `input` on it — so each host-side layer is tested with muster
// misbehaving rather than relying on muster's own guards.
func drive(t *testing.T, interactive bool, sessions []string, open mirror.Msg, input string) drivenResult {
	t.Helper()
	res := drivenResult{backend: &fakeBackend{}}
	res.backend.set(sessions...)
	finished := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		defer close(finished)
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if _, _, err := ws.Read(ctx); err != nil { // hello
			return
		}
		write := func(m mirror.Msg) {
			b, _ := json.Marshal(m)
			_ = ws.Write(ctx, websocket.MessageText, b)
		}
		write(mirror.Msg{Type: mirror.TypeWelcome, Version: mirror.ProtocolVersion, Host: "laptop"})
		open.Type, open.ID = mirror.TypeOpen, 1
		write(open)
		for {
			typ, data, err := ws.Read(ctx)
			if err != nil {
				return
			}
			if typ == websocket.MessageBinary {
				if !res.screen {
					res.screen = true
					_ = ws.Write(ctx, websocket.MessageBinary, mirror.EncodeData(1, []byte(input)))
					time.AfterFunc(300*time.Millisecond, cancel)
				}
				continue
			}
			var m mirror.Msg
			if json.Unmarshal(data, &m) == nil && m.Type == mirror.TypeClosed && m.ID == 1 {
				res.closed, res.gotClose = m.Error, true
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	c, err := mirrorhost.New(mirrorhost.Config{URL: srv.URL, Token: mirror.TokenPrefix + "x", Host: "laptop",
		Backend: res.backend, Interactive: interactive, AllowHTTP: true, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = c.RunOnce(ctx) }()
	select {
	case <-finished:
	case <-time.After(6 * time.Second):
		t.Fatal("the hand-driven muster never finished")
	}
	return res
}

func typedInto(res drivenResult, t *testing.T) string {
	if res.backend.attached() == 0 {
		return ""
	}
	typed, _, _ := res.backend.term(t, 0).snapshot()
	return typed
}

// TestTheHostDropsInputForAReadOnlyStreamWhateverMusterSends: an INTERACTIVE
// host, a read-only stream, input sent anyway — the terminal receives none of
// it. The control: the same host, an interactive stream, receives it.
func TestTheHostDropsInputForAReadOnlyStreamWhateverMusterSends(t *testing.T) {
	ro := drive(t, true, []string{"main"}, mirror.Msg{Session: "main", ReadOnly: true}, "typed-anyway")
	if !ro.screen {
		t.Fatal("the read-only stream never drew")
	}
	if got := typedInto(ro, t); got != "" {
		t.Fatalf("muster-mirror typed %q into a READ-ONLY stream", got)
	}
	if !ro.backend.term(t, 0).readOnly {
		t.Fatal("a read-only open attached an interactive client")
	}
	rw := drive(t, true, []string{"main"}, mirror.Msg{Session: "main"}, "typed-anyway")
	if got := typedInto(rw, t); got != "typed-anyway" {
		t.Fatalf("control: an interactive stream on an interactive host received %q", got)
	}
}

// TestTheHostRefusesAnInteractiveOpenItDidNotAdvertise: a host started WITHOUT
// -interactive refuses a rw open (muster misbehaving), attaches nothing, and
// says why. The control is the interactive host above.
func TestTheHostRefusesAnInteractiveOpenItDidNotAdvertise(t *testing.T) {
	res := drive(t, false, []string{"main"}, mirror.Msg{Session: "main"}, "x")
	if res.backend.attached() != 0 {
		t.Fatal("a read-only host attached a client for an INTERACTIVE open")
	}
	if !res.gotClose || !strings.Contains(res.closed, "read-only") {
		t.Fatalf("closed = %q (sent %v), want the read-only refusal", res.closed, res.gotClose)
	}
	// Its read-only opens still work.
	ok := drive(t, false, []string{"main"}, mirror.Msg{Session: "main", ReadOnly: true}, "")
	if !ok.screen {
		t.Fatal("control: a read-only host did not serve a read-only open")
	}
}

// TestTheHostRefusesAnUnlistedSession: muster asks for a session the host did
// not list (it exists on the backend's tmux, but the host never showed it); the
// host refuses without attaching. The control: a listed session attaches.
func TestTheHostRefusesAnUnlistedSession(t *testing.T) {
	res := drive(t, true, []string{"main"}, mirror.Msg{Session: "secret", ReadOnly: true}, "")
	if res.backend.attached() != 0 {
		t.Fatal("the host attached a session it never listed")
	}
	if !res.gotClose || !strings.Contains(res.closed, "not in this host's latest list") {
		t.Fatalf("closed = %q (sent %v), want the unlisted-session refusal", res.closed, res.gotClose)
	}
	if ok := drive(t, true, []string{"main"}, mirror.Msg{Session: "main", ReadOnly: true}, ""); !ok.screen {
		t.Fatal("control: a listed session was not attached")
	}
}
