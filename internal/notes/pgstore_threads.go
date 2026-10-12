package notes

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// sessionCols is the SINGLE task_sessions projection every link read uses, so the
// per-task read, the batched board read and the reverse lookup can never drift out
// of sync with scanSessionLinkInto's target order — exactly as noteCols does for
// notes.
//
// 🔴 THIS PACKAGE NO LONGER READS THE SESSION RECORD AT ALL, AND THAT IS A
// DEPENDENCY-DIRECTION RULE, NOT A TIDY-UP. This projection used to carry a
// `(… IS NOT NULL)` expression off a LEFT JOIN, which is where
// SessionLink.DetailAvailable came from. That table belongs to another service
// and is not in this schema, so a read of it from here — in SQL or in Go — is
// precisely the cross-service dependency this package was carved out to remove.
//
// What that costs, and who pays it: DetailAvailable is now the ZERO VALUE on
// everything this package returns, and the API layer composes it (see
// api.withSessionLiveness). It is NOT optional enrichment — false renders "no
// transcript recorded" over a session whose transcript is alive, which is the
// conflation that disguised a ~2.5-month Stop-hook outage as ordinary retention.
// Any new caller of this package's link reads composes liveness or renders a lie.
//
// ts.detail_seen is the STORED half and is unaffected: it is a real column
// recording whether a transcript was ever OBSERVED for this link.
// Its observation now arrives as an ARGUMENT to LinkSession instead of being probed
// in SQL. One bit without the other cannot tell "reaped" from "never recorded" —
// see SessionLink.DetailSeen.
const sessionCols = `ts.session_id, ts.role, ts.project, ts.cwd, ts.host, ts.first_seen_at, ts.last_seen_at, ts.detail_seen`

// sessionFrom is the FROM clause sessionCols is written against.
const sessionFrom = `FROM task_sessions ts`

// scanSessionLinkInto returns the Scan destinations for sessionCols, in order.
//
// 🔴 THIS AND sessionCols ARE ONE UNIT — change them in the SAME edit. Their order
// is the whole contract, and Postgres will happily scan a value into the wrong
// field of the same type.
//
// 🔴 SessionLink.DetailAvailable IS DELIBERATELY ABSENT FROM THIS LIST. It has no
// column and no expression behind it any more; it is composed at the API layer
// from the other service's session record. Adding a destination for it here would
// need a source, and the only source is the JOIN this package dropped.
func scanSessionLinkInto(l *SessionLink) []any {
	return []any{&l.SessionID, &l.Role, &l.Project, &l.Cwd, &l.Host, &l.FirstSeenAt, &l.LastSeenAt, &l.DetailSeen}
}

// detailSeenUpgradeExpr is the MONOTONIC merge on conflict — the detail_seen twin of
// roleUpgradeExpr. A plain assignment would let a touch made after the 14-day sweep
// reset a link that HAD been observed back to false, which is precisely the
// "recorded then reaped" state the column exists to remember.
const detailSeenUpgradeExpr = `(task_sessions.detail_seen OR EXCLUDED.detail_seen)`

// lastSeenCoalesce is how stale a link's last_seen_at must be before an otherwise
// unchanged touch is allowed to rewrite it.
//
// 🔴 IT EXISTS BECAUSE A SAFE METHOD NOW WRITES. GET /api/tasks/{id} records a
// `read` link, and `ON CONFLICT DO UPDATE` produces a dead tuple on every
// execution — even when every column is assigned its own current value — so a
// polled task would accumulate one dead tuple per read and hand the whole cost to
// autovacuum. Five minutes keeps last_seen_at useful for the reverse lookup's
// ordering (which is a human-scale "what did this session work on") while
// collapsing a burst of reads into at most one write.
//
// A ROLE UPGRADE, NEW CONTEXT, or a detail_seen FALSE→TRUE transition always writes
// regardless of this window — those are real changes, and delaying them would lose
// information rather than defer it.
//
// 🔴 THE detail_seen CLAUSE IS LOAD-BEARING, NOT DEFENSIVE. Leaving it out of the
// `DO UPDATE … WHERE` would suppress exactly the transition detail_seen exists
// to capture, and would do it silently: the link's other columns genuinely have not
// changed, so the coalesce would refuse the write, the read-back path would return
// the OLD detail_seen, and a session whose transcript had just been recorded would
// keep rendering "no transcript recorded" — for up to five minutes, and in the
// common case forever, because a task is rarely touched again.
const lastSeenCoalesce = 5 * time.Minute

// quotedRoleRead is RoleRead as a SQL literal, derived from the Go constant so the
// eviction query cannot drift from the vocabulary. Spelling 'read' inline would be
// a second definition.
var quotedRoleRead = "'" + RoleRead + "'"

// roleUpgradeExpr is the SINGLE SQL expression enforcing the monotonic role rule
// on an upsert: the resulting role is whichever of (existing, incoming) sits
// further along ['read','worked','created'].
//
// 🔴 This is the Postgres MIRROR of roleRank/UpgradeRole in threads.go and the
// array's ORDER IS THE CONTRACT — reversing it, or dropping an element, silently
// turns "never downgrade" into "always take the newest", which no CHECK constraint
// would catch. TestRoleRankMatchesSQLOrdering pins the two orderings together and
// TestPGLinkSessionRoleIsMonotonic exercises this expression against a real
// database in both directions.
//
// GREATEST ignores NULLs, so an unrecognised role on either side degrades to the
// other one rather than nulling the column; the CHECK constraint and ValidRole
// already make that unreachable.
const roleUpgradeExpr = `(ARRAY['read','worked','created'])[GREATEST(
	array_position(ARRAY['read','worked','created'], task_sessions.role),
	array_position(ARRAY['read','worked','created'], EXCLUDED.role))]`

// LinkSession records that a coding-agent session touched a task, upserting the
// (note_id, session_id) edge and returning the resulting link plus whether
// anything OBSERVABLE changed (a new row, or a role upgrade — not a bare
// last_seen_at touch, which callers must not broadcast on).
//
// 🔴 A PROVENANCE BREADCRUMB MUST NEVER BREAK THE OPERATION IT ANNOTATES. The
// first version of this function enforced the per-task session cap as a HARD bound
// in the upsert's WHERE and answered ErrTooManySessions, which callers turned into
// a 409. Two defects fell straight out of that, both measured on a live server:
//
//   - A SPLIT WRITE. PATCH /api/tasks/{id}/status applied the status change and
//     THEN failed the link, so the caller was told `409 session thread is full`
//     about a request that had already succeeded (open → in_progress).
//   - A PERMANENT READ LOCKOUT. Once 50 distinct sessions had read a task, every
//     further session's plain GET answered 409 forever, because links are never
//     pruned. The documented escape hatch (omit the header) stopped being reachable
//     the moment the CLI started sending it unconditionally.
//
// So the cap is now ADVISORY and BEST-EFFORT, and this function CANNOT fail a
// request on account of it. The mechanism is INSERT-THEN-TRIM:
//
//  1. Upsert unconditionally (subject only to the live-parent guard).
//  2. Trim the thread back to MaxTaskSessions by evicting the oldest `read` links.
//     `created` and `worked` are NEVER evicted — they are the rows that record work.
//
// The row just written is by construction the NEWEST `read` link, so it is evicted
// LAST: it goes only when every other link is `created`/`worked`, i.e. when nothing
// else is evictable — which is exactly the "skip the link" case, arrived at as a
// consequence of the ordering rather than as a special case. That is reported as
// ErrTooManySessions, which callers LOG AND COUNT and never surface.
//
// Insert-then-trim also demotes the concurrency defect the audit measured (the old
// pre-check read the same MVCC snapshot as the INSERT, so 49 + 12 concurrent
// writers produced 57 rows). A racy HARD bound is a defect; a racy ADVISORY one is
// fine, and the trim is self-correcting — the next link on that task brings the
// count back down. This is emphatically NOT a security bound on attacker-influenced
// input; see MaxTaskSessions.
//
// The remaining properties, unchanged:
//
//   - LIVE PARENT ONLY, via the same `liveOnly` predicate every other write goes
//     through, spelled as a `WHERE EXISTS` exactly like AddComment. A dismissed
//     task must not gain a link: the FK still points at the surviving row, so a
//     bare upsert would write an edge into a thread nothing can see. Unknown or
//     dismissed id → pgx.ErrNoRows.
//   - MONOTONIC ROLE, via roleUpgradeExpr.
//   - MONOTONIC detail_seen, via detailSeenUpgradeExpr: every touch ORs the
//     caller's fresh `detailSeen` observation in, so the link records that a
//     transcript was once observed and keeps recording it after the 14-day sweep
//     removes the row. A plain assignment would reset it to false on the first
//     post-sweep touch, turning "expired" back into "never recorded".
//   - FIRST_SEEN_AT IS IMMUTABLE — absent from the DO UPDATE SET list.
//   - The denormalised context columns are only OVERWRITTEN by a non-empty incoming
//     value, so a context-less touch cannot blank what an earlier one recorded.
//
// 🔴 COALESCED last_seen_at (the reason for the `DO UPDATE … WHERE`). GET
// /api/tasks/{id} is a SAFE method that now writes, and `ON CONFLICT DO UPDATE`
// creates a dead tuple on EVERY execution even when every column is set to its own
// value — so a polled task would generate one dead tuple per read forever. The
// WHERE clause makes the update genuinely CONDITIONAL: a repeat touch that neither
// upgrades the role nor carries new context, inside lastSeenCoalesce of the
// previous one, updates NO ROW AT ALL. RETURNING then yields nothing, which is why
// the caller re-reads (see the coalesced branch below) rather than treating an
// empty result as a refusal.
//
// 🔴 detailSeen IS AN INPUT, NOT A PROBE, AND THE CALLER OWNS THE OBSERVATION.
// It answers "does a transcript record for l.SessionID exist RIGHT NOW", which this
// package used to ask in SQL (an `EXISTS` probe of the transcript table) and no
// longer may: that table belongs to another service and is not in this schema.
// It is a PARAMETER rather than a field on l so that adding it could not be
// silently skipped — every caller had to be recompiled and had to decide.
//
// Two consequences worth stating rather than discovering:
//
//   - PASSING false IS ALWAYS SAFE FOR THE STORED COLUMN. detailSeenUpgradeExpr
//     ORs it in, so a caller whose probe FAILED (or that has no session store at
//     all) can pass false and cannot thereby erase a true that was recorded
//     earlier. A failed probe degrades to "learned nothing", never to "unlearned".
//   - IT DOES NOT SET SessionLink.DetailAvailable ON THE RETURNED LINK. This
//     package no longer fills that field anywhere; the API layer composes it from
//     the same observation it passed in. See api.withSessionLiveness.
func (s *PGStore) LinkSession(ctx context.Context, noteID int64, l SessionLink, detailSeen bool) (SessionLink, bool, error) {
	if !ValidRole(l.Role) {
		return SessionLink{}, false, fmt.Errorf("invalid task-session role %q", l.Role)
	}
	if l.SessionID == "" {
		return SessionLink{}, false, fmt.Errorf("task-session link needs a session id")
	}
	var (
		out      SessionLink
		prevRole *string
	)
	err := s.pool.QueryRow(ctx, `
		WITH prev AS (
			SELECT role FROM task_sessions WHERE note_id = $1 AND session_id = $2
		), ins AS (
			INSERT INTO task_sessions (note_id, owner_id, session_id, role, project, cwd, host, detail_seen)
			SELECT $1, owner_id, $2, $3, $4, $5, $6, $8::boolean
			  FROM notes WHERE id = $1 AND `+liveOnly+`
			ON CONFLICT (note_id, session_id) DO UPDATE SET
				role         = `+roleUpgradeExpr+`,
				last_seen_at = now(),
				project      = CASE WHEN EXCLUDED.project <> '' THEN EXCLUDED.project ELSE task_sessions.project END,
				cwd          = CASE WHEN EXCLUDED.cwd     <> '' THEN EXCLUDED.cwd     ELSE task_sessions.cwd     END,
				host         = CASE WHEN EXCLUDED.host    <> '' THEN EXCLUDED.host    ELSE task_sessions.host    END,
				detail_seen  = `+detailSeenUpgradeExpr+`
			WHERE `+roleUpgradeExpr+` <> task_sessions.role
			   OR `+detailSeenUpgradeExpr+` <> task_sessions.detail_seen
			   OR task_sessions.last_seen_at < now() - $7::interval
			   OR (EXCLUDED.project <> '' AND EXCLUDED.project <> task_sessions.project)
			   OR (EXCLUDED.cwd     <> '' AND EXCLUDED.cwd     <> task_sessions.cwd)
			   OR (EXCLUDED.host    <> '' AND EXCLUDED.host    <> task_sessions.host)
			RETURNING session_id, role, project, cwd, host, first_seen_at, last_seen_at, detail_seen
		)
		SELECT ins.session_id, ins.role, ins.project, ins.cwd, ins.host, ins.first_seen_at, ins.last_seen_at,
		       ins.detail_seen,
		       (SELECT role FROM prev)
		  FROM ins`,
		noteID, l.SessionID, l.Role, l.Project, l.Cwd, l.Host, lastSeenCoalesce.String(), detailSeen).
		Scan(&out.SessionID, &out.Role, &out.Project, &out.Cwd, &out.Host,
			&out.FirstSeenAt, &out.LastSeenAt, &out.DetailSeen, &prevRole)
	if err != nil {
		if err == pgx.ErrNoRows {
			// TWO different situations produce an empty result, and they are not
			// interchangeable: the write was COALESCED away (the link already exists
			// and nothing about it needed to change), or the parent is not live. Read
			// the row back — through the live-parent join — to tell them apart, rather
			// than guessing.
			existing, ok, gerr := s.getLink(ctx, noteID, l.SessionID)
			if gerr != nil {
				return SessionLink{}, false, gerr
			}
			if ok {
				return existing, false, nil // coalesced no-op: nothing changed, nothing written
			}
			return SessionLink{}, false, pgx.ErrNoRows
		}
		return SessionLink{}, false, err
	}
	// `changed` stays a statement about the NEW-ROW / ROLE-UPGRADE pair only.
	// Callers broadcast an SSE nudge on it, and a detail_seen flip — while a real
	// write — changes only which of two labels the row prints, at most once per
	// link, and is picked up on the next render. Widening `changed` here would nudge
	// every open tab for a cosmetic transition; narrowing the WHERE above would drop
	// the write entirely. They are different decisions and only one of them is safe.
	changed := prevRole == nil || *prevRole != out.Role
	if prevRole == nil {
		// A NEW link may have pushed the thread past the advisory cap. Trim, and find
		// out whether the row we just wrote was the one evicted.
		evicted, terr := s.trimThread(ctx, noteID)
		if terr != nil {
			// The link IS written; a failed trim is a hygiene problem, not a reason to
			// tell the caller the link failed.
			return out, changed, nil
		}
		for _, sid := range evicted {
			if sid == out.SessionID {
				return SessionLink{}, false, fmt.Errorf(
					"%w: task %d already carries %d created/worked links, so this read was not recorded",
					ErrTooManySessions, noteID, MaxTaskSessions)
			}
		}
	}
	return out, changed, nil
}

// getLink reads one link through the SAME live-parent predicate every other read
// applies, so a link whose task was dismissed after the link was written reads back
// as ABSENT. That is what keeps the coalesced-write branch above from quietly
// turning a dismissed task's touch into a success.
func (s *PGStore) getLink(ctx context.Context, noteID int64, sessionID string) (SessionLink, bool, error) {
	var l SessionLink
	err := s.pool.QueryRow(ctx, `
		SELECT `+sessionCols+` `+sessionFrom+`
		 JOIN notes n ON n.id = ts.note_id
		WHERE ts.note_id = $1 AND ts.session_id = $2 AND n.`+liveOnly, noteID, sessionID).
		Scan(scanSessionLinkInto(&l)...)
	if err == pgx.ErrNoRows {
		return SessionLink{}, false, nil
	}
	if err != nil {
		return SessionLink{}, false, err
	}
	return l, true, nil
}

// trimThread enforces the ADVISORY cap after the fact: it deletes the oldest
// `read` links until the task carries at most MaxTaskSessions of them, and returns
// the session ids it evicted.
//
// 🔴 `created` and `worked` are NEVER evicted — those rows record that someone DID
// something, which is the entire point of the table; a `read` link is a
// nice-to-have breadcrumb. Ordering by last_seen_at ASC means the least recently
// active reader goes first, and (because a just-written link's last_seen_at is
// now()) the row a caller just inserted is evicted LAST — only when there is
// nothing else evictable at all.
//
// It is deliberately tolerant of a thread that is already over the cap for any
// reason (a concurrent writer, a cap that was lowered): it always trims down to the
// current constant rather than assuming it is exactly one over.
func (s *PGStore) trimThread(ctx context.Context, noteID int64) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		DELETE FROM task_sessions
		 WHERE note_id = $1
		   AND session_id IN (
		     SELECT session_id FROM task_sessions
		      WHERE note_id = $1 AND role = `+quotedRoleRead+`
		      ORDER BY last_seen_at ASC, session_id ASC
		      LIMIT GREATEST((SELECT count(*) FROM task_sessions WHERE note_id = $1) - $2, 0))
		RETURNING session_id`, noteID, MaxTaskSessions)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			return nil, err
		}
		out = append(out, sid)
	}
	return out, rows.Err()
}

// SessionsForTask returns one task's thread, oldest-joined first (first_seen_at,
// then session_id so ties are deterministic). A task nothing has touched yields an
// EMPTY slice, never an error.
func (s *PGStore) SessionsForTask(ctx context.Context, noteID int64) ([]SessionLink, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+sessionCols+` `+sessionFrom+`
		WHERE ts.note_id = $1 ORDER BY ts.first_seen_at, ts.session_id`, noteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]SessionLink, 0)
	for rows.Next() {
		var l SessionLink
		if err := rows.Scan(scanSessionLinkInto(&l)...); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// sessionsForNotes fetches every note id's thread in ONE query, in the same order
// SessionsForTask uses per note, so the board read and the single-task read can
// never disagree about ordering. It is the sessions twin of listCommentsForNotes.
func (s *PGStore) sessionsForNotes(ctx context.Context, ids []int64) ([]noteSessionLink, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT ts.note_id, `+sessionCols+` `+sessionFrom+`
		WHERE ts.note_id = ANY($1) ORDER BY ts.note_id, ts.first_seen_at, ts.session_id`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]noteSessionLink, 0)
	for rows.Next() {
		var nl noteSessionLink
		dest := append([]any{&nl.NoteID}, scanSessionLinkInto(&nl.Link)...)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		out = append(out, nl)
	}
	return out, rows.Err()
}

// TasksForSession is the REVERSE lookup: every LIVE task the session touched,
// most-recently-touched first (the ordering idx_task_sessions_reverse exists for).
//
// Dismissed tasks are excluded via the same `liveOnly` predicate every other read
// applies — a dismissed task answers 404 by id, so listing it here would hand a
// caller an id it cannot then fetch.
func (s *PGStore) TasksForSession(ctx context.Context, sessionID string) ([]TaskLink, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT n.id, n.title, n.directory, n.status, ts.role, ts.first_seen_at, ts.last_seen_at
		  FROM task_sessions ts
		  JOIN notes n ON n.id = ts.note_id
		 WHERE ts.session_id = $1 AND n.`+liveOnly+`
		 ORDER BY ts.last_seen_at DESC, n.id DESC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]TaskLink, 0)
	for rows.Next() {
		var t TaskLink
		if err := rows.Scan(&t.NoteID, &t.Title, &t.Directory, &t.Status, &t.Role, &t.FirstSeenAt, &t.LastSeenAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
