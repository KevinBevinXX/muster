package api

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/notes"
)

// Task-lifecycle Web Push notifications. Three events hang off the existing
// producer/agent hooks and fan out to every subscribed device via the shared
// push.Service, exactly like the permission-request pushes:
//
//  1. Task CREATED (machine/producer path only, POST /api/tasks) — coalesced: a
//     burst of creates (the drafter posts ~6 in a loop) collapses to ONE push.
//  2. Provisioning COMPLETE — a task-linked agent reaching `running`, once.
//  3. Task DONE — a task entering `ready_for_review`.
//
// All three are best-effort and nil-push-safe (pushTask no-ops when push is
// disabled), and none may ever fail the create/dispatch/status handler.
const (
	// taskNotificationType is the payload Type the service worker branches on to
	// render an informational task notification (no Approve/Deny actions) and, on
	// click, focus/open Data.url.
	taskNotificationType = "task"

	// taskCreateDebounce is how long the created-task coalescer waits for the
	// burst to settle before firing one push. Reset on each create; the drafter's
	// ~6-task loop lands well inside it.
	taskCreateDebounce = 4 * time.Second

	// taskCommentDebounce is the same window for EXTERNAL comments (POST
	// /api/tasks/{id}/comments): an agent posting a burst of progress notes
	// collapses into ONE push. Reset on each comment.
	taskCommentDebounce = 4 * time.Second

	// taskBodyMax bounds a task title used as a notification body/summary.
	taskBodyMax = 120
)

// resettableTimer is the minimal timer behaviour the created-task debouncer
// needs. *time.Timer satisfies it, so production uses time.AfterFunc; tests
// inject a fake that fires on demand without a real sleep.
type resettableTimer interface {
	Reset(d time.Duration) bool
	Stop() bool
}

// afterFunc constructs a timer that invokes f after d. It is the injectable
// seam behind the debounce window (production = realAfterFunc).
type afterFunc func(d time.Duration, f func()) resettableTimer

// realAfterFunc is the production afterFunc backed by time.AfterFunc.
func realAfterFunc(d time.Duration, f func()) resettableTimer { return time.AfterFunc(d, f) }

// createdTaskRef is one buffered machine-created task awaiting the coalesced push.
type createdTaskRef struct {
	id    int64
	title string
}

// commentedTaskRef is one buffered EXTERNAL comment awaiting the coalesced push:
// the task it landed on, its derived author, and the already-FLATTENED comment
// text (a push body is plain text — see commentNotificationText).
type commentedTaskRef struct {
	noteID int64
	author string
	text   string
}

// pushTask fans a task-lifecycle payload out to every subscription. It is the
// single choke point every task notification uses: it no-ops when push is
// disabled (s.push == nil) and runs the fan-out in its own goroutine so push
// latency never blocks the create/dispatch/status handler that triggered it.
//
// 🔴 ONE RULE, ONE PLACE — the click-through URL is DEFAULTED here, not spelled
// out at each call site. Three of the four task pushes used to hardcode
// {"url": "/tasks"}, so a tap on "Task #412 ready for review" landed on the
// board and left you hunting for the card. Two of those three already carried
// the task id, so the rule is simply "deeplink when we know which task"; the
// coalesced N-task create push has no single id and correctly keeps the board
// URL. A fifth task push added later inherits the behaviour by construction,
// which three literals could never guarantee.
//
// 🔴 IT IS A DEFAULT, NOT AN OVERRIDE, AND THAT DISTINCTION IS LOAD-BEARING.
// The fourth site — "Task #N: agent ready" (notifyAgentRunning) — sets BOTH the
// task id and an explicit url of /agents/<name>, on purpose: what you want when
// the agent comes up is the agent's chat, not the task. An unconditional
// derivation silently redirected that one to /tasks/<id>. So an explicit
// Data["url"] always wins; this only fills the gap.
//
// The service worker already routes payload.type === 'task' through
// focusOrOpen(data.url), so nothing in sw.js changes.
func (s *Server) pushTask(payload RouterNotification) {
	if s.router == nil {
		return
	}
	if payload.Data["url"] == "" {
		url := "/tasks"
		if payload.ID != "" {
			url = "/tasks/" + payload.ID
		}
		// Copy rather than write through: payload.Data belongs to the caller, and a
		// caller that reused one map across pushes would otherwise see it mutated.
		data := make(map[string]string, len(payload.Data)+1)
		for k, v := range payload.Data {
			data[k] = v
		}
		data["url"] = url
		payload.Data = data
	}
	s.goNotify(payload, func(n int) {
		s.logger.Printf("push: delivered task notification tag=%s to %d device(s)", payload.Tag, n)
	})
}

// --- 1. Task CREATED (coalesced) ---

// notifyTaskCreated buffers a machine-created task and (re)arms the debounce
// timer so a burst of creates collapses into ONE push once it settles. Only the
// producer path (POST /api/tasks) calls this — the FAB create does not, since
// the user just made that task themselves. Best-effort + nil-push-safe.
func (s *Server) notifyTaskCreated(n notes.Note) {
	// Skip the buffer+timer machinery entirely when push is disabled — there is
	// nothing to eventually deliver.
	if s.router == nil {
		return
	}
	s.taskNotifyMu.Lock()
	s.taskNotifyBuf = append(s.taskNotifyBuf, createdTaskRef{id: n.ID, title: taskTitle(n)})
	if s.taskNotifyTimer == nil {
		s.taskNotifyTimer = s.taskCreateAfter(taskCreateDebounce, s.flushCreatedTasks)
	} else {
		s.taskNotifyTimer.Reset(taskCreateDebounce)
	}
	s.taskNotifyMu.Unlock()
}

// flushCreatedTasks fires the single coalesced push for the tasks buffered since
// the window opened, then clears the buffer + timer. It is the debounce timer's
// callback (and is invoked directly by tests via the injected timer). A single
// buffered task → "New task" + its title; N>1 → "N new tasks" + a brief summary.
func (s *Server) flushCreatedTasks() {
	s.taskNotifyMu.Lock()
	buf := s.taskNotifyBuf
	s.taskNotifyBuf = nil
	s.taskNotifyTimer = nil
	s.taskNotifyMu.Unlock()

	if len(buf) == 0 {
		return
	}

	var title, body string
	if len(buf) == 1 {
		title = "New task"
		body = buf[0].title
	} else {
		title = fmt.Sprintf("%d new tasks", len(buf))
		body = truncate(createdTasksSummary(buf), taskBodyMax)
	}

	s.pushTask(RouterNotification{
		Type:  taskNotificationType,
		Title: title,
		Body:  body,
		Tag:   "tasks-new", // a later burst REPLACES rather than stacks
	})
}

// createdTasksSummary joins the buffered task titles into a brief, comma-
// separated one-liner for the N>1 coalesced body.
func createdTasksSummary(buf []createdTaskRef) string {
	titles := make([]string, 0, len(buf))
	for _, t := range buf {
		if s := strings.TrimSpace(t.title); s != "" {
			titles = append(titles, s)
		}
	}
	if len(titles) == 0 {
		return "New tasks were created."
	}
	return strings.Join(titles, ", ")
}

// --- 2. Provisioning COMPLETE (task-linked agent reaches running, once) ---

// notifyAgentRunning fires the "agent ready" push when a TASK-LINKED agent first
// reaches `running`. It is called from BroadcastAgentChanged (the status-change
// hook), which only carries the agent's name, so it re-reads the agent to check
// the edge: status==running && NoteID!=nil && not-already-notified. The dedupe
// set fires it ONCE per agent id (best-effort in-memory) so a later status tick
// for the same agent does not re-push. A repo-less/no-task agent and the
// operator never match (NoteID==nil). Nil-push-safe.
func (s *Server) notifyAgentRunning(agentName string) {
	if s.router == nil || s.ext.Agents == nil || agentName == "" {
		return
	}
	// 🔴 authz.System, BECAUSE THIS HOOK HAS NO CONTEXT TO TAKE A PRINCIPAL
	// FROM. It is BroadcastAgentChanged's status-change hook, called by request
	// handlers and by the background loops alike with only a name. Under
	// row-level security a no-principal read sees nothing, and this push would
	// silently never fire. The row is used here only to decide whether to
	// notify; nothing read under System is returned to any caller. Listed in
	// the System ledger (internal/authz/ledger_test.go).
	a, err := s.ext.Agents.GetByName(authz.System(context.Background()), agentName)
	if err != nil {
		return // unknown agent (or DB blip) — best-effort, skip
	}
	if a.Status != agents.StatusRunning || a.NoteID == nil {
		return
	}

	// Dedupe: fire once per agent id.
	s.taskProvisionedMu.Lock()
	already := s.taskProvisionedNotified[a.ID]
	if !already {
		s.taskProvisionedNotified[a.ID] = true
	}
	s.taskProvisionedMu.Unlock()
	if already {
		return
	}

	noteID := *a.NoteID
	s.pushTask(RouterNotification{
		Type:  taskNotificationType,
		ID:    strconv.FormatInt(noteID, 10),
		Title: fmt.Sprintf("Task #%d: agent ready", noteID),
		Body:  "The agent is now working on it.",
		Tag:   fmt.Sprintf("task-%d-provisioned", noteID),
		Data:  map[string]string{"url": "/agents/" + a.Name},
	})
}

// --- 3. Task DONE (enters ready_for_review) ---

// notifyTaskDone fires the "ready for review" push when a task ENTERS
// ready_for_review, deduped so a no-op re-save of the same status does not
// re-push. A task moving to any OTHER status clears its dedupe mark, so a
// genuine re-entry (ready → in_progress → ready) notifies again. Called from
// every task status-set path (human select + agent self-service). Nil-push-safe.
func (s *Server) notifyTaskDone(n notes.Note) {
	if n.Status != notes.StatusReadyForReview {
		// Left (or never in) the review state: drop any dedupe mark so a future
		// transition back into ready_for_review notifies.
		s.taskDoneMu.Lock()
		delete(s.taskDoneNotified, n.ID)
		s.taskDoneMu.Unlock()
		return
	}
	if s.router == nil {
		return
	}

	s.taskDoneMu.Lock()
	already := s.taskDoneNotified[n.ID]
	if !already {
		s.taskDoneNotified[n.ID] = true
	}
	s.taskDoneMu.Unlock()
	if already {
		return
	}

	s.pushTask(RouterNotification{
		Type:  taskNotificationType,
		ID:    strconv.FormatInt(n.ID, 10),
		Title: fmt.Sprintf("Task #%d ready for review", n.ID),
		Body:  taskTitle(n),
		Tag:   fmt.Sprintf("task-%d-done", n.ID),
	})
}

// --- 4. External COMMENT on a task (machine endpoint only, coalesced) ---

// notifyTaskCommented buffers an EXTERNAL machine comment (POST
// /api/tasks/{id}/comments) and (re)arms its own debounce window so an agent
// posting a burst of progress notes buzzes the phone ONCE when the burst settles.
//
// Machine path ONLY: the session route (the human typing a comment) must not
// notify — same reasoning handleNoteCreate uses for the FAB create — and the
// per-agent route (/agent/task/comment) keeps its existing no-push behaviour.
// Best-effort + nil-push-safe.
func (s *Server) notifyTaskCommented(n notes.Note, c notes.Comment) {
	// Nothing to eventually deliver when push is disabled — skip the machinery.
	if s.router == nil {
		return
	}
	s.taskCommentMu.Lock()
	s.taskCommentBuf = append(s.taskCommentBuf, commentedTaskRef{
		noteID: n.ID,
		author: c.Author,
		// A comment body is markdown (a completion report: `**Done**`, a
		// `[PR](url)` link, backticked paths). A Web Push body is PLAIN TEXT on
		// every platform, so flatten it HERE — the phone would otherwise show
		// literal `**`/backticks (the 0.7.60 bug class).
		text: commentNotificationText(c.Body),
	})
	if s.taskCommentTimer == nil {
		s.taskCommentTimer = s.taskCommentAfter(taskCommentDebounce, s.flushCommentedTasks)
	} else {
		s.taskCommentTimer.Reset(taskCommentDebounce)
	}
	s.taskCommentMu.Unlock()
}

// flushCommentedTasks fires the single coalesced push for the comments buffered
// since the window opened, then clears the buffer + timer. One buffered comment →
// "New comment on task #N" + "author: text"; N>1 → "N new comments" + a brief
// summary. Tagged per-task when it's a single task (so a later comment REPLACES
// rather than stacks) and generically for a mixed burst.
func (s *Server) flushCommentedTasks() {
	s.taskCommentMu.Lock()
	buf := s.taskCommentBuf
	s.taskCommentBuf = nil
	s.taskCommentTimer = nil
	s.taskCommentMu.Unlock()

	if len(buf) == 0 {
		return
	}

	var title, body, tag, id string
	if len(buf) == 1 {
		c := buf[0]
		title = fmt.Sprintf("New comment on task #%d", c.noteID)
		body = truncate(strings.TrimSpace(c.author+": "+c.text), taskBodyMax)
		tag = fmt.Sprintf("task-%d-comment", c.noteID)
		id = strconv.FormatInt(c.noteID, 10)
	} else {
		title = fmt.Sprintf("%d new comments", len(buf))
		body = truncate(commentedTasksSummary(buf), taskBodyMax)
		tag = "tasks-comments"
	}

	s.pushTask(RouterNotification{
		Type:  taskNotificationType,
		ID:    id,
		Title: title,
		Body:  body,
		Tag:   tag,
	})
}

// commentedTasksSummary joins the buffered comments into a brief one-liner for
// the N>1 coalesced body ("repo-cos on #12, drafter on #14").
func commentedTasksSummary(buf []commentedTaskRef) string {
	parts := make([]string, 0, len(buf))
	for _, c := range buf {
		author := strings.TrimSpace(c.author)
		if author == "" {
			author = "api"
		}
		parts = append(parts, fmt.Sprintf("%s on #%d", author, c.noteID))
	}
	return strings.Join(parts, ", ")
}

// commentNotificationText flattens a markdown comment body to the plain text an
// OS notification renders: the first non-empty line, markdown-stripped via the
// shared notificationText. Falls back to a flattened, whitespace-collapsed form
// of the whole body when every line is markup-only, and to "" when there is
// nothing left to say.
func commentNotificationText(body string) string {
	for _, line := range strings.Split(body, "\n") {
		l := strings.TrimSpace(line)
		if l == "" {
			continue
		}
		if flat := notificationText(l); flat != "" {
			return truncate(flat, taskBodyMax)
		}
	}
	return ""
}

// taskTitle derives a short human title for a task NOTIFICATION: the first
// non-empty line of its body, falling back to its directory, truncated. Mirrors
// the notification-body truncation used by the permission pushes.
//
// NOT the same thing as ui.TaskTitle, despite the name — that one is the DISPLAY
// label (explicit `title`, falling back to `directory`). A push deliberately
// leads with the body because the heading is usually just `<host> · <date>`.
// Do not "consolidate" these two; they answer different questions.
func taskTitle(n notes.Note) string {
	for _, line := range strings.Split(n.Body, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			// Task bodies are markdown (the drafter/repo-cos post `**Goal:** …`,
			// backticked paths, `[text](url)` links). A Web Push body is PLAIN TEXT on
			// every platform — the OS never parses markdown — so flatten it here or the
			// phone shows literal `**`/backticks. Flatten BEFORE truncate so stripped
			// markers don't eat the length budget.
			return truncate(notificationText(s), taskBodyMax)
		}
	}
	if d := strings.TrimSpace(n.Directory); d != "" {
		return truncate(d, taskBodyMax)
	}
	return "New task"
}

var (
	ntLink   = regexp.MustCompile(`\[([^\]]+)\]\([^)]*\)`)             // [text](url) → text
	ntCode   = regexp.MustCompile("`([^`]+)`")                         // `code` → code
	ntEmph   = regexp.MustCompile(`(\*\*|__|\*|_)(.+?)(\*\*|__|\*|_)`) // **b**/_i_ → inner
	ntLead   = regexp.MustCompile(`^\s*(?:#{1,6}|>|[-*+]|\d+[.)])\s+`) // heading/quote/list marker
	ntImgBng = regexp.MustCompile(`!\[([^\]]*)\]\([^)]*\)`)            // ![alt](url) → alt
)

// notificationText flattens the inline markdown that appears in task bodies into
// plain text suitable for an OS notification (which renders no markup). It strips
// leading block markers, unwraps links/images/emphasis/inline-code, and collapses
// stray whitespace. Order matters: block marker → image → link → code → emphasis.
func notificationText(s string) string {
	s = ntLead.ReplaceAllString(s, "")
	s = ntImgBng.ReplaceAllString(s, "$1")
	s = ntLink.ReplaceAllString(s, "$1")
	s = ntCode.ReplaceAllString(s, "$1")
	// Emphasis can nest (***x***); a couple of passes settles the common cases.
	s = ntEmph.ReplaceAllString(s, "$2")
	s = ntEmph.ReplaceAllString(s, "$2")
	return strings.TrimSpace(s)
}
