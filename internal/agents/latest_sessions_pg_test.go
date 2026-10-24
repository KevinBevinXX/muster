package agents

import (
	"context"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestPGStoreLatestSessionIDsIsWhatLatestOrCreateSessionPicks: per agent, the
// id LatestSessionIDs returns is the session LatestOrCreateSession returns —
// the session a bare GET /agents/{name} redirects to, which every in-app link
// now names directly (ui.AgentSessionHref). The fixture makes the OLDER session
// the most recently updated, so "latest" and "newest row" disagree; an agent
// with no session is ABSENT (and nothing is created for it); an unrequested
// agent is absent; an empty ids slice is an empty map.
func TestPGStoreLatestSessionIDsIsWhatLatestOrCreateSessionPicks(t *testing.T) {
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
	tag := time.Now().Format("150405.000000")
	var created []int64
	defer func() { // before pool.Close (see TestPGStoreLastMessageByAgentIDs)
		for _, id := range created {
			cleanupAgent(ctx, pool, id)
		}
	}()
	mk := func(suffix string) Agent {
		a, err := store.Create(ctx, Agent{Name: "lsi-" + suffix + "-" + tag, Namespace: "devpod-lsi-" + suffix + "-" + tag, Status: StatusRunning})
		if err != nil {
			t.Fatalf("create agent %s: %v", suffix, err)
		}
		created = append(created, a.ID)
		return a
	}
	two, none, unasked := mk("two"), mk("none"), mk("unasked")
	older, err := store.CreateSession(ctx, two.ID, two.Name)
	if err != nil {
		t.Fatal(err)
	}
	newer, err := store.CreateSession(ctx, two.ID, two.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateSession(ctx, unasked.ID, unasked.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE chat_sessions SET updated_at = now() + interval '1 hour' WHERE id = $1`, older.ID); err != nil {
		t.Fatal(err)
	}

	got, err := store.LatestSessionIDs(ctx, []int64{two.ID, none.ID})
	if err != nil {
		t.Fatalf("LatestSessionIDs: %v", err)
	}
	if got[two.ID] != older.ID || got[two.ID] == newer.ID {
		t.Errorf("agent with two sessions → %d, want the most recently UPDATED one %d (not the newest row %d)", got[two.ID], older.ID, newer.ID)
	}
	if _, ok := got[none.ID]; ok {
		t.Errorf("an agent with no session is in the map: %v", got)
	}
	if _, ok := got[unasked.ID]; ok {
		t.Errorf("an unrequested agent is in the map: %v", got)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM chat_sessions WHERE agent_id = $1`, none.ID).Scan(&n); err != nil || n != 0 {
		t.Errorf("the read created %d session(s) for the agent that had none (err %v)", n, err)
	}
	// The same answer as the redirect's own read.
	if l, err := store.LatestOrCreateSession(ctx, two.ID, two.Name); err != nil || l.ID != got[two.ID] {
		t.Errorf("LatestOrCreateSession → %d (%v), LatestSessionIDs → %d", l.ID, err, got[two.ID])
	}
	if empty, err := store.LatestSessionIDs(ctx, nil); err != nil || len(empty) != 0 {
		t.Errorf("LatestSessionIDs(nil) = %v, %v; want an empty map", empty, err)
	}
}
