package api

import (
	"context"
	"io"
	"log"
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

// TestAnUnreadableRoleIsMemberAndMarkedUncertain pins the fallback's VALUE:
// for a user whose real role is ADMIN, a failed role read yields member, with
// the uncertain flag. The flag alone is not the guarantee — every reader of
// Role that does not look at the flag (the approval relay, the admin gate, the
// App gate) relies on the fallback being least privilege. roleOf, the other
// reader, is principalFor's role and must agree.
func TestAnUnreadableRoleIsMemberAndMarkedUncertain(t *testing.T) {
	s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	var fail atomic.Bool
	s.UseExtensions(Extensions{Users: failingUsers{fail: &fail}})
	if p := s.principalFor(context.Background(), authz.OperatorUserID, authz.ViaSession); p.Role != authz.RoleAdmin || p.RoleUncertain {
		t.Fatalf("control: a readable admin reads %+v", p)
	}
	fail.Store(true)
	p := s.principalFor(context.Background(), authz.OperatorUserID, authz.ViaSession)
	if p.Role != authz.RoleMember || !p.RoleUncertain {
		t.Fatalf("an admin whose role cannot be read is %+v; want member AND uncertain", p)
	}
	if r := s.roleOf(context.Background(), authz.OperatorUserID); r != authz.RoleMember {
		t.Fatalf("roleOf on an unreadable admin = %q; want member (principalFor's fallback)", r)
	}
}
