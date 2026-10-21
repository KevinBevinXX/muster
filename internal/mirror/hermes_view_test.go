package mirror_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// A HERMES WINDOW's RENDERED VIEW on the server (docs/design/hermes.md,
// Phase 2): the hub asks the host to stream a Hermes window only when the host
// NAMES the conversation it shows (Window.AgentSession) — a host older than
// Phase 2 never does — and everything that sends stays Claude-only.

func TestAHermesWindowWithASessionHasARenderedView(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "h")
	sid := "local-" + strings.Repeat("e", 64)
	raw, err := r.rawConnect(t, tok, winHello("h", true,
		mirror.Window{Session: "s", ID: "@1", Index: 0, Active: true, Runtime: mirror.RuntimeHermes, AgentSession: sid, Status: mirror.StatusIdle, StatusSince: 5},
		mirror.Window{Session: "s", ID: "@2", Index: 1, Runtime: mirror.RuntimeHermes}))
	if err != nil {
		t.Fatal(err)
	}
	r.online(t, 1, "h", "s")
	// The listing says which has a view.
	var views []string
	for _, e := range r.svc.Hosts(asUser(1))[0].Entries() {
		if e.HasRenderedView() {
			views = append(views, e.Window)
		}
	}
	if len(views) != 1 || views[0] != "@1" {
		t.Fatalf("entries with a rendered view = %v, want only @1 (the Hermes window whose session the host named)", views)
	}

	// The window with a session is streamed, addressed by its id …
	go func() { _ = r.svc.WatchWindow(asUser(1), "h", "s", "@1", "", 0, func(mirror.Msg) error { return nil }) }()
	eventually(t, "the watch for the Hermes window", func() bool { return len(raw.of(mirror.TypeWatch)) == 1 })
	if w := raw.of(mirror.TypeWatch)[0]; w.Window != "@1" || w.Session != "s" {
		t.Fatalf("the watch sent the host = %+v", w)
	}
	// … the one without (a stale breadcrumb, or a Phase 1 host) is not.
	// Bounded: a regression that streams it would block until the context
	// ends, and must fail as a refusal missing, not as a 600 s hang.
	bctx, cancel := context.WithTimeout(asUser(1), 3*time.Second)
	defer cancel()
	err = r.svc.WatchWindow(bctx, "h", "s", "@2", "", 0, func(mirror.Msg) error { return nil })
	if !errors.Is(err, mirror.ErrNotClaude) || bctx.Err() != nil {
		t.Fatalf("watch of a Hermes window with no session = %v (ctx %v), want an explicit ErrNotClaude refusal", err, bctx.Err())
	}
	if n := len(raw.of(mirror.TypeWatch)); n != 1 {
		t.Fatalf("the host was asked for %d watches, want 1", n)
	}

	// 🔴 A rendered view is not a send path: the Claude-only paste the
	// rendered view's composer uses is refused for the Hermes window, and so
	// is the Terminal's — nothing reaches the host.
	if err := r.svc.PasteWindow(asUser(1), "h", "s", "@1", "hi", mirror.PasteClaude); err == nil {
		t.Fatal("a rendered-view paste into a Hermes window was accepted")
	}
	if err := r.svc.PasteWindow(asUser(1), "h", "s", "@1", "hi", mirror.PasteActivePane); err == nil {
		t.Fatal("a Terminal paste into a Hermes window was accepted")
	}
	if n := len(raw.of(mirror.TypePaste)); n != 0 {
		t.Fatalf("the host received %d pastes", n)
	}
}

// A v1 host's Hermes window has no rendered view, whatever it claims.
func TestAV1HostsHermesWindowHasNoRenderedView(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "h")
	m := winHello("h", true, mirror.Window{Session: "s", ID: "@1", Index: 0, Active: true, Runtime: mirror.RuntimeHermes,
		AgentSession: "local-" + strings.Repeat("e", 64)})
	m.Version = 1
	if _, err := r.rawConnect(t, tok, m); err != nil {
		t.Fatal(err)
	}
	r.online(t, 1, "h", "s")
	if e := r.svc.Hosts(asUser(1))[0].Entries()[0]; e.HasRenderedView() || e.HermesView() {
		t.Fatalf("a v1 host's Hermes window has a view: %+v", e)
	}
}

// The kind a list names a Hermes window by is "Hermes" — with a session or
// without — never its pane's command (python3.14, S10).
func TestAHermesWindowsKindIsHermes(t *testing.T) {
	for _, w := range []mirror.Window{
		{Session: "s", ID: "@1", Runtime: mirror.RuntimeHermes, AgentSession: "local-x", Command: "python3.14"},
		{Session: "s", ID: "@2", Runtime: mirror.RuntimeHermes, Command: "python3.14"},
	} {
		h := mirror.HostView{Host: "h", Online: true, Rendered: true, Windows: []mirror.Window{w}}
		if k := h.Entries()[0].Kind(); k != mirror.KindHermes {
			t.Errorf("%s: Kind = %q, want %q", w.ID, k, mirror.KindHermes)
		}
	}
	h := mirror.HostView{Host: "h", Windows: []mirror.Window{{Session: "s", ID: "@3", Command: "python3.14"}}}
	if k := h.Entries()[0].Kind(); k != "python3.14" {
		t.Errorf("a plain python window's Kind = %q", k)
	}
}
