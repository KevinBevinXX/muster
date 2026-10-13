package mirror_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
	"github.com/ZacxDev/muster/internal/termsize"
)

// driveV2 runs a REAL muster-mirror (interactive or not) against a HAND-DRIVEN
// muster that sends one v2 request and returns the host's first reply to it —
// so each host-side refusal is tested with muster misbehaving.
func driveV2(t *testing.T, interactive bool, b mirrorhost.Backend, req mirror.Msg) mirror.Msg {
	t.Helper()
	got := make(chan mirror.Msg, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
		defer cancel()
		if _, _, err := ws.Read(ctx); err != nil {
			return
		}
		write := func(m mirror.Msg) { bb, _ := json.Marshal(m); _ = ws.Write(ctx, websocket.MessageText, bb) }
		write(mirror.Msg{Type: mirror.TypeWelcome, Version: 2, Host: "laptop"})
		req.ID = 9
		write(req)
		for {
			typ, data, err := ws.Read(ctx)
			if err != nil {
				return
			}
			var m mirror.Msg
			if typ == websocket.MessageText && json.Unmarshal(data, &m) == nil && m.ID == 9 {
				got <- m
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	c, err := mirrorhost.New(mirrorhost.Config{URL: srv.URL, Token: mirror.TokenPrefix + "x", Host: "laptop",
		Backend: b, Interactive: interactive, AllowHTTP: true, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = c.RunOnce(ctx) }()
	select {
	case m := <-got:
		return m
	case <-time.After(6 * time.Second):
		t.Fatal("the host never answered")
		return mirror.Msg{}
	}
}

// 🔴 The HOST refuses a paste it did not opt into, whatever muster asks; and a
// paste into a listed Claude session on an interactive host goes to the CLI's
// PANE id with the text sanitised again.
func TestTheHostRefusesAPasteItDidNotAdvertise(t *testing.T) {
	tr := newTranscript(t, promptLine("x"))
	b := newClaudeBackend("main", "shell")
	b.setPane("main", mirrorhost.ClaudePane{Pane: "%5", Transcript: tr.path, Gen: "g"})

	m := driveV2(t, false, b, mirror.Msg{Type: mirror.TypePaste, Session: "main", Text: "hi"})
	if m.Type != mirror.TypePasted || !strings.Contains(m.Error, "read-only") || len(b.pasted()) != 0 {
		t.Fatalf("a read-only host answered %+v and pasted %v", m, b.pasted())
	}
	m = driveV2(t, true, b, mirror.Msg{Type: mirror.TypePaste, Session: "shell", Text: "ls"})
	if !strings.Contains(m.Error, "not running") || len(b.pasted()) != 0 {
		t.Fatalf("a paste into a non-Claude session: %+v, pasted %v", m, b.pasted())
	}
	m = driveV2(t, true, b, mirror.Msg{Type: mirror.TypePaste, Session: "main", Text: "a\x1b[201~b"})
	if !strings.Contains(m.Error, "control character") || len(b.pasted()) != 0 {
		t.Fatalf("an ESC paste: %+v, pasted %v", m, b.pasted())
	}
	m = driveV2(t, true, b, mirror.Msg{Type: mirror.TypePaste, Session: "secret", Text: "x"})
	if !strings.Contains(m.Error, "not in this host's latest list") {
		t.Fatalf("an unlisted paste: %+v", m)
	}
	// The control: an interactive host pastes a clean message into main's pane.
	m = driveV2(t, true, b, mirror.Msg{Type: mirror.TypePaste, Session: "main", Text: "hello"})
	if m.Error != "" || !slices.Equal(b.pasted(), []string{"%5:hello"}) {
		t.Fatalf("control paste: %+v, pasted %v", m, b.pasted())
	}
}

func TestTheHostRefusesAWatchOfAnUnlistedOrNonClaudeSession(t *testing.T) {
	tr := newTranscript(t, promptLine("x"))
	b := newClaudeBackend("main", "shell")
	b.setPane("main", mirrorhost.ClaudePane{Pane: "%5", Transcript: tr.path, Gen: "g"})
	if m := driveV2(t, false, b, mirror.Msg{Type: mirror.TypeWatch, Session: "secret"}); m.Type != mirror.TypeClosed || !strings.Contains(m.Error, "not in this host's latest list") {
		t.Fatalf("unlisted watch: %+v", m)
	}
	if m := driveV2(t, false, b, mirror.Msg{Type: mirror.TypeWatch, Session: "shell"}); m.Type != mirror.TypeClosed || !strings.Contains(m.Error, "not running") {
		t.Fatalf("non-Claude watch: %+v", m)
	}
	// The control: main streams.
	if m := driveV2(t, false, b, mirror.Msg{Type: mirror.TypeWatch, Session: "main"}); m.Type != mirror.TypeEvents || !m.Reset || len(m.Events) != 1 {
		t.Fatalf("control watch: %+v", m)
	}
}

func TestBatchLinesBoundsEveryMessageAndNamesExactResumePoints(t *testing.T) {
	big := strings.Repeat("<", cctranscript.MaxText-10) // escapes six-fold in JSON
	var lines []cctranscript.Line
	off := int64(0)
	for i := 0; i < 40; i++ {
		off += 100
		lines = append(lines, cctranscript.Line{Events: []cctranscript.Event{{Kind: "text", Text: big}}, End: off})
	}
	// One line with many small events, to force a mid-line split by count.
	var many []cctranscript.Event
	for i := 0; i < mirror.MaxBatchEvents*2; i++ {
		many = append(many, cctranscript.Event{Kind: "tool_result", ToolID: fmt.Sprint(i)})
	}
	off += 100
	lines = append(lines, cctranscript.Line{Events: many, End: off})

	msgs := mirror.BatchLines(lines, 0)
	total := 0
	for i, m := range msgs {
		b, _ := json.Marshal(m)
		if len(b) > mirror.MaxFrame || len(m.Events) > mirror.MaxBatchEvents {
			t.Fatalf("message %d is %d bytes / %d events", i, len(b), len(m.Events))
		}
		if m.Offset%100 != 0 || m.Offset > off {
			t.Fatalf("message %d resumes at %d, not a line end", i, m.Offset)
		}
		total += len(m.Events)
	}
	if total != 40+len(many) {
		t.Fatalf("%d events framed, want %d", total, 40+len(many))
	}
	if last := msgs[len(msgs)-1]; last.Offset != off {
		t.Fatalf("the last message resumes at %d, want %d", last.Offset, off)
	}
	// A message cut inside the many-event line resumes at that line's START.
	cut := 0
	for _, m := range msgs[:len(msgs)-1] {
		if len(m.Events) > 0 && m.Events[len(m.Events)-1].Kind == "tool_result" {
			cut++
			if m.Offset != off-100 {
				t.Fatalf("a mid-line cut resumes at %d, want the line's start %d", m.Offset, off-100)
			}
		}
	}
	if cut == 0 {
		t.Fatal("positive control: the fixture never cut a line")
	}
}

func TestSanitizePaste(t *testing.T) {
	if got, err := mirror.SanitizePaste("a\r\nb\rc\td"); err != nil || got != "a\nb\nc\td" {
		t.Fatalf("got %q %v", got, err)
	}
	for _, bad := range []string{"", "  \n", "x\x1b[201~y", "x\x00", "x\u0085", strings.Repeat("x", mirror.MaxPaste+1), "\xff"} {
		if _, err := mirror.SanitizePaste(bad); err == nil {
			t.Errorf("SanitizePaste(%q) accepted", bad)
		}
	}
}

// sizedClaude is a Claude backend that can also report a window size, so
// mirrorhost.NoShrink WRAPS it — the composition cmd/muster-mirror ships.
type sizedClaude struct{ *claudeBackend }

func (sizedClaude) WindowSize(context.Context, string) (termsize.Size, error) {
	return termsize.Size{Cols: 120, Rows: 40}, nil
}

// 🔴 THE SEAM BETWEEN TWO FEATURES: muster-mirror wraps its backend in NoShrink
// (terminal sizing), and the rendered view asks the backend whether it can see
// Claude Code. A wrapper that embeds the Backend INTERFACE hides every optional
// capability of what it wraps — each feature passed its own tests while the
// shipped composition reported no Claude pane at all. This runs the composition.
func TestTheRenderedViewSurvivesTheNoShrinkWrapper(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "ws")
	tr := newTranscript(t, promptLine("through the wrapper"))
	b := newClaudeBackend("main")
	b.setPane("main", mirrorhost.ClaudePane{Pane: "%2", Transcript: tr.path, Gen: "g"})
	wrapped := mirrorhost.NoShrink(sizedClaude{b})
	// Positive control: the wrapper really is in the way.
	if _, direct := wrapped.(mirrorhost.ClaudeBackend); direct {
		t.Fatal("NoShrink did not hide the capability; this test would prove nothing")
	}
	r.connect(t, "ws", tok, wrapped, true)
	r.online(t, 1, "ws", "main")
	eventually(t, "the wrapped host still reports main as a Claude pane", func() bool {
		h := r.svc.Hosts(asUser(1))
		return len(h) == 1 && slices.Equal(h[0].Claude, []string{"main"})
	})
	rec, _, _ := watchAs(t, asUser(1), r.svc, "ws", "main", "", 0)
	eventually(t, "the transcript through the wrapper", func() bool { return len(rec.events()) == 1 })
	if err := r.svc.Paste(asUser(1), "ws", "main", "hi", mirror.PasteClaude); err != nil || !slices.Equal(b.pasted(), []string{"%2:hi"}) {
		t.Fatalf("paste through the wrapper: %v, pasted %v", err, b.pasted())
	}
}
