package mirrorhost

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/hermeshost"
	"github.com/ZacxDev/muster/internal/mirror"
)

// THE GATED SEND INTO A HERMES PANE (docs/design/hermes.md, "The send path",
// "The screen check"; Phase 3). It is the ONE deliberate exception to the
// Hermes paste guard (hostSide.typeInto refuses every Hermes pane): every
// Hermes send — Send, Queue, Send now, a routed message — reaches the host as
// a paste with Msg.Runtime "hermes" and comes here, and nowhere else
// (hermes_ledger_test.go holds that). In this order:
//
//  1. the text rules: refused when its stripped text starts with `/` or `!`
//     (a command Hermes runs locally, writing no row; some open overlays the
//     screen cannot see) or ends with `\` (Enter then continues the line);
//  2. the status, NOW: the window's Hermes pane, believed idle (the status
//     file against the lease and the newest row) with EXACTLY the since the
//     server decided on — else superseded;
//  3. the screen check (hermeshost.CheckIdle) on the pane as it is, BEFORE
//     any key: the status bar, the stash mark, a vi label, an image bar, the
//     idle prompt, every input row empty, nothing below; and a message the
//     box could not show whole is refused here too;
//  4. ONE Escape, then a wait above prompt_toolkit's timeoutlen and a
//     re-capture that must pass the check again, else refused. Measured
//     (`make test-livehermes`): every overlay a second or third Escape
//     closed is refused by the screen check before any Escape, and the one
//     overlay an Escape is for — the stash panel when the bar cannot show
//     it — closes on the first;
//  5. the status again, then the paste, bracketed, with no Enter;
//  6. enterDelay later: the status again, and the screen must show EXACTLY
//     the message (or its one collapse placeholder) under the idle prompt —
//     else "pasted, not submitted", with no Enter;
//  7. Enter, as Tmux.PastePane presses it (a pasted newline);
//  8. confirmed BY THE DATABASE: a new user row in the lineage whose content,
//     stripped, is the message, stripped. None within hermesConfirmWait is
//     "may have been submitted" — never "not submitted", which would invite a
//     resend and a second turn.
//
// Only the refusals of 1–5 say "not submitted": no Enter was sent there.

// hermesEscapeWait is the wait after the Escape: above prompt_toolkit's
// timeoutlen (1 s; Hermes does not override it), so the Escape is never read
// as the first half of a double-Escape with a key that follows it.
const hermesEscapeWait = 1500 * time.Millisecond

// hermesConfirmWait bounds the wait for the submitted message's row after
// Enter (step 8). The live harness's slowest Enter-to-row is printed by `make
// test-livehermes`.
const hermesConfirmWait = 8 * time.Second

// hermesSendTimeout bounds one whole gated send on the host: the Escapes
// (3 × 1.5 s), the paste and its checks, and the confirm wait.
const hermesSendTimeout = 30 * time.Second

// HermesSendKind is how a gated send ended, when it did not end "sent".
type HermesSendKind int

const (
	// HermesNotSubmitted: refused before anything was pasted (steps 1–5).
	HermesNotSubmitted HermesSendKind = iota
	// HermesSuperseded: the window's status moved after the server decided
	// (step 2 or 5); nothing was pasted.
	HermesSuperseded
	// HermesPasted: the text was pasted and Enter withheld (step 6): it is in
	// Hermes's input box.
	HermesPasted
	// HermesUnconfirmed: Enter was sent and no row arrived (step 8): it may
	// have been submitted.
	HermesUnconfirmed
)

// HermesSendError is a gated send that did not end "sent".
type HermesSendError struct {
	Kind HermesSendKind
	// Why is the reason, a person's words, no internals.
	Why string
}

func (e *HermesSendError) Error() string {
	switch e.Kind {
	case HermesPasted:
		return "Pasted, not submitted: the text is in Hermes's input box, and muster did not press Enter, because " + e.Why + ". Check the terminal."
	case HermesUnconfirmed:
		return "May have been submitted: check the conversation before sending again. " + e.Why + "."
	case HermesSuperseded:
		return "Not sent: " + e.Why + ". Nothing was pasted."
	}
	return "Not sent: " + e.Why + ". Nothing was pasted."
}

// Refusal is the Msg.Refusal kind of e.
func (e *HermesSendError) Refusal() string {
	switch e.Kind {
	case HermesSuperseded:
		return mirror.PasteRefusalHermesSuperseded
	case HermesPasted:
		return mirror.PasteRefusalHermesPasted
	case HermesUnconfirmed:
		return mirror.PasteRefusalHermesUnconfirmed
	}
	return mirror.PasteRefusalHermesNotSubmitted
}

func notSubmitted(why string) error { return &HermesSendError{Kind: HermesNotSubmitted, Why: why} }

// HermesTextRefusal is why text (already mirror.SanitizePaste'd) is never
// sent to Hermes, "" when it may be. Stripped first, as Hermes strips what it
// submits.
func HermesTextRefusal(text string) string {
	s := strings.TrimSpace(text)
	switch {
	case s == "":
		return "the message is empty"
	case strings.HasPrefix(s, "/"), strings.HasPrefix(s, "!"):
		return "the message starts with " + s[:1] + ", which Hermes would run as a command instead of sending it (reword it, or type it in the terminal)"
	case strings.HasSuffix(s, `\`):
		return `the message ends with \, after which Enter continues the line in Hermes instead of submitting it`
	case allDigits(s):
		return "the message is only a number, which Hermes takes as a choice of session after /resume instead of sending it (add a word to it)"
	}
	return ""
}

// allDigits reports whether s is one or more ASCII digits and nothing else:
// what Hermes's _consume_pending_resume_selection reads as a session to
// switch to, after a person's bare /resume.
func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// hermesSendSteps is what the gated send does to the pane and reads from
// Hermes. The tmux backend fills it (Tmux.SendHermes); a test fills it with a
// script of screens and statuses.
type hermesSendSteps struct {
	// status is the window's Hermes pane NOW (re-resolved: process, breadcrumb,
	// status file believed against the lease and the newest row).
	status func(ctx context.Context) (AgentPane, error)
	// capture is the pane's screen (capture-pane -p -e).
	capture func(ctx context.Context) (string, error)
	// escape types one Escape.
	escape func(ctx context.Context) error
	// paste pastes text, bracketed, with no Enter.
	paste func(ctx context.Context, text string) error
	// enter presses Enter (a pasted newline, as Tmux.PastePane does).
	enter func(ctx context.Context) error
	// cursor is the newest row id of root's conversation.
	cursor func(ctx context.Context, root string) (int64, error)
	// submitted reports whether a user row past after, in root's conversation,
	// holds want (both stripped).
	submitted func(ctx context.Context, root string, after int64, want string) (bool, error)
	// sleep waits d (or until ctx ends).
	sleep func(ctx context.Context, d time.Duration) error
	// logf logs internals on the host, never in a refusal.
	logf func(format string, args ...any)
}

// runHermesSend is the gated send of text into the Hermes pane whose status
// the server decided on at since (the pane's StatusSince). nil is "sent":
// the database has the message.
func runHermesSend(ctx context.Context, s hermesSendSteps, since int64, text string) error {
	if why := HermesTextRefusal(text); why != "" {
		return notSubmitted(why)
	}
	if since <= 0 {
		return notSubmitted("muster sent no status time for this window, so the host cannot tell the status it decided on is still current")
	}
	// Step 2: the status, now.
	root, err := hermesStillIdle(ctx, s, since)
	if err != nil {
		return err
	}
	// Step 3: the screen, before any key.
	raw, err := s.capture(ctx)
	if err != nil {
		s.logf("muster-mirror: capture a Hermes pane: %v", err)
		return notSubmitted("muster-mirror could not read the pane's screen")
	}
	scr := hermeshost.ParseScreen(raw)
	if err := hermeshost.CheckIdle(scr); err != nil {
		return notSubmitted(err.Error())
	}
	if !hermeshost.Collapses(text) {
		if rows := hermeshost.InputRows(text, hermeshost.PromptWidth(scr), hermeshost.InputWidth(scr)); rows > hermeshost.MaxInputRows {
			return notSubmitted(fmt.Sprintf("the message would fill %d rows of Hermes's input box, which shows %d, so muster could not check it on screen before Enter (shorten it, or send it as six or more lines, which Hermes collapses)", rows, hermeshost.MaxInputRows))
		}
	}
	// Step 4: one Escape, and the screen again.
	if err := s.escape(ctx); err != nil {
		s.logf("muster-mirror: Escape into a Hermes pane: %v", err)
		return notSubmitted("muster-mirror could not type into the pane")
	}
	if err := s.sleep(ctx, hermesEscapeWait); err != nil {
		return notSubmitted("the send was cancelled")
	}
	raw, err = s.capture(ctx)
	if err != nil {
		s.logf("muster-mirror: capture a Hermes pane: %v", err)
		return notSubmitted("muster-mirror could not read the pane's screen")
	}
	if err := hermeshost.CheckIdle(hermeshost.ParseScreen(raw)); err != nil {
		return notSubmitted("after the Escape, " + err.Error())
	}
	// Step 5: the status again, then the paste.
	if _, err := hermesStillIdle(ctx, s, since); err != nil {
		return err
	}
	before, err := s.cursor(ctx, root)
	if err != nil {
		s.logf("muster-mirror: read the Hermes conversation %s: %v", root, err)
		return notSubmitted("muster-mirror could not read the conversation, so it could not confirm a submit")
	}
	if err := s.paste(ctx, text); err != nil {
		s.logf("muster-mirror: paste into a Hermes pane: %v", err)
		// A paste-buffer that failed may still have delivered part of it.
		return &HermesSendError{Kind: HermesPasted, Why: "the paste did not complete"}
	}
	// Step 6: after enterDelay, the status and the box.
	if err := s.sleep(ctx, enterDelay); err != nil {
		return &HermesSendError{Kind: HermesPasted, Why: "the send was cancelled"}
	}
	if _, err := hermesStillIdle(ctx, s, since); err != nil {
		var se *HermesSendError
		why := "Hermes's status changed while pasting"
		if errors.As(err, &se) {
			why = se.Why
		}
		return &HermesSendError{Kind: HermesPasted, Why: why}
	}
	raw, err = s.capture(ctx)
	if err != nil {
		s.logf("muster-mirror: capture a Hermes pane: %v", err)
		return &HermesSendError{Kind: HermesPasted, Why: "muster-mirror could not read the screen after the paste"}
	}
	if err := hermeshost.CheckPasted(hermeshost.ParseScreen(raw), text); err != nil {
		return &HermesSendError{Kind: HermesPasted, Why: err.Error()}
	}
	// Step 7: Enter.
	if err := s.enter(ctx); err != nil {
		s.logf("muster-mirror: Enter into a Hermes pane: %v", err)
		return &HermesSendError{Kind: HermesUnconfirmed, Why: "Enter may not have reached Hermes"}
	}
	// Step 8: the database, polled every hermesConfirmPoll for
	// hermesConfirmWait.
	for i := 0; ; i++ {
		ok, err := s.submitted(ctx, root, before, text)
		if err != nil {
			s.logf("muster-mirror: read the Hermes conversation %s: %v", root, err)
		}
		if ok {
			return nil
		}
		if time.Duration(i)*hermesConfirmPoll >= hermesConfirmWait {
			return &HermesSendError{Kind: HermesUnconfirmed, Why: fmt.Sprintf("Hermes recorded no new message within %s of the Enter", hermesConfirmWait)}
		}
		if err := s.sleep(ctx, hermesConfirmPoll); err != nil {
			return &HermesSendError{Kind: HermesUnconfirmed, Why: "the send was cancelled after Enter"}
		}
	}
}

// hermesConfirmPoll is how often step 8 reads the conversation.
const hermesConfirmPoll = 100 * time.Millisecond

// hermesStillIdle re-resolves the pane and refuses unless it is a Hermes
// pane with a known conversation, believed idle at exactly since; its
// conversation's root.
func hermesStillIdle(ctx context.Context, s hermesSendSteps, since int64) (string, error) {
	ap, err := s.status(ctx)
	if err != nil {
		return "", &HermesSendError{Kind: HermesSuperseded, Why: "the window is gone or no longer runs Hermes"}
	}
	switch {
	case ap.Runtime != mirror.RuntimeHermes:
		return "", &HermesSendError{Kind: HermesSuperseded, Why: "the window no longer runs Hermes"}
	case ap.Gen == "":
		why := "muster cannot tell which Hermes conversation the window shows"
		if ap.Hermes != nil && ap.Hermes.NoView != "" {
			why += " (" + ap.Hermes.NoView + ")"
		}
		return "", notSubmitted(why)
	case ap.Status != mirror.StatusIdle:
		why := "Hermes is not idle"
		switch ap.Status {
		case mirror.StatusBusy:
			why = "Hermes is working, and Enter would interrupt the turn"
		case mirror.StatusWaiting:
			why = "Hermes is waiting for an answer in the terminal"
		case "":
			why = "Hermes's status is unknown"
			if ap.Hermes != nil && ap.Hermes.Status.Reason != "" {
				why += " (" + ap.Hermes.Status.Reason + ")"
			}
		}
		return "", &HermesSendError{Kind: HermesSuperseded, Why: why}
	case ap.StatusSince != since:
		return "", &HermesSendError{Kind: HermesSuperseded, Why: "Hermes's status changed after muster decided to send (another turn ran)"}
	}
	return ap.Gen, nil
}

// sleepCtx waits d, or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// HermesSender is a backend that can make the gated send into a Hermes pane.
// The tmux backend implements it (with a HermesSource).
type HermesSender interface {
	SendHermes(ctx context.Context, req HermesSendRequest) error
}

// HermesSenderOf is b's HermesSender, looking through wrappers.
func HermesSenderOf(b Backend) (HermesSender, bool) { return capabilityOf[HermesSender](b) }

// HermesSendRequest is one gated send.
type HermesSendRequest struct {
	// Pane is the Hermes pane's id, as the window resolved when the paste
	// arrived.
	Pane string
	// Since is the StatusSince the server decided on (Msg.Since).
	Since int64
	// Text is the message, mirror.SanitizePaste'd.
	Text string
	// Status re-resolves the window's pane NOW; an error, or a different
	// pane, refuses.
	Status func(ctx context.Context) (AgentPane, error)
	// Before runs once the checks that need no key have passed, before the
	// first Escape: the host cancels its own copy-mode on the pane there.
	Before func()
	// Logf logs internals on the host.
	Logf func(format string, args ...any)
}

// SendHermes implements HermesSender: runHermesSend over this tmux server
// and the host's Hermes home.
//
// 🔴 ONE DRIVE PER PANE: a send while another send or an answer types into
// the same pane is refused before any key (lockPane, shared with AnswerAsk).
// And it refuses — before any key — a pane whose process tree holds a
// terminal multiplexer client (a Hermes behind it is not the one the status
// describes) or whose processes cannot be read, and a tmux that cannot type
// an Escape (no `paste-buffer -S`, MinRawPasteTmux).
func (t Tmux) SendHermes(ctx context.Context, req HermesSendRequest) error {
	if t.Hermes == nil {
		return notSubmitted("this host reads no Hermes home, so it cannot check Hermes's status")
	}
	if !strings.HasPrefix(req.Pane, "%") {
		return notSubmitted("the window has no pane muster can address")
	}
	release, ok := lockPane(strings.Join(append(t.server(), req.Pane), " "))
	if !ok {
		return notSubmitted("muster is already typing into this pane")
	}
	defer release()
	if has, known := t.RawPaste(ctx); known && !has {
		return notSubmitted("this host's tmux cannot type an Escape (paste-buffer -S needs tmux " + MinRawPasteTmux + ")")
	}
	pid, err := t.panePID(ctx, req.Pane)
	if err != nil {
		req.Logf("muster-mirror: read a Hermes pane's process: %v", err)
		return notSubmitted("muster-mirror could not check what runs in the pane")
	}
	hp, found, err := HermesIdentity{Proc: t.Claude.Proc}.MultiplexerInTree(pid)
	switch {
	case err != nil:
		req.Logf("muster-mirror: could not check what runs in pane %s: %v", req.Pane, err)
		return notSubmitted("muster-mirror could not check what runs in the pane")
	case found:
		req.Logf("muster-mirror: pane %s runs a terminal multiplexer client (pid %d); nothing is sent to Hermes there", req.Pane, hp.PID)
		return notSubmitted("the pane also runs a terminal multiplexer, which can show a Hermes muster does not see")
	}
	beforeOnce := false
	pane := tmuxPane{t: t, id: req.Pane}
	state := t.Hermes.State()
	steps := hermesSendSteps{
		status: func(ctx context.Context) (AgentPane, error) {
			ap, err := req.Status(ctx)
			if err == nil && ap.Pane != req.Pane {
				return AgentPane{}, errors.New("the window's pane changed")
			}
			return ap, err
		},
		capture: func(ctx context.Context) (string, error) { return t.capturePane(ctx, req.Pane) },
		escape: func(ctx context.Context) error {
			if !beforeOnce && req.Before != nil {
				req.Before()
			}
			beforeOnce = true
			return pane.Send(ctx, "\x1b")
		},
		paste: func(ctx context.Context, text string) error { return t.pasteBracketed(ctx, req.Pane, text) },
		enter: func(ctx context.Context) error { return t.pasteEnter(ctx, req.Pane) },
		cursor: func(ctx context.Context, root string) (int64, error) {
			p, err := state.Backfill(ctx, root, 1)
			return p.Cursor, err
		},
		submitted: func(ctx context.Context, root string, after int64, want string) (bool, error) {
			return userRowAfter(ctx, state, root, after, want)
		},
		sleep: sleepCtx,
		logf:  req.Logf,
	}
	return runHermesSend(ctx, steps, req.Since, req.Text)
}

// userRowAfter reports whether root's conversation has a user row past
// after whose content, stripped, is want, stripped: the CLI submits
// raw_text.strip() (P:hermes_cli/cli_tui_mixin.py:1452), and a collapsed
// paste is expanded back unstripped (P:hermes_cli/cli_stream_mixin.py:209-227).
func userRowAfter(ctx context.Context, state *hermeshost.State, root string, after int64, want string) (bool, error) {
	want = strings.TrimSpace(want)
	for cursor := after; ; {
		p, err := state.Tail(ctx, root, cursor, 200)
		if err != nil {
			return false, err
		}
		for _, r := range p.Rows {
			if r.Role == "user" && strings.TrimSpace(r.Content) == want {
				return true, nil
			}
		}
		if len(p.Rows) < 200 || p.Cursor <= cursor {
			return false, nil
		}
		cursor = p.Cursor
	}
}

// hermesSend is what this host advertises as Msg.HermesSend: an
// -interactive v2 host started with -hermes-send whose backend can make the
// gated send, and whose Hermes is NOT in vi mode (hermesViRefusal).
func (h *hostSide) hermesSend() bool {
	if h.version < 2 || !h.c.cfg.Interactive || !h.c.cfg.HermesSend {
		return false
	}
	if _, ok := HermesSenderOf(h.c.cfg.Backend); !ok {
		return false
	}
	if why := h.hermesViRefusal(); why != "" {
		h.c.log.Printf("muster-mirror: not sending to Hermes, though -hermes-send is on: %s", why)
		return false
	}
	return true
}

// HermesViModer is a backend that can read whether its Hermes runs in vi mode
// (display.vim_mode in config.yaml, hermeshost.Home.ViMode).
type HermesViModer interface {
	HermesViMode() (bool, error)
}

// HermesViMode implements HermesViModer for the host's Hermes home.
func (t Tmux) HermesViMode() (bool, error) {
	if t.Hermes == nil {
		return false, nil
	}
	return t.Hermes.Home.ViMode()
}

// hermesViRefusal is why this host never sends to its Hermes because of vi
// mode, "" when it may. 🔴 STRUCTURAL, NOT THE SCREEN: in vi mode one Escape
// on an idle line enters navigation, where the message's characters would
// be read as commands, and the status bar's vi label (the screen check's
// second layer) is trimmed off once a title fills the bar. Read on every
// hello and every send, so a config changed under a running host counts. A
// config.yaml that cannot be read is refused too (fail closed).
func (h *hostSide) hermesViRefusal() string {
	vm, ok := capabilityOf[HermesViModer](h.c.cfg.Backend)
	if !ok {
		return ""
	}
	on, err := vm.HermesViMode()
	switch {
	case err != nil:
		return "its Hermes's config.yaml could not be read to rule out vi mode (" + err.Error() + ")"
	case on:
		return "its Hermes runs in vi mode (display.vim_mode), and muster never sends to Hermes in vi mode"
	}
	return ""
}

// hermesPaste is a paste for a Hermes window (Msg.Runtime "hermes"): the
// gated send, and the ONE way this host types into a Hermes pane.
//
// 🔴 REFUSED UNLESS THIS HOST RUNS WITH -hermes-send (the hermesSend switch,
// host half), whatever muster sent — and unless it is -interactive, as every
// paste is. Then only a listed window whose pane runs a Hermes CLI NOW.
func (h *hostSide) hermesPaste(m mirror.Msg) {
	reply := func(err error) {
		ans := mirror.Msg{Type: mirror.TypePasted, ID: m.ID}
		if err != nil {
			ans.Error = err.Error()
			var se *HermesSendError
			if errors.As(err, &se) {
				ans.Refusal = se.Refusal()
			}
			h.c.log.Printf("muster-mirror: send to Hermes in %q%s ended: %v", m.Session, windowSuffix(m.Window), err)
		} else {
			h.c.log.Printf("muster-mirror: sent a message to Hermes in %q%s (confirmed in state.db)", m.Session, windowSuffix(m.Window))
		}
		_ = h.send(ans)
	}
	switch {
	case !h.c.cfg.Interactive:
		reply(notSubmitted("this host is read-only (muster-mirror was not started with -interactive)"))
		return
	case !h.c.cfg.HermesSend:
		reply(notSubmitted("this host's muster-mirror does not send to Hermes (it was not started with -hermes-send)"))
		return
	case m.Window == "":
		reply(notSubmitted("muster named no window"))
		return
	}
	if why := h.hermesViRefusal(); why != "" {
		reply(notSubmitted("this host does not send to Hermes: " + why))
		return
	}
	if err := h.allowed(m); err != nil {
		reply(notSubmitted(err.Error()))
		return
	}
	text, err := mirror.SanitizePaste(m.Text)
	if err != nil {
		reply(notSubmitted(err.Error()))
		return
	}
	hs, ok := HermesSenderOf(h.c.cfg.Backend)
	if !ok {
		reply(notSubmitted("this host cannot send to Hermes"))
		return
	}
	ctx, cancel := context.WithTimeout(h.ctx, hermesSendTimeout)
	defer cancel()
	status := func(ctx context.Context) (AgentPane, error) {
		w, err := h.window(ctx, m.Session, m.Window)
		if err != nil {
			return AgentPane{}, err
		}
		if w.Agent == nil || w.Agent.Runtime != mirror.RuntimeHermes {
			return AgentPane{}, errors.New("the window does not run Hermes now")
		}
		return *w.Agent, nil
	}
	ap, err := status(ctx)
	if err != nil {
		reply(&HermesSendError{Kind: HermesSuperseded, Why: "the window is gone or no longer runs Hermes"})
		return
	}
	reply(hs.SendHermes(ctx, HermesSendRequest{
		Pane: ap.Pane, Since: m.Since, Text: text, Status: status,
		Before: func() { h.leaveCopyModeFor(ap.Pane) },
		Logf:   h.c.log.Printf,
	}))
}
