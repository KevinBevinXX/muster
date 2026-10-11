package ui

import (
	"io"

	"github.com/ZacxDev/muster/internal/notes"
)

// RenderNotesCards writes the /ui/tasks partial: the tag filter chip row plus
// the tasks as ONE flat list ordered by most recent activity, morphed into
// #tasks-list. The view carries each task with its optional linked agent (for the
// status chip), the tag vocabulary (the chip row), and the ACTIVE filter — the
// selection is server-rendered so an SSE-driven refresh preserves it.
func RenderNotesCards(w io.Writer, v TasksView) error {
	return NotesCards(v).Render(w)
}

// RenderTagDatalist writes the GET /ui/tags partial: a bare <datalist> of the tag
// vocabulary, used as the tag input's suggestion source in the edit modal.
func RenderTagDatalist(w io.Writer, vocab []notes.TagCount) error {
	return tagDatalist(vocab).Render(w)
}

// RenderNotesModal writes the /ui/notes/new partial (the create-note form body).
func RenderNotesModal(w io.Writer, directories []string, directoriesFailed bool, prefill string) error {
	return NotesModalBodyWith(directories, directoriesFailed, prefill).Render(w)
}

// RenderNotesEditModal writes the /ui/tasks/{id}/edit partial (the edit-task form
// body, pre-filled with the task's current values).
func RenderNotesEditModal(w io.Writer, v NoteEditView) error {
	return NotesEditModalBody(v).Render(w)
}

// RenderNoteCard writes a single task card, for outerHTML swaps of #task-<id>
// after a status change or new comment. The view carries the task plus its
// optional linked agent so the re-rendered card keeps its status chip.
func RenderNoteCard(w io.Writer, v TaskCardView) error {
	return noteCard(v).Render(w)
}

// RenderTaskDetail writes the full GET /tasks/{id} document: the task rendered
// in DETAIL shape (body, attachments, session thread, comments, comment form)
// inside the app's chrome. The view is the SAME TaskCardView the board renders,
// so there is exactly one card renderer and no forked view.
func RenderTaskDetail(w io.Writer, v TaskCardView, feat Features) error {
	return TaskDetailPage(v, feat).Render(w)
}

// RenderTaskNotFound writes the styled GET /tasks/{id} 404 document (sidebar,
// one <h1>, an explanation and an unboosted link back to the board).
func RenderTaskNotFound(w io.Writer, id string, feat Features) error {
	return TaskNotFoundPage(id, feat).Render(w)
}

// RenderAgentsCards writes the /ui/agents partial (just the cards, morphed into
// #agents-list).
func RenderAgentsCards(w io.Writer, list []AgentCardView) error {
	return AgentsCards(list).Render(w)
}

// RenderDispatchModal writes the /ui/agents/new partial (the FAB dispatch form
// body: Model, Repository, Existing task, ad-hoc task).
func RenderDispatchModal(w io.Writer, k KindChoices) error {
	return DispatchModalBodyFor(k).Render(w)
}

// RenderDispatchModalTask writes the /ui/agents/new?note=<id> partial: the
// TASK-SCOPED dispatch confirm (fixed Model/Repository rows pre-filled
// from the task, each with an inline Edit override).
func RenderDispatchModalTask(w io.Writer, v TaskDispatchView) error {
	return DispatchModalTaskBody(v).Render(w)
}

// RenderAgentDetail writes the full agent detail page (logs + chat).
func RenderAgentDetail(w io.Writer, v AgentDetailView, feat Features) error {
	return AgentDetailPage(v, feat).Render(w)
}

// RenderAgentChatLog writes the inner content of #chat-log (transcript bubbles +
// provisioning/working indicators) for the active session — the GET
// /ui/agents/{name}/chat-log partial swapped in on a sse:chat.reply refresh so a
// server-side (kickoff) turn appears live without a page reload.
func RenderAgentChatLog(w io.Writer, v AgentDetailView) error {
	return chatLogInner(v).Render(w)
}
