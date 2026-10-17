package db

import (
	"context"
	_ "embed"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// rlsOffSQL is rollback/rls_off.sql: the documented way back from migration
// 0007 before an OLDER image is deployed. See that file's header.
//
//go:embed rollback/rls_off.sql
var rlsOffSQL string

// rlsMigrationVersion is the migration whose body RLSOn re-applies.
const rlsMigrationVersion = 7

// RLSOff disables row-level security on every owned table and restores
// owner_id DEFAULT 1 (rollback/rls_off.sql), so that an image built before
// accounts PR 3a can run against this schema. It runs as the migrator (the
// login role), never as AppRole. cmd/muster-migrate --rls-off calls it.
//
// 🔴 schema_migrations IS NOT TOUCHED. Version 7 stays recorded, so a newer
// image booting afterwards does NOT re-enable RLS on its own; RLSOn does that,
// explicitly.
func RLSOff(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(asMigrator(ctx), rlsOffSQL); err != nil {
		return fmt.Errorf("rls off: %w", err)
	}
	return nil
}

// mirrorTokensMigrationVersion is 0008: the mirror plugin's mirror_tokens, an
// owned table under the same row-level security. RLSOff turns it off with the
// rest and RLSOn re-applies its body (idempotent) — but only on a schema that
// HAS the table, so RLSOn never creates it outside Migrate.
const mirrorTokensMigrationVersion = 8

// userCredentialsMigrationVersion is 0011: user_credentials and the owned
// cc_account_marks, under the same row-level security. Same treatment as 0008:
// RLSOff turns them off, RLSOn re-applies 0011's (idempotent) body when the
// schema has user_credentials.
const userCredentialsMigrationVersion = 11

// flowJobsMigrationVersion is 0012: browser flows' flow_jobs and
// flow_runner_tokens, owned and under the same row-level security. Same
// treatment as 0008: RLSOff turns them off, RLSOn re-applies 0012's
// (idempotent) body when the schema has flow_jobs.
const flowJobsMigrationVersion = 12

// pushSubscriptionsMigrationVersion is 0013: push_subscriptions and
// push_preferences (muster-native Web Push), under the same row-level
// security. Same treatment as 0008 and 0011.
const pushSubscriptionsMigrationVersion = 13

// flowRequestsMigrationVersion is 0014: browser flows' agent requests, owned
// and under the same row-level security; same treatment again.
const flowRequestsMigrationVersion = 14

// pluginsMigrationVersion is 0015: plugins and plugin_messages, owned and
// under the same row-level security; same treatment again.
const pluginsMigrationVersion = 15

// pluginRepliesMigrationVersion is 0016: plugin_reply_drafts, owned and under
// the same row-level security; same treatment again, keyed on that table.
const pluginRepliesMigrationVersion = 16

// pinsMigrationVersion is 0017: pins (the Agents list's pinned entries), owned
// and under the same row-level security; same treatment again.
const pinsMigrationVersion = 17

// recentsMigrationVersion is 0018: recents (the Agents list's recently viewed
// entries), owned and under the same row-level security; same treatment again.
const recentsMigrationVersion = 18

// sendQueueMigrationVersion is 0019: send_queue (the server-side send queue),
// owned and under the same row-level security; same treatment again.
const sendQueueMigrationVersion = 19

// agentFocusMigrationVersion is 0020: agent_focus (each session's focus), owned
// and under the same row-level security; same treatment again.
const agentFocusMigrationVersion = 20

// sessionRoutesMigrationVersion is 0021: session_routes (the chief's routed plugin messages), owned
// and under the same row-level security; same treatment again.
const sessionRoutesMigrationVersion = 21

// RLSOn re-applies migration 0007's body (it is idempotent): RLS enabled and
// forced on every owned table, the policies, owner_id defaulting to the
// principal, the role and its grants — and 0008's for mirror_tokens when that
// table exists. It undoes RLSOff.
func RLSOn(ctx context.Context, pool *pgxpool.Pool) error {
	ms, err := loadMigrations()
	if err != nil {
		return err
	}
	ctx = asMigrator(ctx)
	var hasMirrorTokens, hasUserCredentials, hasFlowJobs, hasPushSubscriptions, hasFlowRequests, hasPlugins, hasPluginReplies, hasPins, hasRecents, hasSendQueue, hasAgentFocus, hasSessionRoutes bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.mirror_tokens') IS NOT NULL,
		to_regclass('public.user_credentials') IS NOT NULL,
		to_regclass('public.flow_jobs') IS NOT NULL,
		to_regclass('public.push_subscriptions') IS NOT NULL,
		to_regclass('public.flow_requests') IS NOT NULL,
		to_regclass('public.plugins') IS NOT NULL,
		to_regclass('public.plugin_reply_drafts') IS NOT NULL,
		to_regclass('public.pins') IS NOT NULL,
		to_regclass('public.recents') IS NOT NULL,
		to_regclass('public.send_queue') IS NOT NULL,
		to_regclass('public.agent_focus') IS NOT NULL,
		to_regclass('public.session_routes') IS NOT NULL`).Scan(&hasMirrorTokens, &hasUserCredentials,
		&hasFlowJobs, &hasPushSubscriptions, &hasFlowRequests, &hasPlugins, &hasPluginReplies, &hasPins, &hasRecents, &hasSendQueue, &hasAgentFocus, &hasSessionRoutes); err != nil {
		return fmt.Errorf("rls on: %w", err)
	}
	applied := false
	for _, m := range ms {
		if m.version != rlsMigrationVersion && !(m.version == mirrorTokensMigrationVersion && hasMirrorTokens) &&
			!(m.version == userCredentialsMigrationVersion && hasUserCredentials) &&
			!(m.version == flowJobsMigrationVersion && hasFlowJobs) &&
			!(m.version == pushSubscriptionsMigrationVersion && hasPushSubscriptions) &&
			!(m.version == flowRequestsMigrationVersion && hasFlowRequests) &&
			!(m.version == pluginsMigrationVersion && hasPlugins) &&
			!(m.version == pluginRepliesMigrationVersion && hasPluginReplies) &&
			!(m.version == pinsMigrationVersion && hasPins) &&
			!(m.version == recentsMigrationVersion && hasRecents) &&
			!(m.version == sendQueueMigrationVersion && hasSendQueue) &&
			!(m.version == agentFocusMigrationVersion && hasAgentFocus) &&
			!(m.version == sessionRoutesMigrationVersion && hasSessionRoutes) {
			continue
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("rls on: begin: %w", err)
		}
		if _, err := tx.Exec(ctx, m.sql); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("rls on: apply %s: %w", m.name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("rls on: commit: %w", err)
		}
		applied = applied || m.version == rlsMigrationVersion
	}
	if applied {
		return nil
	}
	return fmt.Errorf("rls on: migration %04d is not embedded", rlsMigrationVersion)
}

// RLSState is what RLSStatus reads back: per owned table, whether row-level
// security is enabled and forced, and owner_id's default expression.
type RLSState struct {
	Table        string
	Enabled      bool
	Forced       bool
	OwnerDefault string
}

// RLSStatus reads pg_class and information_schema for every table that has an
// owner_id column — the verification after RLSOff / RLSOn, so a run that did
// nothing (an image whose muster-migrate predates the flag ignores it and exits
// 0) cannot pass for one that worked.
func RLSStatus(ctx context.Context, pool *pgxpool.Pool) ([]RLSState, error) {
	rows, err := pool.Query(asMigrator(ctx), `
		SELECT c.relname::text, c.relrowsecurity, c.relforcerowsecurity, coalesce(col.column_default, '')
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'public'
		  JOIN information_schema.columns col
		    ON col.table_schema = 'public' AND col.table_name = c.relname AND col.column_name = 'owner_id'
		 WHERE c.relkind = 'r'
		 ORDER BY 1`)
	if err != nil {
		return nil, fmt.Errorf("rls status: %w", err)
	}
	defer rows.Close()
	var out []RLSState
	for rows.Next() {
		var st RLSState
		if err := rows.Scan(&st.Table, &st.Enabled, &st.Forced, &st.OwnerDefault); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("rls status: no table has an owner_id column — this is not a muster schema at version >= 6")
	}
	return out, nil
}
