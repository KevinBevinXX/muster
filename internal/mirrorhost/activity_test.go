package mirrorhost

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/mirror"
)

func TestParseActivityIsTheNewestTimeOnAnyOfASessionsLines(t *testing.T) {
	// window_activity \t session_activity \t session_name, one line per window.
	got := parseActivity("" +
		"1791903623\t1791903619\tnoisy\n" + // the window is newer than the session
		"1791903619\t1791903619\tmulti\n" +
		"1791903628\t1791903619\tmulti\n" + // a second (background) window, newer
		"1791903600\t1791903777\ttyped\n" + // the session (a client's key) is newer
		"x\t1791903500\thalf\n" + // one unusable field: the other still counts
		"\t\tblank\n" + // no usable time: skipped
		"-5\tnope\tjunk\n" +
		"1791903650\t1791903650\tname with\ttabs\r\n" + // the name is the LAST field, whole
		"1791903651\t1791903651\n" + // too few fields
		"1791903652\t1791903652\t\n") // no name
	want := map[string]int64{"noisy": 1791903623, "multi": 1791903628, "typed": 1791903777, "half": 1791903500, "name with\ttabs": 1791903650}
	if !maps.Equal(got, want) {
		t.Fatalf("parseActivity = %v\n          want %v", got, want)
	}
}

// activeBackend is a Backend whose sessions and activity a test moves.
type activeBackend struct {
	mu       sync.Mutex
	sessions []string
	activity map[string]int64
	err      error
	asked    int
}

func (b *activeBackend) List(context.Context) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.sessions...), nil
}

func (b *activeBackend) Attach(context.Context, string, bool, uint16, uint16) (Terminal, error) {
	return nil, errors.New("no")
}

func (b *activeBackend) Activity(context.Context) (map[string]int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.asked++
	return maps.Clone(b.activity), b.err
}

func (b *activeBackend) set(session string, at int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.activity[session] = at
}

func TestSessionActivityFindsTheCapabilityThroughAWrapperAndFloorsIt(t *testing.T) {
	now := time.Unix(1_800_000_037, 0)
	b := &activeBackend{activity: map[string]int64{"scratch13": 1_800_000_011, "gone": 1_800_000_000}}
	got, err := SessionActivity(context.Background(), wrapped{b}, []string{"scratch13", "quiet"}, now)
	if err != nil || !maps.Equal(got, map[string]int64{"scratch13": 1_800_000_000}) {
		t.Fatalf("got %v, %v", got, err)
	}
	// Control: the bare type assertion the helper replaces does NOT see it.
	if _, ok := Backend(wrapped{b}).(ActivityReader); ok {
		t.Fatal("instrument check: the wrapper exposes Activity directly, so this test proves nothing about Unwrap")
	}
	// A backend that cannot tell sends none, and is not an error.
	if got, err := SessionActivity(context.Background(), countingWindows{}, []string{"a"}, now); got != nil || err != nil {
		t.Fatalf("a backend with no activity: %v, %v", got, err)
	}
	// A failed read sends none and reports why.
	b.err = errors.New("tmux is gone")
	if got, err := SessionActivity(context.Background(), b, []string{"scratch13"}, now); got != nil || err == nil {
		t.Fatalf("a failed read: %v, %v", got, err)
	}
}

// fakeMuster is the server side of the tunnel, hand-played: it accepts one
// host, welcomes it, and records every hello and sessions message it is sent —
// so a test can COUNT what a real client re-sends.
type fakeMuster struct {
	srv  *httptest.Server
	mu   sync.Mutex
	msgs []mirror.Msg
}

func newFakeMuster(t *testing.T) *fakeMuster {
	t.Helper()
	f := &fakeMuster{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		for {
			_, data, err := ws.Read(r.Context())
			if err != nil {
				return
			}
			var m mirror.Msg
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			f.mu.Lock()
			f.msgs = append(f.msgs, m)
			f.mu.Unlock()
			if m.Type == mirror.TypeHello {
				b, _ := json.Marshal(mirror.Msg{Type: mirror.TypeWelcome, Version: m.Version, Host: m.Host})
				if ws.Write(r.Context(), websocket.MessageText, b) != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeMuster) of(typ string) []mirror.Msg {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []mirror.Msg
	for _, m := range f.msgs {
		if m.Type == typ {
			out = append(out, m)
		}
	}
	return out
}

// TestABusySessionIsNotReSentEveryRefresh is the RE-SEND POLICY, counted on the
// wire from a real client: a session whose activity time moves on every read
// (a pane printing output) costs NO message while it stays in one
// mirror.ActivityBucket, and exactly ONE when it enters the next.
//
// Measured at three points: several moves inside one bucket (0 messages), the
// first read past the boundary (1), and several moves inside that next bucket
// (still 1). 🔴 The silent phases are not "the refresh never ran": the backend
// counts its reads, and each phase asserts the loop read it again several
// times while sending nothing.
func TestABusySessionIsNotReSentEveryRefresh(t *testing.T) {
	f := newFakeMuster(t)
	// A bucket boundary well in the past, so nothing here is capped at now.
	base := time.Now().Unix()/mirror.ActivityBucket*mirror.ActivityBucket - 20*mirror.ActivityBucket
	b := &activeBackend{sessions: []string{"busy", "idle"}, activity: map[string]int64{"busy": base + 3, "idle": base - 9*mirror.ActivityBucket + 21}}
	c, err := New(Config{URL: f.srv.URL, Token: mirror.TokenPrefix + "t", Host: "laptop", Backend: wrapped{b},
		AllowHTTP: true, Refresh: 10 * time.Millisecond, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = c.RunOnce(ctx) }()

	waitUntil(t, "the hello", func() bool { return len(f.of(mirror.TypeHello)) == 1 })
	if got, want := f.of(mirror.TypeHello)[0].Activity, (map[string]int64{"busy": base, "idle": base - 9*mirror.ActivityBucket}); !maps.Equal(got, want) {
		t.Fatalf("the hello's activity = %v, want it floored: %v", got, want)
	}
	reads := func() int {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.asked
	}
	// churn moves busy's raw time on every one of n refreshes.
	churn := func(from int64, n int) {
		start := reads()
		for i := 0; i < n; i++ {
			b.set("busy", from+int64(i))
			at := reads()
			waitUntil(t, "the refresh loop to read the backend again", func() bool { return reads() > at })
		}
		if reads()-start < n {
			t.Fatalf("instrument check FAILED: the refresh loop read the backend %d time(s) over %d moves", reads()-start, n)
		}
	}

	churn(base+4, 8) // base+4 … base+11: one bucket
	if n := len(f.of(mirror.TypeSessions)); n != 0 {
		t.Fatalf("8 activity moves inside one bucket re-sent the list %d time(s); want 0", n)
	}

	b.set("busy", base+mirror.ActivityBucket+1) // the next bucket
	waitUntil(t, "the one re-send for the new bucket", func() bool { return len(f.of(mirror.TypeSessions)) == 1 })
	if got := f.of(mirror.TypeSessions)[0].Activity["busy"]; got != base+mirror.ActivityBucket {
		t.Fatalf("the re-sent activity = %d, want the new bucket %d", got, base+mirror.ActivityBucket)
	}

	churn(base+mirror.ActivityBucket+2, 8)
	if n := len(f.of(mirror.TypeSessions)); n != 1 {
		t.Fatalf("after the bucket change, 8 more moves inside it brought the total to %d message(s); want 1", n)
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
