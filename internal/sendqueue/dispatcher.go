package sendqueue

import (
	"context"
	"fmt"
	"log"
	"time"
)

// THE DISPATCHER: once a target has been continuously idle for IdleFor, it
// sends that target's next queued message — exactly ONE — through the same
// send path a person's Send uses, records the outcome, and then waits for the
// target to have been busy and gone idle again before it sends the next.
//
// 🔴 ONE AT A TIME PER TARGET, BY CONSTRUCTION. A target's head is claimed
// (queued → sending) before it is sent and finished after; while a row is
// sending, Decide holds its target; and the next row waits until the target's
// idle stretch BEGAN AFTER the last message was handed over (Head.LastAttempt):
// the target must have been seen busy — the turn that message started — and
// then idle again for IdleFor.
//
// 🔴 UNKNOWN IS NOT IDLE. A target whose state the server cannot read (a
// mirror host that does not report it, an agent that is not running) is held,
// with the reason, and nothing is sent to it.

// AgentState is what the server can see of a target right now.
type AgentState struct {
	// Idle: the target is known to be idle (not busy, not waiting on the
	// person).
	Idle bool
	// Since is when the target became idle — continuously since, as far as the
	// server has seen. Set when Idle.
	Since time.Time
	// Why says, when not Idle, why the target is not sendable now — busy, or
	// unknown and why. Shown to the owner.
	Why string
}

// Decision is what Decide made of one head.
type Decision struct {
	Send bool
	// Why says, when not Send, what the head is waiting for.
	Why string
}

// Decide is the dispatcher's whole rule for one head, given the target's state
// now. It is a pure function so that what a composer shows ("waiting: …") and
// what the dispatcher does are the same decision.
//
// turnWait is the one exception to "the target must have been seen busy since
// the last message": a target idle continuously for turnWait since that
// message was handed over is taken to have finished it unseen (a turn shorter
// than the host's refresh is never seen busy). turnWait <= 0 disables it.
func Decide(h Head, st AgentState, now time.Time, idleFor, turnWait time.Duration) Decision {
	if h.State == Sending {
		return Decision{Why: "sending now"}
	}
	if !st.Idle {
		return Decision{Why: st.Why}
	}
	since := st.Since
	if !h.LastAttempt.IsZero() && !since.After(h.LastAttempt) {
		// Idle since BEFORE the last message: its turn has not been seen.
		if turnWait <= 0 || now.Sub(h.LastAttempt) < turnWait {
			return Decision{Why: "waiting for the previous message's turn to start and finish"}
		}
		since = h.LastAttempt
	}
	if idle := now.Sub(since); idle < idleFor {
		return Decision{Why: fmt.Sprintf("idle for %s; sends after %s idle", roundSecond(idle), roundSecond(idleFor))}
	}
	return Decision{Send: true}
}

func roundSecond(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d.Truncate(time.Second)
}

// DispatchStore is the store half the dispatcher drives (PGStore's).
type DispatchStore interface {
	Recover(ctx context.Context, now time.Time) (int, error)
	Heads(ctx context.Context) ([]Head, error)
	Claim(ctx context.Context, id int64, now time.Time) (Item, bool, error)
	Finish(ctx context.Context, id int64, o Outcome, now time.Time) error
	Prune(ctx context.Context, before time.Time) (int, error)
}

// Probe reads one head's target's state now.
type Probe func(ctx context.Context, h Head) AgentState

// Sender sends one claimed message through the manual send path, as its owner.
type Sender func(ctx context.Context, it Item) Outcome

// The defaults.
const (
	// DefaultIdleFor is how long a target must have been idle (MUSTER_SEND_QUEUE_IDLE).
	DefaultIdleFor = 30 * time.Second
	// DefaultTurnWait is Decide's turnWait.
	DefaultTurnWait = 10 * time.Minute
	// DefaultInterval is how often the dispatcher looks.
	DefaultInterval = 2 * time.Second
	// MinIdleFor and MaxIdleFor bound a configured IdleFor.
	MinIdleFor = time.Second
	MaxIdleFor = time.Hour
)

// Dispatcher sends queued messages. Zero durations take the defaults.
type Dispatcher struct {
	Store    DispatchStore
	Probe    Probe
	Send     Sender
	IdleFor  time.Duration
	TurnWait time.Duration
	Now      func() time.Time
	Logger   *log.Logger

	kick chan struct{}
}

func (d *Dispatcher) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *Dispatcher) idleFor() time.Duration {
	if d.IdleFor > 0 {
		return d.IdleFor
	}
	return DefaultIdleFor
}

func (d *Dispatcher) turnWait() time.Duration {
	if d.TurnWait != 0 {
		return d.TurnWait
	}
	return DefaultTurnWait
}

func (d *Dispatcher) logf(format string, args ...any) {
	if d.Logger != nil {
		d.Logger.Printf(format, args...)
	}
}

// Decide is the package Decide with this dispatcher's durations and clock.
func (d *Dispatcher) Decide(h Head, st AgentState) Decision {
	return Decide(h, st, d.now(), d.idleFor(), d.turnWait())
}

// Start recovers what a previous process left mid-send. It must run before the
// first Tick.
func (d *Dispatcher) Start(ctx context.Context) error {
	n, err := d.Store.Recover(ctx, d.now())
	if n > 0 {
		d.logf("sendqueue: %d message(s) were being sent when muster last stopped; marked failed (may have been delivered), not sent again", n)
	}
	return err
}

// Tick makes one pass: for every target whose head Decide says to send, claim
// it, send it, and record the outcome. It returns how many it sent or failed.
func (d *Dispatcher) Tick(ctx context.Context) (int, error) {
	heads, err := d.Store.Heads(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, h := range heads {
		if ctx.Err() != nil {
			return n, ctx.Err()
		}
		if !d.Decide(h, d.Probe(ctx, h)).Send {
			continue
		}
		it, ok, err := d.Store.Claim(ctx, h.ID, d.now())
		if err != nil {
			d.logf("sendqueue: claim message %d (%s): %v", h.ID, h.Target, err)
			continue
		}
		if !ok {
			continue
		}
		o := d.Send(ctx, it)
		if o.Sent {
			o.MayHaveDelivered = true
		}
		// The outcome is recorded even if ctx ended during the send: the row
		// must not stay 'sending' (Recover would call it unknown).
		if err := d.Store.Finish(context.WithoutCancel(ctx), it.ID, o, d.now()); err != nil {
			d.logf("sendqueue: record message %d (%s): %v", it.ID, it.Target, err)
		}
		if o.Sent {
			d.logf("sendqueue: sent message %d (%d bytes) to %s (owner %d)", it.ID, len(it.Body), it.Target, it.Owner)
		} else {
			d.logf("sendqueue: message %d (%d bytes) to %s (owner %d) failed (may have been delivered: %t): %s",
				it.ID, len(it.Body), it.Target, it.Owner, o.MayHaveDelivered, o.Detail)
		}
		n++
	}
	return n, nil
}

// Kick asks Run for a pass now (a message was queued). It never blocks.
func (d *Dispatcher) Kick() {
	if d.kick == nil {
		return
	}
	select {
	case d.kick <- struct{}{}:
	default:
	}
}

// Init makes Kick usable; call it before Run and before anything may Kick.
func (d *Dispatcher) Init() {
	if d.kick == nil {
		d.kick = make(chan struct{}, 1)
	}
}

// Run recovers, then ticks every interval (and on each Kick) until ctx ends,
// pruning finished messages older than Retention once an hour.
func (d *Dispatcher) Run(ctx context.Context, interval time.Duration) {
	d.Init()
	if interval <= 0 {
		interval = DefaultInterval
	}
	if err := d.Start(ctx); err != nil {
		// Without the recovery a message left sending would block its target
		// (the one-sending index) — but it is never re-sent either way.
		d.logf("sendqueue: recover: %v", err)
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	var pruned time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-d.kick:
		}
		if _, err := d.Tick(ctx); err != nil && ctx.Err() == nil {
			d.logf("sendqueue: %v", err)
		}
		if now := d.now(); now.Sub(pruned) > time.Hour {
			pruned = now
			if _, err := d.Store.Prune(ctx, now.Add(-Retention)); err != nil && ctx.Err() == nil {
				d.logf("sendqueue: prune: %v", err)
			}
		}
	}
}
