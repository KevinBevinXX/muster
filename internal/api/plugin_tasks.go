package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plugins"
)

// THE PLUGIN'S WAY TO FILE WORK DIRECTLY: POST /plugin/v1/tasks.
//
// A plugin message goes to the chief, which decides what to do with it. Some
// upstreams are not messages for an LLM to judge but deterministic NOTICES about
// a record elsewhere — "ticket 4711 was assigned to you" — that map to exactly
// one task here. For those the plugin calls this route: find the task carrying
// the upstream's key (an external-id tag such as `tracker:4711`), create it if
// asked and none exists, else append a comment to it. Nothing reaches the chief.
//
// 🔴 IDEMPOTENT BY CONSTRUCTION, BECAUSE PLUGINS RETRY. A plugin re-posts when it
// did not see an answer (docs/plugins.md), and the same upstream record can be
// noticed many times. So: at most one OPEN task per key per owner (not dismissed, complete or merged away)
// (notes.Store.CreateOnceByTag, serialised on an advisory lock), and a comment
// identical to one the thread already holds from this plugin is not appended
// again (notes.Store.AddCommentOnce). A plugin makes each comment unique per
// upstream event (it carries the upstream's message id), so a repeat is a no-op.
//
// 🔴 THE KEY NAMESPACE IS A CLOSED SET, AND THE DEPLOYMENT'S. The key becomes a
// tag, and tags route work: a plugin must not be able to file `gate:`, `auto:`,
// `project:` or `initiative:` tags, nor stamp `superseded-by:`. Only a namespace
// MUSTER_PLUGIN_TASK_NAMESPACES names is accepted (notes.SetPluginTaskNamespaces
// refuses the reserved ones at boot), and each is an external-id namespace, so
// its one-task tags stay out of the board's filter row. With none configured the
// route answers 503, which is not a permanent refusal: a consumer should try the
// notice again later (how it holds or sets it aside meanwhile is its choice),
// whereas a 400 says the same bytes will never be accepted.
//
// The task is created UNGATED and with no `auto:` tag: nothing dispatches it on
// its own (there is no per-owner dispatch policy to apply), and the owner — who
// gets the usual "New task" push — dispatches it by hand.

const pluginTasksPath = pluginAPIPrefix + "/tasks"

// pluginTaskRequestBytes bounds what the route reads: the body and the comment
// each at plugins.MaxBodyBytes, escaped at worst 6 bytes per byte, plus the
// title and key the same way and room for the field names.
const pluginTaskRequestBytes = 6*(2*plugins.MaxBodyBytes+maxTaskFieldLen+notes.MaxTagRunes*4) + 1024

type pluginTaskIn struct {
	Key    string `json:"key"`
	Create *struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	} `json:"create"`
	Comment string `json:"comment"`
}

// truncBytes cuts s to at most n bytes, on a rune boundary.
func truncBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// normalizePluginTask trims the comment (whitespace-only is no comment, as on
// the machine comment route) and TRUNCATES an oversize body, comment or title,
// never refusing one: the machine comment route's "truncate, never reject",
// and a consumer reads a 400 as permanent.
func normalizePluginTask(in *pluginTaskIn) {
	in.Comment = truncBytes(strings.TrimSpace(in.Comment), plugins.MaxBodyBytes)
	if in.Create != nil {
		in.Create.Body = truncBytes(in.Create.Body, plugins.MaxBodyBytes)
		in.Create.Title = capRunes(in.Create.Title, maxTaskFieldLen)
	}
}

// validatePluginTask returns a 400 message, or "". It runs after
// normalizePluginTask.
func validatePluginTask(in pluginTaskIn) string {
	ns, val := notes.ParseTag(in.Key)
	switch {
	case in.Key == "" || notes.NormalizeTag(in.Key) != in.Key:
		return "key is required and must be a normalized tag (lower-case namespace:value)"
	case !notes.IsPluginTaskNamespace(ns) || val == "":
		return "key must be <namespace>:<id> in one of this server's plugin task namespaces (" +
			strings.Join(notes.PluginTaskNamespaces(), ", ") + ")"
	}
	if err := notes.ValidateTags([]string{in.Key}); err != nil {
		return "key: " + err.Error()
	}
	if in.Create == nil && in.Comment == "" {
		return "nothing to do: give create, a non-blank comment, or both"
	}
	fields := map[string]string{"comment": in.Comment}
	if in.Create != nil {
		if strings.TrimSpace(in.Create.Body) == "" {
			return "create.body is required"
		}
		fields["title"], fields["body"] = in.Create.Title, in.Create.Body
	}
	for _, name := range []string{"title", "body", "comment"} {
		if v, ok := fields[name]; ok && (!utf8.ValidString(v) || strings.ContainsRune(v, 0)) {
			return name + " must be UTF-8 text without NUL"
		}
	}
	return ""
}

// pluginCommentAuthor is the author of a comment this route writes: the plugin's
// own name, so the thread says which plugin wrote it and the duplicate check is
// per plugin.
func pluginCommentAuthor(p plugins.Plugin) string { return "plugin:" + p.Name }

// handlePluginTask is POST /plugin/v1/tasks {key, create?, comment?}:
//
//   - an OPEN task carries key: comment (if given) is appended to it unless the
//     thread already holds it; 200 {id, created:false, commented};
//   - none does and create is given: the task is filed with create's title and
//     body and the key as its tag, and the comment (if given) is recorded on it
//     IN THE SAME TRANSACTION, so a resend of the same request finds both and
//     adds nothing; 201 {id, created:true, commented};
//   - none does and create is absent: nothing is written; 200 {id:null, ...}.
func (s *Server) handlePluginTask(w http.ResponseWriter, r *http.Request) {
	if s.ext.Notes == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "this server has no task store"})
		return
	}
	if len(notes.PluginTaskNamespaces()) == 0 {
		s.logger.Printf("plugins: plugin task refused 503: MUSTER_PLUGIN_TASK_NAMESPACES is empty, so this server files no plugin tasks")
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "this server files no plugin tasks: MUSTER_PLUGIN_TASK_NAMESPACES is empty"})
		return
	}
	var in pluginTaskIn
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, pluginTaskRequestBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "the body is {\"key\", \"create\": {\"title\", \"body\"}, \"comment\"}"})
		return
	}
	normalizePluginTask(&in)
	if msg := validatePluginTask(in); msg != "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": msg})
		return
	}
	p := pluginFromContext(r.Context())
	ctx := r.Context()
	answer := func(code int, id *int64, created, commented bool) {
		s.writeJSON(w, code, map[string]any{"id": id, "created": created, "commented": commented})
	}
	var comment *notes.Comment
	if in.Comment != "" {
		comment = &notes.Comment{Author: pluginCommentAuthor(p), Body: in.Comment}
	}

	if in.Create != nil {
		n, created, commented, err := s.createTaskOnce(ctx,
			taskCreateInput{Title: in.Create.Title, Body: in.Create.Body, Source: "api"}, in.Key, comment, nil)
		var ve errTagValidation
		switch {
		case err == nil:
		case errors.As(err, &ve):
			s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": ve.Error()})
			return
		default:
			s.logger.Printf("plugins: plugin %d task %s: create: %v", p.ID, in.Key, err)
			s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the task could not be filed"})
			return
		}
		if created {
			answer(http.StatusCreated, &n.ID, true, commented)
			return
		}
		if commented {
			s.broadcast(ctx, EventTaskChanged, strconv.FormatInt(n.ID, 10))
		}
		answer(http.StatusOK, &n.ID, false, commented)
		return
	}
	found, ok, err := s.ext.Notes.FindLiveByTag(ctx, in.Key)
	if err != nil {
		s.logger.Printf("plugins: plugin %d task %s: lookup: %v", p.ID, in.Key, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the task could not be looked up"})
		return
	}
	if !ok {
		answer(http.StatusOK, nil, false, false)
		return
	}
	comment.NoteID = found.ID
	wrote, err := s.ext.Notes.AddCommentOnce(ctx, *comment)
	switch {
	case err == nil:
	case errors.Is(err, pgx.ErrNoRows):
		// Dismissed between the lookup and the comment: as if it never existed.
		answer(http.StatusOK, nil, false, false)
		return
	default:
		s.logger.Printf("plugins: plugin %d task %d: comment: %v", p.ID, found.ID, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the comment could not be added"})
		return
	}
	if wrote {
		s.broadcast(ctx, EventTaskChanged, strconv.FormatInt(found.ID, 10))
	}
	answer(http.StatusOK, &found.ID, false, wrote)
}
