package api

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plugins"
)

// THE AGENT'S DISPATCH BUDGET AS DATA. The agent's conversation survives
// restarts, so a cap number in task_dispatch's description is a number it
// remembers after the cap changed. These pin that the description states none,
// that every task_dispatch answer and refusal past the opt-in carries the
// current budget, and that task_route_attention stops offering task_dispatch
// when the budget is full.
//
// Fixture counts are pairwise distinct and distinct from the caps (5 live, 10 a
// day), so a budget that reported a cap as a count, or one count as the other,
// cannot pass.

// TestTaskDispatchDescriptionCarriesNoCapNumbers pins task_dispatch's WHOLE
// description (normalised whitespace, exact equality — a reworded description
// fails here, on purpose), and that nothing in it reads "at most <digit>".
func TestTaskDispatchDescriptionCarriesNoCapNumbers(t *testing.T) {
	const want = "Start a new agent on one of your owner's open, ungated tasks. Only with your owner's opt-in. " +
		"Limits apply to how many of the agents you start may be live at once and to how many you may start in a day: every result " +
		"and every refusal of this tool carries your current budget (live, live_cap, today, daily_cap, live_agents), and so does " +
		"task_route_attention's result — read the limits from the latest budget, never from memory. " +
		"Once per task (calling it again answers with that start), until your owner destroys the agent you started there. " +
		"The agent opens a DRAFT pull request at most and never marks the task complete. " +
		"It starts a claude-code agent (a Claude Code pod) unless you pass kind \"gateway\" or this deployment has no " +
		"claude-code. Either kind works in the task's repository, or the one you pass: a claude-code agent's session " +
		"starts inside a checkout of it, so the repository's CLAUDE.md and project skills (.claude/skills) load."
	norm := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	var got string
	found := 0
	for _, d := range taskRoutingToolDefs(true) {
		if d.Name == taskDispatchTool {
			got, found = d.Description, found+1
		}
	}
	if found != 1 {
		t.Fatalf("task_dispatch defined %d times", found)
	}
	if norm(got) != norm(want) {
		t.Errorf("task_dispatch's description\n got %q\nwant %q", norm(got), norm(want))
	}
	if m := regexp.MustCompile(`(?i)at most \d`).FindString(got); m != "" {
		t.Errorf("task_dispatch's description states a cap number (%q): the agent would remember it past a change", m)
	}
}

// liveFleetAgent creates a running agent on a fresh task as if the agent had
// started it (reservation + recorded agent), returning it.
func (f *taskRouteFixture) liveFleetAgent(t *testing.T, label string) agents.Agent {
	t.Helper()
	seed := f.task(t, label)
	if _, err := f.plugins.ReserveTaskDispatch(f.asOwner, seed.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	a, err := agents.NewPG(f.pool).Create(f.asOwner, agents.Agent{Name: "trbud-" + label + "-" + strconv.FormatInt(f.owner, 10),
		Namespace: "ns-trbud", Status: agents.StatusRunning, NoteID: &seed.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.plugins.RecordTaskDispatchAgent(f.asOwner, seed.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	return a
}

// finishedStarts writes n agent starts on task ids from base whose agents are
// gone (agent 0 names no row): they count today, never live.
func (f *taskRouteFixture) finishedStarts(t *testing.T, base int64, n int) {
	t.Helper()
	for i := int64(0); i < int64(n); i++ {
		if _, err := f.plugins.ReserveTaskDispatch(f.asOwner, base+i, nil, nil); err != nil {
			t.Fatalf("seed start %d: %v", base+i, err)
		}
		if err := f.plugins.RecordTaskDispatchAgent(f.asOwner, base+i, 0); err != nil {
			t.Fatal(err)
		}
	}
}

// answerBudget decodes text's `budget`, failing when it has none.
func answerBudget(t *testing.T, text string) (map[string]json.RawMessage, plugins.DispatchBudget) {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		t.Fatalf("answer is not an object: %s", text)
	}
	raw, ok := m["budget"]
	if !ok {
		t.Fatalf("answer carries no budget: %s", text)
	}
	var b plugins.DispatchBudget
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatalf("budget %s: %v", raw, err)
	}
	return m, b
}

func i64(v int64) *int64   { return &v }
func str(v string) *string { return &v }

// TestTaskDispatchStartedAndAlreadyDispatchedCarryTheBudget: a start answers
// with the budget AFTER it (naming the agent it made), its repeat
// (already_dispatched) with the same budget, both through the real /mcp door
// and not flagged as errors. The budget lists the recorded live agents by id
// and name, then a start whose agent is not recorded with id and name null.
func TestTaskDispatchStartedAndAlreadyDispatchedCarryTheBudget(t *testing.T) {
	f := newTaskRouteFixture(t, true, true)
	prior := f.liveFleetAgent(t, "prior") // live, today
	pendingTask := int64(900100)          // reserved, no agent yet: live, today
	if _, err := f.plugins.ReserveTaskDispatch(f.asOwner, pendingTask, nil, nil); err != nil {
		t.Fatal(err)
	}
	f.finishedStarts(t, 900200, 1) // today only
	// Before: live 2, today 3.
	n := f.task(t, "budgeted")
	text, isErr, rpcErr := f.mcpCall(t, f.chiefTok, taskDispatchTool, map[string]any{"task_id": n.ID})
	if rpcErr != nil || isErr {
		t.Fatalf("start: rpc=%+v isError=%v %s", rpcErr, isErr, text)
	}
	m, b := answerBudget(t, text)
	var started struct {
		AgentID int64  `json:"agent_id"`
		Agent   string `json:"agent"`
	}
	if err := json.Unmarshal([]byte(text), &started); err != nil || started.AgentID <= 0 {
		t.Fatalf("start answered %s", text)
	}
	if _, ok := m["error"]; ok {
		t.Fatalf("a start answered with an error: %s", text)
	}
	want := plugins.DispatchBudget{Live: 3, LiveCap: plugins.MaxLiveDispatchedAgents, Today: 4, DailyCap: plugins.MaxDispatchesPerDay,
		LiveAgents: []plugins.LiveDispatchedAgent{
			{ID: i64(prior.ID), Name: str(prior.Name), Task: *prior.NoteID},
			{ID: i64(started.AgentID), Name: str(started.Agent), Task: n.ID},
			{Task: pendingTask},
		}}
	if !reflect.DeepEqual(b, want) {
		t.Fatalf("started budget\n got %+v\nwant %+v\n(%s)", b, want, text)
	}
	if !strings.Contains(text, `{"id":null,"name":null,"task":`+strconv.FormatInt(pendingTask, 10)+`}`) {
		t.Errorf("a start with no agent yet is not spelled id/name null: %s", text)
	}

	again, isErr, rpcErr := f.mcpCall(t, f.chiefTok, taskDispatchTool, map[string]any{"task_id": n.ID})
	if rpcErr != nil || isErr || !strings.Contains(again, `"already_dispatched":true`) {
		t.Fatalf("repeat: rpc=%+v isError=%v %s", rpcErr, isErr, again)
	}
	if _, b2 := answerBudget(t, again); !reflect.DeepEqual(b2, want) {
		t.Fatalf("already_dispatched budget\n got %+v\nwant %+v", b2, want)
	}
}

// TestTaskDispatchCapRefusalsCarryTheBudget: the live-cap and the daily-cap
// refusals each carry the budget that refused them — full on that cap — and are
// still flagged as errors over /mcp ({error, budget} is a failed call).
func TestTaskDispatchCapRefusalsCarryTheBudget(t *testing.T) {
	t.Run("live-cap", func(t *testing.T) {
		f := newTaskRouteFixture(t, true, true)
		f.finishedStarts(t, 900300, 2) // first: once the live cap is full, no start passes
		var live []agents.Agent
		for i := 0; i < plugins.MaxLiveDispatchedAgents; i++ {
			live = append(live, f.liveFleetAgent(t, "cap"+strconv.Itoa(i)))
		}
		// live 5 (the cap), today 7.
		n := f.task(t, "one too many")
		text, isErr, rpcErr := f.mcpCall(t, f.chiefTok, taskDispatchTool, map[string]any{"task_id": n.ID})
		if rpcErr != nil || !isErr {
			t.Fatalf("over the live cap: rpc=%+v isError=%v %s; want a failed call", rpcErr, isErr, text)
		}
		m, b := answerBudget(t, text)
		var msg string
		_ = json.Unmarshal(m["error"], &msg)
		if msg != taskDispatchLiveCap {
			t.Fatalf("refusal %q; want the live cap's %q", msg, taskDispatchLiveCap)
		}
		if b.Live != plugins.MaxLiveDispatchedAgents || b.Today != plugins.MaxLiveDispatchedAgents+2 || b.LiveCap != plugins.MaxLiveDispatchedAgents ||
			b.DailyCap != plugins.MaxDispatchesPerDay || len(b.LiveAgents) != len(live) {
			t.Fatalf("live-cap budget %+v", b)
		}
		for i, a := range live {
			if got := b.LiveAgents[i]; got.ID == nil || *got.ID != a.ID || got.Name == nil || *got.Name != a.Name || got.Task != *a.NoteID {
				t.Errorf("live_agents[%d] = %+v; want agent %d %s on task %d", i, got, a.ID, a.Name, *a.NoteID)
			}
		}
		if f.reservations(t, n.ID) != 0 || len(f.agentsOn(t, n.ID)) != 0 {
			t.Error("a refused start reserved or created something")
		}
	})
	t.Run("daily-cap", func(t *testing.T) {
		f := newTaskRouteFixture(t, true, true)
		a := f.liveFleetAgent(t, "day")
		f.finishedStarts(t, 900400, plugins.MaxDispatchesPerDay-1)
		// live 1, today 10 (the cap).
		n := f.task(t, "eleventh")
		text, isErr, rpcErr := f.mcpCall(t, f.chiefTok, taskDispatchTool, map[string]any{"task_id": n.ID})
		if rpcErr != nil || !isErr {
			t.Fatalf("over the daily cap: rpc=%+v isError=%v %s; want a failed call", rpcErr, isErr, text)
		}
		m, b := answerBudget(t, text)
		var msg string
		_ = json.Unmarshal(m["error"], &msg)
		if msg != taskDispatchDailyCap {
			t.Fatalf("refusal %q; want the daily cap's %q", msg, taskDispatchDailyCap)
		}
		want := plugins.DispatchBudget{Live: 1, LiveCap: plugins.MaxLiveDispatchedAgents, Today: plugins.MaxDispatchesPerDay,
			DailyCap: plugins.MaxDispatchesPerDay, LiveAgents: []plugins.LiveDispatchedAgent{{ID: i64(a.ID), Name: str(a.Name), Task: *a.NoteID}}}
		if !reflect.DeepEqual(b, want) {
			t.Fatalf("daily-cap budget\n got %+v\nwant %+v", b, want)
		}
	})
}

// TestTaskDispatchOtherRefusalsCarryTheBudgetPastTheOptIn: a refusal decided
// after the opt-in (here a gated task) carries the budget; the opt-out refusal,
// before any budget the agent could spend, carries none.
func TestTaskDispatchOtherRefusalsCarryTheBudgetPastTheOptIn(t *testing.T) {
	f := newTaskRouteFixture(t, true, true)
	f.liveFleetAgent(t, "g1")
	f.liveFleetAgent(t, "g2")
	f.finishedStarts(t, 900500, 1)
	n := f.task(t, "gated", "gate:needs-owner-ok")
	text := f.call(f.agent, taskDispatchTool, map[string]any{"task_id": n.ID})
	if !strings.Contains(text, taskDispatchGatedText) {
		t.Fatalf("answer %s", text)
	}
	if _, b := answerBudget(t, text); b.Live != 2 || b.Today != 3 {
		t.Fatalf("gated refusal's budget %+v; want live 2, today 3", b)
	}
	if !isToolErr(text) {
		t.Errorf("{error, budget} is not read as a failed call: %s", text)
	}

	out := newTaskRouteFixture(t, true, false)
	m := out.task(t, "not opted in")
	text = out.call(out.agent, taskDispatchTool, map[string]any{"task_id": m.ID})
	if !strings.Contains(text, taskDispatchOptOut) || strings.Contains(text, `"budget"`) {
		t.Fatalf("opt-out refusal %s; want the opt-out sentence and no budget", text)
	}
}

// budgetUnreadable is the plugin store with TaskDispatchBudget failing.
type budgetUnreadable struct{ plugins.DispatchStore }

func (budgetUnreadable) TaskDispatchBudget(context.Context) (plugins.DispatchBudget, error) {
	return plugins.DispatchBudget{}, errors.New("budget read failed (test)")
}

// TestAnUnreadableBudgetNeverFailsTheCall: with the budget read failing, a
// start still starts and answers (without a budget), a refusal still refuses
// with its own sentence, and task_route_attention still files its ask —
// withholding task_dispatch, and saying the allowance could not be read
// (never "not enabled for you", which an opted-in owner is not).
func TestAnUnreadableBudgetNeverFailsTheCall(t *testing.T) {
	f := newTaskRouteFixture(t, true, true, func(ext *Extensions) {
		ext.TaskDispatch = budgetUnreadable{ext.TaskDispatch}
	})
	n := f.task(t, "still starts")
	text := f.call(f.agent, taskDispatchTool, map[string]any{"task_id": n.ID})
	if isToolErr(text) || !strings.Contains(text, `"agent_id"`) || strings.Contains(text, `"budget"`) {
		t.Fatalf("start with the budget unreadable: %s; want a start with no budget", text)
	}
	g := f.task(t, "gated", "gate:needs-owner-ok")
	if text := f.call(f.agent, taskDispatchTool, map[string]any{"task_id": g.ID}); !isToolErr(text) || !strings.Contains(text, taskDispatchGatedText) {
		t.Fatalf("refusal with the budget unreadable: %s", text)
	}
	o := f.task(t, "ask anyway")
	res := f.call(f.agent, taskRouteAttentionTool, map[string]any{"task_id": o.ID, "note": "?"})
	if isToolErr(res) || !strings.Contains(res, `"actions_offered"`) || strings.Contains(res, `"budget"`) {
		t.Fatalf("task_route_attention with the budget unreadable: %s", res)
	}
	var out struct {
		ID      int64    `json:"id"`
		Offered []string `json:"actions_offered"`
	}
	if err := json.Unmarshal([]byte(res), &out); err != nil {
		t.Fatal(res)
	}
	for _, tool := range out.Offered {
		if tool == taskDispatchTool {
			t.Fatalf("task_dispatch offered with the budget unreadable: %q", out.Offered)
		}
	}
	ask, err := f.srv.ext.Attention.Get(f.asOwner, out.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ask.Question, "its allowance for starting agents could not be read right now") ||
		strings.Contains(ask.Question, "not enabled for you") {
		t.Errorf("the ask with the budget unreadable: %q; want the could-not-be-read sentence", ask.Question)
	}
}

// TestAFullBudgetIsNotBlamedForATaskThatRefusesItself: the ask names the
// reason task_dispatch is not offered, and only the one that applies. For an
// opted-in owner — budget full or with room — a gated or not-open task gets
// the task's own reason (never the allowance, never "not enabled for you"); an
// open task with a full budget gets the allowance, and with room is offered
// dispatch. An owner who is not opted in gets "not enabled for you".
func TestAFullBudgetIsNotBlamedForATaskThatRefusesItself(t *testing.T) {
	const (
		gatedLine   = "no agent is working it, and this task is gated (needs-owner-ok), so only you can release it): it is yours to handle."
		notOpenLine = "no agent is working it, and this task is not open (in_progress), so it cannot start one on it): it is yours to handle."
		fullLine    = "no agent is working it, and it has used up its allowance for starting agents for now): it is yours to handle."
		offLine     = "no agent is working it, and starting agents is not enabled for you): it is yours to handle."
	)
	for _, bc := range []struct {
		name         string
		optIn, full  bool
		gated, moved string // the line each task's ask must carry
		open         string // "" = task_dispatch offered instead
	}{
		{"opted-in/budget-full", true, true, gatedLine, notOpenLine, fullLine},
		{"opted-in/budget-room", true, false, gatedLine, notOpenLine, ""},
		{"not-opted-in", false, false, offLine, offLine, offLine},
	} {
		t.Run(bc.name, func(t *testing.T) {
			f := newTaskRouteFixture(t, true, bc.optIn)
			if bc.full {
				f.finishedStarts(t, 900900, plugins.MaxDispatchesPerDay) // the day is spent
			}
			gated := f.task(t, "gated", "gate:needs-owner-ok")
			moved := f.task(t, "moved on")
			if _, err := f.notes.SetStatus(f.asOwner, moved.ID, notes.StatusInProgress); err != nil {
				t.Fatal(err)
			}
			open := f.task(t, "open")
			for _, c := range []struct {
				name string
				id   int64
				line string
			}{{"gated", gated.ID, bc.gated}, {"not-open", moved.ID, bc.moved}, {"open", open.ID, bc.open}} {
				res := f.call(f.agent, taskRouteAttentionTool, map[string]any{"task_id": c.id, "note": "?"})
				var out struct {
					ID      int64    `json:"id"`
					Offered []string `json:"actions_offered"`
				}
				if err := json.Unmarshal([]byte(res), &out); err != nil || out.ID <= 0 {
					t.Fatalf("%s: %s", c.name, res)
				}
				ask, err := f.srv.ext.Attention.Get(f.asOwner, out.ID)
				if err != nil {
					t.Fatal(err)
				}
				if c.line == "" {
					if strings.Join(out.Offered, ",") != taskDispatchTool {
						t.Errorf("%s task with room: offered %q; want task_dispatch", c.name, out.Offered)
					}
					continue
				}
				if len(out.Offered) != 0 {
					t.Errorf("%s task: offered %q; want nothing", c.name, out.Offered)
				}
				if !strings.Contains(ask.Question, c.line) {
					t.Errorf("%s task: the ask %q\nwant it to carry %q", c.name, ask.Question, c.line)
				}
				if bc.optIn && strings.Contains(ask.Question, "not enabled for you") {
					t.Errorf("%s task of an opted-in owner says dispatch is not enabled: %q", c.name, ask.Question)
				}
				if c.line != fullLine && strings.Contains(ask.Question, "used up its allowance") {
					t.Errorf("%s task blames the allowance: %q", c.name, ask.Question)
				}
			}
		})
	}
}

// TestTheAskNamesTheDeploymentAndAStartingAgent: the two remaining reasons an
// opted-in owner's agent cannot dispatch carry their own WHOLE line — a
// deployment that cannot start an agent now, and an agent on the task that is
// still starting — never "not enabled for you".
func TestTheAskNamesTheDeploymentAndAStartingAgent(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(f *taskRouteFixture, t *testing.T, taskID int64)
		line  string
	}{
		{"deployment-cannot-start", func(f *taskRouteFixture, _ *testing.T, _ int64) {
			f.prov.undeliverable = "the kickoff path is down (test)"
		}, "The target agent cannot forward or dispatch this task now (no agent is working it, and this deployment cannot start an agent now): it is yours to handle."},
		{"agent-still-starting", func(f *taskRouteFixture, t *testing.T, taskID int64) {
			if _, err := agents.NewPG(f.pool).Create(f.asOwner, agents.Agent{Name: "trbud-starting-" + strconv.FormatInt(f.owner, 10),
				Namespace: "ns-trbud", Status: agents.StatusPending, NoteID: &taskID}); err != nil {
				t.Fatal(err)
			}
		}, "The target agent cannot forward or dispatch this task now (no agent is working it, and an agent on this task is still starting, so it cannot start another): it is yours to handle."},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newTaskRouteFixture(t, true, true)
			n := f.task(t, "why not")
			c.setup(f, t, n.ID)
			res := f.call(f.agent, taskRouteAttentionTool, map[string]any{"task_id": n.ID, "note": "?"})
			var out struct {
				ID      int64    `json:"id"`
				Offered []string `json:"actions_offered"`
			}
			if err := json.Unmarshal([]byte(res), &out); err != nil || out.ID <= 0 {
				t.Fatalf("task_route_attention answered %s", res)
			}
			if len(out.Offered) != 0 {
				t.Fatalf("offered %q; want nothing", out.Offered)
			}
			ask, err := f.srv.ext.Attention.Get(f.asOwner, out.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(ask.Question, "\n\n"+c.line) {
				t.Errorf("the ask %q\nwant it to end with the whole line %q", ask.Question, c.line)
			}
			if strings.Contains(ask.Question, "not enabled for you") {
				t.Errorf("an opted-in owner's ask says dispatch is not enabled: %q", ask.Question)
			}
		})
	}
}

// TestTaskRouteAttentionOffersDispatchOnlyWithinTheBudget pins the
// RELATIONSHIP for the budget: across a budget with room, a full live cap and a
// full day, task_dispatch is in actions_offered exactly when the tool then
// starts an agent, the result carries the budget, and an ask offering nothing
// says the allowance is used up.
func TestTaskRouteAttentionOffersDispatchOnlyWithinTheBudget(t *testing.T) {
	for _, c := range []struct {
		name        string
		seed        func(f *taskRouteFixture, t *testing.T)
		live, today int
		offered     bool
	}{
		{"room", func(f *taskRouteFixture, t *testing.T) {
			f.liveFleetAgent(t, "r1")
			f.liveFleetAgent(t, "r2")
			f.finishedStarts(t, 900600, 1)
		}, 2, 3, true},
		{"live-full", func(f *taskRouteFixture, t *testing.T) {
			f.finishedStarts(t, 900700, 2) // first: once the live cap is full, no start passes
			for i := 0; i < plugins.MaxLiveDispatchedAgents; i++ {
				f.liveFleetAgent(t, "l"+strconv.Itoa(i))
			}
		}, plugins.MaxLiveDispatchedAgents, plugins.MaxLiveDispatchedAgents + 2, false},
		{"day-full", func(f *taskRouteFixture, t *testing.T) {
			f.liveFleetAgent(t, "d")
			f.finishedStarts(t, 900800, plugins.MaxDispatchesPerDay-1)
		}, 1, plugins.MaxDispatchesPerDay, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newTaskRouteFixture(t, true, true)
			c.seed(f, t)
			n := f.task(t, "route me")
			res := f.call(f.agent, taskRouteAttentionTool, map[string]any{"task_id": n.ID, "note": "Who takes it?"})
			var out struct {
				ID      int64    `json:"id"`
				Offered []string `json:"actions_offered"`
			}
			if err := json.Unmarshal([]byte(res), &out); err != nil || out.ID <= 0 || out.Offered == nil {
				t.Fatalf("task_route_attention answered %s", res)
			}
			_, b := answerBudget(t, res)
			if b.Live != c.live || b.Today != c.today {
				t.Fatalf("budget %+v; want live %d, today %d", b, c.live, c.today)
			}
			offered := false
			for _, tool := range out.Offered {
				offered = offered || tool == taskDispatchTool
			}
			if offered != c.offered {
				t.Fatalf("task_dispatch offered=%v (%q); want %v", offered, out.Offered, c.offered)
			}
			ask, err := f.srv.ext.Attention.Get(f.asOwner, out.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !c.offered && !strings.Contains(ask.Question, "used up its allowance for starting agents") {
				t.Errorf("a full budget's ask does not say why nothing is offered: %q", ask.Question)
			}
			disp := f.call(f.agent, taskDispatchTool, map[string]any{"task_id": n.ID})
			started := !isToolErr(disp) && strings.Contains(disp, `"agent_id"`)
			if started != offered {
				t.Errorf("task_dispatch offered=%v but the tool started an agent=%v: %s", offered, started, disp)
			}
		})
	}
	// Opted out: no budget at all on the result.
	f := newTaskRouteFixture(t, true, false)
	n := f.task(t, "not opted in")
	if res := f.call(f.agent, taskRouteAttentionTool, map[string]any{"task_id": n.ID, "note": "?"}); strings.Contains(res, `"budget"`) {
		t.Fatalf("an opted-out owner's task_route_attention carries a budget: %s", res)
	}
}
