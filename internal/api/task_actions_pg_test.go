package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/notes"
)

// TestTheActionsOfferedOnATaskAreExactlyTheToolsThatWouldAct pins the
// RELATIONSHIP, not a component: across who works the task (nobody, a running,
// a pending or a stopped agent), the task's own state (open, gated, in
// progress) and the owner's dispatch opt-in (off, on for someone else, on for
// this owner), the actions task_route_attention offers the owner are exactly
// the tools that then ACT when the agent calls them — task_forward forwards,
// task_dispatch starts an agent (any refusal at all counts as not acting).
// route_to_session is never offered (a task event is no_reply). The ask's
// buttons are the offered actions, in order.
func TestTheActionsOfferedOnATaskAreExactlyTheToolsThatWouldAct(t *testing.T) {
	type tally struct{ offered, withheld int }
	seen := map[string]*tally{taskForwardTool: {}, taskDispatchTool: {}}
	type taskCase struct {
		name string
		tags []string
		// status, when set, is the task's status before the ask.
		status string
	}
	// executed counts the subtests that ran PAST the fixture's skip: without a
	// database every subtest skips inside newTaskRouteFixture before doing
	// anything, and the vacuity check below must not then fire — the comparison
	// is not vacuous, it merely never ran. Only a database-less run leaves
	// executed at 0; with one, every subtest reaches it.
	executed := 0
	for _, dc := range []struct {
		name            string
		dispatch, optIn bool
	}{{"dispatch-off", false, false}, {"dispatch-on-for-another", true, false}, {"dispatch-on-for-this-owner", true, true}} {
		for _, worker := range []string{"", agents.StatusRunning, agents.StatusPending, agents.StatusStopped} {
			for _, tc := range []taskCase{{name: "open"}, {name: "gated", tags: []string{"gate:needs-owner-ok"}}, {name: "in-progress", status: notes.StatusInProgress}} {
				if tc.name != "open" && worker != "" {
					continue // the task's own state is varied with nobody on it
				}
				t.Run(dc.name+"/worker-"+firstNonBlank(worker, "none")+"/task-"+tc.name, func(t *testing.T) {
					f := newTaskRouteFixture(t, dc.dispatch, dc.optIn)
					executed++
					f.srv.taskForwardTurn = func(context.Context, agents.Agent, agents.ChatSession, string) {}
					n := f.task(t, "who takes it", tc.tags...)
					if tc.status != "" {
						if _, err := f.notes.SetStatus(f.asOwner, n.ID, tc.status); err != nil {
							t.Fatal(err)
						}
					}
					if worker != "" {
						if _, err := agents.NewPG(f.pool).Create(f.asOwner, agents.Agent{Name: "tact-" + dc.name + "-" + worker,
							Namespace: "ns-tact", Status: worker, NoteID: &n.ID}); err != nil {
							t.Fatal(err)
						}
					}
					res := f.call(f.agent, taskRouteAttentionTool, map[string]any{"task_id": n.ID, "note": "Who should take it?"})
					var out struct {
						ID      int64    `json:"id"`
						Offered []string `json:"actions_offered"`
					}
					if err := json.Unmarshal([]byte(res), &out); err != nil || out.ID <= 0 || out.Offered == nil {
						t.Fatalf("task_route_attention answered %s", res)
					}
					ask, err := f.srv.ext.Attention.Get(f.asOwner, out.ID)
					if err != nil {
						t.Fatal(err)
					}
					if len(ask.Options) != len(out.Offered) {
						t.Fatalf("buttons %q for actions %q", ask.Options, out.Offered)
					}
					offered := map[string]bool{}
					for i, tool := range out.Offered {
						offered[tool] = true
						if !strings.HasPrefix(ask.Options[i], tool+": ") {
							t.Errorf("button %d %q does not name action %s", i, ask.Options[i], tool)
						}
					}
					if offered[routeToSessionToolName] {
						t.Error("route_to_session was offered for a task event, which it refuses (no_reply)")
					}
					fwd := f.call(f.agent, taskForwardTool, map[string]any{"task_id": n.ID, "note": "over to you"})
					forwarded := !strings.Contains(fwd, `"error"`)
					disp := f.call(f.agent, taskDispatchTool, map[string]any{"task_id": n.ID})
					started := !strings.Contains(disp, `"error"`) && strings.Contains(disp, `"agent_id"`)
					if offered[taskForwardTool] != forwarded {
						t.Errorf("task_forward offered=%v, but the tool forwarded=%v: %s", offered[taskForwardTool], forwarded, fwd)
					}
					if offered[taskDispatchTool] != started {
						t.Errorf("task_dispatch offered=%v, but the tool started an agent=%v: %s", offered[taskDispatchTool], started, disp)
					}
					for tool, tl := range seen {
						if offered[tool] {
							tl.offered++
						} else {
							tl.withheld++
						}
					}
					if len(out.Offered) == 0 && !strings.Contains(ask.Question, "it is yours to handle") {
						t.Errorf("an ask offering nothing does not say so: %q", ask.Question)
					}
				})
			}
		}
	}
	// The comparison is not vacuous: each action was both offered and withheld.
	// Only enforce it when at least one subtest executed — a database-less run
	// skipped them all, and a tally that never ran proves nothing.
	if executed > 0 {
		for tool, tl := range seen {
			if tl.offered == 0 || tl.withheld == 0 {
				t.Errorf("%s was offered %d and withheld %d times across the matrix: the comparison cannot see one side", tool, tl.offered, tl.withheld)
			}
		}
	}
}

// TestATaskWithALiveAgentOffersForwardNotDispatch is the audit's case, named:
// the owner opted in, and a running agent works the task — forwarding is the
// action; a dispatch would be refused (an agent is on it), so it is not offered.
func TestATaskWithALiveAgentOffersForwardNotDispatch(t *testing.T) {
	f := newTaskRouteFixture(t, true, true)
	n := f.task(t, "in flight")
	if _, err := agents.NewPG(f.pool).Create(f.asOwner, agents.Agent{Name: "tact-live", Namespace: "ns-tact", Status: agents.StatusRunning, NoteID: &n.ID}); err != nil {
		t.Fatal(err)
	}
	res := f.call(f.agent, taskRouteAttentionTool, map[string]any{"task_id": n.ID, "note": "?"})
	var out struct {
		Offered []string `json:"actions_offered"`
	}
	if err := json.Unmarshal([]byte(res), &out); err != nil {
		t.Fatal(res)
	}
	if strings.Join(out.Offered, ",") != taskForwardTool {
		t.Fatalf("offered %q; want only %s", out.Offered, taskForwardTool)
	}
}
