package mirrorhost

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/hermeshost/hermestest"
	"github.com/ZacxDev/muster/internal/mirror"
)

// hermesBackend is a per-window fake whose one window runs Hermes on the
// session root, with a real state.db behind it (hermestest).
type hermesBackend struct {
	*windowsBackend
	src *HermesSource
}

func (b hermesBackend) HermesSource() *HermesSource { return b.src }

func hermesWindowInfo(gen string) WindowInfo {
	return WindowInfo{Session: "h", ID: "@5", Index: 0, Active: true, Pane: "%5", PanePID: 9, Command: "python3.14",
		Agent: &AgentPane{Runtime: mirror.RuntimeHermes, Pane: "%5", PID: 10, Gen: gen, Status: mirror.StatusIdle, StatusSince: 1}}
}

// events is every event of watch id's batches so far, and whether one was a
// Reset; resets counts them.
func watchEvents(d *drivenMuster, id uint32) (evs []cctranscript.Event, resets int, last mirror.Msg) {
	for _, m := range d.of(mirror.TypeEvents) {
		if m.ID != id {
			continue
		}
		if m.Reset {
			resets++
			evs = nil
		}
		evs = append(evs, m.Events...)
		last = m
	}
	return evs, resets, last
}

func evText(evs []cctranscript.Event) string {
	var b strings.Builder
	for _, e := range evs {
		b.WriteString(e.Kind + ":" + e.Text + e.Output + e.Tool + "|")
	}
	return b.String()
}

// TestAHermesWindowStreamsItsConversation is the rendered view's host half for
// Hermes over the real wire: a watch of the window gets a Reset backfill of
// the conversation (redacted), the prompt index, then appended rows as they
// are written; an in-place rewrite starts the view over; a window whose
// session the host no longer names closes the view.
func TestAHermesWindowStreamsItsConversation(t *testing.T) {
	home := hermestest.New(t)
	root := hermestest.LocalID()
	home.Session(root, hermestest.SessionOpts{})
	secret := "ghp_" + strings.Repeat("A", 36)
	home.Message(root, hermestest.Msg{Role: "user", Content: "deploy with " + secret})
	reply := home.Message(root, hermestest.Msg{Role: "assistant", Content: "On it.", ToolCalls: `[{"id":"call_1","type":"function","function":{"name":"terminal","arguments":"{\"command\":\"make deploy\"}"}}]`})
	src := NewHermesSource(home.Dir, t.TempDir())
	t.Cleanup(func() { _ = src.State().Close() })
	wb := &windowsBackend{windows: []WindowInfo{hermesWindowInfo(root)}}
	d := newDrivenMuster(t)
	d.run(t, NoShrink(hermesBackend{wb, src}), nil, true)

	d.send(t, mirror.Msg{Type: mirror.TypeWatch, ID: 3, Session: "h", Window: "@5"})
	waitUntil(t, "the backfill", func() bool { evs, _, _ := watchEvents(d, 3); return len(evs) >= 3 })
	evs, resets, last := watchEvents(d, 3)
	if resets != 1 || last.Gen != root {
		t.Fatalf("resets %d gen %q", resets, last.Gen)
	}
	got := evText(evs)
	if strings.Contains(got, secret) || !strings.Contains(got, "prompt:deploy with") {
		t.Fatalf("the backfill is not the redacted conversation: %s", got)
	}
	if !strings.Contains(got, "text:On it.") || !strings.Contains(got, "tool_call:") || evs[0].Line == 0 {
		t.Fatalf("backfill = %s (%+v)", got, evs)
	}
	if last.Offset < reply {
		t.Fatalf("the backfill's resume point %d is before the last row %d", last.Offset, reply)
	}
	waitUntil(t, "the prompt index", func() bool {
		for _, m := range d.of(mirror.TypeEvents) {
			if m.ID == 3 && m.PromptIndex && len(m.Prompts) == 1 && !strings.Contains(m.Prompts[0].Text, secret) {
				return true
			}
		}
		return false
	})

	// A new row is tailed.
	home.Message(root, hermestest.Msg{Role: "tool", ToolCallID: "call_1", Content: `{"output":"deployed","exit_code":0}`})
	waitUntil(t, "the tailed result", func() bool { evs, _, _ := watchEvents(d, 3); return strings.Contains(evText(evs), "tool_result:") })
	if _, resets, _ := watchEvents(d, 3); resets != 1 {
		t.Fatalf("an append reset the view (%d resets)", resets)
	}

	// An in-place rewrite moves no id: the view starts over.
	home.Exec(`UPDATE messages SET content = 'On it, rewritten.' WHERE id = ?`, reply)
	waitUntil(t, "the reset after a rewrite", func() bool { _, resets, _ := watchEvents(d, 3); return resets == 2 })
	if evs, _, _ := watchEvents(d, 3); !strings.Contains(evText(evs), "On it, rewritten.") {
		t.Fatalf("the reset did not redraw the rewritten row: %s", evText(evs))
	}

	// The terminal switched sessions: the host names none; the view closes.
	wb.set(hermesWindowInfo(""))
	waitUntil(t, "the view to close", func() bool {
		for _, m := range d.of(mirror.TypeClosed) {
			if m.ID == 3 && strings.Contains(m.Error, "no longer shows a Hermes conversation") {
				return true
			}
		}
		return false
	})
}

// A watch of a Hermes window with no session (stale breadcrumb) is refused
// at once, and so is a watch on a host with no Hermes source.
func TestAHermesWindowWithNoSessionHasNoStream(t *testing.T) {
	src := NewHermesSource(hermestest.New(t).Dir, t.TempDir())
	t.Cleanup(func() { _ = src.State().Close() })
	wb := &windowsBackend{windows: []WindowInfo{hermesWindowInfo("")}}
	d := newDrivenMuster(t)
	d.run(t, hermesBackend{wb, src}, nil, true)
	d.send(t, mirror.Msg{Type: mirror.TypeWatch, ID: 4, Session: "h", Window: "@5"})
	waitUntil(t, "the refusal", func() bool {
		for _, m := range d.of(mirror.TypeClosed) {
			if m.ID == 4 && m.Error != "" {
				return true
			}
		}
		return false
	})
	if evs, _, _ := watchEvents(d, 4); len(evs) != 0 {
		t.Fatalf("a window with no session streamed %+v", evs)
	}
}
