package mirrorhost

import (
	"bytes"
	"context"
	"errors"
	"time"

	"crypto/rand"
	"encoding/hex"
	"fmt"
	"github.com/ZacxDev/muster/internal/mirror"
	"os/exec"
	"strings"

	"github.com/ZacxDev/muster/internal/askdrive"
	"github.com/ZacxDev/muster/internal/askq"
)

// ANSWERING AN AskUserQuestion PICKER FROM MUSTER: the keys go to the pane the
// way they would from a keyboard, through internal/askdrive, which reads the
// pane before and after every one.
//
// 🔴 THE KEYS ARE PASTED, UNSANITIZED (`paste-buffer -S`), NEVER `send-keys`.
// Measured (tmux 3.7c): with a read-only client attached to the session —
// which muster's own Terminal view is — `send-keys` is refused with "client is
// read-only", for a pane target as for a session one. `paste-buffer` has no
// such check; without -S it SANITIZES control characters (an arrow arrived as
// nothing and a Backspace as the text "^?"), with -S the bytes arrive as the
// keys they are. No -p: a bracketed paste is text to the picker, not keys.

// AskBackend answers an AskUserQuestion picker in a pane.
type AskBackend interface {
	AnswerAsk(ctx context.Context, pane string, qs []askq.Question, choices []askdrive.Choice) error
}

// AskOf is b's AskBackend, if it has one (through any wrapper).
func AskOf(b Backend) (AskBackend, bool) { return capabilityOf[AskBackend](b) }

// AnswerAsk implements AskBackend: askdrive.Drive on the pane.
func (t Tmux) AnswerAsk(ctx context.Context, pane string, qs []askq.Question, choices []askdrive.Choice) error {
	if !strings.HasPrefix(pane, "%") {
		return fmt.Errorf("muster-mirror: %q is not a tmux pane id", pane)
	}
	return askdrive.Drive(ctx, tmuxPane{t: t, id: pane}, qs, choices, askdrive.Options{})
}

// tmuxPane is a tmux pane as an askdrive.Pane.
type tmuxPane struct {
	t  Tmux
	id string
}

func (p tmuxPane) run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, p.t.bin(), append(p.t.server(), args...)...)
	cmd.Env = tmuxEnv()
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("tmux %s: %v: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

// Capture is the pane's screen with wrapped lines joined.
func (p tmuxPane) Capture(ctx context.Context) (string, error) {
	b, err := p.run(ctx, nil, "capture-pane", "-p", "-J", "-t", p.id)
	return string(b), err
}

// Send pastes keys unsanitized and unbracketed (see the file comment).
func (p tmuxPane) Send(ctx context.Context, keys string) error {
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	buf := "muster-mirror-ask-" + hex.EncodeToString(rnd[:])
	if _, err := p.run(ctx, []byte(keys), "load-buffer", "-b", buf, "-"); err != nil {
		return err
	}
	if _, err := p.run(ctx, nil, "paste-buffer", "-S", "-d", "-b", buf, "-t", p.id); err != nil {
		_, _ = p.run(ctx, nil, "delete-buffer", "-b", buf)
		return err
	}
	return nil
}

// askAnswer is what this host advertises as Msg.AskAnswer: an -interactive
// host whose backend can both type into a picker and find a Claude pane.
func (h *hostSide) askAnswer() bool {
	_, ask := AskOf(h.c.cfg.Backend)
	_, claude := h.claudeBackend()
	return ask && claude && h.c.cfg.Interactive
}

// answer types m.Ask into the AskUserQuestion picker of the Claude pane of
// what m names, and answers answered.
//
// 🔴 REFUSED UNLESS THIS HOST OPTED IN (-interactive), whatever muster asks —
// muster refuses it too — and only for a listed session, into a pane that
// runs Claude Code NOW (re-resolved here, as for a paste to the rendered view).
// The driver itself refuses to touch a picker that is not this question,
// untouched (internal/askdrive).
func (h *hostSide) answer(m mirror.Msg) {
	reply := func(err error) {
		ans := mirror.Msg{Type: mirror.TypeAnswered, ID: m.ID}
		if err != nil {
			ans.Error = err.Error()
			switch {
			case errors.Is(err, askdrive.ErrNoPicker):
				ans.Refusal = mirror.AskRefusalNoPicker
			case errors.Is(err, askdrive.ErrNotUntouched):
				ans.Refusal = mirror.AskRefusalTouched
			case errors.Is(err, askdrive.ErrLost):
				ans.Refusal = mirror.AskRefusalLost
			case errors.Is(err, errCannotAnswer):
				ans.Refusal = mirror.AskRefusalUnsupported
			}
			h.c.log.Printf("muster-mirror: answer in %q%s refused: %v", m.Session, windowSuffix(m.Window), err)
		} else {
			h.c.log.Printf("muster-mirror: typed an answer into %q%s", m.Session, windowSuffix(m.Window))
		}
		_ = h.send(ans)
	}
	if !h.c.cfg.Interactive {
		reply(fmt.Errorf("%w: this host is read-only (muster-mirror was not started with -interactive)", errCannotAnswer))
		return
	}
	if err := h.allowed(m); err != nil {
		reply(err)
		return
	}
	if m.Ask == nil || len(m.Ask.Questions) == 0 {
		reply(errors.New("no answer to type"))
		return
	}
	ab, ok := AskOf(h.c.cfg.Backend)
	cb, okc := h.claudeBackend()
	if !ok || !okc {
		reply(fmt.Errorf("%w: this host cannot type into a picker", errCannotAnswer))
		return
	}
	ctx, cancel := context.WithTimeout(h.ctx, 50*time.Second)
	defer cancel()
	p, ok := h.claudePaneOf(cb, m)(ctx)
	if !ok {
		reply(fmt.Errorf("%w: %v", askdrive.ErrNoPicker, errNotClaudeNow))
		return
	}
	h.leaveCopyModeFor(p.Pane)
	reply(ab.AnswerAsk(ctx, p.Pane, m.Ask.Questions, m.Ask.Choices))
}

// errCannotAnswer marks a refusal that means this host cannot type answers at
// all (mirror.AskRefusalUnsupported).
var errCannotAnswer = errors.New("muster-mirror: cannot type an answer")
