package mirror_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
)

// DRAG-TO-SCROLL over the wire: a real mirror.Service and a real muster-mirror
// client; only tmux's copy-mode is played (scrollingBackend). The REAL tmux
// half — that copy-mode is entered, moves and leaves — is
// internal/mirrorhost's tmux_integration_test.go.

// scrollingBackend is paneBackend (a current muster-mirror) plus
// mirrorhost.ScrollBackend: per pane, how many lines back it is (absent = not
// in copy-mode), leaving the mode at the bottom like `copy-mode -e`.
type scrollingBackend struct {
	paneBackend
	smu     sync.Mutex
	pos     map[string]int
	calls   []string // "<pane> <lines>"
	cancels []string // panes
	refuse  error
}

func newScrollingBackend(sessions ...string) *scrollingBackend {
	return &scrollingBackend{paneBackend: paneBackend{newClaudeBackend(sessions...)}, pos: map[string]int{}}
}

func (b *scrollingBackend) ScrollPane(_ context.Context, pane string, lines int) (bool, bool, error) {
	b.smu.Lock()
	defer b.smu.Unlock()
	b.calls = append(b.calls, fmt.Sprintf("%s %d", pane, lines))
	if b.refuse != nil {
		return false, false, b.refuse
	}
	p, in := b.pos[pane]
	if !in && lines < 0 {
		return false, false, nil
	}
	if p += lines; p <= 0 {
		delete(b.pos, pane)
		return !in, false, nil
	}
	b.pos[pane] = p
	return !in, true, nil
}

func (b *scrollingBackend) CancelCopyMode(_ context.Context, pane string) error {
	b.smu.Lock()
	defer b.smu.Unlock()
	if _, in := b.pos[pane]; in {
		delete(b.pos, pane)
		b.cancels = append(b.cancels, pane)
	}
	return nil
}

func (b *scrollingBackend) snapshot() (calls, cancels []string, pos map[string]int) {
	b.smu.Lock()
	defer b.smu.Unlock()
	p := map[string]int{}
	for k, v := range b.pos {
		p[k] = v
	}
	return slices.Clone(b.calls), slices.Clone(b.cancels), p
}

// scrollHost connects a REAL muster-mirror for owner 1 with backend b and
// waits for its sessions.
func scrollHost(t *testing.T, r *rig, host string, b mirrorhost.Backend, interactive bool) (stop func()) {
	t.Helper()
	tok, _ := r.store.mint(t, 1, host)
	stop, _ = r.connect(t, host, tok, b, interactive)
	r.online(t, 1, host, "shell")
	return stop
}

// TestAScrollReachesTheSessionsActivePane is the feature: a current,
// -interactive host advertises scrolling, and a scroll lands on the session's
// active pane with its line count — clamped to MaxScrollLines — and with NO
// fresh sign-in (copy-mode reaches no program; see mirror/scroll.go).
func TestAScrollReachesTheSessionsActivePane(t *testing.T) {
	r := newRig(t)
	b := newScrollingBackend("main", "shell")
	scrollHost(t, r, "rw", b, true)
	h := hostOf(t, r, "rw")
	e, _ := h.Find("shell", "")
	if !h.PaneScroll || !e.CanScroll() {
		t.Fatalf("a current interactive host: PaneScroll=%v CanScroll=%v, want both true", h.PaneScroll, e.CanScroll())
	}
	if err := r.svc.ScrollWindow(asUser(1), "rw", "shell", "", 7); err != nil {
		t.Fatalf("scroll back 7: %v", err)
	}
	// A stale sign-in scrolls all the same: no step-up.
	if err := r.svc.ScrollWindow(asStaleUser(1), "rw", "shell", "", -3); err != nil {
		t.Fatalf("a stale sign-in's scroll: %v", err)
	}
	if err := r.svc.ScrollWindow(asUser(1), "rw", "shell", "", 100000); err != nil {
		t.Fatalf("a huge scroll: %v", err)
	}
	calls, _, pos := b.snapshot()
	want := []string{"%a-shell 7", "%a-shell -3", fmt.Sprintf("%%a-shell %d", mirror.MaxScrollLines)}
	if !slices.Equal(calls, want) {
		t.Fatalf("the host scrolled %q, want %q", calls, want)
	}
	if pos["%a-shell"] != 4+mirror.MaxScrollLines {
		t.Fatalf("the pane is %d lines back, want %d", pos["%a-shell"], 4+mirror.MaxScrollLines)
	}
}

// TestAScrollKeepsEveryOtherGate: owner isolation, the allowlist, a non-zero
// count — refused before the host is asked — and the host's own refusal comes
// back as ErrScrollRefused.
func TestAScrollKeepsEveryOtherGate(t *testing.T) {
	r := newRig(t)
	b := newScrollingBackend("main", "shell")
	scrollHost(t, r, "rw", b, true)
	for name, tc := range map[string]struct {
		ctx     context.Context
		session string
		lines   int
		want    error
	}{
		"another owner":       {asUser(2), "shell", 3, mirror.ErrHostOffline},
		"no principal":        {context.Background(), "shell", 3, mirror.ErrNoPrincipal},
		"an unlisted session": {asUser(1), "gone", 3, mirror.ErrSessionGone},
		"zero lines":          {asUser(1), "shell", 0, mirror.ErrBadScroll},
	} {
		if err := r.svc.ScrollWindow(tc.ctx, "rw", tc.session, "", tc.lines); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
	if calls, _, _ := b.snapshot(); len(calls) != 0 {
		t.Fatalf("a refused scroll reached the host: %q", calls)
	}
	b.smu.Lock()
	b.refuse = errors.New("pane %a-shell is in tree-mode, not copy-mode")
	b.smu.Unlock()
	if err := r.svc.ScrollWindow(asUser(1), "rw", "shell", "", 3); !errors.Is(err, mirror.ErrScrollRefused) ||
		errors.Is(err, mirror.ErrScrollPaneInMode) || errors.Is(err, mirror.ErrScrollUnsupported) {
		t.Fatalf("a host refusal = %v, want ErrScrollRefused and nothing more", err)
	}
	// A pane in another mode: the host names it, and muster hears it as
	// ErrScrollPaneInMode — never as "this host cannot scroll".
	b.smu.Lock()
	b.refuse = fmt.Errorf("%w: pane %%a-shell is in tree-mode, not copy-mode", mirrorhost.ErrPaneInAnotherMode)
	b.smu.Unlock()
	if err := r.svc.ScrollWindow(asUser(1), "rw", "shell", "", 3); !errors.Is(err, mirror.ErrScrollPaneInMode) || errors.Is(err, mirror.ErrScrollUnsupported) {
		t.Fatalf("a pane in another mode = %v, want ErrScrollPaneInMode", err)
	}
}

// TestAHostWithoutScrollingIsNeverSentOne: an older muster-mirror (no
// ScrollBackend) and a read-only host advertise nothing; the hub refuses HERE
// with ErrScrollUnsupported, and the page (CanScroll) draws no gesture.
func TestAHostWithoutScrollingIsNeverSentOne(t *testing.T) {
	r := newRig(t)
	old := newClaudeBackend("main", "shell")
	scrollHost(t, r, "old", paneBackend{old}, true)
	ro := newScrollingBackend("main", "shell")
	scrollHost(t, r, "ro", ro, false)
	for _, host := range []string{"old", "ro"} {
		h := hostOf(t, r, host)
		e, _ := h.Find("shell", "")
		if h.PaneScroll || e.CanScroll() {
			t.Errorf("%s: PaneScroll=%v CanScroll=%v, want false", host, h.PaneScroll, e.CanScroll())
		}
		if err := r.svc.ScrollWindow(asUser(1), host, "shell", "", 5); !errors.Is(err, mirror.ErrScrollUnsupported) {
			t.Errorf("%s: a scroll = %v, want ErrScrollUnsupported", host, err)
		}
	}
	if calls, _, _ := ro.snapshot(); len(calls) != 0 {
		t.Fatalf("a read-only host was scrolled: %q", calls)
	}
}

// TestAHostAdvertisesPaneScrollOnlyWhenItWouldAcceptOne: what a REAL
// muster-mirror puts in its hello.
func TestAHostAdvertisesPaneScrollOnlyWhenItWouldAcceptOne(t *testing.T) {
	for name, tc := range map[string]struct {
		interactive bool
		backend     mirrorhost.Backend
		want        bool
	}{
		"interactive, can scroll":       {true, newScrollingBackend("shell"), true},
		"read-only, can scroll":         {false, newScrollingBackend("shell"), false},
		"interactive, an older backend": {true, paneBackend{newClaudeBackend("shell")}, false},
	} {
		if m := helloOf(t, tc.interactive, tc.backend); m.PaneScroll != tc.want {
			t.Errorf("%s: the hello says PaneScroll=%v, want %v", name, m.PaneScroll, tc.want)
		}
	}
}

// TestTheScrollWireIsWhatAHandDrivenHostSees: the frame muster sends, as a bare
// WebSocket host reads it — and none at all to a host whose hello did not
// advertise PaneScroll (an older muster-mirror, or a read-only host claiming it).
func TestTheScrollWireIsWhatAHandDrivenHostSees(t *testing.T) {
	r := newRig(t)
	for name, tc := range map[string]struct {
		paneScroll, interactive, sent bool
	}{
		"advertised":             {true, true, true},
		"not advertised (old)":   {false, true, false},
		"read-only, claiming it": {true, false, false},
	} {
		host := map[bool]string{true: "a", false: "b"}[tc.paneScroll] + map[bool]string{true: "i", false: "r"}[tc.interactive]
		tok, _ := r.store.mint(t, 1, host)
		hello := hello(host, tc.interactive, "shell")
		hello.PaneScroll = tc.paneScroll
		raw, err := r.rawConnect(t, tok, hello)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		r.online(t, 1, host, "shell")
		if got := hostOf(t, r, host).PaneScroll; got != tc.sent {
			t.Errorf("%s: the hub lists PaneScroll=%v, want %v", name, got, tc.sent)
		}
		ctx, cancel := context.WithTimeout(asUser(1), 300*time.Millisecond)
		// Far past the bound: muster clamps it before it is sent.
		err = r.svc.ScrollWindow(ctx, host, "shell", "", -100000)
		cancel()
		var got []mirror.Msg
		raw.mu.Lock()
		for _, m := range raw.text {
			if m.Type == mirror.TypeScroll {
				got = append(got, m)
			}
		}
		raw.mu.Unlock()
		if !tc.sent {
			if !errors.Is(err, mirror.ErrScrollUnsupported) || len(got) != 0 {
				t.Errorf("%s: %v and %d scroll frame(s), want ErrScrollUnsupported and none", name, err, len(got))
			}
			continue
		}
		// The hand-driven host never answers: the wait ends with the context.
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("%s: an unanswered scroll = %v, want the context's deadline", name, err)
		}
		if len(got) != 1 || got[0].Session != "shell" || got[0].Lines != -mirror.MaxScrollLines || got[0].ID == 0 || got[0].Window != "" {
			t.Errorf("%s: the host saw %+v, want one scroll of shell by -%d", name, got, mirror.MaxScrollLines)
		}
	}
}

// TestADisconnectLeavesNoPaneInCopyMode: the host cancels every copy-mode it
// entered as soon as its connection ends, without waiting for the idle timer.
func TestADisconnectLeavesNoPaneInCopyMode(t *testing.T) {
	r := newRig(t)
	b := newScrollingBackend("main", "shell")
	stop := scrollHost(t, r, "rw", b, true)
	if err := r.svc.ScrollWindow(asUser(1), "rw", "shell", "", 9); err != nil {
		t.Fatal(err)
	}
	if _, _, pos := b.snapshot(); pos["%a-shell"] != 9 {
		t.Fatalf("fixture: the pane is not in copy-mode: %v", pos)
	}
	stop()
	eventually(t, "the pane left copy-mode with the connection", func() bool {
		_, cancels, pos := b.snapshot()
		return len(pos) == 0 && slices.Equal(cancels, []string{"%a-shell"})
	})
}

// TestAHostsNamedRefusalIsTheErrorMusterActsOn: a hand-driven host answers
// each scroll with a refusal naming its kind (Msg.Refusal), and muster hears
// each kind as its own error. The one that matters most is "unsupported": it
// must come back as ErrScrollUnsupported (the error internal/api turns into
// ui.ScrollRefusalUnsupported, the only code that stops the page's gesture)
// and NOT as a plain ErrScrollRefused, which the page keeps scrolling through.
// The other kinds are the controls: each row's error is distinct from every
// other row's, so a kind mapped to its neighbour goes red.
func TestAHostsNamedRefusalIsTheErrorMusterActsOn(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "hd")
	hi := hello("hd", true, "shell")
	hi.PaneScroll = true
	raw, err := r.rawConnect(t, tok, hi)
	if err != nil {
		t.Fatal(err)
	}
	r.online(t, 1, "hd", "shell")
	all := []error{mirror.ErrScrollUnsupported, mirror.ErrScrollPaneInMode, mirror.ErrScrollRefused}
	for _, tc := range []struct {
		refusal string
		want    error
	}{
		{mirror.ScrollRefusalUnsupported, mirror.ErrScrollUnsupported},
		{mirror.ScrollRefusalMode, mirror.ErrScrollPaneInMode},
		{"", mirror.ErrScrollRefused},
		{"a-kind-this-muster-does-not-know", mirror.ErrScrollRefused},
	} {
		seen := len(raw.of(mirror.TypeScroll))
		done := make(chan error, 1)
		go func() { done <- r.svc.ScrollWindow(asUser(1), "hd", "shell", "", 3) }()
		var asked mirror.Msg
		eventually(t, "the host is asked to scroll", func() bool {
			if got := raw.of(mirror.TypeScroll); len(got) > seen {
				asked = got[seen]
				return true
			}
			return false
		})
		raw.write(t, mirror.Msg{Type: mirror.TypeScrolled, ID: asked.ID, Error: "no scroll here", Refusal: tc.refusal})
		var err error
		select {
		case err = <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("refusal %q: the scroll never returned", tc.refusal)
		}
		for _, e := range all {
			if errors.Is(err, e) != (e == tc.want) {
				t.Errorf("refusal %q: muster heard %v; want %v and none of the others", tc.refusal, err, tc.want)
				break
			}
		}
	}
}
