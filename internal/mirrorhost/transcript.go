package mirrorhost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/mirror"
)

// THE RENDERED VIEW's host half (tunnel v2): which sessions run Claude Code,
// a transcript stream per open view, and pasting a message into a Claude pane.

// reresolveEvery is how often (in Config.ViewPoll ticks) an open view
// re-resolves its pane: a /clear or restart (a new transcript), the CLI
// exiting, and the busy flag.
const reresolveEvery = 3

// readChunk bounds one poll's read of new transcript bytes.
const readChunk = 1 << 20

// claudeBackend is the backend's ClaudeBackend, when it has one AND this
// connection speaks v2.
func (h *hostSide) claudeBackend() (ClaudeBackend, bool) {
	if h.version < 2 {
		return nil, false
	}
	return ClaudeOf(h.c.cfg.Backend)
}

// resolveClaude is the sorted subset of sessions that run Claude Code (nil on v1
// or with a backend that cannot tell).
func (h *hostSide) resolveClaude(sessions []string) []string {
	cb, ok := h.claudeBackend()
	if !ok || len(sessions) == 0 {
		return nil
	}
	m := cb.ClaudePanes(h.ctx, sessions)
	out := make([]string, 0, len(m))
	for s := range m {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// pane re-resolves session's Claude pane now.
func (h *hostSide) pane(ctx context.Context, cb ClaudeBackend, session string) (ClaudePane, bool) {
	p, ok := cb.ClaudePanes(ctx, []string{session})[session]
	return p, ok
}

// window re-reads ONE window now: where a request that names a window finds
// its pane. A fresh read, at the moment of acting — never the list this host
// last sent, in which the window's pane may since have changed (a pane split,
// Claude Code exiting).
func (h *hostSide) window(ctx context.Context, session, window string) (WindowInfo, error) {
	wl, ok := WindowListerOf(h.c.cfg.Backend)
	if !ok {
		return WindowInfo{}, errors.New("this host does not list windows")
	}
	all, err := wl.Windows(ctx)
	if err != nil {
		return WindowInfo{}, err
	}
	w, ok := FindWindow(all, session, window)
	if !ok {
		return WindowInfo{}, fmt.Errorf("window %s of session %q is gone", window, session)
	}
	return w, nil
}

// claudePaneOf is how a watch or a paste finds the Claude Code pane of what m
// names: THAT WINDOW's active pane when m names a window — so two Claude Code
// windows of one session each stream their own transcript — else the session's
// active pane, as before.
func (h *hostSide) claudePaneOf(cb ClaudeBackend, m mirror.Msg) func(context.Context) (ClaudePane, bool) {
	if m.Window == "" {
		return func(ctx context.Context) (ClaudePane, bool) { return h.pane(ctx, cb, m.Session) }
	}
	return func(ctx context.Context) (ClaudePane, bool) {
		w, err := h.window(ctx, m.Session, m.Window)
		if err != nil || w.Claude == nil {
			return ClaudePane{}, false
		}
		return *w.Claude, true
	}
}

func (h *hostSide) unwatch(id uint32) {
	h.mu.Lock()
	cancel := h.watches[id]
	delete(h.watches, id)
	h.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// watch opens a transcript stream for m.Session and serves it until muster
// closes it, the connection ends, or the pane stops running Claude Code.
//
// 🔴 ONLY A SESSION THIS HOST LISTED, and only one whose pane resolves to Claude
// Code NOW — the same allowlist a terminal open passes. A watch is read-only:
// it reads one file and writes nothing anywhere.
func (h *hostSide) watch(m mirror.Msg) {
	fail := func(err error) {
		h.c.log.Printf("muster-mirror: watch %q: %v", m.Session, err)
		_ = h.send(mirror.Msg{Type: mirror.TypeClosed, ID: m.ID, Error: err.Error()})
	}
	cb, ok := h.claudeBackend()
	if !ok {
		fail(errors.New("this host cannot read Claude Code transcripts"))
		return
	}
	if err := h.allowed(m); err != nil {
		fail(err)
		return
	}
	ctx, cancel := context.WithCancel(h.ctx)
	h.mu.Lock()
	if _, dup := h.watches[m.ID]; dup || h.streams[m.ID] != nil {
		h.mu.Unlock()
		cancel()
		fail(fmt.Errorf("stream %d is already open", m.ID))
		return
	}
	h.watches[m.ID] = cancel
	h.mu.Unlock()
	go func() {
		err := h.tail(ctx, h.claudePaneOf(cb, m), m)
		h.unwatch(m.ID)
		msg := ""
		if err != nil && !errors.Is(err, context.Canceled) {
			msg = err.Error()
		}
		if h.ctx.Err() == nil {
			_ = h.send(mirror.Msg{Type: mirror.TypeClosed, ID: m.ID, Error: msg})
		}
	}()
}

// errNotClaudeNow ends a view whose pane no longer runs Claude Code.
var errNotClaudeNow = errors.New("Claude Code is not running in this session's active pane")

// tail serves one watch, resolving its pane with pane (claudePaneOf). It
// returns when ctx ends (nil after cancel), or with the reason the view cannot
// continue.
func (h *hostSide) tail(ctx context.Context, pane func(context.Context) (ClaudePane, bool), m mirror.Msg) error {
	p, ok := pane(ctx)
	if !ok {
		return errNotClaudeNow
	}
	var (
		f    *os.File
		off  int64
		busy = p.Busy
	)
	defer func() {
		if f != nil {
			_ = f.Close()
		}
	}()
	send := func(msgs []mirror.Msg) error {
		for _, b := range msgs {
			b.Type, b.ID, b.Gen = mirror.TypeEvents, m.ID, p.Gen
			if err := h.send(b); err != nil {
				return err
			}
		}
		return nil
	}
	// start (re)opens p's transcript and sends a Reset backfill — or, when the
	// resume names this very transcript at an offset it still has, nothing but
	// a marker that the resume was accepted.
	start := func(resumeGen string, resumeOff int64) error {
		if f != nil {
			_ = f.Close()
		}
		var err error
		if f, err = os.Open(p.Transcript); err != nil {
			return fmt.Errorf("open the transcript: %w", err)
		}
		st, err := f.Stat()
		if err != nil {
			return err
		}
		if resumeGen != "" && resumeGen == p.Gen && resumeOff >= 0 && resumeOff <= st.Size() {
			off = resumeOff
			return send([]mirror.Msg{{Offset: off, Busy: busy}})
		}
		lines, next, err := cctranscript.Backfill(f, st.Size(), mirror.BackfillWindow, mirror.MaxBackfillEvents)
		if err != nil {
			return err
		}
		off = next
		batches := mirror.BatchLines(lines, next)
		if len(batches) == 0 {
			batches = []mirror.Msg{{Offset: off}}
		}
		// The FIRST batch resets the view; the backfill is one transcript from
		// its window's start, and every batch's offset is in it.
		batches[0].Reset = true
		batches[len(batches)-1].Busy = busy
		return send(batches)
	}
	if err := start(m.Gen, m.Offset); err != nil {
		return err
	}

	t := time.NewTicker(h.c.cfg.ViewPoll)
	defer t.Stop()
	for tick := 1; ; tick++ {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		if tick%reresolveEvery == 0 {
			np, ok := pane(ctx)
			if !ok {
				if ctx.Err() != nil {
					return nil
				}
				return errNotClaudeNow
			}
			if np.Gen != p.Gen || np.Transcript != p.Transcript {
				// /clear, a restart, a resumed session: a different transcript.
				p, busy = np, np.Busy
				if err := start("", 0); err != nil {
					return err
				}
				continue
			}
			if np.Busy != busy {
				busy = np.Busy
				if err := send([]mirror.Msg{{Offset: off, Busy: busy}}); err != nil {
					return err
				}
			}
		}
		st, err := f.Stat()
		if err != nil {
			return fmt.Errorf("stat the transcript: %w", err)
		}
		if st.Size() < off {
			// Truncated or replaced in place: start over.
			if err := start("", 0); err != nil {
				return err
			}
			continue
		}
		for off < st.Size() {
			lines, next, err := cctranscript.ReadLines(f, off, st.Size(), readChunk)
			if err != nil {
				return fmt.Errorf("read the transcript: %w", err)
			}
			if next == off {
				break // a partial line: the rest arrives on a later poll
			}
			batches := mirror.BatchLines(lines, off)
			off = next
			if len(batches) == 0 {
				continue // lines with nothing to draw (attachments, hook records)
			}
			// The last batch's resume point is past every consumed line,
			// including trailing ones that drew nothing.
			batches[len(batches)-1].Offset = off
			for i := range batches {
				batches[i].Busy = busy
			}
			if err := send(batches); err != nil {
				return err
			}
		}
	}
}

// paneBackend is the backend's PaneBackend, when it has one AND this
// connection speaks v2 (paste does not exist on v1).
func (h *hostSide) paneBackend() (PaneBackend, bool) {
	if h.version < 2 {
		return nil, false
	}
	return PaneOf(h.c.cfg.Backend)
}

// panePaste is what this host advertises as Msg.PanePaste: it accepts a paste
// into a session's active pane whatever runs there. Only an -interactive host
// whose backend can do it — a read-only host advertises nothing it would
// refuse.
func (h *hostSide) panePaste() bool {
	_, ok := h.paneBackend()
	return ok && h.c.cfg.Interactive
}

// paste types m.Text into a pane of m.Session and answers pasted.
//
// 🔴 REFUSED UNLESS THIS HOST OPTED IN (-interactive), whatever muster asks —
// muster refuses it too, and this layer does not depend on it. Then only a
// listed session, and only text that passes mirror.SanitizePaste again.
//
// 🔴 WHICH PANE IS DECIDED HERE, BY WHAT muster ASKED FOR:
//   - without m.AnyPane (the rendered view's send, and every paste an older
//     muster sends): only a pane that runs Claude Code NOW, re-resolved here —
//     a prompt written for Claude is never typed into the shell it left behind;
//   - with m.AnyPane (the Terminal view's composer): the session's active pane
//     whatever runs in it. That is no more than this host already granted by
//     -interactive, which lets the same person type into the same pane.
func (h *hostSide) paste(m mirror.Msg) {
	reply := func(err error) {
		msg := ""
		if err != nil {
			msg = err.Error()
			h.c.log.Printf("muster-mirror: paste into %q%s refused: %v", m.Session, windowSuffix(m.Window), err)
		} else {
			h.c.log.Printf("muster-mirror: pasted a message into %q%s", m.Session, windowSuffix(m.Window))
		}
		_ = h.send(mirror.Msg{Type: mirror.TypePasted, ID: m.ID, Error: msg})
	}
	if !h.c.cfg.Interactive {
		reply(errors.New("this host is read-only (muster-mirror was not started with -interactive)"))
		return
	}
	if err := h.allowed(m); err != nil {
		reply(err)
		return
	}
	text, err := mirror.SanitizePaste(m.Text)
	if err != nil {
		reply(err)
		return
	}
	ctx, cancel := context.WithTimeout(h.ctx, 10*time.Second)
	defer cancel()
	if m.AnyPane {
		pb, ok := h.paneBackend()
		if !ok {
			reply(errors.New("this host cannot paste into a pane"))
			return
		}
		// 🔴 THE TARGET IS A PANE ID, AND THAT IS WHY A PASTE MOVES NOBODY'S
		// WINDOW: paste-buffer -t %<n> needs no client and no current window, so
		// the pane of a window nobody is looking at is reached exactly like the
		// one on screen. A named window's pane is read NOW (hostSide.window).
		pane, ok := "", false
		if m.Window != "" {
			w, err := h.window(ctx, m.Session, m.Window)
			if err != nil {
				reply(err)
				return
			}
			pane, ok = w.Pane, w.Pane != ""
		} else {
			pane, ok = pb.ActivePane(ctx, m.Session)
		}
		if !ok {
			reply(fmt.Errorf("session %q has no active pane to send to", m.Session))
			return
		}
		h.leaveCopyModeFor(pane)
		reply(pb.PastePane(ctx, pane, text))
		return
	}
	cb, ok := h.claudeBackend()
	if !ok {
		reply(errors.New("this host cannot see Claude Code panes"))
		return
	}
	p, ok := h.claudePaneOf(cb, m)(ctx)
	if !ok {
		reply(errNotClaudeNow)
		return
	}
	h.leaveCopyModeFor(p.Pane)
	reply(cb.Paste(ctx, p, text))
}

// leaveCopyModeFor cancels the copy-mode a drag-to-scroll on THIS connection
// put pane in, before a message is pasted into it (scroller.leaveForPaste):
// otherwise the message lands in the program but the pane goes on showing old
// history. Copy-mode the operator entered is never cancelled.
func (h *hostSide) leaveCopyModeFor(pane string) {
	if h.scroller != nil {
		h.scroller.leaveForPaste(pane)
	}
}
