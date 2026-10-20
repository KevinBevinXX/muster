package api

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/connectors"
)

// TestAChatDoorRoleReadFailureDoesNotReopenAnAdminsAppGrant: an admin talks to
// their gateway agent through a session or route door, whose principal comes
// from principalFor. If the role read fails then, the principal FALLS BACK to
// member — and the agent's in-process connector tools (connector_status,
// connectors_list → agentConnectorView → reconcileGrant) must not act on that
// fallback: the admin's App-approved github grant stays approved. With the
// role readable (control) the grant also stands; a REAL member principal is
// what re-opens an App grant (the other control).
func TestAChatDoorRoleReadFailureDoesNotReopenAnAdminsAppGrant(t *testing.T) {
	f := newConnFixture(t)
	up := newFakeGitHubUpstream(t)
	const tok = "chat-door-agent-token-0123456789abcd"
	id := f.seedAgent(t, "chat-door", tok)
	if _, err := f.pool.Exec(f.ctx, `UPDATE agents SET repo='example-org/widget' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	srv, _, _ := appServer(t, f, up)
	var fail atomic.Bool
	ext := srv.ext
	ext.Users = failingUsers{fail: &fail}
	srv.UseExtensions(ext)
	g, _, err := f.store.RequestGrant(f.ctx, id, "github", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DecideGrant(f.ctx, g.ID, true, "operator", connectors.SourceApp); err != nil {
		t.Fatal(err)
	}
	a, err := srv.ext.Agents.Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	// The chat doors' principal: principalFor, as requireSession and the
	// session-route door build it.
	chat := func() context.Context {
		return authz.WithPrincipal(context.Background(), srv.principalFor(context.Background(), authz.OperatorUserID, authz.ViaSession))
	}
	status := func() string {
		return srv.dispatchAgentTool(chat(), a, "connector_status", `{"name":"github"}`)
	}
	if out := status(); !strings.Contains(out, `"approved"`) {
		t.Fatalf("control: with the role readable the App approval stands: %s", out)
	}
	fail.Store(true)
	_ = status()
	_ = srv.dispatchAgentTool(chat(), a, "connectors_list", `{}`)
	if ng, _ := f.store.GetGrant(f.ctx, g.ID); ng.Status != connectors.StatusApproved {
		t.Fatalf("a chat-door role-read failure re-opened the admin's App grant: %+v", ng)
	}
	fail.Store(false)
	if out := status(); !strings.Contains(out, `"approved"`) {
		t.Fatalf("after the blip: %s", out)
	}
}
