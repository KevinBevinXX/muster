// Package agentstatus keeps an agent's STORED status following its instance: it
// promotes a row stored as `provisioning` to `running` once the instance behind
// it is live and ready.
//
// 🔴 WHY IT EXISTS: NOTHING ELSE EVER WROTE `running` FOR A FRESH AGENT. Every
// write path stores `provisioning` and stops there:
//
//   - agentprovision.Adapter.Dispatch creates the instance and stores
//     `provisioning`;
//   - agentprovision.Adapter.Start stores `provisioning` for an agent that was
//     never kicked off (a saved agent, a claude-code agent, a restart before the
//     first turn) and returns;
//   - internal/agentkickoff delivers the first turn and stamps kicked_off, but
//     never writes a status — it deliberately leaves agents.ActionPersistStatus
//     untaken (its Tick says so);
//   - and agents.DecideReconcile's ActionPersistStatus, the table entry that
//     would have done it, had no driver at all (reconcile.go's footer named it).
//
// The HTML views never showed it, because they refine the stored status against
// the live instance on every render (agents.ComputeStatus via internal/api's
// liveStatus). Every reader of the STORED column did: GET /api/agents and
// `muster agent ls` reported `provisioning` for agents that had been Ready for
// hours, the claude-code account pool counted them as live, and the "agent
// ready" push (api.Server.notifyAgentRunning, which re-reads the stored status)
// never fired for a task-linked agent.
//
// 🔴 IT PROMOTES AND NOTHING ELSE. The one transition is the one the stored
// status was missing, decided by agents.PromoteToRunning. Demotions
// (running → provisioning/error/stopped) are NOT taken here: the stuck and
// lost-kickoff verdicts belong to agentkickoff and the decision table, and Stop
// writes `stopped` itself.
//
// 🔴 IT IS CONDITIONAL ON THE ROW NOT HAVING MOVED (agents.Store.MarkRunning
// compares status AND updated_at). The verdict comes from a row list and a
// separate instance list; a Stop, a Start or an error written in between must win.
//
// ⚠ ITS OWN LOOP, NOT A PHASE OF agentkickoff, ON PURPOSE: the deliverer only
// exists when a gateway is configured (cmd/muster-server's buildKickoffDeliverer),
// and an agent's status is true or false regardless of whether anything can
// deliver it a first turn.
package agentstatus

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

// DefaultInterval is how often [Promoter.Run] looks. A pod takes tens of seconds
// to become ready; this bounds how long after that the stored status lags.
const DefaultInterval = 10 * time.Second

// InstanceLister lists live instances. agentprovision.Adapter satisfies it.
type InstanceLister interface {
	Instances(ctx context.Context) ([]provision.Instance, error)
}

// Config is everything the promoter needs.
type Config struct {
	// Store is the agents store. REQUIRED.
	Store agents.Store
	// Instances lists live instances. REQUIRED.
	Instances InstanceLister
	// NamespacePrefix is the deployment's configured agent namespace prefix; a
	// row is this deployment's iff its namespace is agents.NamespaceFor(prefix,
	// name). Rows another system wrote are never touched.
	NamespacePrefix string
	// Logger is optional.
	Logger *log.Logger
	// OnChange, when set, is called with an agent's name after its status is
	// promoted. cmd/muster-server passes api.Server.BroadcastAgentChanged, which
	// re-renders open Agents lists and Tasks boards and fires the "agent ready"
	// push for a task-linked agent.
	OnChange func(name string)
}

// Promoter promotes provisioning rows whose instance is ready. Build it with [New].
type Promoter struct {
	store  agents.Store
	insts  InstanceLister
	prefix string
	log    *log.Logger
	notify func(string)
	done   chan struct{}
	once   sync.Once
}

// New validates cfg and builds the promoter.
func New(cfg Config) (*Promoter, error) {
	switch {
	case cfg.Store == nil:
		return nil, errors.New("agentstatus: Config.Store is required")
	case cfg.Instances == nil:
		return nil, errors.New("agentstatus: Config.Instances is required (without live " +
			"instances no instance can be judged ready)")
	}
	p := &Promoter{
		store:  cfg.Store,
		insts:  cfg.Instances,
		prefix: agents.ResolveNamespacePrefix(cfg.NamespacePrefix),
		log:    cfg.Logger,
		notify: cfg.OnChange,
		done:   make(chan struct{}),
	}
	if p.log == nil {
		p.log = log.New(io.Discard, "", 0)
	}
	if p.notify == nil {
		p.notify = func(string) {}
	}
	return p, nil
}

// Tick makes one pass and reports how many rows it promoted.
//
// 🔴 A FAILED INSTANCE LIST ENDS THE TICK WITH NO ACTION. An unobserved backend
// is not evidence of anything; the next tick looks again.
func (p *Promoter) Tick(ctx context.Context) (int, error) {
	rows, err := p.store.List(ctx)
	if err != nil {
		return 0, fmt.Errorf("list agents: %w", err)
	}
	var waiting []agents.Agent
	for _, a := range rows {
		if a.Status == agents.StatusProvisioning && a.Namespace == agents.NamespaceFor(p.prefix, a.Name) {
			waiting = append(waiting, a)
		}
	}
	if len(waiting) == 0 {
		// No instance read at all when nothing is waiting: the steady state costs
		// one indexed list read per tick.
		return 0, nil
	}
	insts, err := p.insts.Instances(ctx)
	if err != nil {
		return 0, fmt.Errorf("list instances (no action taken this tick): %w", err)
	}
	idx := agents.InstanceIndex(insts)
	promoted := 0
	for _, a := range waiting {
		if !agents.PromoteToRunning(a, idx[a.Name]) {
			continue
		}
		applied, err := p.store.MarkRunning(ctx, a.ID, a.UpdatedAt)
		if err != nil {
			p.log.Printf("agentstatus: agent %d (%s): mark running: %v", a.ID, a.Name, err)
			continue
		}
		if !applied {
			// The row moved after it was read (stopped, restarted, errored, or
			// another replica promoted it). Its new state wins; nothing to do.
			continue
		}
		promoted++
		p.log.Printf("agentstatus: agent %d (%s): provisioning -> running (instance %s ready)",
			a.ID, a.Name, idx[a.Name].InstanceID)
		p.notify(a.Name)
	}
	return promoted, nil
}

// Run ticks every interval until ctx is cancelled, then closes [Promoter.Done].
func (p *Promoter) Run(ctx context.Context, interval time.Duration) {
	defer p.once.Do(func() { close(p.done) })
	if interval <= 0 {
		interval = DefaultInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := p.Tick(ctx); err != nil && ctx.Err() == nil {
			p.log.Printf("agentstatus: tick: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Done is closed once [Promoter.Run] has returned.
func (p *Promoter) Done() <-chan struct{} { return p.done }
