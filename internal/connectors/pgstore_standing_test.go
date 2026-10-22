package connectors_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/connectors"
)

// Standing grants (migration 0043): an owner's approval shared by all of the
// owner's agents. These pin the STORE's half of the rules; the request and
// decision doors that use them are tested in internal/api
// (connector_standing_pg_test.go).

// approved files agent's request for connector and approves it by hand.
func approved(t *testing.T, ctx context.Context, s *connectors.PGStore, agent int64, connector string) connectors.Grant {
	t.Helper()
	g, _, err := s.RequestGrant(ctx, agent, connector, "")
	if err != nil {
		t.Fatal(err)
	}
	if g, err = s.DecideGrant(ctx, g.ID, true, "operator", connectors.SourceStored); err != nil {
		t.Fatal(err)
	}
	return g
}

func TestStandingRepoKeysTheAppOnTheRepositoryAndAStoredCredentialOnNothing(t *testing.T) {
	for _, c := range []struct {
		source, repo, want string
		ok                 bool
	}{
		{connectors.SourceApp, " Example-Org/Widget ", "example-org/widget", true},
		{connectors.SourceApp, "example-org/other", "example-org/other", true},
		{connectors.SourceApp, "", "", false},
		{connectors.SourceApp, "   ", "", false},
		{connectors.SourceStored, "example-org/widget", "", true},
		{connectors.SourceStored, "", "", true},
	} {
		if got, ok := connectors.StandingRepo(c.source, c.repo); got != c.want || ok != c.ok {
			t.Errorf("StandingRepo(%q, %q) = %q, %v; want %q, %v", c.source, c.repo, got, ok, c.want, c.ok)
		}
	}
}

// TestAStandingGrantApprovesWithItsOwnProvenance: a second agent's pending
// request approved from a standing grant is approved against the standing
// grant's credential source, decided_by 'standing-grant' (not 'operator': no
// one decided it), decided_via 'standing', naming the standing grant — and
// re-opening it clears that provenance, as 0043's CHECK requires.
func TestAStandingGrantApprovesWithItsOwnProvenance(t *testing.T) {
	ctx, pool := testPool(t)
	s := newStore(t, pool, key(1))
	a := seedAgent(t, ctx, pool, "standing-a", "tok-standing-a-0123456789")
	b := seedAgent(t, ctx, pool, "standing-b", "tok-standing-b-0123456789")
	origin := approved(t, ctx, s, a, "svc")
	sg, created, err := s.CreateStandingGrant(ctx, "svc", connectors.SourceStored, "", origin.ID)
	if err != nil || !created || sg.OriginGrantID != origin.ID || sg.Repo != "" {
		t.Fatalf("CreateStandingGrant = %+v created=%v err=%v", sg, created, err)
	}
	again, created, err := s.CreateStandingGrant(ctx, "svc", connectors.SourceStored, "", origin.ID)
	if err != nil || created || again.ID != sg.ID {
		t.Fatalf("a second CreateStandingGrant for the same key = %+v created=%v err=%v; want the live one", again, created, err)
	}
	p, _, _ := s.RequestGrant(ctx, b, "svc", "")
	m, err := s.MatchStandingGrant(ctx, b, "svc", connectors.SourceStored, "")
	if err != nil || m.ID != sg.ID {
		t.Fatalf("MatchStandingGrant = %+v %v", m, err)
	}
	g, err := s.ApproveFromStanding(ctx, p.ID, sg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if g.Status != connectors.StatusApproved || g.DecidedBy != connectors.DecidedByStanding ||
		g.DecidedVia.Via != connectors.DecidedViaStanding || g.StandingGrantID != sg.ID || g.Source != connectors.SourceStored {
		t.Fatalf("the automatically approved grant: %+v", g)
	}
	if _, err := s.ApproveFromStanding(ctx, p.ID, sg.ID); !errors.Is(err, connectors.ErrNotPending) {
		t.Fatalf("approving it again = %v, want ErrNotPending", err)
	}
	// 0043's CHECK: 'standing' needs its standing grant, and only on an approved row.
	if _, err := pool.Exec(ctx, `UPDATE connector_grants SET standing_grant_id = NULL WHERE id = $1`, g.ID); err == nil {
		t.Fatal("the CHECK let a 'standing' grant lose its standing grant")
	}
	if _, err := pool.Exec(ctx, `UPDATE connector_grants SET status = 'denied' WHERE id = $1`, g.ID); err == nil {
		t.Fatal("the CHECK let a 'standing' provenance sit on a denied grant")
	}
	r, reopened, err := s.ReopenGrant(ctx, g.ID)
	if err != nil || !reopened || r.Status != connectors.StatusPending || r.StandingGrantID != 0 || r.DecidedVia.Via != "" {
		t.Fatalf("re-opening an automatic approval = %+v reopened=%v err=%v", r, reopened, err)
	}
}

// TestRevokingAStandingGrantRevokesWhatItGave: revoking a standing grant ends
// the automatic approvals AND revokes every grant it gave — the one it approved
// automatically and the approval it was created from — while an approval it
// did not give (another agent approved by hand, not through it) stays. After
// it, nothing matches and a pending approval from it is refused.
func TestRevokingAStandingGrantRevokesWhatItGave(t *testing.T) {
	ctx, pool := testPool(t)
	s := newStore(t, pool, key(1))
	a := seedAgent(t, ctx, pool, "standing-rv-a", "tok-standing-rv-a-0123456789")
	b := seedAgent(t, ctx, pool, "standing-rv-b", "tok-standing-rv-b-0123456789")
	c := seedAgent(t, ctx, pool, "standing-rv-c", "tok-standing-rv-c-0123456789")
	d := seedAgent(t, ctx, pool, "standing-rv-d", "tok-standing-rv-d-0123456789")
	manual := approved(t, ctx, s, c, "svc") // approved by hand, before any standing grant
	origin := approved(t, ctx, s, a, "svc")
	sg, _, err := s.CreateStandingGrant(ctx, "svc", connectors.SourceStored, "", origin.ID)
	if err != nil {
		t.Fatal(err)
	}
	pb, _, _ := s.RequestGrant(ctx, b, "svc", "")
	if _, err := s.ApproveFromStanding(ctx, pb.ID, sg.ID); err != nil {
		t.Fatal(err)
	}
	pd, _, _ := s.RequestGrant(ctx, d, "svc", "") // still pending at the revoke

	rsg, revoked, err := s.RevokeStandingGrant(ctx, sg.ID, "operator")
	if err != nil || rsg.RevokedAt == nil {
		t.Fatalf("RevokeStandingGrant = %+v %v", rsg, err)
	}
	got := map[int64]bool{}
	for _, g := range revoked {
		got[g.ID] = true
	}
	if len(revoked) != 2 || !got[origin.ID] || !got[pb.ID] {
		t.Fatalf("revoked %+v; want exactly the origin %d and the automatic approval %d", revoked, origin.ID, pb.ID)
	}
	for id, want := range map[int64]string{origin.ID: connectors.StatusRevoked, pb.ID: connectors.StatusRevoked,
		manual.ID: connectors.StatusApproved, pd.ID: connectors.StatusPending} {
		if g, _ := s.GetGrant(ctx, id); g.Status != want {
			t.Errorf("grant %d is %s after the revoke; want %s", id, g.Status, want)
		}
	}
	if _, err := s.MatchStandingGrant(ctx, d, "svc", connectors.SourceStored, ""); !errors.Is(err, connectors.ErrNotFound) {
		t.Fatalf("a revoked standing grant still matches: %v", err)
	}
	if _, err := s.ApproveFromStanding(ctx, pd.ID, sg.ID); !errors.Is(err, connectors.ErrNotPending) {
		t.Fatalf("approving from a revoked standing grant = %v, want ErrNotPending", err)
	}
	if _, _, err := s.RevokeStandingGrant(ctx, sg.ID, "operator"); !errors.Is(err, connectors.ErrNotPending) {
		t.Fatalf("revoking it again = %v, want ErrNotPending", err)
	}
	if live, err := s.ListStandingGrants(ctx); err != nil || len(live) != 0 {
		t.Fatalf("live standing grants after the revoke: %+v %v", live, err)
	}
	// A later approval records a NEW standing grant for the key.
	if sg2, created, err := s.CreateStandingGrant(ctx, "svc", connectors.SourceStored, "", manual.ID); err != nil || !created || sg2.ID == sg.ID {
		t.Fatalf("a new standing grant after the revoke = %+v created=%v err=%v", sg2, created, err)
	}
}

// TestAPerAgentRevocationOutlivesTheStandingGrant: the owner revokes ONE
// agent's automatically approved grant on the tab; that agent asking again is
// not approved by the same standing grant (its own "no" stands), while another
// agent still is. The same holds for a denial made after the standing grant.
func TestAPerAgentRevocationOutlivesTheStandingGrant(t *testing.T) {
	ctx, pool := testPool(t)
	s := newStore(t, pool, key(1))
	a := seedAgent(t, ctx, pool, "standing-pa-a", "tok-standing-pa-a-0123456789")
	b := seedAgent(t, ctx, pool, "standing-pa-b", "tok-standing-pa-b-0123456789")
	c := seedAgent(t, ctx, pool, "standing-pa-c", "tok-standing-pa-c-0123456789")
	e := seedAgent(t, ctx, pool, "standing-pa-e", "tok-standing-pa-e-0123456789")
	origin := approved(t, ctx, s, a, "svc")
	sg, _, _ := s.CreateStandingGrant(ctx, "svc", connectors.SourceStored, "", origin.ID)
	pb, _, _ := s.RequestGrant(ctx, b, "svc", "")
	if _, err := s.ApproveFromStanding(ctx, pb.ID, sg.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RevokeGrant(ctx, pb.ID, "operator"); err != nil {
		t.Fatal(err)
	}
	pe, _, _ := s.RequestGrant(ctx, e, "svc", "")
	if _, err := s.DecideGrant(ctx, pe.ID, false, "operator", ""); err != nil {
		t.Fatal(err)
	}
	for name, agent := range map[string]int64{"the revoked agent": b, "the denied agent": e} {
		if _, err := s.MatchStandingGrant(ctx, agent, "svc", connectors.SourceStored, ""); !errors.Is(err, connectors.ErrNotFound) {
			t.Errorf("%s asking again matched the standing grant: %v", name, err)
		}
	}
	if m, err := s.MatchStandingGrant(ctx, c, "svc", connectors.SourceStored, ""); err != nil || m.ID != sg.ID {
		t.Fatalf("control: another agent = %+v %v", m, err)
	}
}

// TestAStandingGrantNeverCrossesOwners: user 1's standing grant never matches
// user 2's agent — under user 2's principal (row-level security), under user
// 1's (the agent is invisible), and under a scope-all System caller, which sees
// every row and is kept to the AGENT's owner only by MatchStandingGrant's
// explicit predicate (RED with `sg.owner_id = (SELECT owner_id FROM agents …)`
// dropped). The CONTROL is user 1's own agent, matched under System. User 2
// can hold its own standing grant for the same key.
func TestAStandingGrantNeverCrossesOwners(t *testing.T) {
	ctx, pool := testPool(t)
	s := newStore(t, pool, key(1))
	sys := authz.System(ctx)
	var two int64
	if err := pool.QueryRow(sys, `INSERT INTO users (role) VALUES ('member') RETURNING id`).Scan(&two); err != nil {
		t.Fatal(err)
	}
	asTwo := authz.WithPrincipal(ctx, authz.Principal{UserID: two, Role: authz.RoleMember, Via: authz.ViaSession})
	var theirs int64
	if err := pool.QueryRow(asTwo, `INSERT INTO agents (name, namespace, hooks_token) VALUES ('standing-two', 'ns-standing-two', 'tok-standing-two-0123456789') RETURNING id`).Scan(&theirs); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := authz.System(context.Background())
		for _, q := range []string{`DELETE FROM connector_grants WHERE owner_id=$1`, `DELETE FROM connector_standing_grants WHERE owner_id=$1`,
			`DELETE FROM agents WHERE owner_id=$1`, `DELETE FROM users WHERE id=$1`} {
			_, _ = pool.Exec(bg, q, two)
		}
	})
	mine := seedAgent(t, ctx, pool, "standing-one", "tok-standing-one-0123456789")
	origin := approved(t, ctx, s, mine, "svc")
	sg, _, err := s.CreateStandingGrant(ctx, "svc", connectors.SourceStored, "", origin.ID)
	if err != nil {
		t.Fatal(err)
	}
	for who, c := range map[string]context.Context{"user 2": asTwo, "user 1": ctx, "System": sys} {
		if m, err := s.MatchStandingGrant(c, theirs, "svc", connectors.SourceStored, ""); !errors.Is(err, connectors.ErrNotFound) {
			t.Errorf("as %s, user 2's agent matched user 1's standing grant: %+v %v", who, m, err)
		}
	}
	if m, err := s.MatchStandingGrant(sys, mine, "svc", connectors.SourceStored, ""); err != nil || m.ID != sg.ID {
		t.Fatalf("control: user 1's own agent under System = %+v %v", m, err)
	}
	// User 2's request, approved FROM user 1's standing grant by id, is refused
	// by the composite foreign key even for System.
	p, _, err := s.RequestGrant(asTwo, theirs, "svc", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApproveFromStanding(sys, p.ID, sg.ID); err == nil {
		t.Fatal("a grant of user 2's was approved from user 1's standing grant")
	}
	if g, _ := s.GetGrant(sys, p.ID); g.Status != connectors.StatusPending {
		t.Fatalf("user 2's request is %s; want pending", g.Status)
	}
	// User 2 holds its own, for the same key.
	pd, err := s.DecideGrant(asTwo, p.ID, true, "operator", connectors.SourceStored)
	if err != nil {
		t.Fatal(err)
	}
	if sg2, created, err := s.CreateStandingGrant(asTwo, "svc", connectors.SourceStored, "", pd.ID); err != nil || !created || sg2.ID == sg.ID {
		t.Fatalf("user 2's own standing grant = %+v created=%v err=%v", sg2, created, err)
	}
	if live, _ := s.ListStandingGrants(asTwo); len(live) != 1 || live[0].ID == sg.ID {
		t.Fatalf("user 2 lists %+v; want only its own", live)
	}
}

// TestDeletingAConnectorDeletesItsStandingGrants: a connector re-created under
// the same name inherits no shared approval.
func TestDeletingAConnectorDeletesItsStandingGrants(t *testing.T) {
	ctx, pool := testPool(t)
	s := newStore(t, pool, key(1))
	a := seedAgent(t, ctx, pool, "standing-del-a", "tok-standing-del-a-0123456789")
	b := seedAgent(t, ctx, pool, "standing-del-b", "tok-standing-del-b-0123456789")
	createGitHub(t, ctx, s)
	origin := approved(t, ctx, s, a, "github")
	if _, _, err := s.CreateStandingGrant(ctx, "github", connectors.SourceStored, "", origin.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteConnector(ctx, "github"); err != nil {
		t.Fatal(err)
	}
	createGitHub(t, ctx, s)
	if live, err := s.ListStandingGrants(ctx); err != nil || len(live) != 0 {
		t.Fatalf("standing grants after the delete: %+v %v", live, err)
	}
	if _, err := s.MatchStandingGrant(ctx, b, "github", connectors.SourceStored, ""); !errors.Is(err, connectors.ErrNotFound) {
		t.Fatalf("a re-created connector inherited a standing grant: %v", err)
	}
}

// TestAStandingGrantUnderOneSourceRetiresTheOther: recording a standing grant
// for the App retires the connector's live stored-token standing grant (and
// the other way round), so only one source's shared approval is ever live.
// Another connector's standing grant is untouched.
func TestAStandingGrantUnderOneSourceRetiresTheOther(t *testing.T) {
	ctx, pool := testPool(t)
	s := newStore(t, pool, key(1))
	a := seedAgent(t, ctx, pool, "standing-src-a", "tok-standing-src-a-0123456789")
	b := seedAgent(t, ctx, pool, "standing-src-b", "tok-standing-src-b-0123456789")
	ga := approved(t, ctx, s, a, "github")
	gs := approved(t, ctx, s, a, "svc")
	stored, _, _ := s.CreateStandingGrant(ctx, "github", connectors.SourceStored, "", ga.ID)
	other, _, _ := s.CreateStandingGrant(ctx, "svc", connectors.SourceStored, "", gs.ID)
	gb := approved(t, ctx, s, b, "github")
	app, created, err := s.CreateStandingGrant(ctx, "github", connectors.SourceApp, "example-org/widget", gb.ID)
	if err != nil || !created {
		t.Fatalf("the App standing grant: %+v %v %v", app, created, err)
	}
	live := map[int64]bool{}
	list, _ := s.ListStandingGrants(ctx)
	for _, sg := range list {
		live[sg.ID] = true
	}
	if live[stored.ID] || !live[app.ID] || !live[other.ID] {
		t.Fatalf("live standing grants %v: want the App one and svc's, not the stored github one %d", live, stored.ID)
	}
	if _, err := s.MatchStandingGrant(ctx, b, "github", connectors.SourceStored, ""); !errors.Is(err, connectors.ErrNotFound) {
		t.Fatalf("the retired stored-token standing grant still matches: %v", err)
	}

	// The other way round: a stored-token standing grant retires the App one.
	stored2, created, err := s.CreateStandingGrant(ctx, "github", connectors.SourceStored, "", ga.ID)
	if err != nil || !created {
		t.Fatalf("a new stored-token standing grant: %+v %v %v", stored2, created, err)
	}
	if _, err := s.MatchStandingGrant(ctx, b, "github", connectors.SourceApp, "example-org/widget"); !errors.Is(err, connectors.ErrNotFound) {
		t.Fatalf("the App standing grant still matches after a stored-token approval: %v", err)
	}
	if m, err := s.MatchStandingGrant(ctx, b, "github", connectors.SourceStored, ""); err != nil || m.ID != stored2.ID {
		t.Fatalf("control: the new stored-token standing grant = %+v %v", m, err)
	}
}

// TestRetiringIsScopedToTheApprovingOwner: a standing grant created by a
// scope-all (System) caller — which row-level security lets see every owner's
// rows — retires only the ORIGIN grant's owner's other-source standing grants:
// another owner's App standing grant for the same connector stays live (RED
// with the retire's `owner_id = …` condition dropped).
func TestRetiringIsScopedToTheApprovingOwner(t *testing.T) {
	ctx, pool := testPool(t)
	s := newStore(t, pool, key(1))
	sys := authz.System(ctx)
	var two int64
	if err := pool.QueryRow(sys, `INSERT INTO users (role) VALUES ('member') RETURNING id`).Scan(&two); err != nil {
		t.Fatal(err)
	}
	asTwo := authz.WithPrincipal(ctx, authz.Principal{UserID: two, Role: authz.RoleMember, Via: authz.ViaSession})
	var theirs int64
	if err := pool.QueryRow(asTwo, `INSERT INTO agents (name, namespace, hooks_token) VALUES ('retire-two', 'ns-retire-two', 'tok-retire-two-0123456789') RETURNING id`).Scan(&theirs); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := authz.System(context.Background())
		for _, q := range []string{`DELETE FROM connector_grants WHERE owner_id=$1`, `DELETE FROM connector_standing_grants WHERE owner_id=$1`,
			`DELETE FROM agents WHERE owner_id=$1`, `DELETE FROM users WHERE id=$1`} {
			_, _ = pool.Exec(bg, q, two)
		}
	})
	gt := approved(t, asTwo, s, theirs, "github")
	theirApp, _, err := s.CreateStandingGrant(asTwo, "github", connectors.SourceApp, "example-org/widget", gt.ID)
	if err != nil {
		t.Fatal(err)
	}
	mine := seedAgent(t, ctx, pool, "retire-one", "tok-retire-one-0123456789")
	gm := approved(t, ctx, s, mine, "github")
	if _, _, err := s.CreateStandingGrant(sys, "github", connectors.SourceStored, "", gm.ID); err != nil {
		t.Fatal(err)
	}
	if live, _ := s.ListStandingGrants(asTwo); len(live) != 1 || live[0].ID != theirApp.ID {
		t.Fatalf("user 2's App standing grant after user 1's stored-token approval (as System): %+v; want it live", live)
	}
}
