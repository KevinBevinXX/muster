package mirror_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
)

// THE RENDERED VIEW (tunnel v2), end to end through a REAL muster-mirror client,
// a REAL hub and a real transcript FILE the test appends to as Claude Code would.
// Only tmux and /proc are played: claudeBackend says which session runs Claude
// Code, in which pane, writing which file.

type claudeBackend struct {
	*fakeBackend
	mu     sync.Mutex
	panes  map[string]mirrorhost.ClaudePane
	pastes []string // "pane:text"
}

func newClaudeBackend(sessions ...string) *claudeBackend {
	b := &claudeBackend{fakeBackend: &fakeBackend{}, panes: map[string]mirrorhost.ClaudePane{}}
	b.set(sessions...)
	return b
}

func (b *claudeBackend) setPane(session string, p mirrorhost.ClaudePane) {
	b.mu.Lock()
	b.panes[session] = p
	b.mu.Unlock()
}

func (b *claudeBackend) ClaudePanes(_ context.Context, sessions []string) map[string]mirrorhost.ClaudePane {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]mirrorhost.ClaudePane{}
	for _, s := range sessions {
		if p, ok := b.panes[s]; ok {
			out[s] = p
		}
	}
	return out
}

func (b *claudeBackend) Paste(_ context.Context, p mirrorhost.ClaudePane, text string) error {
	b.mu.Lock()
	b.pastes = append(b.pastes, p.Pane+":"+text)
	b.mu.Unlock()
	return nil
}

func (b *claudeBackend) pasted() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.pastes...)
}

// transcript is a JSONL file the test writes as the CLI would.
type transcript struct {
	t    *testing.T
	path string
}

func newTranscript(t *testing.T, lines ...string) *transcript {
	t.Helper()
	tr := &transcript{t: t, path: filepath.Join(t.TempDir(), "11111111-aaaa.jsonl")}
	tr.write(strings.Join(lines, ""))
	return tr
}

func (tr *transcript) write(s string) {
	tr.t.Helper()
	f, err := os.OpenFile(tr.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		tr.t.Fatal(err)
	}
	if _, err := f.WriteString(s); err != nil {
		tr.t.Fatal(err)
	}
	_ = f.Close()
}

func promptLine(s string) string {
	b, _ := json.Marshal(map[string]any{"type": "user", "promptId": "p", "message": map[string]any{"content": s}})
	return string(b) + "\n"
}

func textLine(s string) string {
	b, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": s}}}})
	return string(b) + "\n"
}

func thinkingLine(s string) string {
	b, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "thinking", "thinking": s}}}})
	return string(b) + "\n"
}

func toolLine(id, name, input string) string {
	b, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
		map[string]any{"type": "tool_use", "id": id, "name": name, "input": json.RawMessage(input)}}}})
	return string(b) + "\n"
}

func resultLine(id, out string, isErr bool) string {
	b, _ := json.Marshal(map[string]any{"type": "user", "promptId": "p", "message": map[string]any{"content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": id, "content": out, "is_error": isErr}}}})
	return string(b) + "\n"
}

// watchRec collects a Watch's batches.
type watchRec struct {
	mu   sync.Mutex
	msgs []mirror.Msg
}

func (w *watchRec) add(m mirror.Msg) error {
	w.mu.Lock()
	w.msgs = append(w.msgs, m)
	w.mu.Unlock()
	return nil
}

func (w *watchRec) all() []mirror.Msg {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]mirror.Msg(nil), w.msgs...)
}

// events is every event since the last Reset, flattened.
func (w *watchRec) events() []cctranscript.Event {
	var out []cctranscript.Event
	for _, m := range w.all() {
		if m.Reset {
			out = nil
		}
		out = append(out, m.Events...)
	}
	return out
}

func (w *watchRec) has(pred func(cctranscript.Event) bool) bool {
	return slices.ContainsFunc(w.events(), pred)
}

func watchAs(t *testing.T, ctx context.Context, svc *mirror.Service, host, session, gen string, offset int64) (*watchRec, func(), <-chan error) {
	t.Helper()
	rec := &watchRec{}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- svc.Watch(ctx, host, session, gen, offset, rec.add) }()
	t.Cleanup(cancel)
	return rec, cancel, done
}

func TestTheRenderedViewStreamsTheTranscriptLive(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "ws")
	tr := newTranscript(t, promptLine("first question"), thinkingLine("PRIVATE REASONING"), textLine("first answer"))
	b := newClaudeBackend("main", "shell")
	b.setPane("main", mirrorhost.ClaudePane{Pane: "%3", PID: 42, Transcript: tr.path, Gen: "gen-a"})
	r.connect(t, "ws", tok, b, true)
	r.online(t, 1, "ws", "main")

	eventually(t, "the host reports main as a Claude pane", func() bool {
		h := r.svc.Hosts(asUser(1))
		return len(h) == 1 && slices.Equal(h[0].Claude, []string{"main"}) && h[0].Rendered
	})

	rec, _, _ := watchAs(t, asUser(1), r.svc, "ws", "main", "", 0)
	eventually(t, "the backfill", func() bool {
		return rec.has(func(e cctranscript.Event) bool { return e.Kind == cctranscript.KindText && e.Text == "first answer" })
	})
	first := rec.all()[0]
	if !first.Reset || first.Gen != "gen-a" {
		t.Fatalf("the first batch is %+v, want a Reset of gen-a", first)
	}

	// A turn runs: a tool call is written, then — partially first — its result.
	tr.write(promptLine("second question") + toolLine("tu1", "Bash", `{"command":"go test ./..."}`))
	eventually(t, "the live tool call", func() bool {
		return rec.has(func(e cctranscript.Event) bool {
			return e.Kind == cctranscript.KindToolCall && e.ToolID == "tu1" && e.Tool == "Bash" && e.Target == "go test ./..."
		})
	})
	res := resultLine("tu1", "FAIL boom", true)
	tr.write(res[:len(res)/2])
	time.Sleep(150 * time.Millisecond)
	if rec.has(func(e cctranscript.Event) bool { return e.Kind == cctranscript.KindToolResult }) {
		t.Fatal("a HALF-written result line produced an event")
	}
	tr.write(res[len(res)/2:])
	eventually(t, "the result once its line is complete", func() bool {
		return rec.has(func(e cctranscript.Event) bool {
			return e.Kind == cctranscript.KindToolResult && e.ToolID == "tu1" && !e.OK && e.Output == "FAIL boom"
		})
	})

	// Busy flips with Claude Code's own status.
	p := b.ClaudePanes(context.Background(), []string{"main"})["main"]
	p.Busy = true
	b.setPane("main", p)
	eventually(t, "busy", func() bool {
		all := rec.all()
		return all[len(all)-1].Busy
	})

	// Every event's offsets only grow; thinking never crossed the wire.
	var last int64
	for _, m := range rec.all() {
		if m.Offset < last {
			t.Fatalf("offset went back: %d after %d", m.Offset, last)
		}
		last = m.Offset
		for _, e := range m.Events {
			if strings.Contains(e.Text+e.Output+e.Input, "PRIVATE REASONING") {
				t.Fatal("thinking reached muster")
			}
		}
	}
	if st, _ := os.Stat(tr.path); last != st.Size() {
		t.Fatalf("final offset %d, want the file's end %d", last, st.Size())
	}
}

// A browser reconnecting with (gen, offset) gets only what came after it — no
// reset, no repeat; with a stale gen it gets a fresh Reset backfill.
func TestAWatchResumesFromItsOffset(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "ws")
	tr := newTranscript(t, promptLine("one"), textLine("two"))
	b := newClaudeBackend("main")
	b.setPane("main", mirrorhost.ClaudePane{Pane: "%1", Transcript: tr.path, Gen: "g1"})
	r.connect(t, "ws", tok, b, false)
	eventually(t, "claude listed", func() bool { h := r.svc.Hosts(asUser(1)); return len(h) == 1 && len(h[0].Claude) == 1 })

	st, _ := os.Stat(tr.path)
	tr.write(textLine("three"))
	rec, _, _ := watchAs(t, asUser(1), r.svc, "ws", "main", "g1", st.Size())
	eventually(t, "the event after the offset", func() bool { return len(rec.events()) == 1 })
	if all := rec.all(); all[0].Reset || rec.events()[0].Text != "three" {
		t.Fatalf("resume delivered %+v", all)
	}

	rec2, _, _ := watchAs(t, asUser(1), r.svc, "ws", "main", "an-old-gen", st.Size())
	eventually(t, "a fresh backfill", func() bool { return len(rec2.events()) == 3 })
	if !rec2.all()[0].Reset {
		t.Fatal("a stale gen was resumed instead of reset")
	}
}

// /clear (a new session id and file) restarts the view on the new transcript.
func TestANewClaudeSessionInThePaneResetsTheView(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "ws")
	old := newTranscript(t, promptLine("old session"))
	b := newClaudeBackend("main")
	b.setPane("main", mirrorhost.ClaudePane{Pane: "%1", Transcript: old.path, Gen: "old"})
	r.connect(t, "ws", tok, b, false)
	eventually(t, "claude listed", func() bool { h := r.svc.Hosts(asUser(1)); return len(h) == 1 && len(h[0].Claude) == 1 })
	rec, _, _ := watchAs(t, asUser(1), r.svc, "ws", "main", "", 0)
	eventually(t, "old backfill", func() bool { return len(rec.events()) == 1 })

	fresh := newTranscript(t, promptLine("new session"))
	b.setPane("main", mirrorhost.ClaudePane{Pane: "%1", Transcript: fresh.path, Gen: "new"})
	eventually(t, "the view reset to the new session", func() bool {
		ev := rec.events()
		return len(ev) == 1 && ev[0].Text == "new session"
	})
	all := rec.all()
	if last := all[len(all)-1]; last.Gen != "new" {
		t.Fatalf("gen %q, want new", last.Gen)
	}
}

// 🔴 OWNER ISOLATION: user 2 can neither watch nor paste into user 1's host —
// it answers exactly as a host that does not exist — and nothing reaches it.
func TestAnotherOwnerCannotWatchOrPaste(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "ws")
	tr := newTranscript(t, promptLine("secret prompt"))
	b := newClaudeBackend("main")
	b.setPane("main", mirrorhost.ClaudePane{Pane: "%1", Transcript: tr.path, Gen: "g"})
	r.connect(t, "ws", tok, b, true)
	eventually(t, "claude listed", func() bool { h := r.svc.Hosts(asUser(1)); return len(h) == 1 && len(h[0].Claude) == 1 })

	rec, _, done := watchAs(t, asUser(2), r.svc, "ws", "main", "", 0)
	if err := within(t, done, "user 2's watch"); !errors.Is(err, mirror.ErrHostOffline) {
		t.Fatalf("user 2's watch = %v, want ErrHostOffline", err)
	}
	if len(rec.all()) != 0 {
		t.Fatal("user 2 received user 1's transcript")
	}
	if err := r.svc.Paste(asUser(2), "ws", "main", "hi", mirror.PasteClaude); !errors.Is(err, mirror.ErrHostOffline) {
		t.Fatalf("user 2's paste = %v, want ErrHostOffline", err)
	}
	if err := r.svc.Paste(context.Background(), "ws", "main", "hi", mirror.PasteClaude); !errors.Is(err, mirror.ErrNoPrincipal) {
		t.Fatalf("no principal's paste = %v", err)
	}
	// The control: the owner's own watch and paste work.
	own, _, _ := watchAs(t, asUser(1), r.svc, "ws", "main", "", 0)
	eventually(t, "the owner's watch", func() bool { return len(own.events()) == 1 })
	if err := r.svc.Paste(asUser(1), "ws", "main", "hi", mirror.PasteClaude); err != nil {
		t.Fatalf("the owner's paste: %v", err)
	}
	if got := b.pasted(); !slices.Equal(got, []string{"%1:hi"}) {
		t.Fatalf("pasted %v, want only the owner's", got)
	}
}

func TestPasteIsGatedOnInteractiveClaudeAndCleanText(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "ro")
	tr := newTranscript(t, promptLine("x"))
	b := newClaudeBackend("main", "shell")
	b.setPane("main", mirrorhost.ClaudePane{Pane: "%7", Transcript: tr.path, Gen: "g"})
	r.connect(t, "ro", tok, b, false)
	eventually(t, "claude listed", func() bool { h := r.svc.Hosts(asUser(1)); return len(h) == 1 && len(h[0].Claude) == 1 })

	if err := r.svc.Paste(asUser(1), "ro", "main", "hi", mirror.PasteClaude); !errors.Is(err, mirror.ErrInteractiveRefused) {
		t.Fatalf("paste on a read-only host = %v, want ErrInteractiveRefused", err)
	}

	tok2, _ := r.store.mint(t, 1, "rw")
	b2 := newClaudeBackend("main", "shell")
	b2.setPane("main", mirrorhost.ClaudePane{Pane: "%8", Transcript: tr.path, Gen: "g"})
	r.connect(t, "rw", tok2, b2, true)
	eventually(t, "rw claude listed", func() bool { return len(r.svc.Hosts(asUser(1))) == 2 })
	eventually(t, "rw claude", func() bool {
		for _, h := range r.svc.Hosts(asUser(1)) {
			if h.Host == "rw" {
				return len(h.Claude) == 1
			}
		}
		return false
	})
	if err := r.svc.Paste(asUser(1), "rw", "shell", "ls", mirror.PasteClaude); !errors.Is(err, mirror.ErrNotClaude) {
		t.Fatalf("paste into a non-Claude session = %v, want ErrNotClaude", err)
	}
	if err := r.svc.Paste(asUser(1), "rw", "main", "evil\x1b[201~!rm -rf /", mirror.PasteClaude); !errors.Is(err, mirror.ErrBadPaste) {
		t.Fatalf("paste with ESC = %v, want ErrBadPaste", err)
	}
	// 🔴 A stale sign-in is refused before anything reaches the host; so is a
	// context that carries no sign-in time at all.
	if err := r.svc.Paste(asStaleUser(1), "rw", "main", "hi", mirror.PasteClaude); !errors.Is(err, mirror.ErrStepUpRequired) {
		t.Fatalf("paste on a stale sign-in = %v, want ErrStepUpRequired", err)
	}
	noTime := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: authz.ViaSession})
	if err := r.svc.Paste(noTime, "rw", "main", "hi", mirror.PasteClaude); !errors.Is(err, mirror.ErrStepUpRequired) {
		t.Fatalf("paste with no sign-in time = %v, want ErrStepUpRequired", err)
	}
	if len(b2.pasted()) != 0 {
		t.Fatal("a stale paste reached the host")
	}
	if err := r.svc.Paste(asUser(1), "rw", "main", "line one\r\nline two", mirror.PasteClaude); err != nil {
		t.Fatalf("a clean paste: %v", err)
	}
	if got := b2.pasted(); !slices.Equal(got, []string{"%8:line one\nline two"}) {
		t.Fatalf("pasted %q", got)
	}
	if len(b.pasted()) != 0 {
		t.Fatal("the read-only host's backend was pasted into")
	}
}

// A v1 host (an older muster-mirror) is ACCEPTED by this muster and spoken to as
// v1: welcomed with version 1, listed and attachable, with no rendered view —
// even if it sends a claude list, it is ignored.
func TestAV1HostIsAcceptedWithoutARenderedView(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "old")
	h1 := hello("old", true, "main")
	h1.Version = 1
	h1.Claude = []string{"main"}
	raw, err := r.rawConnect(t, tok, h1)
	if err != nil {
		t.Fatalf("a v1 host was refused: %v", err)
	}
	_ = raw
	r.online(t, 1, "old", "main")
	h := r.svc.Hosts(asUser(1))
	if len(h) != 1 || h[0].Rendered || len(h[0].Claude) != 0 {
		t.Fatalf("v1 host view %+v, want no rendered view", h)
	}
	_, _, done := watchAs(t, asUser(1), r.svc, "old", "main", "", 0)
	if err := within(t, done, "watch on v1"); !errors.Is(err, mirror.ErrNoRenderedView) {
		t.Fatalf("watch on a v1 host = %v, want ErrNoRenderedView", err)
	}
	// Its terminal still attaches.
	_, _, _, _, res := attach(t, asUser(1), r.svc, "old", "main", true)
	eventually(t, "the v1 host is asked to open", func() bool { _, ok := raw.opened(); return ok })
	select {
	case err := <-res:
		t.Fatalf("the v1 attach ended early: %v", err)
	default:
	}
}

func TestTheWelcomeNamesTheHostsOwnVersionAndOutOfRangeIsRefused(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "h")
	for _, v := range []int{0, mirror.ProtocolVersion + 1} {
		m := hello("h", false, "main")
		m.Version = v
		if _, err := r.rawConnect(t, tok, m); err == nil || !strings.Contains(err.Error(), "tunnel versions") {
			t.Fatalf("version %d: err = %v, want the version refusal", v, err)
		}
	}
	for _, v := range []int{1, 2} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		hdr := http.Header{}
		hdr.Set("Authorization", "Bearer "+tok)
		ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(r.srv.URL, "http")+mirror.ConnectPath, &websocket.DialOptions{HTTPHeader: hdr})
		if err != nil {
			t.Fatal(err)
		}
		m := hello("h", false, "main")
		m.Version = v
		b, _ := json.Marshal(m)
		_ = ws.Write(ctx, websocket.MessageText, b)
		_, data, err := ws.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var w mirror.Msg
		_ = json.Unmarshal(data, &w)
		if w.Error != "" || w.Version != v {
			t.Fatalf("hello v%d welcomed %+v, want version %d", v, w, v)
		}
		ws.CloseNow()
		cancel()
	}
}

// A v2 host against a v1 muster: refused with the v1 version error, then
// reconnected AT ONCE as v1 — and served as v1 (no claude list sent).
func TestAV2HostFallsBackToV1AgainstAnOldMuster(t *testing.T) {
	var mu sync.Mutex
	var hellos []mirror.Msg
	welcomed := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		ctx := r.Context()
		_, data, err := ws.Read(ctx)
		if err != nil {
			return
		}
		var m mirror.Msg
		_ = json.Unmarshal(data, &m)
		mu.Lock()
		hellos = append(hellos, m)
		mu.Unlock()
		write := func(m mirror.Msg) { b, _ := json.Marshal(m); _ = ws.Write(ctx, websocket.MessageText, b) }
		// Exactly what a v1 muster (0.21.0) does: its readHello's error text.
		if m.Version != 1 {
			write(mirror.Msg{Type: mirror.TypeWelcome, Error: "mirror: this muster speaks tunnel version 1 and the host sent 2; upgrade the older side"})
			_ = ws.Close(websocket.StatusPolicyViolation, "version")
			return
		}
		write(mirror.Msg{Type: mirror.TypeWelcome, Version: 1, Host: m.Host})
		welcomed <- struct{}{}
		<-ctx.Done()
	}))
	t.Cleanup(srv.Close)
	b := newClaudeBackend("main")
	b.setPane("main", mirrorhost.ClaudePane{Pane: "%1", Transcript: "/nonexistent", Gen: "g"})
	c, err := mirrorhost.New(mirrorhost.Config{URL: srv.URL, Token: mirror.TokenPrefix + "x", Host: "laptop",
		Backend: b, AllowHTTP: true, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = c.RunOnce(ctx) }()
	select {
	case <-welcomed:
	case <-time.After(5 * time.Second):
		t.Fatal("the v2 host never connected to the v1 muster")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hellos) != 2 || hellos[0].Version != 2 || hellos[1].Version != 1 {
		t.Fatalf("hellos %+v, want v2 then v1", hellos)
	}
	if len(hellos[0].Claude) != 1 || len(hellos[1].Claude) != 0 {
		t.Fatalf("claude lists %v / %v: v2 hello carries it, v1 must not", hellos[0].Claude, hellos[1].Claude)
	}
}

// A browser that stops reading is cut off with ErrViewOverrun once streamBuffer
// batches are queued — the host's read loop never blocks on it.
func TestAWatchThatFallsBehindIsEndedNotStalled(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "h")
	m := hello("h", false, "main", "other")
	m.Claude = []string{"main"}
	raw, err := r.rawConnect(t, tok, m)
	if err != nil {
		t.Fatal(err)
	}
	r.online(t, 1, "h", "main")
	block := make(chan struct{})
	ctx, cancel := context.WithCancel(asUser(1))
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		done <- r.svc.Watch(ctx, "h", "main", "", 0, func(mirror.Msg) error { <-block; return nil })
	}()
	var id uint32
	eventually(t, "the watch reaches the host", func() bool {
		raw.mu.Lock()
		defer raw.mu.Unlock()
		for _, m := range raw.text {
			if m.Type == mirror.TypeWatch {
				id = m.ID
				return true
			}
		}
		return false
	})
	for i := 0; i < 400; i++ {
		b, _ := json.Marshal(mirror.Msg{Type: mirror.TypeEvents, ID: id, Events: []cctranscript.Event{{Kind: "text", Text: "x"}}})
		if err := raw.ws.Write(context.Background(), websocket.MessageText, b); err != nil {
			t.Fatal(err)
		}
	}
	// The hub reads one connection in order, so once it has applied this list
	// it has delivered every batch above — only then may the browser catch up
	// (released any sooner it could keep pace, and nothing would overrun).
	m.Type, m.Sessions = mirror.TypeSessions, []string{"main", "marker", "other"}
	b, _ := json.Marshal(m)
	if err := raw.ws.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
	r.online(t, 1, "h", "marker")
	close(block) // the owed batches drain, then the overrun
	if err := within(t, done, "the overrun"); !errors.Is(err, mirror.ErrViewOverrun) {
		t.Fatalf("watch = %v, want ErrViewOverrun", err)
	}
	// The connection is still serving: the other session is still online.
	if !r.svc.Hub().Online(1, "h", "other") {
		t.Fatal("an overrun watch took the host down")
	}
	if _, _, done := watchAs(t, asUser(1), r.svc, "h", "other", "", 0); !errors.Is(within(t, done, "non-claude watch"), mirror.ErrNotClaude) {
		t.Fatal("a session the host did not mark as Claude was watchable")
	}
}

func TestAHostHasAtMostMaxWatchesPerHostViews(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "h")
	m := hello("h", false, "main")
	m.Claude = []string{"main"}
	raw, err := r.rawConnect(t, tok, m)
	if err != nil {
		t.Fatal(err)
	}
	r.online(t, 1, "h", "main")
	for i := 0; i < mirror.MaxWatchesPerHost; i++ {
		watchAs(t, asUser(1), r.svc, "h", "main", "", 0)
	}
	eventually(t, "the host is asked for every view", func() bool {
		raw.mu.Lock()
		defer raw.mu.Unlock()
		n := 0
		for _, m := range raw.text {
			if m.Type == mirror.TypeWatch {
				n++
			}
		}
		return n == mirror.MaxWatchesPerHost
	})
	_, _, done := watchAs(t, asUser(1), r.svc, "h", "main", "", 0)
	if err := within(t, done, "the view past the cap"); !errors.Is(err, mirror.ErrTooManyWatches) {
		t.Fatalf("watch #%d = %v, want ErrTooManyWatches", mirror.MaxWatchesPerHost+1, err)
	}
}
