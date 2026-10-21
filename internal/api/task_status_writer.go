package api

import (
	"context"

	"github.com/ZacxDev/muster/internal/notes"
)

// ---------------------------------------------------------------------------
// WHO writes a task status.
//
// The choke point that LOGS the write lives in internal/notes
// (notes.WithStatusLogging); this is the seam that tells it who is calling. The
// two halves are separate on purpose: the decorator sees EVERY writer including
// one in a package that never heard of this file, and this side makes forgetting
// to identify yourself a compile-time impossibility rather than a review note.
//
// 🔴 THE LABEL IS A REQUIRED ARGUMENT, NOT A CONVENTION. setNoteStatus takes a
// statusWriter positionally, so a new call site cannot be written without
// naming one. The weaker design — stamp the context at each HTTP entry point and
// hope — is exactly the shape that produced the incident: at the time, two of the
// eight writers WERE native function tools with no route and no request log, so
// "whoever forgot" was invisible. Here the compiler asks. (⚠ Past tense, and the
// numbers have moved: upstream task #653 phase two deleted writerOperatorTool, so
// there are seven writers today and ONE of them is a native tool. The argument is
// unchanged; the arithmetic is not, which is why it is dated rather than restated
// as a standing fact.)
//
// Pair this with task_status_ledger_test.go, which pins that internal/api
// contains EXACTLY ONE call to Notes.SetStatus (inside setNoteStatus) and pins
// the set of setNoteStatus callers with the label each passes. A writer neither
// ledger has seen fails one of the two, whichever way it is written.
// ---------------------------------------------------------------------------

// statusWriter names the path performing a task-status write. It is a named
// type, not a bare string, so a caller cannot pass an arbitrary message and call
// it a writer identity.
type statusWriter string

const (
	// writerAgentRoute is PATCH /agent/task/status — the in-devpod agent's HTTP
	// route (and what `muster agent task status` reaches, PR #430).
	writerAgentRoute statusWriter = "agent-http-route"
	// writerAgentTool is the NATIVE function tool `agent_set_task_status`. It has
	// no route, so it produces no request log line — one of the two paths the
	// incident could not distinguish, and the reason this whole seam exists. It is
	// still registered (AgentToolDefsFor) and agentSystemPrompt still instructs the
	// worker to call it, so #430/#431 did NOT retire it.
	writerAgentTool statusWriter = "agent_set_task_status"
	// writerDispatchAdvance is the open→in_progress advance after a dispatch.
	writerDispatchAdvance statusWriter = "dispatch-advance"
	// writerTaskMerge retires the LOSER of a task merge to complete.
	writerTaskMerge statusWriter = "task-merge"
	// writerHumanUI is PATCH /tasks/{id}/status — the session/human card control.
	writerHumanUI statusWriter = "human-ui"
	// writerMachineAPI is PATCH /api/tasks/{id}/status — the hook-token machine
	// route (what `muster task status` reaches).
	writerMachineAPI statusWriter = "machine-api"
	// writerTaskTool is the all-agents tool `task_set_status` (attention_tools.go):
	// one of the calling agent's own tasks, by id, over the gateway or MCP.
	writerTaskTool statusWriter = "task_set_status"
	// writerDestroyReopen is the in_progress→open step after a mirrored
	// session's agent_destroy tore down the last live agent on a task
	// (agent_destroy_tool.go), so the chief can task_dispatch it again.
	writerDestroyReopen statusWriter = "agent-destroy-reopen"
	// 🔴 TWO LABELS WERE DELETED HERE (upstream task #653 phase two), and the
	// deletion-tracking note matters more than usual because these are LOG VALUES:
	//
	//	writerOperatorRoute = "operator-http-route"      PATCH /operator/tasks/{id}/status
	//	writerOperatorTool  = "operator_set_task_status" the native operator tool
	//
	// Both call sites went with internal/api/operator.go, so internal/api now has
	// SEVEN status writers, not nine. Historic rows in the status log still carry
	// these two strings — a reader of `writer=` must not take their absence here as
	// evidence such a write never happened; it means no CURRENT code path produces
	// one. 🔴 DO NOT REUSE EITHER STRING for a new writer: an old row and a new row
	// would then be indistinguishable, which is the exact confusion this seam exists
	// to prevent.
)

// setNoteStatus is the ONE place in internal/api that calls Notes.SetStatus. It
// stamps writer onto the context so the store-side choke point can name the
// author of the write, then delegates.
//
// It returns the store's note and error unchanged. Callers keep their own
// validation, side effects and error mapping — this deliberately does not become
// a second place where status POLICY lives, because StatusAllowedForAgent
// (agents may not set `complete`) is a separate, deliberate rule that differs per
// caller and must not be flattened here.
//
// 🔴 ONE SIDE EFFECT LIVES HERE, AND ONLY ONE: a write that LANDED with status
// `complete` stops the agent the chief started for the task
// (stopChiefAgentOnComplete). It is here, not in the callers, because this is
// the funnel every writer passes — four of them can set `complete` — and a copy
// per caller is a place for the next complete-writer to forget it. A failed
// write stops nothing. TestTheCompleteStopIsOnTheStatusFunnel pins the call here.
//
// An empty writer is not silently accepted-as-fine: notes.WithWriter refuses to
// store it, so the write logs `writer=UNLABELLED` rather than an empty field.
//
// 🔴 BUT NOTHING IN THE TYPE SYSTEM STOPS A CALLER PASSING ONE. `statusWriter`
// is a defined string type, so `s.setNoteStatus(ctx, "", id, …)` compiles — this
// was measured, not assumed. The guard that catches it is
// TestEveryStatusWriterIsLabelled's caller ledger, not the compiler; see the
// UnlabelledWriter comment in internal/notes/statuslog.go, which used to claim
// the opposite and is now consistent with this.
func (s *Server) setNoteStatus(ctx context.Context, writer statusWriter, id int64, status string) (notes.Note, error) {
	n, err := s.ext.Notes.SetStatus(notes.WithWriter(ctx, string(writer)), id, status)
	if err == nil && status == notes.StatusComplete {
		s.stopChiefAgentOnComplete(ctx, n)
	}
	return n, err
}
