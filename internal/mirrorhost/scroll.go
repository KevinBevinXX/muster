package mirrorhost

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// SCROLLING A PANE'S HISTORY (the Terminal view's drag-to-scroll): the host half.
// See internal/mirror's scroll.go for why it is tmux copy-mode, and why it asks
// for no fresh sign-in.
//
// 🔴 THIS HOST'S COPY-MODE IS ALWAYS EVENTUALLY LEFT. It enters copy-mode with
// `-e`, so scrolling back to the bottom leaves it; and every pane it put in
// copy-mode has an idle timer (DefaultScrollIdle, reset by each scroll of that
// pane) that sends `-X cancel` when it fires. A connection ending cancels at
// once (hostSide.closeAll). A message sent from the composer into a pane this
// host put in copy-mode cancels it first (scroller.leaveForPaste), so the
// message and its answer are on screen rather than behind the scrolled view.
//
// ⚠ TMUX MODES STACK. A pane in this host's copy-mode in which someone then
// opens choose-tree has #{pane_in_mode}=2 and #{pane_mode}=tree-mode: copy-mode
// is UNDER the menu, and `-X cancel` cannot reach it. A cancel that finds
// another mode on top (ErrPaneInAnotherMode) does not forget the pane: it
// re-arms the idle timer and tries again, so this host's copy-mode is cancelled
// once the menu closes, whenever that is. The connection ending does the same
// for such a pane rather than leave it behind.
//
// ⚠ THE IDLE CANCEL IS BY THE CLOCK, NOT BY WHAT SOMEONE IS DOING IN THE PANE.
// Only a scroll from muster resets it. Someone at the host's own terminal who
// starts a selection in copy-mode THIS host entered loses it when the timer
// fires 30 s after the last scroll.
//
// ⚠ ONLY COPY-MODE THIS HOST ENTERED IS ITS TO CANCEL. A pane already in
// copy-mode when a scroll arrives (its operator pressed prefix-[) is scrolled,
// but no timer is armed for it: cancelling would throw the operator out of a
// mode they chose. The residual case is the operator leaving this host's
// copy-mode by hand and entering their own before the timer fires; it then
// cancels theirs. The timer is disarmed as soon as a scroll finds the pane out
// of copy-mode, which keeps that window short.

// ErrPaneInAnotherMode: the pane's top mode is not copy-mode (choose-tree, a
// menu, view-mode). A scroll is refused with it (mirror.ScrollRefusalMode on
// the wire) and a cancel reports it, since copy-mode, if it is there at all, is
// under that mode where `-X cancel` cannot reach it.
var ErrPaneInAnotherMode = errors.New("muster-mirror: the pane is in another mode")

// DefaultScrollIdle is how long a pane this host put in copy-mode stays there
// with no further scroll before the host cancels it (Config.ScrollIdle).
const DefaultScrollIdle = 30 * time.Second

// ScrollBackend is a Backend that can scroll a pane through its history in
// tmux copy-mode. The tmux backend implements it.
type ScrollBackend interface {
	// ScrollPane scrolls pane by lines (positive is back, mirror.Msg.Lines).
	// A pane in no mode is put in copy-mode (`copy-mode -e`) first — only for a
	// scroll BACK; a scroll forward of a pane in no mode is already at the
	// bottom and does nothing. entered reports that this call entered
	// copy-mode; inMode that the pane is in copy-mode afterwards (false once
	// -e has left it at the bottom).
	ScrollPane(ctx context.Context, pane string, lines int) (entered, inMode bool, err error)
	// CancelCopyMode leaves copy-mode on pane; a pane in no mode is left alone
	// (nil). A pane whose top mode is another one is left alone too, and
	// answers ErrPaneInAnotherMode: copy-mode may be under it.
	CancelCopyMode(ctx context.Context, pane string) error
}

// ScrollOf is b's ScrollBackend, looking through wrappers like ClaudeOf.
func ScrollOf(b Backend) (ScrollBackend, bool) { return capabilityOf[ScrollBackend](b) }

// paneMode is whether pane is in any mode, and its TOP mode (#{pane_mode}:
// "copy-mode", "tree-mode", …).
//
// ⚠ #{pane_in_mode} IS A COUNT, NOT A FLAG: modes stack, and copy-mode with
// choose-tree opened over it is 2 (measured, tmux 3.7c). Anything but 0 is
// "in a mode".
func (t Tmux) paneMode(ctx context.Context, pane string) (bool, string, error) {
	cmd := exec.CommandContext(ctx, t.bin(), t.query("display-message", "-p", "-t", pane, "#{pane_in_mode}\t#{pane_mode}")...)
	cmd.Env = tmuxEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, "", fmt.Errorf("tmux display-message: %v: %s", err, strings.TrimSpace(string(out)))
	}
	in, mode, _ := strings.Cut(strings.TrimRight(string(out), "\r\n"), "\t")
	return in != "0" && in != "", mode, nil
}

// ScrollPane implements ScrollBackend.
//
// The pane is a PANE ID (%<n>), never a session or window name, so the scroll
// reaches exactly the pane muster resolved and needs no client — it works
// with only muster's read-only viewer attached (measured:
// tmux_integration_test.go).
func (t Tmux) ScrollPane(ctx context.Context, pane string, lines int) (entered, inMode bool, err error) {
	if !strings.HasPrefix(pane, "%") {
		return false, false, fmt.Errorf("muster-mirror: %q is not a tmux pane id", pane)
	}
	lines, ok := mirror.ClampScroll(lines)
	if !ok {
		return false, false, errors.New("muster-mirror: nothing to scroll")
	}
	in, mode, err := t.paneMode(ctx, pane)
	if err != nil {
		return false, false, err
	}
	switch {
	case in && mode != "copy-mode":
		// Another mode (choose-tree, a view-mode's output): not this host's to
		// leave, and copy-mode's commands mean nothing there.
		return false, true, fmt.Errorf("%w: pane %s is in %s, not copy-mode", ErrPaneInAnotherMode, pane, mode)
	case !in && lines < 0:
		return false, false, nil
	case !in:
		if _, err := t.run(ctx, "copy-mode", "-e", "-t", pane); err != nil {
			return false, false, err
		}
		entered = true
	}
	cmd, n := "scroll-up", lines
	if lines < 0 {
		cmd, n = "scroll-down", -lines
	}
	if _, err := t.run(ctx, "send-keys", "-X", "-N", fmt.Sprint(n), "-t", pane, cmd); err != nil {
		return entered, true, err
	}
	in, mode, err = t.paneMode(ctx, pane)
	if err != nil {
		return entered, true, err
	}
	return entered, in && mode == "copy-mode", nil
}

// CancelCopyMode implements ScrollBackend.
func (t Tmux) CancelCopyMode(ctx context.Context, pane string) error {
	if !strings.HasPrefix(pane, "%") {
		return fmt.Errorf("muster-mirror: %q is not a tmux pane id", pane)
	}
	in, mode, err := t.paneMode(ctx, pane)
	switch {
	case err != nil || !in:
		return err
	case mode != "copy-mode":
		return fmt.Errorf("%w: pane %s is in %s", ErrPaneInAnotherMode, pane, mode)
	}
	_, err = t.run(ctx, "send-keys", "-X", "-t", pane, "cancel")
	return err
}

// scroller is one connection's scrolls: which panes it put in copy-mode, and
// each one's idle timer.
type scroller struct {
	sb   ScrollBackend
	idle time.Duration
	log  *log.Logger
	// afterFunc starts an idle timer: time.AfterFunc, except in tests, which
	// fire the timers by hand rather than race a wall clock.
	afterFunc func(time.Duration, func()) stopper

	// op serialises every scroll and every idle cancel, so a cancel can never
	// land between a scroll's "is it in copy-mode" and its scroll command.
	op sync.Mutex

	mu    sync.Mutex
	owned map[string]*ownedPane
	gen   uint64
	shut  bool
}

// stopper is the one method of *time.Timer the scroller uses.
type stopper interface{ Stop() bool }

// ownedPane is one pane this host put in copy-mode.
type ownedPane struct {
	t   stopper
	gen uint64
}

func newScroller(sb ScrollBackend, idle time.Duration, logger *log.Logger) *scroller {
	if idle <= 0 {
		idle = DefaultScrollIdle
	}
	return &scroller{sb: sb, idle: idle, log: logger, owned: map[string]*ownedPane{},
		afterFunc: func(d time.Duration, f func()) stopper { return time.AfterFunc(d, f) }}
}

// scroll scrolls pane by lines and (re)arms, or disarms, its idle cancel.
func (s *scroller) scroll(ctx context.Context, pane string, lines int) error {
	s.op.Lock()
	defer s.op.Unlock()
	entered, inMode, err := s.sb.ScrollPane(ctx, pane, lines)
	s.mu.Lock()
	cur := s.owned[pane]
	shut := s.shut
	switch {
	case shut:
		// The connection ended while this ran and closeAll has already swept:
		// leave what this call entered at once, below.
	case !inMode:
		// At the bottom (-e left copy-mode) or never entered: nothing to cancel.
		if cur != nil {
			cur.t.Stop()
			delete(s.owned, pane)
		}
	case cur != nil || entered:
		if cur != nil {
			cur.t.Stop()
		}
		s.arm(pane)
		if entered {
			s.log.Printf("muster-mirror: pane %s entered copy-mode for a scroll (cancelled after %s idle)", pane, s.idle)
		}
	}
	s.mu.Unlock()
	if shut && entered && inMode {
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.sb.CancelCopyMode(cctx, pane)
	}
	return err
}

// arm (re)starts pane's idle timer. The caller holds s.mu.
func (s *scroller) arm(pane string) {
	s.gen++
	gen := s.gen
	s.owned[pane] = &ownedPane{gen: gen, t: s.afterFunc(s.idle, func() { s.expire(pane, gen) })}
}

// expire is pane's idle timer: cancel its copy-mode, unless a later scroll
// re-armed (gen) or disarmed it. A pane with another mode over copy-mode is
// re-armed, not forgotten (ErrPaneInAnotherMode): the cancel is tried again
// one idle spell later, until the pane is out of that mode.
func (s *scroller) expire(pane string, gen uint64) {
	s.op.Lock()
	defer s.op.Unlock()
	s.mu.Lock()
	cur := s.owned[pane]
	if cur == nil || cur.gen != gen {
		s.mu.Unlock()
		return
	}
	delete(s.owned, pane)
	s.mu.Unlock()
	s.cancelOrRearm(pane, "after "+s.idle.String()+" with no scroll")
}

// cancelOrRearm cancels this host's copy-mode on pane now, or — when another
// mode is on top of it — arms pane's timer to try again. The caller holds s.op
// and has taken pane out of s.owned.
func (s *scroller) cancelOrRearm(pane, why string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := s.sb.CancelCopyMode(ctx, pane)
	switch {
	case errors.Is(err, ErrPaneInAnotherMode):
		s.mu.Lock()
		if s.owned[pane] == nil {
			s.arm(pane)
		}
		s.mu.Unlock()
		s.log.Printf("muster-mirror: pane %s has another mode over copy-mode; trying again in %s", pane, s.idle)
	case err != nil:
		s.log.Printf("muster-mirror: leave copy-mode on pane %s %s: %v", pane, why, err)
	default:
		s.log.Printf("muster-mirror: pane %s left copy-mode %s", pane, why)
	}
}

// leaveForPaste cancels the copy-mode THIS host put pane in, before a message
// is pasted into it: a paste lands in the program underneath, but the pane
// keeps showing the scrolled history, so the message and its answer would be
// out of view. Copy-mode the operator entered is not in s.owned and is left
// alone. Another mode on top keeps the timer armed (cancelOrRearm); the paste
// goes ahead either way.
func (s *scroller) leaveForPaste(pane string) {
	s.op.Lock()
	defer s.op.Unlock()
	s.mu.Lock()
	cur := s.owned[pane]
	if cur == nil {
		s.mu.Unlock()
		return
	}
	cur.t.Stop()
	delete(s.owned, pane)
	s.mu.Unlock()
	s.cancelOrRearm(pane, "for a message sent to it")
}

// closeAll cancels copy-mode on every pane this connection put in it, now:
// the page that was scrolling it is gone with the connection.
func (s *scroller) closeAll() {
	s.mu.Lock()
	s.shut = true
	panes := make([]string, 0, len(s.owned))
	for p, o := range s.owned {
		o.t.Stop()
		panes = append(panes, p)
	}
	s.owned = map[string]*ownedPane{}
	s.mu.Unlock()
	if len(panes) == 0 {
		return
	}
	s.op.Lock()
	defer s.op.Unlock()
	for _, p := range panes {
		// A pane with another mode on top keeps a timer past the connection's
		// end, so its copy-mode is still cancelled once that mode closes.
		s.cancelOrRearm(p, "as its connection ended")
	}
}

// WindowPaner is a Backend that can name one window's active pane with ONE
// cheap read — no Claude Code lookup, unlike WindowLister.Windows, whose
// LocatePanes scans /proc (measured on a host of 75 windows: 50–130 ms a call,
// for EVERY scroll). The tmux backend implements it.
type WindowPaner interface {
	// WindowPane is the active pane id of window (a tmux window id, "@12") of
	// session, read now; an error when the session is gone or the window is no
	// longer in it.
	WindowPane(ctx context.Context, session, window string) (string, error)
}

// WindowPanerOf is b's WindowPaner, looking through wrappers like ClaudeOf.
func WindowPanerOf(b Backend) (WindowPaner, bool) { return capabilityOf[WindowPaner](b) }

// WindowPane implements WindowPaner with one `tmux list-windows` of that ONE
// session (#{pane_id} in a window's format is its active pane).
//
// 🔴 THE SESSION IS LISTED, NOT TARGETED AS `session:@id`. tmux resolves
// `=other:@12` to other's CURRENT window when @12 is not in it, and an unknown
// `@999` to an empty answer with exit status 0 (both measured, 3.7c) — a
// display-message on such a target would name the wrong pane, or none,
// without an error. Listing the session's own windows and matching the id
// exactly is how this knows the window is still the session's (as AttachWindow
// does).
func (t Tmux) WindowPane(ctx context.Context, session, window string) (string, error) {
	if err := mirror.ValidSession(session); err != nil {
		return "", err
	}
	if err := mirror.ValidWindowID(window); err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, t.bin(), t.query("list-windows", "-t", "="+session, "-F", "#{window_id}\t#{pane_id}")...)
	cmd.Env = tmuxEnv()
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("muster-mirror: tmux session %q is gone", session)
	}
	for _, line := range strings.Split(string(out), "\n") {
		id, pane, ok := strings.Cut(strings.TrimRight(line, "\r"), "\t")
		if ok && id == window && strings.HasPrefix(pane, "%") {
			return pane, nil
		}
	}
	return "", fmt.Errorf("window %s of session %q is gone", window, session)
}

// windowPane is the active pane of window of session, read now: by
// WindowPaner's one cheap read when the backend has it, else by a full
// hostSide.window (a backend without it — the browser suite's fake).
func (h *hostSide) windowPane(ctx context.Context, session, window string) (string, error) {
	if wp, ok := WindowPanerOf(h.c.cfg.Backend); ok {
		return wp.WindowPane(ctx, session, window)
	}
	w, err := h.window(ctx, session, window)
	if err != nil {
		return "", err
	}
	if w.Pane == "" {
		return "", fmt.Errorf("window %s of session %q has no active pane", window, session)
	}
	return w.Pane, nil
}

// scrollBackend is the backend's ScrollBackend when this host advertises
// scrolling (paneScroll).
func (h *hostSide) scrollBackend() (ScrollBackend, bool) {
	if !h.paneScroll() {
		return nil, false
	}
	return ScrollOf(h.c.cfg.Backend)
}

// paneScroll is what this host advertises as Msg.PaneScroll: an -interactive
// v2 host whose backend can both scroll a pane and resolve a session's active
// pane. A read-only host advertises nothing it would refuse.
func (h *hostSide) paneScroll() bool {
	_, scroll := ScrollOf(h.c.cfg.Backend)
	_, pane := h.paneBackend()
	return scroll && pane && h.c.cfg.Interactive
}

// errCannotScroll marks a refusal that means this host cannot scroll at all
// (mirror.ScrollRefusalUnsupported) — the one a page stops asking after.
var errCannotScroll = errors.New("muster-mirror: cannot scroll")

// scroll scrolls the active pane of what m names through its history and
// answers scrolled.
//
// 🔴 REFUSED UNLESS THIS HOST OPTED IN (-interactive), whatever muster asks,
// and only for a listed session/window with a line count mirror.ClampScroll
// accepts — muster checks the same, and this layer does not depend on it.
func (h *hostSide) scroll(m mirror.Msg) {
	reply := func(err error) {
		ans := mirror.Msg{Type: mirror.TypeScrolled, ID: m.ID}
		if err != nil {
			ans.Error = err.Error()
			switch {
			case errors.Is(err, ErrPaneInAnotherMode):
				ans.Refusal = mirror.ScrollRefusalMode
			case errors.Is(err, errCannotScroll):
				ans.Refusal = mirror.ScrollRefusalUnsupported
			}
			h.c.log.Printf("muster-mirror: scroll %q%s refused: %v", m.Session, windowSuffix(m.Window), err)
		}
		_ = h.send(ans)
	}
	if !h.c.cfg.Interactive {
		reply(fmt.Errorf("%w: this host is read-only (muster-mirror was not started with -interactive)", errCannotScroll))
		return
	}
	if err := h.allowed(m); err != nil {
		reply(err)
		return
	}
	lines, ok := mirror.ClampScroll(m.Lines)
	if !ok {
		reply(errors.New("a scroll must move at least one line"))
		return
	}
	if _, ok := h.scrollBackend(); !ok || h.scroller == nil {
		reply(fmt.Errorf("%w: this host cannot scroll a pane", errCannotScroll))
		return
	}
	ctx, cancel := context.WithTimeout(h.ctx, 4*time.Second)
	defer cancel()
	// The pane is read NOW, but cheaply (windowPane): a scroll comes once an
	// animation frame, and the full window read finds Claude Code too.
	pane, ok := "", false
	if m.Window != "" {
		p, err := h.windowPane(ctx, m.Session, m.Window)
		if err != nil {
			reply(err)
			return
		}
		pane, ok = p, true
	} else if pb, has := h.paneBackend(); has {
		pane, ok = pb.ActivePane(ctx, m.Session)
	}
	if !ok {
		reply(fmt.Errorf("session %q has no active pane to scroll", m.Session))
		return
	}
	reply(h.scroller.scroll(ctx, pane, lines))
}
