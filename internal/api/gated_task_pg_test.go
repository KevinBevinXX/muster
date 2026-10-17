package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plugins"
)

// gatedFixture is the connector fixture (real Postgres, real /mcp door) with the
// task store and the plugin store wired, and a FRESH member owning a chief and a
// worker — fresh so no other test's agent labelled "chief" can outrank this one
// in chiefPredicate's lowest-id rule.
type gatedFixture struct {
	*connFixture
	owner     int64
	asOwner   context.Context
	chief     agents.Agent
	worker    agents.Agent
	chiefTok  string
	workerTok string
	plugins   *plugins.PGStore
	notes     notes.Store
	// focus is the session focus store, and summ the fake summarizer the
	// focus tools' refresher calls (focus_tools_pg_test.go).
	focus *focus.PGStore
	summ  *fakeFocusSummarizer
}

func newGatedFixture(t *testing.T) *gatedFixture {
	t.Helper()
	g := &gatedFixture{}
	g.connFixture = newConnFixtureWith(t, func(ext *Extensions, pool *pgxpool.Pool) {
		g.plugins = plugins.NewPG(pool)
		g.notes = notes.NewPG(pool)
		ext.Plugins, ext.Notes = g.plugins, g.notes
		g.focus, g.summ = focus.NewPG(pool), &fakeFocusSummarizer{}
		ext.Focus, ext.FocusSummarizer = g.focus, g.summ
	})
	g.owner = g.newMember(t)
	g.asOwner = g.as(g.owner)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	g.chiefTok, g.workerTok = "gated-chief-token-"+suffix, "gated-worker-token-"+suffix
	g.chief = g.newAgent(t, g.owner, "gtchief-"+suffix, "chief", g.chiefTok)
	g.worker = g.newAgent(t, g.owner, "gtworker-"+suffix, "", g.workerTok)
	return g
}

func (g *gatedFixture) as(uid int64) context.Context {
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: authz.RoleMember, Via: authz.ViaSession})
}

func (g *gatedFixture) newMember(t *testing.T) int64 {
	t.Helper()
	var id int64
	if err := g.pool.QueryRow(authz.System(g.ctx), `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`,
		"gated-"+strconv.FormatInt(time.Now().UnixNano(), 36)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		sys := authz.System(context.Background())
		for _, q := range []string{`DELETE FROM notes WHERE owner_id = $1`, `DELETE FROM plugins WHERE owner_id = $1`,
			`DELETE FROM agents WHERE owner_id = $1`, `DELETE FROM audit_log WHERE user_id = $1`, `DELETE FROM users WHERE id = $1`} {
			_, _ = g.pool.Exec(sys, q, id)
		}
	})
	return id
}

func (g *gatedFixture) newAgent(t *testing.T, owner int64, name, display, token string) agents.Agent {
	t.Helper()
	a, err := agents.NewPG(g.pool).Create(g.as(owner), agents.Agent{Name: name, Namespace: "devpod-" + name,
		DisplayName: display, Status: agents.StatusRunning, HooksToken: token})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// deliveredMessage stores one plugin message for owner and settles it as
// delivered to agentID (or leaves it queued when agentID is 0), through the
// store's own delivery steps.
func (g *gatedFixture) deliveredMessage(t *testing.T, owner, agentID int64, external string) plugins.Message {
	t.Helper()
	m := g.deliveringMessage(t, owner, agentID, external)
	if agentID != 0 {
		if ok, err := g.plugins.RecordDelivery(g.as(owner), m.ID, agentID, plugins.StatusDelivered); err != nil || !ok {
			t.Fatalf("record delivery: %v %v", ok, err)
		}
	}
	return m
}

// deliveringMessage stores one plugin message for owner and marks it as being
// delivered to agentID — the state during the delivery turn — or leaves it
// queued when agentID is 0.
func (g *gatedFixture) deliveringMessage(t *testing.T, owner, agentID int64, external string) plugins.Message {
	t.Helper()
	ctx := g.as(owner)
	ps, err := g.plugins.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var p plugins.Plugin
	if len(ps) > 0 {
		p = ps[0]
	} else if _, p, err = g.plugins.Create(ctx, "example-inbox", "Example inbox", "example-inbox-data"); err != nil {
		t.Fatal(err)
	}
	m, _, err := g.plugins.Submit(ctx, p.ID, plugins.MessageIn{ExternalID: external, Sender: "someone@example.test", Body: "please do a thing"})
	if err != nil {
		t.Fatal(err)
	}
	if agentID != 0 {
		if ok, err := g.plugins.BeginDelivery(ctx, m.ID, agentID); err != nil || !ok {
			t.Fatalf("begin delivery: %v %v", ok, err)
		}
	}
	return m
}

// callGated calls task_create_gated over POST /mcp with token, returning the
// tool's text and whether MCP flagged it an error. A JSON-RPC error (the tool not
// listed for this caller) is returned as rpcErr.
func (g *gatedFixture) callGated(t *testing.T, token string, args map[string]any) (text string, isErr bool, rpcErr *mcpError) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": gatedTaskToolName, "arguments": args}})
	r, code := g.mcp(t, token, string(raw), nil)
	if r == nil {
		t.Fatalf("/mcp answered HTTP %d", code)
	}
	if r.Error != nil {
		return "", false, r.Error
	}
	text, isErr = toolText(t, r)
	return text, isErr, nil
}

func (g *gatedFixture) ownerTasks(t *testing.T) []notes.Note {
	t.Helper()
	list, err := g.notes.List(g.asOwner)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

// TestTheChiefFilesAGatedTaskOverMCP: the chief's own token, through the real
// /mcp door and the real stores, files a task that is stored GATED with the
// server's gate tag, owned by the chief's owner, citing the plugin message in a
// server-written body line — and the reservation is audited.
func TestTheChiefFilesAGatedTaskOverMCP(t *testing.T) {
	g := newGatedFixture(t)
	m := g.deliveredMessage(t, g.owner, g.chief.ID, "ext-1")

	text, isErr, rpcErr := g.callGated(t, g.chiefTok, map[string]any{
		"title": "Answer the inbox", "body": "Summarised request.", "gate_reason": "Needs Owner OK!",
		"message_id": m.ID, "tags": []string{"Inbox"},
	})
	if rpcErr != nil || isErr {
		t.Fatalf("the chief's call failed: rpc=%+v text=%s", rpcErr, text)
	}
	var out struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil || out.ID <= 0 {
		t.Fatalf("result %s", text)
	}
	n, err := g.notes.Get(g.asOwner, out.ID)
	if err != nil {
		t.Fatalf("the task is not the owner's: %v", err)
	}
	if want := []string{"gate:needs-owner-ok", "inbox"}; strings.Join(n.Tags, ",") != strings.Join(want, ",") {
		t.Errorf("stored tags %v, want %v", n.Tags, want)
	}
	if !notes.IsGated(n.Tags) {
		t.Error("the stored task is not gated")
	}
	if want := fmt.Sprintf("It cites plugin message %d (Example inbox), which is untrusted outside content.", m.ID); !strings.Contains(n.Body, want) {
		t.Errorf("the body does not carry the server's citation line %q:\n%s", want, n.Body)
	}
	var audited int
	if err := g.pool.QueryRow(g.asOwner, `SELECT count(*) FROM audit_log WHERE action = $1 AND object = $2
		AND (detail->>'message_id')::bigint = $3`, plugins.ActionGatedTask, plugins.AgentObject(g.chief.ID), m.ID).Scan(&audited); err != nil {
		t.Fatal(err)
	}
	if audited != 1 {
		t.Errorf("%d %s audit rows for the chief citing the message, want 1", audited, plugins.ActionGatedTask)
	}
}

// TestAWorkerIsNeitherOfferedNorAbleToCallTheGatedTool over the real /mcp door:
// the tool is not in the worker's tools/list, a tools/call for it is an unknown
// tool (-32602), and no task appears.
func TestAWorkerIsNeitherOfferedNorAbleToCallTheGatedTool(t *testing.T) {
	g := newGatedFixture(t)
	listed := func(token string) bool {
		r, _ := g.mcp(t, token, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)
		if r == nil || r.Error != nil {
			t.Fatalf("tools/list: %+v", r)
		}
		return strings.Contains(string(r.Result), `"name":"`+gatedTaskToolName+`"`)
	}
	// Positive control: the chief IS offered it, so the absence below is a filter.
	if !listed(g.chiefTok) {
		t.Fatal("positive control: the chief's tools/list does not offer the gated tool")
	}
	if listed(g.workerTok) {
		t.Error("a worker's tools/list offers the chief-only gated tool")
	}
	_, _, rpcErr := g.callGated(t, g.workerTok, map[string]any{"title": "x", "body": "y", "gate_reason": "z"})
	if rpcErr == nil || rpcErr.Code != -32602 {
		t.Errorf("a worker's tools/call: %+v, want -32602 unknown tool", rpcErr)
	}
	if got := g.ownerTasks(t); len(got) != 0 {
		t.Errorf("a worker's call created %d task(s)", len(got))
	}
}

// TestTheGatedTaskBudgetIsTwentyADay: exactly MaxGatedTasksPerDay calls succeed
// and the next is refused with nothing created.
//
// ⚠ A LITERAL 20, NOT THE CONSTANT: a mutant that changed the constant would move
// a test written against it.
func TestTheGatedTaskBudgetIsTwentyADay(t *testing.T) {
	g := newGatedFixture(t)
	if plugins.MaxGatedTasksPerDay != 20 {
		t.Fatalf("the budget is %d; the operator decision is 20", plugins.MaxGatedTasksPerDay)
	}
	for i := 1; i <= 20; i++ {
		text, isErr, rpcErr := g.callGated(t, g.chiefTok, map[string]any{"title": fmt.Sprintf("t%d", i), "body": "b", "gate_reason": "r"})
		if rpcErr != nil || isErr {
			t.Fatalf("call %d of 20 refused: %+v %s", i, rpcErr, text)
		}
	}
	text, isErr, _ := g.callGated(t, g.chiefTok, map[string]any{"title": "t21", "body": "b", "gate_reason": "r"})
	if !isErr || !strings.Contains(text, "filed 20 gated tasks in the last day") {
		t.Errorf("the 21st call: isError=%v %s, want the budget refusal", isErr, text)
	}
	if got := len(g.ownerTasks(t)); got != 20 {
		t.Errorf("%d tasks exist after 21 calls, want 20", got)
	}
}

// TestConcurrentReservationsCannotOvershootTheBudget: 30 reservations racing for
// one agent land exactly 20 — the agent-row lock serialises the count and the
// insert. Measured red with the `FOR UPDATE` removed.
func TestConcurrentReservationsCannotOvershootTheBudget(t *testing.T) {
	g := newGatedFixture(t)
	var mu sync.Mutex
	ok, capped := 0, 0
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := g.plugins.ReserveGatedTask(g.asOwner, g.chief.ID, nil)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, plugins.ErrGatedTaskCap):
				capped++
			default:
				t.Errorf("reserve: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok != 20 || capped != 10 {
		t.Fatalf("30 racing reservations: %d landed, %d capped; want 20 and 10", ok, capped)
	}
}

// TestAGatedTaskCitesOnlyAMessageDeliveredToThisChief: message_id must name a
// plugin message delivered — or BEING delivered — to THIS chief. A queued one
// (not being delivered to anyone), one being delivered or delivered to another
// of the owner's agents, another owner's (row-level security hides it), and a
// made-up id are each refused with nothing created and no budget spent. A message
// being delivered to this chief — the state during the turn that tells the chief
// about it (audit round 1) — and one already delivered to it are accepted.
func TestAGatedTaskCitesOnlyAMessageDeliveredToThisChief(t *testing.T) {
	g := newGatedFixture(t)
	queued := g.deliveredMessage(t, g.owner, 0, "q-1")
	toWorker := g.deliveredMessage(t, g.owner, g.worker.ID, "w-1")
	deliveringToWorker := g.deliveringMessage(t, g.owner, g.worker.ID, "dw-1")
	other := g.newMember(t)
	otherChief := g.newAgent(t, other, "gtother-"+strconv.FormatInt(time.Now().UnixNano(), 36), "chief", "gated-other-token-0123456789")
	foreign := g.deliveringMessage(t, other, otherChief.ID, "f-1")
	delivering := g.deliveringMessage(t, g.owner, g.chief.ID, "d-1")
	mine := g.deliveredMessage(t, g.owner, g.chief.ID, "m-1")

	for name, id := range map[string]int64{"queued": queued.ID, "delivered to the worker": toWorker.ID,
		"being delivered to the worker": deliveringToWorker.ID, "another owner's": foreign.ID, "made up": 999999999} {
		text, isErr, _ := g.callGated(t, g.chiefTok, map[string]any{"title": "t", "body": "b", "gate_reason": "r", "message_id": id})
		if !isErr || !strings.Contains(text, "is not a plugin message delivered to you") {
			t.Errorf("%s message: isError=%v %s", name, isErr, text)
		}
	}
	if got := len(g.ownerTasks(t)); got != 0 {
		t.Fatalf("%d task(s) created citing messages that were not delivered to this chief", got)
	}
	var spent int
	if err := g.pool.QueryRow(g.asOwner, `SELECT count(*) FROM audit_log WHERE action = $1`, plugins.ActionGatedTask).Scan(&spent); err != nil {
		t.Fatal(err)
	}
	if spent != 0 {
		t.Errorf("refused citations spent %d unit(s) of the budget", spent)
	}
	for name, id := range map[string]int64{"being delivered to this chief": delivering.ID, "delivered to this chief": mine.ID} {
		if text, isErr, _ := g.callGated(t, g.chiefTok, map[string]any{"title": "t", "body": "b", "gate_reason": "r", "message_id": id}); isErr {
			t.Errorf("citing a message %s was refused: %s", name, text)
		}
	}
}

// TestANULByteIsRefusedBeforeTheBudgetIsSpent (audit round 1): Postgres refuses
// a NUL byte in text, so a title, body or repo carrying one used to pass the
// tool's checks, reserve a unit, and then fail in the insert — a unit spent and
// no task. Each is refused BEFORE the reservation; the budget is unchanged.
func TestANULByteIsRefusedBeforeTheBudgetIsSpent(t *testing.T) {
	g := newGatedFixture(t)
	for _, field := range []string{"title", "body", "repo"} {
		args := map[string]any{"title": "t", "body": "b", "gate_reason": "r"}
		args[field] = "has a \x00 byte"
		text, isErr, rpcErr := g.callGated(t, g.chiefTok, args)
		if rpcErr != nil || !isErr || !strings.Contains(text, field+" must be UTF-8 text without NUL") {
			t.Errorf("a NUL in %s: rpc=%+v isError=%v %s; want refused naming the field", field, rpcErr, isErr, text)
		}
	}
	var spent int
	if err := g.pool.QueryRow(g.asOwner, `SELECT count(*) FROM audit_log WHERE action = $1`, plugins.ActionGatedTask).Scan(&spent); err != nil {
		t.Fatal(err)
	}
	if spent != 0 || len(g.ownerTasks(t)) != 0 {
		t.Fatalf("NUL-carrying calls spent %d unit(s) and created %d task(s); want none", spent, len(g.ownerTasks(t)))
	}
	// Control: the same call without the NUL files a task and spends one unit.
	if text, isErr, _ := g.callGated(t, g.chiefTok, map[string]any{"title": "t", "body": "b", "gate_reason": "r", "repo": "acme/widgets"}); isErr {
		t.Fatalf("control refused: %s", text)
	}
	if err := g.pool.QueryRow(g.asOwner, `SELECT count(*) FROM audit_log WHERE action = $1`, plugins.ActionGatedTask).Scan(&spent); err != nil || spent != 1 {
		t.Fatalf("control spent %d unit(s) (%v); want 1", spent, err)
	}
}
