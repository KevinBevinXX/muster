package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/metrics"
	"github.com/ZacxDev/muster/internal/notes"
)

// Task threads (migration 0023): every task-scoped machine call carrying a
// session-id header joins that session to the task's thread, so "which
// sessions worked task #N" is a query instead of prose in a comment body.
//
// 🔴 THE DETECTOR IS STRUCTURAL, NOT TEXTUAL. Nothing here — or anywhere else —
// scans a comment body or a transcript tail for "#123". A link exists because a
// session made a request against that task's route while identifying itself, which
// is a fact about the request, not an interpretation of prose. A regex over
// comment text would gain links from a session merely QUOTING another task's
// number, lose every link whose phrasing changed, and be silently wrong in both
// directions.

// maxSessionHostLen caps the optional host header. Like
// maxSessionIDLen it is an attacker-influenceable header on any hook-token call,
// and it lands in a stored column; a hostname is well under this.
const maxSessionHostLen = 128

// taskSessionHost reads the OPTIONAL host header — under BOTH the current
// X-Muster-Host and the pre-extraction X-Clawgate-Host spelling, via the shared
// provenanceHeader lookup (rune-capped, so a multibyte hostname can never be
// truncated into invalid UTF-8 that Postgres rejects on write). Absent ⇒ "" ⇒ the
// column keeps whatever an earlier touch recorded.
func taskSessionHost(r *http.Request) string {
	return capRunes(provenanceHeader(r, headerHost, legacyHeaderHost), maxSessionHostLen)
}

// linkTaskSession joins the request's session to noteID's thread with the given
// role, returning whether anything OBSERVABLE changed (a new link, or a role
// upgrade — never a bare last_seen_at touch).
//
// It is a NO-OP with no error when the request carries no session id: the vast
// majority of machine calls (repo-cos, the drafter, CI) are not Claude Code
// sessions and must not be forced to invent an identity. The scope is deliberately
// the cc_sessions TEXT id space only — dispatched devpod agents keep their existing
// `Task #N` badge and are NOT folded into this table.
//
// A dismissed or unknown task answers pgx.ErrNoRows, which is swallowed here: every
// caller either probes the task itself (and answers its own 404) or is about to,
// and a link failure must never be the thing that decides an HTTP status for a task
// that does not exist.
//
// The project/cwd enrichment is read from cc_sessions ONCE, here, and then STORED
// on the link row — it is not a join at read time. That is the whole retention
// story: cc_sessions is swept at 14 days, so a link that borrowed its context live
// would go blank exactly when the thread became the only surviving record. Nil-safe
// (no Suggest store ⇒ empty context, and the link still records).
func (s *Server) linkTaskSession(r *http.Request, noteID int64, role string) (bool, error) {
	_, sessionID := taskSource(r)
	if sessionID == "" {
		return false, nil
	}
	link := notes.SessionLink{
		SessionID: sessionID,
		Role:      role,
		Host:      taskSessionHost(r),
	}
	// 🔴 GetSessionMeta, NOT GetSession — the projection, not the whole row. The two
	// fields read below are the only ones this enrichment wants, and suggest.SessionMeta
	// carries both. GetSession would detoast and ship the session's transcript tail as
	// well: measured on the upstream Postgres, 506 rows, 248 MB of tail,
	// AVERAGE 502 kB — a half-megabyte read per task-session link POST to fill two short
	// strings. A projection that exists and is not called is not a fix, which is how this
	// call site survived the round that introduced it.
	//
	// 🔴 THIS ONE LOOKUP NOW ANSWERS TWO QUESTIONS, AND THE SECOND IS THE NEW ONE.
	// `ok` — does a transcript record for this session exist right now — is what
	// internal/notes used to probe for itself in SQL and may not any more (plan
	// §2.2): it is the input to the STORED, MONOTONIC detail_seen column, which is
	// the bit that separates "transcript expired" from "no transcript recorded" in
	// the UI. It is read here, from the store that owns it, and PASSED DOWN.
	//
	// 🔴 SO `ok` IS TAKEN WHETHER OR NOT THE ROW EXISTS — the old `err == nil && ok`
	// guard covered the whole statement and would have thrown the absence away. The
	// project/cwd copy still only happens on a hit; a miss must not blank a context
	// an earlier touch recorded.
	//
	// A FAILED lookup leaves detailSeen false, which is safe in this direction and
	// only this direction: the store ORs it into the column, so a false teaches the
	// row nothing and can never reset a true. (The READ path has no such luxury —
	// see withSessionLiveness, where a failed probe is an error rather than a false.)
	detailSeen := false
	if s.router != nil {
		if sess, ok, err := s.router.SessionMeta(r.Context(), sessionID); err == nil {
			detailSeen = ok
			if ok {
				link.Project = sess.Project
				link.Cwd = sess.Cwd
			}
		} else {
			// Enrichment only — a lookup failure must not cost us the link itself.
			s.logger.Printf("task sessions: session context %q: %v", sessionID, err)
		}
	}
	_, changed, err := s.ext.Notes.LinkSession(r.Context(), noteID, link, detailSeen)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if changed {
		// Bounded label: role is one of exactly three Go constants. The SESSION ID
		// is never a label — it is unbounded producer-supplied text, the same
		// discipline taskSourceAllowlist exists for.
		metrics.TaskSessionLinks.WithLabelValues(role).Inc()
	}
	return changed, nil
}

// noteTaskSession is the handler-side wrapper. It links and reports only whether
// anything CHANGED (so the caller can broadcast); it can never fail a request.
//
// 🔴 THERE IS DELIBERATELY NO FAILURE RETURN. This replaces a
// `linkTaskSessionOrFail` that answered 409 when the per-task session cap was hit,
// which produced two measured defects: PATCH …/status applied the status change and
// THEN 409'd (the caller told it failed about a request that had succeeded), and a
// plain GET answered 409 forever once fifty sessions had read a task. A provenance
// breadcrumb must never break the operation it is annotating, so no outcome here
// can fail a request.
//
// ⚠ WHAT IS ACTUALLY COUNTED, stated precisely because the counter is read as a
// completeness signal. Exactly TWO outcomes reach this function's error branch and
// increment muster_task_session_links_skipped_total:
//
//	thread_full — the cap had nothing evictable, so a `read` breadcrumb was dropped
//	error       — a store failure
//
// A DISMISSED or UNKNOWN task is NOT among them. linkTaskSession swallows
// pgx.ErrNoRows itself and returns (false, nil) — see its own comment for why: the
// caller either probes the task or is about to, and a link failure must not decide
// the HTTP status for a task that does not exist. So that case is neither logged
// nor counted here, and the counter is a measure of links lost DESPITE a live task,
// not of every link not written. An earlier version of this comment listed the
// dismissed case as counted, which would have made anyone auditing the counter
// believe it covered more than it does.
func (s *Server) noteTaskSession(r *http.Request, noteID int64, role string) bool {
	changed, err := s.linkTaskSession(r, noteID, role)
	if err != nil {
		if errors.Is(err, notes.ErrTooManySessions) {
			// The thread is full of created/worked links, so this `read` breadcrumb
			// was dropped. Counted (bounded label) so the case is visible in metrics
			// rather than only in a log line nobody reads.
			metrics.TaskSessionLinksSkipped.WithLabelValues("thread_full").Inc()
			s.logger.Printf("task sessions: link %d (%s) skipped: %v", noteID, role, err)
			return false
		}
		metrics.TaskSessionLinksSkipped.WithLabelValues("error").Inc()
		s.logger.Printf("task sessions: link %d (%s): %v", noteID, role, err)
		return false
	}
	return changed
}

// handleAPISessionTasks handles GET /api/sessions/{id}/tasks (machine,
// hook-token-gated): the REVERSE lookup — every live task the given Claude Code
// session touched, most-recently-touched first.
//
// This is the ONE new route this feature adds. There is deliberately NO
// GET /api/tasks/{id}/sessions to match it: the codebase EMBEDS a task's children
// (comments, attachments, and now sessions) on the task reads rather than adding
// sub-GETs — the absent /comments GET answers 405 and has already sent people
// hunting for a route that was never there. The reverse direction has no such
// carrier, which is why it gets a route.
//
// An unknown session is an empty 200, not a 404: sessions are not resources
// muster owns (the id space is Claude Code's, and cc_sessions is reaped at 14
// days), so "no tasks recorded for this id" is an ordinary answer and a 404 would
// read as "this session never existed", which we cannot know.
func (s *Server) handleAPISessionTasks(w http.ResponseWriter, r *http.Request) {
	sessionID := strings.TrimSpace(r.PathValue("id"))
	if sessionID == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing session id"})
		return
	}
	// 🔴 REFUSE an over-long id; do NOT truncate it, and do NOT echo it back. A real
	// session id is a 36-char uuid, so anything past maxSessionIDLen is a caller bug
	// — and this is a LOOKUP KEY, where truncating would silently query a different
	// session than the one asked for (the header can truncate because it is being
	// STORED, not matched). Echoing was the other half of the defect: a 4000-char
	// path id came back inside a 4028-byte 200 body, i.e. an unbounded reflector.
	// The message names the limit and nothing the caller sent.
	if len([]rune(sessionID)) > maxSessionIDLen {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": fmt.Sprintf("session id too long: max %d characters", maxSessionIDLen),
		})
		return
	}
	links, err := s.ext.Notes.TasksForSession(r.Context(), sessionID)
	if err != nil {
		s.logger.Printf("task sessions: tasks for session %q: %v", sessionID, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not list tasks for session"})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"sessionId": sessionID,
		"tasks":     links,
	})
}

// broadcastTaskLink fires task.changed for a link that actually changed something.
// A bare last_seen_at touch does NOT broadcast: the Stop hook drives a session's
// activity, so every read would otherwise nudge every open Tasks tab on a timer.
func (s *Server) broadcastTaskLink(ctx context.Context, changed bool, noteID int64) {
	if changed {
		s.broadcast(ctx, EventTaskChanged, strconv.FormatInt(noteID, 10))
	}
}
