package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// THE LIVE TERMINAL OF A claude-code AGENT.
//
// GET /agents/{name}/terminal is the page; GET /agents/{name}/terminal/ws is
// the socket that bridges the browser's terminal to a pty exec in the agent's
// pod running `tmux attach` on the session ccd drives
// (Provisioner.AttachTerminal).
//
// 🔴 THIS IS THE FIRST ROUTE IN muster THAT REACHES A TERMINAL, AND auth.go's
// HISTORY SAYS muster HAD NONE. That was true of the carve: the permission
// router's send-keys surface onto the OPERATOR'S terminals stayed on its side.
// This one reaches only an agent's own session, inside that agent's pod, and it
// sits behind exactly the door the chat socket beside it does — requireSession,
// the human tier — and NO OTHER. The machine tier's bearer token (hook token)
// does not open it: TestTheTerminalSocketRefusesTheHookToken.
//
// 🔴 READ-ONLY IS THE DEFAULT AND IT IS ENFORCED TWICE. Anything but ?mode=rw
// attaches read-only, so a missing or mangled parameter cannot widen it. A
// read-only terminal (1) attaches tmux with -r, which ignores the client's keys,
// and (2) has NO input forwarded to it at all: this handler drops every input
// frame, so even a tmux that honoured them would receive nothing.
//
// The wire, both directions over one socket:
//   - binary frames are terminal bytes (browser→pod: keys; pod→browser: screen);
//   - text frames are JSON control messages: browser→server
//     {"type":"resize","cols":C,"rows":R}; server→browser {"type":"ready",
//     "mode":"ro"|"rw"} before the terminal's first output, then
//     {"type":"error","message":…} or {"type":"exit","message":…} as the last
//     message before the close.
// ---------------------------------------------------------------------------

const (
	terminalModeReadOnly    = "ro"
	terminalModeInteractive = "rw"

	// terminalPingInterval keeps an idle terminal's socket alive through
	// proxies that close quiet connections (a read-only view of an idle TUI
	// can send nothing for minutes), and detects a peer that vanished without
	// a close frame — a phone that lost signal — so its exec does not stay
	// attached until TCP gives up.
	terminalPingInterval = 25 * time.Second
	// terminalWriteTimeout bounds one write to the browser. A stalled browser
	// ends the exec rather than backing the pod's output up into muster.
	terminalWriteTimeout = 10 * time.Second
	// terminalMaxDim clamps a client-sent size; tmux has its own limits and a
	// browser has no business sending more.
	terminalMaxDim = 1000
)

// handleAgentTerminalPage serves the terminal page for a claude-code agent.
func (s *Server) handleAgentTerminalPage(w http.ResponseWriter, r *http.Request) {
	a, err := s.ext.Agents.GetByName(r.Context(), r.PathValue("name"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if agents.ResolveKind(a.Kind) != agents.KindClaudeCode {
		http.Error(w, fmt.Sprintf("agent %q is a %s agent, which has no terminal", a.Name, agents.KindLabel(a.Kind)), http.StatusNotFound)
		return
	}
	v := ui.AgentTerminalView{Name: a.Name, DisplayName: a.DisplayName}
	if s.ext.Provisioner == nil {
		v.Unavailable = "this build has no agent provisioner wired, so it cannot attach a terminal"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := ui.AgentTerminalPage(v).Render(w); err != nil {
		s.logger.Printf("agents: render terminal page: %v", err)
	}
}

// handleAgentTerminalWS bridges a browser socket to the agent's terminal.
func (s *Server) handleAgentTerminalWS(w http.ResponseWriter, r *http.Request) {
	a, err := s.ext.Agents.GetByName(r.Context(), r.PathValue("name"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// Refused BEFORE the upgrade, while a status code can still say why.
	if err := agents.CheckTerminal(a.Kind); err != nil {
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	}
	q := r.URL.Query()
	readOnly := q.Get("mode") != terminalModeInteractive
	initial, hasInitial := terminalSize(q.Get("cols"), q.Get("rows"))

	// Same-origin only, exactly as the chat socket: a cross-site page cannot
	// open an authenticated terminal in a logged-in operator's browser.
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: s.wsAllowedOrigins(r)})
	if err != nil {
		s.logger.Printf("agents: terminal ws accept: %v", err)
		return
	}
	defer c.CloseNow()

	// Registered so a shutdown can end it: a hijacked connection is outside
	// http.Server.Shutdown's reach, and a process that exits with a terminal
	// open skips that terminal's release (Provisioner.AttachTerminal).
	ctx, done, ok := s.terminals.add(r.Context())
	if !ok {
		_ = c.Close(websocket.StatusGoingAway, "muster is shutting down")
		return
	}
	defer done()

	mode := terminalModeInteractive
	if readOnly {
		mode = terminalModeReadOnly
	}
	s.logger.Printf("agents: terminal %s attached (%s)", a.Name, mode)
	err = bridgeTerminal(ctx, s.logger, c, readOnly, initial, hasInitial,
		func(ctx context.Context, stdin io.Reader, stdout io.Writer, resize <-chan provision.TermSize) error {
			return s.ext.Provisioner.AttachTerminal(ctx, a, readOnly, stdin, stdout, resize)
		})
	s.logger.Printf("agents: terminal %s detached: %v", a.Name, errOrNone(err))
}

// EndTerminals ends every live agent terminal, refuses new ones, and waits up
// to timeout for their handlers to return — which is after each terminal's
// release has run (Provisioner.AttachTerminal returns only then). It reports
// whether they all did. Call during graceful shutdown.
//
// 🔴 WITHOUT IT A REDEPLOY LEAKS EVERY OPEN TERMINAL'S tmux CLIENT. A terminal's
// socket is hijacked, so http.Server.Shutdown neither cancels nor waits for
// it; the process would exit with the handler mid-bridge and the release that
// ends the client in the agent's pod would never run.
func (s *Server) EndTerminals(timeout time.Duration) bool {
	return s.terminals.endAll(s.logger, timeout)
}

// terminalSet tracks live terminals. The zero value is ready to use.
type terminalSet struct {
	mu      sync.Mutex
	closed  bool
	next    uint64
	cancels map[uint64]context.CancelFunc
	wg      sync.WaitGroup
}

// add registers a terminal whose context is parent's, also cancelled by
// endAll. ok is false once endAll has run; otherwise done must be called when
// the terminal's handler returns.
func (t *terminalSet) add(parent context.Context) (ctx context.Context, done func(), ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, nil, false
	}
	if t.cancels == nil {
		t.cancels = map[uint64]context.CancelFunc{}
	}
	ctx, cancel := context.WithCancel(parent)
	id := t.next
	t.next++
	t.cancels[id] = cancel
	t.wg.Add(1)
	return ctx, func() {
		t.mu.Lock()
		delete(t.cancels, id)
		t.mu.Unlock()
		cancel()
		t.wg.Done()
	}, true
}

func (t *terminalSet) endAll(logger *log.Logger, timeout time.Duration) bool {
	t.mu.Lock()
	t.closed = true
	for _, cancel := range t.cancels {
		cancel()
	}
	t.mu.Unlock()
	finished := make(chan struct{})
	safeGo(logger, "terminal drain", func() {
		t.wg.Wait()
		close(finished)
	})
	select {
	case <-finished:
		return true
	case <-time.After(timeout):
		return false
	}
}

func errOrNone(err error) string {
	if err == nil {
		return "exited"
	}
	return err.Error()
}

// terminalSize parses a client-sent size, clamped; ok is false when either is
// missing or not a positive number.
func terminalSize(cols, rows string) (provision.TermSize, bool) {
	c, err1 := strconv.Atoi(cols)
	r, err2 := strconv.Atoi(rows)
	if err1 != nil || err2 != nil || c < 1 || r < 1 {
		return provision.TermSize{}, false
	}
	return provision.TermSize{Cols: uint16(min(c, terminalMaxDim)), Rows: uint16(min(r, terminalMaxDim))}, true
}

// attachFunc is the terminal on the far side of the bridge.
type attachFunc func(ctx context.Context, stdin io.Reader, stdout io.Writer, resize <-chan provision.TermSize) error

// bridgeTerminal runs attach with its stdin, stdout and resize fed from c, and
// closes c when attach returns. It returns attach's error.
//
// 🔴 EVERY EXIT PATH ENDS THE OTHER SIDE, AND THAT IS THE TEARDOWN CONTRACT.
// The browser closing (a closed tab, a lost phone) ends the read loop, which
// cancels attach's context; so do a failed keepalive and a shutdown
// (EndTerminals). attach returning (the client exited, or the exec was
// refused) closes the socket. Nothing here outlives the call:
// TestClosingTheTerminalSocketEndsTheExecAndLeaksNothing.
//
// ⚠ CANCELLING attach's CONTEXT CLOSES THE EXEC STREAM, AND THAT DOES NOT END
// tmux's CLIENT IN THE POD: the container runtime keeps an exec'd process
// running after its stream is gone. Ending it is attach's own job
// (Provisioner.AttachTerminal releases it before returning), so this function
// waits for attach to return rather than abandoning it.
func bridgeTerminal(parent context.Context, logger *log.Logger, c *websocket.Conn, readOnly bool, initial provision.TermSize, hasInitial bool, attach attachFunc) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	stdinR, stdinW := io.Pipe()
	resize := make(chan provision.TermSize, 8)
	if hasInitial {
		resize <- initial
	}

	readDone := make(chan struct{})
	safeGo(logger, "terminal read loop", func() {
		defer close(readDone)
		defer cancel()
		defer stdinW.Close()
		for {
			typ, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			switch typ {
			case websocket.MessageBinary:
				// 🔴 THE FIRST LAYER OF READ-ONLY: nothing typed is forwarded.
				if readOnly {
					continue
				}
				if _, err := stdinW.Write(data); err != nil {
					return
				}
			case websocket.MessageText:
				var msg struct {
					Type string `json:"type"`
					Cols int    `json:"cols"`
					Rows int    `json:"rows"`
				}
				if json.Unmarshal(data, &msg) != nil || msg.Type != "resize" {
					continue
				}
				sz, ok := terminalSize(strconv.Itoa(msg.Cols), strconv.Itoa(msg.Rows))
				if !ok {
					continue
				}
				// Never block the read loop on a slow consumer: a newer size
				// supersedes an unread one.
				select {
				case resize <- sz:
				default:
					select {
					case <-resize:
					default:
					}
					select {
					case resize <- sz:
					default:
					}
				}
			}
		}
	})

	pingDone := make(chan struct{})
	safeGo(logger, "terminal keepalive", func() {
		defer close(pingDone)
		t := time.NewTicker(terminalPingInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				pctx, pcancel := context.WithTimeout(ctx, terminalWriteTimeout)
				err := c.Ping(pctx)
				pcancel()
				if err != nil {
					cancel()
					return
				}
			}
		}
	})

	mode := terminalModeInteractive
	if readOnly {
		mode = terminalModeReadOnly
	}
	err := attach(ctx, stdinR, &terminalWriter{ctx: ctx, c: c, mode: mode}, resize)
	_ = stdinR.Close()

	// The last word, unless the browser is already gone.
	if ctx.Err() == nil {
		if err != nil {
			_ = terminalWriteJSON(ctx, c, map[string]string{"type": "error", "message": terminalErrorText(err)})
			_ = c.Close(websocket.StatusInternalError, "terminal refused")
		} else {
			_ = terminalWriteJSON(ctx, c, map[string]string{"type": "exit", "message": "the terminal's session client exited"})
			_ = c.Close(websocket.StatusNormalClosure, "terminal exited")
		}
	}
	cancel()
	_ = c.CloseNow()
	<-readDone
	<-pingDone
	return err
}

// terminalErrorText is what the browser is told. The refusals carry their own
// remedy (the RBAC rule, the kind, the driver), so they are passed through.
func terminalErrorText(err error) string {
	switch {
	case errors.Is(err, agents.ErrNoTerminal), errors.Is(err, provision.ErrUnsupported),
		errors.Is(err, provision.ErrNotFound):
		return err.Error()
	}
	return "the terminal ended with an error: " + err.Error()
}

func terminalWriteJSON(ctx context.Context, c *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, terminalWriteTimeout)
	defer cancel()
	return c.Write(wctx, websocket.MessageText, b)
}

// terminalWriter sends the terminal's output to the browser as binary frames.
//
// 🔴 "ready" IS SENT ON THE FIRST OUTPUT, NOT WHEN THE SOCKET OPENS. The first
// byte from the terminal is the first evidence that anything attached (tmux
// draws the session immediately); announcing "connected" before that would
// show a connected terminal over an exec the cluster then refuses.
type terminalWriter struct {
	ctx   context.Context
	c     *websocket.Conn
	mode  string
	ready bool
}

func (w *terminalWriter) Write(p []byte) (int, error) {
	if !w.ready {
		w.ready = true
		if err := terminalWriteJSON(w.ctx, w.c, map[string]string{"type": "ready", "mode": w.mode}); err != nil {
			return 0, err
		}
	}
	wctx, cancel := context.WithTimeout(w.ctx, terminalWriteTimeout)
	defer cancel()
	if err := w.c.Write(wctx, websocket.MessageBinary, p); err != nil {
		return 0, err
	}
	return len(p), nil
}
