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
	// ErrAskUnrecognised: the host could not read the terminal's picker —
	// Claude Code's layout has probably changed. Nothing was typed.
	ErrAskUnrecognised = errors.New("mirror: the host could not recognise the terminal's picker (Claude Code may have changed)")
	// ErrAskBusy: another answer is being typed into that terminal. Nothing
	// was typed.
	ErrAskBusy = errors.New("mirror: another answer is being typed into that terminal")
	// ErrAskUnreadable: the host could not read the question off the
	// terminal's screen as the one muster registered — NOT someone answering
	// it there. Alone, nothing was typed; with ErrAskLost, keys were typed
	// before it went out of reading and nothing was submitted.
	ErrAskUnreadable = errors.New("mirror: the host could not read this question off the terminal's screen")
	// ErrAskNotSent wraps an AnswerWindow error from BEFORE the answer was
	// sent to the host: nothing can be being typed.
	ErrAskNotSent = errors.New("mirror: the answer was not sent to the host")
)

// askKeysError is a host's refusal with how many keys of the answer's plan
// the picker had received (Msg.Keys).
type askKeysError struct {
	keys int
	err  error
}

func (e askKeysError) Error() string { return e.err.Error() }
func (e askKeysError) Unwrap() error { return e.err }

// WithAskKeys is err carrying keys, as a host's refusal reports them (AskKeys):
// how a host's outcome reaches AnswerWindow's caller, and how a stand-in for
// the hub (a test's) reports one. nil stays nil, and keys 0 adds nothing.
func WithAskKeys(err error, keys int) error {
	if err == nil || keys <= 0 {
		return err
	}
	return askKeysError{keys: keys, err: err}
}

// AskKeys is how many keys of the answer's plan the host reported the picker
// had received when AnswerWindow's err ended it: 0 when it reported none (an
// older host, nothing typed) or err is not the host's report.
func AskKeys(err error) int {
	var k askKeysError
	if errors.As(err, &k) {
		return k.keys
	}
	return 0
}

// AskOutcomeKnown reports whether err (from AnswerWindow) means the host is
// NOT typing any more: nil (typed), a refusal the host reported, or an
// answer never sent. False — the host may still be typing — for a timeout, a
// host that dropped after the answer was sent, or a send that failed part way.
func AskOutcomeKnown(err error) bool {
	if err == nil {
		return true
	}
	for _, e := range []error{ErrAskNotSent, ErrAskNoPicker, ErrAskTouched, ErrAskLost, ErrAskUnsupported, ErrAskUnrecognised, ErrAskBusy, ErrAskUnreadable, ErrAskCancelUnsupported, ErrAskRefused,
		ErrHermesApprovalGone, ErrHermesUnreachable} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// askAnswerTimeout bounds one answer end to end: a few dozen keys, each
// waiting at most a few seconds for its screen.
const askAnswerTimeout = 60 * time.Second

// AnswerWindow types choices for qs into the AskUserQuestion picker in the
// Claude pane of one WINDOW of owner's host's session (its tmux window id; ""
// = the session's active window), and waits for the host's outcome. prior is
// what an earlier answer is known to have left in that picker (nil: nothing);
// a refusal carries how many keys the picker has received (AskKeys).
func (h *Hub) AnswerWindow(ctx context.Context, owner int64, host, session, window string, qs []askq.Question, choices []askq.Choice, prior *askq.Progress) error {
	ch, id, hc, m, err := h.prepareAnswer(ctx, owner, host, session, window, qs, choices)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrAskNotSent, err)
	}
	m.Ask.Prior = prior
	defer func() {
		hc.mu.Lock()
		delete(hc.answers, id)
		hc.mu.Unlock()
	}()
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

// prepareAnswer is AnswerWindow's gates, and the answer frame with the
// channel its outcome arrives on (registered in hc.answers).
func (h *Hub) prepareAnswer(ctx context.Context, owner int64, host, session, window string, qs []askq.Question, choices []askq.Choice) (chan error, uint32, *hostConn, Msg, error) {
	h.mu.Lock()
	hc := h.hosts[hostKey{owner, host}]
	h.mu.Unlock()
	if hc == nil {
		return nil, 0, nil, Msg{}, fmt.Errorf("%w (host %q)", ErrHostOffline, host)
	}
	windowID, _, err := hc.target(session, window)
	if err != nil {
		return nil, 0, nil, Msg{}, err
	}
	if !hc.askAnswer || !hc.interactive {
		return nil, 0, nil, Msg{}, fmt.Errorf("%w (host %q)", ErrAskUnsupported, host)
	}
	// The same freshness gate as a paste, checked again here whatever the
	// caller did.
	if !stepup.Fresh(ctx, h.now()) {
		return nil, 0, nil, Msg{}, ErrStepUpRequired
	}
	hc.mu.Lock()
	hc.next++
	id := hc.next
	ch := make(chan error, 1)
	hc.answers[id] = ch
	hc.mu.Unlock()
	return ch, id, hc, Msg{Type: TypeAnswer, ID: id, Session: session, Window: windowID, Ask: &AskTyping{Questions: qs, Choices: choices}}, nil
}

// answered hands the host's outcome to the answer waiting on id: keys is how
// many keys of its plan the picker had received (Msg.Keys).
func (hc *hostConn) answered(id uint32, why, refusal string, keys int) {
	hc.mu.Lock()
	ch := hc.answers[id]
	delete(hc.answers, id)
	hc.mu.Unlock()
	if ch == nil {
		return
	}
	ch <- WithAskKeys(askAnswerError(why, refusal), keys)
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
	case AskRefusalUnrecognised:
		return fmt.Errorf("%w: %s", ErrAskUnrecognised, why)
	case AskRefusalBusy:
		return fmt.Errorf("%w: %s", ErrAskBusy, why)
	case AskRefusalUnreadable:
		return fmt.Errorf("%w: %s", ErrAskUnreadable, why)
	case AskRefusalUnreadableTyped:
		return fmt.Errorf("%w: %w: %s", ErrAskUnreadable, ErrAskLost, why)
	case AskRefusalHermesGone:
		return fmt.Errorf("%w: %s", ErrHermesApprovalGone, why)
	case AskRefusalHermesUnreachable:
		return fmt.Errorf("%w: %s", ErrHermesUnreachable, why)
	case AskRefusalHermesUnknown:
		return fmt.Errorf("%w: %s", ErrHermesAnswerUnknown, why)
	}
	return fmt.Errorf("%w: %s", ErrAskRefused, why)
}

// AnswerWindow is Hub.AnswerWindow for the viewing principal's host.
func (s *Service) AnswerWindow(ctx context.Context, host, session, window string, qs []askq.Question, choices []askq.Choice, prior *askq.Progress) error {
	o, err := owner(ctx)
	if err != nil {
		return err
	}
	return s.hub.AnswerWindow(ctx, o, host, session, window, qs, choices, prior)
}
