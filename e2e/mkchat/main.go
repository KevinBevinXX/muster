// Command mkchat gives a browser spec an agent of its OWN with a persisted
// chat, so the spec can drive real server-rendered bubbles (chatBubble's
// data-md and data-msg-id) without adding rows to the seeded agents other
// specs count. With MUSTER_E2E_DELETE=1 it deletes that agent (and, by the
// foreign keys, its sessions and messages) instead.
//
//	DATABASE_URL=…_e2e MUSTER_E2E_AGENT=<name> go run ./e2e/mkchat < messages.json
//	DATABASE_URL=…_e2e MUSTER_E2E_AGENT=<name> MUSTER_E2E_DELETE=1 go run ./e2e/mkchat
//
// messages.json is a JSON array of {"role":"user"|"assistant","content":"…"},
// persisted in order into the agent's chat session. Every value a spec sends
// is invented fixture data.
//
// 🔴 IT REFUSES ANY DATABASE WHOSE NAME DOES NOT END IN "_e2e", like e2e/seed.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mkchat:", err)
		os.Exit(1)
	}
}

func run() error {
	dsn := os.Getenv("DATABASE_URL")
	u, err := url.Parse(dsn)
	if err != nil || !strings.HasSuffix(strings.TrimPrefix(u.Path, "/"), "_e2e") {
		return fmt.Errorf("refusing: DATABASE_URL must name a database ending in _e2e")
	}
	name := os.Getenv("MUSTER_E2E_AGENT")
	if name == "" {
		return fmt.Errorf("MUSTER_E2E_AGENT is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	// Every row is the operator's, as the seed's are (row-level security).
	ctx = authz.Operator(ctx)
	as := agents.NewPG(pool)

	if os.Getenv("MUSTER_E2E_DELETE") == "1" {
		a, err := as.GetByName(ctx, name)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // already gone
		}
		if err != nil {
			return fmt.Errorf("find agent: %w", err)
		}
		return as.Delete(ctx, a.ID)
	}

	var msgs []agents.ChatMessage
	if err := json.NewDecoder(os.Stdin).Decode(&msgs); err != nil {
		return fmt.Errorf("messages on stdin: %w", err)
	}
	a, err := as.Create(ctx, agents.Agent{Name: name, Namespace: "agents", Status: "running", KickedOff: true})
	if err != nil {
		return fmt.Errorf("create agent: %w", err)
	}
	sess, err := as.LatestOrCreateSession(ctx, a.ID, a.Name)
	if err != nil {
		return fmt.Errorf("chat session: %w", err)
	}
	for _, m := range msgs {
		m.AgentID, m.SessionID = a.ID, sess.ID
		if _, err := as.AddChatMessage(ctx, m); err != nil {
			return fmt.Errorf("add message: %w", err)
		}
	}
	return nil
}
