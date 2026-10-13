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

func (hc *hostConn) pasted(id uint32, why string) {
	hc.mu.Lock()
	ch := hc.pastes[id]
	delete(hc.pastes, id)
	hc.mu.Unlock()
	if ch == nil {
		return
	}
	if why != "" {
		ch <- fmt.Errorf("%w: %s", ErrPasteRefused, why)
		return
	}
	ch <- nil
}

// lookupClaude is owner's host serving session as a Claude pane over v2, or the
// refusal that says which of those it is not.
func (h *Hub) lookupClaude(owner int64, host, session string) (*hostConn, error) {
	h.mu.Lock()
	hc := h.hosts[hostKey{owner, host}]
	h.mu.Unlock()
	switch {
	case hc == nil:
		return nil, fmt.Errorf("%w (host %q)", ErrHostOffline, host)
	case !hc.has(session):
		return nil, fmt.Errorf("%w (host %q, session %q)", ErrSessionGone, host, session)
	case hc.version < 2:
		return nil, fmt.Errorf("%w (host %q)", ErrNoRenderedView, host)
	case !hc.isClaude(session):
		return nil, fmt.Errorf("%w (host %q, session %q)", ErrNotClaude, host, session)
	}
	return hc, nil
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
	hc, err := h.lookupClaude(owner, host, session)
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

	if err := writeJSON(ctx, hc.ws, Msg{Type: TypeWatch, ID: id, Session: session, Gen: gen, Offset: offset}); err != nil {
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

// Paste types text into owner's host's session's Claude pane and presses Enter
// — the way ccd hands a provisioned agent a prompt (a bracketed paste, then
// Enter) — and waits for the host to confirm it.
//
// 🔴 IT IS A WRITE INTO THE OPERATOR'S SESSION, SO IT IS REFUSED UNLESS THE
// REQUEST'S SIGN-IN IS FRESH (internal/stepup) AND THE HOST ADVERTISED
// -interactive — both here, whatever the caller (internal/api) already checked,
// and the host refuses a paste it did not opt into again.
func (h *Hub) Paste(ctx context.Context, owner int64, host, session, text string) error {
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
	hc, err := h.lookupClaude(owner, host, session)
	if err != nil {
		return err
	}
	if !hc.interactive {
		return fmt.Errorf("%w (host %q)", ErrInteractiveRefused, host)
	}
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
	if err := writeJSON(ctx, hc.ws, Msg{Type: TypePaste, ID: id, Session: session, Text: text}); err != nil {
		return fmt.Errorf("mirror: send the message to the host: %w", err)
	}
	t := time.NewTimer(pasteTimeout)
	defer t.Stop()
	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return ErrPasteTimeout
	}
}
