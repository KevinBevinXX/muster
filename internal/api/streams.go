package api

import (
	"context"
	"sync"
)

// EndStreams ends every open server-sent-event stream, and any opened after it.
// It is idempotent and safe to call concurrently. cmd/muster-server registers it
// with http.Server.RegisterOnShutdown, so it runs the moment Shutdown begins.
//
// 🔴 WITHOUT IT EVERY SHUTDOWN WITH A BROWSER OPEN COST THE WHOLE DRAIN BUDGET.
// http.Server.Shutdown closes idle connections and WAITS for active ones, and an
// SSE stream is never idle: its handler returns only when the client goes away
// or the request context is cancelled, and Shutdown cancels no request context.
// Every page's shell opens /events, so one open tab held the process for the
// full shutdownDrainTimeout and then exited on the deadline with the "drain did
// not complete" line. MEASURED at `a75b09b` against the built binary on SIGTERM:
// ~10,010 ms with one /events stream open, under 100 ms with none.
//
// ⚠ IT ENDS STREAMS ONLY, AND THAT IS THE POINT. The tempting fix — a
// BaseContext cancelled on shutdown — would cancel EVERY in-flight request,
// turning the drain into an abort for the ordinary requests it exists to let
// finish. A stream has no "finished" state to drain to, so ending it loses
// nothing: the EventSource reconnects to whichever process answers next.
//
// It is a second registry beside terminalSet deliberately, not a missed reuse:
// terminalSet.endAll also refuses new entries and waits for each release, and
// a stream needs neither — sharing it would change how terminals shut down.
func (s *Server) EndStreams() { s.streams.end() }

// streamContext returns a context for an SSE handler's loop: parent's, also
// cancelled by EndStreams. The caller must call cancel when the handler returns.
//
// Every handler that answers text/event-stream must take its loop's context
// from here rather than from r.Context() — TestEveryEventStreamHandlerEndsOnShutdown
// holds that as a ledger.
func (s *Server) streamContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	// AfterFunc, not a watcher goroutine: nothing runs per stream until the end
	// is signalled, and stop unregisters it when the handler returns first.
	stop := context.AfterFunc(s.streams.context(), cancel)
	return ctx, func() { stop(); cancel() }
}

// streamEnd is a cancel-once signal. The zero value is ready to use, so a
// Server built as a literal (as some tests do) still has one.
type streamEnd struct {
	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
}

func (e *streamEnd) context() context.Context {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ctx == nil {
		e.ctx, e.cancel = context.WithCancel(context.Background())
	}
	return e.ctx
}

func (e *streamEnd) end() {
	e.context()
	e.cancel() // idempotent
}
