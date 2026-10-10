package agents

import (
	"errors"
	"fmt"
)

// ErrNoTerminal is the refusal for an agent whose kind has no terminal to
// attach to.
var ErrNoTerminal = errors.New("agents: this agent's kind has no terminal to attach to")

// terminalAttachScript attaches to the tmux session ccd runs the Claude Code
// TUI in. It is run with `sh -c`, so the session and socket are read from the
// SAME variables ccd reads them from (cmd/ccd/main.go: CCD_TMUX_SOCKET,
// CCD_TMUX_TARGET, with the same defaults) in the container's own environment
// — the exec inherits it — rather than being a second copy of ccd's
// configuration that could disagree with the first.
//
//   - `env TERM=xterm-256color`: an exec gets no TERM, and a tmux client with
//     none refuses to start ("missing or unsuitable terminal"). xterm-256color
//     is what the browser's terminal emulates.
//   - `-u`: the TUI draws with box and block glyphs; without a UTF-8 locale
//     tmux would replace them with `_`. The flag says the client is UTF-8
//     whatever the container's locale.
//   - `-L ${CCD_TMUX_SOCKET:-default}`: "default" IS tmux's default socket
//     name, so the unset case names the server ccd started.
//   - `-t =…`: `=` is an EXACT session match. A bare name is a prefix match, so
//     `cc` would also attach a session named `cc2` if one existed and `cc` did
//     not.
//   - `"$@"` carries `-r` for a read-only client; see TerminalCommand.
const terminalAttachScript = `exec env TERM=xterm-256color tmux -u -L "${CCD_TMUX_SOCKET:-default}" ` +
	`attach-session "$@" -t "=${CCD_TMUX_TARGET:-cc}"`

// TerminalReadOnlyFlag is tmux's read-only attach flag. With it the client's
// keys are ignored by tmux itself — the second of two layers, the first being
// that muster forwards no input to a read-only terminal at all
// (internal/api's terminal handler).
const TerminalReadOnlyFlag = "-r"

// TerminalCommand is the command that attaches a terminal to an agent's live
// session, or ErrNoTerminal for a kind without one.
//
// 🔴 READ-ONLY IS AN ARGUMENT, NOT A DEFAULT THE CALLER CAN FORGET: the flag
// is the LAST argv element when set, so it is visible to anything that logs or
// inspects the command (the e2e fake backend reports it), and there is no
// spelling of this function that attaches read-write by omission.
//
// ⚠ A CLIENT AFFECTS THE WINDOW SIZE EVEN WHEN READ-ONLY. Measured on tmux
// 3.7c: an `attach-session -r` client reports flags `ignore-size,read-only`
// and the window still took its size (200x50 → 60x19, then 45x29 on a resize),
// and KEPT it after the client detached. So a phone attaching leaves the TUI
// phone-narrow until a wider client attaches. ccd reads turns from the
// transcript rather than the screen, so its answers are unaffected; what the
// operator sees in a later, wider attach is a TUI redrawn at that width.
func TerminalCommand(kind string, readOnly bool) ([]string, error) {
	if ResolveKind(kind) != KindClaudeCode {
		return nil, fmt.Errorf("%w: kind %q runs no interactive session (only %q does)",
			ErrNoTerminal, ResolveKind(kind), KindClaudeCode)
	}
	cmd := []string{"sh", "-c", terminalAttachScript, "muster-terminal"}
	if readOnly {
		cmd = append(cmd, TerminalReadOnlyFlag)
	}
	return cmd, nil
}
