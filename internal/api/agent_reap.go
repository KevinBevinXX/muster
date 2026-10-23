package api

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
)

// ---------------------------------------------------------------------------
// THE STOPPED-AGENT REAPER: an agent whose stored status has been `stopped` for
// MUSTER_AGENT_REAP_STOPPED_AFTER (default 14 days) is destroyed — whatever the
// provisioner's Destroy removes for its instance, then its row — through
// destroyAgent, the teardown the Agents page's Delete and the agent_destroy
// tool take.
//
// It rides the idle-task reaper's loop (RunTaskReap, every TaskReapInterval),
// and like it runs on every pod. What keeps two pods from destroying one agent
// twice is the claim, not a leader: each due agent is claimed by one
// conditional UPDATE (agents.PGStore.ClaimStoppedReap) after destroyAgent's
// chief check and before anything is torn down, and only the winner starts the
// teardown.
//
// What is never reaped:
//   - the chief: destroyAgent refuses it (agentIsChief) before the claim;
//   - any agent whose stored status is not `stopped` — `error` included;
//   - a pool worker (worker_pool set);
//   - an agent that never ran: saved for later and never started (no
//     first_started_at, migration 0045);
//   - an agent Started since the scan read it: the claim re-checks the status
//     and the window. A Stop after a Start stamps a new stopped_at (migration
//     0045's trigger), so a restarted agent that is stopped again waits a
//     fresh window.
//
// ⚠ KNOWN RESIDUAL: Start brings the instance up BEFORE it writes
// `provisioning` (agentprovision.Adapter.Start). A Start in flight in that gap
// still reads `stopped`, so a claim landing in it wins and the teardown removes
// what the Start brought up. The same race exists between the Agents page's
// Start and Delete today.
//
// The record of a reap is the server log: one line per agent whose teardown
// was started, naming the agent, its owner, its task and when it stopped. The
// teardown's own failure is logged by destroyAgent, and Adapter.Destroy keeps
// the row with status `error` when the instance could not be removed, so such
// an agent is not retried.
// ---------------------------------------------------------------------------

// DefaultAgentReapStoppedAfter is how long an agent may stay stopped before the
// reaper destroys it, when MUSTER_AGENT_REAP_STOPPED_AFTER is unset.
const DefaultAgentReapStoppedAfter = 14 * 24 * time.Hour

// stoppedReapStore is the agents store's reaper half. *agents.PGStore is the
// one implementation; a store without it (a test fake) reaps nothing.
type stoppedReapStore interface {
	ListReapableStopped(ctx context.Context, after time.Duration) ([]agents.StoppedAgent, error)
	ClaimStoppedReap(ctx context.Context, id int64, after time.Duration) (bool, error)
}

var _ stoppedReapStore = (*agents.PGStore)(nil)

// SetAgentReapStoppedAfter sets how long an agent may stay stopped before the
// reaper destroys it. Zero or less turns the reaper off.
func (s *Server) SetAgentReapStoppedAfter(d time.Duration) { s.agentReapStoppedAfter = d }

// reapPrincipal is ctx acting as owner, through authz.ViaReap: how the reaper
// claims and destroys one owner's agent, after reading the due rows under
// System. Acting as the owner is what makes the teardown the owner's own
// Delete: the chief check reads the owner's agents, the provisioner acts on the
// owner's row, and the chief-start release (releaseChiefStart) is recorded for
// the owner. internal/authz's withPrincipalLedger lists it.
func (s *Server) reapPrincipal(ctx context.Context, owner int64) context.Context {
	return authz.WithPrincipal(ctx, s.principalFor(ctx, owner, authz.ViaReap))
}

// reapStoppedAgents is one pass of the stopped-agent reaper, under ctx's System
// principal. It returns how many teardowns it started.
func (s *Server) reapStoppedAgents(ctx context.Context) int {
	after := s.agentReapStoppedAfter
	if after <= 0 || s.ext.Agents == nil || s.ext.Provisioner == nil {
		return 0
	}
	st, ok := s.ext.Agents.(stoppedReapStore)
	if !ok {
		return 0
	}
	due, err := st.ListReapableStopped(ctx, after)
	if err != nil {
		s.logger.Printf("agent-reap: list stopped agents: %v", err)
		return 0
	}
	started := 0
	for _, c := range due {
		c := c
		octx := s.reapPrincipal(ctx, c.OwnerID)
		// The provisioner is reached in THIS function's closure: the nil check
		// above is its barrier (provisioner_seam_test.go).
		destroy := func(ctx context.Context, id int64) error { return s.ext.Provisioner.Destroy(ctx, id) }
		claim := func(ctx context.Context) (bool, error) { return st.ClaimStoppedReap(ctx, c.ID, after) }
		var chief errDestroyChief
		switch _, err := s.destroyAgent(octx, c.ID, destroy, destroyHooks{claim: claim}); {
		case err == nil:
		case errors.As(err, &chief), errors.Is(err, errDestroyNotClaimed):
			// The chief stays (silently: it would be refused again on every
			// pass), and a lost claim is another pod's reap or a Start.
			continue
		default:
			s.logger.Printf("agent-reap: agent %d (%s): nothing was destroyed: %v", c.ID, c.Name, err)
			continue
		}
		task := "none"
		if c.NoteID != nil {
			task = strconv.FormatInt(*c.NoteID, 10)
		}
		s.logger.Printf("agent-reap: destroying agent %d (%s) of user %d, task %s: stopped since %s, longer than %s",
			c.ID, c.Name, c.OwnerID, task, c.StoppedAt.UTC().Format(time.RFC3339), after)
		started++
	}
	return started
}
