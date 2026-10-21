package agentprovision

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/termsize"
)

// TerminalSizeTimeout bounds the window-size query an attach makes first. A
// slow answer is "size unknown", never a slow terminal.
const TerminalSizeTimeout = 3 * time.Second

// windowSize asks the agent's tmux how big its window is, as the client size
// that shows it exactly (agents.TerminalSizeCommand). Zero means unknown — no
// session yet, a driver that cannot exec, a slow pod — and the attach then
// proceeds exactly as it did before the no-shrink rule.
func (a *Adapter) windowSize(ctx context.Context, ref provision.Ref) termsize.Size {
	qctx, cancel := context.WithTimeout(ctx, TerminalSizeTimeout)
	defer cancel()
	var out bytes.Buffer
	if err := provision.Exec(qctx, a.driver, ref, agents.TerminalSizeCommand(termsize.TmuxWindowFormat), strings.NewReader(""), &out, io.Discard); err != nil {
		a.log.Printf("agents: terminal %s: window size unknown (%v); the browser's size is used as asked", ref.Name, err)
		return termsize.Size{}
	}
	s, err := termsize.ParseTmuxWindow(out.String())
	if err != nil {
		a.log.Printf("agents: terminal %s: window size unknown (%v)", ref.Name, err)
		return termsize.Size{}
	}
	return s
}

// noShrink applies internal/termsize's rule to one agent terminal: every size
// the browser sends (the initial one included) goes to the exec as
// termsize.Decide(size, current), and whenever that differs from what was asked
// the browser is told — in-band, ahead of the next output — what its client
// really is. win is the window's size before the attach; unknown leaves stdout
// and resize untouched (the previous behaviour, and the page then fits itself).
//
// current starts as the window's size and then follows what THIS attach set:
// muster does not re-query the pod on every resize (an exec each), so a window
// another browser grows meanwhile is not seen until the next attach — the
// worst case is a client smaller than the window, which tmux pans, never a
// shrunk window.
func noShrink(ctx context.Context, win termsize.Size, stdout io.Writer, in <-chan provision.TermSize) (io.Writer, <-chan provision.TermSize) {
	if !win.Known() {
		return stdout, in
	}
	w := &reportingWriter{w: stdout}
	out := make(chan provision.TermSize, cap(in)+1)
	cur := win
	// The bridge queues the browser's initial size before the attach; decide
	// it NOW so the exec starts at the decided size, and report it (always:
	// the report is also how the page learns this side follows the rule).
	select {
	case sz := <-in:
		cur = termsize.Decide(termsize.Size{Cols: sz.Cols, Rows: sz.Rows}, win)
		out <- provision.TermSize{Cols: cur.Cols, Rows: cur.Rows}
	default:
		out <- provision.TermSize{Cols: cur.Cols, Rows: cur.Rows}
	}
	w.owe(termsize.Report(cur))
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case sz := <-in:
				req := termsize.Size{Cols: sz.Cols, Rows: sz.Rows}
				got := termsize.Decide(req, cur)
				if got != cur {
					cur = got
					select {
					case out <- provision.TermSize{Cols: got.Cols, Rows: got.Rows}:
					case <-ctx.Done():
						return
					}
				}
				if got != req {
					w.owe(termsize.Report(got))
				}
			}
		}
	}()
	return w, out
}

// reportingWriter writes an owed size report, as a write of its own, ahead of
// the next output. Ahead of the FIRST output too, never before it: the socket
// bridge announces "connected" on the first bytes it relays, which must be the
// session's, not muster's.
type reportingWriter struct {
	mu   sync.Mutex
	w    io.Writer
	owed []byte
}

func (r *reportingWriter) owe(b []byte) {
	r.mu.Lock()
	r.owed = b
	r.mu.Unlock()
}

func (r *reportingWriter) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.owed) > 0 && len(p) > 0 {
		if _, err := r.w.Write(r.owed); err != nil {
			return 0, err
		}
		r.owed = nil
	}
	return r.w.Write(p)
}
