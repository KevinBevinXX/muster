package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/users"
)

// scriptedRuntime answers request 1 of a turn with one function_call (name,
// args) and every later request with terminal text, recording each body.
type scriptedRuntime struct {
	mu     sync.Mutex
	name   string
	args   string
	bodies []string
}

func (f *scriptedRuntime) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.bodies = append(f.bodies, string(body))
	n := len(f.bodies)
	name, args := f.name, f.args
	f.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	if n == 1 {
		item, _ := json.Marshal(map[string]any{"type": "function_call", "call_id": "call-1", "name": name, "arguments": args})
		fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[%s]}}\n\n", item)
		return
	}
	fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\","+
		"\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"done\"}]}]}}\n\n")
}

func (f *scriptedRuntime) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.bodies...)
}

// pluginPlane is the real plane a delivered plugin message runs through: real
// Postgres stores, the real deliverer and chatTurn, the real agentgateway.Gateway
// — only the agent runtime (rt) is fake. The owner's chief is a gateway-kind
// agent displayed "chief".
type pluginPlane struct {
	srv     *Server
	ps      *plugins.PGStore
	as      agents.Store
	ns      notes.Store
	owner   context.Context
	chief   agents.Agent
	plugin  plugins.Plugin
	ownerID int64
}

func newPluginPlane(t *testing.T, rt http.Handler) *pluginPlane {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dbtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	var uid int64
	if err := pool.QueryRow(authz.System(ctx), `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`, "pp"+suffix).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM notes WHERE owner_id = $1`, `DELETE FROM plugins WHERE owner_id = $1`,
			`DELETE FROM agents WHERE owner_id = $1`, `DELETE FROM audit_log WHERE user_id = $1`, `DELETE FROM users WHERE id = $1`} {
			_, _ = pool.Exec(authz.System(context.Background()), q, uid)
		}
	})
	owner := authz.WithPrincipal(ctx, authz.Principal{UserID: uid, Role: authz.RoleMember, Via: authz.ViaPlugin})
	as := agents.NewPG(pool)
	chief, err := as.Create(owner, agents.Agent{Name: "ppchief-" + suffix, Namespace: "devpod-ppchief-" + suffix,
		DisplayName: "chief", Status: agents.StatusRunning, HooksToken: "pp-hooks-" + suffix})
	if err != nil {
		t.Fatal(err)
	}
	up := httptest.NewServer(rt)
	t.Cleanup(up.Close)
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(up.URL, "http://"))
	port, _ := strconv.Atoi(portStr)
	gw, err := agentgateway.New(agentgateway.Config{Driver: fixedEndpoint{provision.Endpoint{Host: host, Port: port}},
		Runtime: agentgateway.HooksSHA256(), Model: "runtime-sentinel", Client: up.Client()})
	if err != nil {
		t.Fatal(err)
	}
	ps, ns := plugins.NewPG(pool), notes.NewPG(pool)
	srv := New(nil, AuthConfig{HookToken: tokTestHook}, log.New(io.Discard, "", 0))
	srv.UseExtensions(Extensions{Agents: as, Users: users.NewPG(pool), Plugins: ps, Notes: ns, PluginGateway: gw, Gateway: gw})
	_, p, err := ps.Create(owner, "example-inbox", "Example inbox", "example-inbox")
	if err != nil {
		t.Fatal(err)
	}
	return &pluginPlane{srv: srv, ps: ps, as: as, ns: ns, owner: owner, chief: chief, plugin: p, ownerID: uid}
}

// TestTheChiefFilesAGatedTaskOnTheTurnThatDeliversTheMessage is finding 6 of
// audit round 1, through the real plane: the runtime's FIRST response on the
// delivery turn is a task_create_gated call citing the message being delivered —
// the core flow, the chief acting on the turn that tells it about the message.
// The call goes through the real ChatWithTools → dispatch path; the task must be
// created, gated, with the server's citation line, and the tool result must be a
// success. Before the fix the message was still `queued` with no agent during the
// turn and the citation was refused.
func TestTheChiefFilesAGatedTaskOnTheTurnThatDeliversTheMessage(t *testing.T) {
	rt := &scriptedRuntime{name: gatedTaskToolName}
	pp := newPluginPlane(t, rt)
	m, _, err := pp.ps.Submit(pp.owner, pp.plugin.ID, plugins.MessageIn{ExternalID: "seam-1", Body: "please book the room"})
	if err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{"title": "Book the room", "body": "The sender asks for a room booking.",
		"gate_reason": "owner approval", "message_id": m.ID})
	rt.mu.Lock()
	rt.args = string(args)
	rt.mu.Unlock()

	if r := pp.srv.pluginDeliverer.Deliver(pp.owner, pp.plugin.ID, m.ID, pp.chief); r.Outcome != plugins.OutcomeDelivered {
		t.Fatalf("delivery: %+v; want delivered", r)
	}
	sent := rt.seen()
	if len(sent) != 2 {
		t.Fatalf("the runtime saw %d request(s); want 2 (the call, then its output)", len(sent))
	}
	list, err := pp.ns.List(pp.owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("the owner has %d task(s) after the chief's call; want 1 — the tool answered: %s", len(list), sent[1])
	}
	n := list[0]
	cite := "It cites plugin message " + strconv.FormatInt(m.ID, 10) + " (Example inbox)"
	if !strings.Contains(n.Body, cite) {
		t.Fatalf("the task body does not carry the server's citation line %q:\n%s", cite, n.Body)
	}
	gated := false
	for _, tag := range n.Tags {
		if strings.HasPrefix(tag, notes.NSGate+":") {
			gated = true
		}
	}
	if !gated {
		t.Fatalf("the task's tags %v carry no gate", n.Tags)
	}
	parts, _ := pp.as.ListChatMessages(pp.owner, *mustPlugin(t, pp).ChiefSessionID)
	ok := false
	for _, part := range parts {
		if part.Kind == "tool_result" && part.ToolName == gatedTaskToolName && part.ToolOK {
			ok = true
		}
	}
	if !ok {
		t.Fatal("the session stores no successful task_create_gated result")
	}
}

func mustPlugin(t *testing.T, pp *pluginPlane) plugins.Plugin {
	t.Helper()
	p, err := pp.ps.Plugin(pp.owner, pp.plugin.ID)
	if err != nil || p.ChiefSessionID == nil {
		t.Fatalf("the plugin %+v (%v); want its chief session recorded", p, err)
	}
	return p
}
