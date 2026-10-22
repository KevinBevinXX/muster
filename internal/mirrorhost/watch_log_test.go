package mirrorhost

import (
	"bytes"
	"context"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/hermeshost/hermestest"
	"github.com/ZacxDev/muster/internal/mirror"
)

// lockedBuf is a log sink the host's goroutines write while the test reads.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestAWatchLogsItsOpenAndItsEnd pins the host's log of every transcript
// stream — the browser suite counts these lines to prove a view switch opened
// no second stream (e2e/tests/view-swap.spec.ts), so their spelling is a
// contract: a `watching` line per open, saying whether it was asked for the
// transcript from the start or to resume at an offset, and an `unwatched` line
// per end.
func TestAWatchLogsItsOpenAndItsEnd(t *testing.T) {
	home := hermestest.New(t)
	root := hermestest.LocalID()
	home.Session(root, hermestest.SessionOpts{})
	home.Message(root, hermestest.Msg{Role: "user", Content: "hello"})
	src := NewHermesSource(home.Dir, t.TempDir())
	t.Cleanup(func() { _ = src.State().Close() })
	wb := &windowsBackend{windows: []WindowInfo{hermesWindowInfo(root)}}
	d := newDrivenMuster(t)
	var out lockedBuf
	c, err := New(Config{URL: d.srv.URL, Token: mirror.TokenPrefix + "t", Host: "laptop", Backend: hermesBackend{wb, src},
		AllowHTTP: true, Refresh: 10 * time.Millisecond, Logger: log.New(&out, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = c.RunOnce(ctx) }()
	waitUntil(t, "the hello", func() bool { return len(d.of(mirror.TypeHello)) == 1 })

	has := func(line string) func() bool {
		return func() bool { return strings.Contains(out.String(), line+"\n") }
	}
	d.send(t, mirror.Msg{Type: mirror.TypeWatch, ID: 3, Session: "h", Window: "@5"})
	waitUntil(t, "the open of a watch from the start", has(`muster-mirror: watching "h" window @5 (from the start, stream 3)`))
	d.send(t, mirror.Msg{Type: mirror.TypeWatch, ID: 4, Session: "h", Window: "@5", Gen: root, Offset: 7})
	waitUntil(t, "the open of a resumed watch", has(`muster-mirror: watching "h" window @5 (resuming at 7, stream 4)`))
	if strings.Contains(out.String(), "unwatched") {
		t.Fatalf("a watch was logged as ended while open:\n%s", out.String())
	}
	d.send(t, mirror.Msg{Type: mirror.TypeClose, ID: 3})
	waitUntil(t, "the end of watch 3", has(`muster-mirror: unwatched "h" window @5 (stream 3)`))
	if strings.Contains(out.String(), `unwatched "h" window @5 (stream 4)`) {
		t.Fatalf("watch 4 was logged as ended while open:\n%s", out.String())
	}
	// A gen with offset 0 is the start of that conversation: from the start.
	d.send(t, mirror.Msg{Type: mirror.TypeWatch, ID: 5, Session: "h", Window: "@5", Gen: root})
	waitUntil(t, "the open of a gen at offset 0", has(`muster-mirror: watching "h" window @5 (from the start, stream 5)`))
}
