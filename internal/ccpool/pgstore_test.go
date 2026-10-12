package ccpool

import (
	"context"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestPGStoreMarksUpsertAndLiveCountsByKindAndStatus runs the store against real
// Postgres (migration 0003's table and columns).
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
	if _, err := pool.Exec(ctx, `DELETE FROM agents`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM cc_account_marks`); err != nil {
		t.Fatal(err)
	}
	st := NewPG(pool)

	if err := st.MarkRateLimited(ctx, "alpha", "resets 5pm", t0); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkAuthFailed(ctx, "alpha", "abc123abc123", "401", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// A second rate-limit UPDATES the row; it must not erase the auth mark.
	if err := st.MarkRateLimited(ctx, "alpha", "resets 9pm", t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	marks, err := st.Marks(ctx)
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
	live, err := st.LiveCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if live["alpha"] != 2 || live["beta"] != 0 || len(live) != 1 {
		t.Fatalf("live = %v, want map[alpha:2]", live)
	}
}
