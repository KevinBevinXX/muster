package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/sse"
)

// agent_destroy (agent_destroy_tool.go) and the owner's destroy on both doors,
// over a real Postgres: the operator's mirrored Claude Code session (POST
// /mirror/mcp) destroys one of the owner's agents through the browser's own
// teardown; the task is kept and becomes dispatchable again; an owner's destroy
// of the agent the CHIEF started releases the chief's one start on that task,
// once, on either door; open tabs are told — and nobody else can call it: not a
// dispatched agent, not the chief, not another owner's host; and the chief
// itself cannot be destroyed this way.
//
// ⚠ THE TOOL'S NAME, ITS REFUSALS AND THE RELEASE ACTION ARE STRING LITERALS
// here, so the behaviour tests are red on the pre-feature tree for a
// behavioural reason. (reopenBeforeWrite, the race hook, is new: the file
// needs it to build.)

// destroyProv records each Destroy and deletes the row the way
// agentprovision.Adapter.Destroy does on success, or fails and keeps it when
// fail is set; Dispatch succeeds (for task_dispatch's start).
type destroyProv struct {
	stubProvisioner
	pool      *pgxpool.Pool
	mu        sync.Mutex
	destroyed []int64
	fail      error
}

func (p *destroyProv) Destroy(_ context.Context, id int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.destroyed = append(p.destroyed, id)
	if p.fail != nil {
		return p.fail
	}
	_, err := p.pool.Exec(authz.System(context.Background()), `DELETE FROM agents WHERE id = $1`, id)
	return err
}

func (p *destroyProv) destroyedIDs() []int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int64(nil), p.destroyed...)
}

// destroyFixture is the attention fixture (user 1's agents op and op2; a member
// with a chief and a worker) plus: a recording provisioner, user 1's own chief
// (its reserved slug), task_dispatch on for user 1 with the plugin store as its
// ledger, an SSE bus with user 1's stream subscribed, and mirror tokens — ws
// (user 1's host "ws", listing focusSID), laptop (user 1, lists nothing) and
// members (the member's token for a host also named "ws").
type destroyFixture struct {
	*attnFixture
	prov                *destroyProv
	ownChief            agents.Agent
	ownChiefTok         string
	ws, laptop, members string
	stream              <-chan sse.Event
}

func newDestroyFixture(t *testing.T) *destroyFixture {
	t.Helper()
	f := &destroyFixture{prov: &destroyProv{}}
	f.attnFixture = newAttnFixture(t, func(ext *Extensions, pool *pgxpool.Pool) {
		f.prov.pool = pool
		ext.Provisioner = f.prov
		ext.TaskRouting = &TaskRouting{DispatchOwners: map[int64]bool{1: true}}
		ext.ChiefDispatch = plugins.NewPG(pool)
	})
	bus := sse.New(512)
	f.srv.bus = bus
	_, f.stream = bus.Subscribe(1)
	// User 1's chief is its RESERVED slug, which is the chief whatever else the
	// package's shared database holds (chiefPredicate: agents.IsChiefName). A
	// row another test left is reused, and only a row made here is removed.
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	f.ownChiefTok = "ad-chief-token-" + suffix
	if a, err := agents.NewPG(f.pool).GetByName(asUser(1), agents.ChiefNameFor(1)); err == nil {
		f.ownChief = a
		if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE agents SET hooks_token = $2 WHERE id = $1`, a.ID, f.ownChiefTok); err != nil {
			t.Fatal(err)
		}
	} else {
		f.ownChief = f.agentFor(t, 1, agents.ChiefNameFor(1), "chief", f.ownChiefTok)
	}
	f.ws, f.laptop, f.members = mirrorTok('a'), mirrorTok('b'), mirrorTok('c')
	f.tokens[f.ws] = mirror.Token{ID: 1, OwnerID: 1, Host: "ws"}
	f.tokens[f.laptop] = mirror.Token{ID: 2, OwnerID: 1, Host: "laptop"}
	f.tokens[f.members] = mirror.Token{ID: 3, OwnerID: f.member, Host: "ws"}
	t.Cleanup(func() {
		sys := authz.System(context.Background())
		_, _ = f.pool.Exec(sys, `DELETE FROM audit_log WHERE user_id = 1 AND action IN ($1, $2, $3, 'task.chief_dispatch_released')`,
			plugins.ActionChiefDispatch, plugins.ActionChiefDispatchAgent, plugins.ActionTaskRouted)
		_, _ = f.pool.Exec(sys, `DELETE FROM attention_asks WHERE owner_id = 1 AND mirror_host IS NOT NULL`)
	})
	return f
}

// destroy calls agent_destroy through /mirror/mcp as token's host and session
// sid: the tool's text and whether it is a tool error.
func (f *destroyFixture) destroy(t *testing.T, token, sid string, args map[string]any) (string, bool) {
	t.Helper()
	code, r, body := f.mirrorRPC(t, mirrorAs(token, sid), mcpToolCall("agent_destroy", args))
	if code != http.StatusOK || r == nil || r.Error != nil {
		t.Fatalf("agent_destroy %v: HTTP %d %s", args, code, body)
	}
	return toolText(t, r)
}

// browserDelete is the Agents page's Delete of agent id, as the operator.
func (f *destroyFixture) browserDelete(t *testing.T, id int64) {
	t.Helper()
	if w := f.call(t, http.MethodDelete, "/agents/"+strconv.FormatInt(id, 10), nil, f.operator()); w.Code != http.StatusOK {
		t.Fatalf("DELETE /agents/%d: %d %s", id, w.Code, w.Body)
	}
}

// task files a task of user 1's, in status.
func (f *destroyFixture) task(t *testing.T, title, status string) notes.Note {
	t.Helper()
	n, err := f.notes.Create(asUser(1), notes.Note{Title: title, Body: "Do " + title + ".", Status: notes.StatusOpen})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM notes WHERE id = $1`, n.ID)
	})
	if status != notes.StatusOpen {
		f.setStatus(t, n.ID, status)
	}
	return n
}

// setStatus moves task id to status as the owner would on its card.
func (f *destroyFixture) setStatus(t *testing.T, id int64, status string) {
	t.Helper()
	if _, err := f.srv.setNoteStatus(asUser(1), writerHumanUI, id, status); err != nil {
		t.Fatal(err)
	}
}

// agentOn creates a running agent of owner bound to task id.
func (f *destroyFixture) agentOn(t *testing.T, owner int64, name string, task *int64) agents.Agent {
	t.Helper()
	a, err := agents.NewPG(f.pool).Create(asUser(owner), agents.Agent{Name: name + "-" + strconv.FormatInt(time.Now().UnixNano(), 36),
		Namespace: "devpod-" + name, Status: agents.StatusRunning, NoteID: task})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM agents WHERE id = $1`, a.ID)
	})
	return a
}

// exists reports whether agent id's row is still there.
func (f *destroyFixture) exists(t *testing.T, id int64) bool {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM agents WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

// waitGone waits up to 5s for agent id's row to go, then 200ms more for the
// teardown goroutine's after-steps (release, broadcasts, reopen).
func (f *destroyFixture) waitGone(t *testing.T, id int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for f.exists(t, id) {
		if time.Now().After(deadline) {
			t.Fatalf("agent %d was never destroyed (destroyed: %v)", id, f.prov.destroyedIDs())
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
}

// waitDestroyCalled waits up to 3s for Destroy to have run n times, then 200ms.
func (f *destroyFixture) waitDestroyCalled(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for len(f.prov.destroyedIDs()) < n && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := f.prov.destroyedIDs(); len(got) != n {
		t.Fatalf("Destroy ran %v, want %d call(s)", got, n)
	}
	time.Sleep(200 * time.Millisecond)
}

// status is task id's status now.
func (f *destroyFixture) status(t *testing.T, id int64) string {
	t.Helper()
	n, err := f.notes.Get(asUser(1), id)
	if err != nil {
		t.Fatalf("the task is gone: %v", err)
	}
	return n.Status
}

// releases is the number of chief-start release rows for task id.
func (f *destroyFixture) releases(t *testing.T, id int64) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM audit_log WHERE action = 'task.chief_dispatch_released' AND object = $1`,
		plugins.NoteObject(id)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// drain is the "name data" of every event on user 1's stream so far.
func (f *destroyFixture) drain() []string {
	var out []string
	for {
		select {
		case ev := <-f.stream:
			out = append(out, ev.Name+" "+ev.Data)
		default:
			return out
		}
	}
}

// dispatch is the chief's task_dispatch on task id, through the gateway
// dispatcher as user 1.
func (f *destroyFixture) dispatch(id int64) string {
	return f.srv.dispatchAgentTool(asUser(1), f.ownChief, "task_dispatch", `{"task_id":`+strconv.FormatInt(id, 10)+`}`)
}

// mustStart is a chief task_dispatch on task id that must start an agent: its id.
func (f *destroyFixture) mustStart(t *testing.T, id int64) int64 {
	t.Helper()
	got := f.dispatch(id)
	var out struct {
		AgentID int64 `json:"agent_id"`
	}
	if err := json.Unmarshal([]byte(got), &out); err != nil || out.AgentID <= 0 || strings.Contains(got, "already_dispatched") {
		t.Fatalf("the chief's task_dispatch on task %d did not start an agent: %s", id, got)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM agents WHERE id = $1`, out.AgentID)
	})
	return out.AgentID
}

// mustBeAlreadyDispatched is a chief task_dispatch on task id that must start
// nothing because the task's one start is spent.
func (f *destroyFixture) mustBeAlreadyDispatched(t *testing.T, id int64) {
	t.Helper()
	if got := f.dispatch(id); !strings.Contains(got, `"already_dispatched":true`) {
		t.Fatalf("the chief's task_dispatch on task %d: %s; want already_dispatched", id, got)
	}
}

// TestAMirroredSessionDestroysAnAgentAndKeepsItsTask is the feature: user 1's
// own agent works task n (in_progress) and has an open ask. The CONTROL first:
// the chief cannot task_dispatch n while it does. Then ws's session destroys the
// agent by name: Destroy runs for exactly that agent, its row and its open ask
// are gone, the owner's stream gets agent.changed (naming it) and
// attention_queue.changed, no audit row of any kind is written for the destroy,
// the task is KEPT and back to open, and the chief's task_dispatch now starts a
// new agent on it.
func TestAMirroredSessionDestroysAnAgentAndKeepsItsTask(t *testing.T) {
	f := newDestroyFixture(t)
	n := f.task(t, "stuck work", notes.StatusInProgress)
	stuck := f.agentOn(t, 1, "adstuck", &n.ID)
	ask, err := f.attn.Create(asUser(1), attention.New{Asker: attention.Asker{AgentID: stuck.ID, Label: stuck.Name}, Question: "still there?"})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.dispatch(n.ID); !strings.Contains(got, `"error"`) {
		t.Fatalf("control: the chief started an agent on a task another agent works: %s", got)
	}
	f.drain()

	text, isErr := f.destroy(t, f.ws, focusSID, map[string]any{"name": stuck.Name})
	if isErr || !strings.Contains(text, `"destroying":true`) || !strings.Contains(text, `"agent_id":`+strconv.FormatInt(stuck.ID, 10)) ||
		!strings.Contains(text, `"task_id":`+strconv.FormatInt(n.ID, 10)) {
		t.Fatalf("agent_destroy: %s", text)
	}
	f.waitGone(t, stuck.ID)
	if got := f.prov.destroyedIDs(); len(got) != 1 || got[0] != stuck.ID {
		t.Fatalf("Destroy ran for %v, want exactly [%d]", got, stuck.ID)
	}
	if _, err := f.attn.Get(asUser(1), ask.ID); err == nil {
		t.Errorf("the destroyed agent's open ask %d is still there", ask.ID)
	}
	evs := strings.Join(f.drain(), "\n")
	for _, want := range []string{`agent.changed {"id":"` + stuck.Name + `"}`, `attention_queue.changed {"id":"agent-destroyed-` + strconv.FormatInt(stuck.ID, 10) + `"}`} {
		if !strings.Contains(evs, want) {
			t.Errorf("the owner's stream lacks %q:\n%s", want, evs)
		}
	}
	var audited int
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM audit_log WHERE object = $1`,
		"agent:"+strconv.FormatInt(stuck.ID, 10)).Scan(&audited); err != nil || audited != 0 {
		t.Errorf("the destroy wrote %d audit row(s) (%v); it writes none", audited, err)
	}
	if st := f.status(t, n.ID); st != notes.StatusOpen {
		t.Fatalf("the task is %q after the destroy, want open", st)
	}
	f.mustStart(t, n.ID)
}

// TestADestroyByIDKeepsATaskAnotherAgentStillWorks: destroyed by id, the agent
// goes; its task stays in_progress because ANOTHER live agent works it (the
// reopen's own guard), and a ready_for_review task is never downgraded.
func TestADestroyByIDKeepsATaskAnotherAgentStillWorks(t *testing.T) {
	f := newDestroyFixture(t)
	shared := f.task(t, "two on it", notes.StatusInProgress)
	gone := f.agentOn(t, 1, "adgone", &shared.ID)
	stays := f.agentOn(t, 1, "adstays", &shared.ID)
	review := f.task(t, "in review", notes.StatusReadyForReview)
	reviewer := f.agentOn(t, 1, "adreview", &review.ID)
	for _, a := range []agents.Agent{gone, reviewer} {
		if text, isErr := f.destroy(t, f.ws, focusSID, map[string]any{"id": a.ID}); isErr {
			t.Fatalf("destroy %d: %s", a.ID, text)
		}
		f.waitGone(t, a.ID)
	}
	if st := f.status(t, shared.ID); st != notes.StatusInProgress {
		t.Errorf("a task another live agent works went %q, want in_progress", st)
	}
	if st := f.status(t, review.ID); st != notes.StatusReadyForReview {
		t.Errorf("a ready_for_review task went %q", st)
	}
	if !f.exists(t, stays.ID) {
		t.Error("the other agent on the task was destroyed too")
	}
}

// TestTheReopenIsConditionalOnTheTaskStillBeingInProgress: the owner moves
// the task to ready_for_review BETWEEN the reopen's read and its write (the
// race hook); the conditional write leaves it ready_for_review.
func TestTheReopenIsConditionalOnTheTaskStillBeingInProgress(t *testing.T) {
	f := newDestroyFixture(t)
	n := f.task(t, "raced", notes.StatusInProgress)
	a := f.agentOn(t, 1, "adraced", &n.ID)
	hooked := 0
	reopenBeforeWrite = func(id int64) {
		if id == n.ID {
			hooked++
			f.setStatus(t, id, notes.StatusReadyForReview)
		}
	}
	t.Cleanup(func() { reopenBeforeWrite = nil })
	if text, isErr := f.destroy(t, f.ws, focusSID, map[string]any{"id": a.ID}); isErr {
		t.Fatal(text)
	}
	f.waitGone(t, a.ID)
	if hooked != 1 {
		t.Fatalf("the race hook ran %d time(s): the window was not exercised", hooked)
	}
	if st := f.status(t, n.ID); st != notes.StatusReadyForReview {
		t.Fatalf("a status set between the reopen's read and write was overwritten: the task is %q", st)
	}
}

// TestAnOwnersMCPDestroyReleasesTheChiefsStartOnce: the chief starts agent A
// on task n; the owner's mirrored session destroys A; the release is recorded
// (one row), n is open again, and the chief's task_dispatch starts a NEW agent
// B. A third chief dispatch with no further destroy is already_dispatched,
// naming B.
func TestAnOwnersMCPDestroyReleasesTheChiefsStartOnce(t *testing.T) {
	f := newDestroyFixture(t)
	n := f.task(t, "chief started", notes.StatusOpen)
	first := f.mustStart(t, n.ID)
	f.mustBeAlreadyDispatched(t, n.ID) // control: the start is spent before any destroy
	if text, isErr := f.destroy(t, f.ws, focusSID, map[string]any{"id": first}); isErr {
		t.Fatal(text)
	}
	f.waitGone(t, first)
	if got := f.releases(t, n.ID); got != 1 {
		t.Fatalf("release rows for the task = %d, want 1", got)
	}
	if st := f.status(t, n.ID); st != notes.StatusOpen {
		t.Fatalf("the task is %q, want open", st)
	}
	second := f.mustStart(t, n.ID)
	if second == first {
		t.Fatalf("the second start reused agent %d", first)
	}
	got := f.dispatch(n.ID)
	if !strings.Contains(got, `"already_dispatched":true`) || !strings.Contains(got, `"agent_id":`+strconv.FormatInt(second, 10)) {
		t.Fatalf("a chief dispatch with no further destroy: %s; want already_dispatched naming %d", got, second)
	}
}

// TestAnOwnersBrowserDeleteReleasesTheChiefsStart: the same release through
// the Agents page's Delete (a deliberate change to the browser path). The
// browser does not reopen the task, so the owner reopens it on its card; the
// chief may then start one new agent, and not a second.
func TestAnOwnersBrowserDeleteReleasesTheChiefsStart(t *testing.T) {
	f := newDestroyFixture(t)
	n := f.task(t, "browser destroyed", notes.StatusOpen)
	first := f.mustStart(t, n.ID)
	f.browserDelete(t, first)
	f.waitGone(t, first)
	if got := f.releases(t, n.ID); got != 1 {
		t.Fatalf("release rows for the task = %d, want 1", got)
	}
	if st := f.status(t, n.ID); st != notes.StatusInProgress {
		t.Fatalf("the browser's Delete moved the task to %q; it leaves it in_progress", st)
	}
	f.setStatus(t, n.ID, notes.StatusOpen)
	f.mustStart(t, n.ID)
	f.mustBeAlreadyDispatched(t, n.ID)
}

// TestDestroyingAnAgentTheChiefDidNotStartReleasesNothing: the chief started
// agent A on task n; the owner's OWN agent B also works n. Destroying B (on
// either door) writes no release: with A stopped and n open, the chief's
// dispatch is still already_dispatched. And on a task the chief never started
// anything for, destroying the owner's agent writes nothing and the chief's
// dispatch starts an agent exactly as it did before any destroy existed.
func TestDestroyingAnAgentTheChiefDidNotStartReleasesNothing(t *testing.T) {
	f := newDestroyFixture(t)
	n := f.task(t, "mixed", notes.StatusOpen)
	chiefs := f.mustStart(t, n.ID)
	mcpOwn := f.agentOn(t, 1, "adownmcp", &n.ID)
	webOwn := f.agentOn(t, 1, "adownweb", &n.ID)
	if text, isErr := f.destroy(t, f.ws, focusSID, map[string]any{"id": mcpOwn.ID}); isErr {
		t.Fatal(text)
	}
	f.waitGone(t, mcpOwn.ID)
	f.browserDelete(t, webOwn.ID)
	f.waitGone(t, webOwn.ID)
	if got := f.releases(t, n.ID); got != 0 {
		t.Fatalf("destroying agents the chief did not start wrote %d release(s)", got)
	}
	if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE agents SET status = 'stopped' WHERE id = $1`, chiefs); err != nil {
		t.Fatal(err)
	}
	f.setStatus(t, n.ID, notes.StatusOpen)
	f.mustBeAlreadyDispatched(t, n.ID)

	fresh := f.task(t, "never chief", notes.StatusInProgress)
	own := f.agentOn(t, 1, "adnever", &fresh.ID)
	if text, isErr := f.destroy(t, f.ws, focusSID, map[string]any{"id": own.ID}); isErr {
		t.Fatal(text)
	}
	f.waitGone(t, own.ID)
	if got := f.releases(t, fresh.ID); got != 0 {
		t.Fatalf("a task the chief never started for got %d release(s)", got)
	}
	f.mustStart(t, fresh.ID)
}

// TestAFailedTeardownReleasesAndReopensNothing: when the provisioner's Destroy
// fails (the row is kept, as Adapter.Destroy keeps it), the task is NOT
// reopened and the chief's start is NOT released — the agent may still be
// working it.
func TestAFailedTeardownReleasesAndReopensNothing(t *testing.T) {
	f := newDestroyFixture(t)
	n := f.task(t, "teardown fails", notes.StatusOpen)
	chiefs := f.mustStart(t, n.ID)
	f.prov.fail = errors.New("the backend is unreachable")
	if text, isErr := f.destroy(t, f.ws, focusSID, map[string]any{"id": chiefs}); isErr {
		t.Fatal(text)
	}
	f.waitDestroyCalled(t, 1)
	if !f.exists(t, chiefs) {
		t.Fatal("the fake deleted the row on a failed teardown")
	}
	if st := f.status(t, n.ID); st != notes.StatusInProgress {
		t.Fatalf("after a FAILED teardown the task went %q, want in_progress", st)
	}
	if got := f.releases(t, n.ID); got != 0 {
		t.Fatalf("a FAILED teardown wrote %d release(s)", got)
	}
}

// TestAgentDestroyRefusesTheChiefOthersAgentsAndBadNames: the owner's chief (by
// name and by id) gets the chief refusal; the member's worker (by name and by
// id), an unknown name and a missing id all read "no such agent"; both or
// neither of name/id is refused. Nothing reaches the provisioner and every
// agent is still there.
func TestAgentDestroyRefusesTheChiefOthersAgentsAndBadNames(t *testing.T) {
	f := newDestroyFixture(t)
	const chiefRefused = `{"error":"the chief agent cannot be deleted; to replace the chief use ` + "`muster chief replace`" + `"}`
	for _, args := range []map[string]any{{"name": f.ownChief.Name}, {"id": f.ownChief.ID}} {
		if text, _ := f.destroy(t, f.ws, focusSID, args); text != chiefRefused {
			t.Errorf("the chief %v: %s", args, text)
		}
	}
	for want, args := range map[string]map[string]any{
		`{"error":"no such agent: ` + f.worker.Name + `"}`:                                             {"name": f.worker.Name},
		`{"error":"no such agent: ` + strconv.FormatInt(f.worker.ID, 10) + `"}`:                        {"id": f.worker.ID},
		`{"error":"no such agent: no-such-agent-xyz"}`:                                                 {"name": "no-such-agent-xyz"},
		`{"error":"no such agent: 9000000123"}`:                                                        {"id": 9_000_000_123},
		`{"error":"name the agent by exactly one of name (its slug) or id, not both and not neither"}`: {"name": f.op.Name, "id": f.op.ID},
	} {
		if text, _ := f.destroy(t, f.ws, focusSID, args); text != want {
			t.Errorf("%v: %s\nwant %s", args, text, want)
		}
	}
	if text, _ := f.destroy(t, f.ws, focusSID, map[string]any{}); !strings.Contains(text, "exactly one of name") {
		t.Errorf("neither: %s", text)
	}
	time.Sleep(100 * time.Millisecond)
	if got := f.prov.destroyedIDs(); len(got) != 0 {
		t.Fatalf("refused calls reached Destroy for %v", got)
	}
	for _, a := range []agents.Agent{f.ownChief, f.worker, f.op} {
		if !f.exists(t, a.ID) {
			t.Errorf("agent %s is gone", a.Name)
		}
	}
	// CONTROL: the same door destroys a plain agent of the owner's — one with
	// no task, whose after-steps (the release's no-task case included) still
	// reach the owner's open tabs.
	f.drain()
	if text, isErr := f.destroy(t, f.ws, focusSID, map[string]any{"name": f.op.Name}); isErr {
		t.Fatalf("control: %s", text)
	}
	f.waitGone(t, f.op.ID)
	if evs := strings.Join(f.drain(), "\n"); !strings.Contains(evs, `agent.changed {"id":"`+f.op.Name+`"}`) {
		t.Fatalf("destroying a task-less agent did not tell the owner's tabs:\n%s", evs)
	}
}

// TestOnlyAMirroredSessionCanDestroyAnAgent: a worker and both chiefs, calling
// agent_destroy BY NAME through the gateway dispatcher (a gateway runtime names
// any tool it likes), are refused by the handler; over POST /mcp it is neither
// listed nor callable (unknown tool); a non-mirror credential on /mirror/mcp is
// 401; the member's mirror token cannot reach user 1's agent. The target
// survives all of it.
func TestOnlyAMirroredSessionCanDestroyAnAgent(t *testing.T) {
	f := newDestroyFixture(t)
	target := f.op2
	args := `{"id":` + strconv.FormatInt(target.ID, 10) + `}`
	callers := map[string]struct {
		a     agents.Agent
		tok   string
		owner int64
	}{
		"a worker":        {f.op, f.opTok, 1},
		"the owner chief": {f.ownChief, f.ownChiefTok, 1},
		"another chief":   {f.chief, f.chiefTok, f.member},
	}
	const refused = `{"error":"agent_destroy is available only to your owner's own mirrored Claude Code sessions"}`
	for name, c := range callers {
		if got := f.srv.dispatchAgentTool(asUser(c.owner), c.a, "agent_destroy", args); got != refused {
			t.Errorf("%s through the gateway dispatcher: %s", name, got)
		}
		if _, _, rpc := f.tool(t, c.tok, "agent_destroy", map[string]any{"id": target.ID}); rpc == nil || rpc.Code != -32602 {
			t.Errorf("%s over POST /mcp: %+v, want unknown tool", name, rpc)
		}
		if f.listed(t, c.tok)["agent_destroy"] {
			t.Errorf("%s is offered agent_destroy on /mcp", name)
		}
		for _, d := range f.srv.AgentToolDefsFor(asUser(c.owner), c.a) {
			if d.Name == "agent_destroy" {
				t.Errorf("%s is offered agent_destroy as a gateway tool", name)
			}
		}
	}
	for name, tok := range map[string]string{"the hook token": connTestHookToken, "the chief's token": f.ownChiefTok, "an agent's token": f.opTok} {
		if code, _, body := f.mirrorRPC(t, mirrorAs(tok, focusSID), mcpToolCall("agent_destroy", map[string]any{"id": target.ID})); code != http.StatusUnauthorized {
			t.Errorf("%s on /mirror/mcp: %d %s, want 401", name, code, body)
		}
	}
	// The member's own host token, naming user 1's agent: their host lists no
	// session (the fake hub serves user 1 only), so the call binds to nothing.
	if text, isErr := f.destroy(t, f.members, focusSID, map[string]any{"id": target.ID}); !isErr || !strings.Contains(text, "lists no window running Claude session") {
		t.Errorf("the member's mirror token: %s", text)
	}
	time.Sleep(100 * time.Millisecond)
	if got := f.prov.destroyedIDs(); len(got) != 0 || !f.exists(t, target.ID) {
		t.Fatalf("a refused caller destroyed %v", got)
	}
	// CONTROL: the mirrored session destroys it.
	if text, isErr := f.destroy(t, f.ws, focusSID, map[string]any{"id": target.ID}); isErr {
		t.Fatalf("control: %s", text)
	}
	f.waitGone(t, target.ID)
}
