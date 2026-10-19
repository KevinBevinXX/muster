package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
)

// THE GATEWAY RUNTIME BESIDE ccd (muster's agentspec/chief_sidecar.go).
//
// A claude-code chief's pod also runs the model-gateway runtime, on LOOPBACK
// only. muster sends a turn for a chat session on that runtime to ccd's port —
// the only port the pod's network policy lets muster reach — with
// X-Muster-Runtime: gateway, and ccd hands the request, unread, to the runtime
// beside it, streaming the answer back.
//
// 🔴 ccd CHECKS THE BEARER FIRST, AND THE RUNTIME CHECKS IT AGAIN. Both are
// handed the same derived credential (MUSTER_GATEWAY_BEARER), so a request ccd
// forwards carries the Authorization it arrived with, and a request that did not
// pass ccd's check never reaches the runtime at all.
//
// The headers that only ccd reads (X-Muster-*) are dropped on the way; the
// session key and the model override (the runtime's own headers) pass through,
// so the runtime keeps one conversation per muster session and runs each on the
// session's model.

const (
	headerRuntime  = "X-Muster-Runtime"
	runtimeGateway = "gateway"
)

// newGatewayProxy builds the hand-off to upstream, which must be a loopback
// http URL: the runtime is never meant to be reachable any other way, and a
// non-loopback upstream would send a bearer-carrying request off the pod.
func newGatewayProxy(upstream string) (*httputil.ReverseProxy, error) {
	u, err := url.Parse(upstream)
	if err != nil || u.Scheme != "http" || u.Host == "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("ccd: CCD_GATEWAY_UPSTREAM=%q is not an http://host:port URL", upstream)
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, fmt.Errorf("ccd: CCD_GATEWAY_UPSTREAM=%q is not a loopback address; the gateway runtime beside "+
			"ccd is reached on loopback only", upstream)
	}
	p := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(u)
			for _, h := range []string{headerRuntime, headerCCSession, headerCCModel} {
				pr.Out.Header.Del(h)
			}
		},
		// The answer is an event stream: every event is passed on as it comes.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			writeFailure(w, &failure{Status: http.StatusServiceUnavailable, Type: failNotReady,
				Message: "the gateway runtime beside ccd did not answer (starting, or down): " + err.Error()})
		},
	}
	return p, nil
}

// isGatewayTurn reports whether r is for the gateway runtime.
func isGatewayTurn(r *http.Request) bool { return r.Header.Get(headerRuntime) == runtimeGateway }

// handleGatewayTurn hands an authenticated request to the gateway runtime.
func (s *server) handleGatewayTurn(w http.ResponseWriter, r *http.Request) {
	if !bearerOK(r, s.cfg.Bearer) {
		writeFailure(w, &failure{Status: http.StatusUnauthorized, Type: failUnauthorized, Message: "missing or wrong bearer"})
		return
	}
	if s.gateway == nil {
		writeFailure(w, &failure{Status: http.StatusServiceUnavailable, Type: failNotReady,
			Message: "this pod runs no gateway runtime beside ccd (CCD_GATEWAY_UPSTREAM is unset)"})
		return
	}
	s.gateway.ServeHTTP(w, r)
}

// handleChatCompletions serves muster's toolless fallback transport for the
// gateway runtime only; ccd itself speaks /v1/responses alone, and answers 404
// here as it always did.
func (s *server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if !isGatewayTurn(r) {
		http.NotFound(w, r)
		return
	}
	s.handleGatewayTurn(w, r)
}
