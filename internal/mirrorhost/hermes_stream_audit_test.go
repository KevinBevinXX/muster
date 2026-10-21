package mirrorhost

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/hermeshost/hermestest"
	"github.com/ZacxDev/muster/internal/mirror"
)

// startHermesView is a host with one Hermes window on root (a real state.db)
// and an open watch (id) of it whose backfill has arrived.
func startHermesView(t *testing.T, home *hermestest.Home, root string, id uint32, wantBackfill int) *drivenMuster {
	t.Helper()
	src := NewHermesSource(home.Dir, t.TempDir())
	t.Cleanup(func() { _ = src.State().Close() })
	d := newDrivenMuster(t)
	d.run(t, hermesBackend{&windowsBackend{windows: []WindowInfo{hermesWindowInfo(root)}}, src}, nil, true)
	d.send(t, mirror.Msg{Type: mirror.TypeWatch, ID: id, Session: "h", Window: "@5"})
	waitUntil(t, "the backfill", func() bool { evs, _, _ := watchEvents(d, id); return len(evs) >= wantBackfill })
	return d
}

// 🔴 A DISPLAY KIND STAMPED AFTER THE ROW WAS DRAWN (round-1 audit, #190
// 🟡3): Hermes writes an auto_continue or process_complete row, then stamps
// its display_kind. The view had already drawn it as the person's prompt; the
// host notices the stamp and redraws, so it ends as a notice — a turn the
// model wrote is never left as the person's bubble.
func TestAStampedRowIsRedrawnAsANotice(t *testing.T) {
	for _, kind := range []string{"auto_continue", "process_complete"} {
		home := hermestest.New(t)
		root := hermestest.LocalID()
		home.Session(root, hermestest.SessionOpts{})
		home.Message(root, hermestest.Msg{Role: "user", Content: "the real prompt"})
		d := startHermesView(t, home, root, 11, 1)
		row := home.Message(root, hermestest.Msg{Role: "user", Content: "STAMPED later"})
		waitUntil(t, "the unstamped row drawn", func() bool {
			evs, _, _ := watchEvents(d, 11)
			return strings.Contains(evText(evs), "prompt:STAMPED later")
		})
		home.Exec(`UPDATE messages SET display_kind = ? WHERE id = ?`, kind, row)
		waitUntil(t, kind+": the redraw", func() bool {
			evs, resets, _ := watchEvents(d, 11)
			return resets >= 2 && !strings.Contains(evText(evs), "prompt:STAMPED") && strings.Contains(evText(evs), "notice:")
		})
	}
}

// A secret written AFTER the backfill arrives redacted on the live tail
// (round-1 audit, #190 🟡5).
func TestASecretOnTheLiveTailArrivesRedacted(t *testing.T) {
	home := hermestest.New(t)
	root := hermestest.LocalID()
	home.Session(root, hermestest.SessionOpts{})
	home.Message(root, hermestest.Msg{Role: "user", Content: "start"})
	d := startHermesView(t, home, root, 12, 1)
	secret := "ghp_" + strings.Repeat("Z", 36)
	home.Message(root, hermestest.Msg{Role: "assistant", Content: "here is the token " + secret})
	home.Message(root, hermestest.Msg{Role: "tool", ToolCallID: "c1", Content: "export GH_TOKEN=" + secret})
	waitUntil(t, "the tailed rows", func() bool {
		evs, _, _ := watchEvents(d, 12)
		return strings.Contains(evText(evs), "here is the token") && strings.Contains(evText(evs), "tool_result:")
	})
	if evs, _, _ := watchEvents(d, 12); strings.Contains(evText(evs), secret) {
		t.Fatalf("the live tail carried the secret: %s", evText(evs))
	}
}

// slowHermesBackend lists its windows slowly once slow is set.
type slowHermesBackend struct {
	hermesBackend
	slow *atomic.Bool
}

func (b slowHermesBackend) Windows(ctx context.Context) ([]WindowInfo, error) {
	if b.slow.Load() {
		time.Sleep(2 * time.Second)
	}
	return b.hermesBackend.Windows(ctx)
}

// 🔴 THE READ LOOP DOES NO I/O FOR A WATCH (round-1 audit, #190 🟡2): which
// stream serves a watch (Hermes or Claude Code) is decided in the watch's own
// goroutine, so a slow window listing never holds up the next message on the
// connection — here a paste, which a read-only host answers at once.
func TestASlowListingNeverBlocksTheReadLoop(t *testing.T) {
	home := hermestest.New(t)
	root := hermestest.LocalID()
	home.Session(root, hermestest.SessionOpts{})
	src := NewHermesSource(home.Dir, t.TempDir())
	t.Cleanup(func() { _ = src.State().Close() })
	slow := &atomic.Bool{}
	b := slowHermesBackend{hermesBackend{&windowsBackend{windows: []WindowInfo{hermesWindowInfo(root)}}, src}, slow}
	d := newDrivenMuster(t)
	d.run(t, b, nil, false)
	slow.Store(true)
	d.send(t, mirror.Msg{Type: mirror.TypeWatch, ID: 21, Session: "h", Window: "@5"})
	start := time.Now()
	d.send(t, mirror.Msg{Type: mirror.TypePaste, ID: 22, Session: "h", Window: "@5", Text: "x"})
	waitUntil(t, "the paste's answer", func() bool {
		for _, m := range d.of(mirror.TypePasted) {
			if m.ID == 22 {
				return true
			}
		}
		return false
	})
	if el := time.Since(start); el > time.Second {
		t.Fatalf("the paste was answered after %v: the watch's listing held the read loop", el)
	}
	slow.Store(false)
}

// TestOneRenderedViewLoop (round-0 decision D3): the Claude Code and the
// Hermes streams share ONE loop (runView) — one ticker on ViewPoll in this
// package's code, not one per stream.
func TestOneRenderedViewLoop(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	n, scanned := 0, 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		scanned++
		n += strings.Count(string(b), "time.NewTicker(h.c.cfg.ViewPoll)")
	}
	if scanned < 10 || n != 1 {
		t.Fatalf("%d rendered-view tickers in %d files, want exactly 1 (runView)", n, scanned)
	}
}
