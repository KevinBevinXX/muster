package mirror

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestABurstOfActivityChangesSchedulesOneTrailingTimerAndADisconnectCancelsIt:
// 5000 activity-only changes inside ONE throttle window schedule at most one
// trailing timer (each later change is covered by the pending one), and the
// host disconnecting cancels it — a timer must not outlive its connection.
func TestABurstOfActivityChangesSchedulesOneTrailingTimerAndADisconnectCancelsIt(t *testing.T) {
	h := NewHub(log.New(io.Discard, "", 0))
	h.debounce = time.Millisecond
	h.activityEvery = time.Hour // no trailing nudge fires inside this test
	var mu sync.Mutex
	var timers []*time.Timer
	h.activityAfter = func(d time.Duration, f func()) *time.Timer {
		tm := time.AfterFunc(d, f)
		mu.Lock()
		timers = append(timers, tm)
		mu.Unlock()
		return tm
	}
	var nudges sync.WaitGroup
	nudges.Add(1) // the connect's
	var once sync.Once
	h.SetNotify(func(int64) { once.Do(nudges.Done) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		_ = h.ServeConn(r.Context(), 1, "alpha", ws)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	send := func(m Msg) {
		b, _ := json.Marshal(m)
		if err := ws.Write(ctx, websocket.MessageText, b); err != nil {
			t.Fatal(err)
		}
	}
	const changes = 5000
	start := time.Now().Unix() - (changes+10)*ActivityBucket
	start -= start % ActivityBucket
	send(Msg{Type: TypeHello, Version: ProtocolVersion, Host: "alpha", Sessions: []string{"main"}, Activity: map[string]int64{"main": start}})
	if _, _, err := ws.Read(ctx); err != nil { // the welcome
		t.Fatal(err)
	}
	nudges.Wait() // the counter can see a nudge

	activity := func() int64 {
		for _, v := range h.Hosts(1) {
			return v.Activity["main"]
		}
		return 0
	}
	last := start
	for i := 1; i <= changes; i++ {
		last = start + int64(i)*ActivityBucket
		send(Msg{Type: TypeSessions, Sessions: []string{"main"}, Activity: map[string]int64{"main": last}})
	}
	deadline := time.Now().Add(20 * time.Second)
	for activity() != last {
		if time.Now().After(deadline) {
			t.Fatalf("the hub never took the last change: activity %d, want %d", activity(), last)
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	n := len(timers)
	mu.Unlock()
	// The first change nudges at once (the window was open); the 4999 after it
	// are inside the window, and all of them share one trailing timer.
	if n != 1 {
		t.Fatalf("%d activity-only changes in one window scheduled %d trailing timer(s), want exactly 1", changes, n)
	}

	_ = ws.Close(websocket.StatusNormalClosure, "bye")
	deadline = time.Now().Add(5 * time.Second)
	for len(h.Hosts(1)) == 0 || h.Hosts(1)[0].Online {
		if time.Now().After(deadline) {
			t.Fatal("the host never went offline")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Stop reports true only for a timer still pending: the disconnect must
	// already have stopped it (it cannot have fired: the window is an hour).
	if timers[0].Stop() {
		t.Fatal("the host disconnected and its trailing activity timer is still pending")
	}
}
