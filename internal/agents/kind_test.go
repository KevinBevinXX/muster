package agents

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestKindsMatchTheMigrationsCheck pins Kinds to the set the NEWEST migration
// that re-creates agents_kind_check admits (0003 created it, 0037 widened it for
// hermes), so a kind added in Go and not in the schema (or the reverse) reddens
// here instead of as a CHECK violation at a dispatch.
func TestKindsMatchTheMigrationsCheck(t *testing.T) {
	files, err := filepath.Glob("../db/migrations/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("instrument check: no migrations found (%v)", err)
	}
	sort.Strings(files)
	check := regexp.MustCompile(`ADD CONSTRAINT agents_kind_check\s+CHECK \(kind IN \(([^)]*)\)\)`)
	var newest string
	var m []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if got := check.FindStringSubmatch(string(b)); got != nil {
			newest, m = f, got
		}
	}
	if m == nil {
		t.Fatal("instrument check: no migration re-creates agents_kind_check with an IN-list")
	}
	if !strings.HasSuffix(newest, "0037_agent_kind_hermes.sql") {
		t.Fatalf("the newest agents_kind_check is in %s; update this test's expectation with the migration that moved it", newest)
	}
	var sqlKinds []string
	for _, p := range strings.Split(m[1], ",") {
		sqlKinds = append(sqlKinds, strings.Trim(strings.TrimSpace(p), "'"))
	}
	if strings.Join(sqlKinds, ",") != strings.Join(Kinds, ",") {
		t.Fatalf("migration %s admits %v, Kinds is %v", newest, sqlKinds, Kinds)
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
	// Likewise five minutes past hermesd's own HERMESD_TURN_TIMEOUT default.
	if KindTurnTimeout("hermes") != 35*time.Minute || KindLabel("hermes") != "Hermes" || !ValidKind("hermes") {
		t.Fatalf("hermes: turn budget %s, label %q, valid %t", KindTurnTimeout("hermes"), KindLabel("hermes"), ValidKind("hermes"))
	}
	for kind, want := range map[string]bool{"": false, "gateway": false, "claude-code": true, "hermes": true, "teleport": false} {
		if RunsPodSupervisor(kind) != want {
			t.Fatalf("RunsPodSupervisor(%q) = %t", kind, !want)
		}
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
