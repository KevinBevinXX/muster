package api

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/textplain"
	"github.com/ZacxDev/muster/internal/ui"
)

// Task-lifecycle Web Push notifications. Each hangs off an existing
// producer/agent hook and goes to ONE owner — the task's (or the agent's, or
// the connector request's) — through goNotify, which delivers it to that
// owner's own devices (muster-native push) and, for an admin, to the router:
//
//  1. Task CREATED (machine/producer path only, POST /api/tasks) — coalesced: a
//     burst of creates (the drafter posts ~6 in a loop) collapses to ONE push.
//
//  2. Provisioning COMPLETE — a task-linked agent reaching `running`, once.
//
//  3. Task DONE — a task entering `ready_for_review`.
//
//  4. Task LEAVES review — a "resolved" control message that clears the
//     notification from (3) on the owner's other devices.
//
// All are best-effort and no-ops with no push configured, and none may ever
// fail the create/dispatch/status handler.
//
// 🔴 THE COALESCERS ARE PER OWNER. They used to be one global buffer, which was
// only safe because the router admin-gate sat in front of them: a coalesced
// "3 new tasks" push is ONE notification for every buffered task, so with two
// owners in one buffer, one user's task titles would land on the other's phone.
// ownerBuffer keys the buffer AND its debounce timer by owner.
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

// pushTask sends a task-lifecycle payload to owner, as event (the /account
// toggle that can mute it). It is the single choke point every task
// notification uses, and goNotify runs the fan-out in its own goroutine so push
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
// ownerBuffer is a debounce buffer PER OWNER: each owner's burst collapses into
// that owner's one notification, on that owner's own timer.
type ownerBuffer[T any] struct {
	mu     sync.Mutex
	bufs   map[int64][]T
	timers map[int64]resettableTimer
}

// add buffers v for owner and (re)arms owner's debounce timer, which calls
// flush(owner) once the burst settles.
func (b *ownerBuffer[T]) add(owner int64, v T, after afterFunc, d time.Duration, flush func(int64)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.bufs == nil {
		b.bufs, b.timers = map[int64][]T{}, map[int64]resettableTimer{}
	}
	b.bufs[owner] = append(b.bufs[owner], v)
	if t, ok := b.timers[owner]; ok {
		t.Reset(d)
		return
	}
	b.timers[owner] = after(d, func() { flush(owner) })
}

// take empties owner's buffer and forgets its timer.
func (b *ownerBuffer[T]) take(owner int64) []T {
	b.mu.Lock()
	defer b.mu.Unlock()
	v := b.bufs[owner]
	delete(b.bufs, owner)
	delete(b.timers, owner)
	return v
}

// routerServes reports whether an event owned by owner may reach the router —
// a push to the operator's phone or a mirror onto the router's bus.
//
// 🔴 ONLY AN ADMIN'S (accounts PR 3b). The router is the OPERATOR's service:
// its push subscriptions are the operator's devices and its bus the operator's
// page, with no notion of a muster user. A member's task title, comment or
// connector request pushed there would land on the operator's phone — the same
// cross-user leak the SSE owner filter closes, through a different door. The
// approval relay draws the same line (operator decision: only admin-owned
// agents relay to the router). A member's notifications go to the member's
// OWN devices through muster-native push (goNotify), never to the router's.
// roleOf fails to member, so an unreadable role reaches no router.
func (s *Server) routerServes(owner int64) bool {
	return owner > 0 && s.roleOf(context.Background(), owner) == authz.RoleAdmin
}

func (s *Server) pushTask(owner int64, event string, payload RouterNotification) {
	if !s.notifiable() {
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
	s.goNotify(owner, event, payload, func(n int) {
		s.logger.Printf("push: delivered task notification tag=%s owner=%d to %d device(s)", payload.Tag, owner, n)
	})
}

// --- 1. Task CREATED (coalesced) ---

// notifyTaskCreated buffers a machine-created task and (re)arms the debounce
// timer so a burst of creates collapses into ONE push once it settles. Only the
// producer path (POST /api/tasks) calls this — the FAB create does not, since
// the user just made that task themselves. Best-effort + nil-push-safe.
func (s *Server) notifyTaskCreated(n notes.Note) {
	// Skip the buffer+timer machinery entirely when nothing can be delivered.
	// The buffer is the task OWNER's: the coalesced push is one notification for
	// every buffered task, so owners must never share one.
	if !s.notifiable() || n.OwnerID <= 0 {
		return
	}
	s.createdBuf.add(n.OwnerID, createdTaskRef{id: n.ID, title: taskTitle(n)}, s.taskCreateAfter,
		taskCreateDebounce, s.flushCreatedTasks)
}

// flushCreatedTasks fires owner's single coalesced push for the tasks buffered
// since owner's window opened, then clears that buffer + timer. It is the debounce timer's
// callback (and is invoked directly by tests via the injected timer). A single
// buffered task → "New task" + its title; N>1 → "N new tasks" + a brief summary.
func (s *Server) flushCreatedTasks(owner int64) {
	buf := s.createdBuf.take(owner)

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

	s.pushTask(owner, push.EventCreated, RouterNotification{
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
	if !s.notifiable() || s.ext.Agents == nil || agentName == "" {
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
	s.pushTask(a.OwnerID, push.EventAgent, RouterNotification{
		Type:  taskNotificationType,
		ID:    strconv.FormatInt(noteID, 10),
		Title: fmt.Sprintf("Task #%d: agent ready", noteID),
		Body:  "The agent is now working on it.",
		Tag:   fmt.Sprintf("task-%d-provisioned", noteID),
		// The BARE page, on purpose (#237): it resolves the agent's latest session
		// at TAP time, which is what a notification opened later must land on.
		Data: map[string]string{"url": ui.AgentPath(a.Name)},
	})
}

// --- 3. Task DONE (enters ready_for_review) ---

// notifyTaskDone fires the "ready for review" push when a task ENTERS
// ready_for_review, deduped so a no-op re-save of the same status does not
// re-push. A task moving to any OTHER status clears its dedupe mark, so a
// genuine re-entry (ready → in_progress → ready) notifies again. Called from
// every task status-set path (human select + agent self-service). Nil-push-safe.
//
// LEAVING review sends ONE "resolved" control message (event resolved) that
// closes the ready-for-review notification on the owner's devices — the
// Android icon dot is the unread-notification count, so this is what clears
// it. It fires only for a task this process actually notified (the dedupe
// mark), so a task that never buzzed sends nothing, and a repeated save of
// the new status sends nothing more.
func (s *Server) notifyTaskDone(n notes.Note) {
	if n.Status != notes.StatusReadyForReview {
		// Left (or never in) the review state: drop any dedupe mark so a future
		// transition back into ready_for_review notifies.
		s.taskDoneMu.Lock()
		was := s.taskDoneNotified[n.ID]
		delete(s.taskDoneNotified, n.ID)
		s.taskDoneMu.Unlock()
		if was {
			s.goNotify(n.OwnerID, push.EventResolved, RouterNotification{
				Type: push.TypeResolved,
				ID:   strconv.FormatInt(n.ID, 10),
				Tag:  fmt.Sprintf("task-%d-done", n.ID),
			}, nil)
		}
		return
	}
	if !s.notifiable() || n.OwnerID <= 0 {
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

	s.pushTask(n.OwnerID, push.EventReview, RouterNotification{
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
	if !s.notifiable() || n.OwnerID <= 0 {
		return
	}
	s.commentBuf.add(n.OwnerID, commentedTaskRef{
		noteID: n.ID,
		author: c.Author,
		// A comment body is markdown (a completion report: `**Done**`, a
		// `[PR](url)` link, backticked paths). A Web Push body is PLAIN TEXT on
		// every platform, so flatten it HERE — the phone would otherwise show
		// literal `**`/backticks (the 0.7.60 bug class).
		text: commentNotificationText(c.Body),
	}, s.taskCommentAfter, taskCommentDebounce, s.flushCommentedTasks)
}

// flushCommentedTasks fires the single coalesced push for the comments buffered
// since the window opened, then clears the buffer + timer. One buffered comment →
// "New comment on task #N" + "author: text"; N>1 → "N new comments" + a brief
// summary. Tagged per-task when it's a single task (so a later comment REPLACES
// rather than stacks) and generically for a mixed burst.
func (s *Server) flushCommentedTasks(owner int64) {
	buf := s.commentBuf.take(owner)

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

	s.pushTask(owner, push.EventComment, RouterNotification{
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
// OS notification renders: the first non-empty line, markdown-stripped by
// textplain.Title, the one title rule. Falls back to a flattened, whitespace-collapsed form
// of the whole body when every line is markup-only, and to "" when there is
// nothing left to say.
func commentNotificationText(body string) string {
	for _, line := range strings.Split(body, "\n") {
		l := strings.TrimSpace(line)
		if l == "" {
			continue
		}
		if flat := textplain.Title(l); flat != "" {
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
		// A line that is all markup (a fence, a rule) flattens to nothing and is
		// skipped, as commentNotificationText skips it.
		if s := textplain.Title(line); s != "" {
			// Task bodies are markdown (the drafter/repo-cos post `**Goal:** …`,
			// backticked paths, `[text](url)` links). A Web Push body is PLAIN TEXT on
			// every platform — the OS never parses markdown — so flatten it here or the
			// phone shows literal `**`/backticks. Flatten BEFORE truncate so stripped
			// markers don't eat the length budget.
			return truncate(s, taskBodyMax)
		}
	}
	if d := strings.TrimSpace(n.Directory); d != "" {
		return truncate(d, taskBodyMax)
	}
	return "New task"
}
