package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/ui"
)

// mergeLabelRunes bounds the other task's title as embedded in a merge comment,
// so a 4k-rune title can't produce a 4k-rune comment on the counterpart task.
const mergeLabelRunes = 80

// mergeCommentAuthor is the author stamped on both merge audit comments.
//
// 🔴 AN EARLIER VERSION OF THIS COMMENT WAS FALSE, AND IT WAS BUILT ON. It said
// "a merge is only reachable from the SESSION UI (the human tapped it)" and "this
// route has no machine counterpart precisely so that stays true". At the time both
// were wrong on the LAN, because `requireSession` was a pass-through no-op, so the
// route answered uncredentialed LAN POSTs and any machine there could mint a
// `user`-authored merge comment. Measured then — no token, no cookie, no header:
//
//	curl -s -X POST http://<lan-nodeport>/tasks/merge -d 'winner=1&loser=1'  -> 400
//
// A 4xx from the handler's own validation means the handler RAN. That sentence was
// cited to decide against a machine merge route in the #366 decision record, on
// reasoning the code did not support; the audit caught it only after the record was
// written, committed and pushed. See upstream task #394.
//
// ⚠ THE EXPOSURE IS CLOSED; THE SCOPE CAVEAT IS NOT. requireSession now refuses a
// caller with no valid signed session cookie, so the curl above gets a 401 and
// "the human tapped it" is true again on both paths. What it is NOT is a claim
// about WHICH human: the tier is one shared operator password, so "user" means
// "whoever holds the operator session", not "a specific person". Nothing in the
// codebase branches on a comment's author (measured: no `Author == "user"` /
// `== "operator"` test outside tests; the UI and push paths only DISPLAY it), so
// this stays a human-read record rather than an authorization signal. Re-read
// #394 before the team instance lands, where "user" would have to mean a
// particular person and one shared password cannot supply that.
const mergeCommentAuthor = "user"

// supersededByNS is the DESCRIPTIVE namespace stamped on a merge loser. It is
// deliberately not in notes.routingNamespaces: reserving it would make every
// tag-only PATCH carrying it hit the in-progress 409, which would break the
// hand-rolled supersede procedure in supersede-decision-2026-08-28.md §(b).
// TestSupersededByTagStaysDescriptive pins that.
//
// 🔴 IT IS THE notes CONSTANT, NOT A SECOND SPELLING OF THE SAME WORD. notes now
// declares it because the tag filter row has to know the namespace is an opaque
// external id (notes.IsExternalIDTag) rather than a label; two copies of the
// string would let the writer and the renderer disagree about which tags this
// is, and the renderer's half would fail silently — a chip per merged task,
// which is the shape of the defect that motivated the set.
const supersededByNS = notes.NSSupersededBy

// supersededByTag renders the loser's successor stamp. The value is an int64 the
// handler already parsed, so the result is always a legal tag: the namespace is
// lowercase [a-z-], the value is digits (optionally a leading '-'), and the whole
// string is at most len("superseded-by:")+20 = 34 runes against MaxTagRunes 64.
// It can therefore never be the thing that makes a merge fail — pinned for a
// max-int64 id by TestSupersededByTagIsAlwaysAValidTag.
func supersededByTag(winnerID int64) string {
	return supersededByNS + ":" + strconv.FormatInt(winnerID, 10)
}

// withoutSupersedeStamps returns tags with every `superseded-by:` entry dropped. It
// exists for ONE caller — the merge union — and the reason is at that call site.
//
// It goes through notes.ParseTag rather than a `strings.HasPrefix` so there is one
// definition of what a namespace is; a prefix test would also match a descriptive tag
// literally named `superseded-byX:…`. The input is never mutated.
func withoutSupersedeStamps(tags []string) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if ns, _ := notes.ParseTag(t); ns == supersededByNS {
			continue
		}
		out = append(out, t)
	}
	return out
}

// handleTaskMerge handles POST /tasks/merge (session): merge two tasks by
// SUPERSEDE — the winner keeps working, the loser is closed with a pointer to it.
//
// 🔴 NOTHING IS DELETED. See the design note on notes.MergeTaskTags for why
// "absorb + delete the loser" was rejected: DELETE shares dismissTask and tears
// down the loser's live agent pod, agents.note_id is ON DELETE SET NULL with no
// way to re-point it, comments/attachments CASCADE, and the schema has no
// tombstone, so the loss would be silent and unrecoverable. Every effect below is
// additive and hand-reversible.
//
// Effects, in this exact order:
//  1. the winner gains the UNION of both tag sets (notes.AddTags — a single
//     statement, so a concurrent producer's tag is not lost),
//  2. the winner gains a comment naming what it absorbed,
//  3. the loser gains the tag `superseded-by:<winnerID>`,
//  4. the loser gains a comment naming what superseded it,
//  5. the loser's status becomes complete.
//
// 🔑 (3) IS THE MACHINE-READABLE HALF OF (4), and it comes FIRST inside the loser
// block on purpose. AddTags is idempotent; AddComment is not. So tag-then-comment
// makes a retry after a mid-sequence failure re-add the tag as a no-op, whereas
// comment-then-tag would duplicate the comment on every retry.
//
// 🔑 The LOSER IS MUTATED LAST, deliberately. The store interface has no
// transaction (the same accepted residual documented on the PATCH tag path), so a
// mid-sequence failure is possible. This ordering means a failure can leave the
// winner enriched but the loser still open — visibly un-merged, and re-runnable —
// rather than the loser closed while the winner never received its tags, which
// would silently retire work.
//
// Contract: 400 on a bad/absent id, on merging a task into itself, or on either
// tag guard (project conflict / tag cap) · 404 if either task is unknown · 409 if
// EITHER task is in progress (an agent is working the spec; the same wording and
// status the edit path uses) or if EITHER task is already complete (a repeat
// merge on the loser side, open work retired into a dead task on the winner side
// — each with its own wording) · 500 on a store failure. On success: 200 with an
// empty body and `HX-Trigger: tasks:changed`, which refetches #tasks-list
// (rendering the list here would drop the filter — this route has no `tag` query
// params).
//
// 🔴 That refetch carries the active filter ONLY because of tagScript's
// `htmx:configRequest` listener. This comment used to say the container
// "re-issues ITS OWN hx-get and the active tag filter survives" — FALSE, and it
// is the belief that shipped the paging defect: htmx 2.0.4 captures a verb path
// at process time and closes over it, so writing `hx-get` moves the ATTRIBUTE
// and not the request. `tasks:changed` is a trigger-driven refetch like any
// other. See internal/ui/components.go for the listener.
func (s *Server) handleTaskMerge(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	winnerID, werr := strconv.ParseInt(r.FormValue("winner"), 10, 64)
	loserID, lerr := strconv.ParseInt(r.FormValue("loser"), 10, 64)
	if werr != nil || lerr != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if winnerID == loserID {
		http.Error(w, "cannot merge a task into itself", http.StatusBadRequest)
		return
	}

	winner, ok := s.mergeLoad(w, r, winnerID)
	if !ok {
		return
	}
	loser, ok := s.mergeLoad(w, r, loserID)
	if !ok {
		return
	}

	// Refuse in_progress on EITHER side. A merge rewrites the winner's routing tags
	// and retires the loser — both are spec changes, and an agent is mid-flight
	// against that spec. This reuses the edit path's exact 409 shape rather than
	// inventing a new one.
	//
	// ⚠ Scope, stated honestly: this is a READ-THEN-WRITE against a store with no
	// transaction, so it is not a hard block against a CONCURRENT transition. A
	// task that becomes in_progress between this Get and the writes below is not
	// caught. The window is narrow and the same residual the PATCH tag path
	// documents; closing it needs a conditional UPDATE the Store interface does not
	// have. What this DOES guarantee is that a merge is refused for any task
	// already in progress when the request arrived.
	if winner.Status == notes.StatusInProgress || loser.Status == notes.StatusInProgress {
		http.Error(w, "task is in progress and cannot be merged", http.StatusConflict)
		return
	}

	// 🔴 A COMPLETE task can be neither side of a merge. Two distinct hazards, so
	// two guards with DELIBERATELY distinct wording (a mutation test must be able
	// to tell which one fired, and the toast prints the message verbatim):
	//
	//   - LOSER complete → the merge already happened. This is the plain DOUBLE-TAP:
	//     a second POST of the same pair re-writes the "Superseded by" comment onto
	//     an already-retired task and duplicates it.
	//   - WINNER complete → open work would be silently retired INTO a dead task:
	//     the loser is closed and folded into something already done, leaving
	//     nothing working. This also refuses the CIRCULAR case (merge 1←2, then
	//     2←1, which a two-button double-tap produces) — that used to answer 200
	//     twice and leave BOTH tasks complete, each pointing at the other.
	//
	// The loser check is first only so the same-pair repeat gets the message that
	// describes what actually happened.
	if loser.Status == notes.StatusComplete {
		http.Error(w, "that task is already complete — it has nothing left to merge in", http.StatusConflict)
		return
	}
	if winner.Status == notes.StatusComplete {
		http.Error(w, "the task you chose to keep is already complete — keep the open one instead", http.StatusConflict)
		return
	}

	// Validate the UNION before touching anything: MergeTags does not validate, so
	// two legal tag sets can union into an illegal one (different projects, or past
	// MaxTags). Both are client-fixable → 400 with the guard's own message.
	//
	// 🔴 THE LOSER'S OWN SUPERSEDE STAMPS ARE EXCLUDED FROM THE UNION, AND THIS IS
	// LOAD-BEARING, NOT TIDINESS. This union is recomputed from scratch on every
	// request, so without the filter a loser that already carries
	// `superseded-by:<w>` hands it to the winner — which then advertises that IT was
	// superseded, inverting the exact signal effect (3) exists to create, and
	// permanently, because nothing on this path removes a tag. Two reachable routes:
	// a RETRY after effect (4)/(5) fails (the ordering note above deliberately leaves
	// the loser stamped AND open, i.e. re-runnable), which stamps the winner with its
	// OWN id; and a REOPENED superseded task merged into a DIFFERENT winner, which
	// needs no failure at all. It also un-breaks recovery at the cap: with the stamp
	// in the union, a re-run of a merge whose union was exactly MaxTags answers 400
	// naming a cap the operator never exceeded.
	//
	// Only the loser's CONTRIBUTION is filtered. Nothing is removed from the loser
	// itself, and the winner's own tags pass through untouched — a task keeps every
	// stamp it has ever been given.
	merged, err := notes.MergeTaskTags(winner.Tags, withoutSupersedeStamps(loser.Tags))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// (1) Winner gains the union. AddTags is idempotent and single-statement.
	if len(merged) > 0 {
		if _, err := s.ext.Notes.AddTags(r.Context(), winnerID, merged); err != nil {
			s.logger.Printf("notes: merge %d<-%d addTags: %v", winnerID, loserID, err)
			http.Error(w, "could not merge tags", http.StatusInternalServerError)
			return
		}
		observeRoutingTags(merged)
	}
	// (2) Winner records what it absorbed.
	if _, err := s.ext.Notes.AddComment(r.Context(), notes.Comment{
		NoteID: winnerID, Author: mergeCommentAuthor,
		Body: "Merged in task #" + strconv.FormatInt(loserID, 10) + mergeLabelSuffix(loser) + ".",
	}); err != nil {
		s.logger.Printf("notes: merge %d<-%d winner comment: %v", winnerID, loserID, err)
		http.Error(w, "could not record the merge", http.StatusInternalServerError)
		return
	}
	// (3) The loser is STAMPED with its successor, so a supersede done through the
	// UI leaves the same machine-readable trace (`jq .tags`) that the hand-rolled
	// procedure writes. Before this, the UI path left only prose — the LESS legible
	// of the two, which is backwards.
	//
	// 🔴 THE CHECKED-500 IS SAFE HERE, AND THAT REQUIRED A MEASUREMENT, NOT AN
	// ASSUMPTION. notes.AddTags is NOT a validation boundary: it runs
	// NormalizeTags and nothing else (see its doc comment on PGStore), and
	// ValidateTags has exactly two callers — NormalizeAndValidate and
	// MergeTaskTags — neither on this path. So this write CANNOT fail on MaxTags
	// or on the project rule, and turning its error into a 500 therefore cannot
	// make a merge fail that succeeds today. It fires only on a real store failure,
	// which is what the sibling writes above already do.
	//
	// ⚠ The flip side, stated rather than hidden: because nothing caps it here,
	// this can push a loser that already carries MaxTags tags to MaxTags+1 (pinned
	// by TestTaskMergeStampsALoserAlreadyAtTheTagCap). That is not a new hazard — it
	// is one more instance of the residual AddTags already documents (two concurrent
	// adders can do the same) — and the affected task is one being retired. Skipping
	// the write near the cap was rejected: it would make the guarantee silently
	// conditional.
	//
	// 🔴 THE COST OF THAT IS WIDER THAN "a tags REPLACE is refused", which is what an
	// earlier draft of this comment claimed. MEASURED: the session edit modal
	// resubmits every field AND every chip, so `POST /tasks/{id}/edit` on an over-cap
	// task answers 400 "too many tags: 21 (max 20)" and the WHOLE form is refused —
	// title, body, model, repo, branch — naming a cap the operator never
	// exceeded. It is recoverable in-modal by removing a chip, and the task is
	// `complete` and therefore behind the Done FILTER chip (it was the collapsed
	// Done section before the board flattened), which is why this is
	// accepted rather than blocking. Do not restate it as a tags-only refusal.
	//
	// ⚠ ALSO NOT ADDRESSED HERE, and named so it is not mistaken for closed: every
	// merge adds one permanent, count-1 entry to the tag VOCABULARY. TagVocabulary
	// has no status predicate and neither the filter chip row nor the edit-modal
	// datalist caps what it renders, so `superseded-by:<id>` accumulates in both for
	// the life of the task. Count-DESC ordering keeps it at the tail, so this
	// degrades slowly rather than breaking — but it is the unbounded-vocabulary shape
	// this codebase guards against elsewhere, and the Prometheus paragraph below
	// closes only the metrics half of it.
	//
	// observeRoutingTags is deliberately NOT called: `superseded-by` is not in
	// routingNamespaces, so this tag is descriptive and must stay out of the
	// Prometheus label space.
	if _, err := s.ext.Notes.AddTags(r.Context(), loserID, []string{supersededByTag(winnerID)}); err != nil {
		s.logger.Printf("notes: merge %d<-%d loser supersede tag: %v", winnerID, loserID, err)
		http.Error(w, "could not record the merge", http.StatusInternalServerError)
		return
	}
	// (4) Loser records what superseded it, then (5) is retired. Loser last — see
	// the ordering note above.
	//
	// ⚠ "Its tags were merged there" is now very slightly inexact and is being left
	// alone deliberately: an earlier `superseded-by:` marker on this task is the one
	// thing NOT carried over (see the union filter above). Rewording user-facing copy
	// to cover a case only reachable by reopening an already-superseded task would
	// cost every reader clarity to serve almost none, and this comment is the record
	// that the imprecision is known rather than overlooked.
	if _, err := s.ext.Notes.AddComment(r.Context(), notes.Comment{
		NoteID: loserID, Author: mergeCommentAuthor,
		Body: "Superseded by task #" + strconv.FormatInt(winnerID, 10) + mergeLabelSuffix(winner) + ". Its tags were merged there; nothing was deleted.",
	}); err != nil {
		s.logger.Printf("notes: merge %d<-%d loser comment: %v", winnerID, loserID, err)
		http.Error(w, "could not record the merge", http.StatusInternalServerError)
		return
	}
	if _, err := s.setNoteStatus(r.Context(), writerTaskMerge, loserID, notes.StatusComplete); err != nil {
		s.logger.Printf("notes: merge %d<-%d close loser: %v", winnerID, loserID, err)
		http.Error(w, "could not close the merged task", http.StatusInternalServerError)
		return
	}

	// Both cards moved (tags/comments on one, status+comment on the other), so both
	// ids are broadcast for any other open tab.
	s.broadcast(r.Context(), EventTaskChanged, strconv.FormatInt(winnerID, 10))
	s.broadcast(r.Context(), EventTaskChanged, strconv.FormatInt(loserID, 10))
	// The REQUESTING tab refreshes via HX-Trigger rather than a rendered body, so
	// the list is re-fetched instead of rendered blind here. What carries the
	// active filter across that refetch is tagScript's htmx:configRequest
	// listener, NOT the hx-get attribute on its own — see handleTaskMerge's
	// doc comment.
	w.Header().Set("HX-Trigger", "tasks:changed")
	w.WriteHeader(http.StatusOK)
}

// mergeLoad fetches one side of a merge, mapping an unknown id to 404 and a store
// failure to 500. It returns ok=false once the response has been written.
func (s *Server) mergeLoad(w http.ResponseWriter, r *http.Request, id int64) (notes.Note, bool) {
	n, err := s.ext.Notes.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "task not found", http.StatusNotFound)
			return notes.Note{}, false
		}
		s.logger.Printf("notes: merge load %d: %v", id, err)
		http.Error(w, "could not load task", http.StatusInternalServerError)
		return notes.Note{}, false
	}
	return n, true
}

// mergeLabelSuffix renders " — <title>" for a merge comment, or "" when the task
// has no display label at all. ui.TaskTitle is the ONE definition of a task's
// display label (title, falling back to directory) — the same one the dispatch
// picker and the card use, so a merge comment can never name a task differently
// from the card the user is looking at.
func mergeLabelSuffix(n notes.Note) string {
	label := ui.TaskTitle(n)
	if label == "" {
		return ""
	}
	return " — " + capRunes(label, mergeLabelRunes)
}
