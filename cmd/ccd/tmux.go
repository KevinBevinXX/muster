package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// tmuxTerminal delivers prompts into the interactive session as ONE bracketed
// paste: `load-buffer` from stdin into a uniquely named buffer, `paste-buffer -p -d`
// into the pane, then Enter.
//
// Measured with the pinned CLI: "para one\n\npara two with \"quotes\" and $HOME"
// arrived in the transcript byte-for-byte this way (the screen collapsed the
// blank line; the transcript did not).
//
// ⚠ WHY A PASTE AND NOT `send-keys -l`, STATED AT ITS REAL STRENGTH: on the pinned
// CLI a TYPED LF also became a newline rather than a submit (measured), so for
// plain multi-line text the two are equivalent and the mutation battery records
// "type it instead" as an equivalent mutant, not a caught one. What the paste buys
// is that the text travels on stdin, NEVER argv — a prompt in argv is visible in
// the process table, bounded by ARG_MAX, and parsed by tmux — and that it is the
// path the fidelity measurement above exercised. The paste does NOT stop the TUI
// reading a leading `!` as shell mode, and a paste is only a paste for as long as
// its text cannot END it: both are the caller's job (sanitizePrompt and
// neutralizeInputMode in server.go; a leading `/` is let through on purpose).
//
// 🔴 INPUT GOES TO THE CLI's PANE ID, NEVER TO THE SESSION NAME. `-t cc` resolves
// to the session's ACTIVE pane — whichever pane an attached operator last focused —
// so a split with a shell in it would receive the prompt and run it (measured
// against this code's predecessor, which targeted the session). Paste and Enter
// take the paneRef the supervisor got from new-session.
//
// 🔴 A PANE ID ALONE DOES NOT NAME A PANE: tmux numbers panes per SERVER, from %0.
// After `kill-server` (or the last session's `kill-session`), a server someone
// starts again on the same socket gives ITS first pane the id %0 — measured on
// tmux 3.7c and 3.3a — and the code before this one pasted a turn into exactly such
// a recreated SHELL pane, which ran it. So a paneRef also carries the server's
// identity, `#{pid} #{start_time}`, and every Paste, Enter and Respawn first reads
// that identity, the pane id and pane_dead in ONE display-message, refusing
// (errPaneNotLive / errTerminalLost) a pane that is gone, dead or on another server.
// ⚠ It is a check before the paste, not a lock: load-buffer and paste-buffer are
// further tmux invocations, so a server replaced in the milliseconds between them
// and the check is not caught. What it closes is a server replaced at ANY earlier
// time, which is the case that was measured.
//
// ⚠ NOT EVERY BYTE IS PRESERVED, AND THE EXCEPTIONS ARE THE CLI'S, NOT tmux's: a
// TAB became four spaces in the same measurement. CRLF and lone CR are folded to
// LF by the caller before it gets here (see sanitizePrompt).
type tmuxTerminal struct {
	bin    string
	socket string // -L name; "" = the default server
	conf   string // -f file for a server this process starts; "" = tmux's default
	// target is the SESSION name: what Start creates, and what Alive checks
	// before there is a pane. It is never an input target (see above).
	target string
	// enterDelay separates the paste from the Enter, so the TUI has ingested the
	// paste before the submit key arrives.
	enterDelay time.Duration
}

func (t tmuxTerminal) run(ctx context.Context, stdin []byte, args ...string) error {
	_, err := t.exec(ctx, stdin, args...)
	return err
}

func (t tmuxTerminal) output(ctx context.Context, args ...string) (string, error) {
	return t.exec(ctx, nil, args...)
}

func (t tmuxTerminal) exec(ctx context.Context, stdin []byte, args ...string) (string, error) {
	var full []string
	if t.socket != "" {
		full = append(full, "-L", t.socket)
	}
	if t.conf != "" {
		full = append(full, "-f", t.conf)
	}
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, t.bin, full...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var outb, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &outb, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("tmux %s: %v: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return outb.String(), nil
}

// errPaneNotLive: the pane to paste into no longer exists, its process has
// exited, or it is not the pane ccd started (errTerminalLost). The server answers
// not_ready for it.
var errPaneNotLive = errors.New("the CLI's pane is gone or its process has exited")

// errTerminalLost: the pane ccd started is gone, or the pane with its id now
// belongs to a DIFFERENT tmux server (one started again on the same socket). The
// supervisor treats it as the end of the CLI it can see (supervisor.lose).
var errTerminalLost = errors.New("the CLI's tmux pane is gone or now belongs to another tmux server")

// paneRef names the CLI's pane: its id AND the tmux server it was created in.
type paneRef struct {
	Server string // "#{pid} #{start_time}" of the server, as Start read it
	Pane   string // "%N"
}

func (r paneRef) String() string { return fmt.Sprintf("%s (tmux server %q)", r.Pane, r.Server) }

// serverFormat is the tmux server's identity: its pid and its start time. Either
// alone could repeat (a recycled pid; two servers started in one second); the
// pair, read on the socket ccd uses, does not in practice.
const serverFormat = "#{pid} #{start_time}"

// paneStateFormat is what state reads: the server identity, the pane id actually
// resolved (EMPTY when the id names no pane — measured on 3.3a and 3.7c, where
// `display-message -t %999` prints empty pane fields and exits 0), then
// paneDeadFormat.
const paneStateFormat = serverFormat + " #{pane_id} " + paneDeadFormat

// state reads ref's pane in ONE display-message and refuses, with
// errTerminalLost, a pane that does not exist or a server that is not ref's.
func (t tmuxTerminal) state(ctx context.Context, ref paneRef) (paneExit, error) {
	if !strings.HasPrefix(ref.Pane, "%") || ref.Server == "" {
		return paneExit{}, fmt.Errorf("%w: %w: %v is not a pane id with its server's identity", errPaneNotLive, errTerminalLost, ref)
	}
	out, err := t.output(ctx, "display-message", "-p", "-t", ref.Pane, paneStateFormat)
	if err != nil {
		return paneExit{}, fmt.Errorf("%w: %w: %v", errPaneNotLive, errTerminalLost, err)
	}
	f := strings.SplitN(strings.TrimRight(out, "\r\n"), " ", 4)
	if len(f) != 4 {
		return paneExit{}, fmt.Errorf("tmux display-message %q printed %q", paneStateFormat, out)
	}
	if server := f[0] + " " + f[1]; server != ref.Server || f[2] != ref.Pane {
		return paneExit{}, fmt.Errorf("%w: %w: %s now resolves to pane %q on tmux server %q", errPaneNotLive,
			errTerminalLost, ref, f[2], server)
	}
	return parsePaneExit(f[3])
}

// live is state for input: a pane that is ref's and whose process is running.
func (t tmuxTerminal) live(ctx context.Context, ref paneRef) error {
	e, err := t.state(ctx, ref)
	if err != nil {
		return err
	}
	if e.Dead {
		return fmt.Errorf("%w: pane %s is dead (%s)", errPaneNotLive, ref.Pane, e.describe())
	}
	return nil
}

func (t tmuxTerminal) Paste(ctx context.Context, ref paneRef, text string) error {
	if err := t.live(ctx, ref); err != nil {
		return err
	}
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	buf := "ccd-" + hex.EncodeToString(rnd[:])
	if err := t.run(ctx, []byte(text), "load-buffer", "-b", buf, "-"); err != nil {
		return err
	}
	if err := t.run(ctx, nil, "paste-buffer", "-p", "-d", "-b", buf, "-t", ref.Pane); err != nil {
		_ = t.run(context.Background(), nil, "delete-buffer", "-b", buf)
		return err
	}
	select {
	case <-time.After(t.enterDelay):
	case <-ctx.Done():
		return ctx.Err()
	}
	return afterPaste(t.Enter(ctx, ref))
}

// errPastedNotSubmitted marks a failure AFTER the prompt was pasted into the
// pane: the text is sitting in the CLI's input box.
var errPastedNotSubmitted = errors.New("the prompt was pasted but could not be submitted")

// afterPaste rewraps a failure of the post-paste Enter so it is NOT
// errPaneNotLive.
//
// 🔴 errPaneNotLive BECOMES A `503 not_ready`, AND muster RE-SENDS ON not_ready
// because that answer promises nothing was pasted (internal/agentkickoff). Here
// something WAS. If the pane really died the text died with it; but `live` also
// reports errPaneNotLive for a transient display-message failure on a pane that
// is still alive, and then a re-send would paste a second copy beside the first
// and submit both as one prompt. So the pane-liveness failure of the Enter is carried as
// text, not as a wrapped sentinel, and the server answers it as a terminal
// failure (502), which no caller re-sends.
func afterPaste(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %v", errPastedNotSubmitted, err)
}

func (t tmuxTerminal) Enter(ctx context.Context, ref paneRef) error {
	if err := t.live(ctx, ref); err != nil {
		return err
	}
	return t.run(ctx, nil, "send-keys", "-t", ref.Pane, "Enter")
}

// Start creates the detached session running argv in dir and returns its pane's
// paneRef, which the supervisor polls and respawns and the server pastes into from
// then on (the session name alone would resolve to whatever pane an attached
// operator last focused).
//
// remain-on-exit is set IN THE SAME tmux COMMAND LIST as new-session, so a CLI
// that dies at once still leaves a dead pane to read its status from, rather than
// taking the session down with it (the real-tmux crash-loop test starts exactly
// such a CLI and reads its status).
//
// 🔴 THE PANE'S PROCESS IS THE CLI ITSELF, NEVER A SHELL AROUND IT. tmux runs an
// argv of ONE element through `sh -c` (two or more are exec'd directly), and
// whether that shell then execs the command is the shell's choice. ccd tells the
// primary CLI's own hooks from a nested CLI's by the pane's pid (proctree.go),
// so a lone argv is run as `env <it>`: env execs it, in the pane's own process.
func (t tmuxTerminal) Start(ctx context.Context, dir string, argv []string) (paneRef, error) {
	args := append([]string{"new-session", "-d", "-P", "-F", serverFormat + " #{pane_id}", "-s", t.target,
		"-x", "200", "-y", "50", "-c", dir}, execArgv(argv)...)
	args = append(args, ";", "set-option", "-w", "-t", t.target, "remain-on-exit", "on")
	out, err := t.output(ctx, args...)
	if err != nil {
		return paneRef{}, err
	}
	f := strings.Fields(out)
	if len(f) != 3 || !strings.HasPrefix(f[2], "%") {
		return paneRef{}, fmt.Errorf("tmux new-session printed %q, not %q", out, serverFormat+" #{pane_id}")
	}
	return paneRef{Server: f[0] + " " + f[1], Pane: f[2]}, nil
}

// Respawn runs argv in the dead pane again, after checking the pane is still
// ref's (errTerminalLost otherwise: a respawn on a recreated server would start a
// CLI in a pane ccd did not create). Without -k respawn-pane refuses a pane whose
// process is still running, so it can never kill a live CLI.
func (t tmuxTerminal) Respawn(ctx context.Context, ref paneRef, dir string, argv []string) error {
	if _, err := t.state(ctx, ref); err != nil {
		return err
	}
	return t.run(ctx, nil, append([]string{"respawn-pane", "-t", ref.Pane, "-c", dir}, execArgv(argv)...)...)
}

// execArgv is argv as tmux must be given it: behind `env`, which execs it, so
// the pane's process IS argv[0] (see Start) — and with CLAUDE_CODE_ENTRYPOINT
// removed from its environment. The CLI stamps its transcript's entries with
// that variable when it is set, and an `sdk-*` value inherited from whatever
// started ccd would label the interactive CLI as a `claude -p` run, which the
// primary's fallback prefers not to resume (newestPrimaryTranscript).
func execArgv(argv []string) []string {
	return append([]string{"env", "-u", "CLAUDE_CODE_ENTRYPOINT"}, argv...)
}

// PanePID is the pid of ref's pane process: the CLI (see Start).
func (t tmuxTerminal) PanePID(ctx context.Context, ref paneRef) (int, error) {
	if !strings.HasPrefix(ref.Pane, "%") || ref.Server == "" {
		return 0, fmt.Errorf("no pane yet")
	}
	out, err := t.output(ctx, "display-message", "-p", "-t", ref.Pane, serverFormat+" #{pane_id} #{pane_pid}")
	if err != nil {
		return 0, err
	}
	f := strings.Fields(out)
	if len(f) != 4 || f[0]+" "+f[1] != ref.Server || f[2] != ref.Pane {
		return 0, fmt.Errorf("%w: %s now reads %q", errTerminalLost, ref, out)
	}
	return strconv.Atoi(f[3])
}

// paneDeadFormat is the part of paneStateFormat parsePaneExit reads.
const paneDeadFormat = "#{pane_dead} #{pane_dead_status} #{pane_dead_signal}"

// Exited reads whether the pane's process has exited and, once tmux knows it, how
// — or errTerminalLost when the pane is gone or on another server.
func (t tmuxTerminal) Exited(ctx context.Context, ref paneRef) (paneExit, error) {
	return t.state(ctx, ref)
}

// parsePaneExit reads "<dead> <status> <signal>", where status and signal are
// EMPTY until tmux has reaped the process.
//
// 🔴 DEAD IS NOT THE SAME AS REAPED, AND READING THEM AS ONE RECORDS A CRASH AS
// A CLEAN EXIT. tmux marks the pane dead at pty EOF and fills the status only once
// it has waited for the process, so "1  " (dead, nothing known) is a real read —
// read as status 0, it is what made the crash-loop test record 0 for a CLI that
// exits 3. It is not always brief: tmux can miss the SIGCHLD altogether
// (supervisor.run, Reap). A process killed by a signal has an EMPTY status FOR
// EVER (tmux fills pane_dead_status only for WIFEXITED) and the signal in
// pane_dead_signal instead — a NUMBER on Linux (tmux's sig2name falls back to
// the number without BSD's sys_signame; measured 9 for SIGKILL on tmux 3.3a and
// 3.7c), a name elsewhere, which is kept as text with the status unknown.
// "Wait until status is set" would hang on a signal death and on a missed
// SIGCHLD alike; the supervisor bounds the unknown case instead
// (supervisor.statusGrace).
func parsePaneExit(out string) (paneExit, error) {
	f := strings.Split(strings.TrimRight(out, "\r\n"), " ")
	if len(f) != 3 || (f[0] != "0" && f[0] != "1") {
		return paneExit{}, fmt.Errorf("tmux display-message %q printed %q", paneDeadFormat, out)
	}
	if f[0] == "0" {
		return paneExit{}, nil
	}
	e := paneExit{Dead: true}
	switch {
	case f[1] != "":
		n, err := strconv.Atoi(f[1])
		if err != nil {
			return paneExit{}, fmt.Errorf("pane_dead_status %q: %v", f[1], err)
		}
		e.Known, e.Status = true, n
	case f[2] != "":
		e.Known, e.SignalText, e.Status = true, f[2], exitUnknown
		if n, err := strconv.Atoi(f[2]); err == nil && n > 0 {
			e.Signal, e.Status = n, 128+n
		}
	}
	return e, nil
}

// Reap runs a no-op job in the tmux server. The job's own SIGCHLD makes the
// server run its waitpid(WAIT_ANY) loop, which also reaps a pane process whose
// SIGCHLD it missed (supervisor.run says when and why). `run-shell` without -b
// returns once the job has finished.
func (t tmuxTerminal) Reap(ctx context.Context) error {
	return t.run(ctx, nil, "run-shell", "true")
}

// Alive reports whether the terminal is there: with no pane yet (zero ref), that
// the session exists; once there is one, that ref's pane exists on ref's server —
// a session of the same name on a recreated server is NOT alive. A dead pane is
// alive here: it is kept on purpose (remain-on-exit) while the supervisor restarts it.
func (t tmuxTerminal) Alive(ctx context.Context, ref paneRef) error {
	if ref == (paneRef{}) {
		return t.run(ctx, nil, "has-session", "-t", t.target)
	}
	_, err := t.state(ctx, ref)
	return err
}

// KillSession ends the session named by target (a lane's, lanes.go) and every
// process in it. A session that is already gone is not an error.
func (t tmuxTerminal) KillSession(ctx context.Context) error {
	// "=" makes the target an EXACT session name: tmux otherwise matches a
	// session-name target by prefix too.
	if err := t.run(ctx, nil, "has-session", "-t", "="+t.target); err != nil {
		return nil
	}
	return t.run(ctx, nil, "kill-session", "-t", "="+t.target)
}
