package mirrorhost

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"

	"github.com/ZacxDev/muster/internal/termsize"
)

// WindowSizer is a Backend that can say how big a session's window is right
// now. Tmux is one; a backend that is not simply attaches at the size asked
// (and reports nothing), as before.
type WindowSizer interface {
	WindowSize(ctx context.Context, session string) (termsize.Size, error)
}

// WindowSize is the CLIENT size that shows session's current window exactly
// (termsize.ParseTmuxWindow: the window plus its status line).
func (t Tmux) WindowSize(ctx context.Context, session string) (termsize.Size, error) {
	cmd := exec.CommandContext(ctx, t.bin(), append(t.server(), "display-message", "-p", "-t", "="+session+":",
		termsize.TmuxWindowFormat)...)
	cmd.Env = tmuxEnv()
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return termsize.Size{}, fmt.Errorf("tmux display-message: %v: %s", err, strings.TrimSpace(errb.String()))
	}
	return termsize.ParseTmuxWindow(out.String())
}

// NoShrink wraps b so that no attach — read-only or interactive — shrinks the
// session's window (internal/termsize.Decide), and so that the browser is told,
// in-band, the size its client really has. A b that cannot report window sizes
// is returned unchanged.
//
// 🔴 WHY HERE AND NOT IN THE BROWSER: the window's size is a fact only this
// host has. The browser asks for its own fit; this decides, and says what it
// decided. A browser that asks for something smaller anyway (an old page, a
// bug) still cannot reflow the operator's session.
//
// 🔴 WHY NOT tmux's `window-size largest`: that is a SESSION option, so setting
// it would change the operator's own tmux for every client they attach — their
// laptop's terminal would then be the one panning a window sized for their
// desktop — and it would outlive muster-mirror. It also does not help when the
// phone is the only client (largest of one is the phone). Sizing OUR client is
// the only change that touches nothing but muster's own view.
func NoShrink(b Backend) Backend {
	ws, ok := b.(WindowSizer)
	if !ok {
		return b
	}
	return noShrink{Backend: b, sizer: ws}
}

type noShrink struct {
	Backend
	sizer WindowSizer
}

// Unwrap is the backend this wraps. 🔴 A WRAPPER THAT EMBEDS THE Backend
// INTERFACE HIDES EVERY OPTIONAL CAPABILITY OF WHAT IT WRAPS (a type assertion
// on the wrapper sees only Backend's own methods), so a capability lookup must
// walk through it: see ClaudeOf.
func (n noShrink) Unwrap() Backend { return n.Backend }

func (n noShrink) Attach(ctx context.Context, session string, readOnly bool, cols, rows uint16) (Terminal, error) {
	return sized(ctx, cols, rows,
		func(ctx context.Context) (termsize.Size, error) { return n.sizer.WindowSize(ctx, session) },
		func(c, r uint16) (Terminal, error) { return n.Backend.Attach(ctx, session, readOnly, c, r) })
}

// AttachWindow is the same rule for ONE window of a session (WindowAttacher):
// the client is sized from THAT window, which is not the session's current one
// when the viewer is looking at a window the operator is not.
//
// ⚠ A window viewer's client is ignore-size (ViewClientFlags), so beside
// another client it sizes nothing whatever size it has. This still matters:
// tmux does not honour ignore-size for the ONLY client of a window (measured on
// 3.7c — a lone 60x20 ignore-size viewer left a 150x40 window at 60x19), which
// is every window of a session nobody is attached to.
func (n noShrink) AttachWindow(ctx context.Context, session, window string, readOnly bool, cols, rows uint16) (Terminal, error) {
	wa, ok := n.Backend.(WindowAttacher)
	if !ok {
		return nil, errors.New("muster-mirror: this host's backend cannot attach to a single window")
	}
	size := func(context.Context) (termsize.Size, error) {
		return termsize.Size{}, errors.New("no window size")
	}
	if ws, ok := n.Backend.(WindowOfSizer); ok {
		size = func(ctx context.Context) (termsize.Size, error) { return ws.WindowSizeOf(ctx, session, window) }
	}
	return sized(ctx, cols, rows, size,
		func(c, r uint16) (Terminal, error) { return wa.AttachWindow(ctx, session, window, readOnly, c, r) })
}

// sized attaches (attach) a client at termsize.Decide's size for the window
// size reads now, under the no-shrink rule for every later resize.
func sized(ctx context.Context, cols, rows uint16, size func(context.Context) (termsize.Size, error), attach func(cols, rows uint16) (Terminal, error)) (Terminal, error) {
	req := termsize.Size{Cols: cols, Rows: rows}
	win, err := size(ctx)
	if err != nil {
		// Unknown: attach as asked and report nothing — the page then fits
		// itself exactly as it did before this existed.
		win = termsize.Size{}
	}
	got := termsize.Decide(req, win)
	t, err := attach(got.Cols, got.Rows)
	if err != nil {
		return nil, err
	}
	st := &sizedTerm{Terminal: t, ctx: ctx, size: size, cur: got, known: win.Known()}
	if st.known {
		st.pending = termsize.Report(got)
	}
	return st, nil
}

// sizedTerm is one attached client under NoShrink: it applies Decide to every
// resize, and delivers a size report AHEAD of output, as a read of its own.
//
// ⚠ THE REPORT WAITS FOR THE CLIENT'S FIRST OUTPUT. muster's socket bridge
// announces "connected" on the first bytes it relays (that is its evidence the
// client attached); a report sent before tmux drew anything would announce a
// client that might still fail. So the first Read that produces output returns
// the report instead, and the output follows on the next Read.
type sizedTerm struct {
	Terminal
	ctx context.Context
	// size reads the window's size now.
	size func(context.Context) (termsize.Size, error)

	mu      sync.Mutex
	cur     termsize.Size
	known   bool
	pending []byte // a report owed before the next output
	held    []byte // output read while a report went first
	heldErr error
}

func (s *sizedTerm) Read(p []byte) (int, error) {
	s.mu.Lock()
	if len(s.held) > 0 || s.heldErr != nil {
		n := copy(p, s.held)
		s.held = s.held[n:]
		var err error
		if len(s.held) == 0 {
			err, s.heldErr = s.heldErr, nil
		}
		s.mu.Unlock()
		return n, err
	}
	s.mu.Unlock()
	n, err := s.Terminal.Read(p)
	s.mu.Lock()
	defer s.mu.Unlock()
	if n == 0 || len(s.pending) == 0 || len(p) < len(s.pending) {
		return n, err
	}
	s.held = append(s.held[:0], p[:n]...)
	s.heldErr = err
	n = copy(p, s.pending)
	s.pending = nil
	return n, nil
}

// Resize applies Decide: a size that would shrink the window is replaced by
// the window's, and the browser is told what it got instead.
func (s *sizedTerm) Resize(cols, rows uint16) error {
	req := termsize.Size{Cols: cols, Rows: rows}
	s.mu.Lock()
	known, cur := s.known, s.cur
	s.mu.Unlock()
	if !known {
		// This attach reported nothing, so the page fits itself; keep the old
		// behaviour rather than half of the new one.
		return s.Terminal.Resize(cols, rows)
	}
	win, err := s.size(s.ctx)
	if err != nil {
		win = cur // the window is at least what this client last set
	}
	got := termsize.Decide(req, win)
	if got != cur {
		if err := s.Terminal.Resize(got.Cols, got.Rows); err != nil {
			return err
		}
	}
	s.mu.Lock()
	s.cur = got
	if got != req {
		s.pending = termsize.Report(got)
	}
	s.mu.Unlock()
	return nil
}
