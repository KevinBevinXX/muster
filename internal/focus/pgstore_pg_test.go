package focus

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// Users in this package's scratch database: 1 is the seeded operator; 2 and 3
// are members.
func pgStore(t *testing.T) (*PGStore, *pgxpool.Pool) {
	t.Helper()
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(dbtest.AsOperator(ctx), pool, log.New(io.Discard, "", 0)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	dbtest.OwnerExec(t, dsn, `INSERT INTO users (id, role) OVERRIDING SYSTEM VALUE VALUES (2, 'member'), (3, 'member') ON CONFLICT (id) DO NOTHING`)
	dbtest.OwnerExec(t, dsn, `TRUNCATE agent_focus`)
	dbtest.OwnerExec(t, dsn, `DELETE FROM agents WHERE name LIKE 'focus-%'`)
	return NewPG(pool), pool
}

func as(uid int64) context.Context {
	role := authz.RoleMember
	if uid == authz.OperatorUserID {
		role = authz.RoleAdmin
	}
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: role, Via: authz.ViaSession})
}

func mustAgent(t *testing.T, pool *pgxpool.Pool, uid int64, name string) agents.Agent {
	t.Helper()
	a, err := agents.NewPG(pool).Create(as(uid), agents.Agent{Name: name, Namespace: "devpod-" + name, DisplayName: name,
		Status: agents.StatusRunning, HooksToken: "hooks-" + name})
	if err != nil {
		t.Fatalf("agent %s: %v", name, err)
	}
	return a
}

func TestUpsertGetAndRecordError(t *testing.T) {
	s, pool := pgStore(t)
	a := mustAgent(t, pool, 2, "focus-up")
	sub := AgentSubject(a.ID)
	if _, ok, err := s.Get(as(2), sub); err != nil || ok {
		t.Fatalf("before: %v %v", ok, err)
	}
	f, err := s.Upsert(as(2), sub, Write{Title: "Ship it", Description: "Release prep.", Source: SourceSummarizer,
		Fingerprint: Fingerprint("tail")})
	if err != nil || f.Title != "Ship it" || f.Owner != 2 {
		t.Fatalf("upsert: %+v %v", f, err)
	}
	// A second write is an update of the same row, not a second row.
	if _, err := s.Upsert(as(2), sub, Write{Title: "Ship it now", Source: SourceSummarizer}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordError(as(2), sub, Reason(&HTTPError{Status: 500})); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Get(as(2), sub)
	if err != nil || !ok || got.Title != "Ship it now" || got.Source != SourceSummarizer || got.LastError == "" || got.ErrorAt.IsZero() {
		t.Fatalf("after error: %+v %v %v", got, ok, err)
	}
	all, _ := s.List(as(2))
	if len(all) != 1 {
		t.Fatalf("%d rows, want 1", len(all))
	}
	// A recorded error on a subject with no focus yet makes a row that has none.
	m := MirrorSubject("0b2c3d4e-5f60-4718-8a9b-0c1d2e3f4a5b")
	if err := s.RecordError(as(2), m, "no transcript"); err != nil {
		t.Fatal(err)
	}
	if got, ok, _ := s.Get(as(2), m); !ok || got.Has() {
		t.Fatalf("error-only row: %+v %v", got, ok)
	}
	// An empty title is refused before the database.
	if _, err := s.Upsert(as(2), m, Write{Title: "  ", Source: SourceSummarizer}); !errors.Is(err, ErrEmpty) {
		t.Fatalf("empty title: %v", err)
	}
}

// TestFocusIsPerOwner: another user neither reads nor overwrites your focus,
// and cannot write a focus for your agent.
func TestFocusIsPerOwner(t *testing.T) {
	s, pool := pgStore(t)
	a := mustAgent(t, pool, 2, "focus-own")
	sub := AgentSubject(a.ID)
	if _, err := s.Upsert(as(2), sub, Write{Title: "Mine", Source: SourceSummarizer}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Get(as(3), sub); ok {
		t.Fatal("user 3 reads user 2's focus")
	}
	if all, _ := s.List(as(3)); len(all) != 0 {
		t.Fatalf("user 3 lists user 2's focus: %+v", all)
	}
	if hits, _ := s.Search(as(3), "mine", nil, 10); len(hits) != 0 {
		t.Fatalf("user 3 searches user 2's focus: %+v", hits)
	}
	if _, err := s.Upsert(as(3), sub, Write{Title: "Theirs", Source: SourceSummarizer}); err == nil {
		t.Fatal("user 3 wrote a focus for user 2's agent")
	}
	if got, _, _ := s.Get(as(2), sub); got.Title != "Mine" {
		t.Fatalf("user 2's focus became %q", got.Title)
	}
	// The focus dies with its agent.
	if err := agents.NewPG(pool).Delete(as(2), a.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Get(as(2), sub); ok {
		t.Fatal("the focus outlived its agent")
	}
}

// TestSearchRanksTheRightFocusFirst: full-text search over distinct fixtures —
// each query's own fixture ranks first, a title match outranks a description
// match, and a non-matching query returns nothing.
func TestSearchRanksTheRightFocusFirst(t *testing.T) {
	s, _ := pgStore(t)
	fixtures := map[string]Write{
		"mirror-billing":  {Title: "Stripe invoice reconciliation", Description: "Matching payouts against the ledger export.", Source: SourceSummarizer},
		"mirror-kernel":   {Title: "Kernel upgrade on the GPU node", Description: "Draining workloads before the reboot.", Source: SourceSummarizer},
		"mirror-docs":     {Title: "Docs pass on the mirroring page", Description: "Rewording the invoice example and the kernel note.", Source: SourceSummarizer},
		"mirror-unrelate": {Title: "Lunch order", Description: "Sandwiches.", Source: SourceSummarizer},
	}
	for key, w := range fixtures {
		if _, err := s.Upsert(as(2), MirrorSubject(key), w); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		q, first string
		n        int
	}{
		{"invoice", "mirror-billing", 2},
		{"kernel upgrade", "mirror-kernel", 1},
		{"mirroring", "mirror-docs", 1},
	} {
		hits, err := s.Search(as(2), c.q, searchLive, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != c.n || hits[0].Subject.Key != c.first {
			t.Fatalf("%q: %+v, want %d hits led by %s", c.q, hits, c.n, c.first)
		}
		if len([]rune(hits[0].Snippet)) > MaxSnippetRunes {
			t.Fatalf("snippet too long: %q", hits[0].Snippet)
		}
	}
	if len(mustSearch(t, s, "invoice")) < 2 || mustSearch(t, s, "invoice")[0].Rank <= mustSearch(t, s, "invoice")[1].Rank {
		t.Fatal("the title match does not outrank the description match")
	}
	for _, q := range []string{"zeppelin", "the", ""} {
		if hits := mustSearch(t, s, q); len(hits) != 0 {
			t.Fatalf("%q matched %+v", q, hits)
		}
	}
}

// searchLive is TestSearchRanksTheRightFocusFirst's mirrored sessions, all live.
var searchLive = []string{"mirror-billing", "mirror-kernel", "mirror-docs", "mirror-unrelate"}

// 🔴 TestSearchLeavesOutDeadMirrorsBeforeTheLimit: more than the limit of DEAD
// mirrored rows outranking a live session and an agent do not crowd them out.
// Filtering after the LIMIT returned nothing at all here.
func TestSearchLeavesOutDeadMirrorsBeforeTheLimit(t *testing.T) {
	s, pool := pgStore(t)
	a := mustAgent(t, pool, 2, "focus-crowded")
	for i := 0; i < MaxSearchResults+5; i++ {
		if _, err := s.Upsert(as(2), MirrorSubject(fmt.Sprintf("0b2c3d4e-dead-%02d", i)),
			Write{Title: "Kernel upgrade kernel rollout", Source: SourceSummarizer}); err != nil {
			t.Fatal(err)
		}
	}
	live := MirrorSubject("0b2c3d4e-live")
	if _, err := s.Upsert(as(2), live, Write{Title: "Lunch", Description: "and a kernel note", Source: SourceSummarizer}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Upsert(as(2), AgentSubject(a.ID), Write{Title: "Errands", Description: "a kernel note too", Source: SourceSummarizer}); err != nil {
		t.Fatal(err)
	}
	hits, err := s.Search(as(2), "kernel", []string{live.Key}, MaxSearchResults)
	if err != nil {
		t.Fatal(err)
	}
	got := map[Subject]bool{}
	for _, h := range hits {
		got[h.Subject] = true
		if h.Subject.Kind == KindMirror && h.Subject != live {
			t.Errorf("a dead mirrored session is a hit: %s", h.Subject)
		}
	}
	if len(hits) != 2 || !got[live] || !got[AgentSubject(a.ID)] {
		t.Fatalf("hits %+v, want exactly the live session and the agent", hits)
	}
}

func mustSearch(t *testing.T, s *PGStore, q string) []Hit {
	t.Helper()
	hits, err := s.Search(as(2), q, searchLive, 10)
	if err != nil {
		t.Fatal(err)
	}
	return hits
}

func TestOwnersIsEveryOwnerWithSomethingToDescribe(t *testing.T) {
	s, pool := pgStore(t)
	mustAgent(t, pool, 3, "focus-owners")
	owners, err := s.Owners(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, o := range owners {
		found = found || o == 3
	}
	if !found {
		t.Fatalf("owners %v lack 3", owners)
	}
}

// TestOwnersSkipsDisabledOwners: a disabled user's sessions are not refreshed.
func TestOwnersSkipsDisabledOwners(t *testing.T) {
	s, pool := pgStore(t)
	mustAgent(t, pool, 3, "focus-disabled")
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE users SET disabled_at = now() WHERE id = 3`)
	t.Cleanup(func() { dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE users SET disabled_at = NULL WHERE id = 3`) })
	owners, err := s.Owners(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range owners {
		if o == 3 {
			t.Fatalf("owners %v include the disabled user 3", owners)
		}
	}
	mustAgent(t, pool, 2, "focus-enabled")
	if owners, _ = s.Owners(context.Background()); !containsOwner(owners, 2) {
		t.Fatalf("positive control: owners %v lack the enabled user 2", owners)
	}
}

func containsOwner(owners []int64, o int64) bool {
	for _, x := range owners {
		if x == o {
			return true
		}
	}
	return false
}

// TestTouchAndPruneKeepSeenMirrorsAndAgents: PruneAll deletes EVERY owner's
// mirror rows last seen before the cut — a DISABLED owner's included, which no
// owner pass reaches — and never a row Touch just marked, never an agent's.
func TestTouchAndPruneKeepSeenMirrorsAndAgents(t *testing.T) {
	s, pool := pgStore(t)
	a := mustAgent(t, pool, 2, "focus-prune")
	live, dead := MirrorSubject("0b2c3d4e-live"), MirrorSubject("0b2c3d4e-dead")
	for _, sub := range []Subject{live, dead, AgentSubject(a.ID)} {
		if _, err := s.Upsert(as(2), sub, Write{Title: "t " + sub.Key, Source: SourceSummarizer}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Upsert(as(3), dead, Write{Title: "other owner", Source: SourceSummarizer}); err != nil {
		t.Fatal(err)
	}
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE agent_focus SET seen_at = now() - interval '10 days'`)
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE users SET disabled_at = now() WHERE id = 3`)
	t.Cleanup(func() { dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE users SET disabled_at = NULL WHERE id = 3`) })
	if err := s.Touch(as(2), []Subject{live}); err != nil {
		t.Fatal(err)
	}
	n, err := s.PruneAll(context.Background(), time.Now().Add(-7*24*time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("pruned %d (%v), want 2 (owner 2's dead row and disabled owner 3's)", n, err)
	}
	for sub, want := range map[Subject]bool{live: true, dead: false, AgentSubject(a.ID): true} {
		if _, ok, _ := s.Get(as(2), sub); ok != want {
			t.Errorf("%s present=%v, want %v", sub, ok, want)
		}
	}
	if _, ok, _ := s.Get(as(3), dead); ok {
		t.Error("the disabled owner's stale row was kept")
	}
}
