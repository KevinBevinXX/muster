package api

import (
	"context"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

// ---------------------------------------------------------------------------
// ONE PLACE THE DISPLAYED AGENT STATUS IS DECIDED.
//
// 🔴 THIS REPLACES THE SAME FOUR LINES OPEN-CODED AT SIX CALL SITES, AND
// CONSOLIDATING THEM IS WHAT MADE A LATENT DEFECT AUDIBLE RATHER THAN JUST
// TIDY. Each site read:
//
//	status := a.Status
//	if s.ext.Provisioner != nil {
//	    if pods, err := s.ext.Provisioner.Pods(ctx); err == nil {
//	        status = agents.ComputeStatus(a, agents.PodIndex(pods)[a.Namespace])
//	    }
//	}
//
// — keyed on a.Namespace, because upstream's index was keyed on the pod's
// Kubernetes namespace. muster's index is keyed on the instance's Ref.Name,
// because [provision.Instance] states Group is EMPTY for a driver with no
// namespacing concept, so a group-keyed index collapses every instance onto the
// key "" and the last one wins. A namespace-keyed lookup against a name-keyed
// index does not fail: it returns nil, ComputeStatus takes its nil-instance
// branch, and every agent renders "stopped" while running perfectly. Silent, on
// every surface, with nothing logged.
//
// Six copies is six chances to fix five of them. One function is one place the
// key is spelled, and liveStatus is the only caller of InstanceIndex in this
// package — TestLiveStatusIsTheOnlyStatusComposer asserts that.
//
// ⚠ A FAILED INSTANCE READ FALLS BACK TO THE STORED STATUS RATHER THAN
// ERRORING, WHICH IS THE PRE-EXISTING BEHAVIOUR AND IS KEPT DELIBERATELY. The
// stored status is stale but not wrong — it is what this agent last was — and
// the alternative on a provisioner blip is a 500 on the whole board. This is a
// different judgement from the session-liveness probe next door, which DOES
// propagate its error, and the difference is which way the wrong answer reads:
// a stale agent status self-corrects on the next poll, while a false
// "no transcript recorded" is permanent-looking and was read as retention.
// ---------------------------------------------------------------------------

// instanceIndex returns the live instance index keyed by agent name, or nil
// when there is no provisioner or the read failed.
//
// A nil map is a usable map for reads, so callers may index it directly — but
// they must distinguish nil from empty before deciding whether live state was
// OBSERVED. See liveStatusIndexed.
func (s *Server) instanceIndex(ctx context.Context) map[string]*provision.Instance {
	if s.ext.Provisioner == nil {
		return nil
	}
	return s.indexOf(s.ext.Provisioner.Instances(ctx))
}

// indexOf is an Instances read as the index liveStatusIndexed takes: nil (not
// observed) on an error, which is logged. The ONE place the index is keyed.
func (s *Server) indexOf(insts []provision.Instance, err error) map[string]*provision.Instance {
	if err != nil {
		s.logger.Printf("agents: list instances: %v", err)
		return nil
	}
	return agents.InstanceIndex(insts)
}

// instanceIndexWithin is instanceIndex, given up after within (0 = never): a
// read still running then answers nil — "live state was not observed", the
// stored status — and is left to finish on its own. The wait does not rely on
// the provisioner honouring ctx: a Kubernetes client with no timeout of its own
// does not, so the bound is a select, not only a deadline on ctx.
func (s *Server) instanceIndexWithin(ctx context.Context, within time.Duration) map[string]*provision.Instance {
	if within <= 0 || s.ext.Provisioner == nil {
		return s.instanceIndex(ctx)
	}
	// The provisioner is read HERE, not in the goroutine: a read left running
	// past the bound must not touch the Server after this call returns.
	p := s.ext.Provisioner
	ctx, cancel := context.WithTimeout(ctx, within)
	type answer struct {
		insts []provision.Instance
		err   error
	}
	done := make(chan answer, 1)
	safeGo(s.logger, "agents: bounded instance read", func() {
		defer cancel()
		insts, err := p.Instances(ctx)
		done <- answer{insts, err}
	})
	select {
	case a := <-done:
		return s.indexOf(a.insts, a.err)
	case <-ctx.Done():
		s.logger.Printf("agents: list instances: no answer within %s; drawing stored status", within)
		return nil
	}
}

// liveStatusIndexed composes one agent's displayed status against an index the
// caller already fetched. Use it inside a loop; one read for N agents.
//
// 🔴 A nil INDEX MEANS "LIVE STATE WAS NOT OBSERVED" AND FALLS BACK TO THE
// STORED STATUS. An EMPTY (non-nil) index means "observed, and this agent is
// not running", which ComputeStatus resolves to stopped. Those are different
// answers and the nil check is what keeps them apart — collapsing them would
// report every agent stopped whenever the provisioner was briefly unreachable.
func liveStatusIndexed(a agents.Agent, idx map[string]*provision.Instance) string {
	if idx == nil {
		return a.Status
	}
	return agents.ComputeStatus(a, idx[a.Name])
}

// liveStatus composes one agent's displayed status, fetching live state itself.
// Use it on a single-agent path; do NOT use it in a loop.
func (s *Server) liveStatus(ctx context.Context, a agents.Agent) string {
	return liveStatusIndexed(a, s.instanceIndex(ctx))
}
