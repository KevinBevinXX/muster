package mirrorhost

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/askdrive"
	"github.com/ZacxDev/muster/internal/mirror"
)

// DISMISSING AN AskUserQuestion PICKER FROM MUSTER (mirror.TypeCancelAsk): one
// Escape into the Claude pane, after the pane is read and found to show a
// picker (askdrive.Cancel). Measured (Claude Code 2.1.289): one Escape
// dismisses the whole ask, and the CLI records it declined.
//
// 🔴 THE ONLY KEY IS askdrive.KeyEscape. The message names the window and
// nothing else; a field an older or a hostile muster added is never typed,
// and the backend's method takes no keys at all (AskCanceller).

// AskCanceller dismisses the AskUserQuestion picker in a pane with one Escape:
// gone false when it was still showing after the Escape (askdrive.Cancel).
type AskCanceller interface {
	CancelAsk(ctx context.Context, pane string) (gone bool, err error)
}

// askStillShowingNote is the note a dismissal answers with when the picker was
// still on screen after the Escape: it was sent; nothing more is.
const askStillShowingNote = "Esc sent; the terminal still shows the question."

// CancelAsk implements AskCanceller: askdrive.Cancel on the pane — the same
// one-drive-per-pane lock as an answer (ErrPaneBusy), and the same need for
// `paste-buffer -S` (ErrNoRawPaste: a sanitizing paste would turn the Escape
// into nothing).
func (t Tmux) CancelAsk(ctx context.Context, pane string) (bool, error) {
	if !strings.HasPrefix(pane, "%") {
		return false, fmt.Errorf("muster-mirror: %q is not a tmux pane id", pane)
	}
	release, ok := lockPane(strings.Join(append(t.server(), pane), " "))
	if !ok {
		return false, ErrPaneBusy
	}
	defer release()
	if has, known := t.RawPaste(ctx); known && !has {
		return false, ErrNoRawPaste
	}
	t.dropLeftoverBuffers(ctx)
	return askdrive.Cancel(ctx, tmuxPane{t: t, id: pane}, askdrive.Options{})
}

// askCancel is what this host advertises as Msg.AskCancel, given what it
// advertises as Msg.AskAnswer (answering: an -interactive host that can type
// a picker's keys — the Escape needs the same raw paste — and find a Claude
// pane): that, and a backend that can dismiss a picker.
func (h *hostSide) askCancel(answering bool) bool {
	_, ok := capabilityOf[AskCanceller](h.c.cfg.Backend)
	return ok && answering
}

// cancelAsk dismisses the AskUserQuestion picker in the Claude pane of what m
// names, and answers ask-cancelled (with askStillShowingNote when the picker
// was still there after the Escape).
//
// 🔴 REFUSED UNLESS THIS HOST OPTED IN (-interactive), only for a listed
// session, into a pane that runs Claude Code NOW, through typeInto and so the
// Hermes paste guard — exactly an answer's gates (answer). The key is
// askdrive.KeyEscape, always, and only when the pane shows a picker.
func (h *hostSide) cancelAsk(m mirror.Msg) {
	note := ""
	reply := func(err error) {
		out := mirror.Msg{Type: mirror.TypeAskCancelled, ID: m.ID}
		if err != nil {
			out.Error = err.Error()
			switch {
			case errors.Is(err, askdrive.ErrNoPicker):
				out.Refusal = mirror.AskRefusalNoPicker
			case errors.Is(err, ErrPaneBusy):
				out.Refusal = mirror.AskRefusalBusy
			case errors.Is(err, askdrive.ErrUnrecognised):
				out.Refusal = mirror.AskRefusalUnrecognised
			case errors.Is(err, errCannotAnswer):
				out.Refusal = mirror.AskRefusalUnsupported
			}
			h.c.log.Printf("muster-mirror: dismissing a question in %q%s refused: %v", m.Session, windowSuffix(m.Window), err)
		} else {
			out.Text = note
			h.c.log.Printf("muster-mirror: sent Escape to a question in %q%s %s", m.Session, windowSuffix(m.Window), note)
		}
		_ = h.send(out)
	}
	if !h.c.cfg.Interactive {
		reply(fmt.Errorf("%w: this host is read-only (muster-mirror was not started with -interactive)", errCannotAnswer))
		return
	}
	if err := h.allowed(m); err != nil {
		reply(err)
		return
	}
	cb, okb := capabilityOf[AskCanceller](h.c.cfg.Backend)
	cl, okc := h.claudeBackend()
	if !okb || !okc {
		reply(fmt.Errorf("%w: this host cannot dismiss a picker", errCannotAnswer))
		return
	}
	ctx, cancel := context.WithTimeout(h.ctx, 15*time.Second)
	defer cancel()
	p, ok := h.claudePaneOf(cl, m)(ctx)
	if !ok {
		reply(fmt.Errorf("%w: %v", askdrive.ErrNoPicker, errNotClaudeNow))
		return
	}
	reply(h.typeInto(ctx, p.Pane, func() error {
		gone, err := cb.CancelAsk(ctx, p.Pane)
		if err == nil && !gone {
			note = askStillShowingNote
		}
		return err
	}))
}
