package mirrorhost

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// THE LOCAL MCP ENDPOINT: how a Claude Code session in one of this host's tmux
// windows reaches muster's attention and task tools (muster's POST /mirror/mcp).
//
// A Claude Code session holds no muster credential. This host does — its
// mirror token — so muster-mirror serves MCP on a LOOPBACK address and
// forwards each request to muster with the token and the CALLING session's
// Claude session id. muster believes that id only if this host lists a window
// running it now (internal/api's mirrorSessionOf), so the most a caller here
// can do is act as one of this host's own sessions.
//
// 🔴 LOOPBACK ONLY, AND A SECRET EVEN THERE. ListenMCP refuses any address
// that is not a literal loopback IP, so nothing off this machine reaches it.
// Another local USER could still reach a loopback port, so every request must
// carry the per-host secret (Authorization: Bearer <secret>), kept in a 0600
// file only this user can read (EnsureMCPSecret). A request carrying an Origin
// is refused: a web page cannot drive it, even one that guessed the secret.
//
// HOW A SESSION NAMES ITSELF. The MCP client config sends X-Muster-Pane:
// ${TMUX_PANE:-} and X-Muster-Tmux: ${TMUX:-} — Claude Code inherits both from
// its pane and expands them in the config's headers — and the proxy maps that
// pane, in that tmux server, to the Claude session running in it NOW (a /clear
// or restart changes the id; the pane stays). Any pane of a window counts, not
// only its active one.

// MCPPaneHeader and MCPTmuxHeader are how a caller names itself: its pane and
// its tmux server. MCPSessionHeader is what the proxy tells muster.
const (
	MCPPaneHeader    = "X-Muster-Pane"
	MCPTmuxHeader    = "X-Muster-Tmux"
	MCPSessionHeader = "X-Muster-Claude-Session"
	// MCPPath is muster's mirrored-sessions MCP door.
	MCPPath = "/mirror/mcp"
	// mcpMaxBody bounds one forwarded JSON-RPC message (muster's own bound).
	mcpMaxBody = 64 << 10
)

// MCPProxy is the local endpoint's handler.
type MCPProxy struct {
	// URL is muster's base URL (as Config.URL); Token this host's mirror token.
	URL   string
	Token string
	// AllowHTTP permits a plain http:// URL (tests and local development).
	AllowHTTP bool
	// Secret is what a local caller must present.
	Secret string
	// Windows lists this host's windows: how a pane is resolved to its Claude
	// session. nil resolves none.
	Windows WindowLister
	// Socket is this host's tmux server: a caller's $TMUX must name it. nil
	// (a backend that is not tmux) skips the check.
	Socket ServerSocketer
	// Client sends the forwarded request; nil is a client with no overall
	// timeout (attention_wait long-polls for up to ten minutes).
	Client *http.Client
	Logger *log.Logger
}

// MCPTarget is muster's /mirror/mcp URL for base.
func MCPTarget(base string, allowHTTP bool) (string, error) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil {
		return "", fmt.Errorf("muster-mirror: bad muster URL %q: %w", base, err)
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && allowHTTP:
	default:
		return "", fmt.Errorf("muster-mirror: muster URL %q must be https:// (or http:// with -allow-http)", base)
	}
	if u.Host == "" {
		return "", fmt.Errorf("muster-mirror: muster URL %q has no host", base)
	}
	u.Path = strings.TrimRight(u.Path, "/") + MCPPath
	u.RawQuery, u.Fragment = "", ""
	return u.String(), nil
}

func (p *MCPProxy) logf(format string, args ...any) {
	if p.Logger != nil {
		p.Logger.Printf(format, args...)
	}
}

func mcpRefuse(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = fmt.Fprintf(w, "{\"error\":%q}\n", msg)
}

// session is the calling Claude session's id, resolved from its tmux pane —
// ANY pane of a mirrored window, the active one or another in a split — after
// checking the caller's tmux server is the one this host mirrors (pane ids are
// per server: %3 on another server is another pane).
func (p *MCPProxy) session(ctx context.Context, r *http.Request) (string, error) {
	pane := strings.TrimSpace(r.Header.Get(MCPPaneHeader))
	if pane == "" {
		return "", fmt.Errorf("name the calling session: send %s: ${TMUX_PANE:-} from inside the mirrored tmux server", MCPPaneHeader)
	}
	if p.Windows == nil {
		return "", errors.New("this host cannot resolve a tmux pane")
	}
	if p.Socket != nil {
		want, err := p.Socket.ServerSocket(ctx)
		if err != nil {
			return "", fmt.Errorf("read this host's tmux server: %w", err)
		}
		// $TMUX is "<socket path>,<server pid>,<session index>".
		got, _, _ := strings.Cut(strings.TrimSpace(r.Header.Get(MCPTmuxHeader)), ",")
		if got == "" || got != want {
			return "", fmt.Errorf("the caller is not in the tmux server this host mirrors (send %s: ${TMUX:-})", MCPTmuxHeader)
		}
	}
	ws, err := p.Windows.Windows(ctx)
	if err != nil {
		return "", fmt.Errorf("list windows: %w", err)
	}
	for _, w := range ws {
		for _, c := range append([]ClaudePane{derefClaude(w.Claude)}, w.Others...) {
			if c.Pane == pane && c.Gen != "" {
				return c.Gen, nil
			}
		}
	}
	return "", fmt.Errorf("no Claude Code session runs in pane %s", pane)
}

func derefClaude(c *ClaudePane) ClaudePane {
	if c == nil {
		return ClaudePane{}
	}
	return *c
}

func (p *MCPProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/mcp" {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Origin") != "" {
		mcpRefuse(w, http.StatusForbidden, "cross-origin requests are refused")
		return
	}
	presented, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if p.Secret == "" || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(presented)), []byte(p.Secret)) != 1 {
		mcpRefuse(w, http.StatusUnauthorized, "this endpoint takes this host's local MCP secret (Authorization: Bearer …)")
		return
	}
	if r.Method != http.MethodPost {
		// The transport's "no server-initiated stream" answer, as muster gives it.
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	sid, err := p.session(r.Context(), r)
	if err != nil {
		mcpRefuse(w, http.StatusBadRequest, err.Error())
		return
	}
	target, err := MCPTarget(p.URL, p.AllowHTTP)
	if err != nil {
		mcpRefuse(w, http.StatusInternalServerError, err.Error())
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, mcpMaxBody))
	if err != nil {
		mcpRefuse(w, http.StatusRequestEntityTooLarge, "the request is too large")
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target, strings.NewReader(string(body)))
	if err != nil {
		mcpRefuse(w, http.StatusInternalServerError, err.Error())
		return
	}
	req.Header.Set("Authorization", "Bearer "+p.Token)
	req.Header.Set(MCPSessionHeader, sid)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	client := p.Client
	if client == nil {
		client = &http.Client{}
	}
	resp, err := client.Do(req)
	if err != nil {
		p.logf("muster-mirror: mcp: forward: %v", err)
		mcpRefuse(w, http.StatusBadGateway, "muster could not be reached")
		return
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	// Copy as it arrives: muster keeps a long call alive with whitespace, and
	// the caller's connection must see it to stay open too.
	rc := http.NewResponseController(w)
	buf := make([]byte, 32<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			_ = rc.Flush()
		}
		if rerr != nil {
			return
		}
	}
}

// ListenMCP listens on addr, which must be a literal loopback IP and port
// ("127.0.0.1:47823", "[::1]:47823").
func ListenMCP(addr string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("muster-mirror: -mcp-listen %q: %w", addr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("muster-mirror: -mcp-listen %q is not a loopback address; the MCP endpoint listens on 127.0.0.1 or ::1 only", addr)
	}
	return net.Listen("tcp", addr)
}

// ServeMCP serves h on l until ctx ends.
func ServeMCP(ctx context.Context, l net.Listener, h http.Handler) error {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// DefaultMCPSecretFile is where the local MCP secret is kept by default.
func DefaultMCPSecretFile() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "muster-mirror", "mcp-secret")
}

// EnsureMCPSecret reads the secret from path, creating it (32 random bytes,
// hex; the file 0600, its directory 0700) when it does not exist. A file
// readable by others is refused rather than used.
func EnsureMCPSecret(path string) (string, error) {
	if path == "" {
		return "", errors.New("muster-mirror: no MCP secret file (set -mcp-secret-file)")
	}
	if fi, err := os.Stat(path); err == nil {
		if fi.Mode().Perm()&0o077 != 0 {
			return "", fmt.Errorf("muster-mirror: %s is readable by others (mode %v); chmod 600 it", path, fi.Mode().Perm())
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		s := strings.TrimSpace(string(b))
		if len(s) < 32 {
			return "", fmt.Errorf("muster-mirror: %s holds no usable secret", path)
		}
		return s, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	secret := hex.EncodeToString(b[:])
	// 🔴 ATOMIC: written whole to a private temp file, then LINKED into place,
	// which fails if the file exists. No reader ever sees a partial or empty
	// secret, and of two hosts starting at once exactly one secret wins — the
	// loser reads it.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".mcp-secret-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return "", err
	}
	if _, err := tmp.WriteString(secret + "\n"); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Link(tmp.Name(), path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return EnsureMCPSecret(path)
		}
		return "", err
	}
	return secret, nil
}
