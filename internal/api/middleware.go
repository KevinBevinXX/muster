package api

import (
	"log"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/metrics"
)

// BuildVersion is the running build version, set from main via -ldflags
// "-X github.com/ZacxDev/muster/internal/api.BuildVersion=<ver>". Surfaced in
// /health and the startup log so the running build is identifiable.
var BuildVersion = "dev"

// telemetryConnectSrc, when non-empty, is an extra origin appended to the CSP
// connect-src directive so the browser may POST cross-origin RUM (the Grafana
// Faro collect endpoint lives on another host). Set once at startup.
var telemetryConnectSrc string

// SetTelemetryConnectSrc registers an additional connect-src CSP origin from a
// full URL (only its scheme://host is used). Without this, connect-src 'self'
// silently blocks every Faro POST and no telemetry is ever sent. Empty/invalid
// is a no-op (telemetry off ⇒ no CSP relaxation).
func SetTelemetryConnectSrc(rawURL string) {
	if rawURL == "" {
		return
	}
	if u, err := url.Parse(rawURL); err == nil && u.Scheme != "" && u.Host != "" {
		telemetryConnectSrc = u.Scheme + "://" + u.Host
	}
}

// withMiddleware wraps the mux with the cross-cutting middleware chain, outermost
// first: panic recovery (so a handler panic can never crash the single-replica
// process), security headers, then access logging.
func (s *Server) withMiddleware(next http.Handler) http.Handler {
	return s.recoverMiddleware(securityHeaders(s.accessLog(next)))
}

// recoverMiddleware turns a panic in any handler into a logged 500 instead of a
// process crash. muster runs a single replica fronting every Claude Code
// approval; one nil-deref must not take the gateway down.
func (s *Server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				metrics.Panics.WithLabelValues("http").Inc()
				s.logger.Printf("PANIC %s %s: %v\n%s", r.Method, r.URL.Path, rec, debug.Stack())
				// Best-effort 500; if the handler already wrote a header this is a no-op.
				w.WriteHeader(http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// safeGo runs fn in a goroutine with panic recovery, so a panic in background
// work (dispatch, kickoff, grant apply, push) is logged rather than crashing the
// process. Use in place of a bare `go func(){...}` for any goroutine that touches
// external data (k8s objects, gateway responses, the DB).
func safeGo(logger *log.Logger, what string, fn func()) {
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				metrics.Panics.WithLabelValues("goroutine").Inc()
				logger.Printf("PANIC in goroutine %s: %v\n%s", what, rec, debug.Stack())
			}
		}()
		fn()
	}()
}

// permissionsPolicy is the Permissions-Policy every response carries.
//
// 🔴 IT NAMES THE MICROPHONE, AND ONLY THE MICROPHONE, FOR THIS ORIGIN ALONE.
// The message box's dictation (internal/ui/dictate.go) records through
// getUserMedia; a top-level page of this origin may do that by the browser's
// default already, so this widens nothing — it pins the grant to `self`, which
// is what stops any frame of another origin from ever asking (the CSP's
// frame-ancestors 'none' already keeps muster out of other pages' frames). No
// other feature is listed, so none changes from the browser's default.
const permissionsPolicy = "microphone=(self)"

// securityHeaders sets conservative security headers on every response. The app
// is server-rendered HTML + vendored htmx/idiomorph + inline <script> blocks and
// `hx-on:` attributes. htmx compiles `hx-on:` handlers via `new Function`, so the
// script-src needs BOTH 'unsafe-inline' (inline scripts + hx-on attrs) AND
// 'unsafe-eval' (the Function constructor) or modal-close/form-reset break. The
// CSP still earns its keep by blocking external origins, framing (clickjacking),
// base-uri and form-action hijacking; Referrer-Policy:no-referrer keeps the
// magic-link token out of the Referer header.
func securityHeaders(next http.Handler) http.Handler {
	connectSrc := "'self'"
	if telemetryConnectSrc != "" {
		connectSrc += " " + telemetryConnectSrc
	}
	csp := "default-src 'self'; " +
		"script-src 'self' 'unsafe-inline' 'unsafe-eval'; " +
		"style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data:; " +
		"connect-src " + connectSrc + "; " +
		"frame-ancestors 'none'; " +
		"base-uri 'self'; " +
		"form-action 'self'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Permissions-Policy", permissionsPolicy)
		next.ServeHTTP(w, r)
	})
}

// accessLog logs one line per request (method, path, status, duration). It skips
// the high-frequency, low-value health + SSE endpoints to keep logs readable.
func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health", "/readyz", "/events":
			next.ServeHTTP(w, r)
			return
		}
		sw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(sw, r)
		dur := time.Since(start)
		metrics.ObserveHTTP(routeLabel(r), r.Method, sw.status, dur)
		s.logger.Printf("%s %s %d %s", r.Method, accessLogPath(r.URL), sw.status, dur.Round(time.Millisecond))
	})
}

// loggedPath is the path accessLog writes. Anything under /auth/claim/ is
// cut to that prefix: the claim token lives in the URL fragment now (never
// sent), but an old-style or hand-made /auth/claim/<token> request must still
// not put a credential into the log. Defence in depth.
//
// The same holds for the invite and reset links (accounts PR 5): their tokens
// ride in the fragment too, and a hand-made path form is cut the same way.
// accessLogPath is a request's path as the access log writes it.
//
// 🔴 REDACT ON THE DECODED PATH, THEN ESCAPE. Redaction must see the path the
// mux routes on — the DECODED one — or `/auth/%63laim/<token>` (which reaches
// the claim route) would log its token. And what is logged must be ESCAPED: a
// decoded path is attacker-chosen text, and a %0A in it would start a forged
// line of its own in this log.
func accessLogPath(u *url.URL) string {
	if red := loggedPath(u.Path); red != u.Path {
		return red // a fixed prefix plus "[redacted]": nothing of the request's
	}
	return u.EscapedPath()
}

func loggedPath(p string) string {
	for _, prefix := range []string{"/auth/claim/", "/invite/", "/auth/reset/"} {
		if strings.HasPrefix(p, prefix) {
			return prefix + "[redacted]"
		}
	}
	return p
}

// routeLabel returns the matched ServeMux pattern for the request as a bounded
// metric label, falling back to "unmatched" when no route matched (the 404 path,
// finally made visible). r.Pattern is set by the mux during ServeHTTP (Go 1.23+),
// so this MUST be read AFTER next.ServeHTTP returns. The raw path is deliberately
// NOT used — it carries ids/tokens and would blow up label cardinality.
func routeLabel(r *http.Request) string {
	if r.Pattern == "" {
		return "unmatched"
	}
	return r.Pattern
}

// statusRecorder captures the response status for access logging.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wroteHeader = true
	return r.ResponseWriter.Write(b)
}

// Unwrap exposes the underlying ResponseWriter so http.ResponseController (used
// for SSE/WebSocket Flush/Hijack) reaches the real writer through the recorder.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Flush implements http.Flusher so SSE/streaming handlers that type-assert
// w.(http.Flusher) directly (e.g. handleAgentLogsStream) still work when the
// response is wrapped for access logging — a bare type assertion does NOT follow
// Unwrap, so without this the assertion fails and the handler 500s. Delegates to
// the underlying writer's Flusher when it has one.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
