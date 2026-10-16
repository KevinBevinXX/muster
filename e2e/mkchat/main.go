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
// THREADS. With MUSTER_E2E_THREADS=1 stdin is instead a JSON array of
// {"title":"…","hoursAgo":N,"messages":[…]}: one chat session per entry, its
// title and its last-activity time set as given (so a spec can draw a thread
// list whose recency it knows). MUSTER_E2E_DISPLAY_NAME sets the agent's
// display name — "chief" makes it the chief panel's agent.
//
//	DATABASE_URL=…_e2e MUSTER_E2E_AGENT=<name> MUSTER_E2E_DISPLAY_NAME=chief MUSTER_E2E_THREADS=1 go run ./e2e/mkchat < threads.json
//
// 🔴 IT REFUSES ANY DATABASE WHOSE NAME DOES NOT END IN "_e2e" (e2edb.Check).
//
// 🔴 THE AGENT, ITS SESSION AND EVERY MESSAGE ARE ONE TRANSACTION. A message
// Postgres refuses (a NUL byte, say) leaves NO agent behind, so a failed run
// cannot leak a fixture agent into the shared e2e database for later specs to
// count. Deleting is idempotent: an agent already gone is success.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/e2e/internal/e2edb"
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
	if err := e2edb.Check(dsn); err != nil {
		return err
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

	display := os.Getenv("MUSTER_E2E_DISPLAY_NAME")
	if os.Getenv("MUSTER_E2E_THREADS") == "1" {
		var threads []thread
		if err := json.NewDecoder(os.Stdin).Decode(&threads); err != nil {
			return fmt.Errorf("threads on stdin: %w", err)
		}
		return createThreads(ctx, pool, name, display, threads)
	}
	var msgs []agents.ChatMessage
	if err := json.NewDecoder(os.Stdin).Decode(&msgs); err != nil {
		return fmt.Errorf("messages on stdin: %w", err)
	}
	return createThreads(ctx, pool, name, display, []thread{{Messages: msgs}})
}

// thread is one chat session to write: its title ("" keeps the one the first
// user message gives it), how long ago it was last active (0 leaves it now),
// and its messages in order.
type thread struct {
	Title    string               `json:"title"`
	HoursAgo int                  `json:"hoursAgo"`
	Messages []agents.ChatMessage `json:"messages"`
}

// create writes the agent named name, its chat session and msgs in ONE
// transaction (see the package doc): any error rolls all of it back.
func create(ctx context.Context, pool *pgxpool.Pool, name string, msgs []agents.ChatMessage) error {
	return createThreads(ctx, pool, name, "", []thread{{Messages: msgs}})
}

// createThreads is create with a display name and any number of sessions, still
// ONE transaction.
func createThreads(ctx context.Context, pool *pgxpool.Pool, name, display string, threads []thread) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		as := agents.NewPGTx(tx)
		a, err := as.Create(ctx, agents.Agent{Name: name, DisplayName: display, Namespace: "agents", Status: "running", KickedOff: true})
		if err != nil {
			return fmt.Errorf("create agent: %w", err)
		}
		for ti, th := range threads {
			var sess agents.ChatSession
			if ti == 0 {
				sess, err = as.LatestOrCreateSession(ctx, a.ID, a.Name)
			} else {
				sess, err = as.CreateSession(ctx, a.ID, a.Name)
			}
			if err != nil {
				return fmt.Errorf("chat session %d: %w", ti, err)
			}
			for i, m := range th.Messages {
				m.AgentID, m.SessionID = a.ID, sess.ID
				if _, err := as.AddChatMessage(ctx, m); err != nil {
					return fmt.Errorf("add message %d (%d bytes): %w", i, len(m.Content), err)
				}
			}
			if th.Title != "" {
				if _, err := tx.Exec(ctx, `UPDATE chat_sessions SET title=$1 WHERE id=$2`, th.Title, sess.ID); err != nil {
					return fmt.Errorf("title thread %d: %w", ti, err)
				}
			}
			if th.HoursAgo > 0 {
				if _, err := tx.Exec(ctx, `UPDATE chat_sessions SET updated_at = now() - make_interval(hours => $1) WHERE id=$2`,
					th.HoursAgo, sess.ID); err != nil {
					return fmt.Errorf("age thread %d: %w", ti, err)
				}
			}
		}
		return nil
	})
}
