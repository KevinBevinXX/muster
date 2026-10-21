package mirror

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestANewHostBeyondTheCapIsRefused: with the hub full, a NEW host is refused
// with ErrTooManyHosts and is not registered; a reconnect of a host already
// there (a replacement) is not a new host and is admitted.
func TestANewHostBeyondTheCapIsRefused(t *testing.T) {
	h := NewHub(log.New(io.Discard, "", 0))
	h.maxHosts = 1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		_ = h.ServeConn(r.Context(), 1, r.URL.Query().Get("host"), ws)
	}))
	defer srv.Close()
	dial := func(host string) (Msg, *websocket.Conn) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/?host="+host, nil)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(Msg{Type: TypeHello, Version: ProtocolVersion, Host: host, Sessions: []string{"main"}})
		_ = ws.Write(ctx, websocket.MessageText, b)
		_, data, err := ws.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var m Msg
		_ = json.Unmarshal(data, &m)
		return m, ws
	}
	if m, ws := dial("alpha"); m.Error != "" {
		t.Fatalf("the first host was refused: %s", m.Error)
	} else {
		defer ws.CloseNow()
	}
	m, ws := dial("bravo")
	defer ws.CloseNow()
	if !strings.Contains(m.Error, ErrTooManyHosts.Error()) {
		t.Fatalf("a host beyond the cap got %+v, want ErrTooManyHosts", m)
	}
	if h.Online(1, "bravo", "main") {
		t.Fatal("the refused host was registered")
	}
	if m, ws := dial("alpha"); m.Error != "" {
		t.Fatalf("a reconnect of a host already there was refused: %s", m.Error)
	} else {
		defer ws.CloseNow()
	}
}

// TestNothingIsSentAHostBeforeItsWelcome: the welcome is the first frame a
// host reads — muster-mirror hangs up on anything else ("muster's first message
// was not a welcome"). While the welcome is held, the host is not online and an
// attach to it is refused as offline rather than writing its open on the
// socket; released, the host's first frame is the welcome.
//
// The race this pins is the tmuxit TestTmuxThroughTheTunnel flake: the host
// was registered BEFORE its welcome was written, an attach that saw it online
// wrote its open first, and the host dropped the connection — the terminal
// ended before it drew anything.
func TestNothingIsSentAHostBeforeItsWelcome(t *testing.T) {
	h := NewHub(log.New(io.Discard, "", 0))
	held, release := make(chan struct{}), make(chan struct{})
	h.beforeWelcome = func() { close(held); <-release }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		_ = h.ServeConn(r.Context(), 1, "alpha", ws)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	b, _ := json.Marshal(Msg{Type: TypeHello, Version: ProtocolVersion, Host: "alpha", Sessions: []string{"main"}})
	if err := ws.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
	<-held
	if h.Online(1, "alpha", "main") {
		t.Error("the host is online before its welcome was written")
	}
	actx, acancel := context.WithCancel(ctx)
	defer acancel()
	attached := make(chan error, 1)
	go func() {
		attached <- h.Attach(actx, 1, "alpha", "main", true, strings.NewReader(""), io.Discard, nil)
	}()
	select {
	case err := <-attached:
		if !errors.Is(err, ErrHostOffline) {
			t.Errorf("an attach while the welcome is held = %v, want ErrHostOffline", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("an attach while the welcome is held did not return: it was sent the host before the welcome")
	}
	close(release)
	_, data, err := ws.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var first Msg
	_ = json.Unmarshal(data, &first)
	if first.Type != TypeWelcome {
		t.Fatalf("the host's first frame is %q (%s), want the welcome", first.Type, data)
	}
	eventuallyInternal(t, "the host online after its welcome", func() bool { return h.Online(1, "alpha", "main") })
}

func eventuallyInternal(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
