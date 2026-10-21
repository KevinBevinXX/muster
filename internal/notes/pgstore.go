package notes

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/taskstatus"
)

// PGStore is the Postgres-backed notes Store.
type PGStore struct{ pool *pgxpool.Pool }

// NewPG constructs a Postgres-backed notes Store.
func NewPG(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

// Create inserts a note (with its dispatch config) and returns it with its
// generated ID and timestamps.
func (s *PGStore) Create(ctx context.Context, n Note) (Note, error) {
	// pgx encodes a nil slice as SQL NULL, which the NOT NULL DEFAULT '{}' tags
	// column rejects: normalize to a non-nil empty slice so a tagless task stores
	// '{}' rather than failing on NULL.
	n.Tags = NormalizeTags(n.Tags)
	// source_type / source_session_id are nullable (*string); a nil pointer encodes
	// as SQL NULL (a pre-0017 / header-less task), which reads back as NULL.
	err := s.pool.QueryRow(ctx, `
		INSERT INTO notes (directory, title, body, model, repo, repo_branch, tags, source_type, source_session_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING `+noteCols,
		n.Directory, n.Title, n.Body, n.Model, n.Repo, n.RepoBranch, n.Tags, n.SourceType, n.SourceSessionID).
		Scan(scanNoteInto(&n)...)
	return n, err
}

// noteCols is the SINGLE column list every full-note read uses, so Create/Get/
// List/ListByTags can never drift out of sync with scanNoteInto's target order.
const noteCols = `id, directory, title, body, status, model, repo, repo_branch, tags, source_type, source_session_id, created_at, updated_at, owner_id`

// liveOnly is the SINGLE soft-delete predicate, spelled once so no read or write
// path can quietly opt out of it. It now covers BOTH soft-deleted tables — notes
// and note_comments — because the column name
// and the semantics are identical; a second spelling would be a second convention
// to drift.
//
// A dismissed task carries a non-NULL notes.deleted_at and must be invisible to
// EVERY application path — the board, the machine API, the dispatch picker, the
// tag vocabulary, the idle-task reaper (which would otherwise re-reap dismissed
// rows forever) and every mutator.
//
// 🔴 It is deliberately applied to the WRITES as well as the reads. Filtering only
// the reads would leave `SetStatus`/`UpdateNote`/`AddTags` half-writing: the UPDATE
// would land on the dismissed row and the follow-up Get would then answer
// ErrNoRows, i.e. a mutation that reports failure while having succeeded. With the
// predicate on both, a dismissed task simply does not exist until Restore.
//
// COMMENTS are NOT filtered by liveOnly on the reads — they are REDACTED by
// commentCols instead. See that constant for why.
//
// ListAttachments / GetAttachment stay unfiltered — attachments have no
// deleted_at, and they are by-note-id child reads every handler reaches only
// through a parent Get. That is also what lets an operator enumerate a dismissed
// task's surviving attachments while deciding whether to Restore it.
const liveOnly = `deleted_at IS NULL`

// ownerOfNote is the owner_id a CHILD row of note $1 is inserted with: the
// parent's own (accounts PR 3a). 0006's composite foreign keys already demand
// child.owner_id = parent.owner_id, so this is the only legal value — and it is
// right for a background loop running as authz.System too, which names no user
// for owner_id's default to take. A parent the caller cannot see (row-level
// security) or that does not exist yields the caller's own id instead, so the
// foreign key still refuses the row exactly as it did before (23503).
const ownerOfNote = `COALESCE((SELECT owner_id FROM notes WHERE id = $1), nullif(current_setting('muster.uid', true), '')::bigint)`

// commentCols is the SINGLE comment projection — the ONE place
// retraction is applied on a read, used by both comment reads (ListComments and
// listCommentsForNotes) so they cannot drift, exactly as noteCols keeps the note
// reads aligned. Its column order matches scanCommentInto.
//
// 🔴 REDACT, DO NOT FILTER — and redact IN POSTGRES. Two independent reasons:
//
//  1. A retracted comment stays in the thread as a TOMBSTONE so the thread cannot
//     silently SHORTEN. A comment that merely vanishes is the worse failure mode
//     on a board agents read as authoritative: a quiet mass retraction leaves no
//     trace at all, whereas the task delete it was modelled on at least makes a
//     card visibly disappear. Position and created_at survive; only the body goes.
//  2. The CASE runs server-side, so the retracted text is never selected out of
//     the database. That is stronger than hiding it in the renderer: no read path
//     — the HTML card, the machine JSON, a future export — can leak a body it was
//     never given. "Absent from the page" is a property of the query here, not a
//     property of each consumer remembering to check the flag.
//
// The knock-on that keeps this ONE rule rather than two: because the row is still
// returned, len(Note.Comments) counts tombstones, so apiTaskSummary.CommentCount
// and the card's "N comments" badge (both plain len()) stay consistent with what
// is rendered without either of them learning about retraction.
//
// 🔴 The empty-string branch is deliberately NOT the placeholder text. The
// tombstone COPY belongs to the renderer (internal/ui); baking it into SQL would
// make it indistinguishable from a real comment body whose author typed it.
const commentCols = `id, note_id, author, CASE WHEN deleted_at IS NULL THEN body ELSE '' END, created_at, (deleted_at IS NOT NULL)`

// scanCommentInto returns the Scan destinations for commentCols, in order.
func scanCommentInto(c *Comment) []any {
	return []any{&c.ID, &c.NoteID, &c.Author, &c.Body, &c.CreatedAt, &c.Retracted}
}

// scanNoteInto returns the Scan destinations for noteCols, in order.
func scanNoteInto(n *Note) []any {
	return []any{
		&n.ID, &n.Directory, &n.Title, &n.Body, &n.Status, &n.Model, &n.Repo, &n.RepoBranch,
		&n.Tags, &n.SourceType, &n.SourceSessionID, &n.CreatedAt, &n.UpdatedAt, &n.OwnerID,
	}
}

// Get returns a note with its attachment metadata (no blob data) and comments. A
// soft-deleted (dismissed) task answers pgx.ErrNoRows, which is what keeps
// DELETE/GET /api/tasks/{id} a 404 for a dismissed id — the same answer they gave
// when the row was really deleted.
func (s *PGStore) Get(ctx context.Context, id int64) (Note, error) {
	var n Note
	err := s.pool.QueryRow(ctx, `SELECT `+noteCols+` FROM notes WHERE id=$1 AND `+liveOnly, id).Scan(scanNoteInto(&n)...)
	if err != nil {
		return Note{}, err
	}
	atts, err := s.ListAttachments(ctx, id)
	if err != nil {
		return Note{}, err
	}
	n.Attachments = atts
	comments, err := s.ListComments(ctx, id)
	if err != nil {
		return Note{}, err
	}
	n.Comments = comments
	// The task's thread , embedded exactly as the comments are.
	sessions, err := s.SessionsForTask(ctx, id)
	if err != nil {
		return Note{}, err
	}
	n.Sessions = sessions
	return n, nil
}

// qualifyCols prefixes every column in a comma-joined projection with a table
// alias. It is DERIVED rather than a second hand-written list, for exactly the
// reason noteCols exists at all: a second spelling of the same 14 columns would
// be a second thing to keep in sync with scanNoteInto's target order, and the
// drift would be a silent mis-scan rather than a compile error.
func qualifyCols(alias, cols string) string {
	parts := strings.Split(cols, ", ")
	for i := range parts {
		parts[i] = alias + "." + parts[i]
	}
	return strings.Join(parts, ", ")
}

// noteColsN is noteCols qualified for the aliased `notes n` used by the activity
// query below — where a bare `id` would be ambiguous against the lateral joins.
var noteColsN = qualifyCols("n", noteCols)

// taskActivityExpr is the ordering key: the most recent of the note's own
// updated_at and the three activity sources joined in by ListPage.
//
// GREATEST IGNORES NULLs in Postgres (it is NULL only if every argument is), and
// n.updated_at is NOT NULL — so a task with no comments, no agent and no session
// links orders by exactly the old key. That is what makes the new rule a
// SUPERSET of `ORDER BY updated_at DESC` rather than a different one: the key is
// never smaller than updated_at, and it equals it whenever nothing else has
// happened.
//
// 🔴 IT IS SELECTED INTO ITS OWN FIELD, Note.ActivityAt, AND NEVER INTO
// UpdatedAt. notes.updated_at is also the CARD REVISION (rendered as
// data-card-rev, consumed by refuseStaleCardSwap), and the one way to wedge a
// card permanently is to hand the renderer a different number for it. This
// comment used to say the value was deliberately NOT selected so there was no
// field for it to leak through; it is selected now because the board card prints
// it (a card showing its CREATION age over an activity-ordered list read as an
// unsorted list), into a field whose name is not the revision and whose JSON tag
// is "-". TestListPageActivityIsNotTheCardRevision pins the separation.
const taskActivityExpr = `GREATEST(n.updated_at, cm.at, ag.at, sl.at)`

// taskActivityOrder is the board's ordering, spelled once.
//
// The `n.id DESC` tiebreak is not decoration: two tasks created by the same
// batch (a merge, a backfill, an import) can share an activity timestamp to the
// microsecond, and without a deterministic second key their relative order is
// whatever the plan happens to produce — which makes an idiomorph swap reorder
// cards for no reason the user can see. id DESC also agrees with the primary
// key's direction, so the tie resolves "newer task first".
const taskActivityOrder = taskActivityExpr + ` DESC, n.id DESC`

// List returns all notes (most recent ACTIVITY first) with attachment metadata
// and comments. It runs a constant number of queries regardless of note count:
// one for the notes, then one batched query each for all attachments and all
// comments of the returned ids (grouped per note in Go). The empty case skips
// the batch queries entirely.
func (s *PGStore) List(ctx context.Context) ([]Note, error) {
	return s.ListByTags(ctx, nil)
}

// ListByTags returns the notes carrying EVERY tag in tags (AND semantics via the
// GIN-indexed `tags @> $1` containment operator), most-recent-activity first,
// with the same batched attachment/comment fan-out as List. An empty/nil tags
// slice degenerates to "all notes" (List delegates here), so the two paths can
// never drift. A filter matching nothing returns an EMPTY slice, not an error.
func (s *PGStore) ListByTags(ctx context.Context, tags []string) ([]Note, error) {
	p, err := s.ListPage(ctx, ListFilter{Tags: tags})
	return p.Notes, err
}

// ListPage is the ONE list query. List and ListByTags are thin delegates, so the
// activity ordering, the soft-delete predicate and the batched child fan-out
// exist in exactly one place and cannot drift between the board and every other
// caller.
func (s *PGStore) ListPage(ctx context.Context, f ListFilter) (Page, error) {
	// The three LATERAL sub-selects that make "when did anything last happen on
	// this task" computable WITHOUT a schema change.
	//
	// 🔴 IT IS A LOCAL CONST, NOT A PACKAGE ONE, AND THAT IS DELIBERATE. It names
	// note_comments, and TestNoteCommentsReadPathLedger enumerates every FUNCTION
	// in this file that issues SQL against that table specifically so a query
	// cannot hide in a package-level constant. Parking it outside a function body
	// would make this query invisible to that ledger — see the ListPage entry in
	// wantCommentSites for what the ledger can and cannot say about it.
	//
	// 🔴 EACH JOIN IS A DIFFERENT PRODUCER, AND THEY DO NOT ALL FEED updated_at.
	// Measured on this tree rather than assumed:
	//
	//   - note_comments — AddComment ALREADY bumps notes.updated_at in the same
	//     statement (see the `bump` CTE there), so for every comment this lateral
	//     actually COUNTS today this term is defence-in-depth, not the live
	//     mechanism. (The qualifier is load-bearing: PGStore.FlagIdle is a second
	//     store comment writer and it deliberately does NOT bump updated_at — but
	//     its comments are exactly the ones the author filter below removes, so
	//     "everything counted here also bumped" stays true.) It stays because it
	//     costs nothing and the invariant it protects ("a thread with newer
	//     activity than its note sorts by the activity") is one a future comment
	//     path — a backfill, a direct INSERT, a migration — would otherwise break
	//     silently.
	//
	//     🔴 THE `c.author <> ReapCommentAuthor` FILTER IS DONE, NOT OUTSTANDING —
	//     it is in the SQL below. This entry used to be a MERGE-ORDER CONSTRAINT
	//     addressed to whichever of #727/#729 merged second; #727 landed first
	//     (1041d87ce) and #729 carries the filter. What it guards, so nobody
	//     deletes it as noise: PGStore.FlagIdle writes a ReapCommentAuthor comment
	//     WITHOUT bumping notes.updated_at — deliberately, because updated_at is
	//     the reaper's own selection predicate. Unfiltered, GREATEST(...) would
	//     resolve to that comment's created_at = now() and float a task nobody
	//     touched to the TOP of the board, reintroducing the inverted ordering this
	//     flat board exists to remove. Neither PR's own tests could see it: the
	//     break needs BOTH sides. Pinned behaviourally by
	//     TestReapedTaskDoesNotFloatToTheTopOfTheBoard.
	//
	//     🔴 IT IS THE ONLY AUTHOR EXCLUDED, AND THAT IS THE RULE, NOT A LIST. The
	//     filter is not "hide robots" — every other comment author, human or agent,
	//     IS activity and must float its task. What earns the exclusion is the one
	//     property FlagIdle has and nothing else does: it writes a comment while
	//     deliberately NOT writing updated_at. Any future writer with that property
	//     needs the same treatment; any writer that bumps updated_at does not.
	//   - agents — every agent mutation writes `agents.updated_at = now()` and
	//     NONE of them touch the note. An agent that provisions, runs and errors
	//     moved the task not at all under the old ordering. This term is why a
	//     task being worked RIGHT NOW floats to the top.
	//   - task_sessions — LinkSession writes last_seen_at and, likewise, never
	//     bumps the note. A session picking a task up is real activity the board
	//     could not see.
	//
	// 🔴 THE COMMENT LATERAL CARRIES NO `deleted_at IS NULL` — and that omission is
	// deliberate, not an oversight the author filter above forgot to extend. A
	// RETRACTED comment still happened, and retracting one must not silently sink
	// the task back down the board; that is the same "tombstone, do not filter"
	// rule comment soft delete applies to the thread itself. Pinned behaviourally by
	// TestListPageRetractedCommentStillFloatsTheTask.
	//
	// ⚠ The two predicates are opposites on purpose and must not be "unified": the
	// AUTHOR filter removes an event that never was activity, the absent DELETED_AT
	// filter keeps an event that was activity and later got retracted. A retracted
	// REAP comment is excluded by the first, which is also correct — it was never
	// activity either.
	//
	// LATERAL + max() rather than LEFT JOIN + GROUP BY: the plain join would
	// multiply note rows by their comments before aggregating, which then has to
	// be undone with a GROUP BY over all 14 note columns. The lateral keeps the
	// query one-row-per-note by construction.
	//
	// The reap-author exclusion is CONCATENATED FROM THE Go CONSTANT rather than
	// bound as a query parameter, deliberately: ReapCommentAuthor is a compile-time
	// const so there is nothing to inject, the joins are built before the WHERE
	// clause's $N numbering starts (a bound author would have to be $1 and shift
	// every filter argument), and spelling it as the constant is what makes
	// renaming the author a compile-time break here instead of a silent one.
	const activityJoins = `
	  LEFT JOIN LATERAL (SELECT max(c.created_at)   AS at FROM note_comments c WHERE c.note_id = n.id AND c.author <> '` + ReapCommentAuthor + `') cm ON true
	  LEFT JOIN LATERAL (SELECT max(a.updated_at)   AS at FROM agents        a WHERE a.note_id = n.id) ag ON true
	  LEFT JOIN LATERAL (SELECT max(l.last_seen_at) AS at FROM task_sessions l WHERE l.note_id = n.id) sl ON true`

	// `n.`+liveOnly rather than a second spelling of the predicate: liveOnly is
	// the single soft-delete rule and this query merely needs it qualified for
	// the alias.
	where := []string{"n." + liveOnly}
	args := []any{}
	if want := NormalizeTags(f.Tags); len(want) > 0 {
		args = append(args, want)
		where = append(where, fmt.Sprintf("n.tags @> $%d", len(args)))
	}
	if len(f.Statuses) > 0 {
		args = append(args, f.Statuses)
		where = append(where, fmt.Sprintf("n.status = ANY($%d)", len(args)))
	}
	// COUNT(*) OVER () is evaluated over the whole filtered set BEFORE LIMIT, so
	// one query answers both "this page" and "how many matched" — which is what
	// lets the show-more control say 50 of 293 without a second round trip, and
	// what keeps the two numbers from disagreeing across a concurrent write.
	q := `SELECT ` + noteColsN + `, ` + taskActivityExpr + ` AS activity_at, COUNT(*) OVER () AS total_matching
		    FROM notes n` + activityJoins + `
		   WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY ` + taskActivityOrder
	if f.Limit > 0 {
		args = append(args, f.Limit)
		q += fmt.Sprintf(" LIMIT $%d", len(args))
	}
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return Page{}, err
	}
	defer rows.Close()
	out := make([]Note, 0)
	total := int64(0)
	for rows.Next() {
		var n Note
		var rowTotal int64
		if err := rows.Scan(append(scanNoteInto(&n), &n.ActivityAt, &rowTotal)...); err != nil {
			return Page{}, err
		}
		total = rowTotal
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return Page{}, err
	}
	if len(out) == 0 {
		// No rows means no window value came back either. Zero is the honest total
		// here, and it is what keeps "0 of 0" from ever rendering a show-more.
		return Page{Notes: out, Total: 0}, nil
	}

	ids := make([]int64, len(out))
	for i := range out {
		ids[i] = out[i].ID
	}

	atts, err := s.listAttachmentsForNotes(ctx, ids)
	if err != nil {
		return Page{}, err
	}
	comments, err := s.listCommentsForNotes(ctx, ids)
	if err != nil {
		return Page{}, err
	}
	groupNoteChildren(out, atts, comments)
	// One more batched query for the task threads , grouped the
	// same way — so the board's cards carry their session counts without a
	// per-card query.
	links, err := s.sessionsForNotes(ctx, ids)
	if err != nil {
		return Page{}, err
	}
	groupSessionLinks(out, links)
	return Page{Notes: out, Total: int(total)}, nil
}

// CountByStatus counts live tasks in one status. The deleted_at predicate is the
// same soft-delete rule every other read applies: a dismissed task does not
// exist until Restore, so it must not keep a badge lit either.
func (s *PGStore) CountByStatus(ctx context.Context, status string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM notes WHERE status = $1 AND deleted_at IS NULL`, status).Scan(&n)
	return n, err
}

// ListSummaries returns every LIVE note's own columns (newest-updated first) in a
// SINGLE query, with NO attachment/comment/session fan-out. List does four
// queries and hydrates every attachment, EVERY COMMENT OF EVERY NOTE and every
// session link, which is what the Tasks page needs and what its two other callers
// do not:
//
//   - the dispatch-modal note picker renders only an id + a label (directory,
//     falling back to the body's first line);
//   - the idle-task reaper (api.reapIdleTasks) reads only id + updated_at, and
//     since 0.8.x runs every 30 minutes rather than daily, so the fan-out it does
//     not use would be paid ~48× a day over a set that only grows.
//
// 🔴 updated_at IS IN THE PROJECTION and must stay. It is the reaper's entire
// selection predicate, and a Note scanned without it carries the ZERO time, which
// is BEFORE every cutoff — a sweep over such rows would consider the whole board
// idle. It is also the card revision behind data-card-rev, where a zero renders
// as a rev older than every real one and wedges the card (ui.renderDetailCard,
// TestZeroUpdatedAtOmitsTheCardRev): no store projection may omit it.
//
// ⚠ It deliberately keeps `ORDER BY updated_at DESC` while List/ListPage moved to
// the activity ordering. This is a PICKER, not the board: it is a flat <select>
// the user scans for a task they already have in mind, the three activity joins
// would triple its cost for a list that shows no activity at all, and it does not
// carry the show-more paging the ordering exists to make coherent. Stated so the
// difference reads as a decision rather than an oversight.
//
// 🔴 The two paragraphs above are BOTH load-bearing and they are about different
// things: the first is about the PROJECTION (updated_at must be selected), the
// second about the ORDER BY (activity ordering deliberately not adopted here).
// Neither supersedes the other — a reaper sweep that lost the column and a picker
// that grew three joins are separate regressions, each earned once already.
func (s *PGStore) ListSummaries(ctx context.Context) ([]Note, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, directory, title, body, model, repo, repo_branch, tags, updated_at, owner_id
		FROM notes WHERE `+liveOnly+` ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Note, 0)
	for rows.Next() {
		var n Note
		if err := rows.Scan(&n.ID, &n.Directory, &n.Title, &n.Body, &n.Model, &n.Repo, &n.RepoBranch, &n.Tags, &n.UpdatedAt, &n.OwnerID); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// AddTags set-UNIONs tags into a task's tag set (and bumps updated_at). It is a
// SINGLE statement — `unnest(tags || $2)` re-aggregated DISTINCT + ORDER BY —
// which is what makes it safe against the LOST-UPDATE class: two producers each
// adding a different tag concurrently both win, whereas a read-modify-write (or
// UpdateNote's replace semantics) would silently drop one. Idempotent: re-adding
// an existing tag is a no-op. Unknown id → pgx.ErrNoRows.
//
// ⚠ "Safe" here means safe against LOST UPDATES ONLY — it is not a validation
// boundary and must not be read as one. This statement runs NormalizeTags and
// nothing else: no ValidateTags, no MaxTags check, no at-most-one-`project:`
// check. Every guard on those lives at the CALLER (notes.MergeTaskTags for the
// merge path, the PATCH handler for the machine path), and those callers READ
// then WRITE without a transaction — so two concurrent adders can each see a
// legal set and together push the row past MaxTags or onto two projects. That
// residual is accepted, not closed; closing it needs a conditional UPDATE the
// Store interface does not expose.
func (s *PGStore) AddTags(ctx context.Context, id int64, tags []string) (Note, error) {
	add := NormalizeTags(tags)
	if len(add) == 0 {
		return s.Get(ctx, id)
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE notes
		   SET tags = COALESCE((SELECT array_agg(DISTINCT t ORDER BY t) FROM unnest(tags || $2::text[]) AS t), '{}'),
		       updated_at = now()
		 WHERE id = $1 AND `+liveOnly, id, add)
	if err != nil {
		return Note{}, err
	}
	if tag.RowsAffected() == 0 {
		return Note{}, pgx.ErrNoRows
	}
	return s.Get(ctx, id)
}

// RemoveTags set-DIFFERENCEs tags out of a task's tag set (and bumps updated_at),
// as a single statement for the same no-lost-update reason as AddTags. Idempotent:
// removing an absent tag is a no-op. Unknown id → pgx.ErrNoRows.
func (s *PGStore) RemoveTags(ctx context.Context, id int64, tags []string) (Note, error) {
	drop := NormalizeTags(tags)
	if len(drop) == 0 {
		return s.Get(ctx, id)
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE notes
		   SET tags = COALESCE((SELECT array_agg(t ORDER BY t) FROM unnest(tags) AS t WHERE t <> ALL($2::text[])), '{}'),
		       updated_at = now()
		 WHERE id = $1 AND `+liveOnly, id, drop)
	if err != nil {
		return Note{}, err
	}
	if tag.RowsAffected() == 0 {
		return Note{}, pgx.ErrNoRows
	}
	return s.Get(ctx, id)
}

// TagVocabulary returns every tag in use with how many tasks carry it, ordered by
// count DESC then tag ASC (ties deterministic, so the chip row and tests are
// stable). Backs the filter chip row and the editor's datalist.
func (s *PGStore) TagVocabulary(ctx context.Context) ([]TagCount, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t, count(*) FROM notes, unnest(tags) AS t
		WHERE `+liveOnly+` GROUP BY t ORDER BY count(*) DESC, t ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]TagCount, 0)
	for rows.Next() {
		var tc TagCount
		if err := rows.Scan(&tc.Tag, &tc.Count); err != nil {
			return nil, err
		}
		out = append(out, tc)
	}
	return out, rows.Err()
}

// listAttachmentsForNotes fetches attachment metadata (no blob data) for every
// note id in one query. Ordering mirrors ListAttachments (note_id, id) so the
// per-note slices come out in the same order as the per-note path.
func (s *PGStore) listAttachmentsForNotes(ctx context.Context, ids []int64) ([]Attachment, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, note_id, filename, content_type, size_bytes, created_at
		FROM note_attachments WHERE note_id = ANY($1) ORDER BY note_id, id`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Attachment, 0)
	for rows.Next() {
		var a Attachment
		if err := rows.Scan(&a.ID, &a.NoteID, &a.Filename, &a.ContentType, &a.SizeBytes, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// listCommentsForNotes fetches comments for every note id in one query, oldest
// first within each note (created_at, id) to match ListComments.
//
// This is the BOARD's comment read, the twin of ListComments, and it goes through
// the SAME commentCols projection — so a retracted comment
// arrives here as a body-less tombstone exactly as it does on the task view.
// Redacting only the per-note read would leave the retracted TEXT rendering on the
// card in the list while being gone from the task's own thread.
func (s *PGStore) listCommentsForNotes(ctx context.Context, ids []int64) ([]Comment, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+commentCols+`
		FROM note_comments WHERE note_id = ANY($1) ORDER BY note_id, created_at, id`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Comment, 0)
	for rows.Next() {
		var c Comment
		if err := rows.Scan(scanCommentInto(&c)...); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetStatus updates a task's lifecycle status (and bumps updated_at) and returns
// the refreshed note. The caller is responsible for authorizing the transition
// (e.g. only the operator/human may set StatusComplete).
//
// 🔴 THE PREVIOUS STATUS COMES FROM THE SAME STATEMENT (WithPreviousStatus).
// The row is selected FOR UPDATE in a CTE with every condition of the write,
// and the UPDATE returns that locked version's status — so "what it was" is
// exactly the value this write replaced, never a separate read another writer
// could slip in front of.
func (s *PGStore) SetStatus(ctx context.Context, id int64, status string) (Note, error) {
	sel := `SELECT id, status FROM notes WHERE id=$1 AND ` + liveOnly
	if forAgent(ctx) {
		// An agent's write: never out of complete, decided by the row itself.
		sel += ` AND status <> '` + StatusComplete + `'`
	}
	args := []any{id, status}
	from, conditional := onlyFrom(ctx)
	if conditional {
		sel += ` AND status = $3`
		args = append(args, from)
	}
	q := `WITH old AS (` + sel + ` FOR UPDATE)
		UPDATE notes SET status=$2, updated_at=now() FROM old WHERE notes.id = old.id RETURNING old.status`
	var prev string
	err := s.pool.QueryRow(ctx, q, args...).Scan(&prev)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Note{}, err
	}
	if err == nil {
		recordPreviousStatus(ctx, prev)
	} else {
		if conditional {
			if _, gerr := s.Get(ctx, id); gerr == nil {
				return Note{}, ErrStatusMoved
			}
		}
		if forAgent(ctx) {
			if cur, gerr := s.Get(ctx, id); gerr == nil && cur.Status == StatusComplete {
				return cur, ErrCompleteLocked
			}
		}
		return Note{}, pgx.ErrNoRows
	}
	return s.Get(ctx, id)
}

// UpdateNote applies a partial edit to a task's editable content + dispatch
// config, bumps updated_at, and returns the refreshed note. It builds the UPDATE
// dynamically so ONLY the columns whose patch field is non-nil are touched —
// status, source_type/source_session_id, and created_at are never in the SET list,
// so an edit can never change a task's lifecycle status or provenance. With no
// fields provided it still bumps updated_at (and validates the row exists). An
// unknown id → pgx.ErrNoRows.
func (s *PGStore) UpdateNote(ctx context.Context, id int64, patch NoteUpdate) (Note, error) {
	sets := make([]string, 0, 6)
	// $1 is always the id (the WHERE clause); provided columns bind $2, $3, …
	args := []any{id}
	add := func(col string, val any) {
		args = append(args, val)
		sets = append(sets, fmt.Sprintf("%s=$%d", col, len(args)))
	}
	if patch.Directory != nil {
		add("directory", *patch.Directory)
	}
	if patch.Title != nil {
		add("title", *patch.Title)
	}
	if patch.Tags != nil {
		// REPLACE semantics. NormalizeTags is total and always returns a non-nil
		// slice, so a provided-but-nil / cleared value stores '{}' rather than
		// tripping the NOT NULL column (mirrors Create).
		add("tags", NormalizeTags(*patch.Tags))
	}
	if patch.Body != nil {
		add("body", *patch.Body)
	}
	if patch.Model != nil {
		add("model", *patch.Model)
	}
	if patch.Repo != nil {
		add("repo", *patch.Repo)
	}
	if patch.RepoBranch != nil {
		add("repo_branch", *patch.RepoBranch)
	}
	// updated_at is ALWAYS bumped (an edit is a change); status/provenance/created_at
	// are deliberately absent from the SET list.
	sets = append(sets, "updated_at=now()")
	q := `UPDATE notes SET ` + strings.Join(sets, ", ") + ` WHERE id=$1 AND ` + liveOnly
	tag, err := s.pool.Exec(ctx, q, args...)
	if err != nil {
		return Note{}, err
	}
	if tag.RowsAffected() == 0 {
		return Note{}, pgx.ErrNoRows
	}
	return s.Get(ctx, id)
}

// AddComment appends a comment to a task's thread (and bumps the note's
// updated_at so it floats to the top of the list).
//
// The INSERT is conditional on the parent being LIVE. The note_comments FK still
// points at a soft-deleted row, so a bare INSERT would happily succeed and write
// the comment into a thread nothing can see — the silent-write class this codebase
// refuses elsewhere. Guarding it in the same statement (rather than a separate
// existence check) keeps it atomic; no rows returned → pgx.ErrNoRows, which the
// agent/operator comment handlers already map to a loud "task not found".
//
// 🔴 THE updated_at BUMP IS IN THIS STATEMENT, AND IT HAS TO BE. Three places
// justify the data-card-rev stale-swap guard with "SetStatus, Edit, AddTags,
// RemoveTags and AddComment all write updated_at=now() in the same statement as
// the change" (internal/ui/notes.go, card_revision_test.go,
// updated_at_is_a_version_test.go). That held for every writer EXCEPT this one:
// the bump used to be a SECOND round-trip whose error was discarded outright —
// `_, _ = s.pool.Exec(...)` — so a failed bump left the rev behind the thread
// that changed, and the guard's soundness argument named a property the code did
// not have. It was benign only because the guard
// APPLIES TIES; that is a different reason from the one written down, and a
// reader auditing the argument would have concluded it held.
//
// The data-modifying CTE restores the stated property: one statement, one
// round-trip, no error left to discard. `bump` reads note_id FROM ins, so an
// INSERT that wrote nothing (parent missing or soft-deleted) yields NULL and
// updates no row — the live-parent guard is preserved and QueryRow still returns
// pgx.ErrNoRows.
func (s *PGStore) AddComment(ctx context.Context, c Comment) (Comment, error) {
	err := s.pool.QueryRow(ctx, `
		WITH ins AS (
			INSERT INTO note_comments (note_id, owner_id, author, body)
			SELECT id, owner_id, $2, $3 FROM notes WHERE id=$1 AND `+liveOnly+`
			RETURNING id, note_id, author, body, created_at
		), bump AS (
			UPDATE notes SET updated_at=now() WHERE id=(SELECT note_id FROM ins)
		)
		SELECT id, note_id, author, body, created_at FROM ins`,
		c.NoteID, c.Author, c.Body).
		Scan(&c.ID, &c.NoteID, &c.Author, &c.Body, &c.CreatedAt)
	if err != nil {
		return Comment{}, err
	}
	return c, nil
}

// onceKeyLockPrefix namespaces CreateOnceByTag's advisory-lock key, so it cannot
// share a lock id with any other user of pg_advisory_xact_lock(bigint).
const onceKeyLockPrefix = "muster.notes.once-by-tag:"

// CreateOnceByTag: see the Store interface.
//
// 🔴 THE LOCK IS WHAT MAKES "ONCE" TRUE; THE LOOKUP ALONE IS A RACE. Two callers
// that both run the SELECT before either INSERT commits both see nothing and both
// insert. The transaction-scoped advisory lock on the key serialises them, and the
// SELECT is a NEW command after the lock is granted, so at READ COMMITTED it sees
// the winner's committed row. TestCreateOnceByTagCreatesOneUnderConcurrency is the
// property; deleting the lock statement fails it.
//
// The lock is keyed on the tag alone, not the owner: two owners filing the same
// key only wait for each other. Row-level security still confines the lookup to
// the caller's own tasks, so each owner gets their own task.
func (s *PGStore) CreateOnceByTag(ctx context.Context, key string, n Note, c *Comment) (Note, bool, bool, error) {
	if k := NormalizeTag(key); k == "" || k != key {
		return Note{}, false, false, fmt.Errorf("notes: once-key %q is not a normalized tag", key)
	}
	n.Tags = MergeTags(n.Tags, []string{key})
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Note{}, false, false, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, onceKeyLockPrefix+key); err != nil {
		return Note{}, false, false, err
	}
	found, ok, err := findLiveByTag(ctx, tx, key)
	if err != nil {
		return Note{}, false, false, err
	}
	created := !ok
	if ok {
		n = found
	} else if err := tx.QueryRow(ctx, `
		INSERT INTO notes (directory, title, body, model, repo, repo_branch, tags, source_type, source_session_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING `+noteCols,
		n.Directory, n.Title, n.Body, n.Model, n.Repo, n.RepoBranch, n.Tags, n.SourceType, n.SourceSessionID).
		Scan(scanNoteInto(&n)...); err != nil {
		return Note{}, false, false, err
	}
	// 🔴 THE COMMENT IS IN THIS TRANSACTION ON BOTH PATHS. Written after the
	// commit (as it once was), a create whose answer was lost left the task
	// without its comment, and the resend — finding the task — then added it:
	// two different outcomes for one request.
	commented := false
	if c != nil {
		cc := *c
		cc.NoteID = n.ID
		if commented, err = addCommentOnceTx(ctx, tx, cc); err != nil {
			return Note{}, false, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Note{}, false, false, err
	}
	return n, created, commented, nil
}

// FindLiveByTag: see the Store interface.
func (s *PGStore) FindLiveByTag(ctx context.Context, key string) (Note, bool, error) {
	return findLiveByTag(ctx, s.pool, key)
}

// rowQuerier is what findLiveByTag needs: the pool, or CreateOnceByTag's tx.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// openOnly is what makes a task THE OPEN task for its key (the operator's rule):
// live (liveOnly — not dismissed), not complete, and not a merge loser — a merge
// marks the loser complete AND tags it `superseded-by:<winner>`, and the winner
// gets the union of tags, so the winner is what a later notice finds. The
// superseded-by test stands on its own, so a loser someone re-opened by hand
// is still not taken for the key. p is the placeholder bound to
// supersededPattern; status values come from taskstatus (the one vocabulary).
func openOnly(p string) string {
	return liveOnly + ` AND status <> '` + taskstatus.Complete + `'
		AND NOT EXISTS (SELECT 1 FROM unnest(tags) AS t WHERE t LIKE ` + p + `)`
}

// supersededPattern is openOnly's LIKE argument: any `superseded-by:` tag.
const supersededPattern = NSSupersededBy + ":%"

// findLiveByTag is THE answer to "which open task carries key": the OLDEST, so
// CreateOnceByTag (which would otherwise create) and FindLiveByTag (the plugin
// task route's comment path) can never name different tasks for one key — the
// two used to be separately written lookups (audit D1).
func findLiveByTag(ctx context.Context, q rowQuerier, key string) (Note, bool, error) {
	var n Note
	err := q.QueryRow(ctx, `SELECT `+noteCols+` FROM notes WHERE tags @> ARRAY[$1]::text[] AND `+openOnly("$2")+`
		ORDER BY id LIMIT 1`, key, supersededPattern).Scan(scanNoteInto(&n)...)
	switch {
	case err == nil:
		return n, true, nil
	case errors.Is(err, pgx.ErrNoRows):
		return Note{}, false, nil
	}
	return Note{}, false, err
}

// AddCommentOnce: see the Store interface. The same two-command shape as
// FlagIdle, for the same reason (its comment explains why a bare NOT EXISTS is
// not enough): command 1 locks the note row, command 2 — a new snapshot — checks
// for the comment and inserts it. The insert keeps AddComment's live-parent guard
// and its same-statement updated_at bump.
//
// The duplicate probe deliberately reads retracted comments too: a comment the
// owner retracted must not come back because its producer retried.
func (s *PGStore) AddCommentOnce(ctx context.Context, c Comment) (bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	wrote, err := addCommentOnceTx(ctx, tx, c)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return wrote, nil
}

// addCommentOnceTx is AddCommentOnce's two commands inside the caller's
// READ COMMITTED transaction — shared with CreateOnceByTag, so "the same
// comment is written once" has ONE spelling.
func addCommentOnceTx(ctx context.Context, tx pgx.Tx, c Comment) (bool, error) {
	// 🔴 THE LOCK RE-CHECKS OPEN, not just not-dismissed: the comment path looks
	// the task up open (FindLiveByTag) and then gets here, so a task completed
	// or superseded in between must not gain the comment (audit round 2, C).
	// Once this row is locked, a concurrent status change waits for the commit.
	var locked int64
	if err := tx.QueryRow(ctx, `SELECT id FROM notes WHERE id=$1 AND `+openOnly("$2")+` FOR UPDATE`,
		c.NoteID, supersededPattern).Scan(&locked); err != nil {
		return false, err
	}
	var wrote int64
	if err := tx.QueryRow(ctx, `
		WITH ins AS (
			INSERT INTO note_comments (note_id, owner_id, author, body)
			SELECT id, owner_id, $2, $3 FROM notes WHERE id=$1 AND `+liveOnly+`
			   AND NOT EXISTS (SELECT 1 FROM note_comments c WHERE c.note_id = notes.id AND c.author = $2 AND c.body = $3)
			RETURNING note_id
		), bump AS (
			UPDATE notes SET updated_at=now() WHERE id=(SELECT note_id FROM ins)
		)
		SELECT count(*) FROM ins`, c.NoteID, c.Author, c.Body).Scan(&wrote); err != nil {
		return false, err
	}
	return wrote > 0, nil
}

// FlagIdle tags an idle task and posts ONE reap comment, at most once per idle
// period, WITHOUT touching updated_at. See the Store interface for the full
// rationale; the two things to keep in view while reading the SQL:
//
//  1. 🔴 `updated_at` IS ABSENT FROM THE SET LIST ON PURPOSE. Every other mutator
//     in this file writes it; this one must not, because it is the field the
//     reaper's own selection predicate reads. There is no BEFORE UPDATE trigger on
//     `notes` to put it back (the soft-delete design says so explicitly, and
//     TestFlagIdleDoesNotMoveUpdatedAt measures it), so omitting it here really
//     does leave the row's clock alone.
//
//  2. 🔴 THE `NOT EXISTS` IS THE ANTI-SPAM GUARD, and it is NOT race-safe on its
//     own — the EXPLICIT `SELECT … FOR UPDATE` below is what makes it so, and the
//     two must be read together. Deleting the lock statement leaves a guard that
//     still passes every SEQUENTIAL test and duplicates the comment under
//     concurrency; TestFlagIdleWritesOnceUnderConcurrency is what catches that.
//
//     🔴 A RETRACTED CLAIM, recorded so it is not re-derived: this used to say
//     that Postgres's EvalPlanQual re-check made the bare `NOT EXISTS` sufficient
//     ("the loser's re-check sees the winner's comment"). MEASURED FALSE against
//     Postgres 16. EvalPlanQual re-evaluates the qual against the updated `notes`
//     TUPLE, but the `NOT EXISTS` subplan on `note_comments` still reads the
//     STATEMENT's original snapshot — the documented rule is "it can see the
//     effects of concurrent updating commands on the same rows it is trying to
//     update, but it does not see effects of those commands on other rows". The
//     loser therefore looked at a pre-winner snapshot of note_comments, found
//     nothing, and inserted a second comment: 12/12 trials with 4 concurrent
//     callers double-wrote. Nor did the leader gate cover it — `leadsRetention`
//     is a per-PROCESS lease check, and reapIdleTasks has TWO in-process callers
//     (RunTaskReap at 30m and retentionPass at 24h, whose ticks coincide on every
//     pod that lives past a day), so both goroutines pass the same gate.
//
//     The lock is the fix because SNAPSHOT TIMING, not the qual, was the problem:
//     the loser blocks on `SELECT … FOR UPDATE`, and the CTE that follows is a
//     SEPARATE command, so under READ COMMITTED it takes a fresh snapshot after
//     the lock was granted and the winner's comment is visible to it. The
//     isolation level is pinned explicitly on the transaction rather than
//     inherited, because the mechanism IS the per-command snapshot: at REPEATABLE
//     READ there is only the transaction snapshot. What that costs was measured
//     rather than assumed — the loser does not duplicate, it FAILS with
//     `could not serialize access due to concurrent update` (SQLSTATE 40001), so
//     a server whose default_transaction_isolation was raised would turn every
//     concurrent sweep into a logged store error instead of a write. Either way
//     this method wants READ COMMITTED, so it asks for it.
//
//  3. 🔴 THE MARKER IS `>=`, NOT `>`, AND THAT IS LOAD-BEARING FOR THE EXISTING
//     BOARD. The PREVIOUS implementation reaped with AddTags + AddComment, and
//     AddComment inserts the comment (created_at DEFAULT now()) and writes
//     `updated_at = now()` in the SAME statement — `now()` is the transaction
//     timestamp, so those two land on the IDENTICAL value. Under `>`, equal is
//     not greater, so every legacy reap was invisible to its successor and the
//     first sweep after deploy would have re-commented the whole flagged
//     population. (What was MEASURED is the per-task behaviour: replay the old
//     reaper verbatim, then call FlagIdle, and it writes again. The ~180-task
//     size of that population is a quoted live-board figure, not something this
//     change re-measured.) Pinned by
//     TestFlagIdleRecognisesAReapWrittenByThePreviousImplementation.
//
//     A consequence worth stating, because it is what makes ReapCommentAuthor's
//     hazard REAL rather than theoretical: `>=` means a ReapCommentAuthor comment
//     written through AddComment DOES satisfy the marker (its created_at ties the
//     updated_at it just wrote), so a second writer under that author really can
//     suppress idle flagging. Under `>` it could not, and that guard would have
//     been describing an unreachable hazard.
//
// The INSERT feeds off `tagged`, so tag and comment land together or not at all,
// and both live in ONE statement inside the transaction — no partial state, no
// discarded error. `liveOnly` on the UPDATE is the ONLY dismissal check — the
// lock SELECT deliberately does not repeat it, so the rule stays in one place AND
// the earlier command cannot mask a regression in the later one. Measured, not
// assumed: with the lock in place, deleting `liveOnly` from this UPDATE still
// fails TestFlagIdleRefusesDismissedAndUnknownTasks with its own message.
//
// 🔴 ORDERING CONSUMERS, READ THIS. This is the one comment writer in the package
// that is deliberately INVISIBLE to notes.updated_at, so a reap is deliberately
// not "activity". Any read that derives a task's activity time or board position
// from note_comments.created_at — a GREATEST(...) over the thread, a max(),
// an ORDER BY on a comment lateral — MUST exclude comments authored by
// ReapCommentAuthor, or reaping a task will float it to the top of the board and
// reintroduce, by a different route, the inverted ordering this method exists to
// remove. That constraint is a property of THIS writer; it holds for whatever
// activity-ordering change lands, not for one particular pull request.
//
// ✅ There is exactly ONE such consumer in the tree today and it IS filtered:
// ListPage's `cm` activity lateral carries `AND c.author <> ReapCommentAuthor`
// (added when this branch merged the flat activity-ordered board). So this
// paragraph is a rule for the NEXT such reader, not an open TODO — and the rule
// is kept honest behaviourally by TestReapedTaskDoesNotFloatToTheTopOfTheBoard,
// which reaps a real task and requires it not to move above a more recent one.
func (s *PGStore) FlagIdle(ctx context.Context, id int64, tag, body string) (bool, error) {
	add := NormalizeTags([]string{tag})
	if len(add) == 0 {
		return false, nil
	}
	// READ COMMITTED is pinned, not inherited: the whole mechanism is that the
	// CTE below is a new COMMAND and therefore gets a new SNAPSHOT, which only
	// this isolation level provides. Measured at REPEATABLE READ: the loser gets
	// SQLSTATE 40001 rather than a duplicate.
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	// Command 1 — take the note's row lock. An unknown id locks nothing and there
	// is nothing to do; a DISMISSED task is locked here on purpose and refused by
	// `liveOnly` in the UPDATE, so dismissal is decided in exactly one place.
	var locked int64
	if err := tx.QueryRow(ctx, `SELECT id FROM notes WHERE id=$1 FOR UPDATE`, id).Scan(&locked); err != nil {
		// errors.Is, not ==: pgx returns the sentinel unwrapped today, but this
		// branch is the one that turns "no such task" into the documented
		// (false, nil), and a wrapped error taking the other path would surface a
		// spurious store error out of a best-effort sweep.
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}

	// Command 2 — a NEW command, so a NEW snapshot, taken after the lock above was
	// granted. This is the only reason the marker below can see a concurrent
	// winner's comment.
	var wrote int64
	err = tx.QueryRow(ctx, `
		WITH tagged AS (
			UPDATE notes
			   SET tags = COALESCE((SELECT array_agg(DISTINCT t ORDER BY t) FROM unnest(tags || $2::text[]) AS t), '{}')
			 WHERE id = $1 AND `+liveOnly+`
			   AND NOT EXISTS (
			         SELECT 1 FROM note_comments c
			          WHERE c.note_id = notes.id
			            AND c.author = $3
			            AND c.created_at >= notes.updated_at)
			RETURNING id, owner_id
		), ins AS (
			INSERT INTO note_comments (note_id, owner_id, author, body)
			SELECT id, owner_id, $3, $4 FROM tagged
			RETURNING id
		)
		SELECT count(*) FROM ins`, id, add, ReapCommentAuthor, body).Scan(&wrote)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return wrote > 0, nil
}

// ListComments returns a task's comments oldest-first through the commentCols
// projection: a retracted comment comes back as a TOMBSTONE —
// present, positioned, timestamped, with an empty Body and Retracted true.
func (s *PGStore) ListComments(ctx context.Context, noteID int64) ([]Comment, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+commentCols+`
		FROM note_comments WHERE note_id=$1 ORDER BY created_at, id`, noteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Comment, 0)
	for rows.Next() {
		var c Comment
		if err := rows.Scan(scanCommentInto(&c)...); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Delete PERMANENTLY removes a note and, via ON DELETE CASCADE, its attachments
// AND its entire comment thread. 🔴 No handler calls it — see the Store interface
// contract. SoftDelete is the dismiss path.
func (s *PGStore) Delete(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM notes WHERE id=$1`, id)
	return err
}

// SoftDelete marks a task dismissed. `AND deleted_at IS NULL` is what makes a
// second dismiss of the same task answer pgx.ErrNoRows rather than silently
// re-stamping deleted_at — so the dismiss timestamp records the FIRST dismissal
// (the one an operator would reconstruct a timeline from), and a repeat
// DELETE /api/tasks/{id} stays a 404 exactly as it was under the hard delete.
// Unknown id → pgx.ErrNoRows.
func (s *PGStore) SoftDelete(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `UPDATE notes SET deleted_at=now() WHERE id=$1 AND `+liveOnly, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// SoftDeleteComment retracts ONE comment from a task's thread.
// It mirrors SoftDelete exactly: `AND deleted_at IS NULL` is what makes a second
// retraction of the same comment answer pgx.ErrNoRows rather than silently
// re-stamping deleted_at, so the timestamp records the FIRST retraction and a
// repeat DELETE stays a 404 — the same contract a repeat DELETE /api/tasks/{id}
// has carried since 0019.
//
// It is scoped by BOTH ids. noteID is not decoration: the route is
// /api/tasks/{id}/comments/{cid}, and without `note_id=$1` a caller could retract
// any comment on the board through any task's path, so a mistyped task id would
// silently hit someone else's thread instead of 404ing.
//
// The parent-live EXISTS mirrors AddComment's guard: a retraction against a
// DISMISSED task is pgx.ErrNoRows, not a silent success on a thread nothing can
// see. Both handlers probe the parent with Get first anyway; this is the atomic
// backstop against a task dismissed between the probe and the update.
//
// Unknown comment id, wrong task id, already-retracted comment, dismissed parent
// → pgx.ErrNoRows. There is deliberately NO restore route (as with tasks):
// recovery is a one-column UPDATE an operator runs against the DB.
//
// What the reads then do with the stamp is commentCols' business, not this
// function's: the comment does not vanish, it comes back BODY-LESS with Retracted
// set, and the renderer draws a tombstone. This is the ONLY writer of the column.
func (s *PGStore) SoftDeleteComment(ctx context.Context, noteID, commentID int64) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE note_comments SET deleted_at=now()
		WHERE id=$2 AND note_id=$1 AND note_comments.`+liveOnly+`
		  AND EXISTS (SELECT 1 FROM notes WHERE id=$1 AND notes.`+liveOnly+`)`, noteID, commentID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// Restore clears a task's dismissal and returns the refreshed note — comments and
// attachments included, because nothing was ever deleted. `AND deleted_at IS NOT
// NULL` means restoring a LIVE task is pgx.ErrNoRows rather than a silent no-op,
// so a caller can never mistake "it was never dismissed" for "I un-dismissed it".
// updated_at is deliberately NOT bumped: a restore is a correction, and preserving
// the original updated_at keeps the task's position in the newest-first board (and
// its exposure to the idle-task reaper) exactly what it was before the dismissal.
func (s *PGStore) Restore(ctx context.Context, id int64) (Note, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE notes SET deleted_at=NULL WHERE id=$1 AND deleted_at IS NOT NULL`, id)
	if err != nil {
		return Note{}, err
	}
	if tag.RowsAffected() == 0 {
		return Note{}, pgx.ErrNoRows
	}
	return s.Get(ctx, id)
}

// ---------------------------------------------------------------------------
// 🔴 `Directories` WAS REMOVED AT THE CARVE, AND THIS IS THE RECORD OF IT.
//
// It answered the task directory picker with the distinct working directories
// seen across PERMISSION REQUESTS — reading the routing service's decision
// archive. That table is not in this schema and must not be: permission routing
// stayed behind, and a store method here that SELECTed from another service's
// table would be the exact dependency direction this extraction exists to
// remove. Re-pointing it at a table muster does own would silently answer a
// different question (the directories of TASKS, not of decisions), which is
// worse than answering none.
//
// WHERE IT GOES INSTEAD: the API layer composes it, the same way it composes
// session liveness — muster's handler calls the routing service's directories
// endpoint and clamps the result. The clamps themselves stayed in this package
// deliberately (DefaultDirectoryLimit / MaxDirectoryResults in notes.go): they
// are payload bounds with measured reasoning attached, and they are the half
// that does not depend on where the rows come from.
//
// ⚠ OWED, AND NAMED SO IT IS NOT AN OBJECT NOBODY CAN CLOSE: nothing in muster
// serves directories today, so the picker has no source until that handler and
// its upstream route exist. CLOSING CONDITION: the pull request that adds
// muster's directories handler either wires it to that route, or records the
// decision to drop the picker. WHO CHECKS IT: the reviewer of the API carve,
// against this comment.
// ---------------------------------------------------------------------------

// AddAttachment stores an attachment's bytes and returns its metadata.
func (s *PGStore) AddAttachment(ctx context.Context, noteID int64, a Attachment) (Attachment, error) {
	a.NoteID = noteID
	err := s.pool.QueryRow(ctx, `
		INSERT INTO note_attachments (note_id, owner_id, filename, content_type, size_bytes, data)
		VALUES ($1, `+ownerOfNote+`, $2, $3, $4, $5)
		RETURNING id, note_id, filename, content_type, size_bytes, created_at`,
		noteID, a.Filename, a.ContentType, a.SizeBytes, a.Data).
		Scan(&a.ID, &a.NoteID, &a.Filename, &a.ContentType, &a.SizeBytes, &a.CreatedAt)
	a.Data = nil
	return a, err
}

// GetAttachment returns a single attachment including its blob data.
func (s *PGStore) GetAttachment(ctx context.Context, id int64) (Attachment, error) {
	var a Attachment
	err := s.pool.QueryRow(ctx, `
		SELECT id, note_id, filename, content_type, size_bytes, data, created_at
		FROM note_attachments WHERE id=$1`, id).
		Scan(&a.ID, &a.NoteID, &a.Filename, &a.ContentType, &a.SizeBytes, &a.Data, &a.CreatedAt)
	return a, err
}

// ListAttachments returns a note's attachment metadata (no blob data).
func (s *PGStore) ListAttachments(ctx context.Context, noteID int64) ([]Attachment, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, note_id, filename, content_type, size_bytes, created_at
		FROM note_attachments WHERE note_id=$1 ORDER BY id`, noteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Attachment, 0)
	for rows.Next() {
		var a Attachment
		if err := rows.Scan(&a.ID, &a.NoteID, &a.Filename, &a.ContentType, &a.SizeBytes, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// compile-time assertion.
var _ Store = (*PGStore)(nil)
