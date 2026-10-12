package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/notes"
)

// TestEachAgentDoorWorksUnderRowLevelSecurity is accounts PR 2's top follow-up,
// closed: each of the three per-agent doors — requireAgentToken (/agent/task),
// requireConnectorAgent (/agent/connectors) and requireRelayAgent
// (/agent/blocked) — resolves the token to its row BEFORE any principal exists.
// Under row-level security a lookup with no principal sees no rows, so without
// agentByToken's authz.System every one of them answered 401.
//
// It runs against REAL Postgres with migration 0007 applied, and with an agent
// owned by a SECOND user, so it also proves the principal is the ROW's owner
// (not a constant 1), and that the handler behind the door then reads as that
// owner: user 2's agent sees user 2's task and user 2's connector, and nothing
// of user 1's.
//
// RED when agentByToken does its lookup on the request's own context (no
// System): every door 401s ("invalid agent token") and the probe sees nothing.
func TestEachAgentDoorWorksUnderRowLevelSecurity(t *testing.T) {
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	sys := authz.System(ctx)

	// User 2 and its rows, written as System with explicit owners.
	var two int64
	if err := pool.QueryRow(sys, `INSERT INTO users (role) VALUES ('member') RETURNING id`).Scan(&two); err != nil {
		t.Fatal(err)
	}
	asTwo := authz.WithPrincipal(ctx, authz.Principal{UserID: two, Role: authz.RoleMember, Via: authz.ViaSession})
	asOne := dbtest.AsOperator(ctx)
	suffix := time.Now().Format("150405.000000")

	ns := notes.NewPG(pool)
	taskTwo, err := ns.Create(asTwo, notes.Note{Title: "user-two-task-" + suffix, Status: notes.StatusOpen})
	if err != nil {
		t.Fatal(err)
	}
	taskOne, err := ns.Create(asOne, notes.Note{Title: "user-one-task-" + suffix, Status: notes.StatusOpen})
	if err != nil {
		t.Fatal(err)
	}
	as := agents.NewPG(pool)
	tokTwo, tokOne := "rls-door-agent-two-"+suffix, "rls-door-agent-one-"+suffix
	agentTwo, err := as.Create(asTwo, agents.Agent{Name: "rls-two-" + suffix, Namespace: "ns-rls-two", HooksToken: tokTwo, NoteID: &taskTwo.ID})
	if err != nil {
		t.Fatal(err)
	}
	agentOne, err := as.Create(asOne, agents.Agent{Name: "rls-one-" + suffix, Namespace: "ns-rls-one", HooksToken: tokOne, NoteID: &taskOne.ID})
	if err != nil {
		t.Fatal(err)
	}
	if agentTwo.OwnerID != two || agentOne.OwnerID != 1 {
		t.Fatalf("fixture: agent owners = %d, %d; want %d, 1", agentTwo.OwnerID, agentOne.OwnerID, two)
	}
	cs, err := connectors.NewPG(pool, bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	connTwo, connOne := "rls-conn-two-"+suffix[7:], "rls-conn-one-"+suffix[7:]
	for _, c := range []struct {
		ctx  context.Context
		name string
	}{{asTwo, connTwo}, {asOne, connOne}} {
		if _, err := cs.CreateConnector(c.ctx, connectors.NewConnector{Name: c.name, Hosts: []string{"api.example.test"},
			Scheme: connectors.SchemeBearer, Secret: "fake-secret-0123456789"}); err != nil {
			t.Fatalf("create connector %s: %v", c.name, err)
		}
	}
	t.Cleanup(func() {
		bg := authz.System(context.Background())
		_, _ = pool.Exec(bg, `DELETE FROM connectors WHERE name = ANY($1)`, []string{connTwo, connOne})
		_, _ = pool.Exec(bg, `DELETE FROM agents WHERE id = ANY($1)`, []int64{agentTwo.ID, agentOne.ID})
		_, _ = pool.Exec(bg, `DELETE FROM notes WHERE id = ANY($1)`, []int64{taskTwo.ID, taskOne.ID})
		_, _ = pool.Exec(bg, `DELETE FROM users WHERE id=$1`, two)
	})

	s := New(nil, AuthConfig{HookToken: connTestHookToken, ServiceToken: connTestServiceToken, UIPassword: connTestPassword},
		log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Agents: as, Notes: ns, Connectors: cs})
	var mu sync.Mutex
	var seen []string
	s.principalProbe = func(r *http.Request) {
		p, _ := authz.From(r.Context())
		mu.Lock()
		seen = append(seen, p.String())
		mu.Unlock()
	}
	h := s.Handler()

	call := func(method, path, body, tok string) *httptest.ResponseRecorder {
		mu.Lock()
		seen = nil
		mu.Unlock()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("X-Muster-Token", tok)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	wantPrincipal := func(door string, uid int64, via authz.Via) {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		want := principalFor(uid, via).String()
		if len(seen) != 1 || seen[0] != want {
			t.Errorf("%s: principal at admit = %v, want [%s]", door, seen, want)
		}
	}

	// Door 1 — requireAgentToken, end to end through the handler.
	for _, c := range []struct {
		tok       string
		uid       int64
		mine, not string
	}{{tokTwo, two, taskTwo.Title, taskOne.Title}, {tokOne, 1, taskOne.Title, taskTwo.Title}} {
		w := call("GET", "/agent/task", "", c.tok)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), c.mine) || strings.Contains(w.Body.String(), c.not) {
			t.Errorf("GET /agent/task as user %d's agent: %d %s; want 200 with %q and without %q", c.uid, w.Code, w.Body, c.mine, c.not)
		}
		wantPrincipal("requireAgentToken", c.uid, authz.ViaAgent)
	}

	// Door 2 — requireConnectorAgent: user 2's agent lists user 2's connector only.
	w := call("GET", "/agent/connectors", "", tokTwo)
	var list struct {
		Connectors []struct {
			Name string `json:"name"`
		} `json:"connectors"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	names := map[string]bool{}
	for _, c := range list.Connectors {
		names[c.Name] = true
	}
	if w.Code != http.StatusOK || !names[connTwo] || names[connOne] {
		t.Errorf("GET /agent/connectors as user 2's agent: %d %s; want 200 listing %q and not %q", w.Code, w.Body, connTwo, connOne)
	}
	wantPrincipal("requireConnectorAgent", two, authz.ViaConnect)

	// Door 3 — requireRelayAgent.
	w = call("POST", "/agent/blocked", `{"blocked":false}`, tokTwo)
	if w.Code != http.StatusNoContent {
		t.Errorf("POST /agent/blocked as user 2's agent: %d %s; want 204", w.Code, w.Body)
	}
	wantPrincipal("requireRelayAgent", two, authz.ViaRelay)

	// Negative control: an unknown token is still refused at every door.
	for _, rt := range []struct{ m, p, b string }{{"GET", "/agent/task", ""}, {"GET", "/agent/connectors", ""}, {"POST", "/agent/blocked", `{"blocked":false}`}} {
		if w := call(rt.m, rt.p, rt.b, "no-such-agent-token-0123456789"); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with an unknown token: %d, want 401", rt.m, rt.p, w.Code)
		}
	}
}

// TestAnAgentRowWithNoOwnerIsRefusedAtEveryDoor: agentOwnerID's zero ("this
// row never read owner_id") must not become user 1. A store that returns an
// owner-less row is refused 401 at all three doors.
func TestAnAgentRowWithNoOwnerIsRefusedAtEveryDoor(t *testing.T) {
	s := New(nil, AuthConfig{HookToken: connTestHookToken, ServiceToken: connTestServiceToken, UIPassword: connTestPassword},
		log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Agents: ownerlessAgents{}, Connectors: principalConnectors{}})
	admitted := false
	s.principalProbe = func(*http.Request) { admitted = true }
	h := s.Handler()
	for _, rt := range []struct{ m, p, b string }{{"GET", "/agent/task", ""}, {"GET", "/agent/connectors", ""}, {"POST", "/agent/blocked", `{}`}} {
		admitted = false
		r := httptest.NewRequest(rt.m, rt.p, strings.NewReader(rt.b))
		r.Header.Set("X-Muster-Token", "ownerless-token-0123456789")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized || admitted {
			t.Errorf("%s %s with an owner-less agent row: %d (admitted=%v), want 401 and no principal", rt.m, rt.p, w.Code, admitted)
		}
	}
}

type ownerlessAgents struct{ agents.Store }

func (ownerlessAgents) GetByHooksToken(_ context.Context, tok string) (agents.Agent, error) {
	return agents.Agent{ID: 9, Name: "ownerless"}, nil
}

// TestTheAgentReadyPushReadsItsAgentUnderRowLevelSecurity: notifyAgentRunning
// is handed only a NAME (by request handlers and background loops alike) and
// re-reads the row to decide whether to push "agent ready". Under row-level
// security a no-principal read finds nothing and the push silently never
// fires; it reads under authz.System. Observed through its dedupe mark, which
// is set only once the row was read and qualified.
func TestTheAgentReadyPushReadsItsAgentUnderRowLevelSecurity(t *testing.T) {
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	op := dbtest.AsOperator(ctx)
	suffix := time.Now().Format("150405.000000")
	task, err := notes.NewPG(pool).Create(op, notes.Note{Title: "ready-push-" + suffix, Status: notes.StatusOpen})
	if err != nil {
		t.Fatal(err)
	}
	as := agents.NewPG(pool)
	a, err := as.Create(op, agents.Agent{Name: "ready-push-" + suffix, Namespace: "ns-ready-push", NoteID: &task.ID, Status: agents.StatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := dbtest.AsOperator(context.Background())
		_, _ = pool.Exec(bg, `DELETE FROM agents WHERE id=$1`, a.ID)
		_, _ = pool.Exec(bg, `DELETE FROM notes WHERE id=$1`, task.ID)
	})
	s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Agents: as})
	s.UseRouter(stubRouter{})
	s.notifyAgentRunning(a.Name)
	s.pushInFlight.Wait()
	s.taskProvisionedMu.Lock()
	fired := s.taskProvisionedNotified[a.ID]
	s.taskProvisionedMu.Unlock()
	if !fired {
		t.Fatalf("the agent-ready push did not fire for running, task-linked agent %q: its row was not read", a.Name)
	}
}
