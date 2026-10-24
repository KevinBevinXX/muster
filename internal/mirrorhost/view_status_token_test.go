package mirrorhost

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// appendLine appends one transcript record.
func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}

// statusWindows is windowsBackend whose Claude windows report a status.
type statusWindows struct{ *windowsBackend }

func (statusWindows) ReportsStatus() bool { return true }

// statusClaudeWindow is one Claude window writing transcript, at status (st, since).
func statusClaudeWindow(transcript, st string, since int64) WindowInfo {
	return WindowInfo{Session: "work", ID: "@1", Index: 1, Active: true, Pane: "%1", PanePID: 1, Command: "claude",
		Claude: &ClaudePane{Pane: "%1", PID: 1, Transcript: transcript, Gen: "g1", Busy: st == mirror.StatusBusy, Status: st, StatusSince: since}}
}

// TestAViewStreamNamesItsStatusTimeAndNudgesTheList: a Claude Code view's
// stream carries the time of the status its busy flag was read from
// (Msg.StatusSince) — on the backfill, on the prompt index and on every
// marker — and a status whose TIME moved while busy stayed the same (a turn
// that started and ended between two re-resolves) is a marker of its own.
// Each such marker is preceded by a nudge, so the window list naming the same
// time reaches muster now, not at the next Refresh (an hour here).
//
// Suggested replies read that time as the turn's own status token
// (internal/api's suggestTracker): without it, a turn end could only be told
// from the idle BEFORE the turn by guessing.
func TestAViewStreamNamesItsStatusTimeAndNudgesTheList(t *testing.T) {
	const s0, s1, s2 = int64(1_792_000_000_100), int64(1_792_000_000_200), int64(1_792_000_000_300)
	tr := filepath.Join(t.TempDir(), "g1.jsonl")
	if err := os.WriteFile(tr, []byte(`{"type":"user","promptId":"p","origin":{"kind":"human"},"message":{"role":"user","content":"go"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := &windowsBackend{windows: []WindowInfo{statusClaudeWindow(tr, mirror.StatusIdle, s0)}}
	d := newDrivenMuster(t)
	c, err := New(Config{URL: d.srv.URL, Token: mirror.TokenPrefix + "t", Host: "laptop", Backend: statusWindows{b},
		AllowHTTP: true, Refresh: time.Hour, ViewPoll: 5 * time.Millisecond, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = c.RunOnce(ctx) }()
	waitUntil(t, "the hello", func() bool { return len(d.of(mirror.TypeHello)) == 1 })
	d.send(t, mirror.Msg{Type: mirror.TypeWatch, ID: 3, Session: "work", Window: "@1"})

	events := func() []mirror.Msg { return d.of(mirror.TypeEvents) }
	waitUntil(t, "the prompt index", func() bool {
		for _, m := range events() {
			if m.PromptIndex {
				return true
			}
		}
		return false
	})
	for _, m := range events() {
		if m.StatusSince != s0 {
			t.Fatalf("a backfill message without the status time: %+v", m)
		}
	}
	listed := func(since int64) func() bool {
		return func() bool {
			for _, m := range d.of(mirror.TypeSessions) {
				for _, w := range m.Windows {
					if w.ID == "@1" && w.StatusSince == since {
						return true
					}
				}
			}
			return false
		}
	}
	marker := func(busy bool, since int64) func() bool {
		return func() bool {
			for _, m := range events() {
				if !m.PromptIndex && len(m.Events) == 0 && m.Busy == busy && m.StatusSince == since {
					return true
				}
			}
			return false
		}
	}

	// A short turn the re-resolve never saw busy: idle(s0) → idle(s1).
	b.set(statusClaudeWindow(tr, mirror.StatusIdle, s1))
	waitUntil(t, "an idle marker naming the new time", marker(false, s1))
	waitUntil(t, "the nudged window list naming it (Refresh is an hour)", listed(s1))

	// The reply written, THEN the idle status — read on the same re-resolve:
	// the reply's lines reach muster before the marker that ends its turn.
	const s1b = s1 + 50
	b.set(statusClaudeWindow(tr, mirror.StatusBusy, s1+10))
	waitUntil(t, "busy", marker(true, s1+10))
	for i := 0; i < 20; i++ { // many rounds: the tick it lands on must not matter
		// Each round lands at another phase of the poll / re-resolve cadence
		// (the busy marker below was sent ON a re-resolve tick; re-resolves are
		// every reresolveEvery ViewPolls).
		time.Sleep(time.Duration(i%reresolveEvery)*5*time.Millisecond + 2*time.Millisecond)
		appendLine(t, tr, `{"type":"assistant","message":{"content":[{"type":"text","text":"reply `+strconv.Itoa(i)+`"}]}}`)
		b.set(statusClaudeWindow(tr, mirror.StatusIdle, s1b+int64(i)*2))
		waitUntil(t, "the idle marker", marker(false, s1b+int64(i)*2))
		replyAt, markerAt := -1, -1
		for k, m := range events() {
			if len(m.Events) > 0 && m.Events[len(m.Events)-1].Text == "reply "+strconv.Itoa(i) {
				replyAt = k
			}
			if len(m.Events) == 0 && !m.PromptIndex && !m.Busy && m.StatusSince == s1b+int64(i)*2 {
				markerAt = k
			}
		}
		if replyAt < 0 || replyAt > markerAt {
			t.Fatalf("round %d: the reply (message %d) did not precede its turn end (message %d)", i, replyAt, markerAt)
		}
		b.set(statusClaudeWindow(tr, mirror.StatusBusy, s1b+int64(i)*2+1))
		waitUntil(t, "busy again", marker(true, s1b+int64(i)*2+1))
	}

	// busy(s2): the busy marker names its time too — and the list is NOT
	// nudged (only toward idle; Refresh is an hour, so it stays unsent).
	b.set(statusClaudeWindow(tr, mirror.StatusBusy, s2))
	waitUntil(t, "a busy marker naming its time", marker(true, s2))
	// WAITING (a permission prompt) is not nudged either: muster must keep
	// learning of a prompt from the host's own refusal (#255).
	const s3 = s2 + 7
	b.set(statusClaudeWindow(tr, mirror.StatusWaiting, s3))
	waitUntil(t, "a waiting marker naming its time", marker(false, s3))
	time.Sleep(100 * time.Millisecond)
	if listed(s2)() || listed(s3)() {
		t.Fatal("the list was nudged toward busy or waiting")
	}

	// A live batch (the turn's reply) carries the time as well.
	f, err := os.OpenFile(tr, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"type":"assistant","message":{"content":[{"type":"text","text":"working on it"}]}}` + "\n")
	_ = f.Close()
	waitUntil(t, "the live reply naming the status time", func() bool {
		for _, m := range events() {
			if len(m.Events) > 0 && m.Events[0].Text == "working on it" {
				return !m.Busy && m.StatusSince == s3
			}
		}
		return false
	})
}
