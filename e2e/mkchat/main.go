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
// With MUSTER_E2E_FOCUS set to a JSON array of {"subject","title","description"}
// (or {"subject","error"}: a failed refresh's reason, and no focus)
// it also writes session FOCUS (internal/focus) after the agent is committed:
// subject "self" is the agent it made, "mirror:<claude session id>" a mirrored
// session. Deleting deletes the agent (its focus goes with it) and every
// mirror focus named there.
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
	"github.com/ZacxDev/muster/internal/focus"
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
		if err := deleteMirrorFocus(ctx, pool); err != nil {
			return err
		}
		return as.Delete(ctx, a.ID)
	}

	display := os.Getenv("MUSTER_E2E_DISPLAY_NAME")
	if os.Getenv("MUSTER_E2E_THREADS") == "1" {
		var threads []thread
		if err := json.NewDecoder(os.Stdin).Decode(&threads); err != nil {
			return fmt.Errorf("threads on stdin: %w", err)
		}
		if err := createThreads(ctx, pool, name, display, threads); err != nil {
			return err
		}
		return writeFocus(ctx, pool, as, name)
	}
	var msgs []agents.ChatMessage
	if err := json.NewDecoder(os.Stdin).Decode(&msgs); err != nil {
		return fmt.Errorf("messages on stdin: %w", err)
	}
	if err := createThreads(ctx, pool, name, display, []thread{{Messages: msgs}}); err != nil {
		return err
	}
	return writeFocus(ctx, pool, as, name)
}

// focusSpec is one MUSTER_E2E_FOCUS entry.
type focusSpec struct {
	Subject     string `json:"subject"`
	Title       string `json:"title"`
	Description string `json:"description"`
	// Error, when set, records a failed refresh with this reason instead of
	// writing a focus (what the card shows as why there is none).
	Error string `json:"error"`
	// History, when > 0, writes that many EARLIER focuses first ("Earlier
	// focus 01" … oldest first), each a change, so the session's focus history
	// (migration 0022) holds History+1 rows, the focus itself newest.
	History int `json:"history"`
}

func focusSpecs() ([]focusSpec, error) {
	raw := os.Getenv("MUSTER_E2E_FOCUS")
	if raw == "" {
		return nil, nil
	}
	var specs []focusSpec
	if err := json.Unmarshal([]byte(raw), &specs); err != nil {
		return nil, fmt.Errorf("MUSTER_E2E_FOCUS: %w", err)
	}
	return specs, nil
}

// writeFocus writes MUSTER_E2E_FOCUS's entries as the operator.
func writeFocus(ctx context.Context, pool *pgxpool.Pool, as *agents.PGStore, name string) error {
	specs, err := focusSpecs()
	if err != nil || len(specs) == 0 {
		return err
	}
	a, err := as.GetByName(ctx, name)
	if err != nil {
		return fmt.Errorf("find agent: %w", err)
	}
	fs := focus.NewPG(pool)
	for _, sp := range specs {
		sub := focus.AgentSubject(a.ID)
		if sp.Subject != "self" {
			if sub, err = focus.ParseSubject(sp.Subject); err != nil {
				return err
			}
		}
		if sp.Error != "" {
			if err := fs.RecordError(ctx, sub, sp.Error); err != nil {
				return fmt.Errorf("focus error %s: %w", sub, err)
			}
			continue
		}
		for i := 1; i <= sp.History; i++ {
			if _, err := fs.Upsert(ctx, sub, focus.Write{Title: fmt.Sprintf("Earlier focus %02d", i),
				Description: fmt.Sprintf("What the session was doing at step %d.", i), Source: focus.SourceSummarizer}); err != nil {
				return fmt.Errorf("focus history %s: %w", sub, err)
			}
		}
		if _, err := fs.Upsert(ctx, sub, focus.Write{Title: sp.Title, Description: sp.Description, Source: focus.SourceSummarizer}); err != nil {
			return fmt.Errorf("focus %s: %w", sub, err)
		}
	}
	return nil
}

// deleteMirrorFocus deletes the mirror focus rows MUSTER_E2E_FOCUS names (their
// history goes with them, by migration 0022's foreign key).
func deleteMirrorFocus(ctx context.Context, pool *pgxpool.Pool) error {
	specs, err := focusSpecs()
	if err != nil {
		return err
	}
	for _, sp := range specs {
		sub, err := focus.ParseSubject(sp.Subject)
		if err != nil || sub.Kind != focus.KindMirror {
			continue
		}
		if _, err := pool.Exec(ctx, `DELETE FROM agent_focus WHERE subject_kind = 'mirror' AND subject_key = $1`, sub.Key); err != nil {
			return fmt.Errorf("delete focus %s: %w", sub, err)
		}
	}
	return nil
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
