package mirror

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// DISMISSING AN AskUserQuestion PICKER FROM MUSTER: the server half. The host
// sends the pane ONE Escape, after reading that a picker is on it
// (internal/askdrive's Cancel), and reads the pane until the picker is gone;
// how the ask ended is the CLI's word (the transcript records it declined), as
// for an answer.
//
// 🔴 THE MESSAGE CARRIES NO KEYS (it names the window and nothing else): the
// host's one response to it is askdrive.KeyEscape. -interactive hosts that
// advertised it only (Msg.AskCancel). There is NO step-up gate — an operator
// decision: the person confirms in a dialog that names the window — but the
// owner and same-origin gates are the caller's (internal/api), as for an
// answer.

// ErrAskCancelUnsupported: the host cannot dismiss a picker (an older or
// read-only muster-mirror).
var ErrAskCancelUnsupported = errors.New("mirror: this host's muster-mirror cannot dismiss a question in the terminal; update it (and start it with -interactive)")

// askCancelNote is a successful dismissal's note (Msg.Text on ask-cancelled),
// carried on the outcome channel answers share; CancelAsk unwraps it.
type askCancelNote struct{ note string }

func (n askCancelNote) Error() string { return n.note }

// cancelled hands the host's dismissal outcome to the call waiting on id.
func (hc *hostConn) cancelled(id uint32, why, refusal, note string) {
	hc.mu.Lock()
	ch := hc.answers[id]
	delete(hc.answers, id)
	hc.mu.Unlock()
	if ch == nil {
		return
	}
	err := askAnswerError(why, refusal)
	if err == nil && note != "" {
		err = askCancelNote{note: note}
	}
	ch <- err
}

// askCancelTimeout bounds one dismissal end to end: two looks, one key, and
// the wait for the picker to go.
const askCancelTimeout = 20 * time.Second

// CancelAsk dismisses the AskUserQuestion picker in the Claude pane of one
// WINDOW of owner's host's session with one Escape, and waits for the host's
// outcome: nil means the Escape was sent, note what the host said about it
// ("" when the picker went; its note when it was still showing — no second
// Escape is sent). A refusal is one of the ask errors (ErrAskNoPicker,
// ErrAskUnrecognised, ErrAskBusy, ErrAskUnsupported, ErrAskRefused); one from
// before the host was asked wraps ErrAskNotSent.
func (h *Hub) CancelAsk(ctx context.Context, owner int64, host, session, window string) (note string, err error) {
	h.mu.Lock()
	hc := h.hosts[hostKey{owner, host}]
	h.mu.Unlock()
	if hc == nil {
		return "", fmt.Errorf("%w: %w (host %q)", ErrAskNotSent, ErrHostOffline, host)
	}
	windowID, _, err := hc.target(session, window)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrAskNotSent, err)
	}
	if !hc.askCancel || !hc.interactive {
		return "", fmt.Errorf("%w: %w (host %q)", ErrAskNotSent, ErrAskCancelUnsupported, host)
	}
	hc.mu.Lock()
	hc.next++
	id := hc.next
	ch := make(chan error, 1)
	hc.answers[id] = ch
	hc.mu.Unlock()
	defer func() {
		hc.mu.Lock()
		delete(hc.answers, id)
		hc.mu.Unlock()
	}()
	// The window, and nothing else: no field of this frame is typed.
	m := Msg{Type: TypeCancelAsk, ID: id, Session: session, Window: windowID}
	if err := writeJSON(ctx, hc.ws, m); err != nil {
		return "", fmt.Errorf("mirror: send the dismissal to the host: %w", err)
	}
	t := time.NewTimer(askCancelTimeout)
	defer t.Stop()
	select {
	case err := <-ch:
		var n askCancelNote
		if errors.As(err, &n) {
			return n.note, nil
		}
		return "", err
	case <-ctx.Done():
		return "", ctx.Err()
	case <-t.C:
		return "", ErrAskTimeout
	}
}

// CancelAsk is Hub.CancelAsk for the viewing principal's host.
func (s *Service) CancelAsk(ctx context.Context, host, session, window string) (string, error) {
	o, err := owner(ctx)
	if err != nil {
		return "", err
	}
	return s.hub.CancelAsk(ctx, o, host, session, window)
}
