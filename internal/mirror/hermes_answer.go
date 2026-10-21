package mirror

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/ZacxDev/muster/internal/stepup"
)

// ANSWERING A HERMES APPROVAL FROM MUSTER (Hermes Phase 4, docs/design/
// hermes.md, "Questions and approvals"): the server half. The host answers
// through Hermes's GATEWAY VIEWER (internal/hermeshost's Viewer.Respond),
// never by typing into the pane — a paste+Enter on Hermes's approval modal
// approves "once" whatever was pasted (S11). This side carries the request
// and the outcome.
//
// 🔴 ONLY THE APPROVAL MUSTER SHOWED, WITH AN ANSWER IT OFFERED. The hub
// re-reads the window's listing at the moment of sending: it must still be a
// Hermes window waiting on an approval with exactly this PromptID, offering
// this choice (Pending.Offers) — "once" or "deny", never "session" or
// "always". The host checks again against what its viewer holds NOW.
//
// 🔴 THE GATES COPY THE CLAUDE ASK-ANSWER PATH (askanswer.go's
// prepareAnswer): an -interactive v2 host that advertised the capability
// (here Msg.HermesAnswer, -hermes-answer), and a fresh sign-in
// (internal/stepup); the owner and same-origin gates are the caller's
// (internal/api's handleMirrorAskAnswer and its Hermes twin).

// HermesApprovalAnswer is a Hermes approval's answer on the wire
// (Msg.HermesApproval).
type HermesApprovalAnswer struct {
	PromptID string `json:"promptId"`
	Choice   string `json:"choice"`
}

// The refusals a host names on answered for a Hermes approval (Msg.Refusal).
const (
	// AskRefusalHermesGone: the approval is no longer pending in Hermes
	// (answered in the terminal, timed out, the turn ended). Nothing was
	// answered.
	AskRefusalHermesGone = "hermes-gone"
	// AskRefusalHermesUnreachable: the host could not reach Hermes's gateway
	// (none running, not the pinned commit, a control path that failed its
	// check). Nothing was answered.
	AskRefusalHermesUnreachable = "hermes-unreachable"
	// AskRefusalHermesUnknown: the answer reached Hermes's gateway, which did
	// not say how it went. It may have been answered.
	AskRefusalHermesUnknown = "hermes-unknown"
)

var (
	// ErrHermesAnswerUnsupported: the host does not answer Hermes approvals
	// (an older or read-only muster-mirror, or one without -hermes-answer).
	ErrHermesAnswerUnsupported = errors.New("mirror: this host's muster-mirror does not answer Hermes approvals (it needs -interactive and -hermes-answer); answer it in the terminal")
	// ErrHermesApprovalGone: the window is no longer waiting on that
	// approval, or not with that choice. Nothing was answered.
	ErrHermesApprovalGone = errors.New("mirror: Hermes is no longer waiting on that approval")
	// ErrHermesUnreachable: the host could not reach Hermes's gateway.
	// Nothing was answered.
	ErrHermesUnreachable = errors.New("mirror: the host could not reach Hermes's gateway; answer it in the terminal")
	// ErrHermesAnswerUnknown: the answer was sent and no outcome came back in
	// time. It may have been answered: check the terminal.
	ErrHermesAnswerUnknown = errors.New("mirror: muster sent the answer but did not hear back; it may have been answered — check the terminal")
)

// hermesAnswerTimeout bounds one answer end to end: the host may attach a
// viewer first (identify, a ticket, a WebSocket, a resume).
const hermesAnswerTimeout = 40 * time.Second

// AnswerHermesApproval answers the Hermes approval promptID in one WINDOW of
// owner's host's session with choice, through the host's gateway viewer, and
// waits for the host's outcome. nil means Hermes took the answer.
func (h *Hub) AnswerHermesApproval(ctx context.Context, owner int64, host, session, window, promptID, choice string) error {
	if !slices.Contains(HermesApprovalChoices, choice) {
		return fmt.Errorf("%w: muster answers a Hermes approval with once or deny only", ErrAskNotSent)
	}
	h.mu.Lock()
	hc := h.hosts[hostKey{owner, host}]
	h.mu.Unlock()
	if hc == nil {
		return fmt.Errorf("%w: %w (host %q)", ErrAskNotSent, ErrHostOffline, host)
	}
	windowID, runtime, err := hc.targetRuntime(session, window)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrAskNotSent, err)
	}
	if runtime != RuntimeHermes {
		return fmt.Errorf("%w: %w (the window does not run Hermes)", ErrAskNotSent, ErrHermesApprovalGone)
	}
	if !hc.hermesAnswer || !hc.interactive || hc.version < 2 {
		return fmt.Errorf("%w: %w (host %q)", ErrAskNotSent, ErrHermesAnswerUnsupported, host)
	}
	if !stepup.Fresh(ctx, h.now()) {
		return fmt.Errorf("%w: %w", ErrAskNotSent, ErrStepUpRequired)
	}
	if p := hc.pendingOf(session, windowID); p == nil || p.Kind != PendingApproval || p.PromptID != promptID || !p.Offers(choice) {
		return fmt.Errorf("%w: %w", ErrAskNotSent, ErrHermesApprovalGone)
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
	m := Msg{Type: TypeAnswer, ID: id, Session: session, Window: windowID, Runtime: RuntimeHermes,
		HermesApproval: &HermesApprovalAnswer{PromptID: promptID, Choice: choice}}
	if err := writeJSON(ctx, hc.ws, m); err != nil {
		return fmt.Errorf("%w: %w", ErrHermesAnswerUnknown, err)
	}
	t := time.NewTimer(hermesAnswerTimeout)
	defer t.Stop()
	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		return fmt.Errorf("%w: %w", ErrHermesAnswerUnknown, ctx.Err())
	case <-t.C:
		return ErrHermesAnswerUnknown
	}
}

// pendingOf is the Pending the host lists for window windowID of session,
// nil for none.
func (hc *hostConn) pendingOf(session, windowID string) *Pending {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	for _, w := range hc.windows {
		if w.Session == session && w.ID == windowID && w.Status == StatusWaiting && w.Pending != nil {
			p := *w.Pending
			return &p
		}
	}
	return nil
}

// AnswerHermesApproval is Hub.AnswerHermesApproval for the viewing
// principal's host.
func (s *Service) AnswerHermesApproval(ctx context.Context, host, session, window, promptID, choice string) error {
	o, err := owner(ctx)
	if err != nil {
		return err
	}
	return s.hub.AnswerHermesApproval(ctx, o, host, session, window, promptID, choice)
}
