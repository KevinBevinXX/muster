package notes

import (
	"context"
	"errors"
	"log"
	"strings"

	"github.com/ZacxDev/muster/internal/taskstatus"
)

// ---------------------------------------------------------------------------
// The ONE choke point that observes task-status TRANSITIONS — every call to
// Store.SetStatus that passes through this Go process. Task CREATION (Store.Create,
// where the status column takes its NOT NULL DEFAULT) and any write that never
// enters the process at all (psql, a restore, an out-of-band job) are outside it;
// see WithStatusLogging for why, and the PR body for what that leaves unruled-out.
//
// 🔴 WHY IT EXISTS. An agent finished a task, opened a pull request and set the
// task `ready_for_review`. Eleven seconds later `notes.updated_at` moved again
// and the task was back in `in_progress` — and the HTTP log for that second
// contained no status request of any kind, so the revert did not come through a
// route. At the time, some of the writers of a task's status were NATIVE
// FUNCTION TOOLS rather than HTTP handlers, and tool calls were not logged at
// all: those paths were unobservable by construction. An agent's completion
// signal was lost with nothing anywhere to attribute the loss to.
//
// 🔴 THE GENERAL SHAPE, WHICH IS WHAT SURVIVES THE PARTICULARS: a value with
// SEVERAL writers, only SOME of which are observable, produces a change nobody
// can attribute — and the unobservable writer is invisible precisely because it
// is unobservable, so the search goes to the observable ones and finds nothing
// wrong with them.
//
// 🔴 THE MECHANISM IS NOT PROVEN and nothing here assumes one. This is
// OBSERVATION ONLY — it does not prevent, reject or reorder any write. Building
// a guard on an unproven mechanism guards the wrong thing; the next incident
// should be attributable instead of re-narrowed.
//
// WHY A DECORATOR AND NOT A LOG LINE AT EACH CALL SITE. A copy per call site is
// a place to forget at every one of them, and the NEXT writer — the one that has
// not been written yet — is exactly the one that would forget. Wrapping the store
// means a writer
// cannot opt out: it can only fail to LABEL itself, and an unlabelled write logs
// `writer=UNLABELLED`, which is louder than the silence it replaces and is
// itself the finding. The api tier stamps the label (see api.setNoteStatus and
// its ledger test).
// ---------------------------------------------------------------------------

// writerKey is the unexported context key carrying the label. Unexported and a
// distinct struct type so no other package can collide with it or set it by
// accident — the only way to put a label on a context is WithWriter.
type writerKey struct{}

// UnlabelledWriter is what the choke point logs when a status write arrives on a
// context nobody labelled.
//
// It is a LOUD sentinel rather than an empty field on purpose: `writer=` reads
// as a formatting bug and gets ignored, while `writer=UNLABELLED` reads as the
// finding it is — a status write from a path that never announced itself, which
// is the exact class of event this whole change exists to surface.
//
// 🔴 IT IS UNREACHABLE IN PRODUCTION TODAY, and its absence from the logs is
// therefore evidence of NOTHING. Every api-tier call site passes a non-empty
// label, and no caller of notes.Store.SetStatus exists outside internal/api, so
// nothing can currently emit it.
//
// 🔴 THE MECHANISM IS THE LEDGER, NOT THE COMPILER. This comment used to say
// "the compiler requires one", and that is MEASURED FALSE: `statusWriter` is
// `type statusWriter string` (api/task_status_writer.go:34), so `""` is an
// untyped constant assignable to it and
//
//	s.setNoteStatus(ctx, "", id, "in_progress")
//
// COMPILES CLEAN. What actually catches it is
// TestEveryStatusWriterIsLabelled, which pins every caller of setNoteStatus to
// the constant it passes and reports an empty literal as
// `mutantEmptyLabel→<not-a-constant-identifier>`. The conclusion above is
// unchanged — UNLABELLED is unreachable today — but a guard credited to the
// wrong mechanism is one refactor away from being deleted as redundant. It is a TRIPWIRE for a FUTURE writer that reaches the store without
// going through the labelled seam. Do not read "we have never seen UNLABELLED"
// as "every write was attributed" — read it as "no writer has been added since".
const UnlabelledWriter = "UNLABELLED"

// unknownStatus is the placeholder for "the previous status could not be read"
// (an unknown/dismissed id, or a store error on the pre-read). It is NOT a task
// status and taskstatus.Rank ranks it -1, so it can never be mistaken for a
// downgrade.
const unknownStatus = "UNKNOWN"

// agentWriteKey marks a status write made for an AGENT (ForAgent).
type agentWriteKey struct{}

// ErrCompleteLocked: an agent's status write on a task that is complete. Only
// the operator/human reopens one.
var ErrCompleteLocked = errors.New("notes: agents may not change a completed task; only the operator/human can reopen it")

// ForAgent marks ctx's status writes as an AGENT's: PGStore.SetStatus then
// moves the row only while it is not complete — in the UPDATE itself, so no
// read-then-write race can reopen a task the operator just completed — and
// answers ErrCompleteLocked when it is.
func ForAgent(ctx context.Context) context.Context {
	return context.WithValue(ctx, agentWriteKey{}, true)
}

func forAgent(ctx context.Context) bool { v, _ := ctx.Value(agentWriteKey{}).(bool); return v }

// fromStatusKey carries OnlyFrom's required current status.
type fromStatusKey struct{}

// ErrStatusMoved: an OnlyFrom status write found the task no longer in the
// required status, so it moved nothing.
var ErrStatusMoved = errors.New("notes: the task is no longer in the status this write required")

// OnlyFrom makes ctx's status writes CONDITIONAL: PGStore.SetStatus moves the
// row only while its status is still from — in the UPDATE itself, so a status
// change landing between a caller's read and its write is never overwritten —
// and answers ErrStatusMoved when it is not. (agent_destroy's reopen uses it.)
func OnlyFrom(ctx context.Context, from string) context.Context {
	return context.WithValue(ctx, fromStatusKey{}, from)
}

func onlyFrom(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(fromStatusKey{}).(string)
	return v, ok
}

// WithWriter returns ctx carrying label as the identity of whoever is about to
// write a task status on it.
//
// An empty/whitespace label is NOT stored: storing it would produce a context
// that claims to be labelled while reading back as unlabelled, and the whole
// point of the sentinel is that "nobody said" is visible.
func WithWriter(ctx context.Context, label string) context.Context {
	if strings.TrimSpace(label) == "" {
		return ctx
	}
	return context.WithValue(ctx, writerKey{}, label)
}

// WriterFrom returns the writer label on ctx, or UnlabelledWriter when there is
// none. It never returns an empty string.
func WriterFrom(ctx context.Context) string {
	if ctx == nil {
		return UnlabelledWriter
	}
	if v, ok := ctx.Value(writerKey{}).(string); ok && strings.TrimSpace(v) != "" {
		return v
	}
	return UnlabelledWriter
}

// statusLoggingStore is a Store that logs every SetStatus and forwards
// everything else untouched.
//
// It EMBEDS the Store interface rather than listing ~30 forwarding methods. That
// is not just brevity: a method added to Store later is forwarded automatically
// instead of failing to compile here and tempting whoever hits it to drop the
// decorator. The only method it overrides is the one it exists for.
type statusLoggingStore struct {
	Store
	logger *log.Logger
}

// WithStatusLogging wraps s so that every SetStatus call is logged once, with
// the note id, the old status, the new status and WHICH writer did it.
//
// 🔴 "EVERY STATUS WRITE" MEANS EVERY SetStatus — NOT every way a row's status
// column can acquire a value. Two things are deliberately outside it:
//   - Store.Create. A new task's status is established at INSERT (the `status`
//     column's NOT NULL DEFAULT — Create does not name the column), and that is
//     not logged. It is a task coming into existence with its opening state, not a
//     transition between states, and the incident this exists for was a
//     TRANSITION. A creation is already attributable from the note row itself.
//   - Anything that does not go through this Go process at all: psql, a restore,
//     an out-of-band job. Nothing in-process can see those.
//
// So read a status write's absence from the log as "no SetStatus ran", never as
// "the column did not change".
//
// A nil store is returned unchanged (nil), so the caller's own "extensions are
// optional" nil checks keep working. A nil logger falls back to log.Default().
func WithStatusLogging(s Store, logger *log.Logger) Store {
	if s == nil {
		return nil
	}
	if logger == nil {
		logger = log.Default()
	}
	return &statusLoggingStore{Store: s, logger: logger}
}

// SetStatus logs the write and delegates. Behaviour is IDENTICAL to the wrapped
// store's: the same note and the same error come back, unmodified, on every
// path. Nothing here may change what a status write does — only what it leaves
// behind.
//
// 🔴 COST, measured against the real store rather than assumed. The pre-read is
// one call to Store.Get, but PGStore.Get is FOUR round trips — the note row,
// ListAttachments, ListComments and SessionsForTask (see pgstore.go). A status
// write therefore goes from roughly 5 database round trips to roughly 9, about
// +80%, not "one extra read". That is still affordable, and deliberately not
// optimised here: status writes are rare — single digits per hour across the
// whole board — so the absolute cost is a few extra queries an hour. A failed
// pre-read degrades to old=UNKNOWN rather than failing the write. If the write
// rate ever climbs, the fix is a narrow status-only read, not dropping the line.
//
// 🔴 KNOWN LIMITATION — THE PRE-READ IS NOT TRANSACTIONAL, so the old status can
// be stale. Get and SetStatus are two separate statements with no transaction
// around them, and a second writer can land between them. That does not affect
// ATTRIBUTION — `writer=` is read off this call's own context and is always
// right — but it can misclassify the DOWNGRADE warning in both directions:
//
//	SUPPRESSED: A reads old=ready_for_review; B writes in_progress; A writes
//	ready_for_review. B's own line reads old=ready_for_review and correctly warns,
//	but interleave it the other way — B reads old=in_progress before A's write
//	lands — and B's genuine ready_for_review→in_progress revert logs as a
//	forward move with no WARN.
//	FABRICATED: A writes in_progress, B (already holding a stale
//	old=ready_for_review) writes in_progress too, and B warns about a downgrade
//	that its own write did not perform.
//
// That is precisely the concurrent-revert shape the WARN exists to catch, so the
// WARN is a HINT, not a verdict — always read the sequence of lines, not one.
// Making the pre-read transactional would change write behaviour (lock ordering,
// a new failure mode on the read), and this change is OBSERVATION ONLY; the
// limitation is documented rather than closed.
func (s *statusLoggingStore) SetStatus(ctx context.Context, id int64, status string) (Note, error) {
	// A pre-read that FAILS, and one that succeeds with a BLANK status, are the
	// same thing for this line's purposes: the previous status is not known. Both
	// must land on the sentinel — printing `old=` would be an empty field in the
	// one log line whose entire job is attribution, and an empty field is what
	// gets scrolled past. (The column is NOT NULL DEFAULT in Postgres, so the
	// blank case is defensive rather than observed.)
	old := unknownStatus
	if prev, err := s.Store.Get(ctx, id); err == nil && strings.TrimSpace(prev.Status) != "" {
		old = prev.Status
	}

	n, err := s.Store.SetStatus(ctx, id, status)

	// ONE log statement, deliberately. The classification below picks the prefix
	// and the outcome; there is no second Printf to drift from this one.
	kind := "task status write"
	// A DOWNGRADE is only claimed for a write that actually LANDED. An attempt
	// that errored changed nothing, and a WARNING about a state the row is not in
	// would poison the grep this exists to serve.
	if err == nil && taskstatus.IsDowngrade(old, status) {
		kind = "WARNING task status DOWNGRADE"
	}
	result := "ok"
	if err != nil {
		result = "FAILED (" + err.Error() + ")"
	}
	s.logger.Printf("%s: task=%d old=%s new=%s writer=%s result=%s",
		kind, id, old, status, WriterFrom(ctx), result)

	return n, err
}
