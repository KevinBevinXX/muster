package agents

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestPGStoreSessionRuntimeAndDefault: a session created on a runtime reads
// back with it; a bad pair is refused before anything is written; an agent with
// no default elects its LEGACY-KEY session (not the latest one), concurrent
// elections agree, and moving the default leaves exactly one.
func TestPGStoreSessionRuntimeAndDefault(t *testing.T) {
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
	name := "rt-" + time.Now().Format("150405.000000")
	a, err := store.Create(ctx, Agent{Name: name, Namespace: "devpod-" + name, Status: StatusProvisioning})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupAgent(ctx, pool, a.ID)

	legacy, err := store.LatestOrCreateSession(ctx, a.ID, a.Name)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.SessionKey != legacySessionKey(a.Name) || legacy.Runtime != "" || legacy.Model != "" {
		t.Fatalf("the first session: %+v", legacy)
	}
	cc, err := store.CreateSessionOn(ctx, a.ID, a.Name, SessionRuntimeClaudeCode, " opus ")
	if err != nil {
		t.Fatal(err)
	}
	gw, err := store.CreateSessionOn(ctx, a.ID, a.Name, SessionRuntimeGateway, "openrouter/vendor-x/model-y:free")
	if err != nil {
		t.Fatal(err)
	}
	if cc.Runtime != "claude-code" || cc.Model != "opus" || cc.IsDefault {
		t.Errorf("claude-code session read back as %+v", cc)
	}
	if got, err := store.GetSession(ctx, gw.ID); err != nil || got.Runtime != "gateway" || got.Model != "openrouter/vendor-x/model-y:free" {
		t.Errorf("gateway session read back as %+v, %v", got, err)
	}
	if got, err := store.SessionByKey(ctx, a.ID, cc.SessionKey); err != nil || got.ID != cc.ID {
		t.Errorf("SessionByKey(%q) = %+v, %v", cc.SessionKey, got, err)
	}
	if _, err := store.SessionByKey(ctx, a.ID+1_000_000, cc.SessionKey); err == nil {
		t.Errorf("SessionByKey found the session under another agent's id")
	}
	for _, bad := range []struct{ runtime, model string }{
		{"", ""}, {"gateway", "not-a-slug"}, {"claude-code", "opus; rm -rf /"}, {"webchat", "opus"},
	} {
		before, _ := store.ListSessions(ctx, a.ID)
		if s, err := store.CreateSessionOn(ctx, a.ID, a.Name, bad.runtime, bad.model); err == nil {
			t.Errorf("CreateSessionOn(%q, %q) was accepted: %+v", bad.runtime, bad.model, s)
		}
		if after, _ := store.ListSessions(ctx, a.ID); len(after) != len(before) {
			t.Errorf("a refused CreateSessionOn(%q, %q) still wrote a row", bad.runtime, bad.model)
		}
	}

	// gw is now the LATEST session; the election must still pick the legacy one.
	var wg sync.WaitGroup
	got := make([]ChatSession, 6)
	errs := make([]error, 6)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i], errs[i] = store.DefaultSession(ctx, a.ID, a.Name)
		}(i)
	}
	wg.Wait()
	for i := range got {
		if errs[i] != nil || got[i].ID != legacy.ID || !got[i].IsDefault {
			t.Errorf("election %d: %+v, %v; want the legacy session %d", i, got[i], errs[i], legacy.ID)
		}
	}

	if err := store.SetDefaultSession(ctx, a.ID, gw.ID); err != nil {
		t.Fatal(err)
	}
	if d, err := store.DefaultSession(ctx, a.ID, a.Name); err != nil || d.ID != gw.ID {
		t.Errorf("after SetDefaultSession(gw), default %+v, %v", d, err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM chat_sessions WHERE agent_id=$1 AND is_default`, a.ID).Scan(&n); err != nil || n != 1 {
		t.Errorf("%d defaults after a move, want 1 (%v)", n, err)
	}
	other, err := store.Create(ctx, Agent{Name: name + "-b", Namespace: "devpod-" + name + "-b", Status: StatusProvisioning})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupAgent(ctx, pool, other.ID)
	if err := store.SetDefaultSession(ctx, other.ID, cc.ID); err == nil {
		t.Errorf("SetDefaultSession accepted another agent's session")
	}
	if d, _ := store.DefaultSession(ctx, a.ID, a.Name); d.ID != gw.ID {
		t.Errorf("a refused SetDefaultSession moved the default to %d", d.ID)
	}

	// 🔴 THE OWNER CHECK, REACHED. The refusal above is the unique index's, not
	// the owner check's: a already HAS a default, so marking another of its
	// sessions default violates one-default-per-agent whatever the check says.
	// Here the session's agent (c) has NO default yet, so nothing but the owner
	// check stands between SetDefaultSession(other, c's session) and making it
	// c's default — a write to another agent's sessions under this agent's id.
	c, err := store.Create(ctx, Agent{Name: name + "-c", Namespace: "devpod-" + name + "-c", Status: StatusProvisioning})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupAgent(ctx, pool, c.ID)
	cSess, err := store.CreateSessionOn(ctx, c.ID, c.Name, SessionRuntimeGateway, "openrouter/vendor-x/model-y")
	if err != nil {
		t.Fatal(err)
	}
	err = store.SetDefaultSession(ctx, other.ID, cSess.ID)
	if err == nil || !strings.Contains(err.Error(), "is not agent") {
		t.Errorf("SetDefaultSession(other, c's session) = %v; want the owner check's refusal", err)
	}
	var cDefaults int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM chat_sessions WHERE agent_id=$1 AND is_default`, c.ID).Scan(&cDefaults); err != nil || cDefaults != 0 {
		t.Errorf("c has %d default sessions after another agent's refused SetDefaultSession (%v); want 0", cDefaults, err)
	}
}

// TestPGStoreDefaultSessionCreatesOneForASessionlessAgent: an agent with no
// session gets a legacy one, and it is the default.
func TestPGStoreDefaultSessionCreatesOneForASessionlessAgent(t *testing.T) {
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 30*time.Second)
	defer cancel()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool, nil); err != nil {
		t.Fatal(err)
	}
	store := NewPG(pool)
	name := "rt-empty-" + time.Now().Format("150405.000000")
	a, err := store.Create(ctx, Agent{Name: name, Namespace: "devpod-" + name, Status: StatusProvisioning})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupAgent(ctx, pool, a.ID)
	d, err := store.DefaultSession(ctx, a.ID, a.Name)
	if err != nil || !d.IsDefault || d.SessionKey != legacySessionKey(a.Name) || d.Runtime != "" {
		t.Fatalf("DefaultSession on a sessionless agent: %+v, %v", d, err)
	}
}
