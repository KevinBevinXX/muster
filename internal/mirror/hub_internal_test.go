package mirror

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
