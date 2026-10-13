package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/api"
	"github.com/ZacxDev/muster/internal/memorelay"
	"github.com/ZacxDev/muster/internal/notes"
)

// validateMemoRelay is the deployment policy for the cairn memo relay
// (internal/memorelay).
//
// 🔴 THE TOKEN ALONE TURNS THE RELAY ON, AND A URL WITHOUT A TOKEN IS A VALID,
// INERT STATE — NOT HALF A PAIR. The credential is issued by a separate, manual
// step in another system, after this deployment already names the listener: the
// manifest carries the URL and an OPTIONAL secret reference, so "URL set, token
// unset" is exactly what runs between the two. Refusing it would make the
// rollout order (deploy, then issue) impossible.
//
// The other direction IS refused: a token with no listener URL is a credential
// with nowhere to go, and the relay would silently never run.
func (c config) validateMemoRelay() error {
	if c.MemoToken == "" {
		return nil
	}
	if c.MemoURL == "" {
		return fmt.Errorf("%s is set but %s is not: the memo relay has a credential and no listener to "+
			"present it to. Set %s to the cairn memo listener's base URL, or unset %s",
			envMemoToken, envMemoURL, envMemoURL, envMemoToken)
	}
	// The same shape check the relay's own constructor applies, at boot rather
	// than at the first tick. The clock is irrelevant here.
	if _, err := memorelay.NewListener(c.MemoURL, c.MemoToken, nil); err != nil {
		return fmt.Errorf("invalid %s: %w", envMemoURL, err)
	}
	return nil
}

// memoRelayEnabled is the ONE predicate for "this process runs the relay", read
// by the builder and the banner alike: a credential, a database to keep the
// delivery record in, and a gateway to deliver a turn through.
func memoRelayEnabled(cfg config, hasPool, hasGateway bool) bool {
	return cfg.MemoToken != "" && hasPool && hasGateway
}

// buildMemoRelay builds the relay, or (nil, nil) when it is not enabled.
//
// 🔴 gw IS TAKEN AS THE CONCRETE POINTER, NOT AS memorelay.Gateway. A nil
// *agentgateway.Gateway stored in an interface is a NON-nil interface, so a
// builder that took the interface would see "a gateway" on a deployment with
// none and start a relay whose every delivery panics.
func buildMemoRelay(cfg config, pool *pgxpool.Pool, store agents.Store, tasks notes.Store, gw *agentgateway.Gateway, logger *log.Logger) (*memorelay.Relay, error) {
	if !memoRelayEnabled(cfg, pool != nil, gw != nil) {
		return nil, nil
	}
	listener, err := memorelay.NewListener(cfg.MemoURL, cfg.MemoToken, time.Now)
	if err != nil {
		return nil, err
	}
	return memorelay.New(memorelay.Config{
		Agents:          store,
		TaskRepo:        taskRepoReader(tasks),
		Gateway:         gw,
		Source:          listener,
		Ledger:          memorelay.NewPGLedger(pool),
		NamespacePrefix: cfg.AgentNamespacePrefix,
		Logger:          logger,
	})
}

// taskRepoReader adapts the task store to memorelay.Config.TaskRepo: the
// `owner/name` a task names, "" when it names none — or when the task is gone
// (or not the acting user's: row-level security answers both the same way).
func taskRepoReader(tasks notes.Store) func(context.Context, int64) (string, error) {
	return func(ctx context.Context, id int64) (string, error) {
		n, err := tasks.Get(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		return n.Repo, nil
	}
}

// logMemoRelayBanner states the relay's posture in all three of its states.
func (a *app) logMemoRelayBanner(ext api.Extensions) {
	l := a.logger
	switch {
	case a.cfg.MemoToken == "":
		l.Printf("memo relay: OFF — %s is unset, so no cairn memo is read or delivered and the listener "+
			"(%s: %s) is never contacted. This is the state a deployment waits in until its credential is issued",
			envMemoToken, envMemoURL, presence(a.cfg.MemoURL))
	case !memoRelayEnabled(a.cfg, a.pool != nil, ext.Gateway != nil):
		l.Printf("memo relay: CONFIGURED BUT NOT RUNNING — %s and %s are set, but the relay delivers a memo "+
			"as a turn through the agent gateway and keeps its delivery record in the database, and one of "+
			"the two is missing (gateway: %s, database: %s)", envMemoToken, envMemoURL,
			wiredWord(ext.Gateway == nil), wiredWord(a.pool == nil))
	default:
		l.Printf("memo relay: ON — %s is set; live cairn memos are read from %s=%s every %s (ONE request at "+
			"a time, none for the first %s, and none for %s after a refused credential) and delivered once "+
			"to each of the OPERATOR's running agents whose repository — its own, else its task's — names "+
			"a scope. An agent with neither is sent nothing and counted",
			envMemoToken, envMemoURL, a.cfg.MemoURL, memorelay.DefaultInterval, memorelay.StartDelay,
			memorelay.RefusedBackoff)
	}
}

// waitForMemoRelay waits, bounded, for the relay to finish recording the turns
// shutdown cut off — the same reason waitForKickoffs exists: a record still in
// flight when the pool closes is lost, and the next process would deliver those
// memos a second time.
func (a *app) waitForMemoRelay(timeout time.Duration) {
	if a.memo == nil {
		return
	}
	select {
	case <-a.memo.Done():
	case <-time.After(timeout):
		a.logger.Printf("memo relay did not finish recording within %s; exiting anyway (a delivery cut "+
			"off by this shutdown may be repeated by the next process)", timeout)
	}
}
