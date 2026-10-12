package ccpool

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestPGStoreMarksUpsertAndLiveCountsByKindAndStatus runs the store against real
// Postgres (migration 0003's table and columns, per owner since 0011).
//
// Owner 2 (a member) has an account with the SAME NAME as the admin's
// ("alpha") and live agents on it: the admin's marks and counts must not see
// them, and theirs must not see the admin's — read under a USER principal and
// under authz.System alike (#64's audit note: LiveCounts used to count every
// owner's agents when the caller's scope was all).
func TestPGStoreMarksUpsertAndLiveCountsByKindAndStatus(t *testing.T) {
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 30*time.Second)
	defer cancel()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// This package's database is private to it (dbtest), so clearing it is safe
	// and makes the counts below exact.
	if _, err := pool.Exec(authz.System(ctx), `DELETE FROM agents`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(authz.System(ctx), `DELETE FROM cc_account_marks`); err != nil {
		t.Fatal(err)
	}
	st := NewPG(pool)
	dbtest.OwnerExec(t, dsn, `INSERT INTO users (id, role) OVERRIDING SYSTEM VALUE VALUES (2, 'member') ON CONFLICT (id) DO NOTHING`)
	member := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 2, Role: authz.RoleMember, Via: authz.ViaSession})
	sys := authz.System(context.Background())

	if err := st.MarkRateLimited(ctx, 1, "alpha", "resets 5pm", t0); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkAuthFailed(ctx, 1, "alpha", "abc123abc123", "401", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// A second rate-limit UPDATES the row; it must not erase the auth mark.
	if err := st.MarkRateLimited(ctx, 1, "alpha", "resets 9pm", t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// The member's own "alpha": a different account.
	if err := st.MarkRateLimited(member, 2, "alpha", "member limit", t0.Add(5*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// A member cannot write a mark against the admin's account (row-level
	// security's WITH CHECK).
	if err := st.MarkRateLimited(member, 1, "alpha", "forged", t0); err == nil {
		t.Fatal("a member's principal wrote a mark against owner 1's account")
	}
	for _, c := range []struct {
		name string
		ctx  context.Context
	}{{"operator", ctx}, {"system", sys}} {
		mm, err := st.Marks(c.ctx, 2)
		if c.name == "operator" {
			if err != nil || len(mm) != 0 {
				t.Fatalf("the operator's principal read owner 2's marks: %v %v", mm, err)
			}
			continue
		}
		if err != nil || mm["alpha"].RateLimitedDetail != "member limit" {
			t.Fatalf("%s: owner 2's alpha = %+v, %v", c.name, mm["alpha"], err)
		}
	}
	if sm, err := st.Marks(sys, 1); err != nil || sm["alpha"].RateLimitedDetail != "resets 9pm" {
		t.Fatalf("system: owner 1's alpha = %+v, %v (another owner's same-named mark leaked in?)", sm["alpha"], err)
	}
	marks, err := st.Marks(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	m := marks["alpha"]
	if !m.RateLimitedAt.Equal(t0.Add(2*time.Hour)) || m.RateLimitedDetail != "resets 9pm" ||
		!m.AuthFailedAt.Equal(t0.Add(time.Hour)) || m.AuthFailedToken != "abc123abc123" || m.AuthFailedDetail != "401" {
		t.Fatalf("alpha's mark = %+v", m)
	}
	if _, ok := marks["beta"]; ok {
		t.Fatal("an unmarked account has a row")
	}

	// Live counts: only claude-code agents in a live status, per account.
	as := agents.NewPG(pool)
	stamp := time.Now().Format("150405.000000")
	mk := func(name, kind, account, status string) {
		t.Helper()
		if _, err := as.Create(ctx, agents.Agent{Name: name + "-" + stamp, Namespace: "ns", Kind: kind, CCAccount: account, Status: status}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	mk("lc-a", agents.KindClaudeCode, "alpha", agents.StatusRunning)
	mk("lc-b", agents.KindClaudeCode, "alpha", agents.StatusProvisioning)
	mk("lc-c", agents.KindClaudeCode, "alpha", agents.StatusStopped) // owns, not live
	mk("lc-d", agents.KindClaudeCode, "beta", agents.StatusError)    // owns, not live
	mk("lc-e", agents.KindGateway, "", agents.StatusRunning)         // other kind
	// The member's three live agents on ITS "alpha".
	for i, st := range []string{agents.StatusRunning, agents.StatusPending, agents.StatusRunning} {
		if _, err := as.Create(member, agents.Agent{Name: "lc-m" + strconv.Itoa(i) + "-" + stamp, Namespace: "ns",
			Kind: agents.KindClaudeCode, CCAccount: "alpha", Status: st}); err != nil {
			t.Fatalf("create member agent: %v", err)
		}
	}
	for _, c := range []struct {
		name string
		ctx  context.Context
	}{{"operator", ctx}, {"system", sys}} {
		live, err := st.LiveCounts(c.ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		if live["alpha"] != 2 || live["beta"] != 0 || len(live) != 1 {
			t.Fatalf("%s: owner 1's live = %v, want map[alpha:2] (the member's 3 on its own alpha must not count)", c.name, live)
		}
	}
	for _, c := range []struct {
		name string
		ctx  context.Context
	}{{"member", member}, {"system", sys}} {
		live, err := st.LiveCounts(c.ctx, 2)
		if err != nil {
			t.Fatal(err)
		}
		if live["alpha"] != 3 || len(live) != 1 {
			t.Fatalf("%s: owner 2's live = %v, want map[alpha:3]", c.name, live)
		}
	}
}
