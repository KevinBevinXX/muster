package api

import (
	"context"
	"sync"
)

// EndStreams ends every server-sent-event stream this server serves — its own
// handlers' and any text/event-stream relayed by the /connect/ proxy — and any
// opened after it. It is idempotent and safe to call concurrently.
// cmd/muster-server calls it once at the top of its shutdown, before EITHER of
// its http.Servers (main and connect) begins to drain.
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
// Pinned by cmd/muster-server's TestShutdownEndsStreamsButLetsAnOrdinaryRequestFinish.
//
// 🔴 A STREAM PROXIED THROUGH /connect/ ENDS TOO — THIS WAS AN OPEN RESIDUAL.
// connectors.Proxy.Serve relays the upstream on the request's own context, so
// a long-lived upstream response was an active connection nothing here ended,
// on the main listener and the connect one alike. MEASURED at `f44c849` at
// component level: Shutdown ran to its 10 s deadline with one proxied stream
// open, against 0 s with none. The proxy now holds the SAME signal
// (connectors.Proxy.StreamsEnd, set in UseExtensions) and ends a relayed
// exchange when it fires — but only a GET answered with text/event-stream,
// because the proxy also carries requests the drain must let finish: a git
// fetch's pack, and a POST that streams one finite, unresumable answer as
// text/event-stream (an LLM call, an MCP tool call). Why that pair is the test,
// and what it still does not end, is on connectors.endsAtShutdown.
// Pinned through buildApp + Run (not the built binary) by cmd/muster-server's
// TestShutdownEndsAProxiedEventStreamOnBothListeners,
// TestShutdownLetsAnOrdinaryProxiedRequestFinish and
// TestShutdownLetsAProxiedPOSTEventStreamFinish.
//
// It is a second registry beside terminalSet deliberately, not a missed reuse:
// terminalSet.endAll also refuses new entries and waits for each release, and
// a stream needs neither — sharing it would change how terminals shut down.
func (s *Server) EndStreams() { s.streams.end() }

// streamContext returns a context for an SSE handler's loop: parent's, also
// cancelled by EndStreams. The caller must call cancel when the handler returns.
//
// Every handler in this package that sets the literal "text/event-stream" must
// take its loop's context from here rather than from r.Context() —
// TestEveryEventStreamHandlerEndsOnShutdown holds that as a ledger, and that is
// all it sees: a stream relayed by the /connect/ proxy sets no such literal and
// ends through connectors.Proxy.StreamsEnd instead, pinned behaviourally.
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
