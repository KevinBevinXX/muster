package agents

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
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
//   - `$1` is the attach's [TerminalTag]. It is carried from the first instant
//     of the process to its last — sh's argv, then `env`'s argv, then the tmux
//     client's ENVIRONMENT (exec replaces the image, the pid stays) — which is
//     what lets [TerminalReleaseCommand] find and end exactly this client.
//   - the tombstone check: a release that ran BEFORE this process started
//     (the browser left while the exec was still being set up) leaves a file
//     named after the tag; the attach then exits instead of attaching a client
//     nobody will ever release. See terminalReleaseScript for why the order of
//     the two makes that airtight.
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
//
// ⚠ attach-session never STARTS a tmux server, and that matters: a server it
// started would inherit the tag in its environment and be ended by the release
// with the client. The tag is not in tmux's update-environment list, so it is
// not copied into the session or its panes either.
const terminalAttachScript = `tag=$1; shift; ` +
	`t="${TMPDIR:-/tmp}/` + terminalTombstoneDir + `/${tag#` + terminalTagVar + `=}"; ` +
	`if [ -e "$t" ]; then rm -f "$t"; echo "muster: this terminal was closed before it attached" >&2; exit 0; fi; ` +
	`exec env "$tag" TERM=xterm-256color tmux -u -L "${CCD_TMUX_SOCKET:-default}" ` +
	`attach-session "$@" -t "=${CCD_TMUX_TARGET:-cc}"`

// terminalReleaseScript ends the attach whose tag arrives on stdin, and prints
// how many processes it ended ("released N").
//
// 🔴 WHY THIS EXISTS: ENDING THE EXEC STREAM DOES NOT END THE EXEC'D PROCESS.
// Measured on a live cluster (containerd, muster 0.6.0): a terminal's
// tmux client — read-only and interactive alike — was still attached more than
// 60 s after muster cancelled its stream. The runtime keeps the process and its
// pty and signals neither, so nothing inside the pod can tell the stream died:
// no hangup, no EOF. The release is therefore a SECOND exec, run by muster after
// every attach (internal/agentprovision's AttachTerminal).
//
//   - The tag comes on STDIN, so the release's own shell never carries it. The
//     one process that does — the grep doing the matching — cannot match
//     itself: the /proc glob it is handed was expanded before it existed.
//   - The tombstone is written BEFORE the scan. An attach process exists from
//     some instant on and carries the tag from that instant (sh's argv), and
//     checks the tombstone before it attaches. If it started before the scan,
//     the scan sees and ends it; if after, the tombstone is already there and
//     it exits. Either way no client survives a release — the race of a tab
//     closed while its exec was still being set up included.
//   - Matching is on a WHOLE NUL-separated entry of /proc/<pid>/cmdline or
//     /proc/<pid>/environ equal to the tag — a 128-bit random value — so the
//     only process it can name is the attach it was minted for. A pid number
//     or tty alone would not do: both are reused.
//   - SIGKILL, not tmux detach-client: a detached tmux client prints its
//     "[detached]" line with a blocking write to a terminal nobody reads any
//     more, and can hang there. A killed client closes its socket, and the
//     tmux server drops it from the session at once. The session is untouched.
//   - The tombstone is removed when a process was ended (that attach can never
//     check again). When none was — the client had already exited, or the
//     attach never started — it stays: an empty file per such terminal in the
//     pod's TMPDIR, gone with the pod.
//
// Exit status 2 is a malformed tag. Outside sh's builtins it needs GNU grep
// (-z), mkdir and rm — all in the Debian base of the agent image; it does not
// assume ps. ONE grep reads every process's cmdline and environ, rather than a
// command per process, so its cost does not grow with forks per process.
const terminalReleaseScript = `IFS= read -r tag || exit 2; ` +
	`id=${tag#` + terminalTagVar + `=}; ` +
	`case "$tag" in ` + terminalTagVar + `=*) ;; *) echo "muster-terminal-release: malformed tag" >&2; exit 2;; esac; ` +
	`case "$id" in ''|*[!0-9a-f]*) echo "muster-terminal-release: malformed tag" >&2; exit 2;; esac; ` +
	`d="${TMPDIR:-/tmp}/` + terminalTombstoneDir + `"; ` +
	`mkdir -p "$d" 2>/dev/null && : > "$d/$id" 2>/dev/null; ` +
	`n=0; ` +
	`for f in $(grep -lsxzF -e "$tag" /proc/[0-9]*/cmdline /proc/[0-9]*/environ); do ` +
	`p=${f#/proc/}; p=${p%%/*}; kill -KILL "$p" 2>/dev/null && n=$((n+1)); done; ` +
	`[ "$n" -gt 0 ] && rm -f "$d/$id"; ` +
	`echo "released $n"`

const (
	// terminalTagVar is the environment variable a terminal's tmux client
	// carries its tag in.
	terminalTagVar = "MUSTER_TERMINAL_ID"
	// terminalTombstoneDir is the directory under the container's TMPDIR a
	// release marks its tag in.
	terminalTombstoneDir = "muster-terminal"
)

// TerminalReadOnlyFlag is tmux's read-only attach flag. With it the client's
// keys are ignored by tmux itself — the second of two layers, the first being
// that muster forwards no input to a read-only terminal at all
// (internal/api's terminal handler).
const TerminalReadOnlyFlag = "-r"

// TerminalTag identifies ONE terminal attach inside the agent's container, so
// that attach's process can be found and ended when the browser leaves. It is
// `MUSTER_TERMINAL_ID=<32 hex>`: an environment assignment, exactly as it
// appears in the attach's argv and in its tmux client's environment.
type TerminalTag string

var terminalTagPattern = regexp.MustCompile(`^` + terminalTagVar + `=[0-9a-f]{32}$`)

// NewTerminalTag mints a fresh random tag.
func NewTerminalTag() (TerminalTag, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("agents: mint a terminal tag: %w", err)
	}
	return TerminalTag(terminalTagVar + "=" + hex.EncodeToString(b[:])), nil
}

// CheckTerminal is ErrNoTerminal for a kind with no terminal to attach to, and
// nil for one with.
func CheckTerminal(kind string) error {
	if ResolveKind(kind) != KindClaudeCode {
		return fmt.Errorf("%w: kind %q runs no interactive session (only %q does)",
			ErrNoTerminal, ResolveKind(kind), KindClaudeCode)
	}
	return nil
}

// TerminalCommand is the command that attaches a terminal to an agent's live
// session, or ErrNoTerminal for a kind without one. tag identifies the attach
// for [TerminalReleaseCommand]; a malformed one is refused, because an attach
// nothing can find is an attach nothing can end.
//
// 🔴 READ-ONLY IS AN ARGUMENT, NOT A DEFAULT THE CALLER CAN FORGET: the flag
// is the LAST argv element when set, so it is visible to anything that logs or
// inspects the command (the e2e fake backend reports it), and there is no
// spelling of this function that attaches read-write by omission.
//
// 🔴 EVERY ATTACH MUST BE FOLLOWED BY ITS RELEASE. The attached client outlives
// the exec stream that started it (see terminalReleaseScript), so whoever runs
// this command owes a [TerminalReleaseCommand] with the same tag once the
// stream ends, however it ends.
//
// ⚠ A CLIENT AFFECTS THE WINDOW SIZE EVEN WHEN READ-ONLY. Measured on tmux
// 3.7c: an `attach-session -r` client reports flags `ignore-size,read-only`
// and the window still took its size (200x50 → 60x19, then 45x29 on a resize),
// and KEPT it after the client detached. So a phone attaching leaves the TUI
// phone-narrow until a wider client attaches. ccd reads turns from the
// transcript rather than the screen, so its answers are unaffected; what the
// operator sees in a later, wider attach is a TUI redrawn at that width.
func TerminalCommand(kind string, readOnly bool, tag TerminalTag) ([]string, error) {
	if err := CheckTerminal(kind); err != nil {
		return nil, err
	}
	if !terminalTagPattern.MatchString(string(tag)) {
		return nil, fmt.Errorf("agents: terminal tag %q is not %s=<32 hex>", tag, terminalTagVar)
	}
	cmd := []string{"sh", "-c", terminalAttachScript, "muster-terminal", string(tag)}
	if readOnly {
		cmd = append(cmd, TerminalReadOnlyFlag)
	}
	return cmd, nil
}

// TerminalReleaseCommand is the command that ends the attach tagged
// TerminalReleaseInput's tag: run it as an ordinary (non-terminal) exec in the
// same container with that input on stdin. It prints "released N".
func TerminalReleaseCommand() []string {
	return []string{"sh", "-c", terminalReleaseScript, "muster-terminal-release"}
}

// TerminalReleaseInput is the stdin [TerminalReleaseCommand] reads its tag from.
func TerminalReleaseInput(tag TerminalTag) string { return string(tag) + "\n" }
