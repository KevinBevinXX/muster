package hermestest

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// GATEWAY is a fake Hermes gateway for one home, speaking what the pin's
// gateway speaks to a viewer (docs/design/hermes.md, "The gateway viewer"),
// as measured against the pin in Phase 4: the control socket's identify and
// session-ticket, and the WebSocket's session.resume, approval.respond and
// events. It answers every resume with the secret-bearing fields the real
// one sends (the conversation, pending[].text, session.info's system_prompt
// and credential_hint), each holding DecoyMarker, so a test can prove none
// of it leaves the viewer.

// Gateway is a running fake gateway.
type Gateway struct {
	t    testing.TB
	home string
	// Socket is the control socket's path: <home>/gateway.sock, or — when
	// that would not fit sun_path, as Hermes does — <short dir>/hermes-gw-
	// <hash>/control.sock with <home>/gateway.sock.path naming it.
	Socket string
	api    *httptest.Server
	ln     net.Listener

	mu sync.Mutex
	// Identify is what identify answers (mutable before a request).
	Identify map[string]any
	// Prompts are the approvals session.resume lists.
	Prompts []map[string]any
	// Responds are the approval.respond params received.
	Responds []map[string]any
	// RespondRepeat, when over 1, answers approval.respond that many times
	// with the same id (a misbehaving gateway).
	RespondRepeat int
	// RespondError, when set, is the error message approval.respond answers.
	RespondError string
	// Tickets are the tickets minted; Resumes the session ids resumed.
	Tickets []string
	Resumes []string
	// ServerAnswers counts responses a viewer sent to a server request
	// (sudo, secret, clarify): a passive viewer sends none.
	ServerAnswers int
	conns         []*websocket.Conn
	// NoSubprotocol makes the WebSocket accept without echoing
	// hermes-gateway-v1 (a gateway speaking another protocol).
	NoSubprotocol bool
	// TicketInstance, when set, is the instance a minted ticket names (a
	// ticket for another gateway).
	TicketInstance string
	// Attached counts the viewers connected now.
	attached int
}

// GatewayPin is the pin as the fake's identify names it by default.
const GatewayPin = "5fc490e2b34123212d73ef316086f939567a725a"

// NewGateway starts a fake gateway for the home at dir (which must exist,
// 0700): its control socket, and a loopback API for its WebSocket.
func NewGateway(t testing.TB, dir string) *Gateway {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	g := &Gateway{t: t, home: resolved}
	g.api = httptest.NewServer(http.HandlerFunc(g.serveWS))
	t.Cleanup(g.api.Close)
	g.Identify = map[string]any{
		"protocol": 1, "kind": "hermes-gateway", "pid": os.Getpid(), "code_sha": GatewayPin, "code_version": "0.0.0",
		"instance_id": "fakeinstance0001", "runtime_protocol": 1, "authority_epoch": 1, "state": "ready",
		"api_origin": g.api.URL, "hermes_home": resolved,
		"served_profiles": []any{map[string]any{"profile_id": resolved, "home": resolved}},
		"capabilities":    []any{"session-authority-v1", "durable-admission-v1", "event-replay-v1"},
	}
	g.Socket = filepath.Join(resolved, "gateway.sock")
	if len(g.Socket) > 100 {
		short, err := os.MkdirTemp("", "hgw")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(short) })
		sum := sha256.Sum256([]byte(resolved))
		d := filepath.Join(short, "hermes-gw-"+hex.EncodeToString(sum[:])[:16])
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
		g.Socket = filepath.Join(d, "control.sock")
		if err := os.WriteFile(filepath.Join(resolved, "gateway.sock.path"), []byte(g.Socket), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	g.ln, err = net.Listen("unix", g.Socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(g.Socket, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.ln.Close() })
	go g.serveControl()
	return g
}

// Close stops the gateway: its socket and API (a viewer's connection ends).
func (g *Gateway) Close() {
	_ = g.ln.Close()
	g.mu.Lock()
	for _, c := range g.conns {
		_ = c.CloseNow()
	}
	g.mu.Unlock()
	g.api.Close()
}

// Set changes one identify field.
func (g *Gateway) Set(key string, value any) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.Identify[key] = value
}

// AddPrompt lists an approval in session.resume.
func (g *Gateway) AddPrompt(id string, gen int, command, description string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.Prompts = append(g.Prompts, map[string]any{"kind": "approval", "prompt_id": id, "execution_generation": gen,
		"command": command, "description": description, "choices": []any{"once", "deny", "session", "always"}, "timeout_s": 300})
}

// ServerAnswersSeen is ServerAnswers, read under the lock.
func (g *Gateway) ServerAnswersSeen() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.ServerAnswers
}

// Viewers is how many viewers are connected now.
func (g *Gateway) Viewers() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.attached
}

// RespondsSeen is a copy of the approval.respond params received.
func (g *Gateway) RespondsSeen() []map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]map[string]any(nil), g.Responds...)
}

// Send sends a frame to every connected viewer.
func (g *Gateway) Send(frame map[string]any) {
	b, _ := json.Marshal(frame)
	g.mu.Lock()
	conns := append([]*websocket.Conn(nil), g.conns...)
	g.mu.Unlock()
	for _, c := range conns {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = c.Write(ctx, websocket.MessageText, b)
		cancel()
	}
}

// Event sends one session event to every viewer.
func (g *Gateway) Event(session, typ string, payload map[string]any) {
	g.Send(map[string]any{"jsonrpc": "2.0", "method": "event", "params": map[string]any{"type": typ, "session_id": session,
		"execution_generation": 1, "payload": payload}})
}

// SecretFrames sends every secret-bearing frame the real gateway streams to
// a viewer, each carrying DecoyMarker: session.info's system_prompt and
// credential_hint, a delta, a tool result, and the sudo, secret and clarify
// server requests.
func (g *Gateway) SecretFrames(session string) {
	g.Event(session, "session.info", map[string]any{"system_prompt": DecoyMarker, "credential_hint": DecoyMarker})
	g.Event(session, "message.delta", map[string]any{"text": DecoyMarker})
	g.Event(session, "tool.complete", map[string]any{"result": DecoyMarker, "args": map[string]any{"command": DecoyMarker}})
	for _, m := range []string{"sudo", "secret", "clarify"} {
		g.Send(map[string]any{"jsonrpc": "2.0", "id": "srq-" + m + "00000000", "method": m, "params": map[string]any{
			"session_id": session, "prompt": DecoyMarker, "questions": []any{map[string]any{"qid": "q0", "question": DecoyMarker}}}})
	}
	// An approval request whose description carries the marker IS forwarded
	// (it is the approval's own text, shown to its owner): tests that look
	// for leaks never send one.
}

func (g *Gateway) serveControl() {
	for {
		c, err := g.ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(5 * time.Second))
			line, err := bufio.NewReader(c).ReadBytes('\n')
			if err != nil {
				return
			}
			var req struct {
				Verb   string         `json:"verb"`
				ID     any            `json:"id"`
				Params map[string]any `json:"params"`
			}
			_ = json.Unmarshal(line, &req)
			resp := map[string]any{"ok": true, "protocol": 1, "id": req.ID}
			g.mu.Lock()
			switch req.Verb {
			case "identify":
				resp["result"] = g.Identify
			case "session-ticket":
				var b [16]byte
				_, _ = rand.Read(b[:])
				tk := hex.EncodeToString(b[:])
				g.Tickets = append(g.Tickets, tk)
				inst := req.Params["instance_id"]
				if g.TicketInstance != "" {
					inst = g.TicketInstance
				}
				resp["result"] = map[string]any{"ticket": tk, "expires_in_seconds": 30, "instance_id": inst,
					"profile_id": req.Params["profile_id"], "runtime_protocol": 1}
			default:
				resp = map[string]any{"ok": false, "error": "unknown verb", "protocol": 1, "id": req.ID}
			}
			g.mu.Unlock()
			b, _ := json.Marshal(resp)
			_, _ = c.Write(append(b, '\n'))
		}()
	}
}

func (g *Gateway) serveWS(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/ws" {
		http.NotFound(w, r)
		return
	}
	ok := false
	g.mu.Lock()
	for _, p := range strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",") {
		tk, found := strings.CutPrefix(strings.TrimSpace(p), "hermes-gateway-ticket.")
		for i, t := range g.Tickets {
			if found && t == tk {
				ok = true
				g.Tickets = append(g.Tickets[:i], g.Tickets[i+1:]...)
				break
			}
		}
	}
	g.mu.Unlock()
	if !ok {
		http.Error(w, "no ticket", http.StatusForbidden)
		return
	}
	protos := []string{"hermes-gateway-v1"}
	g.mu.Lock()
	if g.NoSubprotocol {
		protos = nil
	}
	g.mu.Unlock()
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: protos})
	if err != nil {
		return
	}
	g.mu.Lock()
	g.conns = append(g.conns, c)
	g.attached++
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.attached--
		g.mu.Unlock()
	}()
	ctx := context.Background()
	write := func(v any) {
		b, _ := json.Marshal(v)
		_ = c.Write(ctx, websocket.MessageText, b)
	}
	write(map[string]any{"jsonrpc": "2.0", "method": "event", "params": map[string]any{"type": "gateway.ready",
		"payload": map[string]any{"heartbeat": true, "skin": map[string]any{"name": DecoyMarker}}}})
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		var req struct {
			ID     any            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if json.Unmarshal(data, &req) != nil {
			continue
		}
		if req.Method == "" {
			if id, _ := req.ID.(string); strings.HasPrefix(id, "srq-") {
				g.mu.Lock()
				g.ServerAnswers++
				g.mu.Unlock()
			}
			continue
		}
		g.mu.Lock()
		switch req.Method {
		case "session.resume":
			sid, _ := req.Params["session_id"].(string)
			g.Resumes = append(g.Resumes, sid)
			res := map[string]any{"session_id": sid, "stored_session_id": sid, "running": true, "execution_generation": 1,
				"messages": []any{map[string]any{"role": "user", "content": DecoyMarker}},
				"pending":  []any{map[string]any{"admission_id": "a1", "text": DecoyMarker}},
				"prompts":  append([]map[string]any(nil), g.Prompts...),
				"info":     map[string]any{"system_prompt": DecoyMarker, "credential_hint": DecoyMarker},
			}
			g.mu.Unlock()
			write(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": res})
		case "approval.respond":
			g.Responds = append(g.Responds, req.Params)
			errMsg := g.RespondError
			g.mu.Unlock()
			if errMsg != "" {
				write(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": 4009, "message": errMsg}})
				continue
			}
			g.mu.Lock()
			n := max(g.RespondRepeat, 1)
			g.mu.Unlock()
			for i := 0; i < n; i++ {
				write(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"status": "resolved", "prompt_id": req.Params["prompt_id"]}})
			}
		default:
			g.mu.Unlock()
			write(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": "unknown_method"}})
		}
	}
}
