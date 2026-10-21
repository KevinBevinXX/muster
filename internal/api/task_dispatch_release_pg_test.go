package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/github"
	"github.com/ZacxDev/muster/internal/notes"
)

// refusedThenRetried drives one post-reservation refusal of task n through
// task_dispatch: the call is refused, no agent exists, the one start is NOT
// left spent; then, the cause fixed by fix, the next call starts an agent and
// the one after answers already_dispatched (the release freed exactly one).
func refusedThenRetried(t *testing.T, f *taskRouteFixture, n notes.Note, want string, fix func()) {
	t.Helper()
	text := f.call(f.chief, taskDispatchTool, map[string]any{"task_id": n.ID})
	if !strings.Contains(text, `"error"`) || !strings.Contains(text, "could not be started") || !strings.Contains(text, want) {
		t.Fatalf("the refused start: %s; want a refusal naming %q", text, want)
	}
	if got := f.agentsOn(t, n.ID); len(got) != 0 {
		t.Fatalf("a refused start created %d agent(s)", len(got))
	}
	if r := f.reservations(t, n.ID); r != 1 {
		t.Fatalf("reservations after the refusal: %d, want 1 (the unit is spent)", r)
	}
	fix()
	text = f.call(f.chief, taskDispatchTool, map[string]any{"task_id": n.ID})
	if strings.Contains(text, `"error"`) || strings.Contains(text, "already_dispatched") || !strings.Contains(text, `"agent_id"`) {
		t.Fatalf("the call after the refusal: %s; want a started agent (the refused start released)", text)
	}
	if got := f.agentsOn(t, n.ID); len(got) != 1 {
		t.Fatalf("agents on the task after the retry: %d, want 1", len(got))
	}
	if again := f.call(f.chief, taskDispatchTool, map[string]any{"task_id": n.ID}); !strings.Contains(again, `"already_dispatched":true`) {
		t.Fatalf("a third call: %s", again)
	}
}

// TestAnUncredentialedModelRefusedAfterTheReservationReleasesTheStart (#209
// audit 🟡3): the model check runs in createAndDispatchAgent, after the
// reservation and before any agent row.
func TestAnUncredentialedModelRefusedAfterTheReservationReleasesTheStart(t *testing.T) {
	f := newTaskRouteFixture(t, true, true)
	n := f.task(t, "uncredentialed model")
	if _, err := f.pool.Exec(f.asOwner, `UPDATE notes SET model = 'anthropic/example-model' WHERE id = $1`, n.ID); err != nil {
		t.Fatal(err)
	}
	refusedThenRetried(t, f, n, "", func() {
		if _, err := f.pool.Exec(f.asOwner, `UPDATE notes SET model = '' WHERE id = $1`, n.ID); err != nil {
			t.Fatal(err)
		}
	})
}

// TestAnExhaustedAccountPoolRefusedAfterTheReservationReleasesTheStart: the
// claude-code account is picked (SelectClaudeAccount) after the reservation.
func TestAnExhaustedAccountPoolRefusedAfterTheReservationReleasesTheStart(t *testing.T) {
	f := newTaskRouteFixture(t, true, true, withKinds("pool-acct-a"))
	f.srv.ext.Kinds = stubKinds{err: errors.New("every pool account is at its session limit")}
	n := f.task(t, "pool exhausted")
	refusedThenRetried(t, f, n, "every pool account is at its session limit", func() {
		f.srv.ext.Kinds = stubKinds{pick: "pool-acct-a"}
	})
}

// TestARepositoryDisconnectedAfterTheReservationReleasesTheStart: the
// connected-repository check runs before the reservation AND again in
// createAndDispatchAgent; a repository that leaves the owner's list in
// between is refused there.
func TestARepositoryDisconnectedAfterTheReservationReleasesTheStart(t *testing.T) {
	f := newTaskRouteFixture(t, true, true, withKinds("pool-acct-a"))
	n, err := f.notes.Create(f.asOwner, notes.Note{Title: "repo goes away", Body: "b", Repo: "example/app"})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	dropAfterReserve := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		drop := dropAfterReserve
		mu.Unlock()
		if r.URL.Path != "/user/repos" || r.URL.Query().Get("page") != "1" || (drop && f.reservations(t, n.ID) > 0) {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_, _ = w.Write([]byte(`[{"full_name":"example/app"}]`))
	}))
	t.Cleanup(srv.Close)
	github.UseAPIBaseURL(srv.URL)
	t.Cleanup(func() { github.UseAPIBaseURL("https://api.github.com") })
	f.srv.ext.GitHub = fakeGitHubConn{}
	refusedThenRetried(t, f, n, chiefDispatchRepo, func() {
		mu.Lock()
		dropAfterReserve = false
		mu.Unlock()
	})
}

// TestAForwardToARunningAgentWhoseFirstTurnIsOwedRidesThatTurn (quiet-stoat):
// a claude-code agent is RUNNING while its first turn is held (its repository
// is still being cloned; the turn is unstamped). A forward sent then as a turn
// of its own reached a runtime that answered not_ready, and nothing re-sent
// it. It is appended to the owed first turn instead — no turn of its own —
// and once that turn is stamped, a forward is a turn again.
func TestAForwardToARunningAgentWhoseFirstTurnIsOwedRidesThatTurn(t *testing.T) {
	f := newTaskRouteFixture(t, false, false)
	n := f.task(t, "held first turn")
	store := agents.NewPG(f.pool)
	const first = "Work the held task."
	a, err := store.Create(f.asOwner, agents.Agent{Name: "trheld-" + strconv.FormatInt(f.owner, 10),
		Namespace: "ns-trheld", Status: agents.StatusRunning, NoteID: &n.ID, PendingNote: first})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var turns []string
	f.srv.taskForwardTurn = func(_ context.Context, x agents.Agent, _ agents.ChatSession, text string) {
		mu.Lock()
		defer mu.Unlock()
		turns = append(turns, text)
		f.srv.turns.leave(x.ID)
	}
	turnCount := func() int { mu.Lock(); defer mu.Unlock(); return len(turns) }

	text := f.call(f.chief, taskForwardTool, map[string]any{"task_id": n.ID, "note": "Scope changed upstream"})
	if strings.Contains(text, `"error"`) || !strings.Contains(text, `"turn_started":false`) {
		t.Fatalf("the forward: %s", text)
	}
	cur, err := store.Get(f.asOwner, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cur.PendingNote, first) || !strings.Contains(cur.PendingNote, "Scope changed upstream") || turnCount() != 0 {
		t.Fatalf("first turn %q, %d turn(s); want the forward appended to the held first turn and no turn", cur.PendingNote, turnCount())
	}

	// Stamped: the next forward is a turn.
	if _, err := f.pool.Exec(f.asOwner, `UPDATE agents SET kicked_off = true WHERE id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	if text := f.call(f.chief, taskForwardTool, map[string]any{"task_id": n.ID, "note": "And again"}); !strings.Contains(text, `"turn_started":true`) {
		t.Fatalf("a forward after the stamp: %s", text)
	}
	if turnCount() != 1 {
		t.Fatalf("turns after the stamp: %d, want 1", turnCount())
	}
}

// TestForwardsFoldedIntoAHeldFirstTurnKeepTheRulesLastAndStayBounded (#209
// audit 🟢6): a forward folded into a chief-started agent's owed first turn
// goes BEFORE muster's rules, which stay the message's last word; and past the
// size bound a forward is not added — the turn says so, once — so the first
// turn stays within what ccd reads (1 MiB).
func TestForwardsFoldedIntoAHeldFirstTurnKeepTheRulesLastAndStayBounded(t *testing.T) {
	f := newTaskRouteFixture(t, false, false)
	n := f.task(t, "held, rules last")
	store := agents.NewPG(f.pool)
	first := "Work the held task." + chiefDispatchRules
	a, err := store.Create(f.asOwner, agents.Agent{Name: "trrules-" + strconv.FormatInt(f.owner, 10),
		Namespace: "ns-trrules", Status: agents.StatusRunning, NoteID: &n.ID, PendingNote: first})
	if err != nil {
		t.Fatal(err)
	}
	f.srv.taskForwardTurn = func(_ context.Context, x agents.Agent, _ agents.ChatSession, _ string) { f.srv.turns.leave(x.ID) }
	note := func() string {
		cur, err := store.Get(f.asOwner, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		return cur.PendingNote
	}
	if text := f.call(f.chief, taskForwardTool, map[string]any{"task_id": n.ID, "note": "First change upstream"}); !strings.Contains(text, `"turn_started":false`) {
		t.Fatalf("the forward: %s", text)
	}
	got := note()
	if !strings.HasSuffix(got, chiefDispatchRules) || !strings.Contains(got, "First change upstream") ||
		strings.Index(got, "First change upstream") > strings.Index(got, chiefDispatchRules) {
		t.Fatalf("first turn %q: want the forward folded in BEFORE muster's rules, which stay last", got)
	}

	saved := foldedForwardsMaxBytes
	t.Cleanup(func() { foldedForwardsMaxBytes = saved })
	foldedForwardsMaxBytes = len(got) + 200 // room for the marker, not for a forward
	for _, c := range []string{"Second change " + strings.Repeat("x", 300), "Third change " + strings.Repeat("y", 300)} {
		if text := f.call(f.chief, taskForwardTool, map[string]any{"task_id": n.ID, "note": c}); strings.Contains(text, `"error"`) {
			t.Fatalf("a forward past the bound: %s", text)
		}
	}
	got = note()
	if strings.Contains(got, "Second change") || strings.Contains(got, "Third change") {
		t.Fatalf("a forward past the bound was added: %d bytes", len(got))
	}
	if strings.Count(got, "were not added here") != 1 || !strings.HasSuffix(got, chiefDispatchRules) {
		t.Fatalf("first turn %q: want the size-limit line once, and muster's rules still last", got)
	}
}
