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
// It rides the idle-task reaper's loop (RunTaskReap, every TaskReapInterval).
// That loop consults a leader gate (leadsTaskLoops), but cmd/muster-server
// installs none (SetTaskLeaderGate has no caller there; see its
// startBackgroundLoops and doc_seams.go item 4), and a nil gate runs the body
// on every pod. So it runs on every pod, and what keeps two pods from
// destroying one agent twice is the claim, not a leader: each due agent is claimed by one
// conditional UPDATE (agents.PGStore.ClaimStoppedReap) after destroyAgent's
// agent check and before anything is torn down, and only the winner starts the
// teardown.
//
// What is never reaped:
//   - the agent: destroyAgent refuses it (agentIsChief) before the claim;
//   - any agent whose stored status is not `stopped` — `error` included;
//   - a pool worker (worker_pool set);
//   - an agent that never ran: saved for later and never started (no
//     first_started_at, migration 0045);
//   - an agent Started since the scan read it: the claim re-checks the status
//     and the window. A Stop after a Start stamps a new stopped_at (migration
//     0045's trigger), so a restarted agent that is stopped again waits a
//     fresh window;
//   - an agent whose INSTANCE is not stopped, whatever its row says: the pass
//     reads the provisioner's instances first and skips (and logs) any due
//     agent whose instance reads running, provisioning or error; if the
//     instances cannot be read, it destroys nothing.
//
// ⚠ KNOWN RESIDUAL: Start brings the instance up BEFORE it writes
// `provisioning` (agentprovision.Adapter.Start). A Start in flight in that gap,
// whose instance the pass's read did not yet see as up, still reads `stopped`,
// so a claim landing in it wins and the teardown removes what the Start brought
// up. The same race exists between the Agents page's Start and Delete today.
//
// The record of a reap is the server log: one line per agent whose teardown
// was started, naming the agent, its owner, its task and when it stopped. The
// teardown's own failure is logged by destroyAgent, and Adapter.Destroy keeps
// the row with status `error` when the instance could not be removed, so such
// an agent is not retried.
// ---------------------------------------------------------------------------

// defaultAgentReapInstancesWithin bounds the reaper's read of the
// provisioner's instances. It runs in a background loop, not behind a page, so
// it can wait longer than a render does; it only has to end.
const defaultAgentReapInstancesWithin = 30 * time.Second

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
// Delete: the agent check reads the owner's agents, the provisioner acts on the
// owner's row, and the agent-start release (releaseDispatchStart) is recorded for
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
	if len(due) == 0 {
		return 0
	}
	// 🔴 THE STORED STATUS IS CHECKED AGAINST THE BACKEND BEFORE ANYTHING IS
	// CLAIMED. A stored `stopped` is not proof the instance is down: Start brings
	// the instance up BEFORE it writes `provisioning`, so a Start whose status
	// write failed leaves a running instance under a `stopped` row that nothing
	// corrects. An agent whose instance reads as anything but stopped
	// (liveStatusIndexed, the one composer the cards use) is skipped and logged.
	// FAILS CLOSED: when the instances cannot be read (instanceIndex answers
	// nil, "not observed"), the whole pass destroys nothing.
	//
	// 🔴 THE READ IS BOUNDED (s.reapInstancesWithin). The Kubernetes
	// client carries no timeout of its own, so an unbounded List that stalls
	// would hang this pass, and with it the idle-task reaper sharing the loop.
	// Past the bound instanceIndexWithin answers nil, which is the same
	// fail-closed path as an error.
	idx := s.instanceIndexWithin(ctx, s.reapInstancesWithin)
	if idx == nil {
		s.logger.Printf("agent-reap: live instances could not be read; destroying nothing this pass (%d due)", len(due))
		return 0
	}
	started := 0
	for _, c := range due {
		c := c
		if live := liveStatusIndexed(agents.Agent{Name: c.Name, Status: agents.StatusStopped}, idx); live != agents.StatusStopped {
			s.logger.Printf("agent-reap: agent %d (%s) is stored stopped but its instance reads %s; not reaped", c.ID, c.Name, live)
			continue
		}
		octx := s.reapPrincipal(ctx, c.OwnerID)
		// The provisioner is reached in THIS function's closure: the nil check
		// above is its barrier (provisioner_seam_test.go).
		destroy := func(ctx context.Context, id int64) error { return s.ext.Provisioner.Destroy(ctx, id) }
		claim := func(ctx context.Context) (bool, error) { return st.ClaimStoppedReap(ctx, c.ID, after) }
		hooks := destroyHooks{claim: claim}
		if signal := s.agentReapDone; signal != nil {
			// Destroy's failure is signalled here; a success after the shared
			// after-steps, through done.
			destroy = func(ctx context.Context, id int64) error {
				err := s.ext.Provisioner.Destroy(ctx, id)
				if err != nil {
					signal(id, err)
				}
				return err
			}
			hooks.done = func(context.Context, agents.Agent) { signal(c.ID, nil) }
		}
		switch _, err := s.destroyAgent(octx, c.ID, destroy, hooks); {
		case err == nil:
		case errors.Is(err, errDestroyNotClaimed):
			// A lost claim is another pod's reap or a Start. (No agent is exempt
			// from the reaper any more: the labelled `agent` used to be skipped
			// silently here, because the destroy would refuse it on every pass.)
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
