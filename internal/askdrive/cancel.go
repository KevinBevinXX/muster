package askdrive

import (
	"context"
	"fmt"
	"time"
)

// KeyEscape is the Escape key, as the byte a terminal sends for it.
//
// Measured (Claude Code 2.1.289, a private tmux server): ONE Escape on the
// AskUserQuestion picker — on a question screen, with or without previews, or
// on the review screen — dismisses the whole ask; the CLI records "User
// declined to answer questions" and returns to its prompt.
const KeyEscape = "\x1b"

// Cancel dismisses the picker in p with ONE Escape — the only key it can
// send — and waits for the picker to go (the CLI records the ask declined).
// gone false: the Escape was sent and a picker was still on screen when
// Settle ran out; no second Escape is sent either way.
//
// 🔴 ONLY INTO A PICKER: before the key it reads the pane once, and it must
// show a picker — a question screen, the review screen, or a picker taller
// than the pane (Clipped). With none on screen the Escape would reach Claude
// Code itself (an Escape there interrupts a turn), so nothing is sent
// (ErrNoPicker); something like a picker that does not parse is refused too
// (ErrUnrecognised). It does NOT check that the picker is the card's own ask:
// a clipped picker's question cannot be read, so it accepts ANY picker. One
// picker shows at a time, and a newer ask supersedes the older card, whose
// Cancel is then gone (askq.Store.Open).
func Cancel(ctx context.Context, p Pane, opt Options) (gone bool, err error) {
	if opt.Settle <= 0 {
		opt.Settle = 3 * time.Second
	}
	if opt.Poll <= 0 {
		opt.Poll = 40 * time.Millisecond
	}
	scr, err := p.Capture(ctx)
	if err != nil {
		return false, err
	}
	switch Parse(scr).Kind {
	case None:
		return false, fmt.Errorf("%w: no picker on screen", ErrNoPicker)
	case Unknown:
		return false, fmt.Errorf("%w: the screen does not read as a picker", ErrUnrecognised)
	}
	if err := p.Send(ctx, KeyEscape); err != nil {
		return false, err
	}
	deadline := time.Now().Add(opt.Settle)
	for {
		scr, err := p.Capture(ctx)
		if err != nil {
			return false, err
		}
		if Parse(scr).Kind == None {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		if err := sleep(ctx, opt.Poll); err != nil {
			return false, err
		}
	}
}
