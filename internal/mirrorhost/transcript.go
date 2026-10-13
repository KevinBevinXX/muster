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
	if !h.isListed(m.Session) {
		fail(fmt.Errorf("session %q is not in this host's latest list", m.Session))
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
		err := h.tail(ctx, cb, m)
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

// tail serves one watch. It returns when ctx ends (nil after cancel), or with
// the reason the view cannot continue.
func (h *hostSide) tail(ctx context.Context, cb ClaudeBackend, m mirror.Msg) error {
	p, ok := h.pane(ctx, cb, m.Session)
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
			np, ok := h.pane(ctx, cb, m.Session)
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

// paste types m.Text into m.Session's Claude pane and answers pasted.
//
// 🔴 REFUSED UNLESS THIS HOST OPTED IN (-interactive), whatever muster asks —
// muster refuses it too, and this layer does not depend on it. Then only a
// listed session whose pane runs Claude Code NOW (re-resolved here, so the
// paste goes to the pane id of the CLI, never to a shell the operator split
// beside it), and only text that passes mirror.SanitizePaste again.
func (h *hostSide) paste(m mirror.Msg) {
	reply := func(err error) {
		msg := ""
		if err != nil {
			msg = err.Error()
			h.c.log.Printf("muster-mirror: paste into %q refused: %v", m.Session, err)
		} else {
			h.c.log.Printf("muster-mirror: pasted a message into %q", m.Session)
		}
		_ = h.send(mirror.Msg{Type: mirror.TypePasted, ID: m.ID, Error: msg})
	}
	if !h.c.cfg.Interactive {
		reply(errors.New("this host is read-only (muster-mirror was not started with -interactive)"))
		return
	}
	if !h.isListed(m.Session) {
		reply(fmt.Errorf("session %q is not in this host's latest list", m.Session))
		return
	}
	text, err := mirror.SanitizePaste(m.Text)
	if err != nil {
		reply(err)
		return
	}
	cb, ok := h.claudeBackend()
	if !ok {
		reply(errors.New("this host cannot see Claude Code panes"))
		return
	}
	ctx, cancel := context.WithTimeout(h.ctx, 10*time.Second)
	defer cancel()
	p, ok := h.pane(ctx, cb, m.Session)
	if !ok {
		reply(errNotClaudeNow)
		return
	}
	reply(cb.Paste(ctx, p, text))
}
