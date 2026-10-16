package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/metrics"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/taskstatus"
	"github.com/ZacxDev/muster/internal/ui"
)

// taskSourceAllowlist bounds the cardinality of the muster_tasks_created_total
// {source} label: taskSource only ever emits one of these values (anything else
// collapses to "api"), so an arbitrary Origin / X-Muster-Source header can
// never blow up the label space.
var taskSourceAllowlist = map[string]bool{
	"extension":   true,
	"api":         true,
	"drafter":     true,
	"repo-cos":    true,
	"claude-code": true,
	"clickup":     true,
}

// taskSource is the SINGLE source of truth for a POST /api/tasks request's
// provenance: it derives BOTH the muster_tasks_created_total{source} metric
// label AND the stored source_type/source_session_id (migration 0017) — no client
// change required. It returns:
//   - source: the producer id, ALWAYS one of the fixed allowlist values so the
//     metric label cardinality stays bounded. An explicit source header
//     wins IF it names an allowlisted producer (extension/api/drafter/repo-cos/
//     claude-code/clickup); an unknown value collapses to "api"; else a chrome-extension://
//     or moz-extension:// Origin → "extension"; else the default "api".
//   - sessionID: the trimmed session-id header ("" when absent).
//
// 🔴 BOTH HEADERS ARE READ UNDER TWO SPELLINGS — the current X-Muster-* and the
// pre-extraction X-Clawgate-* that installed clients still send — via the single
// provenanceHeader lookup in provenance_headers.go, which also holds the measured
// account of what the rename cost. Do NOT re-open-code the preference here.
//
// STORAGE SEMANTICS (see handleAPITaskCreate): the default/unidentified "api"
// source is stored as NULL — a plain `api` post or an omitted X-Muster-Source
// header ⇒ source_type NULL = pre-0017 behavior. Only a NON-default named producer
// (extension/drafter/repo-cos/claude-code/clickup) is persisted. The metric label,
// however, is ALWAYS the returned source (incl. "api"), so metric behavior is
// unchanged from pre-0017.
// maxSessionIDLen caps the stored session-id header. It's an attacker-influenced
// header on any hook-token POST; a real id (a CC session UUID) is 36 chars, so 128 is
// generous. Capping at ingest bounds notes.source_session_id (DB-bloat hygiene) — the
// render path already truncates+escapes, this protects storage.
const maxSessionIDLen = 128

func taskSource(r *http.Request) (source, sessionID string) {
	sessionID = provenanceHeader(r, headerSessionID, legacyHeaderSessionID)
	if rs := []rune(sessionID); len(rs) > maxSessionIDLen {
		// rune-safe cap: byte-slicing could split a multibyte rune into invalid
		// UTF-8, which Postgres rejects on INSERT (a failed task-create, not a
		// bounded store). Cap by runes so the stored value stays valid text.
		sessionID = string(rs[:maxSessionIDLen])
	}
	if h := provenanceHeader(r, headerSource, legacyHeaderSource); h != "" {
		if taskSourceAllowlist[h] {
			return h, sessionID
		}
		return "api", sessionID
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if strings.HasPrefix(origin, "chrome-extension://") || strings.HasPrefix(origin, "moz-extension://") {
		return "extension", sessionID
	}
	return "api", sessionID
}

// storedSourceType maps a derived taskSource producer to the value PERSISTED as
// notes.source_type: a named producer is stored verbatim; the default/unidentified
// "api" (a plain post or an omitted/unknown X-Muster-Source header) is stored as
// NULL (nil), which reads back exactly as a pre-0017 task (no chip, field omitted).
func storedSourceType(source string) *string {
	if source == "" || source == "api" {
		return nil
	}
	s := source
	return &s
}

// nilIfEmpty returns a *string for a non-empty value, or nil (SQL NULL) for the
// empty string — so an absent X-Muster-Session-Id stores NULL.
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// taskCardViews pairs each task with its most-recent linked agent (STORED status,
// no k8s call) for the grouped Tasks page. Best-effort: an agent-lookup failure
// degrades to agent-less cards (they keep the Dispatch button) rather than 500.
func (s *Server) taskCardViews(ctx context.Context, list []notes.Note) []ui.TaskCardView {
	byNote := map[int64]agents.Agent{}
	if s.ext.Agents != nil && len(list) > 0 {
		ids := make([]int64, 0, len(list))
		for _, n := range list {
			ids = append(ids, n.ID)
		}
		if m, err := s.ext.Agents.AgentsByNoteIDs(ctx, ids); err == nil {
			byNote = m
		} else {
			s.logger.Printf("notes: agents by note ids: %v", err)
		}
	}
	views := make([]ui.TaskCardView, 0, len(list))
	for _, n := range list {
		v := ui.TaskCardView{Note: n}
		if a, ok := byNote[n.ID]; ok {
			v.Agent = agentBrief(a)
		}
		views = append(views, v)
	}
	return views
}

// taskCardView builds the single-card view (task + its latest linked agent) for
// the outerHTML re-render after a status change / comment. Best-effort agent
// lookup (nil on miss/failure ⇒ the card falls back to the Dispatch button).
func (s *Server) taskCardView(ctx context.Context, n notes.Note) ui.TaskCardView {
	v := ui.TaskCardView{Note: n}
	if s.ext.Agents != nil {
		if m, err := s.ext.Agents.AgentsByNoteIDs(ctx, []int64{n.ID}); err == nil {
			if a, ok := m[n.ID]; ok {
				v.Agent = agentBrief(a)
			}
		}
	}
	return v
}

// agentBrief projects a stored agent into the minimal linked-agent view a task
// card renders (id, slug name, display name, stored status).
func agentBrief(a agents.Agent) *ui.AgentBrief {
	return &ui.AgentBrief{ID: a.ID, Name: a.Name, DisplayName: a.DisplayName, Status: a.Status}
}

// maxAttachmentBytes caps a single note attachment. Attachments are stored
// inline in Postgres, so this also bounds row size.
const maxAttachmentBytes = 10 << 20 // 10 MiB

// handleNotesContent serves the /ui/notes partial: the note list plus the FAB
// and the create-note modal (with the directory picker sourced from the cwd
// history of permission requests).
// It also honours the repeated `?tag=` AND filter (plus `?status=` and
// `?limit=`): the filter chip rows and the list are rendered together in this one
// partial from what THIS request asked for, so the chips can never disagree with
// the cards beside them.
//
// ⚠ That is consistency, not persistence. This comment used to end "and an
// SSE-driven refresh keeps whatever filter is applied", which was false: a
// refresh keeps only what its URL carries, and until tagScript's
// htmx:configRequest listener landed (internal/ui/components.go) a
// trigger-driven refetch of #tasks-list carried nothing at all.
func (s *Server) handleNotesContent(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := queryTags(q)
	status := queryStatus(q)
	limit := queryTaskLimit(q)
	// 🔴 ALL THREE NARROW TOGETHER. They are one ListFilter and therefore one
	// query — a tag filter, a project (which IS a tag) and a status compose by
	// construction rather than by each combination being remembered.
	page, err := s.ext.Notes.ListPage(r.Context(), notes.ListFilter{
		Tags:     filter,
		Statuses: statusPredicate(status),
		Limit:    limit,
	})
	if err != nil {
		s.logger.Printf("notes: list: %v", err)
		http.Error(w, "could not load notes", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// One vocabulary read serves BOTH filter rows — the project row is derived from
	// the same tag counts, so the two can never disagree (and it costs no extra
	// query). Best-effort like the tag row: a failure degrades to no chips, not a 500.
	vocab := s.tagVocabulary(r.Context())
	if err := ui.RenderNotesCards(w, ui.TasksView{
		Cards:        s.taskCardViews(r.Context(), page.Notes),
		Vocabulary:   vocab,
		ActiveTags:   filter,
		Projects:     notes.ProjectsFromVocabulary(vocab),
		ActiveStatus: status,
		Limit:        limit,
		Total:        page.Total,
		// The next page is computed HERE, not in the browser: the server is the
		// only side that knows Total, and the only side that knows its own clamp
		// (nextTaskLimit answers 0 once maxTaskLimit is already applied, so the
		// control does not render a tap that cannot advance).
		//
		// ⚠ This used to add defaultTaskPageSize inline and call the resulting URL
		// "the one place a limit survives #tasks-list's SSE-triggered refetch".
		// The URL is necessary but was not sufficient — htmx captures the
		// container's verb path at process time, so until tagScript's
		// htmx:configRequest listener landed (internal/ui/components.go) no
		// trigger-driven refetch carried it, and the tag filter had been silently
		// resetting on the same channel since it shipped.
		NextLimit: nextTaskLimit(limit),
	}); err != nil {
		s.logger.Printf("notes: render: %v", err)
	}
}

// directorySeed reads the picker's SEED set — the most recently used working
// directories — and reports whether the read FAILED, as distinct from returning
// nothing.
//
// 🔴 THE bool IS THE POINT, AND IT REPLACES A COMMENT THAT SAID THE OPPOSITE.
// Both modal handlers used to write `dirs = nil // non-fatal: the picker is just
// empty` and move on. "The picker is just empty" is precisely the problem: an
// empty picker is ALSO what a broken read looks like, so the two states were
// indistinguishable on screen and the failure had nowhere to surface but a log
// nobody tails. Non-fatal is still right — a missing suggestion list must not
// block filing a task — but non-fatal is not the same as unsaid. The flag rides
// into the view and renders as an amber line (ui.directoryPickerFailed).
//
// The log line is KEPT rather than replaced: it is the only record with the
// underlying error text, which the operator-facing sentence deliberately omits.
func (s *Server) directorySeed(r *http.Request) (dirs []string, failed bool) {
	return s.directories(r.Context(), "", notes.DefaultDirectoryLimit)
}

// handleNoteNewModal serves the /ui/notes/new partial: the create-note form body
// (directory picker seeded from the cwd history of permission requests, with the
// rest of the archive reachable through ui.DirectorySearchPath).
func (s *Server) handleNoteNewModal(w http.ResponseWriter, r *http.Request) {
	dirs, failed := s.directorySeed(r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// `body` pre-fills the task text: ComposePage (a share, the New-task
	// shortcut) opens the sheet with it. Display only — nothing is written until
	// the operator presses Save, which is the ordinary POST /tasks.
	if err := ui.RenderNotesModal(w, dirs, failed, r.URL.Query().Get("body")); err != nil {
		s.logger.Printf("notes: render modal: %v", err)
	}
}

// handleDirectorySearch serves GET /api/directories?q=…: the directory picker's
// search answer, as a JSON array of paths.
//
// 🔴 IT IS THE REASON THE PICKER CAN BE SMALL AND STILL COMPLETE. The modal
// renders only notes.DefaultDirectoryLimit seeded options (6,789 bytes); every
// other directory in the archive — 1,532 of them live — is reached by typing, which
// the combobox turns into a debounced, client-cached call to this route. JSON
// (not an HTML fragment) because this is the shape the existing
// data-combobox-remote component already consumes, byte for byte the same as
// /api/openrouter/models: an array of strings, no new client code.
//
// ⚠ THE /api/ PREFIX IS LOAD-BEARING, NOT DECORATIVE. web/static/sw.js's fetch
// handler returns early for anything under /api/ (and for /events), so this
// route bypasses the service worker's cache entirely. Under /ui/ it would have
// been eligible for the shell cache, and a picker served a stale directory list
// from a cache is a quieter version of the bug this change closes.
//
// A blank q is NOT an error and NOT an empty answer — it returns the same seed
// the modal rendered, which is what makes clicking into the field (and clearing
// the box) show the recents again rather than nothing. Same idiom as
// handleChiefThreadSearch.
//
// ⚠ A store error answers 500 rather than `[]`. An empty array here would be
// indistinguishable from "nothing matched" — the exact conflation this whole
// change exists to remove — and the combobox already degrades safely, because
// its fetch treats a non-OK response as "no remote results" and leaves the
// seeded options on screen.
func (s *Server) handleDirectorySearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	limit := notes.MaxDirectoryResults
	if q == "" {
		limit = notes.DefaultDirectoryLimit
	}
	dirs, failed := s.directories(r.Context(), q, limit)
	if failed {
		http.Error(w, "could not search directories", http.StatusInternalServerError)
		return
	}
	// A non-nil empty slice, so the body is `[]` rather than `null` — the
	// combobox's `(list || [])` would cope, but a JSON null answer to a search is
	// a needless second spelling of "nothing matched".
	if dirs == nil {
		dirs = []string{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(dirs)
}

// handleNoteCreate handles POST /tasks (multipart/form-data): create a note and
// store any attachments, then re-render the notes panel (which resets the modal
// to closed).
//
// An attachment over maxAttachmentBytes refuses the WHOLE request with a 413 and
// writes nothing — see the guard below for why that is the only shape that
// reaches the user.
func (s *Server) handleNoteCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseMultipartForm(maxAttachmentBytes); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	body := strings.TrimSpace(r.FormValue("body"))
	directory := r.FormValue("directory")
	if body == "" {
		// Empty body is a no-op: the inline quick-add guards this client-side and
		// shows a hint, so we don't surface a 400 toast. Re-render the unchanged
		// list (a 200) so any in-flight htmx swap is harmless.
		s.renderNotesPanel(w, r)
		return
	}

	// 🔴 AN OVERSIZE ATTACHMENT FAILS THE REQUEST. IT IS NOT SKIPPED.
	//
	// What this replaced: the storage loop below used to `continue` past any file
	// over the cap after logging a line nobody reads, and the handler still fell
	// through to renderNotesPanel's 200. That is silent data loss, not a degraded
	// save — htmx swaps a 200, the form's `hx-on::after-request` sees
	// `event.detail.successful` and closes the modal, and resyncScript's toast
	// listener is bound to `htmx:responseError`, which a 200 is not. The task
	// saved, the modal closed, and the attachment was gone with NOTHING on
	// screen.
	//
	// 🔴 IT RUNS BEFORE Notes.Create, AND THAT ORDER IS THE POINT. Rejecting
	// after the write would leave the task stored while the browser is told the
	// request failed, so the obvious retry silently duplicates the task. Checking
	// first makes the request atomic: nothing is written, htmx does not swap, the
	// modal stays open with the user's text intact, and the plain-text body below
	// is exactly what resyncScript's htmx:responseError listener renders as a
	// toast (it falls back to a generic message only for a body starting with
	// '<', so this must stay plain text).
	//
	// This is the ONLY size predicate for an attachment. The storage loop below
	// deliberately does not repeat it — a second copy is a second thing to get
	// wrong, and it is what made the first one silent.
	if r.MultipartForm != nil {
		for _, fh := range r.MultipartForm.File["attachments"] {
			if fh.Size > maxAttachmentBytes {
				s.logger.Printf("notes: attachment %q too large (%d bytes > %d), rejecting create", fh.Filename, fh.Size, maxAttachmentBytes)
				http.Error(w, fmt.Sprintf(
					"attachment %q is %.1f MiB, over the %d MiB limit for a single file. Nothing was saved — remove it and save again.",
					fh.Filename, float64(fh.Size)/(1<<20), maxAttachmentBytes>>20,
				), http.StatusRequestEntityTooLarge)
				return
			}
		}
	}

	// Optional dispatch config from the create form (mirrors handleAgentCreate's
	// parse of the same field names): model + repo + repo_branch.
	note, err := s.ext.Notes.Create(ctx, notes.Note{
		Directory:  directory,
		Body:       body,
		Model:      strings.TrimSpace(r.FormValue("model")),
		Repo:       strings.TrimSpace(r.FormValue("repo")),
		RepoBranch: strings.TrimSpace(r.FormValue("repo_branch")),
	})
	if err != nil {
		s.logger.Printf("notes: create: %v", err)
		http.Error(w, "could not create note", http.StatusInternalServerError)
		return
	}

	if r.MultipartForm != nil {
		for _, fh := range r.MultipartForm.File["attachments"] {
			if fh.Size == 0 {
				continue
			}
			// No size check here: the pre-create guard above already refused the
			// whole request for anything over maxAttachmentBytes, so every fh that
			// reaches this loop is within the cap. The LimitReader below is the
			// backstop against a fh.Size that disagrees with the actual stream.
			f, err := fh.Open()
			if err != nil {
				s.logger.Printf("notes: open attachment %q: %v", fh.Filename, err)
				continue
			}
			data, err := io.ReadAll(io.LimitReader(f, maxAttachmentBytes))
			f.Close()
			if err != nil {
				s.logger.Printf("notes: read attachment %q: %v", fh.Filename, err)
				continue
			}
			ct := fh.Header.Get("Content-Type")
			if ct == "" {
				ct = "application/octet-stream"
			}
			if _, err := s.ext.Notes.AddAttachment(ctx, note.ID, notes.Attachment{
				Filename:    fh.Filename,
				ContentType: ct,
				SizeBytes:   int64(len(data)),
				Data:        data,
			}); err != nil {
				s.logger.Printf("notes: add attachment %q: %v", fh.Filename, err)
			}
		}
	}

	s.renderNotesPanel(w, r)
}

// handleAPITaskCreate handles POST /api/tasks (machine, hook-token-gated): create
// a durable Task (a Note in the notes domain) from a drafted task-spec. This lets
// the external task-spec-drafter route verified specs into the durable Tasks queue
// instead of the ephemeral 5-min-TTL permission inbox. It mirrors handleNoteCreate
// but speaks JSON (not multipart) and returns the new task id, then broadcasts
// task.changed so any open Tasks tab refreshes live (no browser swap drives this
// machine path).
func (s *Server) handleAPITaskCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Directory string `json:"directory"`
		// Title (migration 0018) is the task's display title. It USED to be silently
		// dropped (Go ignores unknown JSON fields), which is why every producer
		// smuggles the title through `directory` — the PR #156 footgun. Optional:
		// omitting it keeps the pre-0018 behaviour exactly (the card falls back to
		// `directory`), so no producer has to change.
		Title string `json:"title"`
		Body  string `json:"body"`
		// Optional dispatch config (backward-compatible: a body without these still
		// creates a config-less task). This is the contract repo-cos + the drafter
		// fill so a produced Task dispatches pre-filled.
		Model  string `json:"model"`
		Repo   string `json:"repo"`
		Branch string `json:"branch"`
		// ⚠ `privileges` WAS A FIELD HERE until the privilege subsystem was
		// removed. The decoder does not refuse unknown keys, so a producer that
		// still sends it gets its task created and the key ignored.
		//
		// Optional tags. Absent ⇒ '{}' (unchanged behaviour for every existing
		// producer). An invalid tag is a hard 400 naming the offending tag — a
		// routing tag must never be silently ignored.
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}
	source, sessionID := taskSource(r)
	note, err := s.createTask(r.Context(), taskCreateInput{
		Directory: body.Directory, Title: body.Title, Body: body.Body,
		Model: body.Model, Repo: body.Repo, Branch: body.Branch, Tags: body.Tags,
		Source: source, SessionID: sessionID,
	}, func(id int64) {
		// Task thread (migration 0023): the creating session joins its own task's
		// thread with the TERMINAL `created` role. This is the live twin of the 0023
		// backfill (which derives the same row from notes.source_session_id for every
		// pre-existing task), so a task created before and after this change carries
		// the identical first thread row. Cap/store failures cannot fail a create that
		// already succeeded — the task exists and the id has been handed back, so the
		// link is best-effort here (unlike the task-scoped routes, where the link is
		// the only reason the request was interesting).
		s.noteTaskSession(r, id, notes.RoleCreated)
	})
	var ve errTagValidation
	switch {
	case err == nil:
	case errors.Is(err, errTaskBodyRequired):
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "body required"})
		return
	case errors.As(err, &ve):
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": ve.Error()})
		return
	case errors.Is(err, errTaskTagsUnchecked):
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not validate tags"})
		return
	default:
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not create task"})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"id": note.ID})
}

// taskCreateInput is everything one machine-path task create carries. Source and
// SessionID are the DERIVED provenance (taskSource) — the caller derives them,
// because only an HTTP caller has the headers they come from.
type taskCreateInput struct {
	Directory, Title, Body string
	Model, Repo, Branch    string
	Tags                   []string
	Source, SessionID      string
}

// errTaskBodyRequired: the create carried no body (a 400).
var errTaskBodyRequired = errors.New("body required")

// errTaskTagsUnchecked: the tags could not be validated for a reason that is not
// the caller's (a 500); a caller's bad tag is errTagValidation instead.
var errTaskTagsUnchecked = errors.New("could not validate tags")

// createTask is THE machine-path task create: validate, resolve tags, store, and
// every side effect a created task has — the thread link (linked, called with the
// new id; nil for a caller with no session to link), the task-changed broadcast,
// the created-tasks metric, the routing-tag metric, the auto-dispatch log line,
// and the coalesced `created` push (notifyTaskCreated).
//
// 🔴 ONE FUNCTION SO A SECOND DOOR CANNOT FORGET A SIDE EFFECT. It was the body of
// handleAPITaskCreate, moved verbatim and in the same order; the plugin interface's
// gated-task tool (internal/plugins, a later PR) creates tasks through it, and the
// `created` push it ends with is that tool's "notify me". A door that re-spelled
// the create would silently skip the push or the metric.
//
// The FAB create (handleNoteCreate) deliberately does NOT come through here: it does
// not notify, because the user just made that task themselves.
func (s *Server) createTask(ctx context.Context, in taskCreateInput, linked func(id int64)) (notes.Note, error) {
	n, _, _, err := s.createTaskOnce(ctx, in, "", nil, linked)
	return n, err
}

// createTaskOnce is createTask with an optional once-key: with key set, the store
// files the task only if no open task already carries that tag
// (notes.Store.CreateOnceByTag), and created reports which happened; comment,
// when set (and key is), is recorded once on whichever task that is, in the same
// transaction, and commented reports whether it was written. A task that
// already existed gets NONE of the create's side effects — no thread link,
// broadcast, metric or push — because nothing was created.
func (s *Server) createTaskOnce(ctx context.Context, in taskCreateInput, key string, comment *notes.Comment, linked func(id int64)) (notes.Note, bool, bool, error) {
	if strings.TrimSpace(in.Body) == "" {
		return notes.Note{}, false, false, errTaskBodyRequired
	}
	tags, err := s.resolveTaskTags(ctx, in.Tags)
	if err != nil {
		var ve errTagValidation
		if errors.As(err, &ve) {
			return notes.Note{}, false, false, ve
		}
		s.logger.Printf("notes: api create tags: %v", err)
		return notes.Note{}, false, false, errTaskTagsUnchecked
	}
	// Provenance (migration 0017): taskSource is the single source of truth for BOTH
	// the metric label (below) and the stored value. The default/unidentified "api"
	// source is stored as NULL (pre-0017 behavior); only a named producer is
	// persisted. Session id is stored whenever present, independent of source.
	want := notes.Note{
		Directory:       in.Directory,
		Title:           capRunes(strings.TrimSpace(in.Title), maxTaskFieldLen),
		Body:            in.Body,
		Model:           in.Model,
		Repo:            in.Repo,
		RepoBranch:      in.Branch,
		Tags:            tags,
		SourceType:      storedSourceType(in.Source),
		SourceSessionID: nilIfEmpty(in.SessionID),
	}
	var note notes.Note
	created, commented := true, false
	if key == "" {
		note, err = s.ext.Notes.Create(ctx, want)
	} else {
		note, created, commented, err = s.ext.Notes.CreateOnceByTag(ctx, key, want, comment)
	}
	if err != nil {
		s.logger.Printf("notes: api create: %v", err)
		return notes.Note{}, false, false, err
	}
	if !created {
		return note, false, commented, nil
	}
	if linked != nil {
		linked(note.ID)
	}
	s.broadcast(ctx, EventTaskChanged, strconv.FormatInt(note.ID, 10))
	// Usage telemetry: count the created task by its derived producer source so
	// captures (esp. from the Brave extension) show up in the usage dashboard.
	// Bounded label — taskSource only emits allowlisted values. Note this uses the
	// full derived source (incl. "api"), so the metric label behavior is UNCHANGED
	// from pre-0017 even though "api" is stored as a NULL source_type.
	metrics.TasksCreated.WithLabelValues(in.Source).Inc()
	// Routing-tag telemetry (namespace only — never a tag value; see metrics.RoutingTags)
	// + the auto:dispatch plumbing log line.
	observeRoutingTags(note.Tags)
	s.logAutoDispatch(note.ID, note.Tags)
	// Coalesced background push: a producer (repo-cos / the drafter) posting a
	// burst of tasks buzzes the phone ONCE when the burst settles. Machine path
	// only — the FAB create (handleNoteCreate) intentionally does NOT notify, the
	// user just made that task themselves. Best-effort + nil-push-safe.
	s.notifyTaskCreated(note)
	return note, true, commented, nil
}

// --- machine task reads: the agent join + server-side filtering ---

// apiTask is the FULL machine-read shape of a task: every field notes.Note
// already emitted (the embedded struct promotes them, so the existing contract is
// byte-identical) plus the `agent` key.
//
// The agent link was the gap: agents.note_id lives one table over, taskCardViews
// has computed the join for the HTML cards since forever, and the JSON read never
// applied it — so a producer could file work and then had no way to see what
// happened to it.
//
// `agent` is emitted UNCONDITIONALLY and is `null` when the task has no linked
// agent. Not `omitempty`, and never a zero-valued object: an in-progress task with
// no agent at all is a real (and alarming) state, and `{"id":0,"status":""}` would
// read to a consumer as a genuine agent that is merely blank.
type apiTask struct {
	notes.Note
	Agent *agentJSON `json:"agent"`
}

// apiTaskSummary is the `?summary=1` shape: the routing/lifecycle fields a poller
// needs, with the three unbounded payloads REMOVED — the task body, the comment
// thread, and the attachment list — and the latter two replaced by counts.
//
// It is a separate struct rather than apiTask with the fields blanked, on purpose:
// notes.Note tags `body` without omitempty, so zeroing it would emit `"body": ""`,
// which reads as "this task has an empty body" rather than "this projection does
// not carry bodies". A key that is absent cannot be misread.
type apiTaskSummary struct {
	ID              int64      `json:"id"`
	Directory       string     `json:"directory"`
	Title           string     `json:"title"`
	Status          string     `json:"status"`
	Tags            []string   `json:"tags,omitempty"`
	Model           string     `json:"model"`
	Repo            string     `json:"repo"`
	RepoBranch      string     `json:"repoBranch"`
	SourceType      *string    `json:"sourceType,omitempty"`
	SourceSessionID *string    `json:"sourceSessionId,omitempty"`
	CommentCount    int        `json:"commentCount"`
	AttachmentCount int        `json:"attachmentCount"`
	CreatedAt       time.Time  `json:"createdAt"`
	UpdatedAt       time.Time  `json:"updatedAt"`
	Agent           *agentJSON `json:"agent"`
}

// taskListQuery is the parsed + VALIDATED GET /api/tasks filter.
type taskListQuery struct {
	tags     []string // repeated ?tag=, AND semantics (existing behaviour)
	statuses []string // repeated ?status=, OR within, AND with tags; empty = no filter
	limit    int      // 0 = unlimited
	summary  bool     // ?summary=1
}

// validTaskStatuses is the closed status vocabulary, in the order the 400 message
// lists it. It now comes straight from taskstatus.All() rather than re-listing
// the four constants: a hand-written list is a second enumeration that a newly
// added status silently omits, which is exactly the drift that bit
// cmd/muster. TestAPITaskListRejectsEveryNonStatus still pins the
// agreement.
var validTaskStatuses = taskstatus.All()

// parseTaskListQuery reads and validates the GET /api/tasks query string.
//
// 🔴 EVERY malformed value is a 400, never a silent fallback. Silently ignoring an
// unrecognised filter IS the bug this endpoint had: `?status=open` and `?limit=1`
// were both dropped on the floor and the caller got all 18 tasks / 195 KB back
// while believing it had asked for one. A filter that "fails open" to the full
// table is worse than no filter, because the caller cannot tell.
//
// 400-vs-empty, decided deliberately and differently for tags and statuses:
//
//   - An unknown TAG yields an empty 200. The tag vocabulary is OPEN (any string
//     is a legal tag), so "no task carries this label" is an ordinary answer, and
//     that is the documented pre-existing behaviour of `?tag=`.
//   - An unknown STATUS is a 400. The status vocabulary is CLOSED and enumerable
//     (notes.ValidStatus), so `?status=opne` cannot be a legitimate question — it
//     is a caller bug, and answering `[]` would hide the typo while answering
//     everything would restore the original bug in a new costume.
//   - A VALID status that no task currently has still yields an empty 200:
//     matching nothing is not an error.
//
// `?limit=` must parse as an integer ≥ 1. `limit=0` is a 400 rather than
// "unlimited" for the same reason — a zero that silently means "everything" is the
// original bug again. Absent means unlimited (unchanged behaviour).
//
// `?summary=` is parsed with strconv.ParseBool, so `1/true/0/false` work and
// anything else is a 400 (a typo'd `?summary=yes` must not silently ship 195 KB).
func parseTaskListQuery(q url.Values) (taskListQuery, error) {
	out := taskListQuery{tags: queryTags(q)}

	seen := map[string]bool{}
	for _, raw := range q["status"] {
		st := strings.TrimSpace(raw)
		if !notes.ValidStatus(st) {
			return taskListQuery{}, fmt.Errorf("invalid status %q: must be one of %s",
				raw, strings.Join(validTaskStatuses, ", "))
		}
		if !seen[st] {
			seen[st] = true
			out.statuses = append(out.statuses, st)
		}
	}

	if raw, ok := q["limit"]; ok && len(raw) > 0 {
		n, err := strconv.Atoi(strings.TrimSpace(raw[0]))
		if err != nil || n < 1 {
			return taskListQuery{}, fmt.Errorf("invalid limit %q: must be a positive integer", raw[0])
		}
		out.limit = n
	}

	if raw, ok := q["summary"]; ok && len(raw) > 0 {
		v, err := strconv.ParseBool(strings.TrimSpace(raw[0]))
		if err != nil {
			return taskListQuery{}, fmt.Errorf("invalid summary %q: must be 1/0 or true/false", raw[0])
		}
		out.summary = v
	}

	return out, nil
}

// filterByStatus keeps the tasks whose status is in statuses (OR within the
// param). An empty statuses slice is "no status filter" and returns list
// unchanged. Order is preserved — the store already returns its rows in the
// board's newest-ACTIVITY-first order (notes.taskActivityExpr), so this filter
// never has to re-sort. (It read "newest-updated-first" until the ordering moved;
// the property this function relies on — that the store, not this loop, decides
// the order — is unchanged.)
func filterByStatus(list []notes.Note, statuses []string) []notes.Note {
	if len(statuses) == 0 {
		return list
	}
	want := make(map[string]bool, len(statuses))
	for _, st := range statuses {
		want[st] = true
	}
	out := make([]notes.Note, 0, len(list))
	for _, n := range list {
		if want[n.Status] {
			out = append(out, n)
		}
	}
	return out
}

// applyLimit truncates list to at most n entries (n ≤ 0 = unlimited).
//
// 🔴 THE CALL ORDER IS THE CONTRACT: limit is applied AFTER every filter, so
// `?status=open&limit=5` means "the 5 most-recently-ACTIVE OPEN tasks" (the
// store's order — notes.taskActivityExpr; this used to say "updated"). Limiting
// first would mean "of the 5 newest tasks, whichever happen to be open" — which
// silently returns fewer rows than asked for, or none, and looks identical on a
// fixture where every task shares a status. Pinned by
// TestAPITaskListAppliesLimitAfterStatusFilter.
func applyLimit(list []notes.Note, n int) []notes.Note {
	if n <= 0 || len(list) <= n {
		return list
	}
	return list[:n]
}

// apiAgentsByNote resolves each task's LATEST linked agent, projected for JSON
// with its liveness timestamp, in two batched reads (AgentsByNoteIDs +
// LastMessageByAgentIDs) — the SAME join taskCardViews uses for the HTML cards, so
// the machine read and the card can never disagree about which agent owns a task.
//
// Best-effort, exactly like taskCardViews: a lookup failure degrades to
// agent-less tasks (`"agent": null`) rather than failing the whole read.
func (s *Server) apiAgentsByNote(ctx context.Context, list []notes.Note) map[int64]agentJSON {
	out := map[int64]agentJSON{}
	if s.ext.Agents == nil || len(list) == 0 {
		return out
	}
	ids := make([]int64, 0, len(list))
	for _, n := range list {
		ids = append(ids, n.ID)
	}
	byNote, err := s.ext.Agents.AgentsByNoteIDs(ctx, ids)
	if err != nil {
		s.logger.Printf("notes: api agents by note ids: %v", err)
		return out
	}
	flat := make([]agents.Agent, 0, len(byNote))
	for _, a := range byNote {
		flat = append(flat, a)
	}
	lastMsg := s.lastMessageByAgents(ctx, flat)
	for noteID, a := range byNote {
		out[noteID] = newAgentJSON(a, lastMsg[a.ID])
	}
	return out
}

// apiTaskOne projects ONE task into the full wire shape with its linked agent.
// It is the single writer of the single-task machine response, shared by
// GET /api/tasks/{id}, PATCH /api/tasks/{id} and PATCH /api/tasks/{id}/status —
// those two PATCH handlers document their response as "the same shape as
// GET /api/tasks/{id}", and routing all three through one function is what keeps
// that claim true rather than merely intended. Pinned by
// TestSingleTaskResponsesAllCarryTheAgent.
func (s *Server) apiTaskOne(ctx context.Context, note notes.Note) apiTask {
	out := apiTask{Note: note}
	if a, ok := s.apiAgentsByNote(ctx, []notes.Note{note})[note.ID]; ok {
		out.Agent = &a
	}
	return out
}

// apiTasks projects the (already filtered + limited) task list into the wire
// shape, attaching each task's linked agent.
func (s *Server) apiTasks(ctx context.Context, list []notes.Note, summary bool) any {
	byNote := s.apiAgentsByNote(ctx, list)
	agentFor := func(id int64) *agentJSON {
		if a, ok := byNote[id]; ok {
			v := a
			return &v
		}
		return nil
	}
	if summary {
		out := make([]apiTaskSummary, 0, len(list))
		for _, n := range list {
			out = append(out, apiTaskSummary{
				ID: n.ID, Directory: n.Directory, Title: n.Title, Status: n.Status,
				Tags: n.Tags, Model: n.Model, Repo: n.Repo, RepoBranch: n.RepoBranch,
				SourceType: n.SourceType, SourceSessionID: n.SourceSessionID,
				CommentCount: len(n.Comments), AttachmentCount: len(n.Attachments),
				CreatedAt: n.CreatedAt, UpdatedAt: n.UpdatedAt,
				Agent: agentFor(n.ID),
			})
		}
		return out
	}
	out := make([]apiTask, 0, len(list))
	for _, n := range list {
		out = append(out, apiTask{Note: n, Agent: agentFor(n.ID)})
	}
	return out
}

// handleAPITaskList handles GET /api/tasks (machine, hook-token-gated): return
// durable Tasks as JSON. It mirrors the operator read (handleOpTasksList) but is
// reachable with the shared hook token, so a machine producer that POSTed a Task
// (e.g. repo-cos) can read back its status without a browser session.
//
// Each task now carries its linked `agent` (null when none) — see apiTask.
//
// Filters, all applied SERVER-side (they were previously parsed for `tag` only and
// `status`/`limit` were silently discarded, so every caller pulled the whole table):
//
//	?tag=a&tag=b        AND — tasks carrying BOTH. Unknown tag ⇒ empty 200.
//	?status=x&status=y  OR within the param, AND with the tag filter.
//	                    Unknown value ⇒ 400 (closed vocabulary — see
//	                    parseTaskListQuery); valid-but-unmatched ⇒ empty 200.
//	?limit=n            n ≥ 1, applied AFTER the filters, over the store's
//	                    newest-ACTIVITY-first order ⇒ "the n most recently active
//	                    matches". n < 1 or non-numeric ⇒ 400.
//	?summary=1          drop the task body, the comment thread and the attachment
//	                    list; emit commentCount/attachmentCount instead.
//
// 🔴 THE ORDER THIS PAGES OVER CHANGED, and the line above used to say
// "newest-updated-first". Store.List now orders by the most recent of the note's
// own updated_at and its comment / agent / session-link activity
// (notes.taskActivityExpr) — so `?limit=n` still means "the n most recent
// matches", but "recent" now counts an agent working the task or a session
// picking it up, neither of which touches notes.updated_at. For most producers
// that is strictly better; for one comparing successive polls it is a behaviour
// change worth knowing about, which is why it is stated rather than left to be
// discovered.
//
// ⚠ THIS SURFACE'S ?status= AND ?limit= ARE **NOT** THE ONES /ui/tasks USES, and
// the duplication is deliberate rather than an oversight:
//
//	                 /api/tasks (machine)              /ui/tasks (board)
//	?status=         RAW statuses, repeatable          ONE status (taskstatus.All)
//	unknown status   400 — a closed vocabulary a       ignored, board shows all —
//	                 producer must not typo past       a filter is a read and must
//	                                                   not be able to hide work
//	?limit=          applied in Go, after the read     applied as SQL LIMIT
//	bad limit        400                               the default page
//
// A machine producer wants a loud refusal; a person tapping a chip wants the
// board back. Collapsing them would have to sacrifice one of those. If they are
// ever unified, the 400s here are the part producers depend on.
func (s *Server) handleAPITaskList(w http.ResponseWriter, r *http.Request) {
	q, err := parseTaskListQuery(r.URL.Query())
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	list, err := s.listTasksFiltered(r.Context(), q.tags)
	if err != nil {
		s.logger.Printf("notes: api list: %v", err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not list tasks"})
		return
	}
	list = applyLimit(filterByStatus(list, q.statuses), q.limit)
	s.writeJSON(w, http.StatusOK, s.apiTasks(r.Context(), list, q.summary))
}

// handleAPITaskGet handles GET /api/tasks/{id} (machine, hook-token-gated):
// return a single durable Task (with comments/attachments) as JSON, or 404 if it
// no longer exists. Lets a producer poll one task's lifecycle status by id — and,
// since this change, see WHICH agent is working it and when that agent was last
// alive (the `agent` key; null when the task has no linked agent).
func (s *Server) handleAPITaskGet(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad id"})
		return
	}
	// Task thread (migration 0023): a session reading a task joins its thread with
	// the lowest role. Linking BEFORE the read is what makes the response's
	// `sessions` array include this very touch — the alternative (read, link,
	// re-read) costs a second full fan-out to answer the same question. A dismissed
	// or unknown id makes the link a silent no-op and the Get below answers the 404.
	changed := s.noteTaskSession(r, id, notes.RoleRead)
	note, err := s.ext.Notes.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "task not found"})
			return
		}
		s.logger.Printf("notes: api get %d: %v", id, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not load task"})
		return
	}
	s.broadcastTaskLink(r.Context(), changed, id)
	s.writeJSON(w, http.StatusOK, s.apiTaskOne(r.Context(), note))
}

// maxTaskFieldLen defensively caps an edited single-line field (directory / model
// slug / repo / branch) and maxTaskBodyLen the body markdown, so an edit can't
// bloat a row with unbounded attacker-influenced text. Rune-safe caps keep the
// stored value valid UTF-8 (Postgres rejects invalid text on write).
const (
	maxTaskFieldLen = 512
	maxTaskBodyLen  = 200_000
)

// capRunes truncates s to at most n runes (never splitting a multibyte rune).
func capRunes(s string, n int) string {
	if rs := []rune(s); len(rs) > n {
		return string(rs[:n])
	}
	return s
}

// handleAPITaskEdit handles PATCH /api/tasks/{id} (machine, hook-token-gated):
// partially edit a durable Task's editable content + dispatch config. It mirrors
// handleAPITaskCreate's JSON field names (directory/body/model/repo/branch),
// but ALL are optional pointers so an absent key leaves that column
// unchanged (vs a present key, which sets it). status / provenance / created_at in
// the body are IGNORED (never mapped to the update). An in-progress task is
// immutable → 409 (checked BEFORE any mutation). Unknown id → 404. On success it
// returns the updated task JSON (same shape as GET /api/tasks/{id}).
func (s *Server) handleAPITaskEdit(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad id"})
		return
	}
	// Pointer fields: a nil pointer = "key absent" (leave unchanged). Only these
	// keys are decoded — a status/sourceType/createdAt key in the body is silently
	// ignored (no DisallowUnknownFields, matching handleAPITaskCreate), so it can
	// never change an immutable field.
	var body struct {
		Directory *string `json:"directory"`
		// Title (migration 0018): the display title, previously silently dropped.
		Title  *string `json:"title"`
		Body   *string `json:"body"`
		Model  *string `json:"model"`
		Repo   *string `json:"repo"`
		Branch *string `json:"branch"`
		// Tag edits. `tags` REPLACES the whole set; `addTags`/`removeTags` MERGE
		// (set-union / set-difference) so two author classes can't lose each other's
		// updates. Supplying `tags` together with either merge key is AMBIGUOUS →
		// 400 (refuse rather than guess).
		Tags       *[]string `json:"tags"`
		AddTags    []string  `json:"addTags"`
		RemoveTags []string  `json:"removeTags"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}
	if body.Tags != nil && (len(body.AddTags) > 0 || len(body.RemoveTags) > 0) {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "ambiguous tag edit: send either \"tags\" (replace) or \"addTags\"/\"removeTags\" (merge), not both",
		})
		return
	}
	// Load the current task to enforce the in-progress guard BEFORE mutating.
	cur, err := s.ext.Notes.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "task not found"})
			return
		}
		s.logger.Printf("notes: api edit get %d: %v", id, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not load task"})
		return
	}
	// In-progress rule, REFINED (spec §6): a DESCRIPTIVE-tag-only edit is allowed
	// while in progress (a label is not a spec change); any routing-tag change, or
	// any other field, still 409s. tagOnlyEdit reports whether this PATCH touches
	// nothing but tags.
	tagOnly := body.Directory == nil && body.Title == nil && body.Body == nil && body.Model == nil &&
		body.Repo == nil && body.Branch == nil
	if cur.Status == notes.StatusInProgress {
		touched := append(append([]string{}, body.AddTags...), body.RemoveTags...)
		if body.Tags != nil {
			touched = append(touched, *body.Tags...)
			// A replace also (potentially) DROPS the task's current routing tags, so
			// the existing set counts as touched too.
			touched = append(touched, cur.Tags...)
		}
		if !tagOnly || len(notes.RoutingTags(notes.NormalizeTags(touched))) > 0 {
			s.writeJSON(w, http.StatusConflict, map[string]any{"error": "task is in progress and cannot be edited"})
			return
		}
	}

	// 🔴 THE `worked` LINK IS NOT WRITTEN HERE. It lives below the LAST store write in
	// this handler — past UpdateNote AND past the RemoveTags/AddTags set-operations —
	// where its full reasoning is. Every exit between this point and there refuses the
	// request, and a refused write must record NO link at all. Do not "simplify" a
	// link back up to here.
	//
	// ⚠ This comment deliberately does NOT enumerate those exits. The list it used to
	// carry ("empty body, invalid tags, the project merge guard") was the set that was
	// easy to see, and it read as if the hazard were fully closed while the tag
	// set-operation failures below were still open. A description wider than its
	// implementation stops the next person looking, which is worse than no comment.

	// Map only the provided keys → the partial NoteUpdate.
	var patch notes.NoteUpdate
	if body.Directory != nil {
		v := capRunes(*body.Directory, maxTaskFieldLen)
		patch.Directory = &v
	}
	if body.Title != nil {
		v := capRunes(strings.TrimSpace(*body.Title), maxTaskFieldLen)
		patch.Title = &v
	}
	if body.Tags != nil {
		v, terr := s.resolveTaskTags(r.Context(), *body.Tags)
		if terr != nil {
			var ve errTagValidation
			if errors.As(terr, &ve) {
				s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": ve.Error()})
				return
			}
			s.logger.Printf("notes: api edit tags %d: %v", id, terr)
			s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not validate tags"})
			return
		}
		patch.Tags = &v
	}
	if body.Body != nil {
		v := strings.TrimSpace(*body.Body)
		if v == "" {
			s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "body cannot be empty"})
			return
		}
		v = capRunes(v, maxTaskBodyLen)
		patch.Body = &v
	}
	if body.Model != nil {
		v := capRunes(*body.Model, maxTaskFieldLen)
		patch.Model = &v
	}
	if body.Repo != nil {
		v := capRunes(*body.Repo, maxTaskFieldLen)
		patch.Repo = &v
	}
	if body.Branch != nil {
		v := capRunes(*body.Branch, maxTaskFieldLen)
		patch.RepoBranch = &v
	}

	// Validate the MERGE keys before any mutation, so an invalid addTags never
	// half-applies alongside a valid field edit.
	addTags, err := s.resolveTaskTags(r.Context(), body.AddTags)
	if err == nil {
		// removeTags only needs the grammar: a tag must always be removable,
		// whatever it names.
		var rerr error
		if _, rerr = notes.NormalizeAndValidate(body.RemoveTags); rerr != nil {
			err = errTagValidation{rerr}
		}
	}
	if err != nil {
		var ve errTagValidation
		if errors.As(err, &ve) {
			s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": ve.Error()})
			return
		}
		s.logger.Printf("notes: api edit merge tags %d: %v", id, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not validate tags"})
		return
	}
	removeTags := notes.NormalizeTags(body.RemoveTags)

	// `addTags` MERGES, so the at-most-one-project rule must be checked against the
	// merged result — resolveTaskTags above only saw the incoming set. Checked here,
	// BEFORE any mutation, for the same reason the tag validation is: a rejected
	// project must never half-apply alongside a valid field edit. `removeTags` is
	// passed so a same-request project reassignment stays legal.
	if code, msg, ok := s.projectMergeGuard(r.Context(), id, addTags, removeTags); !ok {
		s.writeJSON(w, code, map[string]any{"error": msg})
		return
	}

	updated, err := s.ext.Notes.UpdateNote(r.Context(), id, patch)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "task not found"})
			return
		}
		s.logger.Printf("notes: api edit %d: %v", id, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not update task"})
		return
	}

	// 🔴 STILL NOT HERE. UpdateNote has landed, but RemoveTags and AddTags below can
	// each answer 500 — and for a TAGS-ONLY patch the NoteUpdate above is EMPTY, so
	// UpdateNote was a bare updated_at bump and NOTHING the caller asked for has been
	// applied yet. Linking at this point recorded `worked` on a request that answered
	// `500 could not add tags`. See the link's real home below the tag block.

	// Merge semantics, applied as SINGLE-statement set operations so a concurrent
	// producer's tag can never be lost between our read and our write.
	//
	// 🔴 REMOVE BEFORE ADD — the order is the safety property, not a style choice.
	// These are two statements, not one transaction. A legal one-request project
	// REASSIGNMENT (`removeTags:["project:a"] + addTags:["project:b"]`) therefore
	// has an observable intermediate state, and if the SECOND statement fails the
	// row KEEPS whatever the first one left. Adding first meant the intermediate
	// state carried BOTH project tags, so a failure persisted a row violating the
	// at-most-one-project invariant the whole feature rests on — while returning
	// 500 — and notes.ProjectName would then resolve that row ambiguously (first
	// in sort order). Removing first makes the intermediate state carry ZERO or
	// ONE project tag, so no failure can leave two. Pinned by
	// TestPatchProjectReassignmentNeverLeavesTwoProjectTags.
	//
	// Consequence, stated rather than hidden: `addTags` and `removeTags` naming the
	// SAME tag in one request now resolves to ADDED (it was REMOVED). That is the
	// more defensible reading — "remove these, then add these" — and it is pinned
	// by TestPatchAddAndRemoveSameTagResolvesToAdded.
	//
	// RESIDUAL, accepted deliberately: this is still not atomic, and a concurrent
	// AddTags from another producer between the two statements could still land a
	// second project tag (the projectMergeGuard read is likewise not serialized
	// with the write). Closing that needs a real transaction — a store-interface
	// change touching both the Postgres and in-memory implementations — which is
	// out of proportion for a single-user tool with one writer in practice. The
	// narrowed window is: a failure can no longer create the bad state on its own.
	if len(removeTags) > 0 {
		if updated, err = s.ext.Notes.RemoveTags(r.Context(), id, removeTags); err != nil {
			s.logger.Printf("notes: api edit removeTags %d: %v", id, err)
			s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not remove tags"})
			return
		}
	}
	if len(addTags) > 0 {
		if updated, err = s.ext.Notes.AddTags(r.Context(), id, addTags); err != nil {
			s.logger.Printf("notes: api edit addTags %d: %v", id, err)
			s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not add tags"})
			return
		}
	}
	if patch.Tags != nil || len(addTags) > 0 {
		observeRoutingTags(updated.Tags)
		s.logAutoDispatch(updated.ID, updated.Tags)
	}

	// Task thread (migration 0023): the session joins with the `worked` role — HERE,
	// below EVERY store write this handler can make, so the role is a statement about
	// what the server DID rather than what a caller asked for.
	//
	// 🔴 A REJECTED EDIT MUST RECORD NO LINK AT ALL — and "rejected" means EVERY exit
	// above this line, not the ones that were easy to see. The link originally sat
	// just after the in-progress guard, which closed the 409 and nothing else: an
	// empty body, an invalid tag, projectMergeGuard and an UpdateNote failure all
	// still recorded `worked`, permanently, since the role never downgrades. Moving
	// it below UpdateNote closed those four and left a fifth: RemoveTags/AddTags each
	// answer 500, and on a TAGS-ONLY patch UpdateNote is a bare updated_at bump, so
	// `500 could not add tags` still recorded `worked` for a request in which nothing
	// the caller asked for had landed. Both misplacements are pinned by
	// TestRejectedEditDoesNotRecordWorked; the second was found only because a mutant
	// that moved the link back up SURVIVED the whole suite.
	//
	// 🔴 AND A REFUSED WRITE MUST NOT FALL BACK TO `read` EITHER. Recording the
	// weaker role on a request the server refused manufactures thread membership out
	// of an attempt — the same over-report in a quieter form. No link is the answer.
	//
	// 🔴 THE GENERAL RULE, so the next writer does not have to rediscover it: this
	// link belongs below the LAST fallible WRITE, and any new fallible write added
	// to this handler must go ABOVE it. Adding one below silently re-opens the
	// defect.
	//
	// Deliberately "write", not "store call": the conditional re-read below is
	// itself a store call sitting under this link, and it is correct there — it
	// cannot refuse the request, so it cannot make `worked` a claim about an
	// attempt. Saying "store call" would have made this rule wider than what it
	// protects, in the same commit that added the paragraph above to kill exactly
	// that class of comment.
	linkChanged := s.noteTaskSession(r, id, notes.RoleWorked)

	// Re-read ONLY when the link actually changed something, so the response's
	// `sessions` array carries the link this very request created.
	//
	// It is load-bearing on EVERY success path now: UpdateNote, AddTags and RemoveTags
	// all end in the store's own Get, and all three now run BEFORE the link, so
	// `updated` can never carry it. (While the link sat above the tag block this was
	// true only of the bare-body shape — AddTags/RemoveTags re-read after the link and
	// already included it — so the merge shape could not witness a broken re-read.
	// Moving the link fixed that too: both shapes are now sensitive.)
	//
	// Conditional because an already-linked session's repeat touch leaves the thread
	// byte-identical, so a second fan-out there is pure overhead on the common path
	// (a producer PATCHes the same task many times; it joins the thread once).
	if linkChanged {
		if fresh, ferr := s.ext.Notes.Get(r.Context(), id); ferr == nil {
			updated = fresh
		} else {
			// Non-fatal: the edit landed. The response is one touch stale, which is
			// strictly better than failing a request that succeeded.
			s.logger.Printf("notes: api edit refresh %d: %v", id, ferr)
		}
	}
	// Broadcast so any open Tasks tab re-fetches #tasks-list and shows the edit.
	s.broadcast(r.Context(), EventTaskChanged, strconv.FormatInt(updated.ID, 10))
	s.writeJSON(w, http.StatusOK, s.apiTaskOne(r.Context(), updated))
}

// handleNoteDelete handles DELETE /notes/{id}: dismiss a task off the board and
// return the empty card-removal fragment for htmx.
//
// The dismissal is SOFT (migration 0019) — the row, its comment thread and its
// attachments all survive; only their visibility goes. Dismissing ALSO tears down
// the task's dispatched agent (helm uninstall + ns) so a dismiss cleans up after
// itself instead of orphaning a running pod; that teardown is real and
// irreversible, and is deliberately unchanged. Best-effort — it runs in the
// background and can never block/fail the dismissal.
func (s *Server) handleNoteDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := s.dismissTask(r.Context(), id); err != nil {
		s.logger.Printf("notes: delete %d: %v", id, err)
		http.Error(w, "could not delete note", http.StatusInternalServerError)
		return
	}
	// 🔴 THE DETAIL PAGE MUST LEAVE, NOT EMPTY ITSELF. On the board the empty 200
	// is exactly right: htmx removes #task-<id> and the rest of the list stays. On
	// /tasks/{id} that same card IS the document's only content, so the swap left
	// a header and a back link floating over an empty <main> — on a URL that 404s
	// the moment you reload it. HX-Redirect sends the browser to the board, which
	// is where a just-dismissed task's reader belongs.
	if rendersInDetailShape(r, id) {
		w.Header().Set("HX-Redirect", "/tasks")
	}
	// htmx removes the targeted #task-<id> element on an empty 200.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
}

// dismissTask SOFT-deletes a task and tears down its linked agent pod (if any) in
// the background — so dismissing a task cleans up its pod instead of orphaning it.
// Shared by the human dismiss (handleNoteDelete) and the machine DELETE
// (handleAPITaskDelete); the idle-task reaper (reapIdleTasks) is NOT a caller —
// it flags idle tasks and dismisses nothing. Returns the
// store error; the teardown is best-effort/async and never blocks or fails it.
//
// 🔴 SOFT, since migration 0019. It used to call Notes.Delete, a real row delete,
// and note_comments CASCADEs on notes(id) — so dismissing a task DESTROYED its
// comment thread, which is where the investigation actually lives, unrecoverably
// (the daily backup window means a same-day task is in no snapshot). SoftDelete
// stamps deleted_at instead: the task vanishes from every read and write path just
// as before, but the thread survives and notes.Store.Restore brings the whole task
// back. Do not "simplify" this back to Delete.
//
// The agent teardown is UNCHANGED and intentional — a dismiss must not orphan a
// running pod. What changed is why the link is read first: agents.note_id is
// ON DELETE SET NULL, so under the old hard delete reading it after the delete was
// impossible. Under a soft delete the FK is never nulled, so the link now SURVIVES
// the dismiss (and points at a task every read answers ErrNoRows for — every
// consumer of that link already degrades to "no task", which is what a nulled link
// produced). Reading it first is kept regardless: it is the order that makes the
// two deletion modes behave identically, and it is what a future purge needs.
//
// 🔴 THE CHIEF IS NEVER TORN DOWN BY A DISMISS. The TASK is still dismissed —
// the operator asked for that and it is recoverable — but a linked agent that
// isChiefAmong answers for is left running, and the skip is logged. Refusing the
// whole dismiss instead would make a task un-dismissable for as long as the
// chief stays linked to it, and both the human dismiss and the machine DELETE
// come through here. A failed chief check also leaves the agent
// alone (agentIsChief: fail closed); the dismiss itself is unaffected.
func (s *Server) dismissTask(ctx context.Context, id int64) error {
	// Resolve the task's linked agent (latest) before dismissing it.
	var linkedAgentID int64
	if s.ext.Agents != nil {
		if m, err := s.ext.Agents.AgentsByNoteIDs(ctx, []int64{id}); err == nil {
			if a, ok := m[id]; ok {
				linkedAgentID = a.ID
			}
		}
	}
	if linkedAgentID != 0 {
		_, chief, err := s.agentIsChief(ctx, linkedAgentID)
		switch {
		case err != nil:
			s.logger.Printf("dismiss task %d: chief check for linked agent %d: %v; leaving the agent running", id, linkedAgentID, err)
			linkedAgentID = 0
		case chief:
			s.logger.Printf("dismiss task %d: linked agent %d is the chief; dismissing the task and leaving the agent running", id, linkedAgentID)
			linkedAgentID = 0
		}
	}
	if err := s.ext.Notes.SoftDelete(ctx, id); err != nil {
		return err
	}
	if linkedAgentID != 0 && s.ext.Provisioner != nil {
		opCtx := context.WithoutCancel(ctx) // keeps the principal; see Provisioner
		safeGo(s.logger, "dismiss-destroy", func() {
			if err := s.ext.Provisioner.Destroy(opCtx, linkedAgentID); err != nil {
				s.logger.Printf("dismiss task %d: destroy linked agent %d: %v", id, linkedAgentID, err)
			}
		})
	}
	// Broadcast task.changed so any open Tasks tab re-fetches #tasks-list and
	// re-sorts the flat activity-ordered list (an empty-200 card removal
	// alone leaves the section headers stale). Shared by the human dismiss and
	// the machine DELETE, so both paths regroup consistently.
	s.broadcast(ctx, EventTaskChanged, strconv.FormatInt(id, 10))
	return nil
}

// handleAPITaskDelete handles DELETE /api/tasks/{id} (machine, hook-token-gated):
// delete a durable Task so a machine producer can clean up after itself. It closes
// the last producer-parity gap — create/read/edit/set-status were all reachable
// with the hook token, but deletion was session-only (DELETE /tasks/{id}).
//
// 🔴 BLAST RADIUS: this reuses the SAME dismissTask helper as the human dismiss and
// the idle-task reaper, so deleting a task with a LIVE dispatched agent TEARS DOWN
// THAT AGENT POD (helm uninstall + namespace delete, best-effort/background). That
// is intentional — machine and UI deletion must not diverge — but a producer author
// must not discover it by accident. Delete a dispatched task only if you mean to
// kill its agent.
//
// There is deliberately NO in-progress guard (unlike PATCH /api/tasks/{id}, which
// 409s): dismissing an in-progress task is a supported, deliberate action in the UI
// — it IS the agent-teardown feature — and consistency with the UI wins.
//
// Since migration 0019 the deletion is SOFT: the task's comment thread and
// attachments survive it and only notes.Store.Restore (DB/store-level; no route)
// brings it back. The response shape is unchanged — `{"id":N,"deleted":true}` —
// because from a producer's side nothing observable changed: the task is gone from
// GET /api/tasks and answers 404 by id, exactly as before.
//
// 🔴 STATUS CODE, PINNED: a DELETE of an ALREADY-dismissed task is **404**, not
// 200. That is deliberately identical to what a repeat DELETE answered under the
// hard delete (the row was gone, so the probe 404'd), so no producer's retry
// keying changes. The existence probe below is what enforces it and stays
// load-bearing for the same reason it always was — the store's dismiss succeeds
// with 0 rows affected for an id it cannot see, so without the Get first an
// unknown OR already-dismissed id would answer 200. (SoftDelete additionally
// answers ErrNoRows in that case, so the contract holds even against a racing
// concurrent dismiss — but it surfaces as a 500 there, not a 404; the probe is
// what makes the ordinary repeat a 404.) Bad id → 400; unknown or
// already-dismissed id → 404; store failure → 500; no/bad hook token → 401
// (middleware). Always JSON — never the empty htmx card fragment the session route
// returns.
func (s *Server) handleAPITaskDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad id"})
		return
	}
	if _, err := s.ext.Notes.Get(r.Context(), id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "task not found"})
			return
		}
		s.logger.Printf("notes: api delete get %d: %v", id, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not load task"})
		return
	}
	// dismissTask owns the whole dismiss contract: resolve + tear down the linked
	// agent (background, best-effort), delete the row, broadcast task.changed.
	if err := s.dismissTask(r.Context(), id); err != nil {
		s.logger.Printf("notes: api delete %d: %v", id, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not delete task"})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"id": id, "deleted": true})
}

// errInvalidStatus is the sentinel applyTaskStatus returns when the requested
// status is not a known task status, so each caller can map it to its own
// content-type-appropriate 400 (an HTML http.Error for the session path, a JSON
// body for the machine path).
var errInvalidStatus = errors.New("invalid task status")

// applyTaskStatus is the shared core behind BOTH task status-set paths — the
// session/human PATCH /tasks/{id}/status (handleNoteStatus) and the machine
// hook-token PATCH /api/tasks/{id}/status (handleAPITaskStatus) — so the two can
// never diverge on validation or side-effects. It validates the status, applies
// it via the store, and fires the shared side-effect (the "ready for review"
// push, deduped + nil-push-safe). It returns:
//   - errInvalidStatus for an unknown/absent status (caller → 400),
//   - pgx.ErrNoRows for an unknown id (caller → 404),
//   - any other store error (caller → 500),
//
// and otherwise the refreshed note.
//
// 🔴 CALLERS OWN THEIR RESPONSE RENDERING; THEY DO **NOT** OWN THE BROADCAST —
// this function fires task.changed for both of them. That split is the whole
// point: this docstring used to read "Callers own their own RESPONSE rendering
// AND their SSE broadcast: the session path re-renders the card fragment … (so
// no explicit broadcast — behavior unchanged); the machine path … broadcasts
// task.changed itself", and THREE of its clauses were false by the end of the
// same commit that fixed the bug it describes: "AND their SSE broadcast",
// "behavior unchanged", and "the machine path … broadcasts task.changed itself".
// The rest is still true and is restated above — callers DO own response
// rendering (handleNoteStatus renders the card, handleAPITaskStatus writes JSON,
// and they must, because the two need different content types; that is what the
// `writer` argument below is for). An earlier draft of this paragraph said
// "every clause of it was false", which contradicted its own opening line four
// lines up and would send a reader the other way — into moving renderNoteCard
// into this function, where it cannot work
// (upstream task 487 item 1, caught by the
// audit of PR #681 — the two comments in agents.go and
// dispatch_advance_broadcast_test.go were corrected while the docstring of the
// function actually being changed was not).
//
// Read literally, the old text tells a maintainer adding a third status route to
// re-add s.broadcast to their handler. Do NOT: handleNoteStatus and
// handleAPITaskStatus both reach task.changed through here, so a handler-level
// broadcast double-fires and every open tab does two full #tasks-list re-fetches
// per flip. Nothing would go red — human_status_broadcast_test.go asserts that AN
// event arrives, not that exactly one does.
//
// 🔴 It takes a `writer` because it is SHARED by two different callers, so it
// cannot name one itself — a hardcoded label here would attribute every machine
// write to the human UI or vice versa, which is worse than UNLABELLED because it
// is confidently wrong. Its two callers are pinned by their own ledger in
// task_status_ledger_test.go.
func (s *Server) applyTaskStatus(ctx context.Context, writer statusWriter, id int64, status string) (notes.Note, error) {
	if !notes.ValidStatus(status) {
		return notes.Note{}, errInvalidStatus
	}
	note, err := s.setNoteStatus(ctx, writer, id, status)
	if err != nil {
		return notes.Note{}, err
	}
	// Background push when this task ENTERS ready_for_review (deduped so a re-save
	// doesn't re-buzz). Best-effort + nil-push-safe.
	s.notifyTaskDone(note)
	// ⚠ NOTHING CLOSES THAT NOTIFICATION WHEN THE TASK LEAVES REVIEW, so the
	// Android icon dot (Android's badge is the unread-notification count; it has
	// no badging API) is NOT cleared by muster. A router-relayed close was tried
	// and removed (PR #38, round 0): as deployed, muster runs standalone — no
	// router, no push routes — so it could never reach a device, and it cost a DB
	// read before every status write. Clearing the dot needs muster-native Web Push — a
	// separate arc. The desktop badge (setAppBadge, /ui/tasks/review-count) is
	// unaffected: it re-reads the count on task.changed.
	// 🔴 BROADCAST HERE, FOR EVERY CALLER, AND AFTER THE WRITE — the human route
	// used to broadcast NOTHING while the machine route broadcast from its own
	// handler. That split is what let the two disagree, and two comments in this
	// package asserted the human path broadcast when it did not (upstream task
	// 487 item 1).
	//
	// The rule those comments stated — "the machine path returns JSON with no card
	// swap, SO it broadcasts" — reasons only about the client that made the
	// request. An htmx swap reaches THAT browser; every other device gets nothing.
	// With /tasks/42 open on a phone and the board on a laptop, a flip on the
	// phone left the laptop's card in the wrong status group until a
	// focus/visibility resync. comment_delete.go already argues exactly this for
	// retractions; the status writer never got it.
	//
	// Safe for the requesting client too: its card was already swapped by the
	// response, and the extra re-fetch this triggers is refused-or-applied by the
	// data-card-rev stale-swap guard (internal/ui/notes.go) — a status write moves
	// notes.updated_at, so the re-fetch is never STRICTLY older and ties apply.
	s.broadcast(ctx, EventTaskChanged, strconv.FormatInt(note.ID, 10))
	return note, nil
}

// handleNoteStatus handles PATCH /tasks/{id}/status: set a task's lifecycle
// status and re-render the single card. This is the session (human) path, so it
// may set any valid status including `complete`.
func (s *Server) handleNoteStatus(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	note, err := s.applyTaskStatus(r.Context(), writerHumanUI, id, r.FormValue("status"))
	if err != nil {
		switch {
		case errors.Is(err, errInvalidStatus):
			http.Error(w, "invalid status", http.StatusBadRequest)
		case errors.Is(err, pgx.ErrNoRows):
			http.NotFound(w, r)
		default:
			s.logger.Printf("notes: set status %d: %v", id, err)
			http.Error(w, "could not update status", http.StatusInternalServerError)
		}
		return
	}
	s.renderNoteCard(w, r, note)
}

// handleAPITaskStatus handles PATCH /api/tasks/{id}/status (machine,
// hook-token-gated): set a durable Task's lifecycle status from a machine
// producer holding just the hook token (an in-cluster agent / CI / a CC session).
// It closes the gap where such a producer could create/read/edit a Task but not
// set its status: the session route is LAN/human-only and the bound-agent route
// forbids `complete`. A hook-token producer is trusted, so ALL statuses are
// allowed here INCLUDING `complete`. It shares applyTaskStatus with the human
// path (full side-effect parity: the ready_for_review push AND the task.changed
// broadcast, which applyTaskStatus fires — NOT this handler), and returns the
// updated task JSON (same shape as GET /api/tasks/{id}).
//
// ⚠ This used to read "broadcasts task.changed (since a JSON response can't swap
// the card)". Both halves are now wrong, and the parenthetical was the reasoning
// ERROR that produced upstream task 487 item 1 in the first place: "the response
// swaps the card, so no broadcast is needed" is true only of the client that
// MADE the request and treats every other device as absent. That is why the
// human route went years without one. Do not reinstate the rule or the
// handler-level broadcast — applyTaskStatus fires it for both callers, and a
// second one here double-fires with nothing to catch it (both this route's and
// the human route's tests assert AN event, not exactly one).
//
// Unknown/absent status →
// 400; unknown id → 404; no/bad hook token → 401 (middleware). There is NO
// in-progress guard (status changes are allowed on a task of any current status).
func (s *Server) handleAPITaskStatus(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad id"})
		return
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}
	note, err := s.applyTaskStatus(r.Context(), writerMachineAPI, id, strings.TrimSpace(body.Status))
	if err != nil {
		switch {
		case errors.Is(err, errInvalidStatus):
			s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid or missing status"})
		case errors.Is(err, pgx.ErrNoRows):
			s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "task not found"})
		default:
			s.logger.Printf("notes: api set status %d: %v", id, err)
			s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not update status"})
		}
		return
	}
	// Task thread (migration 0023): a status flip is WORK. Unlike the edit path
	// (which probes the task first and can link before mutating), this route's
	// existence check IS applyTaskStatus, so the link necessarily comes after — and
	// the note in hand was read before the link existed. Re-read it, but ONLY when
	// the link actually changed something: an already-linked session's repeat touch
	// leaves the note byte-identical, so paying for a second fan-out there would be
	// pure overhead on the common path.
	changed := s.noteTaskSession(r, note.ID, notes.RoleWorked)
	if changed {
		if fresh, ferr := s.ext.Notes.Get(r.Context(), note.ID); ferr == nil {
			note = fresh
		} else {
			s.logger.Printf("notes: api set status refresh %d: %v", note.ID, ferr)
		}
	}
	// No broadcast here: applyTaskStatus fires task.changed for BOTH of its
	// callers — this handler and handleNoteStatus. It used to be emitted from this
	// handler, which is why the human route could go without one; sharing the site
	// is what stops those two drifting again. Re-broadcasting here double-fires.
	//
	// ⚠ SCOPE, precisely — this comment previously claimed applyTaskStatus fires
	// "for EVERY status writer" and was "the single site", and BOTH are false.
	// Only ONE setNoteStatus caller is applyTaskStatus; the rest bypass it —
	// advanceTaskToInProgress (agents.go), handleTaskMerge (merge.go), and
	// handleAgentTaskStatus / dispatchAgentTool (agent.go).
	// Each of those broadcasts from its own handler, so the property holds by
	// REPETITION, not by construction, exactly as the corrected comment in
	// agents.go says. Restating it as a package-wide invariant is the same
	// over-claim this PR exists to remove — and it was written INTO this PR while
	// the commit next to it warned against doing so.
	//
	// 🔴 NAMED BY SYMBOL AND CARRYING ITS DERIVATION, BECAUSE THE COUNTS HERE WENT
	// STALE INSIDE THIS PR. This paragraph read "8 call sites … operator.go:230/552,
	// agents.go:684, agent.go:148/318/593, merge.go:293 … 27 non-test sites … wrong
	// by 26". Task #653 phase two deleted operator.go, which removed two of those
	// callers — so the counts were falsified by the very change that shipped
	// alongside them, and the site list named a file that no longer exists. Neither
	// figure is restated here. Re-derive instead, and note both patterns over-match
	// if you include the definition or the const:
	//
	//	find . -name '*.go' ! -name '*_test.go' -print0 | xargs -0 grep -n 's\.setNoteStatus('
	//	find . -name '*.go' ! -name '*_test.go' -print0 | xargs -0 grep -c 's\.broadcast(EventTaskChanged'
	//
	// (plain `grep` under xargs — this repo's interactive `grep` is a ugrep function
	// honouring .gitignore, and `command grep` is a builtin xargs cannot exec.)
	s.writeJSON(w, http.StatusOK, s.apiTaskOne(r.Context(), note))
}

// handleNoteEditModal serves GET /ui/tasks/{id}/edit: the edit-task form body,
// pre-filled with the task's current editable values (directory/body/model/repo/
// branch), loaded into #task-modal-body when the card's Edit button is
// tapped. The in-progress guard is enforced on the POST (submit) — the card hides
// the Edit affordance for an in-progress task, so this GET just renders the form.
func (s *Server) handleNoteEditModal(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	note, err := s.ext.Notes.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		s.logger.Printf("notes: edit modal get %d: %v", id, err)
		http.Error(w, "could not load task", http.StatusInternalServerError)
		return
	}
	dirs, dirsFailed := s.directorySeed(r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := ui.RenderNotesEditModal(w, ui.NoteEditView{
		Note:              note,
		Directories:       dirs,
		DirectoriesFailed: dirsFailed,
		// The tag input's <datalist> suggestions come from the live vocabulary, so
		// the human reuses existing labels instead of minting near-duplicates.
		Vocabulary: s.tagVocabulary(r.Context()),
	}); err != nil {
		s.logger.Printf("notes: render edit modal: %v", err)
	}
}

// handleNoteEdit handles POST /tasks/{id}/edit (session/human path): apply the
// edited content + dispatch config and re-render the single card. It enforces the
// in-progress guard SERVER-side (the card hides Edit for an in-progress task, but
// this is the authoritative check) → 409. Every editable field is submitted by the
// form, so this is a full replace of the editable columns (status/provenance/
// created_at are never touched by UpdateNote).
func (s *Server) handleNoteEdit(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	// Enforce the in-progress guard BEFORE mutating (defense in depth — the UI
	// hides Edit for an in-progress task).
	cur, err := s.ext.Notes.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		s.logger.Printf("notes: edit get %d: %v", id, err)
		http.Error(w, "could not load task", http.StatusInternalServerError)
		return
	}
	if cur.Status == notes.StatusInProgress {
		http.Error(w, "task is in progress and cannot be edited", http.StatusConflict)
		return
	}

	// Tags: the modal's chip editor submits one hidden `tag` input per chip, so the
	// form is a full REPLACE of the task's tag set (the human is looking at the
	// whole set). Validated loudly — an invalid tag is a 400 that htmx surfaces as an error toast, never a silent drop.
	tags, terr := s.resolveTaskTags(r.Context(), r.Form["tag"])
	if terr != nil {
		var ve errTagValidation
		if errors.As(terr, &ve) {
			http.Error(w, ve.Error(), http.StatusBadRequest)
			return
		}
		s.logger.Printf("notes: edit tags %d: %v", id, terr)
		http.Error(w, "could not validate tags", http.StatusInternalServerError)
		return
	}

	body := strings.TrimSpace(r.FormValue("body"))
	if body == "" {
		http.Error(w, "task body required", http.StatusBadRequest)
		return
	}
	body = capRunes(body, maxTaskBodyLen)
	directory := capRunes(r.FormValue("directory"), maxTaskFieldLen)
	title := capRunes(strings.TrimSpace(r.FormValue("title")), maxTaskFieldLen)
	model := capRunes(strings.TrimSpace(r.FormValue("model")), maxTaskFieldLen)
	repo := capRunes(strings.TrimSpace(r.FormValue("repo")), maxTaskFieldLen)
	branch := capRunes(strings.TrimSpace(r.FormValue("repo_branch")), maxTaskFieldLen)
	// The form submits every editable field → set them all.
	updated, err := s.ext.Notes.UpdateNote(r.Context(), id, notes.NoteUpdate{
		Directory:  &directory,
		Title:      &title,
		Body:       &body,
		Model:      &model,
		Repo:       &repo,
		RepoBranch: &branch,
		Tags:       &tags,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		s.logger.Printf("notes: edit %d: %v", id, err)
		http.Error(w, "could not update task", http.StatusInternalServerError)
		return
	}
	observeRoutingTags(updated.Tags)
	s.logAutoDispatch(updated.ID, updated.Tags)
	s.broadcast(r.Context(), EventTaskChanged, strconv.FormatInt(updated.ID, 10))
	s.renderNoteCard(w, r, updated)
}

// reservedCommentAuthors are the PRIVILEGED comment identities an external
// machine producer must never be able to author as:
//   - "user" is the human (handleNoteComment hardcodes it) — only the operator/
//     human may declare a task complete, and commentAuthor renders the author
//     string VERBATIM on the card, so an impersonated "user" comment would read
//     as something Zach wrote.
//   - "operator" is the reserved privileged agent NAME (reservedOperatorSlug, in
//     internal/api/reserved_names.go — operator.go was deleted by upstream task #653
//     phase two, and that file's header says why the constant did NOT move to
//     auth.go). ⚠ Nothing in internal/api can stamp it on a comment any longer; the
//     entry is kept because historic rows still carry it and an external producer id
//     equal to it would still be an impersonation.
//
// The guarantee is STRUCTURAL, not a filter: taskCommentAuthor only ever returns
// a value from taskSourceAllowlist, which contains neither. This var exists so
// TestTaskSourceAllowlistExcludesPrivilegedIdentities can pin that invariant if
// someone later adds a producer id to the allowlist.
var reservedCommentAuthors = map[string]bool{"user": true, "operator": true}

// taskCommentAuthor derives the comment author for the machine endpoint from the
// request's PROVENANCE (the same bounded X-Muster-Source allowlist that stamps
// task provenance) — never from a client-supplied field, so a producer cannot
// choose who it appears to be.
//
// It returns the derived source verbatim: `repo-cos`, `drafter`, `claude-code`,
// `clickup`, `extension`, or — for the default/unidentified case (no header, or an unknown
// value) — "api". Unlike task provenance (which stores the default as NULL), a
// comment row's author column is NOT NULL and is rendered verbatim, so the
// default needs a real value: "api" is honest (it IS the machine API), clearly
// non-human, and reuses the provenance vocabulary rather than inventing a second
// one. It can never be "user"/"operator" because the allowlist cannot produce
// them.
func taskCommentAuthor(r *http.Request) string {
	source, _ := taskSource(r)
	if source == "" {
		return "api"
	}
	return source
}

// handleAPITaskComment handles POST /api/tasks/{id}/comments (machine,
// hook-token-gated): append a comment to a durable Task's thread from an EXTERNAL
// producer that muster did not provision (repo-cos, the task-drafter,
// mail-actions, the initiatives dispatcher, CI, a CC session). It closes the last
// producer gap: such a producer could create/read/edit/set-status/delete a task
// but could not report ON it — the per-agent route (POST /agent/task/comment)
// needs a muster-issued agent token, and the session route hardcodes
// Author:"user", so an external reporter could not attribute itself.
//
// AUTHOR is derived from provenance (taskCommentAuthor), never from the body: an
// `author` key in the JSON is silently IGNORED (matching how handleAPITaskEdit
// ignores immutable keys), and the bounded allowlist structurally cannot yield
// the privileged "user"/"operator" identities.
//
// ⚠ THAT LAST CLAUSE IS A PROPERTY OF **THIS ROUTE**, NOT OF THE SYSTEM. The
// session routes `POST /tasks/{id}/comments` and `POST /tasks/merge` hardcode
// `Author: "user"` for whoever posts to them, so `user` is minted by a DIFFERENT
// door that this allowlist does not guard. That door is no longer open to the
// whole LAN — requireSession refuses a caller with no valid signed cookie — but
// it is still one shared operator password, so `user` means "whoever holds the
// operator session", not a particular person. So this allowlist bounds what an
// EXTERNAL PRODUCER can claim ON THIS PATH; it does not make `user` a strong
// identity. Read as a system guarantee it is false, and it was read that way
// once — see upstream task #394 and merge.go's mergeCommentAuthor.
//
// Contract mirrors the sibling machine endpoints — always JSON, never the htmx
// card fragment the session route returns: 200 + the created comment; 400 bad id
// / invalid JSON / empty-or-whitespace body; 404 unknown task; 500 store failure;
// 401 no/bad hook token (middleware). It broadcasts task.changed (a JSON response
// can't swap the card) and fires a coalesced Web Push so a report lands on the
// phone.
func (s *Server) handleAPITaskComment(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad id"})
		return
	}
	// Only `body` is decoded. An `author` key is silently dropped by Go's decoder
	// (no DisallowUnknownFields) — the impersonation guard is that there is simply
	// no field to carry it.
	var body struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}
	text := strings.TrimSpace(body.Body)
	if text == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "comment body required"})
		return
	}
	// Cap the attacker-influenceable text (hook-token producers are trusted, but a
	// runaway report must not bloat the row). Rune-safe so the stored value stays
	// valid UTF-8. Truncate, never reject — a long report still lands.
	text = capRunes(text, maxTaskBodyLen)

	// Existence probe BEFORE the insert: AddComment on an unknown note_id fails as
	// an FK violation (a 500-shaped store error), not pgx.ErrNoRows, so without
	// this an unknown id would answer 500 instead of 404. It also gives us the note
	// for the push title.
	note, err := s.ext.Notes.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "task not found"})
			return
		}
		s.logger.Printf("notes: api comment get %d: %v", id, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not load task"})
		return
	}
	c, err := s.ext.Notes.AddComment(r.Context(), notes.Comment{
		NoteID: id,
		Author: taskCommentAuthor(r),
		Body:   text,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "task not found"})
			return
		}
		s.logger.Printf("notes: api comment %d: %v", id, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not add comment"})
		return
	}
	// Task thread (migration 0023): commenting is WORK, so the session joins with
	// the `worked` role — upgrading a `read` link it may already hold.
	//
	// 🔴 AFTER AddComment, deliberately — the same rule as the edit path. This used
	// to run BEFORE the insert, so a comment that failed (an unknown-but-live-looking
	// id → 404, or a store error → 500) still marked the session as having worked the
	// task, permanently, because the role never downgrades. A refused write records
	// NO link — not `worked` and not a consolation `read`.
	//
	// Nothing needs re-reading afterwards: the response is the COMMENT, not the task,
	// so it never carried a `sessions` array to keep fresh. The handler's own
	// end-of-request task.changed broadcast (on the SUCCESS path only) covers the
	// thread change.
	//
	// 🔴 NO `defer broadcastTaskLink` HERE, deliberately. It used to be deferred, so
	// a comment insert that FAILED still broadcast task.changed and told every open
	// tab to re-fetch a task nothing had changed. A deferred broadcast fires on
	// every return path, including the error ones — which is precisely what you do
	// not want from a notification.
	//
	// 🔴 This is what replaces the prose ritual — the pickup convention writes
	// "**Starting** — host x, session abc123" into the body, and this link records
	// the same fact structurally, from the request itself. The comment body is
	// never parsed.
	s.noteTaskSession(r, id, notes.RoleWorked)
	// The machine path returns JSON (no htmx card swap), so broadcast task.changed
	// so any open Tasks tab re-fetches and shows the new comment live.
	s.broadcast(r.Context(), EventTaskChanged, strconv.FormatInt(id, 10))
	// Coalesced background push (machine path ONLY — the session route is the human
	// typing their own comment and must not buzz their own phone; the per-agent
	// route keeps its existing no-push behaviour). Best-effort + nil-push-safe.
	s.notifyTaskCommented(note, c)
	s.writeJSON(w, http.StatusOK, c)
}

// handleNoteComment handles POST /tasks/{id}/comments: append a comment and
// re-render the single card.
//
// ⚠ "AUTHORED BY THE HUMAN" IS TRUE, BUT IT DOES NOT NAME ONE. This route is
// wrapped in `requireSession`, which refuses a caller with no valid signed
// session cookie, so `Author: "user"` is stamped only for someone who completed
// a login — on the LAN NodePort as well as behind the Authelia edge. What the
// tier cannot supply is WHICH human: it is one shared operator password, so
// "user" means "whoever holds the operator session". Same scope caveat as
// merge.go's mergeCommentAuthor — see upstream task #394 for the decision, and do
// not restate this as an unqualified guarantee about a person.
func (s *Server) handleNoteComment(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	body := strings.TrimSpace(r.FormValue("body"))
	if body == "" {
		http.Error(w, "comment body required", http.StatusBadRequest)
		return
	}
	if _, err := s.ext.Notes.AddComment(r.Context(), notes.Comment{NoteID: id, Author: "user", Body: body}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		s.logger.Printf("notes: add comment %d: %v", id, err)
		http.Error(w, "could not add comment", http.StatusInternalServerError)
		return
	}
	note, err := s.ext.Notes.Get(r.Context(), id)
	if err != nil {
		s.logger.Printf("notes: get %d after comment: %v", id, err)
		http.Error(w, "could not load task", http.StatusInternalServerError)
		return
	}
	s.renderNoteCard(w, r, note)
}

// renderNoteCard writes a single task card; it morphs/replaces #task-<id> via
// htmx outerHTML swap. It resolves the task's linked agent (best-effort) so the
// re-rendered card keeps its live status chip / Open-chat state.
//
// 🔴 THIS IS THE ONE PLACE THE BOARD/DETAIL SHAPE IS DECIDED. Every mutation
// route that re-renders a card — PATCH /tasks/{id}/status, POST
// /tasks/{id}/comments, DELETE /tasks/{id}/comments/{cid}, POST /tasks/{id}/tags,
// DELETE /tasks/{id}/tags/{tag}, POST /tasks/{id}/edit — funnels through here,
// so the predicate lives here ONCE rather than being open-coded per route (and
// no route is duplicated for the detail page). The ledger of those call sites is
// pinned by TestRenderNoteCardCallSitesAreLedgered.
//
// The shape is derived from htmx's HX-Current-URL (see view.go): the request's
// own statement of where the browser is, recomputed per request. It is NOT read
// from a header the page's markup stamps — that design leaked the detail shape
// onto the board across a boosted sidebar navigation, because body ATTRIBUTES
// survive an innerHTML body swap. view.go carries the full account.
//
// It takes the *http.Request rather than a context precisely so it can read that
// header — a context cannot carry it.
func (s *Server) renderNoteCard(w http.ResponseWriter, r *http.Request, note notes.Note) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	v := s.taskCardView(r.Context(), note)
	v.Detail = rendersInDetailShape(r, note.ID)
	if err := ui.RenderNoteCard(w, v); err != nil {
		s.logger.Printf("notes: render card: %v", err)
	}
}

// handleTaskDetail serves GET /tasks/{id}: the full, standalone task page. It is
// shaped exactly like handleSessionDetail — parse, load, 404, render, log-only
// on a render failure (never a 500 mid-stream, the response is already
// committed by then).
//
// A SOFT-DELETED task 404s here for free: (*PGStore).Get applies liveOnly, so a
// dismissed task comes back as pgx.ErrNoRows exactly like an unknown id.
//
// 🔴 THE 404 IS A PAGE, NOT http.NotFound's PLAIN TEXT — and that is about the
// DEAD END, not about polish. taskHashScript upgrades every legacy
// `/tasks#task-N` link (they are out in ClickUp comments, ClickHouse
// activity.events and several docs) with location.replace(), which CONSUMES the
// history entry. Land such a link on a dismissed task and Go's bare
// "404 page not found" leaves no chrome, no sidebar and no link back — and Back
// leaves muster entirely, because the entry the user arrived from is gone.
// Before this page existed the board loaded and toasted "Task #N is not on this
// board."; a styled 404 with the sidebar and a link to /tasks is the equivalent.
func (s *Server) handleTaskDetail(w http.ResponseWriter, r *http.Request) {
	raw := r.PathValue("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	// 🔴 SERVE THIS DOCUMENT AT ONE URL ONLY. ParseInt accepts spellings the view
	// seam's regexp does not — `+12` most of all, which a paste can produce — and
	// a page served at /tasks/+12 issues every mutation with
	// HX-Current-URL=/tasks/+12, which detailDocumentPath does not match, so the
	// answers come back BOARD-shaped and morph into the detail page. Redirecting
	// to canonicalTaskPath means the two parsers can never disagree, instead of
	// asking a second parser to agree with the first. See view.go.
	if raw != strconv.FormatInt(id, 10) {
		http.Redirect(w, r, canonicalTaskPath(id), http.StatusFound)
		return
	}
	note, err := s.ext.Notes.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.renderTaskNotFound(w, r, r.PathValue("id"))
			return
		}
		s.logger.Printf("notes: get %d for detail: %v", id, err)
		http.Error(w, "could not load task", http.StatusInternalServerError)
		return
	}
	view := s.taskCardView(r.Context(), note)
	view.Detail = true
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := ui.RenderTaskDetail(w, view, s.shellFeatures(r.Context())); err != nil {
		s.logger.Printf("notes: render task detail %d: %v", id, err)
	}
}

// renderTaskNotFound writes the styled 404 document for a task that is gone
// (dismissed) or never existed. Status FIRST, then the body — this is a real 404
// to crawlers and to the e2e suite, not a 200 that says "not found".
func (s *Server) renderTaskNotFound(w http.ResponseWriter, r *http.Request, id string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	if err := ui.RenderTaskNotFound(w, id, s.shellFeatures(r.Context())); err != nil {
		s.logger.Printf("notes: render task 404 %s: %v", id, err)
	}
}

// handleNoteCard serves GET /ui/tasks/{id}/card: the single card, re-rendered.
// It is the detail page's equivalent of the board re-fetching /ui/tasks — see
// ui.taskDetailLive. The shape comes from renderNoteCard's one derivation, so
// this route needs no view logic of its own.
func (s *Server) handleNoteCard(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	note, err := s.ext.Notes.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		s.logger.Printf("notes: get %d for card: %v", id, err)
		http.Error(w, "could not load task", http.StatusInternalServerError)
		return
	}
	s.renderNoteCard(w, r, note)
}

// handleNoteAttachment handles GET /notes/{id}/attachments/{aid}: stream an
// attachment's bytes for download/preview.
func (s *Server) handleNoteAttachment(w http.ResponseWriter, r *http.Request) {
	aid, err := strconv.ParseInt(r.PathValue("aid"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	att, err := s.ext.Notes.GetAttachment(r.Context(), aid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		s.logger.Printf("notes: get attachment %d: %v", aid, err)
		http.Error(w, "could not load attachment", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", att.ContentType)
	w.Header().Set("Content-Disposition", "inline; filename=\""+att.Filename+"\"")
	w.Header().Set("Content-Length", strconv.Itoa(len(att.Data)))
	_, _ = w.Write(att.Data)
}

// renderNotesPanel writes the refreshed notes list (used after a create); it
// morphs into #notes-list.
func (s *Server) renderNotesPanel(w http.ResponseWriter, r *http.Request) {
	s.handleNotesContent(w, r)
}
