package main

import (
	"context"
	"errors"
	"io"
	"log"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// 🔴 A message Postgres refuses leaves NO agent behind: the agent, its session
// and its messages are one transaction. Before that, the agent and session
// committed one statement at a time, so a refused message (here a NUL byte,
// which a Postgres text column cannot hold) failed the run AFTER leaving a
// fixture agent in the shared e2e database for every later spec to see.
func TestARefusedMessageLeavesNoAgent(t *testing.T) {
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool, log.New(io.Discard, "", 0)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	dbtest.OwnerExec(t, dsn, `DELETE FROM agents WHERE name LIKE 'mkchat-%'`)
	as := agents.NewPG(pool)

	// Positive control: the same call with clean messages creates the agent
	// and every message, so the lookup below can see one.
	good := []agents.ChatMessage{{Role: "user", Content: "first"}, {Role: "assistant", Content: "second"}}
	if err := create(ctx, pool, "mkchat-good", good); err != nil {
		t.Fatalf("create with clean messages: %v", err)
	}
	a, err := as.GetByName(ctx, "mkchat-good")
	if err != nil {
		t.Fatalf("positive control: the clean agent is missing: %v", err)
	}
	sess, err := as.LatestOrCreateSession(ctx, a.ID, a.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := as.ListChatMessages(ctx, sess.ID); err != nil || len(got) != 2 {
		t.Fatalf("positive control: messages = %d (%v), want 2", len(got), err)
	}

	// The second message is refused, after the agent and the first message.
	bad := []agents.ChatMessage{{Role: "user", Content: "first"}, {Role: "assistant", Content: "bad\x00byte"}}
	if err := create(ctx, pool, "mkchat-bad", bad); err == nil {
		t.Fatal("create with a NUL byte succeeded; want the run to fail (rc != 0)")
	}
	if _, err := as.GetByName(ctx, "mkchat-bad"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("after a refused message the agent is still there (lookup err %v); want it rolled back", err)
	}
}

// Threads mode writes one session per entry with the title, recency and display
// name asked for — the shape the chief thread-search spec reads its order from.
func TestThreadsGetTheirTitlesRecencyAndDisplayName(t *testing.T) {
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool, log.New(io.Discard, "", 0)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	dbtest.OwnerExec(t, dsn, `DELETE FROM agents WHERE name LIKE 'mkchat-%'`)
	as := agents.NewPG(pool)

	threads := []thread{
		{Title: "older one", HoursAgo: 30, Messages: []agents.ChatMessage{{Role: "user", Content: "a"}}},
		{Title: "newer one", HoursAgo: 2, Messages: []agents.ChatMessage{{Role: "user", Content: "b"}}},
	}
	if err := createThreads(ctx, pool, "mkchat-threads", "Chief", threads); err != nil {
		t.Fatalf("createThreads: %v", err)
	}
	a, err := as.GetByName(ctx, "mkchat-threads")
	if err != nil {
		t.Fatal(err)
	}
	if a.DisplayName != "Chief" {
		t.Errorf("display name %q, want Chief", a.DisplayName)
	}
	list, err := as.ListSessions(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, s := range list {
		titles = append(titles, s.Title)
	}
	if len(titles) != 2 || titles[0] != "newer one" || titles[1] != "older one" {
		t.Errorf("sessions (most recent first) = %q, want [newer one, older one]", titles)
	}
}
