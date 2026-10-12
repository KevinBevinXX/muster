package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/metrics"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/taskstatus"
	"github.com/ZacxDev/muster/internal/ui"
)

// --- Task tags: validation, telemetry, and the tag HTTP surface ---------------
//
// Tags are a ROUTING KEY authored by BOTH machine producers and the human, so:
//   - a routing tag must FAIL LOUD, never be a silently-ignored string
//     (resolveTaskTags validates every tag's grammar and refuses a bad one),
//   - merges must not lose an update between the two author classes
//     (AddTags/RemoveTags are single-statement set operations in the store),
//   - Prometheus label cardinality must stay bounded (observeRoutingTags emits
//     ONLY the reserved NAMESPACE, never a free-form tag or a tag's value).

// errTagValidation wraps a tag grammar/routing failure so callers can map it to a
// 400 naming the offending tag.
type errTagValidation struct{ err error }

func (e errTagValidation) Error() string { return e.err.Error() }

// resolveTaskTags is the SINGLE validation entry point for every tag write path
// (POST/PATCH /api/tasks, the session tag routes, the edit form). It normalizes
// (lowercase / whitespace→'-' / dedupe / sort, dropping empties), validates the
// grammar loudly. Reserved routing namespaces:
//
//   - ⚠ `runbook:<name>` USED TO BE HARD-validated against the runbook store
//     here. Runbooks are removed; `runbook:` is now a descriptive tag and a task
//     still carrying one (written before the removal) saves like any other.
//   - `initiative:<slug>` is deliberately NOT resolved: the initiatives store lives
//     in a different cluster's Postgres, and making task creation depend on a
//     cross-cluster DB would trade a real availability risk for a cosmetic check.
//     Unresolved slugs render as a muted chip instead of failing the write.
//   - `gate:` / `auto:` need only the charset/length check.
//
// It returns errTagValidation for anything a client can fix (→ 400) and a plain
// error for an infrastructure failure (→ 500).
func (s *Server) resolveTaskTags(ctx context.Context, raw []string) ([]string, error) {
	tags, err := notes.NormalizeAndValidate(raw)
	if err != nil {
		return nil, errTagValidation{err}
	}
	return tags, nil
}

// observeRoutingTags records the CARDINALITY-SAFE routing-tag metric: one
// increment per routing tag, labelled by its reserved NAMESPACE ONLY. A tag's
// value (and any free-form tag) NEVER reaches a Prometheus label — free-form tags
// can carry repo/project names, and the label space must stay bounded. This
// mirrors the taskSourceAllowlist discipline and is pinned by a regression test.
func observeRoutingTags(tags []string) {
	for _, t := range tags {
		if !notes.IsRoutingTag(t) {
			continue
		}
		ns, _ := notes.ParseTag(t)
		metrics.RoutingTags.WithLabelValues(ns).Inc()
	}
}

// logAutoDispatch is the auto-dispatch PLUMBING (spec §4): recognise the tag,
// evaluate eligibility against the feature flag, and leave a log line. It
// deliberately does NOT dispatch anything — the manual dispatch loop has not yet
// closed end-to-end even once, so arming this would turn an unproven feature into
// an unattended one. Enabling it is a one-env-var, fully reversible change
// (MUSTER_TAG_AUTODISPATCH).
func (s *Server) logAutoDispatch(id int64, tags []string) {
	if !notes.HasAutoDispatch(tags) {
		return
	}
	if notes.AutoDispatchEligible(s.ext.TagAutoDispatch, tags) {
		s.logger.Printf("tags: task %d is auto:dispatch ELIGIBLE (MUSTER_TAG_AUTODISPATCH on) — auto-dispatch behaviour is not implemented in this release; dispatch it manually", id)
		return
	}
	s.logger.Printf("tags: task %d carries auto:dispatch but auto-dispatch is OFF (MUSTER_TAG_AUTODISPATCH) or the task is gated — treating it as a descriptive tag", id)
}

// queryTags reads the repeated `?tag=` query parameters and normalizes them.
// Invalid tags in a FILTER are not an error: a filter is a read, so a bogus value
// simply matches nothing (the caller renders the filtered-empty state).
func queryTags(q url.Values) []string { return notes.NormalizeTags(q["tag"]) }

// listTasksFiltered returns the task list for an optional AND tag filter,
// delegating to the store's indexed path. An empty filter is a plain List.
func (s *Server) listTasksFiltered(ctx context.Context, tags []string) ([]notes.Note, error) {
	if len(tags) == 0 {
		return s.ext.Notes.List(ctx)
	}
	return s.ext.Notes.ListByTags(ctx, tags)
}

// The board's paging constants.
//
// 🔴 defaultTaskPageSize EXISTS BECAUSE THE FRAGMENT HAD NO CAP AT ALL. Measured
// on the live board before this change: GET /ui/tasks answered 2.1 MB across 293
// cards, every one of them re-fetched and re-morphed on EVERY sse:task.changed
// and sse:agent.changed event — i.e. whenever any agent anywhere moved. The cap
// is on the RENDER, not on the query's meaning: Page.Total still counts the whole
// matching set, so the list can say how much it is not showing.
const (
	defaultTaskPageSize = 50
	// maxTaskLimit bounds an attacker- (or typo-) supplied ?limit=. Without it a
	// single request could ask the server to render an unbounded number of cards.
	// It is well above the live task count so "show everything" stays reachable.
	maxTaskLimit = 2000
)

// queryTaskLimit reads ?limit= and clamps it into [1, maxTaskLimit].
//
// 🔴 EVERY BAD INPUT FALLS BACK TO THE DEFAULT PAGE, never to "unlimited" and
// never to zero. An absent, empty, non-numeric, zero or negative value all mean
// "the caller did not choose", and the safe answer to that is the ordinary first
// page: falling back to unlimited would reinstate the 2.1 MB fragment on a typo,
// and falling back to zero would render an empty board that looks like data loss.
func queryTaskLimit(q url.Values) int {
	raw := strings.TrimSpace(q.Get("limit"))
	if raw == "" {
		return defaultTaskPageSize
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return defaultTaskPageSize
	}
	if n > maxTaskLimit {
		return maxTaskLimit
	}
	return n
}

// nextTaskLimit is the limit the board's show-more control should request given
// the limit that was APPLIED, or 0 when there is no larger page to ask for.
//
// 🔴 0 IS THE WHOLE POINT, AND IT IS A DEAD-CONTROL GUARD. The caller used to
// render `limit + defaultTaskPageSize` unconditionally. Once the applied limit
// reached maxTaskLimit on a board with more matching tasks than that, the
// control kept rendering (len(Cards) < Total is still true) while every tap
// asked for maxTaskLimit+defaultTaskPageSize, which queryTaskLimit clamped
// straight back to the limit already on screen: a button that renders forever
// and changes nothing. 0 tells tasksShowMore to render the honest count with no
// button rather than an affordance that cannot act.
//
// Unreachable at today's task counts (maxTaskLimit is 2000 against a live board
// of a few hundred). It is a latent control, not a live bug, and it is fixed
// here rather than left because the fix is one clamp in the same file that owns
// the other one.
func nextTaskLimit(limit int) int {
	if limit >= maxTaskLimit {
		return 0
	}
	if n := limit + defaultTaskPageSize; n < maxTaskLimit {
		return n
	}
	return maxTaskLimit
}

// queryStatus reads ?status= and returns the status, or "" for absent/unknown.
//
// An unknown status degrades to "no status filter" — the whole board — rather
// than to an empty result. A filter is a READ, so a bogus value must not be able
// to hide work: the same rule queryTags already applies to a bogus tag, except
// that a tag genuinely matches nothing whereas a status has no such reading.
//
// ⚠ IT USED TO BE queryStatusLane AND TO VALIDATE AGAINST A SEPARATE "LANE"
// VOCABULARY. That vocabulary is gone (see internal/taskstatus/label.go): the
// chips ARE the statuses now, so there is one thing to validate against. The
// user-visible consequence is that `?status=done` — the old spelling of
// Complete — is no longer valid and lands on the whole board rather than on the
// completed tasks. Pinned by TestAnUnknownStatusFilterShowsEverything.
func queryStatus(q url.Values) string {
	status := strings.TrimSpace(q.Get("status"))
	if !taskstatus.Valid(status) {
		return ""
	}
	return status
}

// statusPredicate turns a validated status into the ListFilter.Statuses value.
//
// 🔴 nil IS "NO PREDICATE" AND AN EMPTY NON-NIL SLICE IS "MATCH NOTHING", AND
// THE TWO ARE ONE CHARACTER APART. `Statuses` becomes `status = ANY($n)`, so
// handing it `[]string{}` builds a query that matches no rows — a bogus filter
// value would silently empty the board while the chip row showed nothing
// selected. This is the rule that used to live in taskstatus.LaneStatuses; it
// moved here with the layer's deletion, to the ONE call site that needs it.
func statusPredicate(status string) []string {
	if status == "" {
		return nil
	}
	return []string{status}
}

// tagVocabulary loads the tag vocabulary (tag + count, most-used first).
// Best-effort for RENDER paths: a failure degrades to no chips rather than a 500.
func (s *Server) tagVocabulary(ctx context.Context) []notes.TagCount {
	v, err := s.ext.Notes.TagVocabulary(ctx)
	if err != nil {
		s.logger.Printf("notes: tag vocabulary: %v", err)
		return nil
	}
	return v
}

// handleAPITagVocabulary handles GET /api/tags (machine, hook-token-gated):
// the tag vocabulary with counts, for producers choosing a consistent label and
// for the editor's datalist.
func (s *Server) handleAPITagVocabulary(w http.ResponseWriter, r *http.Request) {
	v, err := s.ext.Notes.TagVocabulary(r.Context())
	if err != nil {
		s.logger.Printf("notes: api tag vocabulary: %v", err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not load tags"})
		return
	}
	if v == nil {
		v = []notes.TagCount{}
	}
	s.writeJSON(w, http.StatusOK, v)
}

// handleUITagVocabulary handles GET /ui/tags (session): the same vocabulary, for
// the edit modal's <datalist>. Separate from the machine route so the browser
// (which holds no hook token) can populate the tag input's suggestions.
func (s *Server) handleUITagVocabulary(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := ui.RenderTagDatalist(w, s.tagVocabulary(r.Context())); err != nil {
		s.logger.Printf("notes: render tag datalist: %v", err)
	}
}

// handleNoteTagAdd handles POST /tasks/{id}/tags (session): MERGE one or more
// tags into a task (set-union, idempotent) and re-render the card. Merge — not
// replace — because a machine producer may be editing the same task's tags
// concurrently.
func (s *Server) handleNoteTagAdd(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	// Accept both a repeated `tag` field and a single comma/space-separated `tags`.
	raw := append([]string{}, r.Form["tag"]...)
	raw = append(raw, splitTagInput(r.FormValue("tags"))...)
	tags, err := s.resolveTaskTags(r.Context(), raw)
	if err != nil {
		var ve errTagValidation
		if errors.As(err, &ve) {
			http.Error(w, ve.Error(), http.StatusBadRequest)
			return
		}
		s.logger.Printf("notes: add tags %d: %v", id, err)
		http.Error(w, "could not add tags", http.StatusInternalServerError)
		return
	}
	// A routing-tag change on an in-progress task is a spec change → refused; a
	// descriptive-only edit is just a label and stays allowed (see taskTagGuard).
	if code, msg, ok := s.taskTagGuard(r.Context(), id, tags); !ok {
		http.Error(w, msg, code)
		return
	}
	// This route MERGES, so the at-most-one-project rule has to be evaluated
	// against the merged result, not just the incoming tags (see projectMergeGuard).
	if code, msg, ok := s.projectMergeGuard(r.Context(), id, tags, nil); !ok {
		http.Error(w, msg, code)
		return
	}
	note, err := s.ext.Notes.AddTags(r.Context(), id, tags)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		s.logger.Printf("notes: add tags %d: %v", id, err)
		http.Error(w, "could not add tags", http.StatusInternalServerError)
		return
	}
	observeRoutingTags(tags)
	s.logAutoDispatch(note.ID, note.Tags)
	s.broadcast(r.Context(), EventTaskChanged, strconv.FormatInt(note.ID, 10))
	s.renderNoteCard(w, r, note)
}

// handleNoteTagRemove handles DELETE /tasks/{id}/tags/{tag} (session): remove one
// tag (set-difference, idempotent) and re-render the card.
func (s *Server) handleNoteTagRemove(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	tag := notes.NormalizeTag(r.PathValue("tag"))
	if tag == "" {
		http.Error(w, "tag required", http.StatusBadRequest)
		return
	}
	if code, msg, ok := s.taskTagGuard(r.Context(), id, []string{tag}); !ok {
		http.Error(w, msg, code)
		return
	}
	note, err := s.ext.Notes.RemoveTags(r.Context(), id, []string{tag})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		s.logger.Printf("notes: remove tag %d: %v", id, err)
		http.Error(w, "could not remove tag", http.StatusInternalServerError)
		return
	}
	s.broadcast(r.Context(), EventTaskChanged, strconv.FormatInt(note.ID, 10))
	s.renderNoteCard(w, r, note)
}

// taskTagGuard refines the blanket in-progress immutability rule for TAG edits
// (spec §6): a DESCRIPTIVE-tag-only change is allowed while a task is in progress
// (a label is not a spec change), but any ROUTING-tag change still 409s. It
// returns (statusCode, message, ok=false) when the edit must be refused.
func (s *Server) taskTagGuard(ctx context.Context, id int64, tags []string) (int, string, bool) {
	if len(notes.RoutingTags(tags)) == 0 {
		return 0, "", true
	}
	cur, err := s.ext.Notes.Get(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return http.StatusNotFound, "task not found", false
		}
		s.logger.Printf("notes: tag guard get %d: %v", id, err)
		return http.StatusInternalServerError, "could not load task", false
	}
	if cur.Status == notes.StatusInProgress {
		return http.StatusConflict, "task is in progress: routing tags cannot be changed", false
	}
	return 0, "", true
}

// splitTagInput splits a free-text tag field on commas and whitespace, so the
// editor can submit "a, b c" as three tags. Normalization/validation happens
// downstream in resolveTaskTags.
func splitTagInput(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
}
