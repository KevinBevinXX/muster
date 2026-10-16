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
// 🔴 A PANE IS NEVER LEFT IN COPY-MODE BY THIS HOST. It enters copy-mode with
// `-e`, so scrolling back to the bottom leaves it; and every pane it put in
// copy-mode has an idle timer (DefaultScrollIdle, reset by each scroll of that
// pane) that sends `-X cancel` when it fires. A connection ending cancels at
// once (hostSide.closeAll).
//
// ⚠ ONLY COPY-MODE THIS HOST ENTERED IS ITS TO CANCEL. A pane already in
// copy-mode when a scroll arrives (its operator pressed prefix-[) is scrolled,
// but no timer is armed for it: cancelling would throw the operator out of a
// mode they chose. The residual case is the operator leaving this host's
// copy-mode by hand and entering their own before the timer fires; it then
// cancels theirs. The timer is disarmed as soon as a scroll finds the pane out
// of copy-mode, which keeps that window short.

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
	// CancelCopyMode leaves copy-mode on pane; a pane not in copy-mode is left
	// alone.
	CancelCopyMode(ctx context.Context, pane string) error
}

// ScrollOf is b's ScrollBackend, looking through wrappers like ClaudeOf.
func ScrollOf(b Backend) (ScrollBackend, bool) { return capabilityOf[ScrollBackend](b) }

// paneMode is pane's #{pane_in_mode} and #{pane_mode} ("copy-mode", …).
func (t Tmux) paneMode(ctx context.Context, pane string) (bool, string, error) {
	cmd := exec.CommandContext(ctx, t.bin(), t.query("display-message", "-p", "-t", pane, "#{pane_in_mode}\t#{pane_mode}")...)
	cmd.Env = tmuxEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, "", fmt.Errorf("tmux display-message: %v: %s", err, strings.TrimSpace(string(out)))
	}
	in, mode, _ := strings.Cut(strings.TrimRight(string(out), "\r\n"), "\t")
	return in == "1", mode, nil
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
		return false, true, fmt.Errorf("muster-mirror: pane %s is in %s, not copy-mode", pane, mode)
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
	if err != nil || !in || mode != "copy-mode" {
		return err
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

	// op serialises every scroll and every idle cancel, so a cancel can never
	// land between a scroll's "is it in copy-mode" and its scroll command.
	op sync.Mutex

	mu    sync.Mutex
	owned map[string]*ownedPane
	gen   uint64
	shut  bool
}

// ownedPane is one pane this host put in copy-mode.
type ownedPane struct {
	t   *time.Timer
	gen uint64
}

func newScroller(sb ScrollBackend, idle time.Duration, logger *log.Logger) *scroller {
	if idle <= 0 {
		idle = DefaultScrollIdle
	}
	return &scroller{sb: sb, idle: idle, log: logger, owned: map[string]*ownedPane{}}
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
		s.gen++
		gen := s.gen
		s.owned[pane] = &ownedPane{gen: gen, t: time.AfterFunc(s.idle, func() { s.expire(pane, gen) })}
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

// expire is pane's idle timer: cancel its copy-mode, unless a later scroll
// re-armed (gen) or disarmed it.
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.sb.CancelCopyMode(ctx, pane); err != nil {
		s.log.Printf("muster-mirror: leave copy-mode on pane %s after %s idle: %v", pane, s.idle, err)
		return
	}
	s.log.Printf("muster-mirror: pane %s left copy-mode after %s with no scroll", pane, s.idle)
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, p := range panes {
		_ = s.sb.CancelCopyMode(ctx, p)
	}
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

// scroll scrolls the active pane of what m names through its history and
// answers scrolled.
//
// 🔴 REFUSED UNLESS THIS HOST OPTED IN (-interactive), whatever muster asks,
// and only for a listed session/window with a line count mirror.ClampScroll
// accepts — muster checks the same, and this layer does not depend on it.
func (h *hostSide) scroll(m mirror.Msg) {
	reply := func(err error) {
		msg := ""
		if err != nil {
			msg = err.Error()
			h.c.log.Printf("muster-mirror: scroll %q%s refused: %v", m.Session, windowSuffix(m.Window), err)
		}
		_ = h.send(mirror.Msg{Type: mirror.TypeScrolled, ID: m.ID, Error: msg})
	}
	if !h.c.cfg.Interactive {
		reply(errors.New("this host is read-only (muster-mirror was not started with -interactive)"))
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
		reply(errors.New("this host cannot scroll a pane"))
		return
	}
	ctx, cancel := context.WithTimeout(h.ctx, 4*time.Second)
	defer cancel()
	pane, ok := "", false
	if m.Window != "" {
		w, err := h.window(ctx, m.Session, m.Window)
		if err != nil {
			reply(err)
			return
		}
		pane, ok = w.Pane, w.Pane != ""
	} else if pb, has := h.paneBackend(); has {
		pane, ok = pb.ActivePane(ctx, m.Session)
	}
	if !ok {
		reply(fmt.Errorf("session %q has no active pane to scroll", m.Session))
		return
	}
	reply(h.scroller.scroll(ctx, pane, lines))
}
