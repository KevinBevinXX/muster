package api

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/notes"
)

// ---------------------------------------------------------------------------
// A TASK BECOMING COMPLETE STOPS THE AGENT THE AGENT STARTED FOR IT.
//
// Operator decision: when a task's status becomes `complete`, muster STOPS (not
// destroys) the agent the agent started for that task, immediately — a turn it
// is running is cut off, because Stop scales its instance to zero. Agents the
// owner started by hand are left alone, and `ready_for_review` stops nothing: an
// agent awaiting review must stay reachable through task_forward.
//
// WHY: the agent's live-agent cap (plugins.MaxLiveDispatchedAgents) counts the STORED
// status of the agents its starts made. An idle agent on a completed task still
// read `running`, held a slot, and blocked a dispatch the owner had approved. A
// stopped agent is not live for the cap, so stopping it frees the slot while
// the cap still equals what is actually running.
//
// What does NOT change: the agent's one start on the task stays SPENT. A stop
// writes no release (plugins.ReleaseTaskDispatch is written only by the
// owner's destroy), so task_dispatch on the task still answers
// already_dispatched, and Start brings the agent back.
//
// 🔴 IT RUNS FROM setNoteStatus, THE ONE PLACE IN THIS PACKAGE A TASK STATUS IS
// WRITTEN (task_status_ledger_test.go pins that), so no writer of `complete` can
// bypass it. TestTheCompleteStopIsOnTheStatusFunnel pins the call's position,
// and TestEveryCompleteWriterStopsTheChiefsAgent drives each writer that can
// set `complete` through its own handler.
// ---------------------------------------------------------------------------

// stopDispatchedAgentOnComplete runs once per transition of task n INTO complete
// (setNoteStatus decides that). It stops the agent the agent's latest,
// unreleased start on task n made (plugins.DispatchStore.DispatchStartedAgent), when that agent
// belongs to n's owner, is bound to n, and its stored status is live
// (agentLive — exactly the statuses the cap counts). Anything else is a no-op:
// no agent start, an agent the owner started by hand, one already stopped or in
// error, or a deployment with no ledger or no provisioner.
//
// The lookup runs synchronously, as the writer's principal (row-level security
// scopes the ledger and the agent row to the task's owner); the Stop runs in the
// background, detached from the request's cancellation but not from its
// principal, exactly as the Agents page's Stop does (agentLifecycle). Best
// effort: a failure is logged and the status write it follows stands.
func (s *Server) stopDispatchedAgentOnComplete(ctx context.Context, n notes.Note) {
	// The provisioner barrier (provisioner_seam_test.go): this body calls the
	// provisioner, so it nil-checks it.
	if s.ext.TaskDispatch == nil || s.ext.Provisioner == nil || s.ext.Agents == nil {
		return
	}
	agentID, ok, err := s.ext.TaskDispatch.DispatchStartedAgent(ctx, n.ID)
	if err != nil {
		s.logger.Printf("tasks: complete %d: read the agent's start: %v", n.ID, err)
		return
	}
	if !ok {
		return
	}
	a, err := s.ext.Agents.Get(ctx, agentID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return // destroyed already
	case err != nil:
		s.logger.Printf("tasks: complete %d: read agent %d: %v", n.ID, agentID, err)
		return
	}
	// The ledger names the agent; the row must agree it is this owner's agent on
	// this task before anything is stopped.
	if a.OwnerID != n.OwnerID || a.NoteID == nil || *a.NoteID != n.ID || !agentLive(a.Status) {
		return
	}
	opCtx := context.WithoutCancel(ctx)
	safeGo(s.logger, "stop on complete", func() {
		if err := s.ext.Provisioner.Stop(opCtx, a.ID); err != nil {
			s.logger.Printf("tasks: complete %d: stop the dispatched agent %d (%s): %v", n.ID, a.ID, a.Name, err)
			return
		}
		s.logger.Printf("tasks: complete %d: stopped the dispatched agent %d (%s)", n.ID, a.ID, a.Name)
		s.clearAgentWork(a.ID)
		s.agentChangedFor(a.OwnerID, a.Name)
	})
}
