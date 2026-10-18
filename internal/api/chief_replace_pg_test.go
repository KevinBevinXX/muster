package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/plugins"
)

// THE CHIEF REPLACEMENT (chief_replace.go) on a real Postgres: the confirm
// gate, the two in-flight refusals, the retry-safe states, and the shadowing
// refusal. The provisioner is a recorder whose Destroy deletes the row the way
// agentprovision.Adapter.Destroy does on success.

type replaceProv struct {
	stubProvisioner
	pool *pgxpool.Pool
	mu   sync.Mutex
	// destroyErr, when set, fails Destroy and keeps the row (Adapter.Destroy's
	// behaviour on a failed teardown).
	destroyErr         error
	destroyed, started []int64
	dispatched         chan int64
}

func (p *replaceProv) Destroy(_ context.Context, id int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.destroyErr != nil {
		return p.destroyErr
	}
	p.destroyed = append(p.destroyed, id)
	_, err := p.pool.Exec(authz.System(context.Background()), `DELETE FROM agents WHERE id = $1`, id)
	return err
}

func (p *replaceProv) Start(_ context.Context, id int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.started = append(p.started, id)
	return nil
}

func (p *replaceProv) Dispatch(_ context.Context, id int64, kickoff bool) error {
	if kickoff {
		p.dispatched <- id
	}
	return nil
}

func (p *replaceProv) destroyedIDs() []int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int64(nil), p.destroyed...)
}

type replaceFixture struct {
	*attnFixture
	prov   *replaceProv
	owner  int64
	old    agents.Agent
	plugin plugins.Plugin
}

// newReplaceFixture: a member whose reserved-slug chief is a running GATEWAY
// agent, on a server with both kinds enabled and the plugin store wired.
func newReplaceFixture(t *testing.T, mutate func(*Extensions)) *replaceFixture {
	t.Helper()
	f := &replaceFixture{prov: &replaceProv{dispatched: make(chan int64, 4)}}
	f.attnFixture = &attnFixture{}
	f.attnFixture.connFixture = newConnFixtureWith(t, func(ext *Extensions, pool *pgxpool.Pool) {
		f.prov.pool = pool
		f.attnFixture.plugins = plugins.NewPG(pool)
		ext.Plugins, ext.Provisioner, ext.Kinds = f.attnFixture.plugins, f.prov, stubKinds{}
		if mutate != nil {
			mutate(ext)
		}
	})
	f.owner = f.newMember(t)
	f.old = f.agentFor(t, f.owner, agents.ChiefNameFor(f.owner), ChiefDisplayName, "replace-old-chief-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM agents WHERE owner_id = $1`, f.owner)
	})
	return f
}

func (f *replaceFixture) replace(req chiefReplaceRequest) (int, map[string]any) {
	return f.srv.replaceChief(asUser(f.owner), f.owner, req)
}

func (f *replaceFixture) chiefRow(t *testing.T) (agents.Agent, bool) {
	t.Helper()
	a, err := agents.NewPG(f.pool).GetByName(asUser(f.owner), agents.ChiefNameFor(f.owner))
	if err != nil {
		return agents.Agent{}, false
	}
	return a, true
}

func (f *replaceFixture) waitDispatch(t *testing.T) int64 {
	t.Helper()
	select {
	case id := <-f.prov.dispatched:
		return id
	case <-time.After(10 * time.Second):
		t.Fatal("the new chief was never dispatched")
		return 0
	}
}

func ccReq(confirm string) chiefReplaceRequest {
	return chiefReplaceRequest{Kind: agents.KindClaudeCode, CCAccount: "acct-x", Confirm: confirm}
}

// TestReplacingTheChiefNeedsConfirmAndDeletesOnlyThen: without the slug in
// confirm — absent, or the OPERATOR's slug for a member — nothing is destroyed or
// created and the plan names the agent it would delete. With it, the old chief is
// destroyed once and a claude-code chief is created under the same slug,
// labelled chief, on the pinned account, and dispatched with its first turn.
func TestReplacingTheChiefNeedsConfirmAndDeletesOnlyThen(t *testing.T) {
	f := newReplaceFixture(t, nil)
	slug := agents.ChiefNameFor(f.owner)
	for _, confirm := range []string{"", agents.ChiefName, slug + "x"} {
		code, out := f.replace(ccReq(confirm))
		if code != http.StatusPreconditionRequired {
			t.Fatalf("confirm %q: %d %v; want 428", confirm, code, out)
		}
		msg, _ := out["error"].(string)
		if !strings.Contains(msg, "DELETES agent "+slug+" (id "+strconv.FormatInt(f.old.ID, 10)) ||
			!strings.Contains(msg, "creates "+slug+" on kind claude-code, Claude account acct-x") {
			t.Fatalf("confirm %q: the plan does not name what it would do: %q", confirm, msg)
		}
		if out["confirm"] != slug {
			t.Fatalf("the plan names confirm %v, want %q", out["confirm"], slug)
		}
	}
	if d := f.prov.destroyedIDs(); len(d) != 0 {
		t.Fatalf("an unconfirmed call destroyed %v", d)
	}
	if row, ok := f.chiefRow(t); !ok || row.ID != f.old.ID {
		t.Fatalf("an unconfirmed call changed the chief row: %+v %v", row, ok)
	}

	code, out := f.replace(ccReq(slug))
	if code != http.StatusOK {
		t.Fatalf("confirmed: %d %v", code, out)
	}
	if d := f.prov.destroyedIDs(); len(d) != 1 || d[0] != f.old.ID {
		t.Fatalf("destroyed %v, want exactly the old chief %d", d, f.old.ID)
	}
	deleted, _ := out["deleted"].(map[string]any)
	if deleted == nil || deleted["id"] != f.old.ID || deleted["kind"] != agents.KindGateway {
		t.Fatalf("the report of what was deleted: %v", out["deleted"])
	}
	row, ok := f.chiefRow(t)
	if !ok || row.ID == f.old.ID || agents.ResolveKind(row.Kind) != agents.KindClaudeCode || row.CCAccount != "acct-x" ||
		row.DisplayName != ChiefDisplayName || row.OwnerID != f.owner {
		t.Fatalf("the new chief row: %+v (found %v)", row, ok)
	}
	if !strings.Contains(row.NoteText, "mcp__muster__") {
		t.Fatalf("the new chief's first turn is the gateway intro: %q", row.NoteText)
	}
	if got := f.waitDispatch(t); got != row.ID {
		t.Fatalf("dispatched %d, want the new chief %d", got, row.ID)
	}
	// The resolver every consumer uses now answers the new row.
	list, _ := agents.NewPG(f.pool).List(asUser(f.owner))
	if c, ok := resolveChief(list); !ok || c.ID != row.ID {
		t.Fatalf("resolveChief = %+v %v, want the new chief %d", c, ok, row.ID)
	}
}

// TestReplacingTheChiefRefusesWhileAPluginMessageIsDelivering: a message
// `delivering` to the old chief refuses the swap (409, its id reported) and
// nothing is destroyed; the swap's own session claim is RELEASED on that
// refusal; once the message settles the same call goes through.
func TestReplacingTheChiefRefusesWhileAPluginMessageIsDelivering(t *testing.T) {
	f := newReplaceFixture(t, nil)
	_, p, err := f.plugins.Create(asUser(f.owner), "replace-inbox", "Replace inbox", "replace-inbox")
	if err != nil {
		t.Fatal(err)
	}
	m, _, err := f.plugins.Submit(asUser(f.owner), p.ID, plugins.MessageIn{ExternalID: "rp-1", Sender: "a@example.test", Subject: "s", Body: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := f.plugins.BeginDelivery(asUser(f.owner), m.ID, f.old.ID); err != nil || !ok {
		t.Fatalf("begin: %v %v", ok, err)
	}
	slug := agents.ChiefNameFor(f.owner)
	code, out := f.replace(ccReq(slug))
	ids, _ := out["delivering"].([]int64)
	if code != http.StatusConflict || len(ids) != 1 || ids[0] != m.ID {
		t.Fatalf("with a message delivering: %d %v; want 409 naming message %d", code, out, m.ID)
	}
	if d := f.prov.destroyedIDs(); len(d) != 0 {
		t.Fatalf("destroyed %v while a message was delivering", d)
	}
	won, err := agents.NewPG(f.pool).ClaimKickoff(asUser(f.owner), f.old.ID, "a-later-delivery", time.Minute)
	if err != nil || !won {
		t.Fatalf("the refused swap left the chief's session claimed (won=%v err=%v)", won, err)
	}
	_ = agents.NewPG(f.pool).ReleaseKickoffClaim(asUser(f.owner), f.old.ID, "a-later-delivery")
	if ok, err := f.plugins.RecordDelivery(asUser(f.owner), m.ID, f.old.ID, plugins.StatusDelivered); err != nil || !ok {
		t.Fatalf("settle: %v %v", ok, err)
	}
	if code, out := f.replace(ccReq(slug)); code != http.StatusOK {
		t.Fatalf("after the message settled: %d %v", code, out)
	}
	f.waitDispatch(t)
}

// TestReplacingTheChiefRefusesWhileATurnHoldsItsSession: a plugin delivery, a
// kickoff or a memo turn holds the chief's session claim for its whole turn; the
// swap cannot take it, so it refuses and destroys nothing.
func TestReplacingTheChiefRefusesWhileATurnHoldsItsSession(t *testing.T) {
	f := newReplaceFixture(t, nil)
	if won, err := agents.NewPG(f.pool).ClaimKickoff(asUser(f.owner), f.old.ID, "a-delivery-in-flight", time.Minute); err != nil || !won {
		t.Fatalf("control: could not take the claim: %v %v", won, err)
	}
	code, out := f.replace(ccReq(agents.ChiefNameFor(f.owner)))
	if code != http.StatusConflict || !strings.Contains(out["error"].(string), "a turn is being delivered to the chief") {
		t.Fatalf("with the session claimed: %d %v; want 409", code, out)
	}
	if d := f.prov.destroyedIDs(); len(d) != 0 {
		t.Fatalf("destroyed %v while a turn held the session", d)
	}
}

// TestAHalfFinishedReplacementIsFinishedByRunningItAgain covers the three
// states a failed run can leave, each answered by re-running the SAME call:
// a destroy that failed (row kept, nothing created) is retried; an old chief
// gone with no new one is created without a destroy; a chief already on the
// requested kind is re-applied (Start), not deleted.
func TestAHalfFinishedReplacementIsFinishedByRunningItAgain(t *testing.T) {
	f := newReplaceFixture(t, nil)
	slug := agents.ChiefNameFor(f.owner)

	f.prov.destroyErr = errors.New("apiserver unreachable")
	code, out := f.replace(ccReq(slug))
	if code != http.StatusBadGateway || out["created"] != false {
		t.Fatalf("a failed destroy: %d %v; want 502 and nothing created", code, out)
	}
	if row, ok := f.chiefRow(t); !ok || row.ID != f.old.ID {
		t.Fatalf("a failed destroy lost the old chief row: %+v %v", row, ok)
	}
	f.prov.destroyErr = nil

	// The state a create failure leaves: the old chief is gone, no new one.
	if _, err := f.pool.Exec(authz.System(context.Background()), `DELETE FROM agents WHERE id = $1`, f.old.ID); err != nil {
		t.Fatal(err)
	}
	if code, out := f.replace(ccReq("")); code != http.StatusPreconditionRequired ||
		!strings.Contains(out["error"].(string), "there is no "+slug+"; the confirmed call deletes nothing") {
		t.Fatalf("the plan with no chief: %d %v", code, out)
	}
	code, out = f.replace(ccReq(slug))
	if code != http.StatusOK || out["deleted"] != nil || out["created"] != true {
		t.Fatalf("re-run with no chief: %d %v; want created, nothing deleted", code, out)
	}
	newID := f.waitDispatch(t)
	if d := f.prov.destroyedIDs(); len(d) != 0 {
		t.Fatalf("re-run destroyed %v", d)
	}

	// And once more: the chief is now the requested kind and account, so the
	// call keeps it and re-applies it.
	code, out = f.replace(ccReq(slug))
	if code != http.StatusOK || out["started"] != true || out["deleted"] != nil {
		t.Fatalf("re-run on the finished swap: %d %v; want started, nothing deleted", code, out)
	}
	if row, ok := f.chiefRow(t); !ok || row.ID != newID {
		t.Fatalf("re-run replaced a chief that already matched: %+v", row)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.prov.mu.Lock()
		started := append([]int64(nil), f.prov.started...)
		f.prov.mu.Unlock()
		if len(started) == 1 && started[0] == newID {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("started %v, want [%d]", started, newID)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// A DIFFERENT account on the same kind is a replacement, not a keep (the
	// account is immutable on the row).
	if code, out := f.replace(chiefReplaceRequest{Kind: agents.KindClaudeCode, CCAccount: "acct-y"}); code != http.StatusPreconditionRequired ||
		!strings.Contains(out["error"].(string), "DELETES agent "+slug) {
		t.Fatalf("another account: %d %v; want a plan that deletes", code, out)
	}
}

// TestReplacingTheChiefRefusesWhenAnotherAgentIsLabelledChief: the new row would
// have the highest id, so a lower-id agent labelled chief would keep answering
// as the chief; the swap refuses, naming it, and changes nothing.
func TestReplacingTheChiefRefusesWhenAnotherAgentIsLabelledChief(t *testing.T) {
	f := newReplaceFixture(t, nil)
	other := f.agentFor(t, f.owner, "replace-labelled-"+strconv.FormatInt(time.Now().UnixNano(), 36), "Chief", "replace-labelled-tok-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	code, out := f.replace(ccReq(agents.ChiefNameFor(f.owner)))
	if code != http.StatusConflict || !strings.Contains(out["error"].(string), strconv.FormatInt(other.ID, 10)) {
		t.Fatalf("with another agent labelled chief: %d %v", code, out)
	}
	if d := f.prov.destroyedIDs(); len(d) != 0 {
		t.Fatalf("destroyed %v", d)
	}
}

// TestReplacingTheChiefRefusesWhatTheCreateWouldRefuseBeforeDeleting: a kind
// that is not enabled, a model for a claude-code chief, and a claude-code chief
// on a server with no MCP door are each refused BEFORE the old chief is touched.
func TestReplacingTheChiefRefusesWhatTheCreateWouldRefuseBeforeDeleting(t *testing.T) {
	f := newReplaceFixture(t, nil)
	slug := agents.ChiefNameFor(f.owner)
	for _, req := range []chiefReplaceRequest{
		{Kind: "nonesuch", Confirm: slug},
		{Kind: agents.KindClaudeCode, Model: "anthropic/claude-x", Confirm: slug},
	} {
		if code, out := f.replace(req); code != http.StatusConflict {
			t.Fatalf("%+v: %d %v; want 409", req, code, out)
		}
	}
	g := newReplaceFixture(t, func(ext *Extensions) { ext.Connectors = nil })
	code, out := g.replace(ccReq(agents.ChiefNameFor(g.owner)))
	if code != http.StatusConflict || !strings.Contains(out["error"].(string), "MCP server") {
		t.Fatalf("claude-code with no MCP door: %d %v", code, out)
	}
	if len(f.prov.destroyedIDs())+len(g.prov.destroyedIDs()) != 0 {
		t.Fatal("a refused replacement destroyed the chief")
	}
}

// TestTheReplaceRouteIsTheHookTierAndChangesNothingWithoutConfirm: the route as
// a CLI reaches it — the shared hook token, a JSON body — answers the plan and
// destroys nothing.
func TestTheReplaceRouteIsTheHookTierAndChangesNothingWithoutConfirm(t *testing.T) {
	f := newReplaceFixture(t, nil)
	w := f.call(t, http.MethodPost, "/api/chief/replace", strings.NewReader(`{"kind":"claude-code","cc_account":"acct-x"}`),
		map[string]string{"Authorization": "Bearer " + connTestHookToken, "Content-Type": "application/json"})
	if w.Code != http.StatusPreconditionRequired || !strings.Contains(w.Body.String(), `"confirm":"chief"`) {
		t.Fatalf("POST /api/chief/replace as the hook tier: %d %s", w.Code, w.Body)
	}
	if w := f.call(t, http.MethodPost, "/api/chief/replace", strings.NewReader(`{"kind":`), map[string]string{"Authorization": "Bearer " + connTestHookToken}); w.Code != http.StatusBadRequest {
		t.Fatalf("a malformed body: %d", w.Code)
	}
	if d := f.prov.destroyedIDs(); len(d) != 0 {
		t.Fatalf("destroyed %v", d)
	}
}
