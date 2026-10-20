package mirror_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/mirror"
)

// A HERMES WINDOW on the server (docs/design/hermes.md, "Server changes"),
// driven by a host that speaks the wire's own bytes. The hello and window lists
// here are JSON, as a host sends them, not Go values: the fields that make a
// window Hermes are on the wire whatever this side's types call them.

// jsonHost is a host on the raw wire: it sends its hello as given, answers
// every paste "pasted" and records it, and sends further window lists on
// demand.
type jsonHost struct {
	ws     *websocket.Conn
	mu     sync.Mutex
	pastes []mirror.Msg
}

// connectJSON connects a host whose hello is the JSON object hello.
func connectJSON(t *testing.T, r *rig, token, hello string) *jsonHost {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+token)
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(r.srv.URL, "http")+mirror.ConnectPath, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	if err := ws.Write(ctx, websocket.MessageText, []byte(hello)); err != nil {
		t.Fatal(err)
	}
	if _, data, err := ws.Read(ctx); err != nil {
		t.Fatal(err)
	} else {
		var w mirror.Msg
		_ = json.Unmarshal(data, &w)
		if w.Error != "" {
			t.Fatalf("hello refused: %s", w.Error)
		}
	}
	h := &jsonHost{ws: ws}
	go func() {
		for {
			typ, data, err := ws.Read(context.Background())
			if err != nil {
				return
			}
			var m mirror.Msg
			if typ != websocket.MessageText || json.Unmarshal(data, &m) != nil || m.Type != mirror.TypePaste {
				continue
			}
			h.mu.Lock()
			h.pastes = append(h.pastes, m)
			h.mu.Unlock()
			b, _ := json.Marshal(mirror.Msg{Type: mirror.TypePasted, ID: m.ID})
			_ = ws.Write(context.Background(), websocket.MessageText, b)
		}
	}()
	return h
}

// pasted is every paste the host received, as "<window>:<text>".
func (h *jsonHost) pasted() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, m := range h.pastes {
		out = append(out, m.Window+":"+m.Text)
	}
	return out
}

// sendWindows sends a sessions message whose windows are the JSON array ws.
func (h *jsonHost) sendWindows(t *testing.T, ws string) {
	t.Helper()
	if err := h.ws.Write(context.Background(), websocket.MessageText, []byte(`{"type":"sessions","windows":`+ws+`}`)); err != nil {
		t.Fatal(err)
	}
}

// hermesWindows is one session's windows as JSON: @1 runs Hermes with
// hermesStatus (since t1), @2 runs Claude Code, idle since t2.
func hermesWindows(hermesStatus string) string {
	return `[{"session":"work","id":"@1","index":0,"active":true,"runtime":"hermes","status":"` + hermesStatus + `","statusSince":1792000001111},` +
		`{"session":"work","id":"@2","index":1,"claude":true,"claudeSession":"0b2c3d4e-5f60-4718-8a9b-0c1d2e3f4a5b","status":"idle","statusSince":1792000002222}]`
}

// hermesHello is an -interactive, pane-paste, status-reporting v2 host's hello
// with hermesWindows, plus extra fields (`"hermesSend":true`, or "").
func hermesHello(host, hermesStatus, extra string) string {
	return `{"type":"hello","version":2,"host":"` + host + `","interactive":true,"panePaste":true,"statusReport":true` + extra +
		`,"windows":` + hermesWindows(hermesStatus) + `}`
}

// TestAPasteIntoAHermesWindowIsRefusedUnlessItsHostAdvertisedHermesSend is
// the hermesSend switch's server half. A host that did not advertise
// hermesSend: a paste into its Hermes window — the Terminal view's
// PasteActivePane, which is the only target that reaches a non-Claude window —
// is refused with the paste guard's own Hermes refusal, and NOTHING reaches
// the host; the same host's Claude Code window is pasted into as before. A
// host that did advertise it: the same paste reaches it (the positive
// control: the refusal is the switch, not the window).
func TestAPasteIntoAHermesWindowIsRefusedUnlessItsHostAdvertisedHermesSend(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithTimeout(asUser(1), 3*time.Second)
	defer cancel()

	tok, _ := r.store.mint(t, 1, "off")
	off := connectJSON(t, r, tok, hermesHello("off", "idle", ""))
	r.online(t, 1, "off", "work")
	err := r.svc.PasteWindow(ctx, "off", "work", "@1", "into Hermes", mirror.PasteActivePane)
	if err == nil || mirror.PasteGuardRefusal(err) != mirror.PasteRefusalHermes {
		t.Fatalf("a paste into a Hermes window of a host without hermesSend = %v; want the Hermes refusal", err)
	}
	var pr *mirror.PasteRefusedError
	if !errors.As(err, &pr) || pr.Why != mirror.PasteRefusedHermesText {
		t.Fatalf("the refusal's words = %+v; want the paste guard's own", pr)
	}
	// The rendered view's target is refused too (no Claude Code there).
	if err := r.svc.PasteWindow(ctx, "off", "work", "@1", "into Hermes", mirror.PasteClaude); err == nil {
		t.Fatal("a PasteClaude into a Hermes window was not refused")
	}
	// CONTROL, same host: its Claude Code window is pasted into.
	if err := r.svc.PasteWindow(ctx, "off", "work", "@2", "into Claude", mirror.PasteActivePane); err != nil {
		t.Fatalf("a paste into the same host's Claude Code window: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if got := off.pasted(); len(got) != 1 || got[0] != "@2:into Claude" {
		t.Fatalf("the host without hermesSend received %q; want only the Claude Code window's paste", got)
	}

	// POSITIVE CONTROL: a host that advertised hermesSend is sent the paste.
	tok2, _ := r.store.mint(t, 1, "on")
	on := connectJSON(t, r, tok2, hermesHello("on", "idle", `,"hermesSend":true`))
	r.online(t, 1, "on", "work")
	if err := r.svc.PasteWindow(ctx, "on", "work", "@1", "into Hermes", mirror.PasteActivePane); err != nil {
		t.Fatalf("a paste into a Hermes window of a host WITH hermesSend: %v", err)
	}
	if got := on.pasted(); len(got) != 1 || got[0] != "@1:into Hermes" {
		t.Fatalf("the host with hermesSend received %q; want the Hermes window's paste", got)
	}
}

// TestAHermesWindowsStateIsItsOwnNotNotClaude: the agent state of a Hermes
// window is READ — its status and since, from a host that reports status — and
// is never refused as "no Claude Code here". A plain shell window still is. The
// control is the Claude Code window beside it, unchanged.
func TestAHermesWindowsStateIsItsOwnNotNotClaude(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "ws")
	connectJSON(t, r, tok, `{"type":"hello","version":2,"host":"ws","interactive":true,"statusReport":true,"windows":`+
		strings.TrimSuffix(hermesWindows("busy"), "]")+`,{"session":"work","id":"@3","index":2,"command":"zsh"}]}`)
	r.online(t, 1, "ws", "work")
	st := r.svc.AgentState(asUser(1), "ws", "work", "@1")
	if errors.Is(st.Unknown, mirror.ErrNotClaude) || st.Unknown != nil || st.Status != mirror.StatusBusy || st.Since != 1792000001111 {
		t.Fatalf("the Hermes window's state = %+v; want busy since the host's 1792000001111", st)
	}
	if st := r.svc.AgentState(asUser(1), "ws", "work", "@2"); st.Unknown != nil || st.Status != mirror.StatusIdle || st.Since != 1792000002222 {
		t.Fatalf("CONTROL: the Claude Code window's state = %+v", st)
	}
	if st := r.svc.AgentState(asUser(1), "ws", "work", "@3"); !errors.Is(st.Unknown, mirror.ErrNotClaude) {
		t.Fatalf("a shell window's state = %+v; want ErrNotClaude, as before", st)
	}
}

// TestAHermesWindowThatStartsWaitingTellsTheAttentionQueue: the hub's waiting
// set is keyed by runtime, not by w.Claude — a Hermes window whose status
// turns waiting fires the waiting hook, as a Claude Code window's does (the
// control), and one that turns busy does not.
func TestAHermesWindowThatStartsWaitingTellsTheAttentionQueue(t *testing.T) {
	r := newRig(t)
	var mu sync.Mutex
	fired := 0
	r.svc.SetWaitingNotify(func(int64) { mu.Lock(); fired++; mu.Unlock() })
	count := func() int { mu.Lock(); defer mu.Unlock(); return fired }
	tok, _ := r.store.mint(t, 1, "ws")
	h := connectJSON(t, r, tok, hermesHello("ws", "idle", ""))
	r.online(t, 1, "ws", "work")

	h.sendWindows(t, hermesWindows("busy"))
	time.Sleep(150 * time.Millisecond)
	if n := count(); n != 0 {
		t.Fatalf("a Hermes window turning busy fired the waiting hook %d times", n)
	}
	h.sendWindows(t, hermesWindows("waiting"))
	eventually(t, "the waiting hook for the Hermes window", func() bool { return count() == 1 })

	// CONTROL: the Claude Code window turning waiting fires it too.
	h.sendWindows(t, strings.Replace(hermesWindows("waiting"), `"status":"idle"`, `"status":"waiting"`, 1))
	eventually(t, "the waiting hook for the Claude Code window", func() bool { return count() == 2 })
}
