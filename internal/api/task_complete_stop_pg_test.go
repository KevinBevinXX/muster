package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plugins"
)

// A task becoming `complete` stops the agent the chief started for it
// (task_complete_stop.go), over a real Postgres and the real chief-dispatch
// ledger: the chief's live count drops so a start refused at the cap succeeds;
// `ready_for_review` stops nothing; an agent the owner started by hand is left
// alone; an agent already stopped is a no-op; the task's one start stays spent;
// and every writer that can set `complete` reaches it.
//
// ⚠ The refusal and answer texts asserted here are the tools' own constants
// (chiefDispatchLiveCap, already_dispatched), which exist before this change,
// so the behaviour tests are red on the pre-change tree for a behavioural
// reason: no Stop is ever called.

// stopProv is the task-routing fixture's recording provisioner plus a Stop that
// records the agent and writes `stopped` the way agentprovision.Adapter.Stop
// does on success.
type stopProv struct {
	*recordingProvisioner
	pool    *pgxpool.Pool
	smu     sync.Mutex
	stopped []int64
}

func (p *stopProv) Stop(_ context.Context, id int64) error {
	p.smu.Lock()
	p.stopped = append(p.stopped, id)
	p.smu.Unlock()
	_, err := p.pool.Exec(authz.System(context.Background()), `UPDATE agents SET status = 'stopped' WHERE id = $1`, id)
	return err
}

// stops is how many times Stop ran for agent id, after waiting up to 2s for want.
func (p *stopProv) stops(id int64, want int) int {
	deadline := time.Now().Add(2 * time.Second)
	for {
		p.smu.Lock()
		n := 0
		for _, x := range p.stopped {
			if x == id {
				n++
			}
		}
		p.smu.Unlock()
		if n >= want || time.Now().After(deadline) {
			return n
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// noStopWait is how long a "nothing is stopped" assertion watches for a Stop
// that should not come. The decision not to stop is taken synchronously, before
// any goroutine exists; this window is what lets a mutant that DOES stop be seen.
const noStopWait = 500 * time.Millisecond

type completeStopFixture struct {
	*taskRouteFixture
	sp *stopProv
}

func newCompleteStopFixture(t *testing.T) *completeStopFixture {
	t.Helper()
	sp := &stopProv{}
	f := &completeStopFixture{sp: sp}
	f.taskRouteFixture = newTaskRouteFixture(t, true, true, func(ext *Extensions) {
		sp.recordingProvisioner = ext.Provisioner.(*recordingProvisioner)
		ext.Provisioner = sp
	})
	sp.pool = f.pool
	return f
}

// dispatch has the chief start an agent on n (task_dispatch) and returns it.
func (f *completeStopFixture) dispatch(t *testing.T, n notes.Note) agents.Agent {
	t.Helper()
	text := f.call(f.chief, taskDispatchTool, map[string]any{"task_id": n.ID})
	var out struct {
		AgentID int64 `json:"agent_id"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil || out.AgentID <= 0 || strings.Contains(text, `"error"`) {
		t.Fatalf("the chief's start on task %d: %s", n.ID, text)
	}
	a, err := agents.NewPG(f.pool).Get(f.asOwner, out.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// byHand creates an agent the owner started by hand, running, bound to n.
func (f *completeStopFixture) byHand(t *testing.T, n notes.Note, name string) agents.Agent {
	t.Helper()
	a, err := agents.NewPG(f.pool).Create(f.asOwner, agents.Agent{Name: name + "-" + strconv.FormatInt(time.Now().UnixNano(), 36),
		Namespace: "ns-" + name, Status: agents.StatusRunning, NoteID: &n.ID})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (f *completeStopFixture) agentStatus(t *testing.T, id int64) string {
	t.Helper()
	a, err := agents.NewPG(f.pool).Get(f.asOwner, id)
	if err != nil {
		t.Fatal(err)
	}
	return a.Status
}

// serve runs handler h on req as the owner, with path values pv.
func (f *completeStopFixture) serve(h http.HandlerFunc, req *http.Request, pv map[string]string) *httptest.ResponseRecorder {
	req = req.WithContext(f.asOwner)
	for k, v := range pv {
		req.SetPathValue(k, v)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// The writers that can set `complete`, each driven the way its route is.
func (f *completeStopFixture) humanPatch(t *testing.T, id int64, status string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPatch, "/tasks/"+strconv.FormatInt(id, 10)+"/status",
		strings.NewReader(url.Values{"status": {status}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if rec := f.serve(f.srv.handleNoteStatus, req, map[string]string{"id": strconv.FormatInt(id, 10)}); rec.Code != http.StatusOK {
		t.Fatalf("PATCH /tasks/%d/status %s: HTTP %d %s", id, status, rec.Code, rec.Body)
	}
}

func (f *completeStopFixture) markComplete(t *testing.T, id int64) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/tasks/"+strconv.FormatInt(id, 10)+"/complete", nil)
	if rec := f.serve(f.srv.handleNoteComplete, req, map[string]string{"id": strconv.FormatInt(id, 10)}); rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /tasks/%d/complete: HTTP %d %s", id, rec.Code, rec.Body)
	}
}

func (f *completeStopFixture) machinePatch(t *testing.T, id int64, status string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPatch, "/api/tasks/"+strconv.FormatInt(id, 10)+"/status",
		strings.NewReader(`{"status":"`+status+`"}`))
	if rec := f.serve(f.srv.handleAPITaskStatus, req, map[string]string{"id": strconv.FormatInt(id, 10)}); rec.Code != http.StatusOK {
		t.Fatalf("PATCH /api/tasks/%d/status %s: HTTP %d %s", id, status, rec.Code, rec.Body)
	}
}

func (f *completeStopFixture) merge(t *testing.T, winner, loser int64) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/tasks/merge",
		strings.NewReader(url.Values{"winner": {strconv.FormatInt(winner, 10)}, "loser": {strconv.FormatInt(loser, 10)}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if rec := f.serve(f.srv.handleTaskMerge, req, nil); rec.Code != http.StatusOK {
		t.Fatalf("POST /tasks/merge %d<-%d: HTTP %d %s", winner, loser, rec.Code, rec.Body)
	}
}

// mustNotStop asserts no Stop reaches agent id within noStopWait.
func (f *completeStopFixture) mustNotStop(t *testing.T, id int64, why string) {
	t.Helper()
	time.Sleep(noStopWait)
	if n := f.sp.stops(id, 0); n != 0 {
		t.Errorf("%s: agent %d was stopped %d time(s), want none", why, id, n)
	}
}

// TestCompletingATaskStopsTheChiefsAgentAndFreesItsSlot (cases 1 and 5): the
// chief at its live cap is refused a start; completing one of its tasks stops
// the agent it started there (and only that one), the refused start then
// succeeds, and the completed task's start stays spent.
func TestCompletingATaskStopsTheChiefsAgentAndFreesItsSlot(t *testing.T) {
	f := newCompleteStopFixture(t)
	var tasks []notes.Note
	var started []agents.Agent
	for i := 0; i < plugins.MaxChiefLiveAgents; i++ {
		n := f.task(t, "cap "+strconv.Itoa(i))
		tasks = append(tasks, n)
		started = append(started, f.dispatch(t, n))
	}
	waiting := f.task(t, "approved but refused")
	// (mustRefuse's "no Dispatch at all" check does not fit here: the five
	// starts above each made one.)
	if text := f.call(f.chief, taskDispatchTool, map[string]any{"task_id": waiting.ID}); !strings.Contains(text, chiefDispatchLiveCap) {
		t.Fatalf("at the cap, a start answered %s; want the live cap's refusal", text)
	}
	if len(f.agentsOn(t, waiting.ID)) != 0 || f.reservations(t, waiting.ID) != 0 {
		t.Fatal("the start refused at the cap still made an agent or a reservation")
	}

	f.humanPatch(t, tasks[0].ID, notes.StatusComplete)
	if n := f.sp.stops(started[0].ID, 1); n != 1 {
		t.Fatalf("completing task %d stopped its chief-started agent %d %d time(s), want 1", tasks[0].ID, started[0].ID, n)
	}
	if got := f.agentStatus(t, started[0].ID); got != agents.StatusStopped {
		t.Fatalf("agent %d status %q after the stop, want stopped", started[0].ID, got)
	}
	for _, a := range started[1:] {
		if n := f.sp.stops(a.ID, 0); n != 0 {
			t.Errorf("agent %d on a task that is not complete was stopped %d time(s)", a.ID, n)
		}
	}

	// The slot is free: the start refused at the cap now succeeds.
	text := f.call(f.chief, taskDispatchTool, map[string]any{"task_id": waiting.ID})
	if strings.Contains(text, `"error"`) || !strings.Contains(text, `"agent_id"`) {
		t.Fatalf("after the stop, the refused start is still refused: %s", text)
	}

	// Case 5: the completed task's one start is still spent.
	again := f.call(f.chief, taskDispatchTool, map[string]any{"task_id": tasks[0].ID})
	if !strings.Contains(again, `"already_dispatched":true`) ||
		!strings.Contains(again, `"agent_id":`+strconv.FormatInt(started[0].ID, 10)) {
		t.Fatalf("a repeat start on the completed task: %s; want already_dispatched naming agent %d", again, started[0].ID)
	}
	if got := f.reservations(t, tasks[0].ID); got != 1 {
		t.Errorf("reservations on the completed task = %d, want 1 (the stop released nothing)", got)
	}
}

// TestReadyForReviewStopsNothing (case 2): an agent awaiting review must stay
// reachable through task_forward.
func TestReadyForReviewStopsNothing(t *testing.T) {
	f := newCompleteStopFixture(t)
	n := f.task(t, "in review")
	a := f.dispatch(t, n)
	f.humanPatch(t, n.ID, notes.StatusReadyForReview)
	f.machinePatch(t, n.ID, notes.StatusReadyForReview)
	f.mustNotStop(t, a.ID, "ready_for_review")
	if got := f.agentStatus(t, a.ID); !agentLive(got) {
		t.Errorf("agent status %q after ready_for_review, want it still live", got)
	}
	// Positive control: the same agent IS stopped once the task is complete.
	f.humanPatch(t, n.ID, notes.StatusComplete)
	if got := f.sp.stops(a.ID, 1); got != 1 {
		t.Fatalf("positive control: completing the task stopped agent %d %d time(s), want 1", a.ID, got)
	}
}

// TestAnAgentTheOwnerStartedByHandIsNotStopped (case 3): on a task the chief
// started an agent for, and on one it never did.
func TestAnAgentTheOwnerStartedByHandIsNotStopped(t *testing.T) {
	f := newCompleteStopFixture(t)

	// Alongside the chief's own: only the chief's is stopped.
	shared := f.task(t, "both")
	chiefs := f.dispatch(t, shared)
	mine := f.byHand(t, shared, "cshand")
	f.markComplete(t, shared.ID)
	if got := f.sp.stops(chiefs.ID, 1); got != 1 {
		t.Fatalf("positive control: the chief's agent %d was stopped %d time(s), want 1", chiefs.ID, got)
	}
	f.mustNotStop(t, mine.ID, "the owner's own agent beside the chief's")

	// A task the chief never started anything on.
	solo := f.task(t, "owner only")
	only := f.byHand(t, solo, "csonly")
	f.markComplete(t, solo.ID)
	f.mustNotStop(t, only.ID, "the owner's own agent on a task with no chief start")
	if got := f.agentStatus(t, only.ID); got != agents.StatusRunning {
		t.Errorf("the owner's agent reads %q, want running", got)
	}
}

// TestAnAlreadyStoppedChiefAgentIsANoOp (case 4): nothing is called and the
// status write succeeds.
func TestAnAlreadyStoppedChiefAgentIsANoOp(t *testing.T) {
	f := newCompleteStopFixture(t)
	n := f.task(t, "stopped already")
	a := f.dispatch(t, n)
	if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE agents SET status = 'stopped' WHERE id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	f.machinePatch(t, n.ID, notes.StatusComplete) // fails the test on a non-200
	f.mustNotStop(t, a.ID, "an agent already stopped")
	if cur, err := f.notes.Get(f.asOwner, n.ID); err != nil || cur.Status != notes.StatusComplete {
		t.Fatalf("task status %q (%v), want complete", cur.Status, err)
	}
	// Positive control in the same fixture: a live chief agent IS stopped.
	m := f.task(t, "live one")
	b := f.dispatch(t, m)
	f.machinePatch(t, m.ID, notes.StatusComplete)
	if got := f.sp.stops(b.ID, 1); got != 1 {
		t.Fatalf("positive control: agent %d stopped %d time(s), want 1", b.ID, got)
	}
}

// TestEveryCompleteWriterStopsTheChiefsAgent (case 6): each writer on
// expectedCompleteWriters that can set `complete`, driven through its own
// handler.
func TestEveryCompleteWriterStopsTheChiefsAgent(t *testing.T) {
	f := newCompleteStopFixture(t)
	drive := map[string]func(t *testing.T, n notes.Note){
		"handleNoteStatus":    func(t *testing.T, n notes.Note) { f.humanPatch(t, n.ID, notes.StatusComplete) },
		"handleNoteComplete":  func(t *testing.T, n notes.Note) { f.markComplete(t, n.ID) },
		"handleAPITaskStatus": func(t *testing.T, n notes.Note) { f.machinePatch(t, n.ID, notes.StatusComplete) },
		"handleTaskMerge": func(t *testing.T, n notes.Note) {
			// A merge refuses an in_progress loser; the chief's start moved n there.
			f.humanPatch(t, n.ID, notes.StatusReadyForReview)
			f.merge(t, f.task(t, "merge winner").ID, n.ID)
		},
	}
	for writer, can := range expectedCompleteWriters {
		if !can {
			continue
		}
		run, ok := drive[writer]
		if !ok {
			t.Errorf("%s can set complete but this test does not drive it", writer)
			continue
		}
		t.Run(writer, func(t *testing.T) {
			n := f.task(t, "via "+writer)
			a := f.dispatch(t, n)
			run(t, n)
			if cur, err := f.notes.Get(f.asOwner, n.ID); err != nil || cur.Status != notes.StatusComplete {
				t.Fatalf("task status %q (%v) after %s, want complete", cur.Status, err, writer)
			}
			if got := f.sp.stops(a.ID, 1); got != 1 {
				t.Fatalf("%s set task %d complete and stopped the chief's agent %d %d time(s), want 1", writer, n.ID, a.ID, got)
			}
		})
	}
	if len(drive) != countTrue(expectedCompleteWriters) {
		t.Errorf("this test drives %d writers; expectedCompleteWriters names %d that can set complete", len(drive), countTrue(expectedCompleteWriters))
	}
}

func countTrue(m map[string]bool) int {
	n := 0
	for _, v := range m {
		if v {
			n++
		}
	}
	return n
}

// TestALedgerAgentNoLongerOnTheTaskIsNotStopped: the ledger names the agent the
// chief's start made, but the stop also requires the agent ROW to still be
// bound to the completed task. No muster path rebinds an agent today, so the
// disagreement is made out of band here; the guard keeps a completion from
// stopping an agent that is now working something else.
func TestALedgerAgentNoLongerOnTheTaskIsNotStopped(t *testing.T) {
	f := newCompleteStopFixture(t)
	n := f.task(t, "ledger says so")
	elsewhere := f.task(t, "now working this")
	a := f.dispatch(t, n)
	if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE agents SET note_id = $2 WHERE id = $1`, a.ID, elsewhere.ID); err != nil {
		t.Fatal(err)
	}
	f.humanPatch(t, n.ID, notes.StatusComplete)
	f.mustNotStop(t, a.ID, "an agent the row binds to another task")
	// Positive control: completing a task whose ledger and row agree does stop.
	m := f.task(t, "agree")
	b := f.dispatch(t, m)
	f.humanPatch(t, m.ID, notes.StatusComplete)
	if got := f.sp.stops(b.ID, 1); got != 1 {
		t.Fatalf("positive control: agent %d stopped %d time(s), want 1", b.ID, got)
	}
}
