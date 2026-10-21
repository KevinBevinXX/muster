package api

// registerNotesRoutes wires the Tasks tab endpoints when a notes store is set.
// 🔴 NO `if s.ext.Notes == nil { return }` GUARD — see the package doc. Every
// handler below answers the nil-store case itself, so a no-database boot serves
// a Tasks tab that says why it is empty instead of a tab whose partial 404s.
func (s *Server) registerNotesRoutes(mux Mux) {
	mux.HandleFunc("GET /ui/tasks", s.requireSession(s.handleNotesContent))
	// The task DETAIL page. Server-rendered, so it works regardless of board
	// state — which is the whole point: the old client-side `/tasks#task-<id>`
	// anchor could only reach a card that was already rendered, and gave up with
	// a toast for anything filtered out, collapsed under Done, or archived.
	//
	// 🔴 No mux conflict with POST /tasks/merge (POST-only) or with
	// GET /tasks/{id}/attachments/{aid} (more specific, so it wins). A GET of
	// /tasks/merge lands here with id="merge" and is a 400, not a 500.
	mux.HandleFunc("GET /tasks/{id}", s.requireSession(s.handleTaskDetail))
	mux.HandleFunc("GET /ui/tasks/new", s.requireSession(s.handleNoteNewModal))
	mux.HandleFunc("GET /ui/tasks/{id}/edit", s.requireSession(s.handleNoteEditModal))
	// The directory picker's search. Session-gated like every other browser-facing
	// read.
	//
	// 🔴 THE PATH IS A LITERAL HERE AND A CONSTANT IN internal/ui, AND THE
	// DUPLICATION IS DELIBERATE. `"GET "+ui.DirectorySearchPath` was tried first
	// and is REJECTED by two ledgers in this package
	// (TestEveryBrowserSurfaceRequiresAHumanSession and
	// TestEveryRoutePatternIsALiteralTheLedgerCanRead): they scan this file's
	// source for string literals, so a computed pattern is INVISIBLE to the
	// fail-closed auth ledger rather than merely unlisted. Per that guard's own
	// instruction — "a duplicated string that is pinned is safer than a computed
	// one that is unreadable" — the literal lives here and
	// TestDirectorySearchRouteIsRegisteredAtThePathTheRendererEmits asserts the two
	// spellings agree. A picker pointed at an unrouted path would silently degrade
	// to "seed only" with no error anywhere. See handleDirectorySearch.
	mux.HandleFunc("GET /api/directories", s.requireSession(s.handleDirectorySearch))
	// ONE card, re-fetched. The board refreshes by re-fetching its whole list
	// (GET /ui/tasks); the detail page has no list, so it needs a way to pull its
	// single card again on sse:task.changed / sse:agent.changed / muster:resync
	// (see ui.taskDetailLive). Same renderer, same board/detail derivation as
	// every mutation route — so a request from /tasks/{id} gets the detail shape
	// and one from anywhere else gets the compact card.
	mux.HandleFunc("GET /ui/tasks/{id}/card", s.requireSession(s.handleNoteCard))
	mux.HandleFunc("POST /tasks", s.requireSession(s.handleNoteCreate))
	mux.HandleFunc("POST /tasks/{id}/edit", s.requireSession(s.handleNoteEdit))
	// Machine endpoint (hook-token-gated): the external task-spec-drafter routes a
	// verified spec into the durable Tasks queue. Additive — does not touch the
	// permission-request flow.
	mux.HandleFunc("POST /api/tasks", s.requireHookToken(s.handleAPITaskCreate))
	// Machine reads: a producer that POSTed a Task can poll it back — list all or
	// fetch one by id — to verify its lifecycle status.
	//
	// 🔴 THE LIST IS ON THE ATTRIBUTED TIER, THE GET BY ID IS NOT, AND THE
	// ASYMMETRY IS DELIBERATE (task #633 criterion 2). The board read is the first
	// half of moving the operator's job onto chief: measured against the live pod,
	// GET /api/tasks answered 401 to chief's own credential, so chief could be told
	// to work the board and could not see it. The list already EMBEDS each task's
	// comments, attachments and agent, so widening it grants the capability without
	// also widening the by-id read.
	//
	// 🔴 PHASE 0 STEP 5 ADDED A CREDENTIAL RATHER THAN SWAPPING ONE.
	// requireHookOrServiceToken admits the shared secret, MUSTER_SERVICE_TOKEN with
	// an asserted X-Muster-Actor, AND — still — the one agent row
	// MUSTER_CHIEF_TOKEN names, which is what chief's OWN token is. The row half's
	// deletion is owed to Phase 4; deleting it earlier costs chief the board read that
	// task #633 existed to give it. ⚠ THESE TWO TASK
	// ROUTES MOVE TO muster IN PHASE 4 and are deliberately NOT special-cased on the
	// way: they ride the renamed wrapper like the other four. Adding routes to that
	// wrapper is still a privilege grant; the ledgers are internal/api's
	// serviceTierRouteLedger and cmd/muster's agentRowRoutes.
	mux.HandleFunc("GET /api/tasks", s.requireHookOrServiceToken(s.handleAPITaskList))
	mux.HandleFunc("GET /api/tasks/{id}", s.requireHookToken(s.handleAPITaskGet))
	// Machine edit (hook-token-gated): partially edit a Task's content + dispatch
	// config. In-progress tasks are immutable (409); status/provenance are ignored.
	mux.HandleFunc("PATCH /api/tasks/{id}", s.requireHookToken(s.handleAPITaskEdit))
	// Machine status-set: set a Task's lifecycle status from a producer holding the
	// shared hook token (in-cluster agent / CI / a CC session) — or, since task
	// #633, from the ONE agent row MUSTER_CHIEF_TOKEN names, or, since Phase 0 step
	// 5, from MUSTER_SERVICE_TOKEN + an asserted actor.
	// ALL statuses allowed INCLUDING `complete` (a hook-token producer is trusted,
	// unlike the bound-agent route which forbids complete); NO in-progress guard.
	// Full side-effect parity with the human status route (shared applyTaskStatus).
	//
	// 🔴 THIS IS THE PRIVILEGE EXPANSION IN TASK #633, SAID OUT LOUD. Chief becomes
	// able to mark ANY task complete. The reason that is judged acceptable is the
	// one the card records: a DISPATCHED worker is structurally denied `complete`
	// (notes.StatusAllowedForAgent, on its own /agent/task/status route) precisely
	// so an agent cannot grade its own exam, and chief does not pick up tasks. The
	// narrowing that keeps that true here is requireHookOrServiceToken's — it admits
	// ONE service credential and ONE agent row, not any agent pod. The caller it names
	// is asserted by that service on the first, and resolved from the row on the
	// second. If chief ever works tasks, revisit this.
	mux.HandleFunc("PATCH /api/tasks/{id}/status", s.requireHookOrServiceToken(s.handleAPITaskStatus))
	// Machine delete (hook-token-gated): a producer can clean up a Task it created.
	// Reuses dismissTask, so — exactly like the UI dismiss — deleting a task with a
	// live dispatched agent TEARS DOWN THAT AGENT POD. No in-progress guard (the UI
	// dismiss has none either; that IS the teardown feature).
	mux.HandleFunc("DELETE /api/tasks/{id}", s.requireHookToken(s.handleAPITaskDelete))
	// Machine comment (hook-token-gated): an EXTERNAL producer muster did not
	// provision (repo-cos / the drafter / mail-actions / CI / a CC session) posts a
	// report on a task it created. The author is derived from the X-Muster-Source
	// provenance allowlist — never from the body — so a caller ON THIS ROUTE can never
	// claim to be the privileged `user`/`operator`. ⚠ Scope: that binds this route, not
	// the system. The session routes (`POST /tasks/{id}/comments`, `POST /tasks/merge`)
	// hardcode `Author: "user"` behind `requireSession`, which now demands a signed
	// session cookie — so `user` is no longer mintable by any machine on the LAN, but
	// it still names a SHARED operator login rather than a person. See upstream task
	// #394. The session route and the per-agent route are unchanged.
	mux.HandleFunc("POST /api/tasks/{id}/comments", s.requireHookToken(s.handleAPITaskComment))
	// Machine comment RETRACTION (hook-token-gated), the DELETE twin of the POST
	// directly above. SOFT (migration 0021): the row survives, only its visibility
	// goes. 🔴 It is deliberately NOT behind requireSession — its CALLER is a
	// machine (an external producer retracting what it posted) and a machine cannot
	// complete a browser login to obtain a session cookie. The human spelling of
	// the same operation is DELETE /tasks/{id}/comments/{cid} below. See
	// handleAPITaskCommentDelete for the route shape rationale and the
	// enforce-when-set caveat.
	mux.HandleFunc("DELETE /api/tasks/{id}/comments/{cid}", s.requireHookToken(s.handleAPITaskCommentDelete))
	// Task threads, REVERSE direction (migration 0023): every live task a Claude
	// Code session touched, most-recently-touched first. The FORWARD direction has
	// no route on purpose — a task's `sessions` array is EMBEDDED on
	// GET /api/tasks and GET /api/tasks/{id}, exactly as its comments and
	// attachments are. This is the only new route the feature adds.
	mux.HandleFunc("GET /api/sessions/{id}/tasks", s.requireHookToken(s.handleAPISessionTasks))
	// Tag vocabulary: the machine route (hook-token) for producers choosing a
	// consistent label, and the session route backing the editor's <datalist>.
	mux.HandleFunc("GET /api/tags", s.requireHookToken(s.handleAPITagVocabulary))
	mux.HandleFunc("GET /ui/tags", s.requireSession(s.handleUITagVocabulary))
	// Project vocabulary, DERIVED from the `project:` tags (no column, no table):
	// the capture extension's combobox reads it to offer known projects. Machine
	// route, hook-token-gated exactly like /api/tags — the extension already holds
	// that token for POST /api/tasks.
	mux.HandleFunc("GET /api/projects", s.requireHookToken(s.handleAPIProjects))
	// Session tag MERGE routes (set-union / set-difference, never replace) — the
	// UI mirror of the machine addTags/removeTags contract. Both re-render the card.
	mux.HandleFunc("POST /tasks/{id}/tags", s.requireSession(s.handleNoteTagAdd))
	mux.HandleFunc("DELETE /tasks/{id}/tags/{tag}", s.requireSession(s.handleNoteTagRemove))
	mux.HandleFunc("DELETE /tasks/{id}", s.requireSession(s.handleNoteDelete))
	mux.HandleFunc("PATCH /tasks/{id}/status", s.requireSession(s.handleNoteStatus))
	// The agent page's Mark-complete form WITHOUT JavaScript (a plain form can
	// only GET or POST): completes the task and answers 303 → the Agents list.
	mux.HandleFunc("POST /tasks/{id}/complete", s.requireSession(s.requireSameOrigin(s.handleNoteComplete)))
	mux.HandleFunc("POST /tasks/{id}/comments", s.requireSession(s.handleNoteComment))
	// The UI's per-comment trash control. 🔴 On the SESSION tier, like every other
	// UI route — the browser carries no bearer token, so the alternative was
	// rendering the hook token into the page. Flagged in full at
	// handleNoteCommentDelete; deleting this ONE line removes the human spelling
	// and leaves the machine route intact.
	mux.HandleFunc("DELETE /tasks/{id}/comments/{cid}", s.requireSession(s.handleNoteCommentDelete))
	mux.HandleFunc("GET /tasks/{id}/attachments/{aid}", s.requireSession(s.handleNoteAttachment))
	// Task MERGE (session only). SUPERSEDE semantics: the winner unions in the
	// loser's tags + records the merge; the loser is closed with a pointer back.
	// 🔴 Nothing is deleted — see handleTaskMerge for why "absorb + delete" was
	// rejected (delete tears down the loser's agent pod and cascades its thread).
	// The literal "merge" segment cannot collide with POST /tasks/{id}/... — those
	// patterns have a further segment — nor with POST /tasks, which has none.
	mux.HandleFunc("POST /tasks/merge", s.requireSession(s.handleTaskMerge))
}
