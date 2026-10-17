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

	var msgs []agents.ChatMessage
	if err := json.NewDecoder(os.Stdin).Decode(&msgs); err != nil {
		return fmt.Errorf("messages on stdin: %w", err)
	}
	if err := create(ctx, pool, name, msgs); err != nil {
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
		if _, err := fs.Upsert(ctx, sub, focus.Write{Title: sp.Title, Description: sp.Description, Source: focus.SourceSummarizer}); err != nil {
			return fmt.Errorf("focus %s: %w", sub, err)
		}
	}
	return nil
}

// deleteMirrorFocus deletes the mirror focus rows MUSTER_E2E_FOCUS names.
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

// create writes the agent named name, its chat session and msgs in ONE
// transaction (see the package doc): any error rolls all of it back.
func create(ctx context.Context, pool *pgxpool.Pool, name string, msgs []agents.ChatMessage) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		as := agents.NewPGTx(tx)
		a, err := as.Create(ctx, agents.Agent{Name: name, Namespace: "agents", Status: "running", KickedOff: true})
		if err != nil {
			return fmt.Errorf("create agent: %w", err)
		}
		sess, err := as.LatestOrCreateSession(ctx, a.ID, a.Name)
		if err != nil {
			return fmt.Errorf("chat session: %w", err)
		}
		for i, m := range msgs {
			m.AgentID, m.SessionID = a.ID, sess.ID
			if _, err := as.AddChatMessage(ctx, m); err != nil {
				return fmt.Errorf("add message %d (%d bytes): %w", i, len(m.Content), err)
			}
		}
		return nil
	})
}
