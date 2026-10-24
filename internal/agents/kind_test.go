package agents

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestKindsMatchTheMigrationsCheck pins Kinds + RetiredKinds to the set the
// NEWEST migration that re-creates agents_kind_check admits (0003 created it,
// 0038 widened it for the since-removed hermes kind), so a kind added in Go and
// not in the schema (or the reverse) reddens here instead of as a CHECK
// violation at a dispatch — and a kind retired in Go keeps its rows admitted.
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
	if !strings.HasSuffix(newest, "0038_agent_kind_hermes.sql") {
		t.Fatalf("the newest agents_kind_check is in %s; update this test's expectation with the migration that moved it", newest)
	}
	var sqlKinds []string
	for _, p := range strings.Split(m[1], ",") {
		sqlKinds = append(sqlKinds, strings.Trim(strings.TrimSpace(p), "'"))
	}
	known := append(append([]string{}, Kinds...), RetiredKinds...)
	if strings.Join(sqlKinds, ",") != strings.Join(known, ",") {
		t.Fatalf("migration %s admits %v, Kinds+RetiredKinds is %v", newest, sqlKinds, known)
	}
	for _, k := range RetiredKinds {
		if slices.Contains(Kinds, k) {
			t.Fatalf("kind %q is both offered (Kinds) and retired (RetiredKinds)", k)
		}
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
	for kind, want := range map[string]bool{"": false, "gateway": false, "claude-code": true, "hermes": false, "teleport": false} {
		if RunsPodSupervisor(kind) != want {
			t.Fatalf("RunsPodSupervisor(%q) = %t", kind, !want)
		}
	}
}

// The hermes kind is REMOVED: not valid, not a pod-supervisor kind, labelled as
// removed, and retired. Red on the commit before its removal, where it was a
// live kind (ValidKind true, label "Hermes").
func TestTheHermesKindIsRetired(t *testing.T) {
	if ValidKind("hermes") || slices.Contains(Kinds, "hermes") {
		t.Fatalf("hermes is still an offered kind: ValidKind %t, Kinds %v", ValidKind("hermes"), Kinds)
	}
	if !IsRetiredKind("hermes") || IsRetiredKind(KindClaudeCode) || IsRetiredKind(KindGateway) || IsRetiredKind("") {
		t.Fatal("IsRetiredKind")
	}
	if KindLabel("hermes") != "Hermes (removed)" || KindTurnTimeout("hermes") != 0 {
		t.Fatalf("hermes: label %q, turn budget %s", KindLabel("hermes"), KindTurnTimeout("hermes"))
	}
}

// A claude-code agent dispatched with a repository waits for its checkout;
// without one, as a gateway agent or as a legacy hermes row, it never does.
func TestNeedsCheckoutCoversOnlyThePodSupervisorKind(t *testing.T) {
	for _, c := range []struct {
		kind, repo string
		want       bool
	}{
		{RetiredKindHermes, "o/r", false}, {KindClaudeCode, "o/r", true}, {KindGateway, "o/r", false}, {KindClaudeCode, "", false},
	} {
		if got := NeedsCheckout(Agent{Kind: c.kind, Repo: c.repo}); got != c.want {
			t.Fatalf("NeedsCheckout(%s, %q) = %t, want %t", c.kind, c.repo, got, c.want)
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

	// A LEGACY hermes row (0038 still admits the retired kind): stored with no
	// account, read back and listed, and refused WITH one (0003's
	// agents_cc_account_matches_kind holds for it as for gateway).
	hermes, err := store.Create(ctx, Agent{Name: "kind-hermes-" + stamp, Namespace: "ns", Status: StatusProvisioning, Kind: RetiredKindHermes})
	if err != nil {
		t.Fatalf("create hermes: %v", err)
	}
	defer cleanupAgent(ctx, pool, hermes.ID)
	if got, err := store.Get(ctx, hermes.ID); err != nil || got.Kind != RetiredKindHermes || got.CCAccount != "" {
		t.Fatalf("hermes round trip: %+v, %v", got, err)
	}
	all, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list with a legacy hermes row: %v", err)
	}
	if !slices.ContainsFunc(all, func(a Agent) bool { return a.ID == hermes.ID && a.Kind == RetiredKindHermes }) {
		t.Fatal("the legacy hermes row is missing from List")
	}
	if bad, err := store.Create(ctx, Agent{Name: "kind-hermes-acct-" + stamp, Namespace: "ns", Status: StatusProvisioning, Kind: RetiredKindHermes, CCAccount: "work"}); err == nil {
		cleanupAgent(ctx, pool, bad.ID)
		t.Fatal("a hermes row naming a Claude account was stored")
	}
}
