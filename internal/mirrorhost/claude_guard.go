package mirrorhost

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/ccdialog"
	"github.com/ZacxDev/muster/internal/mirror"
)

// THE CLAUDE PROMPT GUARD: a rendered-view paste (Send, Send now, a queued or
// routed message) into a mirrored Claude Code pane is refused while that pane
// is waiting on the person. The paste ends with Enter, and Enter on a
// permission prompt takes its highlighted "1. Yes": a message would approve a
// tool call with nobody watching. Answering the dialog is the person's, in the
// terminal (or through muster's own answer path for a question, which types
// through typeInto, not here).
//
// 🔴 CHECKED HERE, ON THE HOST, AT THE MOMENT OF TYPING — before the paste AND
// again before its Enter (claudeReady, through Tmux.PasteChecked) — whatever
// muster decided. muster refuses a window its host last REPORTED waiting
// (Hub.PasteWindow) and its send queue holds one, but a host reports on its
// refresh, so a prompt that opened after the last report reaches muster late:
// only a check here sees it. What is checked, each read NOW:
//
//  1. STRUCTURAL FIRST: the pane re-resolved (the same pane, still Claude
//     Code) and Claude Code's own session status, from its sessions file —
//     waiting is a permission prompt or a question (mirror.StatusWaiting);
//  2. THE SCREEN (ccdialog.OnScreen, ccd's reading since #163): a permission
//     prompt, the AskUserQuestion picker or a numbered select menu at the
//     bottom of the pane — for a dialog the status does not say yet, or a
//     numbered menu it never reports (one the person opened). This is the
//     Hermes gated send's approach (hermes_send.go: capture the pane, check
//     it, before any key), with Claude Code's reading of the screen. A screen
//     that cannot be read is refused: it is not presumed safe.
//
// ⚠ A CHECK BEFORE THE KEYS, NOT A LOCK. A dialog that opens in the
// milliseconds between the last check and a key is not caught: Claude Code has
// no "submit only if idle" (docs/design/agent-cards.md, "Deferred: (c)").
//
// The Terminal view's composer (Msg.AnyPane) is NOT gated: it is the person
// typing into the pane they are looking at. Hermes panes have their own gated
// send (hermes_send.go). Opencode panes have no guard of either kind.

// The Claude prompt guard's refusals. Their text is the person's (mirror's
// Claude*Text); internals are logged on the host.
var (
	// ErrClaudeWaiting: Claude Code's session status is waiting.
	ErrClaudeWaiting = errors.New(mirror.ClaudeWaitingText)
	// ErrClaudeDialog: the pane's screen shows a dialog.
	ErrClaudeDialog = errors.New(mirror.ClaudeDialogText)
	// ErrClaudeScreenUnread: the pane's screen could not be read.
	ErrClaudeScreenUnread = errors.New(mirror.ClaudeScreenUnreadText)
	// ErrClaudeBox: Claude Code's input box already holds text.
	ErrClaudeBox = errors.New(mirror.ClaudeBoxText)
	// ErrClaudeBoxUnread: no input box could be read on the screen.
	ErrClaudeBoxUnread = errors.New(mirror.ClaudeBoxUnreadText)
	// ErrClaudeUncheckable: the backend has no screen read or no checked
	// paste (ClaudeScreenBackend, ClaudeCheckedPaster), so the guard cannot
	// run at all. It is no refusal kind: it is not something that clears, so
	// a queued message fails on it rather than waiting.
	ErrClaudeUncheckable = errors.New("Not sent: this muster-mirror cannot check Claude Code's screen before and after typing, so it pastes nothing into a Claude Code pane.")
)

// ClaudeScreenBackend is a backend that can read a Claude Code pane's screen
// for the prompt guard. The tmux backend implements it; a backend that does
// not is refused every Claude paste (ErrClaudeUncheckable).
type ClaudeScreenBackend interface {
	// ClaudeScreen is pane's screen, wrapped lines joined (capture-pane -p -J).
	ClaudeScreen(ctx context.Context, pane string) (string, error)
}

// ClaudeCheckedPaster is a ClaudeBackend whose paste runs before immediately
// before the paste and beforeEnter immediately before its Enter, and presses
// Enter only when both pass. (Two checks, because the second must not refuse
// the box for holding the text just pasted.) The tmux backend implements it; a backend that does not
// is refused every Claude paste (ErrClaudeUncheckable) — a plain Paste's Enter
// is unchecked.
type ClaudeCheckedPaster interface {
	PasteChecked(ctx context.Context, p ClaudePane, text string, before, beforeEnter func(context.Context) error) error
}

// ClaudePastedError: the check before the Enter refused, after the text was
// pasted. The text is in Claude Code's input box; Enter was not pressed.
type ClaudePastedError struct{ Why error }

func (e *ClaudePastedError) Error() string {
	why := strings.TrimSuffix(strings.TrimPrefix(e.Why.Error(), "Not sent: "), " Nothing was typed into the pane.")
	if !strings.HasSuffix(why, ".") {
		why += "."
	}
	return "Pasted, not submitted: the text is in Claude Code's input box, and muster did not press Enter, because " +
		why + " Check the terminal."
}

func (e *ClaudePastedError) Unwrap() error { return e.Why }

// claudeRefusalOf is the Msg.Refusal kind of a Claude prompt guard refusal, ""
// for any other error. A refusal after the paste is never "nothing typed".
func claudeRefusalOf(err error) string {
	var pe *ClaudePastedError
	switch {
	case errors.As(err, &pe):
		return mirror.PasteRefusalClaudePasted
	case errors.Is(err, ErrClaudeBox):
		return mirror.PasteRefusalClaudeBox
	case errors.Is(err, ErrClaudeWaiting), errors.Is(err, ErrClaudeDialog), errors.Is(err, ErrClaudeScreenUnread),
		errors.Is(err, ErrClaudeBoxUnread):
		return mirror.PasteRefusalClaudeWaiting
	}
	return ""
}

// claudeReady is the prompt guard's check of the Claude pane m names, which
// the paste resolved to pane: nil to type, else the refusal. Every read is
// made now. With emptyBox (every check before the paste) Claude Code's input
// box must also be empty: a message pasted after text left there would be
// submitted with it as one turn, as Hermes's send refuses too. The check
// before the Enter passes false — the box then holds the text just pasted.
func (h *hostSide) claudeReady(ctx context.Context, cb ClaudeBackend, m mirror.Msg, pane string, emptyBox bool) error {
	p, ok := h.claudePaneOf(cb, m)(ctx)
	if !ok || p.Pane != pane {
		return errNotClaudeNow
	}
	if p.Status == mirror.StatusWaiting {
		h.c.log.Printf("muster-mirror: Claude Code in pane %s is waiting on the person (its session status); nothing is typed into it", pane)
		return ErrClaudeWaiting
	}
	// 🔴 NO SCREEN READ, NO PASTE: a backend that cannot read the pane cannot
	// rule out a dialog the status does not say yet, so it is refused — never
	// the status check alone.
	sb, ok := capabilityOf[ClaudeScreenBackend](h.c.cfg.Backend)
	if !ok {
		h.c.log.Printf("muster-mirror: this backend cannot read Claude Code pane %s's screen; nothing is typed into it", pane)
		return ErrClaudeUncheckable
	}
	raw, err := sb.ClaudeScreen(ctx, pane)
	if err != nil {
		h.c.log.Printf("muster-mirror: could not read the screen of Claude Code pane %s, so nothing is typed into it: %v", pane, err)
		return ErrClaudeScreenUnread
	}
	if what, shown := ccdialog.OnScreen(raw); shown {
		h.c.log.Printf("muster-mirror: Claude Code pane %s shows %s; nothing is typed into it", pane, what)
		return ErrClaudeDialog
	}
	if !emptyBox {
		return nil
	}
	switch box, found := ccdialog.InputBox(raw); {
	case !found:
		h.c.log.Printf("muster-mirror: no input box could be read on Claude Code pane %s's screen; nothing is typed into it", pane)
		return ErrClaudeBoxUnread
	case box != "":
		h.c.log.Printf("muster-mirror: Claude Code pane %s's input box already holds %d bytes; nothing is typed into it", pane, len(box))
		return ErrClaudeBox
	}
	return nil
}

// checkedPasteSteps is what a checked paste does to the pane: the tmux
// backend's (Tmux.PasteChecked), or a test's script.
type checkedPasteSteps struct {
	paste func(ctx context.Context) error
	enter func(ctx context.Context) error
	sleep func(ctx context.Context, d time.Duration) error
}

// runCheckedPaste is before, the paste, enterDelay, beforeEnter, then Enter.
// A check that refuses before the paste types nothing; one that refuses before
// the Enter leaves the text pasted and withholds the Enter (ClaudePastedError).
func runCheckedPaste(ctx context.Context, s checkedPasteSteps, before, beforeEnter func(context.Context) error) error {
	if err := before(ctx); err != nil {
		return err
	}
	if err := s.paste(ctx); err != nil {
		return err
	}
	if err := s.sleep(ctx, enterDelay); err != nil {
		return errors.New("muster-mirror: the message was pasted but not submitted (cancelled)")
	}
	// 🔴 AGAIN BEFORE THE ENTER: a dialog that opened while the paste was
	// landing would take this Enter as its answer.
	if err := beforeEnter(ctx); err != nil {
		return &ClaudePastedError{Why: err}
	}
	if err := s.enter(ctx); err != nil {
		return fmt.Errorf("muster-mirror: the message was pasted but not submitted: %w", err)
	}
	return nil
}

// CheckedPaste is the checked paste for a backend that types by its own means
// (the tmux backend; a test or e2e fake): before, paste, enterDelay,
// beforeEnter, then enter.
func CheckedPaste(ctx context.Context, paste, enter, before, beforeEnter func(context.Context) error) error {
	return runCheckedPaste(ctx, checkedPasteSteps{paste: paste, enter: enter, sleep: sleepCtx}, before, beforeEnter)
}

// PasteChecked implements ClaudeCheckedPaster: PastePane's paste and Enter,
// with check run before each.
func (t Tmux) PasteChecked(ctx context.Context, p ClaudePane, text string, before, beforeEnter func(context.Context) error) error {
	return CheckedPaste(ctx,
		func(ctx context.Context) error { return t.pasteBracketed(ctx, p.Pane, text) },
		func(ctx context.Context) error { return t.pasteEnter(ctx, p.Pane) },
		before, beforeEnter)
}

// ClaudeScreen implements ClaudeScreenBackend: capture-pane -p -J by the pane's
// id, as ccd reads its own CLI's screen.
func (t Tmux) ClaudeScreen(ctx context.Context, paneID string) (string, error) {
	if !strings.HasPrefix(paneID, "%") {
		return "", fmt.Errorf("%q is not a tmux pane id", paneID)
	}
	cmd := exec.CommandContext(ctx, t.bin(), t.query("capture-pane", "-p", "-J", "-t", paneID)...)
	cmd.Env = tmuxEnv()
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("tmux capture-pane: %w", err)
	}
	return string(out), nil
}
