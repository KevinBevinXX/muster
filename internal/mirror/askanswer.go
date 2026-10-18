package mirror

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/stepup"
)

// TYPING AN AskUserQuestion ANSWER INTO THE TERMINAL'S PICKER: the server half.
// The host reads the pane before and after every key (internal/askdrive) and
// answers with what happened; this side only carries the request and the
// outcome. Whether the CLI took THESE answers is not this answer's to say —
// the transcript's record of the call is (internal/api's question watcher).
//
// 🔴 -interactive HOSTS ONLY, on both halves: typing into the picker is a
// write into the session, like a paste. The step-up and owner gates are the
// caller's (internal/api), exactly as for a Send.

var (
	// ErrAskUnsupported: the host cannot type answers (an older or read-only
	// muster-mirror).
	ErrAskUnsupported = errors.New("mirror: this host's muster-mirror cannot type an answer into the terminal; update it (and start it with -interactive)")
	// ErrAskNoPicker: the question's picker is not on screen (answered or
	// dismissed in the terminal already).
	ErrAskNoPicker = errors.New("mirror: the question is no longer on the terminal's screen")
	// ErrAskTouched: someone is answering the question in the terminal.
	ErrAskTouched = errors.New("mirror: someone is answering this question in the terminal")
	// ErrAskLost: the picker changed while the answer was being typed; nothing
	// was submitted.
	ErrAskLost = errors.New("mirror: the terminal's picker changed while the answer was being typed, so it was not submitted")
	// ErrAskRefused wraps any other refusal by the host.
	ErrAskRefused = errors.New("mirror: the host could not type the answer in")
	// ErrAskTimeout: the host did not answer in time.
	ErrAskTimeout = errors.New("mirror: the host did not report the answer in time")
)

// askAnswerTimeout bounds one answer end to end: a few dozen keys, each
// waiting at most a few seconds for its screen.
const askAnswerTimeout = 60 * time.Second

// AnswerWindow types choices for qs into the AskUserQuestion picker in the
// Claude pane of one WINDOW of owner's host's session (its tmux window id; ""
// = the session's active window), and waits for the host's outcome.
func (h *Hub) AnswerWindow(ctx context.Context, owner int64, host, session, window string, qs []askq.Question, choices []askq.Choice) error {
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
	if !hc.askAnswer || !hc.interactive {
		return fmt.Errorf("%w (host %q)", ErrAskUnsupported, host)
	}
	// The same freshness gate as a paste, checked again here whatever the
	// caller did.
	if !stepup.Fresh(ctx, h.now()) {
		return ErrStepUpRequired
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
	m := Msg{Type: TypeAnswer, ID: id, Session: session, Window: windowID, Ask: &AskTyping{Questions: qs, Choices: choices}}
	if err := writeJSON(ctx, hc.ws, m); err != nil {
		return fmt.Errorf("mirror: send the answer to the host: %w", err)
	}
	t := time.NewTimer(askAnswerTimeout)
	defer t.Stop()
	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return ErrAskTimeout
	}
}

// answered hands the host's outcome to the answer waiting on id.
func (hc *hostConn) answered(id uint32, why, refusal string) {
	hc.mu.Lock()
	ch := hc.answers[id]
	delete(hc.answers, id)
	hc.mu.Unlock()
	if ch == nil {
		return
	}
	ch <- askAnswerError(why, refusal)
}

// askAnswerError is the error a host's answered means; the host's prose (why)
// is carried along, never parsed.
func askAnswerError(why, refusal string) error {
	if why == "" {
		return nil
	}
	switch refusal {
	case AskRefusalNoPicker:
		return fmt.Errorf("%w: %s", ErrAskNoPicker, why)
	case AskRefusalTouched:
		return fmt.Errorf("%w: %s", ErrAskTouched, why)
	case AskRefusalLost:
		return fmt.Errorf("%w: %s", ErrAskLost, why)
	case AskRefusalUnsupported:
		return fmt.Errorf("%w: %s", ErrAskUnsupported, why)
	}
	return fmt.Errorf("%w: %s", ErrAskRefused, why)
}

// AnswerWindow is Hub.AnswerWindow for the viewing principal's host.
func (s *Service) AnswerWindow(ctx context.Context, host, session, window string, qs []askq.Question, choices []askq.Choice) error {
	o, err := owner(ctx)
	if err != nil {
		return err
	}
	return s.hub.AnswerWindow(ctx, o, host, session, window, qs, choices)
}
