package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plugins"
)

// task_dispatch's kind and instructions arguments: which agent the chief
// starts, and the owner's words it may put in front of the task.

// withKinds wires an AgentKinds (both kinds enabled unless enabled says
// otherwise) whose pool picks account.
func withKinds(account string, enabled ...string) func(ext *Extensions) {
	return func(ext *Extensions) { ext.Kinds = stubKinds{pick: account, enabled: enabled} }
}

// reservationDetail is the detail of the owner's one reservation row for task id.
func (f *taskRouteFixture) reservationDetail(t *testing.T, id int64) map[string]any {
	t.Helper()
	var raw []byte
	if err := f.pool.QueryRow(f.asOwner, `SELECT detail FROM audit_log WHERE action = $1 AND object = $2`,
		plugins.ActionChiefDispatch, plugins.NoteObject(id)).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// startedOne asserts text is a start and returns the one agent on task id.
func (f *taskRouteFixture) startedOne(t *testing.T, text string, id int64) agents.Agent {
	t.Helper()
	if strings.Contains(text, `"error"`) {
		t.Fatalf("the start was refused: %s", text)
	}
	on := f.agentsOn(t, id)
	if len(on) != 1 {
		t.Fatalf("agents on task %d: %+v; want one", id, on)
	}
	return on[0]
}

// modelTask is an owner's task that names a (credentialed) model, so a start
// that passed it to a claude-code agent would be refused (claudeCodeNoModel).
func (f *taskRouteFixture) modelTask(t *testing.T, title string) notes.Note {
	t.Helper()
	n, err := f.notes.Create(f.asOwner, notes.Note{Title: title, Body: "Do the thing for " + title + ".", Model: "openrouter/example/model-a"})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestTaskDispatchDefaultsToClaudeCode: kind omitted on a deployment enabling
// claude-code starts a claude-code agent on the owner's pool account, without
// the task's model, and the reservation records the kind.
func TestTaskDispatchDefaultsToClaudeCode(t *testing.T) {
	f := newTaskRouteFixture(t, true, true, withKinds("pool-acct-a"))
	n := f.modelTask(t, "default kind")
	got := f.startedOne(t, f.call(f.chief, taskDispatchTool, map[string]any{"task_id": n.ID}), n.ID)
	if got.Kind != agents.KindClaudeCode || got.CCAccount != "pool-acct-a" || got.Model != "" {
		t.Fatalf("agent kind=%q account=%q model=%q; want claude-code on pool-acct-a with no model", got.Kind, got.CCAccount, got.Model)
	}
	if d := f.reservationDetail(t, n.ID); d["kind"] != agents.KindClaudeCode {
		t.Errorf("reservation detail %v: want kind claude-code", d)
	}
}

// TestTaskDispatchStartsAGatewayWhenAsked: kind gateway on the same deployment
// starts a gateway agent that keeps the task's model.
func TestTaskDispatchStartsAGatewayWhenAsked(t *testing.T) {
	f := newTaskRouteFixture(t, true, true, withKinds("pool-acct-a"))
	n := f.modelTask(t, "gateway kind")
	got := f.startedOne(t, f.call(f.chief, taskDispatchTool, map[string]any{"task_id": n.ID, "kind": "gateway"}), n.ID)
	if got.Kind != agents.KindGateway || got.CCAccount != "" || got.Model != "openrouter/example/model-a" {
		t.Fatalf("agent kind=%q account=%q model=%q; want a gateway agent on the task's model", got.Kind, got.CCAccount, got.Model)
	}
	if d := f.reservationDetail(t, n.ID); d["kind"] != agents.KindGateway {
		t.Errorf("reservation detail %v: want kind gateway", d)
	}
}

// TestTaskDispatchDefaultsToGatewayWithoutClaudeCode: a deployment that does
// not enable claude-code starts a gateway agent when kind is omitted.
func TestTaskDispatchDefaultsToGatewayWithoutClaudeCode(t *testing.T) {
	f := newTaskRouteFixture(t, true, true, withKinds("pool-acct-a", agents.KindGateway))
	n := f.task(t, "gateway only")
	if got := f.startedOne(t, f.call(f.chief, taskDispatchTool, map[string]any{"task_id": n.ID}), n.ID); got.Kind != agents.KindGateway {
		t.Fatalf("agent kind %q, want gateway", got.Kind)
	}
}

// TestTaskDispatchRefusesAKindItCannotStart: a disabled, unknown or unusable
// kind is refused BEFORE the reservation — no unit, no agent.
func TestTaskDispatchRefusesAKindItCannotStart(t *testing.T) {
	f := newTaskRouteFixture(t, true, true, withKinds("pool-acct-a", agents.KindGateway))
	n := f.task(t, "bad kind")
	f.mustRefuse(t, f.call(f.chief, taskDispatchTool, map[string]any{"task_id": n.ID, "kind": "claude-code"}), "is not enabled on this deployment", n.ID)
	f.mustRefuse(t, f.call(f.chief, taskDispatchTool, map[string]any{"task_id": n.ID, "kind": "robot"}), "unknown agent kind", n.ID)

	u := newTaskRouteFixture(t, true, true, func(ext *Extensions) {
		ext.Kinds = stubKinds{pick: "pool-acct-a", unavailable: map[string]string{agents.KindClaudeCode: "add a Claude account first"}}
	})
	m := u.task(t, "no credential")
	u.mustRefuse(t, u.call(u.chief, taskDispatchTool, map[string]any{"task_id": m.ID}), "add a Claude account first", m.ID)
}

// TestTaskDispatchRefusesARepoForClaudeCode: a claude-code agent cannot take a
// repository, named by the chief or by the task — refused before the
// reservation, with the sentence naming gateway.
func TestTaskDispatchRefusesARepoForClaudeCode(t *testing.T) {
	f := newTaskRouteFixture(t, true, true, withKinds("pool-acct-a"))
	f.withConnectedRepos(t, "example/app")
	n := f.task(t, "repo arg")
	f.mustRefuse(t, f.call(f.chief, taskDispatchTool, map[string]any{"task_id": n.ID, "repo": "example/app"}), chiefDispatchCCRepo, n.ID)
	r, err := f.notes.Create(f.asOwner, notes.Note{Title: "task repo", Body: "b", Repo: "example/app"})
	if err != nil {
		t.Fatal(err)
	}
	f.mustRefuse(t, f.call(f.chief, taskDispatchTool, map[string]any{"task_id": r.ID}), chiefDispatchCCRepo, r.ID)
	// The same task as a gateway agent starts, on its repository.
	if got := f.startedOne(t, f.call(f.chief, taskDispatchTool, map[string]any{"task_id": r.ID, "kind": "gateway"}), r.ID); got.Repo != "example/app" {
		t.Fatalf("gateway agent repo %q", got.Repo)
	}
}

// TestTaskDispatchPutsTheInstructionsFirstAndTheRulesLast: the chief's
// instructions open the first message, fenced and stripped of control
// characters; the task's text follows; muster's rules close it. The
// reservation records them, and the agent is still named after the task.
func TestTaskDispatchPutsTheInstructionsFirstAndTheRulesLast(t *testing.T) {
	f := newTaskRouteFixture(t, true, true, withKinds("pool-acct-a"))
	n := f.task(t, "with instructions")
	raw := "Open with the example-blocks skill.\x1b[31m‮\nThen read the task."
	got := f.startedOne(t, f.call(f.chief, taskDispatchTool, map[string]any{"task_id": n.ID, "instructions": raw}), n.ID)
	msg := got.PendingNote
	iInstr := strings.Index(msg, "| Open with the example-blocks skill.[31m\n| Then read the task.\n")
	iBody, iRules := strings.Index(msg, n.Body), strings.Index(msg, chiefDispatchRules)
	if iInstr < 0 || iBody < 0 || iRules < 0 || !(iInstr < iBody && iBody < iRules) {
		t.Fatalf("first message %q: want the fenced instructions, then the task, then the rules", msg)
	}
	if !strings.HasPrefix(msg, "muster: your owner's chief relayed the instructions") ||
		!strings.Contains(msg, "<<<"+chiefInstructionsMarker+" untrusted nonce=") {
		t.Errorf("first message %q: want muster's preface and the fence first", msg)
	}
	if !strings.HasSuffix(msg, chiefDispatchRules) {
		t.Errorf("first message does not END with muster's rules: %q", msg)
	}
	if strings.ContainsAny(msg, "\x1b‮") {
		t.Errorf("a control character survived: %q", msg)
	}
	if got.DisplayName != dispatchDisplayName(n.ID, n.Body) {
		t.Errorf("display name %q, want the task's", got.DisplayName)
	}
	d := f.reservationDetail(t, n.ID)
	if d["instructions"] != "Open with the example-blocks skill.[31m\nThen read the task." || d["kind"] != agents.KindClaudeCode {
		t.Errorf("reservation detail %v: want the cleaned instructions and the kind", d)
	}
}

// TestTaskDispatchRefusesInstructionsOverTheCap: over 1 KiB is refused before
// the reservation.
func TestTaskDispatchRefusesInstructionsOverTheCap(t *testing.T) {
	f := newTaskRouteFixture(t, true, true, withKinds("pool-acct-a"))
	n := f.task(t, "long instructions")
	f.mustRefuse(t, f.call(f.chief, taskDispatchTool, map[string]any{"task_id": n.ID,
		"instructions": strings.Repeat("a", chiefInstructionsMax+1)}), chiefDispatchInstructionsCap, n.ID)
	f.startedOne(t, f.call(f.chief, taskDispatchTool, map[string]any{"task_id": n.ID,
		"instructions": strings.Repeat("a", chiefInstructionsMax)}), n.ID)
}

// TestCleanInstructions pins the cap's boundary and what is stripped.
func TestCleanInstructions(t *testing.T) {
	for _, c := range []struct {
		in, want string
		ok       bool
	}{
		{"  a\r\nb\tc  ", "a\nb c", true},
		{"x\x00y​z\x7f", "xyz", true},
		{strings.Repeat("é", chiefInstructionsMax/2), strings.Repeat("é", chiefInstructionsMax/2), true},
		{strings.Repeat("é", chiefInstructionsMax/2) + "a", "", false},
		{"\x00" + strings.Repeat("b", chiefInstructionsMax), "", false},
	} {
		got, ok := cleanInstructions(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("cleanInstructions(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}
