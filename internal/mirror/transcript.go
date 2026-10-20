package mirror

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/stepup"
)

// THE RENDERED VIEW's server half (tunnel v2): a transcript stream per open
// view, and pasting a message into a Claude pane.
//
// 🔴 NOTHING HERE IS PERSISTED. A transcript batch passes from the host's
// connection to the one browser socket that asked for it, through a bounded
// buffer, and is gone: no table, no cache, no copy on another owner's path. The
// hub is in-memory by design (see Hub), and the transcript is the host's file,
// read on the host, for as long as somebody is looking at it.
//
// 🔴 OWNER-SCOPED EXACTLY LIKE A TERMINAL: every entry point looks the host up
// by (owner, host), so another owner's host answers ErrHostOffline, the same
// as a host that does not exist.

var (
	// ErrNoRenderedView: the host speaks tunnel v1, which carries no transcript.
	ErrNoRenderedView = errors.New("mirror: this host's muster-mirror predates the rendered view (tunnel v1); upgrade it, or use the Terminal")
	// ErrNotClaude: the host does not report Claude Code in the session's pane.
	ErrNotClaude = errors.New("mirror: the host reports no Claude Code session in this tmux session's pane")
	// ErrTooManyWatches: the host already has MaxWatchesPerHost views open.
	ErrTooManyWatches = fmt.Errorf("mirror: this host already has %d rendered views open; close one first", MaxWatchesPerHost)
	// ErrViewOverrun: the browser fell streamBuffer batches behind a view.
	ErrViewOverrun = errors.New("mirror: the transcript arrived faster than the browser read it; reconnect to resume")
	// ErrPasteTimeout: the host did not confirm a paste in time.
	ErrPasteTimeout = errors.New("mirror: the host did not confirm the message in time")
	// ErrPasteRefused wraps the host's own reason for refusing a paste.
	ErrPasteRefused = errors.New("mirror: the host refused the message")
	// ErrPanePasteUnsupported: the session's pane does not run Claude Code and
	// the host's muster-mirror predates pasting into any other pane.
	ErrPanePasteUnsupported = errors.New("mirror: this host's muster-mirror can only send to a Claude Code pane; upgrade it to send into this session, or use the Interactive terminal")
	// ErrBadPaste: a message SanitizePaste refuses (empty, oversized, or
	// carrying a control character).
	ErrBadPaste = errors.New("mirror: the message cannot be pasted")
)

// pasteTimeout bounds how long a paste waits for the host's confirmation.
const pasteTimeout = 15 * time.Second

// watch is one open transcript stream: the batches the host sent and the
// browser has not read yet.
type watch struct {
	out  chan Msg
	done chan struct{}
	once sync.Once
	err  error
}

func (w *watch) finish(err error) {
	w.once.Do(func() {
		w.err = err
		close(w.done)
	})
}

// deliverEvents hands one events message to its watch, or ends the watch with
// ErrViewOverrun when the browser has fallen streamBuffer batches behind — never
// blocking the host's read loop, which serves every stream on the host.
func (hc *hostConn) deliverEvents(m Msg) {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	w := hc.watches[m.ID]
	if w == nil {
		return
	}
	select {
	case w.out <- m:
	default:
		w.finish(ErrViewOverrun)
	}
}

func (hc *hostConn) pasted(id uint32, why, refusal string) {
	hc.mu.Lock()
	ch := hc.pastes[id]
	delete(hc.pastes, id)
	hc.mu.Unlock()
	if ch == nil {
		return
	}
	if why != "" {
		ch <- &PasteRefusedError{Why: why, Refusal: refusal}
		return
	}
	ch <- nil
}

// PasteRefusedError is ErrPasteRefused with the host's reason (Why, its prose,
// never parsed) and the kind it named (Refusal, a PasteRefusal* or "").
type PasteRefusedError struct {
	Why     string
	Refusal string
}

func (e *PasteRefusedError) Error() string { return ErrPasteRefused.Error() + ": " + e.Why }
func (e *PasteRefusedError) Unwrap() error { return ErrPasteRefused }

// PasteGuardRefusal is the host's paste-guard refusal err carries (one of the
// PasteRefusal* kinds), or "" when it is not one.
func PasteGuardRefusal(err error) string {
	var pr *PasteRefusedError
	if !errors.As(err, &pr) {
		return ""
	}
	switch pr.Refusal {
	case PasteRefusalHermes, PasteRefusalMultiplexer, PasteRefusalUnchecked:
		return pr.Refusal
	}
	return ""
}

// lookupClaude is owner's host serving (session, window) as a Claude pane over
// v2 and the window id to send it (hostConn.target), or the refusal that says
// which of those it is not.
func (h *Hub) lookupClaude(owner int64, host, session, window string) (*hostConn, string, error) {
	h.mu.Lock()
	hc := h.hosts[hostKey{owner, host}]
	h.mu.Unlock()
	if hc == nil {
		return nil, "", fmt.Errorf("%w (host %q)", ErrHostOffline, host)
	}
	id, claude, err := hc.target(session, window)
	switch {
	case err != nil:
		return nil, "", err
	case hc.version < 2:
		return nil, "", fmt.Errorf("%w (host %q)", ErrNoRenderedView, host)
	case !claude:
		return nil, "", fmt.Errorf("%w (host %q, session %q)", ErrNotClaude, host, session)
	}
	return hc, id, nil
}

// Watch streams owner's host's session's Claude Code transcript to out until ctx
// ends (nil), the host ends it, or the connection drops. gen and offset ask to
// RESUME (the browser reconnecting with what it already drew); the host honours
// them only when gen is still the session's transcript, and otherwise starts
// with a Reset backfill.
//
// Each batch out receives is an events Msg exactly as the host sent it
// (bounded by MaxBatchEvents / MaxBatchBytes, every field capped by
// cctranscript). An out error ends the watch.
func (h *Hub) Watch(ctx context.Context, owner int64, host, session, gen string, offset int64, out func(Msg) error) error {
	return h.WatchWindow(ctx, owner, host, session, "", gen, offset, out)
}

// WatchWindow is Watch on one WINDOW of the session (its tmux window id), or —
// window == "" — on the session's active window: the transcript of the Claude
// Code session running in THAT window's pane.
func (h *Hub) WatchWindow(ctx context.Context, owner int64, host, session, window, gen string, offset int64, out func(Msg) error) error {
	hc, windowID, err := h.lookupClaude(owner, host, session, window)
	if err != nil {
		return err
	}
	if offset < 0 {
		offset = 0
	}
	hc.mu.Lock()
	if len(hc.watches) >= MaxWatchesPerHost {
		hc.mu.Unlock()
		return fmt.Errorf("%w (host %q)", ErrTooManyWatches, host)
	}
	hc.next++
	id := hc.next
	w := &watch{out: make(chan Msg, streamBuffer), done: make(chan struct{})}
	hc.watches[id] = w
	hc.mu.Unlock()
	defer func() {
		hc.mu.Lock()
		delete(hc.watches, id)
		hc.mu.Unlock()
	}()

	if err := writeJSON(ctx, hc.ws, Msg{Type: TypeWatch, ID: id, Session: session, Window: windowID, Gen: gen, Offset: offset}); err != nil {
		return fmt.Errorf("mirror: ask the host to stream the transcript: %w", err)
	}
	defer func() { _ = writeJSON(hc.ctx, hc.ws, Msg{Type: TypeClose, ID: id}) }()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-w.done:
			// Batches the host sent before it ended the stream are still owed.
			for {
				select {
				case m := <-w.out:
					if err := out(m); err != nil {
						return err
					}
				default:
					return w.err
				}
			}
		case m := <-w.out:
			if err := out(m); err != nil {
				return err
			}
		}
	}
}

// PasteTarget is WHICH pane of a session a paste may land in.
type PasteTarget int

const (
	// PasteClaude: only a pane the host resolves as Claude Code — the rendered
	// view's composer. A session whose pane runs anything else is refused, on
	// muster and again on the host: a prompt written for Claude must never be
	// typed into a shell.
	PasteClaude PasteTarget = iota
	// PasteActivePane: the session's active pane, whatever runs in it — the
	// Terminal view's composer, where the person is looking at that pane.
	PasteActivePane
)

// lookupPaste is owner's host for a paste into (session, window) at target, the
// window id to send it, and whether to ask the host for the active pane
// (Msg.AnyPane) — or the refusal.
//
// An older muster-mirror (no Msg.PanePaste) is never sent AnyPane: for a
// session it reports as Claude Code its Claude-only paste reaches the very pane
// PasteActivePane means, so that is used; for any other session the answer is
// ErrPanePasteUnsupported, here, without asking the host.
func (h *Hub) lookupPaste(owner int64, host, session, window string, target PasteTarget) (*hostConn, string, bool, error) {
	if target != PasteActivePane {
		hc, id, err := h.lookupClaude(owner, host, session, window)
		return hc, id, false, err
	}
	h.mu.Lock()
	hc := h.hosts[hostKey{owner, host}]
	h.mu.Unlock()
	if hc == nil {
		return nil, "", false, fmt.Errorf("%w (host %q)", ErrHostOffline, host)
	}
	id, claude, err := hc.target(session, window)
	switch {
	case err != nil:
		return nil, "", false, err
	case !hc.interactive:
		// Before "can it paste into this pane": a read-only host's answer is
		// that it is read-only, not that it wants upgrading.
		return nil, "", false, fmt.Errorf("%w (host %q)", ErrInteractiveRefused, host)
	case hc.panePaste:
		return hc, id, true, nil
	case hc.version >= 2 && claude:
		return hc, id, false, nil
	}
	return nil, "", false, fmt.Errorf("%w (host %q, session %q)", ErrPanePasteUnsupported, host, session)
}

// Paste types text into a pane of owner's host's session and presses Enter —
// the way ccd hands a provisioned agent a prompt (a bracketed paste, then
// Enter) — and waits for the host to confirm it. target says which pane it may
// be (PasteTarget).
//
// 🔴 IT IS A WRITE INTO THE OPERATOR'S SESSION, SO IT IS REFUSED UNLESS THE
// REQUEST'S SIGN-IN IS FRESH (internal/stepup) AND THE HOST ADVERTISED
// -interactive — both here, whatever the caller (internal/api) already checked,
// and the host refuses a paste it did not opt into again.
func (h *Hub) Paste(ctx context.Context, owner int64, host, session, text string, target PasteTarget) error {
	return h.PasteWindow(ctx, owner, host, session, "", text, target)
}

// PasteWindow is Paste into one WINDOW of the session (its tmux window id), or
// — window == "" — into the session's active window.
//
// 🔴 A PASTE NEVER MOVES ANYONE'S WINDOW. The host pastes into a PANE id
// (tmux's paste-buffer -t %<n>), which needs no client and no current window,
// so a message can be sent to a window nobody is looking at.
//
// opts: ExpectTurn makes it a QUEUED paste, refused (ErrSuperseded) unless the
// window's turn is still the one it was decided on; ReportWindow reports the
// window id it resolved to.
func (h *Hub) PasteWindow(ctx context.Context, owner int64, host, session, window, text string, target PasteTarget, opts ...PasteOption) error {
	o := PasteOptionsOf(opts...)
	// 🔴 A FRESH SIGN-IN, checked HERE as well as by internal/api — the same
	// gate, on the same clock, as an interactive Attach (Service.Attach): a
	// paste types into the operator's session exactly as a keystroke does. A
	// context with no sign-in time is never fresh.
	if !stepup.Fresh(ctx, h.now()) {
		return ErrStepUpRequired
	}
	text, err := SanitizePaste(text)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBadPaste, err)
	}
	hc, windowID, anyPane, err := h.lookupPaste(owner, host, session, window, target)
	if err != nil {
		return err
	}
	if !hc.interactive {
		return fmt.Errorf("%w (host %q)", ErrInteractiveRefused, host)
	}
	if o.Window != nil {
		*o.Window = windowID
	}
	// The window's status time NOW, before the paste is written: the turn
	// this paste starts is a status write newer than it (turns.go). Under
	// turnsMu, a queued paste is refused here if another paste got in first.
	held, err := h.beginPaste(owner, host, session, windowID, hc.statusSince(session, windowID), o.Expect)
	if err != nil {
		return err
	}
	err = h.pasteTo(ctx, hc, session, windowID, text, anyPane)
	h.endPaste(owner, host, session, windowID, held, err == nil || errors.Is(err, ErrPasteUnconfirmed))
	return err
}

// PasteOption is an option of PasteWindow.
type PasteOption func(*PasteOptions)

// PasteOptions is what a list of PasteOptions asks for: Expect, the turn a
// queued paste was decided on (nil: a person's paste); Window, where to report
// the window id the paste resolved to (nil: nowhere).
type PasteOptions struct {
	Expect *Turn
	Window *string
}

// PasteOptionsOf applies opts. A PasteWindow implementation reads its options
// through it (the hub, and internal/api's test fakes).
func PasteOptionsOf(opts ...PasteOption) PasteOptions {
	var o PasteOptions
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// Turn is the window's turn a queued message was decided on: the AgentState's
// Since and Pasted that the send queue read when it decided to send.
type Turn struct {
	Since, Pasted int64
}

// ExpectTurn makes the paste a QUEUED one, decided on turn t: the hub refuses
// it with ErrSuperseded — before anything reaches the host — when another
// paste into the window is in flight, or the window's Since or Pasted is no
// longer t's (another paste, or a status write, came between the decision and
// this paste). A person's own Send carries no turn and is never refused so.
func ExpectTurn(t Turn) PasteOption { return func(o *PasteOptions) { o.Expect = &t } }

// ReportWindow has PasteWindow write the window id it resolved to into dst
// (window "" being the session's active window), once it has resolved it.
func ReportWindow(dst *string) PasteOption { return func(o *PasteOptions) { o.Window = dst } }

// ErrSuperseded refuses a queued paste whose turn is no longer the one it was
// decided on (ExpectTurn). Nothing was sent to the host; the message can be
// queued again and decided on afresh. It is a *SupersededError.
var ErrSuperseded = errors.New("mirror: superseded by a newer turn: something else was pasted into the window (or its status changed) after this message was decided on, so it was not pasted")

// SupersededError is ErrSuperseded with the window's Pasted at the refusal:
// what the message must now wait behind.
type SupersededError struct {
	Pasted int64
}

func (e *SupersededError) Error() string { return ErrSuperseded.Error() }
func (e *SupersededError) Unwrap() error { return ErrSuperseded }

// statusSince is the StatusSince hc lists for (session, window id), 0 for none.
func (hc *hostConn) statusSince(session, id string) int64 {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	for _, w := range hc.windows {
		if w.Session == session && w.ID == id {
			return w.StatusSince
		}
	}
	return 0
}

// pasteTo hands text to hc for (session, window id) and waits for the answer.
func (h *Hub) pasteTo(ctx context.Context, hc *hostConn, session, windowID, text string, anyPane bool) error {
	hc.mu.Lock()
	hc.next++
	id := hc.next
	ch := make(chan error, 1)
	hc.pastes[id] = ch
	hc.mu.Unlock()
	defer func() {
		hc.mu.Lock()
		delete(hc.pastes, id)
		hc.mu.Unlock()
	}()
	if err := writeJSON(ctx, hc.ws, Msg{Type: TypePaste, ID: id, Session: session, Window: windowID, Text: text, AnyPane: anyPane}); err != nil {
		return fmt.Errorf("mirror: send the message to the host: %w", err)
	}
	// From here the host has the message: an answer that never comes is
	// UNCONFIRMED (ErrPasteUnconfirmed), not "not sent" — it may have landed.
	t := time.NewTimer(pasteTimeout)
	defer t.Stop()
	select {
	case err := <-ch:
		if errors.Is(err, ErrHostDisconnected) {
			return unconfirmed{err}
		}
		return err
	case <-ctx.Done():
		return unconfirmed{ctx.Err()}
	case <-t.C:
		return unconfirmed{ErrPasteTimeout}
	}
}
