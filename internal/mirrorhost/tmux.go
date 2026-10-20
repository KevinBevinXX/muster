package mirrorhost

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// Tmux is the real Backend: the host's own tmux server.
type Tmux struct {
	// Bin is the tmux binary ("" = "tmux" on PATH).
	Bin string
	// Socket is tmux's -L socket NAME; SocketPath its -S socket PATH. Both empty
	// means tmux's default socket — the server an operator's plain `tmux` uses.
	Socket     string
	SocketPath string
	// Claude finds Claude Code in a session's pane (the rendered view); its
	// zero value reads /proc and the user's own ~/.claude. Its Proc is also
	// the proc root the Hermes runtime and the Hermes paste guard read.
	Claude ClaudeLocator
}

func (t Tmux) bin() string {
	if t.Bin != "" {
		return t.Bin
	}
	return "tmux"
}

func (t Tmux) server() []string {
	switch {
	case t.SocketPath != "":
		return []string{"-S", t.SocketPath}
	case t.Socket != "":
		return []string{"-L", t.Socket}
	}
	return nil
}

// query is the argv of a tmux command whose `-F` output this process PARSES:
// the server flags, behind `-u`.
//
// 🔴 `-u` IS LOAD-BEARING. Without a UTF-8 locale in its environment (a systemd
// unit with no LANG, a container, `LC_ALL=C`) tmux SANITISES what it prints for
// a command client: every non-printable byte becomes `_` — including the TAB
// these formats use as their field separator (measured on tmux 3.4 and 3.7c:
// `11_%0_283_main`). Every line then fails to parse, with no error: no Claude
// pane is seen, no active pane can be sent to, no label and no activity order —
// a host that simply looks like it has none. `-u` tells tmux the client is
// UTF-8 whatever the locale says, and the TABs arrive.
//
// Only for queries: an ATTACH keeps the locale's own answer, which decides how
// the session is drawn for that client.
func (t Tmux) query(args ...string) []string {
	return append(append([]string{"-u"}, t.server()...), args...)
}

// tmuxEnv is this process's environment WITHOUT $TMUX and $TMUX_PANE, plus a
// TERM the browser's terminal emulates.
//
// 🔴 $TMUX MUST GO. muster-mirror is often started from inside a tmux pane, and
// a tmux client that inherits $TMUX refuses to attach ("sessions should be
// nested with care") — every attach would fail with that line on screen. With
// no -L/-S, $TMUX would also redirect `list-sessions` to whatever server it
// names. $TMUX_TMPDIR is KEPT: it decides where the default socket lives, so it
// is part of naming "the operator's default server".
func tmuxEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "TMUX=") || strings.HasPrefix(kv, "TMUX_PANE=") || strings.HasPrefix(kv, "TERM=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "TERM=xterm-256color")
}

// List is the server's session names, sorted. A host with no tmux server
// running has no sessions — that is not an error.
func (t Tmux) List(ctx context.Context) ([]string, error) {
	cmd := exec.CommandContext(ctx, t.bin(), append(t.server(), "list-sessions", "-F", "#{session_name}")...)
	cmd.Env = tmuxEnv()
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := errb.String()
		if strings.Contains(msg, "no server running") || strings.Contains(msg, "error connecting to") {
			return []string{}, nil
		}
		return nil, fmt.Errorf("tmux list-sessions: %v: %s", err, strings.TrimSpace(msg))
	}
	list := []string{}
	for _, line := range strings.Split(out.String(), "\n") {
		// A viewer session is muster-mirror's own (perwindow.go), never a session
		// to mirror.
		if line = strings.TrimRight(line, "\r"); line != "" && !mirror.IsViewSession(line) {
			list = append(list, line)
		}
	}
	sort.Strings(list)
	return list, nil
}

// AttachArgs is the tmux argv (after the binary) for attaching session.
//
//   - `-u`: the browser terminal is UTF-8 whatever this host's locale is.
//   - `-r` for a read-only attach: tmux ignores the client's keys, the last of
//     the three read-only layers (see mirror.Hub.Attach). In tmux >= 3.2 it also
//     sets the client's ignore-size flag, so a read-only viewer does not resize
//     a window another client is using.
//   - `-t =<session>`: `=` is an EXACT match. A bare name is a prefix match, so
//     `main` would attach `main2` when `main` had ended.
//
// The session is one argv element, never a shell word.
func (t Tmux) AttachArgs(session string, readOnly bool) []string {
	args := append(t.server(), "-u", "attach-session")
	if readOnly {
		args = append(args, "-r")
	}
	return append(args, "-t", "="+session)
}

// Attach runs `tmux attach-session` for session on a fresh pty.
func (t Tmux) Attach(ctx context.Context, session string, readOnly bool, cols, rows uint16) (Terminal, error) {
	if err := mirror.ValidSession(session); err != nil {
		return nil, err
	}
	return t.client(t.AttachArgs(session, readOnly), cols, rows, nil)
}

// client starts a tmux client with argv args on a fresh pty of the given size;
// after runs once the client has ended.
func (t Tmux) client(args []string, cols, rows uint16, after func()) (Terminal, error) {
	master, slave, err := openPTY()
	if err != nil {
		return nil, err
	}
	if cols == 0 || rows == 0 {
		cols, rows = 80, 24
	}
	if err := setSize(master, cols, rows); err != nil {
		_ = master.Close()
		_ = slave.Close()
		return nil, err
	}
	cmd := exec.Command(t.bin(), args...)
	cmd.Env = tmuxEnv()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = ttyAttr()
	if err := cmd.Start(); err != nil {
		_ = master.Close()
		_ = slave.Close()
		return nil, fmt.Errorf("start tmux attach: %w", err)
	}
	_ = slave.Close()
	pt := &ptyTerm{master: master, cmd: cmd, exited: make(chan struct{}), after: after}
	go func() {
		_ = cmd.Wait()
		close(pt.exited)
	}()
	return pt, nil
}

// ptyTerm is one attached tmux client.
type ptyTerm struct {
	master *os.File
	cmd    *exec.Cmd
	exited chan struct{}
	once   sync.Once
	// after, when set, runs once the client has ended (Close): a window
	// viewer's own session is removed there.
	after func()
}

// Read is the client's screen. Linux reports the client's exit as EIO on the
// master; that is the end of the stream, not an error.
func (p *ptyTerm) Read(b []byte) (int, error) {
	n, err := p.master.Read(b)
	if err != nil && (errors.Is(err, syscall.EIO) || errors.Is(err, os.ErrClosed)) {
		err = io.EOF
	}
	return n, err
}

func (p *ptyTerm) Write(b []byte) (int, error) { return p.master.Write(b) }

func (p *ptyTerm) Resize(cols, rows uint16) error { return setSize(p.master, cols, rows) }

// Close ends the client: SIGHUP (what a closed terminal sends; the client
// detaches and exits), then SIGKILL if it has not exited within two seconds.
// The tmux SESSION is never touched — only this client.
func (p *ptyTerm) Close() error {
	p.once.Do(func() {
		_ = p.cmd.Process.Signal(syscall.SIGHUP)
		_ = p.master.Close()
		select {
		case <-p.exited:
		case <-time.After(2 * time.Second):
			_ = p.cmd.Process.Kill()
			<-p.exited
		}
		if p.after != nil {
			p.after()
		}
	})
	return nil
}

// ServerSocketer is a backend that can name its tmux server's socket — what a
// caller's $TMUX must name for the local MCP endpoint (MCPProxy) to believe its
// pane.
type ServerSocketer interface {
	ServerSocket(ctx context.Context) (string, error)
}

// ServerSocketOf is b's ServerSocketer.
func ServerSocketOf(b Backend) (ServerSocketer, bool) { return capabilityOf[ServerSocketer](b) }

// ServerSocket is the socket path of the server this backend lists.
func (t Tmux) ServerSocket(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, t.bin(), t.query("display-message", "-p", "#{socket_path}")...)
	cmd.Env = tmuxEnv()
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("tmux display-message: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
