package agents

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestKindsMatchTheMigrationsCheck pins Kinds to the set migration 0003's
// agents_kind_check admits, so a kind added in Go and not in the schema (or the
// reverse) reddens here instead of as a CHECK violation at a dispatch.
func TestKindsMatchTheMigrationsCheck(t *testing.T) {
	b, err := os.ReadFile("../db/migrations/0003_agent_kinds.sql")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`CHECK \(kind IN \(([^)]*)\)\)`).FindStringSubmatch(string(b))
	if m == nil {
		t.Fatal("instrument check: agents_kind_check's IN-list was not found in 0003")
	}
	var sqlKinds []string
	for _, p := range strings.Split(m[1], ",") {
		sqlKinds = append(sqlKinds, strings.Trim(strings.TrimSpace(p), "'"))
	}
	if strings.Join(sqlKinds, ",") != strings.Join(Kinds, ",") {
		t.Fatalf("migration admits %v, Kinds is %v", sqlKinds, Kinds)
	}
}

func TestKindResolutionAndTurnBudgets(t *testing.T) {
	if ResolveKind("") != "gateway" || ResolveKind("claude-code") != "claude-code" {
		t.Fatal("ResolveKind")
	}
	if !ValidKind("") || !ValidKind("claude-code") || ValidKind("teleport") {
		t.Fatal("ValidKind")
	}
	if KindTurnTimeout("") != 0 || KindTurnTimeout("gateway") != 0 {
		t.Fatal("the gateway kind must keep the gateway's own client budget (0 = unchanged)")
	}
	// 35m: five minutes past ccd's own CCD_TURN_TIMEOUT default (30m), so ccd's
	// typed 504 turn_timeout always arrives before muster's client gives up.
	if KindTurnTimeout("claude-code") != 35*time.Minute {
		t.Fatalf("claude-code turn budget = %s, want 35m", KindTurnTimeout("claude-code"))
	}
}

func TestPGStoreKindAndAccountRoundTrip(t *testing.T) {
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
	store := NewPG(pool)
	stamp := time.Now().Format("150405.000000")

	cc, err := store.Create(ctx, Agent{Name: "kind-cc-" + stamp, Namespace: "ns", Status: StatusProvisioning, Kind: KindClaudeCode, CCAccount: "work"})
	if err != nil {
		t.Fatalf("create claude-code: %v", err)
	}
	defer cleanupAgent(ctx, pool, cc.ID)
	plain, err := store.Create(ctx, Agent{Name: "kind-gw-" + stamp, Namespace: "ns", Status: StatusProvisioning})
	if err != nil {
		t.Fatalf("create kindless: %v", err)
	}
	defer cleanupAgent(ctx, pool, plain.ID)

	got, err := store.Get(ctx, cc.ID)
	if err != nil || got.Kind != KindClaudeCode || got.CCAccount != "work" {
		t.Fatalf("claude-code round trip: %+v, %v", got, err)
	}
	got, err = store.Get(ctx, plain.ID)
	if err != nil || got.Kind != KindGateway || got.CCAccount != "" {
		t.Fatalf("kindless round trip: kind %q account %q, %v", got.Kind, got.CCAccount, err)
	}
}
