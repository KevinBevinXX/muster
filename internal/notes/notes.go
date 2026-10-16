// Package notes is the persistence domain for TASKS — the table is called
// `notes` and every id in it is a task number people cite, so the name is kept
// rather than migrated. It owns the task rows, their comments, their
// attachments, their tags, their status transitions and the thread of sessions
// that touched them.
//
// 🔴 IT IMPORTS EXACTLY ONE PACKAGE FROM THIS MODULE, AND THAT IS A LEDGER, NOT
// AN ACCIDENT: internal/taskstatus, for the lifecycle vocabulary — which itself
// imports nothing. Everything else a task is
// ASSOCIATED with — the session that produced it, the permission decisions that
// name its directory, the agent that worked it — lives in another package or
// another service, and is composed at the API layer rather than joined here.
// extraction_seam_test.go fails if that set changes, in either direction, and
// it fails on SOURCE rather than on the type checker, because the dependencies
// that matter here are SQL joins and they are invisible to the compiler.
package notes

import (
	"context"
	"time"

	"github.com/ZacxDev/muster/internal/taskstatus"
)

// Task status values. The user-facing "Tasks" are the `notes` table; these are
// its lifecycle states. StatusComplete is privileged: only the operator/human
// may set it (agents are restricted to the others — see StatusAllowedForAgent).
//
// 🔴 These are RE-EXPORTS of internal/taskstatus, which owns the one definition.
// They are kept because 35 files already spell them `notes.Status*`; do NOT
// re-declare a literal here. The vocabulary moved out of this package because
// notes carries the pgx store, so consumers that never open a database (a CLI,
// above all) could not import it and kept a divergent copy instead — see the
// taskstatus package doc for the one-directional drift that caused.
const (
	StatusOpen           = taskstatus.Open
	StatusInProgress     = taskstatus.InProgress
	StatusReadyForReview = taskstatus.ReadyForReview
	StatusComplete       = taskstatus.Complete
)

// Directory-picker bounds. They live in the domain package, not in internal/api
// or internal/ui, because the STORE is what has to honour them — a bound a
// caller merely asks for is a bound the next caller can forget.
//
// 🔴 DefaultDirectoryLimit IS THE SEED SIZE, AND IT IS SMALL ON PURPOSE. Every
// seeded option is rendered into the modal's markup on open, so this number is
// a payload: 12 options of an average 74.5-character path measure 6,789 bytes of
// the create-task modal (measured — ui.TestDirectoryPickerPayloadStaysSmall logs
// it). The alternative the picker used to ship — every distinct directory as a
// <datalist> <option> — is ~154 KB at the live archive: 114,122 bytes of path
// text plus 26 bytes of markup for each of 1,532 options. Raising this raises
// the 6,789 linearly.
//
// MaxDirectoryResults caps the SEARCH answer instead: it is what stops a
// one-character query (which matches nearly every path) from turning into a
// 1,532-row response. 25 fills the dropdown's max-h-56 scroll box several times
// over, so a larger page would be scrolled past rather than read.
const (
	DefaultDirectoryLimit = 12
	MaxDirectoryResults   = 25
)

// ValidStatus reports whether s is a known task status.
func ValidStatus(s string) bool { return taskstatus.Valid(s) }

// StatusAllowedForAgent reports whether an agent (as opposed to the operator or
// human) may set status s. Agents can move a task through the working states but
// not declare it complete.
func StatusAllowedForAgent(s string) bool { return taskstatus.AllowedForAgent(s) }

// Note is a freeform note filed against a working directory.
type Note struct {
	ID        int64  `json:"id"`
	Directory string `json:"directory"`
	// Title is the task's display title . It exists because
	// POST /api/tasks SILENTLY DROPPED a `title` key (Go's decoder ignores unknown
	// fields), so every producer had to smuggle the title through Directory — the
	// footgun that motivated the column. NOT NULL DEFAULT '' in the DB, so every legacy row
	// and every titleless producer payload keeps working; the display label prefers
	// Title and falls back to Directory when empty.
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	// ActivityAt is the board's ORDERING KEY for this task — the most recent of
	// updated_at, its newest (non-reaper) comment, its linked agents' updated_at
	// and its linked sessions' last_seen_at (PGStore's taskActivityExpr) — as
	// ListPage computed it. It is set ONLY by ListPage; every other read leaves it
	// zero, and [Note.LastActivity] falls back to UpdatedAt.
	//
	// 🔴 `json:"-"`: THE MACHINE TASK API'S SHAPE IS UNCHANGED. It exists so the
	// board can PRINT the time it sorts by (a card that showed its creation age
	// over an activity-ordered list read as unsorted); /api/tasks producers were
	// promised nothing about it.
	//
	// 🔴 IT IS NOT THE CARD REVISION. data-card-rev reads UpdatedAt and must keep
	// doing so (refuseStaleCardSwap); this field must never be substituted there.
	ActivityAt time.Time `json:"-"`
	// OwnerID is the user the task belongs to (notes.owner_id; accounts PR 3b
	// reads it so an event or notification about the task reaches its owner
	// only). Never serialised: the JSON shape is unchanged. Zero from a store
	// that does not read the column — and zero reaches nobody.
	OwnerID int64 `json:"-"`
	// Tags  are the task's routing key + labels: a normalized,
	// de-duplicated, SORTED set. Routing namespaces (initiative:/gate:/auto:) are
	// behaviour-bearing and validated; everything else is a descriptive label. Always '{}' (never NULL) in the DB; omitempty keeps the JSON shape
	// unchanged for a tagless task, so existing producers see no contract change.
	Tags []string `json:"tags,omitempty"`
	// Dispatch config : a Task carries the agent-dispatch settings
	// so dispatching from its card is a pre-filled confirm rather than a blank
	// form. All optional (empty = "unset", resolved at dispatch time).
	Model      string `json:"model"`      // OpenRouter slug; "" = cluster default
	Repo       string `json:"repo"`       // owner/name; "" = none
	RepoBranch string `json:"repoBranch"` // explicit branch; "" = repo default
	// Source provenance : WHERE the task came from, derived from the
	// X-Muster-Source / X-Muster-Session-Id headers on POST /api/tasks. Both are
	// NULLABLE — a nil pointer is a pre-0017 / header-less task (the omitempty tag
	// then omits the field, and the card renders no provenance chip). SourceType is
	// one of the taskSource allowlist producers (never the default "api", which is
	// stored as NULL); SourceSessionID is opaque here (a label, not a document id).
	SourceType      *string      `json:"sourceType,omitempty"`
	SourceSessionID *string      `json:"sourceSessionId,omitempty"`
	Attachments     []Attachment `json:"attachments,omitempty"`
	Comments        []Comment    `json:"comments,omitempty"`
	// Sessions is the task's THREAD : every coding-agent session
	// that created, worked or read this task, oldest-joined first. It is embedded
	// on the full reads exactly as Comments and Attachments are — there is
	// deliberately no GET /api/tasks/{id}/sessions sub-route.
	//
	// omitempty keeps the JSON shape byte-identical for a task with no thread, so
	// every existing producer sees no contract change.
	Sessions []SessionLink `json:"sessions,omitempty"`
}

// NoteUpdate is a PARTIAL edit to a task's editable content + dispatch config.
// Every field is a pointer so "unset" (nil = leave the column unchanged) is
// distinguishable from "set to empty" (a non-nil pointer to ""). Only the
// user-editable fields are represented: a task's Status, provenance
// (SourceType/SourceSessionID), CreatedAt, and ID are NEVER editable through this
// path (see the Store contract).
type NoteUpdate struct {
	Directory  *string
	Title      *string
	Body       *string
	Model      *string
	Repo       *string
	RepoBranch *string
	// Tags has REPLACE semantics (consistent with the other pointer fields): a
	// non-nil pointer overwrites the whole set. Because tasks have TWO author
	// classes (machine producers and the human), replace-only would create a
	// lost-update class — so AddTags/RemoveTags exist alongside this for merges.
	// A provided-but-nil slice normalizes to '{}' (the column is NOT NULL).
	Tags *[]string
}

// ReapCommentAuthor is the Author the idle-task retention sweep stamps on the
// comment it posts, and — because FlagIdle's "have I already reaped this?" test
// is `EXISTS (a comment by this author newer than updated_at)` — it is a
// STRUCTURAL MARKER, not decoration. Two consequences that must not drift:
//
//   - 🔴 NOTHING ELSE MAY AUTHOR A COMMENT UNDER THIS NAME. A second writer using
//     it would silently suppress reaps on every task it commented on, and the
//     failure is invisible (a task that is never flagged looks exactly like a task
//     that is not idle). Pinned by api.TestReapCommentAuthorIsUnclaimable, which
//     covers BOTH routes to a comment author: the hardcoded one (via the shared
//     privilegedAuthorSites scanner, which also backs
//     TestPrivilegedAuthorSitesAreLedgered) and the DERIVED one (taskSourceAllowlist
//     must not contain this value, or a machine could claim it with one header).
//   - Its VALUE is frozen at "system" on purpose. Changing it would orphan every
//     reap the previous implementation recorded and hand the whole existing board
//     one fresh round of comments.
//
// 🔴 WHAT THIS DOES *NOT* COVER: the `stale` TAG. Nothing removes that tag
// AUTOMATICALLY — no sweep, no status change, no comment handler clears it, so
// once a task has been flagged the chip stays on the card even after the task is
// worked on again. Read it as "this task has been idle at least once", NOT as
// "this task is idle now"; the per-event truth is in the thread, where each reap
// posts its own dated comment.
//
// ⚠ "Automatically" is the whole qualifier, and an earlier version of this
// comment dropped it and was wrong three ways. An OPERATOR has three routes to
// remove it, all live: the machine `PATCH /api/tasks/{id}` with `removeTags`
// (api/notes.go), the routed `DELETE /tasks/{id}/tags/{tag}` (server.go →
// api/tags.go, both RemoveTags callers — there is no "merge path" caller), and
// the Edit modal's `POST /tasks/{id}/edit`, which submits the whole tag set
// through UpdateNote's REPLACE semantics and so drops any chip the operator
// deleted. api.staleTaskTag says the same thing about the Edit modal; these two
// comments must agree.
//
// None of that changes the design, because re-flagging keys off this comment
// marker rather than the tag — see FlagIdle — so an operator clearing the chip
// neither re-arms nor disarms the reaper. What it does change is the ARGUMENT: a
// tag-presence predicate is rejected because the tag is only ever cleared by a
// deliberate operator action, so a task nobody edits could never be re-flagged
// however long it later sat idle, and that failure is invisible (a task that is
// never flagged looks exactly like a task that is not idle). Not because the tag
// is unremovable.
//
// Leaving it un-cleared by the reaper is a DELIBERATE choice. The rejected
// alternative was an auto-un-stale sweep, which would have been a mass write over
// live rows driven by `updated_at`, the very field this change exists because it
// is corrupted on 180 of them; clearing the tag on those is the operator's call,
// not the reaper's.
const ReapCommentAuthor = "system"

// Comment is a single entry in a task's comment thread. Author is the agent
// name, "operator", or "user".
//
// 🔴 A RETRACTED COMMENT IS STILL RETURNED — as a TOMBSTONE, not
// as an absence. Retracted is true and Body is the EMPTY STRING, because the
// redaction happens in the SQL projection (pgstore.commentCols): the retracted
// text is never selected out of Postgres at all, so no read path — HTML, JSON, or
// a future one — can leak it by forgetting a filter.
//
// Why a tombstone rather than dropping the row from the read: a thread that
// silently SHORTENS is the worse failure mode on a board agents treat as
// authoritative. Position and timestamp survive, so a reader can see that
// something was retracted; only the content is gone.
//
// The consequence that makes this one rule instead of two: len(Note.Comments)
// counts tombstones, so apiTaskSummary.CommentCount and the card's "N comments"
// badge — both derived from it — agree with what is rendered, on every surface,
// without either of them knowing about retraction.
type Comment struct {
	ID     int64  `json:"id"`
	NoteID int64  `json:"noteId"`
	Author string `json:"author"`
	// Body is empty whenever Retracted is true. Never trust it to be populated
	// without checking Retracted first.
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
	// Retracted marks a comment whose content has been withdrawn. Machine
	// consumers should render it as a tombstone (or skip it) — but MUST NOT treat
	// an empty Body on a live comment as retraction; the flag is the predicate.
	Retracted bool `json:"retracted,omitempty"`
}

// Attachment is a file attached to a note. Data is only populated when an
// attachment is fetched for download; list queries leave it nil to avoid moving
// blobs around.
type Attachment struct {
	ID          int64     `json:"id"`
	NoteID      int64     `json:"noteId"`
	Filename    string    `json:"filename"`
	ContentType string    `json:"contentType"`
	SizeBytes   int64     `json:"sizeBytes"`
	Data        []byte    `json:"-"`
	CreatedAt   time.Time `json:"createdAt"`
}

// groupNoteChildren assigns each attachment and comment to its parent note by
// note_id, preserving the order in which they appear in the input slices. It is
// the pure assembly step behind the batched List query: the caller fetches all
// notes, all attachments, and all comments in three queries, then groups them
// here. Children whose note_id is not among notes are dropped.
func groupNoteChildren(notes []Note, atts []Attachment, comments []Comment) {
	idx := make(map[int64]int, len(notes))
	for i := range notes {
		idx[notes[i].ID] = i
		// Reset so re-grouping is idempotent and nil-safe.
		notes[i].Attachments = nil
		notes[i].Comments = nil
	}
	for _, a := range atts {
		if i, ok := idx[a.NoteID]; ok {
			notes[i].Attachments = append(notes[i].Attachments, a)
		}
	}
	for _, c := range comments {
		if i, ok := idx[c.NoteID]; ok {
			notes[i].Comments = append(notes[i].Comments, c)
		}
	}
}

// ListFilter narrows and pages the task list. The zero value means "every live
// task, unlimited" — i.e. exactly what List did before paging existed.
//
// 🔴 EVERY FIELD IS AND-ed WITH THE OTHERS. The board's three controls (project
// chips, tag chips, status chips) must COMPOSE: selecting a project and then a
// status has to narrow, not replace. Expressing that as one filter struct with
// one query behind it is what makes composition the default instead of something
// each combination has to be remembered for.
type ListFilter struct {
	// Tags is AND containment (`tags @> $n`), matching ListByTags. Empty/nil is
	// no tag predicate at all. Values are normalized by the store.
	Tags []string
	// Statuses restricts to `status = ANY(...)`. Empty/nil is no status
	// predicate — which is the "All" chip, and also what an unknown ?status=
	// value degrades to (see statusPredicate in internal/api/tags.go: a bad
	// status must show everything, never nothing).
	Statuses []string
	// Limit caps the returned rows. 0 (or negative) means no LIMIT clause.
	// Page.Total still counts the whole matching set, so a caller can tell a
	// truncated page from a complete one.
	Limit int
}

// Page is one page of the task list plus the size of the set it was cut from.
//
// Total is the count BEFORE Limit, computed in the same query (a window
// COUNT(*) OVER ()), so `len(Notes) < Total` is the exact, race-free condition
// for "there is more to show" — rather than two queries whose answers can
// disagree across a concurrent write.
type Page struct {
	Notes []Note
	Total int
}

// Store is the notes persistence behaviour.
type Store interface {
	Create(ctx context.Context, n Note) (Note, error)
	Get(ctx context.Context, id int64) (Note, error)
	List(ctx context.Context) ([]Note, error)
	// ListSummaries returns every live note's OWN columns — no attachments, no
	// comments, no session links — in ONE query. It backs the two readers that do
	// not need List's batched child fan-out: the dispatch-modal note picker (an id
	// + a label) and the idle-task reaper (id + UpdatedAt, every 30 minutes).
	//
	// 🔴 UpdatedAt IS POPULATED and must stay so: it is the reaper's selection
	// predicate, and the zero time is BEFORE every cutoff, so a projection that
	// dropped it would make the reaper treat the whole board as idle.
	ListSummaries(ctx context.Context) ([]Note, error)
	// Delete PERMANENTLY destroys a note and — via the note_comments /
	// note_attachments `ON DELETE CASCADE` FKs — its entire comment thread and every
	// attachment with it.
	//
	// 🔴 NO HANDLER CALLS THIS, deliberately. Dismissing a task (the UI dismiss,
	// DELETE /api/tasks/{id}, and the idle-task reaper all share dismissTask) goes
	// through SoftDelete instead, precisely because destroying the thread as a side
	// effect of tidying a board was the defect soft delete closes. This remains
	// as the explicit hard-purge primitive for an operator who means it; reaching for
	// it from a handler re-opens the unrecoverable-loss class.
	Delete(ctx context.Context, id int64) error
	// SoftDelete marks a task dismissed (`deleted_at = now()`) instead of deleting
	// its row. Every other Store method treats a soft-deleted task as NONEXISTENT —
	// it is absent from List/ListByTags/ListSummaries/TagVocabulary and every read
	// and write by id answers pgx.ErrNoRows — so a dismissed task disappears from
	// the board, the machine API, the dispatch picker and the reaper's own selection
	// set exactly as a deleted row did. What differs is that the row (and therefore
	// its comments and attachments, whose cascades are left in place on purpose)
	// SURVIVES, so Restore brings the whole thread back rather than an empty shell.
	//
	// An unknown id — and a SECOND SoftDelete of an already-dismissed task —
	// answers pgx.ErrNoRows. That is what keeps a repeat DELETE /api/tasks/{id} a
	// 404 rather than a 200, matching byte-for-byte what a repeat delete answered
	// under the old hard delete; producers key their retries on that code.
	SoftDelete(ctx context.Context, id int64) error
	// Restore clears a task's dismissal, returning the refreshed note (comments and
	// attachments included — they were never deleted). It is the recovery path for
	// SoftDelete and is reachable ONLY from the store: no HTTP route, machine or
	// session, exposes an undelete today. An unknown id, or an id that is not
	// currently soft-deleted, answers pgx.ErrNoRows.
	Restore(ctx context.Context, id int64) (Note, error)
	// SetStatus updates a task's lifecycle status and returns the refreshed note.
	SetStatus(ctx context.Context, id int64, status string) (Note, error)
	// UpdateNote applies a partial edit to a task's editable content + dispatch
	// config (ONLY the fields set in patch), bumps updated_at, and returns the
	// refreshed note. It NEVER touches status, provenance
	// (source_type/source_session_id), or created_at. An unknown id → pgx.ErrNoRows.
	// The caller is responsible for authorizing the edit (e.g. refusing an
	// in-progress task).
	UpdateNote(ctx context.Context, id int64, patch NoteUpdate) (Note, error)
	// AddTags set-UNIONs tags into the task's tag set and returns the refreshed
	// note. Idempotent. It exists (rather than a read-modify-write on UpdateNote)
	// specifically to avoid the LOST-UPDATE class that replace-only semantics
	// create with two author classes: the Postgres implementation is a SINGLE
	// statement, so two concurrent writers each adding a tag both win. Callers
	// pass already-normalized tags (the store never accepts un-normalized input).
	// Unknown id → pgx.ErrNoRows.
	AddTags(ctx context.Context, id int64, tags []string) (Note, error)
	// RemoveTags set-DIFFERENCEs tags out of the task's tag set and returns the
	// refreshed note. Idempotent, single-statement (same rationale as AddTags).
	// Unknown id → pgx.ErrNoRows.
	RemoveTags(ctx context.Context, id int64, tags []string) (Note, error)
	// ListByTags returns the notes carrying EVERY tag in tags (AND semantics),
	// most-recent-activity first, with the same attachment/comment fan-out as
	// List. An empty/nil tags slice is equivalent to List. A tag nothing carries
	// yields an empty slice — a filter matching nothing is not an error.
	ListByTags(ctx context.Context, tags []string) ([]Note, error)
	// ListPage is the paged, filtered form of List — see ListFilter and Page. It
	// is the ONE query List and ListByTags delegate to, so the ordering, the
	// soft-delete predicate and the batched child fan-out have a single spelling.
	ListPage(ctx context.Context, f ListFilter) (Page, error)
	// CountByStatus is the number of live (not dismissed) tasks in one status.
	// It exists for the app badge, which re-reads it on every task.changed event,
	// so it is ONE indexed count rather than ListPage with its laterals and child
	// fan-out.
	CountByStatus(ctx context.Context, status string) (int, error)
	// TagVocabulary returns every tag in use with its task count, ordered by count
	// descending (then tag ascending, so ties are deterministic). It backs the
	// filter chip row and the editor's datalist.
	TagVocabulary(ctx context.Context) ([]TagCount, error)
	// LinkSession records that a coding-agent session touched a task, upserting the
	// (task, session) edge that makes "which sessions worked this task" a query.
	// It returns the resulting link and whether anything OBSERVABLE changed — a
	// new row or a role UPGRADE, but NOT a bare last_seen_at touch, so a caller can
	// broadcast on the former without spamming on the latter.
	//
	// Contract, all of it load-bearing:
	//   - Idempotent per (task, session): a repeat touch is ONE row. last_seen_at
	//     moves; first_seen_at never does.
	//   - The role only ever moves UP (read → worked → created); `created` is
	//     terminal. See UpgradeRole.
	//   - A DISMISSED (soft-deleted) task cannot gain a link → pgx.ErrNoRows,
	//     exactly as AddComment refuses to write into a thread nothing can see. An
	//     unknown id answers the same.
	//   - A link that would introduce a NEW session past MaxTaskSessions →
	//     ErrTooManySessions. An already-linked session is never refused.
	//   - detailSeen is the CALLER'S fresh observation of whether a transcript
	//     record exists for l.SessionID right now. It is OR-ed into the stored,
	//     MONOTONIC detail_seen column, so false can only ever fail to teach the
	//     row something — it can never unlearn a true. This package cannot probe
	//     for it: the session record belongs to another service and is not in this
	//     schema. It does NOT populate the returned link's DetailAvailable.
	LinkSession(ctx context.Context, noteID int64, l SessionLink, detailSeen bool) (SessionLink, bool, error)
	// SessionsForTask returns one task's thread, oldest-joined first. Nothing
	// touched it ⇒ an empty slice, not an error. The full reads (Get/List) already
	// embed this on Note.Sessions; this is for callers holding only an id.
	SessionsForTask(ctx context.Context, noteID int64) ([]SessionLink, error)
	// TasksForSession is the REVERSE lookup — every LIVE task a session touched,
	// most-recently-touched first. It backs GET /api/sessions/{id}/tasks.
	TasksForSession(ctx context.Context, sessionID string) ([]TaskLink, error)
	// AddComment appends a comment to a task's thread.
	AddComment(ctx context.Context, c Comment) (Comment, error)
	// CreateOnceByTag creates n unless an OPEN task the caller can see already
	// carries key (an already-normalized tag), and returns that task either way:
	// created reports which. "Open" is FindLiveByTag's: not dismissed, not
	// complete, not a merge loser (`superseded-by:`); a task that is none of
	// those makes a later call file a new one. Two concurrent calls for one key
	// create ONE task (serialised on a transaction-scoped advisory lock on the
	// key), so a producer that retries, or two that race, cannot file it twice.
	// key is added to n's tags. c, when non-nil, is recorded once on whichever
	// task that is (AddCommentOnce's rule) IN THE SAME TRANSACTION — so a resend
	// of the same create-with-comment finds the comment and adds nothing; its
	// NoteID is ignored; commented reports whether it was written.
	CreateOnceByTag(ctx context.Context, key string, n Note, c *Comment) (note Note, created, commented bool, err error)
	// FindLiveByTag returns the OPEN task CreateOnceByTag treats as the one
	// carrying key — the oldest the caller can see that is not dismissed, not
	// complete and not a merge loser (`superseded-by:`) — and whether there is
	// one. Both go through ONE lookup, so they can never disagree.
	FindLiveByTag(ctx context.Context, key string) (note Note, ok bool, err error)
	// AddCommentOnce appends c unless the task's thread already holds a comment
	// with the same author and the same body (a retracted one included, so a
	// retraction is not undone by a retry), and reports whether it wrote.
	// Concurrent calls for one comment write it once (the note row is locked
	// first). Unknown or dismissed task → pgx.ErrNoRows.
	AddCommentOnce(ctx context.Context, c Comment) (wrote bool, err error)
	// FlagIdle records ONE idle-reap on a task: it set-UNIONs tag into the task's
	// tags AND appends body as a comment authored by ReapCommentAuthor — both or
	// neither — and reports whether it actually wrote. It is the retention
	// sweep's only writer.
	//
	// 🔴 IT DOES NOT TOUCH updated_at, AND THAT IS THE WHOLE POINT. Every other
	// mutator here writes `updated_at = now()` in the same statement as the
	// change, because updated_at is the task's activity clock AND its card
	// revision. The reaper SELECTS on that clock (`updated_at < now - TTL`), so a
	// reaper that used AddTags+AddComment was writing to the field it measures:
	// each sweep reset the 7-day idle clock on everything it touched, so the same
	// population re-reaped forever in growing batches, and because the sweep
	// iterates the list in `updated_at DESC` order and bumps each row in turn, the
	// MOST idle task was bumped LAST and sorted HIGHEST — the board's order came
	// out inverted against real activity. Measured on the live board: 186 of 293
	// tasks in same-second clusters, correlation between the stamped updated_at
	// and true last activity −0.948.
	//
	// 🔴 THE NON-BUMP IS ONLY SAFE BECAUSE THE "ALREADY REAPED" TEST MOVED HERE.
	// The bump was doing double duty: after it, the task no longer satisfied
	// `updated_at < cutoff`, which is the ONLY reason the old reaper did not
	// re-comment on every pass. Remove the bump alone and the tag stays idempotent
	// (set-union) but the COMMENT does not — the sweep would append a duplicate
	// system comment forever. So this method carries its own predicate instead:
	//
	//	reap iff the task has NO ReapCommentAuthor comment created AFTER its
	//	current updated_at.
	//
	// That is the honest spelling of "has this task been reaped since its last
	// genuine activity?", and it is strictly better than testing for the tag:
	//
	//   - Exactly one comment per idle period. A second sweep finds the first
	//     comment's created_at newer than the (unmoved) updated_at and writes
	//     nothing.
	//   - RE-FLAGGING STILL WORKS. A real write bumps updated_at past the old reap
	//     comment, so after another full idle window the task is eligible again.
	//     A tag-absence predicate could not do this — nothing removes the tag —
	//     and would have made every reaped task un-flaggable forever.
	//   - It recognises reaps performed by the PREVIOUS implementation, which
	//     authored the same comment, so the existing board does not get a fresh
	//     round of comments. 🔴 That requires the comparison to be `>=`, not `>`:
	//     the old reaper's AddComment wrote the comment's created_at and the row's
	//     updated_at in ONE statement, so they are EQUAL, and a strict `>` would
	//     have re-reaped every already-flagged task on the first sweep after
	//     deploy. See PGStore.FlagIdle.
	//   - Two processes racing it cannot both insert — but 🔴 NOT because the
	//     predicate shares the write's statement. That reasoning was measured
	//     FALSE (the `NOT EXISTS` subplan reads the statement's original snapshot,
	//     so the loser does not see the winner's comment). What makes it safe is
	//     the explicit row lock PGStore.FlagIdle takes in a preceding command; the
	//     leader gate does NOT cover this, because it is a per-process lease and
	//     reapIdleTasks has two in-process callers.
	//
	// RETRACTED reap comments still COUNT. Filtering them out would let an
	// operator who retracts the comment make the task eligible again, and the next
	// sweep would post a new one — a retract/repost loop. Suppression is the safe
	// direction.
	//
	// 🔴 THIS IS THE ONE COMMENT WRITER THAT IS INVISIBLE TO updated_at, so a reap
	// is deliberately not "activity". Any read that derives a task's activity time
	// or board position from note_comments.created_at must EXCLUDE
	// ReapCommentAuthor, or reaping a task floats it to the top of the board and
	// reintroduces the inverted ordering this method exists to remove. See
	// PGStore.FlagIdle.
	//
	// ✅ SATISFIED IN-TREE, not outstanding: the only such reader is the board's
	// activity ordering in PGStore.ListPage, whose comment lateral carries
	// `AND c.author <> ReapCommentAuthor`. The paragraph above is the rule a FUTURE
	// reader must follow, not a TODO. Behavioural pin:
	// TestReapedTaskDoesNotFloatToTheTopOfTheBoard.
	//
	// The `stale` TAG is never removed AUTOMATICALLY — only by a deliberate
	// operator action; see ReapCommentAuthor for what that means and why
	// re-flagging does not depend on it. Unknown, dismissed, or already-reaped
	// id → (false, nil), never an error: "nothing to do" is not a failure. A store
	// error → (false, err).
	FlagIdle(ctx context.Context, id int64, tag, body string) (bool, error)
	// ListComments returns a task's comments oldest-first. A retracted comment is
	// INCLUDED as a tombstone (Retracted true, Body empty) — see Comment and
	// SoftDeleteComment.
	ListComments(ctx context.Context, noteID int64) ([]Comment, error)
	// SoftDeleteComment retracts ONE comment from a task's thread by stamping
	// note_comments.deleted_at  instead of deleting the row. It is
	// the comment-scoped twin of SoftDelete and carries the same contract:
	//
	//   - The comment's BODY disappears from every read — Get's thread, the batched
	//     board read behind List/ListByTags, and therefore both the HTML card and
	//     the machine JSON. The comment itself stays in the thread as a TOMBSTONE
	//     (Retracted true, Body empty) so the thread cannot silently shorten, and
	//     the row (the evidence) survives for a DB-level undo.
	//   - Scoped by BOTH the task id and the comment id, so a comment can only be
	//     retracted through the thread it actually belongs to.
	//   - An unknown comment id, a comment belonging to a DIFFERENT task, a comment
	//     already retracted, and a comment on a DISMISSED task all answer
	//     pgx.ErrNoRows — which is what keeps a repeat DELETE a 404 rather than a
	//     200, matching DELETE /api/tasks/{id}.
	//
	// There is no Restore twin: comment recovery, like task recovery, is
	// store/DB-level only and no HTTP route exposes it.
	SoftDeleteComment(ctx context.Context, noteID, commentID int64) error
	AddAttachment(ctx context.Context, noteID int64, a Attachment) (Attachment, error)
	// GetAttachment returns a single attachment including its Data, for download.
	GetAttachment(ctx context.Context, id int64) (Attachment, error)
	// ListAttachments returns a note's attachments WITHOUT Data (metadata only).
	ListAttachments(ctx context.Context, noteID int64) ([]Attachment, error)
}

// LastActivity is the time the Tasks board orders this task by: ActivityAt when
// the read computed it (ListPage), else UpdatedAt — which ActivityAt is never
// older than, and which equals it the moment after any write to the task itself.
func (n Note) LastActivity() time.Time {
	if n.ActivityAt.After(n.UpdatedAt) {
		return n.ActivityAt
	}
	return n.UpdatedAt
}
