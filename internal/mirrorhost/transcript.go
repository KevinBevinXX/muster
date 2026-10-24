package mirrorhost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
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

// hermesSource is the backend's HermesSource, when it has one AND this
// connection speaks v2 (a rendered view does not exist on v1).
func (h *hostSide) hermesSource() (*HermesSource, bool) {
	if h.version < 2 {
		return nil, false
	}
	hs, ok := capabilityOf[HermesSourcer](h.c.cfg.Backend)
	if !ok || hs.HermesSource() == nil {
		return nil, false
	}
	return hs.HermesSource(), true
}

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
	if err := h.allowed(m); err != nil {
		fail(err)
		return
	}
	cb, claudeOK := h.claudeBackend()
	src, hermesOK := h.hermesSource()
	if !claudeOK && !hermesOK {
		fail(errors.New("this host cannot read Claude Code transcripts"))
		return
	}
	// 🔴 WHICH STREAM SERVES THE WATCH IS DECIDED IN ITS OWN GOROUTINE, never
	// here: this runs on the connection's read loop, and reading the window
	// (a whole listing) here held every later message behind it.
	serve := func(ctx context.Context) error {
		if hermesOK {
			if _, isHermes := h.hermesWindow(ctx, m); isHermes {
				return h.hermesTail(ctx, src, m)
			}
		}
		if !claudeOK {
			return errNoHermesViewNow
		}
		return h.tail(ctx, h.claudePaneOf(cb, m), m)
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
	// The watch's open and end, like a terminal's attached/detached: how many
	// transcript streams a viewer really opened (the browser suite counts them),
	// and what it asked for — the transcript from the start, or a resume from an
	// offset. (A resume whose gen the host no longer has starts over with a
	// Reset; the stream itself says so, not this line.)
	from := "from the start"
	if m.Gen != "" && m.Offset > 0 {
		from = "resuming at " + strconv.FormatInt(m.Offset, 10)
	}
	h.c.log.Printf("muster-mirror: watching %q%s (%s, stream %d)", m.Session, windowSuffix(m.Window), from, m.ID)
	go func() {
		err := serve(ctx)
		h.unwatch(m.ID)
		h.c.log.Printf("muster-mirror: unwatched %q%s (stream %d)", m.Session, windowSuffix(m.Window), m.ID)
		msg := ""
		if err != nil && !errors.Is(err, context.Canceled) {
			msg = err.Error()
		}
		if h.ctx.Err() == nil {
			_ = h.send(mirror.Msg{Type: mirror.TypeClosed, ID: m.ID, Error: msg})
		}
	}()
}

// THE RENDERED VIEW's LOOP, shared by both streams — Claude Code's
// transcript file (tail) and Hermes's state.db (hermesTail): ONE send, ONE
// ticker, ONE re-resolve cadence, ONE busy marker. Each stream supplies what
// differs: how it (re)starts, how it re-resolves its pane, and how it polls.
type renderedView struct {
	// start sends the opening backfill (a Reset, or a resume marker).
	start func() error
	// reresolve re-reads the pane, every reresolveEvery polls; restarted
	// says it started the view over (and this tick polls nothing more).
	reresolve func(context.Context) (restarted bool, err error)
	// poll reads and sends what is new.
	poll func() error
}

// runView serves v until ctx ends (nil) or v fails.
func (h *hostSide) runView(ctx context.Context, v renderedView) error {
	if err := v.start(); err != nil {
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
			restarted, err := v.reresolve(ctx)
			if ctx.Err() != nil {
				return nil
			}
			if err != nil {
				return err
			}
			if restarted {
				continue
			}
		}
		if err := v.poll(); err != nil {
			return err
		}
	}
}

// viewSend frames batches as watch m's events, Gen from gen (read at each
// send: a stream that starts over on another conversation changes it).
func (h *hostSide) viewSend(m mirror.Msg, gen func() string) func([]mirror.Msg) error {
	return func(msgs []mirror.Msg) error {
		for _, b := range msgs {
			b.Type, b.ID, b.Gen = mirror.TypeEvents, m.ID, gen()
			if h.c.cfg.NoPromptIndex {
				for i := range b.Events {
					b.Events[i].Line = 0 // an older host names no prompt's place
				}
			}
			if err := h.send(b); err != nil {
				return err
			}
		}
		return nil
	}
}

// sendBusy sends a busy marker at off when nb differs from *busy.
func sendBusy(busy *bool, nb bool, off int64, send func([]mirror.Msg) error) error {
	if nb == *busy {
		return nil
	}
	*busy = nb
	return send([]mirror.Msg{{Offset: off, Busy: nb}})
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
		// subs is the view's subagents (subagents.go); nil on a host that
		// does not report them.
		subs     *subagentTracker
		withSubs = h.subagentReport()
	)
	defer func() {
		if f != nil {
			_ = f.Close()
		}
	}()
	send := h.viewSend(m, func() string { return p.Gen })
	// sendSubs sends the view's subagent list when it changed since the last
	// one sent (or none was): a message of its own, at the offset the view
	// already has.
	sendSubs := func() error {
		if subs == nil {
			return nil
		}
		list, ok := subs.changed()
		if !ok {
			return nil
		}
		return send([]mirror.Msg{{Offset: off, Busy: busy, SubagentList: true, Subagents: list}})
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
		resume := resumeGen != "" && resumeGen == p.Gen && resumeOff >= 0 && resumeOff <= st.Size()
		var (
			lines []cctranscript.Line
			next  int64
		)
		if !resume || withSubs {
			if lines, next, err = cctranscript.Backfill(f, st.Size(), mirror.BackfillWindow, mirror.MaxBackfillEvents); err != nil {
				return err
			}
		}
		if withSubs {
			// The subagents' parent signals come from the same window the
			// backfill draws — read on a resume too, which draws none of it,
			// so a card the page already has still learns its result.
			subs = newSubagentTracker(p.Transcript)
			subs.observe(cctranscript.Flatten(lines))
		}
		if resume {
			off = resumeOff
			if err := send([]mirror.Msg{{Offset: off, Busy: busy, SubagentList: withSubs}}); err != nil {
				return err
			}
			return sendSubs()
		}
		off = next
		// 🔴 Redacted before it is framed (redact_stream.go).
		redactLines(lines)
		batches := mirror.BatchLines(lines, next)
		if len(batches) == 0 {
			batches = []mirror.Msg{{Offset: off}}
		}
		// The FIRST batch resets the view; the backfill is one transcript from
		// its window's start, and every batch's offset is in it.
		batches[0].Reset = true
		// And says, before the view draws one Agent call, that this host
		// reports subagents: the calls are then drawn as cards. The list itself
		// follows as its own message.
		batches[0].SubagentList = withSubs
		batches[len(batches)-1].Busy = busy
		if err := send(batches); err != nil {
			return err
		}
		if err := sendSubs(); err != nil || h.c.cfg.NoPromptIndex {
			return err
		}
		// Then the prompt index: every prompt in the file up to where the
		// backfill ended (later ones arrive as live events), so the message
		// index can list the ones the window above never loaded.
		//
		// ⚠ A FAILED SCAN COSTS THE INDEX, NOT THE VIEW: the view then lists
		// the prompts it has, as it does for a host that predates the index.
		refs, err := cctranscript.Prompts(f, next)
		if err != nil {
			h.c.log.Printf("muster-mirror: watch %q: index the prompts: %v", m.Session, err)
			return nil
		}
		redactPrompts(refs)
		refs, more := mirror.BoundPrompts(refs)
		return send([]mirror.Msg{{Offset: off, Busy: busy, PromptIndex: true, Prompts: refs, PromptsMore: more}})
	}
	return h.runView(ctx, renderedView{
		start: func() error { return start(m.Gen, m.Offset) },
		reresolve: func(ctx context.Context) (bool, error) {
			np, ok := pane(ctx)
			if !ok {
				return false, errNotClaudeNow
			}
			if np.Gen != p.Gen || np.Transcript != p.Transcript {
				// /clear, a restart, a resumed session: a different transcript.
				p, busy = np, np.Busy
				return true, start("", 0)
			}
			if err := sendBusy(&busy, np.Busy, off, send); err != nil {
				return false, err
			}
			// The subagents' own files move without the parent's: read them
			// on the re-resolve cadence.
			return false, sendSubs()
		},
		poll: func() error {
			st, err := f.Stat()
			if err != nil {
				return fmt.Errorf("stat the transcript: %w", err)
			}
			if st.Size() < off {
				// Truncated or replaced in place: start over.
				return start("", 0)
			}
			for off < st.Size() {
				lines, next, err := cctranscript.ReadLines(f, off, st.Size(), readChunk)
				if err != nil {
					return fmt.Errorf("read the transcript: %w", err)
				}
				if next == off {
					break // a partial line: the rest arrives on a later poll
				}
				if subs != nil {
					subs.observe(cctranscript.Flatten(lines))
				}
				// 🔴 Redacted before it is framed (redact_stream.go).
				redactLines(lines)
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
			return nil
		},
	})
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
//     whatever runs in it — EXCEPT a pane whose process tree holds Hermes,
//     which is refused. Otherwise that is no more than this host already
//     granted by -interactive, which lets the same person type into the same
//     pane.
//
// 🔴 EITHER WAY THE KEYS GO THROUGH typeInto (hermes.go), the one way this host
// types into a pane: the Hermes paste guard refuses a pane whose process tree
// holds Hermes, a multiplexer client or a process it cannot read — a Claude
// Code pane too, when Hermes was started in it (`claude &`, then `hermes`).
func (h *hostSide) paste(m mirror.Msg) {
	reply := func(err error) {
		msg := ""
		if err != nil {
			msg = err.Error()
			h.c.log.Printf("muster-mirror: paste into %q%s refused: %v", m.Session, windowSuffix(m.Window), err)
		} else {
			h.c.log.Printf("muster-mirror: pasted a message into %q%s", m.Session, windowSuffix(m.Window))
		}
		_ = h.send(mirror.Msg{Type: mirror.TypePasted, ID: m.ID, Error: msg, Refusal: pasteRefusalOf(err)})
	}
	if m.Runtime == mirror.RuntimeHermes {
		// The gated send (hermes_send.go): its own checks, replies and
		// refusal kinds, and the one deliberate exception to the guard.
		h.hermesPaste(m)
		return
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
		reply(h.typeInto(ctx, pane, func() error { return pb.PastePane(ctx, pane, text) }))
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
	// 🔴 THE CLAUDE PROMPT GUARD (claude_guard.go): never into a pane waiting
	// on the person. Checked once here, so a refusal touches nothing (not even
	// this connection's copy-mode), and again immediately before the paste and
	// before its Enter, inside the keys.
	before := func(ctx context.Context) error { return h.claudeReady(ctx, cb, m, p.Pane, true) }
	beforeEnter := func(ctx context.Context) error { return h.claudeReady(ctx, cb, m, p.Pane, false) }
	if err := before(ctx); err != nil {
		reply(err)
		return
	}
	// 🔴 ONLY A CHECKED PASTE: a backend without one is refused — a plain
	// Paste presses an Enter nothing checked.
	cp, ok := capabilityOf[ClaudeCheckedPaster](h.c.cfg.Backend)
	if !ok {
		reply(ErrClaudeUncheckable)
		return
	}
	reply(h.typeInto(ctx, p.Pane, func() error { return cp.PasteChecked(ctx, p, text, before, beforeEnter) }))
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
