package api

import (
	"bytes"
	"context"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentprovision"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/provision"
)

// The stopped-agent reaper (agent_reap.go) over a real Postgres and the
// destroy fixture's recording provisioner: an agent stopped past the window is
// torn down through destroyAgent, and nothing else is.
//
// 🔴 FIXTURE AGES OVERSHOOT AND UNDERSHOOT THE 14-DAY WINDOW (15 and 13 days),
// never sit on it.

// lockedBuf is a log sink safe for the teardown goroutine to write while the
// test reads it.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// 🔴 NO WAIT HERE IS ON WALL TIME. A pass's return value is the number of
// teardowns it started (each counted as it starts, before the pass returns),
// so "nothing was started" needs no wait at all; and each teardown it did
// start is awaited through the server's agentReapDone signal, which fires once
// Destroy and the after-steps have finished. The one deadline left
// (reapHangGuard) is a hang guard that only a stuck teardown reaches.
const reapHangGuard = time.Minute

// reapDone is one finished reaper teardown, as agentReapDone signals it.
type reapDone struct {
	id  int64
	err error
}

type reapAPIFixture struct {
	*destroyFixture
	log    *lockedBuf
	reaped chan reapDone
}

func newReapAPIFixture(t *testing.T) *reapAPIFixture {
	t.Helper()
	f := &reapAPIFixture{destroyFixture: newDestroyFixture(t), log: &lockedBuf{}, reaped: make(chan reapDone, 256)}
	f.srv.logger = log.New(f.log, "", 0)
	f.srv.SetAgentReapStoppedAfter(14 * 24 * time.Hour)
	// A teardown finishing after the test stopped reading (one a loop started,
	// or the shared database's other due agents) must not block its goroutine:
	// once the test ends, the send gives way.
	stopped := make(chan struct{})
	t.Cleanup(func() { close(stopped) })
	f.srv.agentReapDone = func(id int64, err error) {
		select {
		case f.reaped <- reapDone{id, err}:
		case <-stopped:
		}
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("server log:\n%s", f.log.String())
		}
	})
	return f
}

// awaitTeardowns waits for n reaper teardowns to finish, failing on one whose
// Destroy failed.
func (f *reapAPIFixture) awaitTeardowns(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case d := <-f.reaped:
			if d.err != nil {
				t.Fatalf("the teardown of agent %d failed: %v", d.id, d.err)
			}
		case <-time.After(reapHangGuard):
			t.Fatalf("%d of %d teardown(s) never finished", n-i, n)
		}
	}
}

// awaitTeardownOf waits for the teardown of agent id to finish, skipping
// others' (a loop's pass reaps whatever in the shared database is due).
func (f *reapAPIFixture) awaitTeardownOf(t *testing.T, id int64) {
	t.Helper()
	for {
		select {
		case d := <-f.reaped:
			if d.id != id {
				continue
			}
			if d.err != nil {
				t.Fatalf("the teardown of agent %d failed: %v", d.id, d.err)
			}
			return
		case <-time.After(reapHangGuard):
			t.Fatalf("agent %d's teardown never finished", id)
		}
	}
}

// mustBeGone fails unless agent id's row is gone.
func (f *reapAPIFixture) mustBeGone(t *testing.T, id int64) {
	t.Helper()
	if f.exists(t, id) {
		t.Fatalf("agent %d is still there (destroyed: %v)", id, f.prov.destroyedIDs())
	}
}

// stopAged is an agent that RAN and was stopped: agent id is brought up
// (`provisioning`, `running`, as a dispatch or a Start writes them), moved to
// `stopped` through the store (as the provisioner's Stop does), and its
// stopped_at aged to `days` days ago.
func (f *reapAPIFixture) stopAged(t *testing.T, id int64, days int) {
	t.Helper()
	f.setAgentStatus(t, id, agents.StatusProvisioning)
	f.setAgentStatus(t, id, agents.StatusRunning)
	f.ageStopped(t, id, days)
}

// savedAged is an agent saved for later and never started: inserted
// `provisioning` (as every agent row is), then `stopped` with no instance built
// (Dispatch without a kickoff), its stopped_at aged to `days` days ago.
func (f *reapAPIFixture) savedAged(t *testing.T, name string, days int) agents.Agent {
	t.Helper()
	a, err := agents.NewPG(f.pool).Create(asUser(1), agents.Agent{Name: name + "-" + strconv.FormatInt(time.Now().UnixNano(), 36),
		Namespace: "devpod-" + name, Status: agents.StatusProvisioning})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM agents WHERE id = $1`, a.ID)
	})
	f.ageStopped(t, a.ID, days)
	return a
}

// ageStopped moves agent id to `stopped` and ages its stopped_at.
func (f *reapAPIFixture) ageStopped(t *testing.T, id int64, days int) {
	t.Helper()
	f.setAgentStatus(t, id, agents.StatusStopped)
	if _, err := f.pool.Exec(authz.System(context.Background()),
		`UPDATE agents SET stopped_at = now() - make_interval(days => $2) WHERE id = $1`, id, days); err != nil {
		t.Fatal(err)
	}
}

// setAgentStatus writes agent id's status as the provisioner does
// (agents.Store.UpdateStatus, through Adapter.setStatus).
func (f *reapAPIFixture) setAgentStatus(t *testing.T, id int64, status string) {
	t.Helper()
	if err := agents.NewPG(f.pool).UpdateStatus(authz.System(context.Background()), id, status, "", ""); err != nil {
		t.Fatal(err)
	}
}

// reap runs one reaper pass, as RunTaskReap does (under System), and waits for
// every teardown it started to have finished. A pass that started none
// returns 0 and there is nothing to wait for.
func (f *reapAPIFixture) reap(t *testing.T) int {
	t.Helper()
	n := f.srv.reapStoppedAgents(authz.System(context.Background()))
	f.awaitTeardowns(t, n)
	return n
}

// destroyedOnly fails unless Destroy ran for exactly want (in any order),
// counting only the ids in watch (the package's database is shared).
func (f *reapAPIFixture) destroyedOnly(t *testing.T, watch []int64, want ...int64) {
	t.Helper()
	in := map[int64]bool{}
	for _, id := range watch {
		in[id] = true
	}
	got := map[int64]int{}
	for _, id := range f.prov.destroyedIDs() {
		if in[id] {
			got[id]++
		}
	}
	wantSet := map[int64]bool{}
	for _, id := range want {
		wantSet[id] = true
		if got[id] != 1 {
			t.Errorf("agent %d: Destroy ran %d time(s), want 1", id, got[id])
		}
	}
	for id, n := range got {
		if !wantSet[id] {
			t.Errorf("agent %d was destroyed (%d time(s)); it must not be", id, n)
		}
	}
}

// TestTheReaperDestroysEveryAgentStoppedPastTheWindow: user 1's agents, each
// with a stopped_at 15 days old unless noted — a stopped one on a task (due), a
// stopped one 13 days old, a running one and an errored one (stopped, aged,
// then moved on, so status is the only difference), and two STANDING agents
// (user 1's own, and the member's agent labelled `chief`), both stopped and
// aged. One pass: Destroy runs for every aged, stopped agent — the two standing
// ones included — the due agent's task is kept, and the log names its reap. A
// second pass destroys nothing more.
//
// 🔴 THE TWO STANDING AGENTS USED TO BE THE EXEMPTIONS THIS TEST PINNED, and
// their inversion is the point. The reaper skipped whichever agent held the
// reserved `chief` slug or carried the `chief` display name, silently and on
// every pass. muster has no privileged agent, so STOPPED AND AGED is the whole
// rule: what keeps an agent is being young, running or errored, never what it
// is called.
func TestTheReaperDestroysEveryAgentStoppedPastTheWindow(t *testing.T) {
	f := newReapAPIFixture(t)
	n := f.task(t, "long stopped", notes.StatusInProgress)
	due := f.agentOn(t, 1, "rpdue", &n.ID)
	young := f.agentOn(t, 1, "rpyoung", nil)
	running := f.agentOn(t, 1, "rprunning", nil)
	errored := f.agentOn(t, 1, "rperror", nil)
	f.stopAged(t, due.ID, 15)
	f.stopAged(t, young.ID, 13)
	for id, st := range map[int64]string{running.ID: agents.StatusRunning, errored.ID: agents.StatusError} {
		f.stopAged(t, id, 15)
		f.setAgentStatus(t, id, st)
	}
	// No status is restored afterwards: these two are reaped by this pass, and
	// the fixture re-creates the standing agent by name when a later test needs it.
	f.stopAged(t, f.ownFleetAgent.ID, 15)
	f.stopAged(t, f.agent.ID, 15)
	watch := []int64{due.ID, young.ID, running.ID, errored.ID, f.ownFleetAgent.ID, f.agent.ID}

	if got := f.reap(t); got < 3 {
		t.Fatalf("the pass started %d teardown(s); want the due agent's and both standing agents'", got)
	}
	for name, id := range map[string]int64{"the due": due.ID, "the owner's standing": f.ownFleetAgent.ID, "the labelled": f.agent.ID} {
		f.mustBeGone(t, id)
		if f.exists(t, id) {
			t.Errorf("%s agent %d survived the reaper", name, id)
		}
	}
	f.destroyedOnly(t, watch, due.ID, f.ownFleetAgent.ID, f.agent.ID)
	for name, id := range map[string]int64{"13 days stopped": young.ID, "running": running.ID, "error": errored.ID} {
		if !f.exists(t, id) {
			t.Errorf("%s agent %d is gone", name, id)
		}
	}
	if st := f.status(t, n.ID); st != notes.StatusInProgress {
		t.Errorf("the reaped agent's task is %q; the reaper leaves it as it was", st)
	}
	wantLog := "agent-reap: destroying agent " + strconv.FormatInt(due.ID, 10) + " (" + due.Name + ") of user 1, task " + strconv.FormatInt(n.ID, 10) + ": stopped since "
	if logs := f.log.String(); !strings.Contains(logs, wantLog) || !strings.Contains(logs, "longer than 336h0m0s") {
		t.Errorf("the log does not record the reap (%q):\n%s", wantLog, logs)
	}

	f.reap(t)
	f.destroyedOnly(t, watch, due.ID, f.ownFleetAgent.ID, f.agent.ID)
}

// TestANeverStartedAgentIsNotReaped: an agent saved for later and never
// started, stopped 15 days, survives a pass; an agent that ran and was then
// stopped 15 days is destroyed by the same pass.
func TestANeverStartedAgentIsNotReaped(t *testing.T) {
	f := newReapAPIFixture(t)
	saved := f.savedAged(t, "rpsaved", 15)
	ran := f.agentOn(t, 1, "rpran", nil)
	f.stopAged(t, ran.ID, 15)
	f.reap(t)
	f.mustBeGone(t, ran.ID)
	if !f.exists(t, saved.ID) {
		t.Fatal("a never-started (saved) agent stopped for 15 days was destroyed")
	}
	f.destroyedOnly(t, []int64{saved.ID, ran.ID}, ran.ID)
}

// TestARestartedAgentWaitsAFreshWindow: an agent stopped 15 days ago is Started
// (provisioning, then running) and stopped again; the pass destroys nothing.
func TestARestartedAgentWaitsAFreshWindow(t *testing.T) {
	f := newReapAPIFixture(t)
	a := f.agentOn(t, 1, "rprestart", nil)
	f.stopAged(t, a.ID, 15)
	for _, st := range []string{agents.StatusProvisioning, agents.StatusRunning, agents.StatusStopped} {
		f.setAgentStatus(t, a.ID, st)
	}
	f.reap(t)
	f.destroyedOnly(t, []int64{a.ID})
	if !f.exists(t, a.ID) {
		t.Fatal("a restarted, re-stopped agent was destroyed")
	}
}

// hookedReapStore is the agents store with a hook between the reaper's scan
// and its claim.
type hookedReapStore struct {
	*agents.PGStore
	afterList func()
	// only, when set, keeps just that agent in the scan's result.
	only int64
}

func (h *hookedReapStore) ListReapableStopped(ctx context.Context, after time.Duration) ([]agents.StoppedAgent, error) {
	out, err := h.PGStore.ListReapableStopped(ctx, after)
	h.afterList()
	if h.only != 0 {
		kept := out[:0]
		for _, a := range out {
			if a.ID == h.only {
				kept = append(kept, a)
			}
		}
		out = kept
	}
	return out, err
}

// TestAnAgentStartedBetweenTheScanAndTheClaimSurvives: the scan reads a due
// agent; before the claim, the agent is Started (and, in the second case,
// stopped again). Destroy never runs and the row stays. CONTROL: with no
// restart the same hooked pass destroys it.
func TestAnAgentStartedBetweenTheScanAndTheClaimSurvives(t *testing.T) {
	for name, statuses := range map[string][]string{
		"running":            {agents.StatusProvisioning, agents.StatusRunning},
		"stopped again":      {agents.StatusProvisioning, agents.StatusRunning, agents.StatusStopped},
		"control: unchanged": nil,
	} {
		t.Run(name, func(t *testing.T) {
			f := newReapAPIFixture(t)
			a := f.agentOn(t, 1, "rprace", nil)
			f.stopAged(t, a.ID, 15)
			f.srv.ext.Agents = &hookedReapStore{PGStore: agents.NewPG(f.pool), afterList: func() {
				for _, st := range statuses {
					f.setAgentStatus(t, a.ID, st)
				}
			}}
			f.reap(t)
			if statuses == nil {
				f.mustBeGone(t, a.ID)
				f.destroyedOnly(t, []int64{a.ID}, a.ID)
				return
			}
			f.destroyedOnly(t, []int64{a.ID})
			if !f.exists(t, a.ID) {
				t.Fatal("an agent Started between the scan and the claim was destroyed")
			}
		})
	}
}

// gatedProv holds every Destroy until gate is closed, so a test can let
// several reaper passes finish claiming before any teardown deletes the row.
type gatedProv struct {
	*destroyProv
	gate chan struct{}
}

func (g *gatedProv) Destroy(ctx context.Context, id int64) error {
	<-g.gate
	return g.destroyProv.Destroy(ctx, id)
}

// TestTwoPassesAtOnceDestroyAnAgentOnce: two reaper passes (two pods) both
// scan before either claims — a barrier after the scan holds each until both
// have scanned — and every teardown is held until both passes have returned,
// so the row is still there when the second pass claims. Exactly one pass
// starts a teardown, and Destroy runs once.
func TestTwoPassesAtOnceDestroyAnAgentOnce(t *testing.T) {
	f := newReapAPIFixture(t)
	a := f.agentOn(t, 1, "rptwice", nil)
	f.stopAged(t, a.ID, 15)
	var arrived sync.WaitGroup
	arrived.Add(2)
	f.srv.ext.Agents = &hookedReapStore{PGStore: agents.NewPG(f.pool), afterList: func() {
		arrived.Done()
		arrived.Wait()
	}}
	gate := make(chan struct{})
	f.srv.ext.Provisioner = &gatedProv{destroyProv: f.prov, gate: gate}
	var passes sync.WaitGroup
	started := make([]int, 2)
	for i := range started {
		passes.Add(1)
		go func(i int) {
			defer passes.Done()
			started[i] = f.srv.reapStoppedAgents(authz.System(context.Background()))
		}(i)
	}
	passes.Wait()
	close(gate)
	// Every teardown either pass started is awaited — the count is the passes'
	// own, so a second teardown, had one been started, is waited for too.
	f.awaitTeardowns(t, started[0]+started[1])
	f.mustBeGone(t, a.ID)
	if started[0]+started[1] != 1 {
		t.Errorf("the two passes started %d and %d teardown(s); want one between them", started[0], started[1])
	}
	f.destroyedOnly(t, []int64{a.ID}, a.ID)
}

// TestReapingTheChiefsAgentReleasesItsStart: the agent's task_dispatch starts
// an agent on task n; the agent is stopped (as the provisioner's Stop writes
// it) and aged past the window, and the task is set open. Before the reap the
// agent's start is spent;
// the reap records one release AS THE OWNER (a System principal cannot write
// it), and the agent may start one new agent on n.
func TestReapingTheChiefsAgentReleasesItsStart(t *testing.T) {
	f := newReapAPIFixture(t)
	n := f.task(t, "agent started, long stopped", notes.StatusOpen)
	started := f.mustStart(t, n.ID)
	f.stopAged(t, started, 15)
	f.setStatus(t, n.ID, notes.StatusOpen)
	f.mustBeAlreadyDispatched(t, n.ID) // control: spent before the reap
	f.reap(t)
	f.mustBeGone(t, started)
	if got := f.releases(t, n.ID); got != 1 {
		t.Fatalf("release rows for the task = %d, want 1\n%s", got, f.log.String())
	}
	f.mustStart(t, n.ID)
}

// TestRunTaskReapAlsoReapsStoppedAgents: the loop production starts
// (RunTaskReap) runs the stopped-agent reaper on its ticks, and announces it
// with its window. (The idle-task half is turned off here so this test touches
// no task.)
func TestRunTaskReapAlsoReapsStoppedAgents(t *testing.T) {
	f := newReapAPIFixture(t)
	f.srv.SetTaskReapAfter(0)
	a := f.agentOn(t, 1, "rploop", nil)
	f.stopAged(t, a.ID, 15)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); f.srv.RunTaskReap(ctx, 20*time.Millisecond) }()
	t.Cleanup(func() { cancel(); <-done })
	f.awaitTeardownOf(t, a.ID)
	f.mustBeGone(t, a.ID)
	if logs := f.log.String(); !strings.Contains(logs, "agent reaper: sweeping every 20ms for agents stopped for >336h0m0s") {
		t.Errorf("the loop did not announce the agent reaper:\n%s", logs)
	}
}

// TestTheReaperIsOffAtZero: with the window set to 0 a due agent stays.
func TestTheReaperIsOffAtZero(t *testing.T) {
	f := newReapAPIFixture(t)
	f.srv.SetAgentReapStoppedAfter(0)
	a := f.agentOn(t, 1, "rpoff", nil)
	f.stopAged(t, a.ID, 15)
	if got := f.reap(t); got != 0 {
		t.Fatalf("an off reaper started %d teardown(s)", got)
	}
	f.destroyedOnly(t, []int64{a.ID})
	// CONTROL: the same agent with the reaper on.
	f.srv.SetAgentReapStoppedAfter(14 * 24 * time.Hour)
	if got := f.reap(t); got < 1 {
		t.Fatalf("control: the reaper turned on started %d teardown(s)", got)
	}
	f.mustBeGone(t, a.ID)
	f.destroyedOnly(t, []int64{a.ID}, a.ID)
}

// TestAStoppedRowOverALiveInstanceIsNotReaped reproduces a Start whose driver
// call succeeded and whose status write was lost, with the real adapter over
// the noop driver: the agent is dispatched (its instance created), Stopped
// (scaled to zero, row `stopped`), then its instance is scaled back up by the
// driver alone — what Start does before it writes `provisioning`. The row says
// `stopped`, 15 days old; the instance is running. The pass destroys nothing
// and says why. CONTROL, same agent: once the instance is scaled to zero again,
// the next pass destroys it.
func TestAStoppedRowOverALiveInstanceIsNotReaped(t *testing.T) {
	f := newReapAPIFixture(t)
	driver := provision.MustNewNoop()
	store := agents.NewPG(f.pool)
	adapter, err := agentprovision.New(agentprovision.Config{Driver: driver, Store: store,
		Spec:               agentspec.Config{ImageRepo: "registry.example.test/muster/agent-runtime", APIBaseURL: "http://muster.example.test:8105"},
		KickoffDeliverable: true})
	if err != nil {
		t.Fatal(err)
	}
	f.srv.ext.Provisioner = adapter
	a := f.agentOn(t, 1, "rplive", nil)
	// The scan is narrowed to THIS agent, so the pass's count below is about
	// it alone and not about whatever else the package's shared database holds.
	f.srv.ext.Agents = &hookedReapStore{PGStore: store, afterList: func() {}, only: a.ID}
	ctx := asUser(1)
	if err := adapter.Dispatch(ctx, a.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Stop(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	f.ageStopped(t, a.ID, 15)
	if err := driver.Scale(context.Background(), agents.RefOf(a), 1); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Get(ctx, a.ID); err != nil || got.Status != agents.StatusStopped {
		t.Fatalf("fixture: the row is %q (%v), want stopped", got.Status, err)
	}

	// reapStoppedAgents counts a teardown as it starts it, before it returns,
	// so zero here means none was started — no waiting on the background
	// teardown to find out.
	if n := f.srv.reapStoppedAgents(authz.System(context.Background())); n != 0 {
		t.Fatalf("the pass started %d teardown(s) for an agent stored stopped whose instance is running", n)
	}
	if !f.exists(t, a.ID) {
		t.Fatal("an agent stored stopped whose instance is running was destroyed")
	}
	if inst, err := driver.Get(context.Background(), agents.RefOf(a)); err != nil || inst.Replicas != 1 {
		t.Fatalf("the live instance was touched: %+v %v", inst, err)
	}
	want := "agent-reap: agent " + strconv.FormatInt(a.ID, 10) + " (" + a.Name + ") is stored stopped but its instance reads "
	if logs := f.log.String(); !strings.Contains(logs, want) {
		t.Errorf("the skip is not logged (%q):\n%s", want, logs)
	}

	if err := driver.Scale(context.Background(), agents.RefOf(a), 0); err != nil {
		t.Fatal(err)
	}
	if n := f.reap(t); n != 1 {
		t.Fatalf("control: the pass started %d teardown(s) once the instance was stopped, want 1", n)
	}
	f.mustBeGone(t, a.ID)
}

// blindProv is the destroy fixture's provisioner whose instance list fails.
type blindProv struct{ *destroyProv }

func (blindProv) Instances(context.Context) ([]provision.Instance, error) {
	return nil, provision.ErrBlind
}

// TestTheReaperDestroysNothingWhenInstancesCannotBeRead: a due agent, and a
// provisioner that cannot list its instances. The pass destroys nothing and
// says so. (Control: TestTheReaperDestroysOnlyAnAgentStoppedPastTheWindow,
// whose provisioner lists none.)
func TestTheReaperDestroysNothingWhenInstancesCannotBeRead(t *testing.T) {
	f := newReapAPIFixture(t)
	f.srv.ext.Provisioner = blindProv{f.prov}
	a := f.agentOn(t, 1, "rpblind", nil)
	f.stopAged(t, a.ID, 15)
	f.srv.ext.Agents = &hookedReapStore{PGStore: agents.NewPG(f.pool), afterList: func() {}, only: a.ID}
	if got := f.reap(t); got != 0 {
		t.Fatalf("a pass that could not read instances started %d teardown(s)", got)
	}
	f.destroyedOnly(t, []int64{a.ID})
	if logs := f.log.String(); !strings.Contains(logs, "agent-reap: live instances could not be read; destroying nothing this pass") {
		t.Errorf("the refusal is not logged:\n%s", logs)
	}
}

// stallProv is the destroy fixture's provisioner whose instance list blocks
// until its context ends (or, with forever, until the test ends), counting
// the calls.
type stallProv struct {
	*destroyProv
	calls   atomic.Int32
	stall   bool
	forever chan struct{}
}

func (p *stallProv) Instances(ctx context.Context) ([]provision.Instance, error) {
	p.calls.Add(1)
	if !p.stall {
		return nil, nil
	}
	if p.forever != nil {
		<-p.forever
		return nil, nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestAStalledInstanceReadEndsThePassAndDestroysNothing: a due agent, and a
// provisioner whose instance list does not answer — one that waits for its
// context, and one that ignores it. With the read bounded at 100ms, each pass
// returns within a few seconds, starts no teardown, and says why.
func TestAStalledInstanceReadEndsThePassAndDestroysNothing(t *testing.T) {
	for name, forever := range map[string]bool{"honours its context": false, "ignores its context": true} {
		t.Run(name, func(t *testing.T) {
			f := newReapAPIFixture(t)
			p := &stallProv{destroyProv: f.prov, stall: true}
			if forever {
				p.forever = make(chan struct{})
				t.Cleanup(func() { close(p.forever) })
			}
			f.srv.ext.Provisioner = p
			f.srv.reapInstancesWithin = 100 * time.Millisecond
			a := f.agentOn(t, 1, "rpstall", nil)
			f.stopAged(t, a.ID, 15)
			done := make(chan int, 1)
			go func() { done <- f.srv.reapStoppedAgents(authz.System(context.Background())) }()
			select {
			case n := <-done:
				if n != 0 {
					t.Fatalf("a pass whose instance read stalled started %d teardown(s)", n)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("a pass whose instance read stalled did not return")
			}
			if p.calls.Load() != 1 {
				t.Fatalf("instance reads = %d, want 1 (the stall was not exercised)", p.calls.Load())
			}
			if logs := f.log.String(); !strings.Contains(logs, "agent-reap: live instances could not be read; destroying nothing this pass") {
				t.Errorf("the refusal is not logged:\n%s", logs)
			}
		})
	}
}

// emptyReapStore is the agents store whose reaper scan finds nothing due.
type emptyReapStore struct{ *agents.PGStore }

func (emptyReapStore) ListReapableStopped(context.Context, time.Duration) ([]agents.StoppedAgent, error) {
	return nil, nil
}

// TestAPassWithNothingDueReadsNoInstances: the instance read is paid only when
// something is due. With nothing due, a pass makes no Instances call; CONTROL:
// with one agent due, it makes exactly one.
func TestAPassWithNothingDueReadsNoInstances(t *testing.T) {
	f := newReapAPIFixture(t)
	p := &stallProv{destroyProv: f.prov}
	f.srv.ext.Provisioner = p
	f.srv.ext.Agents = emptyReapStore{agents.NewPG(f.pool)}
	f.srv.reapStoppedAgents(authz.System(context.Background()))
	if got := p.calls.Load(); got != 0 {
		t.Fatalf("a pass with nothing due read the instances %d time(s)", got)
	}
	f.srv.ext.Agents = agents.NewPG(f.pool)
	a := f.agentOn(t, 1, "rpnothing", nil)
	f.stopAged(t, a.ID, 15)
	f.reap(t)
	if got := p.calls.Load(); got != 1 {
		t.Fatalf("control: a pass with one agent due read the instances %d time(s), want 1", got)
	}
}
