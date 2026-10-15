package api

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"math/rand"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/push"
)

// The gated-task tool over FAKE stores — every path that does not need Postgres
// to be meaningful. gated_task_pg_test.go runs the same tool through the real
// stores and the real /mcp door.

const gtOwner = int64(7)

// gtNotes records every task created through it.
type gtNotes struct {
	notes.Store
	mu      sync.Mutex
	created []notes.Note
}

func (n *gtNotes) Create(_ context.Context, x notes.Note) (notes.Note, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	x.ID = int64(len(n.created) + 1)
	x.OwnerID = gtOwner
	n.created = append(n.created, x)
	return x, nil
}

func (n *gtNotes) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.created)
}

func (n *gtNotes) last() notes.Note {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.created[len(n.created)-1]
}

// gtAgents is an agent list with no row-level security.
type gtAgents struct {
	agents.Store
	list []agents.Agent
}

func (a gtAgents) List(context.Context) ([]agents.Agent, error) { return a.list, nil }

// gtPlugins is a plugin store WITHOUT row-level security: Message hands back any
// row by id, whoever owns it — so the tool's own checks are what is measured.
type gtPlugins struct {
	plugins.Store
	mu       sync.Mutex
	msgs     map[int64]plugins.Message
	reserved int
}

func (p *gtPlugins) ReserveGatedTask(context.Context, int64, *int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reserved++
	return nil
}

func (p *gtPlugins) Message(_ context.Context, id int64) (plugins.Message, error) {
	if m, ok := p.msgs[id]; ok {
		return m, nil
	}
	return plugins.Message{}, plugins.ErrNotFound
}

func (p *gtPlugins) Plugin(_ context.Context, id int64) (plugins.Plugin, error) {
	return plugins.Plugin{ID: id, OwnerID: gtOwner, Label: "Example inbox"}, nil
}

type gatedFakeFixture struct {
	srv           *Server
	ctx           context.Context
	chief, worker agents.Agent
	notes         *gtNotes
	plugins       *gtPlugins
	pusher        *payloadPusher
	timers        *manualTimers
}

func newGatedFakeFixture(t *testing.T) *gatedFakeFixture {
	t.Helper()
	f := &gatedFakeFixture{
		chief:   agents.Agent{ID: 11, OwnerID: gtOwner, Name: "keen-wren", DisplayName: "chief"},
		worker:  agents.Agent{ID: 12, OwnerID: gtOwner, Name: "plain-otter"},
		notes:   &gtNotes{},
		plugins: &gtPlugins{msgs: map[int64]plugins.Message{}},
		pusher:  &payloadPusher{},
		timers:  &manualTimers{},
	}
	store := &memPush{}
	claim(t, store, gtOwner, ep("gated-owner"))
	logger := log.New(io.Discard, "", 0)
	f.srv = New(nil, AuthConfig{}, logger)
	f.srv.UseExtensions(Extensions{
		Users:   stubUsers{},
		Push:    push.NewService(store, f.pusher, push.VAPIDConfig{PublicKey: "k"}, logger),
		Notes:   f.notes,
		Agents:  gtAgents{list: []agents.Agent{f.chief, f.worker}},
		Plugins: f.plugins,
	})
	f.srv.taskCreateAfter = f.timers.after
	f.ctx = authz.WithPrincipal(context.Background(), authz.Principal{UserID: gtOwner, Role: authz.RoleMember, Via: authz.ViaSession})
	return f
}

func (f *gatedFakeFixture) call(a agents.Agent, args any) string {
	raw, _ := json.Marshal(args)
	return f.srv.AgentToolDispatch(f.ctx, a)(gatedTaskToolName, string(raw))
}

// TestAGatedTaskIsAlwaysGatedAndNeverAuto is the PROPERTY: for any input the
// chief can send — tags and gate reasons drawn from an alphabet built to hit the
// reserved namespaces in every spelling normalisation folds together — EITHER the
// call is refused and nothing is created, OR the created task carries exactly one
// `gate:` tag (the server's) and no `auto:` tag at all.
//
// It runs the WHOLE tool (dispatch → validation → createTask → the store), so it
// sees the tags the store was actually handed, not a helper's return value.
func TestAGatedTaskIsAlwaysGatedAndNeverAuto(t *testing.T) {
	f := newGatedFakeFixture(t)
	pieces := []string{"gate", "GATE", "Gate", "auto", "AUTO", "Auto", ":", ":", " ", "\t", "dispatch", "x",
		"needs", "approval", "project", "initiative", "é", "-", "_", ".", "/", "!!", "0", "auto:dispatch", "gate:x"}
	word := func(r *rand.Rand) string {
		var b strings.Builder
		for i, n := 0, 1+r.Intn(4); i < n; i++ {
			b.WriteString(pieces[r.Intn(len(pieces))])
		}
		return b.String()
	}
	type input struct {
		tags   []string
		reason string
	}
	// The seeds a mutant must not survive, then a deterministic random sweep.
	inputs := []input{
		{[]string{"auto:dispatch"}, "x"},
		{[]string{" AUTO:Dispatch "}, ""},
		{[]string{"gate:none"}, "x"},
		{[]string{"GATE:Owner"}, "ok"},
		{nil, ""},
		{nil, "!!!"},
		{[]string{"inbox"}, strings.Repeat("reason ", 40)},
	}
	r := rand.New(rand.NewSource(20261014))
	for i := 0; i < 3000; i++ {
		var in input
		for j, n := 0, r.Intn(5); j < n; j++ {
			in.tags = append(in.tags, word(r))
		}
		in.reason = word(r)
		inputs = append(inputs, in)
	}

	created, refused := 0, 0
	for _, in := range inputs {
		before := f.notes.count()
		out := f.call(f.chief, map[string]any{"title": "t", "body": "b", "gate_reason": in.reason, "tags": in.tags})
		if isToolErr(out) {
			refused++
			if f.notes.count() != before {
				t.Fatalf("input %+v was refused (%s) but a task was created", in, out)
			}
			continue
		}
		created++
		if f.notes.count() != before+1 {
			t.Fatalf("input %+v succeeded (%s) but created %d tasks", in, out, f.notes.count()-before)
		}
		tags := f.notes.last().Tags
		if !notes.IsGated(tags) {
			t.Fatalf("input %+v: the created task is NOT gated: %v", in, tags)
		}
		gates := 0
		for _, tag := range tags {
			switch ns, _ := notes.ParseTag(tag); ns {
			case notes.NSAuto:
				t.Fatalf("input %+v: the created task carries %q", in, tag)
			case notes.NSGate:
				gates++
			}
		}
		if gates != 1 {
			t.Fatalf("input %+v: %d gate tags %v, want exactly the server's one", in, gates, tags)
		}
	}
	// Both halves must have been exercised, or one of the branches above is vacuous.
	if created < 100 || refused < 100 {
		t.Fatalf("the sweep created %d and refused %d; both must be well above zero", created, refused)
	}
}

// TestTheGateSlug pins the reason → tag-value mapping with literals.
func TestTheGateSlug(t *testing.T) {
	for in, want := range map[string]string{
		"":                        "awaiting-approval",
		"   ":                     "awaiting-approval",
		"!!!":                     "awaiting-approval",
		"Needs Owner OK!":         "needs-owner-ok",
		"  trailing and leading ": "trailing-and-leading",
		"Ünïcode reason":          "n-code-reason",
		strings.Repeat("a", 100):  strings.Repeat("a", 59),
	} {
		if got := gateSlug(in); got != want {
			t.Errorf("gateSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestAGatedTaskNotifiesTheOwner: the tool creates through the shared createTask,
// so the owner gets the existing coalesced `created` push — the design's "notify
// me". Measured red by creating through Notes.Create directly instead.
func TestAGatedTaskNotifiesTheOwner(t *testing.T) {
	f := newGatedFakeFixture(t)
	if out := f.call(f.chief, map[string]any{"title": "Reply to the inbox", "body": "b", "gate_reason": "r"}); isToolErr(out) {
		t.Fatalf("the chief's call failed: %s", out)
	}
	if n := f.timers.fireAll(); n != 1 {
		t.Fatalf("%d created-push timers armed, want 1", n)
	}
	f.srv.WaitForPushes()
	got := f.pusher.to(ep("gated-owner"))
	if len(got) != 1 || got[0].Title != "New task" || got[0].Body != taskTitle(f.notes.last()) || got[0].Body == "" {
		t.Fatalf("the owner's device got %+v, want one \"New task\" push naming the task", got)
	}
}

// TestANonChiefCannotCallTheGatedToolThroughTheGateway: the gateway's dispatch
// closure runs whatever name the runtime hands it, offered or not, so the
// chief-only re-check in dispatchAgentTool is the ONLY guard on this path. A
// worker's call is refused with nothing created and no budget spent; so is a
// caller-side copy of the worker's row that CLAIMS to be the chief, because the
// re-check reads the current row. The chief's call is the control.
func TestANonChiefCannotCallTheGatedToolThroughTheGateway(t *testing.T) {
	f := newGatedFakeFixture(t)
	args := map[string]any{"title": "t", "body": "b", "gate_reason": "r"}

	// The listing leaves it out for the worker (the first guard)…
	for _, d := range f.srv.AgentToolDefsFor(f.ctx, f.worker) {
		if d.Name == gatedTaskToolName {
			t.Fatal("the worker is OFFERED the chief-only tool")
		}
	}
	// …and dispatch refuses it anyway (the second).
	forged := f.worker
	forged.DisplayName = "chief"
	for name, a := range map[string]agents.Agent{"worker": f.worker, "worker claiming to be chief": forged} {
		out := f.call(a, args)
		if !strings.Contains(out, "is available to your owner's chief agent only") {
			t.Errorf("%s: %s, want the chief-only refusal", name, out)
		}
	}
	if f.notes.count() != 0 || f.plugins.reserved != 0 {
		t.Fatalf("a non-chief call created %d task(s) and reserved %d unit(s)", f.notes.count(), f.plugins.reserved)
	}
	if out := f.call(f.chief, args); isToolErr(out) {
		t.Fatalf("control: the chief's call failed: %s", out)
	}
}

// TestAGatedTaskCitesOnlyItsOwnersMessageEvenWithoutRLS: over a store with no
// row-level security, the tool itself refuses a message of another owner (even
// one whose row names this chief), one delivered or being delivered to another
// agent, and a queued one — before any budget is spent. The ones delivered and
// being delivered to this chief are the controls.
func TestAGatedTaskCitesOnlyItsOwnersMessageEvenWithoutRLS(t *testing.T) {
	f := newGatedFakeFixture(t)
	chief, worker := f.chief.ID, f.worker.ID
	f.plugins.msgs = map[int64]plugins.Message{
		1: {ID: 1, OwnerID: gtOwner + 1, AgentID: &chief, Status: plugins.StatusDelivered},
		2: {ID: 2, OwnerID: gtOwner, AgentID: &worker, Status: plugins.StatusDelivered},
		3: {ID: 3, OwnerID: gtOwner, AgentID: &chief, Status: plugins.StatusQueued},
		4: {ID: 4, OwnerID: gtOwner, AgentID: &chief, Status: plugins.StatusDelivered},
		5: {ID: 5, OwnerID: gtOwner, AgentID: &worker, Status: plugins.StatusDelivering},
		6: {ID: 6, OwnerID: gtOwner + 1, AgentID: &chief, Status: plugins.StatusDelivering},
		7: {ID: 7, OwnerID: gtOwner, AgentID: &chief, Status: plugins.StatusDelivering},
	}
	for _, id := range []int64{1, 2, 3, 5, 6} {
		out := f.call(f.chief, map[string]any{"title": "t", "body": "b", "gate_reason": "r", "message_id": id})
		if !strings.Contains(out, "is not a plugin message delivered to you") {
			t.Errorf("message %d: %s, want refused", id, out)
		}
	}
	if f.notes.count() != 0 || f.plugins.reserved != 0 {
		t.Fatalf("refused citations created %d task(s) and reserved %d unit(s)", f.notes.count(), f.plugins.reserved)
	}
	// Controls: delivered to this chief, and BEING delivered to it (the turn that
	// tells the chief about the message — audit round 1).
	for _, id := range []int64{4, 7} {
		if out := f.call(f.chief, map[string]any{"title": "t", "body": "b", "gate_reason": "r", "message_id": id}); isToolErr(out) {
			t.Fatalf("control: citing message %d (delivered or delivering to this chief) failed: %s", id, out)
		}
	}
}
