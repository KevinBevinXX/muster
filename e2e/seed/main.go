// Command seed resets an e2e database to a known board: a handful of tasks in
// every status and a few agents in distinct states, so the browser suite and
// the manifest screenshots render the same thing on every run.
//
// 🔴 IT REFUSES ANY DATABASE WHOSE NAME DOES NOT END IN "_e2e". It truncates
// every table it seeds, so pointing it at a real deployment by accident would
// empty the board. The suffix check is the guard; there is no override flag.
//
// Every value is invented fixture data: no real repository, person or host.
package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/notes"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		os.Exit(1)
	}
}

func run() error {
	dsn := os.Getenv("DATABASE_URL")
	if err := checkE2EDatabase(dsn); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	// CASCADE empties every table that references these (comments, attachments,
	// sessions, chat, runs, grants) without this file naming them — the notes
	// package's ledger forbids spelling the comments table outside its store.
	if _, err := pool.Exec(ctx, `TRUNCATE notes, agents, runbooks, privilege_profiles, privilege_requests,
		connectors, connector_audit RESTART IDENTITY CASCADE`); err != nil {
		return fmt.Errorf("truncate: %w", err)
	}

	ns := notes.NewPG(pool)
	tasks := []struct {
		title, body, status string
		tags                []string
	}{
		{"Rotate the staging webhook secret", "Old secret still accepted by the receiver. Swap it, then confirm one delivery lands.", notes.StatusReadyForReview, []string{"infra", "security"}},
		{"Draft release notes for 0.4", "Collect merged changes since the last tag.", notes.StatusInProgress, []string{"docs"}},
		{"Triage the flaky upload test", "Fails about one run in twenty on the slow runner.", notes.StatusOpen, []string{"ci"}},
		{"Archive the old onboarding doc", "Superseded by the new guide.", notes.StatusComplete, nil},
		{"Add a dark-mode screenshot to the README", "", notes.StatusOpen, []string{"docs"}},
	}
	var first int64
	for _, t := range tasks {
		n, err := ns.Create(ctx, notes.Note{Title: t.title, Body: t.body, Tags: t.tags, Status: notes.StatusOpen})
		if err != nil {
			return fmt.Errorf("create task %q: %w", t.title, err)
		}
		if first == 0 {
			first = n.ID
		}
		if t.status != notes.StatusOpen {
			if _, err := ns.SetStatus(ctx, n.ID, t.status); err != nil {
				return fmt.Errorf("set status %q: %w", t.title, err)
			}
		}
	}

	as := agents.NewPG(pool)
	for _, a := range []agents.Agent{
		// The hooks token is the agent's OWN credential; the connector spec presents
		// it as the agent (e2e/lib/env.ts agentToken). Invented.
		{Name: "docs-sweeper", Namespace: "agents", Repo: "example/site", Status: "running", KickedOff: true, NoteID: &first,
			HooksToken: "e2e-agent-token-not-a-real-one"},
		{Name: "release-helper", Namespace: "agents", Repo: "example/api", Status: "error", KickedOff: true, KickoffError: "gateway timed out after 30s"},
		{Name: "notes-tidy", Namespace: "agents", Status: "pending"},
	} {
		if _, err := as.Create(ctx, a); err != nil {
			return fmt.Errorf("create agent %q: %w", a.Name, err)
		}
	}
	fmt.Printf("seed: %d tasks, 3 agents\n", len(tasks))
	return nil
}

// checkE2EDatabase is the package doc's guard: nil only for a DSN naming a database
// whose name ends in _e2e. It is what keeps the manifest screenshots honest
// too — `npm run capture` shares global-setup, which runs this seed before it
// starts the server, so a capture pointed at a real database fails here instead
// of photographing it.
func checkE2EDatabase(dsn string) error {
	if dsn == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	if name := strings.TrimPrefix(u.Path, "/"); !strings.HasSuffix(name, "_e2e") {
		return fmt.Errorf("refusing to truncate database %q: the e2e seed only touches a database whose name ends in _e2e", name)
	}
	return nil
}
