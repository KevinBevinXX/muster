package mirror

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// SCROLLING A PANE'S HISTORY from the Terminal view: the server half.
//
// The viewer's tmux client paints only the visible screen, so the browser's
// terminal holds no history to scroll. The history lives in tmux, and the way
// to read it is tmux's own COPY-MODE on the pane: the host enters it (`copy-mode
// -e`) and scrolls it (`send-keys -X -N <n> scroll-up|scroll-down`), and every
// client showing that pane — the viewer's included — draws the scrolled view.
// The host leaves copy-mode when the scroll returns to the bottom (-e), after
// an idle spell, and before a message is sent into the pane (mirrorhost) — or,
// when a tmux menu is open over it, once the menu closes.
//
// 🔴 NO FRESH SIGN-IN (internal/stepup) IS ASKED FOR, AND THAT IS A DECISION
// (the operator's), NOT AN OMISSION. A paste or a live key reaches the PROGRAM
// in the pane; a scroll cannot: while the pane is in copy-mode, tmux routes
// input to copy-mode itself, and the host sends only `copy-mode -e` and
// copy-mode's own scroll-up / scroll-down / cancel. Nothing typed, no byte to the program — so
// it is a view of output the viewer can already see, moved back in time. What
// is still enforced: the host advertised it AND is -interactive (copy-mode is a
// change its operator sees), owner isolation, the session/window allowlist on
// both halves, ClampScroll on both halves, and internal/api's rate limit.
//
// ⚠ SIDE EFFECTS a person should know (written in the PR too): copy-mode is
// visible to EVERY client attached to that pane, not just this viewer; and
// someone typing at the pane while it is in copy-mode types into copy-mode
// (its key table), not into the program, until it exits — at the bottom, on
// their own `q`, or on the host's idle cancel.

var (
	// ErrScrollUnsupported: the host did not advertise copy-mode scrolling (an
	// older muster-mirror, or a read-only host).
	ErrScrollUnsupported = errors.New("mirror: this host's muster-mirror cannot scroll a pane's history; upgrade it (and start it with -interactive)")
	// ErrScrollRefused wraps the host's own reason for refusing a scroll.
	ErrScrollRefused = errors.New("mirror: the host refused the scroll")
	// ErrScrollPaneInMode: the host refused because the pane is in another
	// tmux mode (a menu, choose-tree, view-mode) — Msg.Refusal
	// ScrollRefusalMode. Unlike ErrScrollUnsupported it passes: the pane
	// scrolls again once that mode is closed.
	ErrScrollPaneInMode = errors.New("mirror: the pane is in another mode (e.g. a tmux menu)")
	// ErrScrollTimeout: the host did not answer a scroll in time.
	ErrScrollTimeout = errors.New("mirror: the host did not answer the scroll in time")
	// ErrBadScroll: a line count ClampScroll refuses (0).
	ErrBadScroll = errors.New("mirror: a scroll must move at least one line")
)

// scrollTimeout bounds how long a scroll waits for the host's answer. A scroll
// is three short tmux commands; a host that takes longer than this is stuck,
// and the page simply stops waiting.
const scrollTimeout = 5 * time.Second

// ScrollWindow scrolls the active pane of one WINDOW of owner's host's session
// (its tmux window id; "" = the session's active window) through its history by
// lines (Msg.Lines: positive is back), and waits for the host's answer.
func (h *Hub) ScrollWindow(ctx context.Context, owner int64, host, session, window string, lines int) error {
	lines, ok := ClampScroll(lines)
	if !ok {
		return ErrBadScroll
	}
	h.mu.Lock()
	hc := h.hosts[hostKey{owner, host}]
	h.mu.Unlock()
	if hc == nil {
		return fmt.Errorf("%w (host %q)", ErrHostOffline, host)
	}
	windowID, _, err := hc.target(session, window)
	if err != nil {
		return err
	}
	if !hc.paneScroll || !hc.interactive {
		return fmt.Errorf("%w (host %q)", ErrScrollUnsupported, host)
	}
	hc.mu.Lock()
	hc.next++
	id := hc.next
	ch := make(chan error, 1)
	hc.scrolls[id] = ch
	hc.mu.Unlock()
	defer func() {
		hc.mu.Lock()
		delete(hc.scrolls, id)
		hc.mu.Unlock()
	}()
	if err := writeJSON(ctx, hc.ws, Msg{Type: TypeScroll, ID: id, Session: session, Window: windowID, Lines: lines}); err != nil {
		return fmt.Errorf("mirror: send the scroll to the host: %w", err)
	}
	t := time.NewTimer(scrollTimeout)
	defer t.Stop()
	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return ErrScrollTimeout
	}
}

// scrolled hands the host's answer to the scroll waiting on id. refusal
// (Msg.Refusal) decides which error a refusal is; the host's prose (why) is
// only ever carried along, never parsed.
func (hc *hostConn) scrolled(id uint32, why, refusal string) {
	hc.mu.Lock()
	ch := hc.scrolls[id]
	delete(hc.scrolls, id)
	hc.mu.Unlock()
	if ch == nil {
		return
	}
	ch <- scrollAnswer(why, refusal)
}

// scrollAnswer is the error a host's scrolled answer means.
func scrollAnswer(why, refusal string) error {
	if why == "" {
		return nil
	}
	switch refusal {
	case ScrollRefusalMode:
		return fmt.Errorf("%w: %s", ErrScrollPaneInMode, why)
	case ScrollRefusalUnsupported:
		return fmt.Errorf("%w: %s", ErrScrollUnsupported, why)
	}
	return fmt.Errorf("%w: %s", ErrScrollRefused, why)
}
