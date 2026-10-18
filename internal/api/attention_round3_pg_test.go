package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/notes"
)

// TestAHostBlipReNotifiesNothing: a waiting session seeded on the host's
// first observation, the host dropping, and coming back within the re-push
// window: no notification at all — neither a clear while it was gone nor a
// push when it returned. A session that starts waiting AFTER the reconnect is
// still pushed.
func TestAHostBlipReNotifiesNothing(t *testing.T) {
	f := newAttnFixture(t)
	clock := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	f.srv.now = func() time.Time { return clock }
	nudge := func() {
		f.srv.BroadcastAgentsChangedFor(1)
		f.srv.WaitForPushes()
	}
	setOnline := func(on bool) {
		f.mirror.mu.Lock()
		f.mirror.online = on
		f.mirror.mu.Unlock()
	}
	nudge() // first observation: @1 already waiting, seeded
	setOnline(false)
	nudge()
	clock = clock.Add(30 * time.Second)
	setOnline(true)
	nudge()
	nudge()
	if got := f.pushes(); len(got) != 0 {
		t.Fatalf("a blip notified: %v", got)
	}
	// A session that starts waiting after the reconnect is new: pushed.
	f.mirror.mu.Lock()
	f.mirror.windows = append(attnWindows(mirror.StatusWaiting),
		mirror.Window{Session: "side", ID: "@3", Index: 0, Active: true, Claude: true, ClaudeSession: "3e5f6071-8293-4a41-9dce-3f4a5b6c7d8e",
			Status: mirror.StatusWaiting, StatusSince: attnSince})
	f.mirror.mu.Unlock()
	nudge()
	if got := f.pushes(); len(got) != 1 || got[0] != "attention attention-"+sessionKey("ws", "side", "@3") {
		t.Fatalf("a session new after the reconnect: %v", got)
	}
}

// TestASeededSessionBackWithinTheWindowIsNotPushed: a session seeded on its
// host's first observation (never pushed) that stops waiting and waits again
// within the re-push window is the same episode — the seed stamped it.
func TestASeededSessionBackWithinTheWindowIsNotPushed(t *testing.T) {
	f := newAttnFixture(t)
	clock := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	f.srv.now = func() time.Time { return clock }
	nudge := func() {
		f.srv.BroadcastAgentsChangedFor(1)
		f.srv.WaitForPushes()
	}
	nudge() // seeded
	f.setWindows(mirror.StatusIdle)
	nudge()
	clock = clock.Add(time.Minute)
	f.setWindows(mirror.StatusWaiting)
	nudge()
	for _, p := range f.pushes() {
		if strings.HasPrefix(p, "attention ") {
			t.Fatalf("a seeded session back within the window was pushed: %v", f.pushes())
		}
	}
}

// TestAHostGoneLongerThanTheWindowReseedsSilently: a host the hub stops
// listing altogether, back after the re-push window has passed, is a new
// first observation: its still-waiting session is seeded, not pushed.
func TestAHostGoneLongerThanTheWindowReseedsSilently(t *testing.T) {
	f := newAttnFixture(t)
	clock := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	f.srv.now = func() time.Time { return clock }
	nudge := func() {
		f.srv.BroadcastAgentsChangedFor(1)
		f.srv.WaitForPushes()
	}
	lap := []mirror.HostView{{Host: "lap", Online: true, StatusReport: true, Windows: mirror.NormalizeWindows(attnWindows(mirror.StatusWaiting), 0)}}
	setOthers := func(o []mirror.HostView) {
		f.mirror.mu.Lock()
		f.mirror.others = o
		f.mirror.mu.Unlock()
	}
	nudge()
	setOthers(lap)
	nudge() // lap's first observation: seeded
	setOthers(nil)
	nudge() // gone from the list
	clock = clock.Add(attentionRepushAfter + time.Minute)
	setOthers(lap)
	nudge()
	for _, p := range f.pushes() {
		if strings.HasPrefix(p, "attention ") {
			t.Fatalf("a reconnecting host's waiting session was pushed: %v", f.pushes())
		}
	}
}

// TestARecomputeNeverCommitsAnOlderReadOverANewerOne: recompute A reads the
// hub (a window waiting) and is held before it commits; the window goes idle
// and recompute B runs. A must not commit its older read after B — the queue
// ends with nothing waiting.
func TestARecomputeNeverCommitsAnOlderReadOverANewerOne(t *testing.T) {
	f := newAttnFixture(t)
	f.srv.BroadcastAgentsChangedFor(1) // seed the host
	f.srv.WaitForPushes()
	// Only the FIRST caller is held. (Not a sync.Once: Once.Do makes a second
	// caller wait for the first to return, so B could never commit first and
	// the test would pass with the serialisation removed.)
	var first atomic.Bool
	read, release := make(chan struct{}), make(chan struct{})
	attentionDiffHook = func() {
		if first.CompareAndSwap(false, true) {
			close(read)
			<-release
		}
	}
	t.Cleanup(func() { attentionDiffHook = nil })
	f.setWindows(mirror.StatusWaiting)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); f.srv.attentionMirrorChanged(1) }() // A
	<-read
	f.setWindows(mirror.StatusIdle)
	go func() { defer wg.Done(); f.srv.attentionMirrorChanged(1) }() // B
	time.Sleep(150 * time.Millisecond)
	close(release)
	wg.Wait()
	f.srv.attention.mu.Lock()
	n := len(f.srv.attention.waiting[1])
	f.srv.attention.mu.Unlock()
	if n != 0 {
		t.Fatalf("the queue holds %d waiting session(s) after the window went idle: an older read was committed last", n)
	}
}

// TestAnAgentCallerOfTheTaskStatusAPIObeysTheAgentRule: PATCH
// /api/tasks/{id}/status with the service credential asserting an AGENT's
// name (the chief's way in) can neither complete a task nor reopen a completed
// one; the operator's hook token can do both.
func TestAnAgentCallerOfTheTaskStatusAPIObeysTheAgentRule(t *testing.T) {
	f := newAttnFixture(t)
	n, err := f.notes.Create(asUser(1), notes.Note{Title: "api status", Body: "api status", Status: notes.StatusOpen})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM notes WHERE id = $1`, n.ID)
	})
	patch := func(hdr map[string]string, status string) *httptest.ResponseRecorder {
		h := map[string]string{"Content-Type": "application/json"}
		for k, v := range hdr {
			h[k] = v
		}
		return f.call(t, http.MethodPatch, "/api/tasks/"+strconv.FormatInt(n.ID, 10)+"/status", strings.NewReader(`{"status":"`+status+`"}`), h)
	}
	agent := map[string]string{"Authorization": "Bearer " + connTestServiceToken, ActorHeader: f.op.Name}
	hook := map[string]string{"Authorization": "Bearer " + connTestHookToken}
	if w := patch(agent, notes.StatusComplete); w.Code != http.StatusForbidden {
		t.Fatalf("an agent completing: %d %s", w.Code, w.Body)
	}
	if w := patch(hook, notes.StatusComplete); w.Code != http.StatusOK {
		t.Fatalf("the operator completing: %d %s", w.Code, w.Body)
	}
	if w := patch(agent, notes.StatusInProgress); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "completed task") {
		t.Fatalf("an agent reopening: %d %s", w.Code, w.Body)
	}
	if got, _ := f.notes.Get(asUser(1), n.ID); got.Status != notes.StatusComplete {
		t.Fatalf("the agent reopened it: %s", got.Status)
	}
	// A service caller that is NOT an agent is unchanged.
	if w := patch(map[string]string{"Authorization": "Bearer " + connTestServiceToken, ActorHeader: "ci-bot"}, notes.StatusInProgress); w.Code != http.StatusOK {
		t.Fatalf("a non-agent service reopening: %d %s", w.Code, w.Body)
	}
	if w := patch(hook, notes.StatusComplete); w.Code != http.StatusOK {
		t.Fatalf("the operator completing again: %d %s", w.Code, w.Body)
	}
	if w := patch(hook, notes.StatusOpen); w.Code != http.StatusOK {
		t.Fatalf("the operator reopening: %d %s", w.Code, w.Body)
	}
}

// staleRead is the real notes store whose reads answer an OLDER status: the
// operator completed the task between an agent's read and its write.
type staleRead struct{ notes.Store }

func (s staleRead) Get(ctx context.Context, id int64) (notes.Note, error) {
	n, err := s.Store.Get(ctx, id)
	n.Status = notes.StatusInProgress
	return n, err
}

// TestAnAgentsStatusWriteRacingTheOperatorsCompleteNeverReopens: each agent
// write path reads the task, then writes; the operator completes it in
// between (a stale read). The write is refused by the row itself, on all three
// paths — the tasks tool (MCP and gateway), agent_set_task_status, and PATCH
// /agent/task/status — and the task stays complete.
func TestAnAgentsStatusWriteRacingTheOperatorsCompleteNeverReopens(t *testing.T) {
	f := newAttnFixture(t)
	task, err := f.notes.Create(asUser(f.member), notes.Note{Title: "attn race", Body: "raced", Status: notes.StatusOpen})
	if err != nil {
		t.Fatal(err)
	}
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	tok := "attn-race-token-" + suffix
	worker, err := agents.NewPG(f.pool).Create(asUser(f.member), agents.Agent{Name: "attnrace-" + suffix, Namespace: "devpod-attnrace-" + suffix,
		Status: agents.StatusRunning, HooksToken: tok, NoteID: &task.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.notes.SetStatus(asUser(f.member), task.ID, notes.StatusComplete); err != nil {
		t.Fatal(err)
	}
	prev := f.srv.ext.Notes
	f.srv.ext.Notes = staleRead{f.notes}
	t.Cleanup(func() { f.srv.ext.Notes = prev })
	still := func(path, got string) {
		t.Helper()
		if n, _ := f.notes.Get(asUser(f.member), task.ID); n.Status != notes.StatusComplete {
			t.Fatalf("%s reopened a task completed after its read (%s): %s", path, n.Status, got)
		}
		if !strings.Contains(got, "completed task") {
			t.Fatalf("%s did not say why: %s", path, got)
		}
	}
	args := map[string]any{"id": task.ID, "status": notes.StatusInProgress}
	text, _, rpc := f.tool(t, tok, taskSetStatusTool, args)
	if rpc != nil {
		text = "rpc " + rpc.Message
	}
	still("the tasks tool (MCP)", text)
	gateway := f.srv.AgentToolDispatch(asUser(f.member), worker)
	raw, _ := json.Marshal(args)
	still("the tasks tool (gateway)", gateway(taskSetStatusTool, string(raw)))
	still("agent_set_task_status", gateway("agent_set_task_status", `{"status":"in_progress"}`))
	w := f.call(t, http.MethodPatch, "/agent/task/status", strings.NewReader(`{"status":"in_progress"}`),
		map[string]string{"Content-Type": "application/json", "X-Muster-Token": tok})
	if w.Code != http.StatusForbidden {
		t.Fatalf("PATCH /agent/task/status: %d %s", w.Code, w.Body)
	}
	still("PATCH /agent/task/status", w.Body.String())
}

// listedAgents lists fixed agents, or fails.
type listedAgents struct {
	agents.Store
	list []agents.Agent
	err  error
}

func (l listedAgents) List(context.Context) ([]agents.Agent, error) { return l.list, l.err }

// TestActorIsAgentNamesTheChiefEitherWayAndFailsClosed: an asserted name is an
// agent's when it is an agent's slug or its display name (case and spacing
// aside); any other name is not; a list that cannot be read counts as an
// agent, the stricter rule.
func TestActorIsAgentNamesTheChiefEitherWayAndFailsClosed(t *testing.T) {
	s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Agents: listedAgents{list: []agents.Agent{{Name: "chief", DisplayName: "Chief of Staff"}}}, Notes: stubNotesStore{}, SessionLiveness: stubLiveness{}})
	for actor, want := range map[string]bool{"chief": true, "CHIEF": true, " chief of staff ": true, "ci-bot": false, "": false} {
		if got := s.actorIsAgent(context.Background(), actor); got != want {
			t.Errorf("actorIsAgent(%q) = %v, want %v (the slug or the display name)", actor, got, want)
		}
	}
	s.UseExtensions(Extensions{Agents: listedAgents{err: errors.New("gone")}, Notes: stubNotesStore{}, SessionLiveness: stubLiveness{}})
	if !s.actorIsAgent(context.Background(), "ci-bot") {
		t.Error("a list that cannot be read answered not-an-agent: the agent rule must fail closed")
	}
}

// statusReadFails is a notes store whose reads fail and whose status writes
// succeed and are counted.
type statusReadFails struct {
	stubNotesStore
	writes *int
}

func (statusReadFails) Get(context.Context, int64) (notes.Note, error) {
	return notes.Note{}, errors.New("the database is gone")
}

func (s statusReadFails) SetStatus(_ context.Context, id int64, status string) (notes.Note, error) {
	*s.writes++
	return notes.Note{ID: id, Status: status}, nil
}

// boundAgents resolves one token to an agent bound to task 42.
type boundAgents struct{ agents.Store }

func (boundAgents) GetByHooksToken(_ context.Context, tok string) (agents.Agent, error) {
	if tok == "bound-agent-token" {
		id := int64(42)
		return agents.Agent{ID: 9, Name: "bound", OwnerID: 1, NoteID: &id}, nil
	}
	return agents.Agent{}, errors.New("no")
}

// TestTheAgentStatusRouteFailsClosedWhenItCannotRead: a status write whose
// task cannot be read is not made.
func TestTheAgentStatusRouteFailsClosedWhenItCannotRead(t *testing.T) {
	s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	writes := 0
	s.UseExtensions(Extensions{Agents: boundAgents{}, Notes: statusReadFails{writes: &writes}, SessionLiveness: stubLiveness{}})
	r := httptest.NewRequest(http.MethodPatch, "/agent/task/status", strings.NewReader(`{"status":"in_progress"}`))
	r.Header.Set("X-Muster-Token", "bound-agent-token")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if writes != 0 {
		t.Fatalf("the status was written (%d) although the task could not be read: %d %s", writes, w.Code, w.Body)
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}
