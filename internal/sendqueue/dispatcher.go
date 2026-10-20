package sendqueue

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"
)

// THE DISPATCHER: once a target has been continuously idle for IdleFor, it
// sends that target's next queued message — exactly ONE — through the same
// send path a person's Send uses, records the outcome, and then waits for the
// turn that message started to end before it sends the next.
//
// 🔴 ONE AT A TIME PER TARGET, BY CONSTRUCTION. A target's head is claimed
// (queued → sending) before it is sent and finished after; while a row is
// sending, Decide holds its target; and the next row waits until the host
// reports a status time STRICTLY NEWER than the one the window held at the
// latest paste into it — a queued message's or a person's own Send
// (Head.TurnAfter, AgentState.Pasted) — then has been idle for IdleFor. The
// status time is Claude Code's own (statusUpdatedAt, through the host), so it
// moves when the turn a paste started ends, however short that turn and
// whether or not anybody saw it busy. There is no time-based fallback: a
// target that never reports a newer status holds its queue.
//
// 🔴 TWO CLOCKS, NEVER MIXED. Status times are the HOST's clock and are
// compared only with each other. IdleFor is measured on MUSTER's clock, from
// when the hub first observed the current status (AgentState.Observed). So a
// host clock that is ahead of or behind muster's neither stalls the queue nor
// releases a message early (TestDecide's skew cases).
//
// 🔴 UNKNOWN IS NOT IDLE. A target whose state the server cannot read (a
// mirror host that does not report it, or does not say since when) is held,
// with the reason, and nothing is sent to it.
//
// 🔴 PROBE AND SEND ACT AS THE MESSAGE'S OWNER. The dispatcher reads every
// owner's heads under the system scope; before it probes a head's target or
// sends it, it hands the hooks ctx as that head's owner (AsOwner), so they see
// that owner's host and nobody else's.
//
// 🔴 A CLAIM IS A LEASE. Each dispatcher has its own Instance id; it claims a
// row for Lease, renews the claim every RenewEvery while the send runs, and
// finishes only its own rows. Every pass first fails rows whose lease expired
// (their sender stopped), so a pod that starts while another is mid-send — a
// rolling update — leaves that send alone. The lease is on Postgres's clock.
//
// 🔴 A PASTE THAT LOST THE RACE GOES BACK, PASTING NOTHING. Send is handed the
// state the decision was made on; a person's Send that got into the window
// between the decision and the paste makes the send path refuse the paste
// before it reaches the host (Outcome.Superseded), and the row is queued again
// behind that Send's turn, to be decided on afresh.

// AgentState is what the server can see of a target right now.
type AgentState struct {
	// Idle: the target is known to be idle (not busy, not waiting on the
	// person).
	Idle bool
	// Since is Claude Code's own time for its current status, Unix ms ON THE
	// HOST'S CLOCK. Set when Idle.
	Since int64
	// Observed is when the server first saw that status, on MUSTER's clock:
	// what IdleFor is measured from. Set when Idle.
	Observed time.Time
	// Pasted is the status time (host clock) the window held at its latest
	// paste that the server remembers in memory; 0 for none.
	Pasted int64
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
func Decide(h Head, st AgentState, now time.Time, idleFor time.Duration) Decision {
	if h.State == Sending {
		return Decision{Why: "sending now"}
	}
	if !st.Idle {
		return Decision{Why: st.Why}
	}
	if st.Since <= 0 || st.Observed.IsZero() {
		// Idle since an unknown time is not idle for any time.
		return Decision{Why: "Claude Code's idle time is unknown"}
	}
	if after := max(h.TurnAfter, st.Pasted); after > 0 && st.Since <= after {
		// The status is no newer than the one held at the last paste: the
		// turn that paste started has not ended yet.
		return Decision{Why: "waiting for the previous message's turn to start and finish"}
	}
	if idle := now.Sub(st.Observed); idle < idleFor {
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
	Expire(ctx context.Context, now time.Time) (int, error)
	Heads(ctx context.Context) ([]Head, error)
	Claim(ctx context.Context, id int64, now time.Time, c Claim) (Item, bool, error)
	Renew(ctx context.Context, id int64, by string, lease time.Duration) (bool, error)
	Finish(ctx context.Context, id int64, by string, o Outcome, now time.Time) error
	Prune(ctx context.Context, before time.Time) (int, error)
}

// Probe reads one head's target's state now. ctx acts as the head's owner.
type Probe func(ctx context.Context, h Head) AgentState

// Sender sends one claimed message through the manual send path. ctx acts as
// the message's owner; st is the target's state the decision to send was made
// on, which the send path must refuse to paste on if it has moved since
// (Outcome.Superseded).
type Sender func(ctx context.Context, it Item, st AgentState) Outcome

// AsOwner is ctx acting as user owner: what Probe and Send are handed.
type AsOwner func(ctx context.Context, owner int64) context.Context

// The defaults.
const (
	// DefaultIdleFor is how long a target must have been idle (MUSTER_SEND_QUEUE_IDLE).
	DefaultIdleFor = 30 * time.Second
	// DefaultInterval is how often the dispatcher looks.
	DefaultInterval = 2 * time.Second
	// MinIdleFor and MaxIdleFor bound a configured IdleFor.
	MinIdleFor = time.Second
	MaxIdleFor = time.Hour
	// DefaultLease is how long a claim holds without a renewal, and
	// DefaultRenewEvery how often a sender renews it: a sender that stops
	// (its pod was killed) has its row failed about a lease later.
	DefaultLease      = 30 * time.Second
	DefaultRenewEvery = 10 * time.Second
)

// Dispatcher sends queued messages. Zero durations take the defaults.
type Dispatcher struct {
	Store   DispatchStore
	Probe   Probe
	Send    Sender
	AsOwner AsOwner
	IdleFor time.Duration
	Now     func() time.Time
	Logger  *log.Logger
	// Instance names this dispatcher in its claims (claimed_by); "" is a
	// random one, made once (InstanceID). Two dispatchers must never share one.
	Instance string
	// Lease and RenewEvery are the claim's lease and how often a send in
	// progress renews it; 0 takes the defaults.
	Lease      time.Duration
	RenewEvery time.Duration

	kick     chan struct{}
	instOnce sync.Once
}

// InstanceID is d's claim name: Instance, or — unset — the host name and 8
// random bytes, made once.
func (d *Dispatcher) InstanceID() string {
	d.instOnce.Do(func() {
		if d.Instance != "" {
			return
		}
		host, _ := os.Hostname()
		if len(host) > 64 {
			host = host[:64]
		}
		b := make([]byte, 8)
		_, _ = rand.Read(b)
		d.Instance = host + "/" + hex.EncodeToString(b)
	})
	return d.Instance
}

func (d *Dispatcher) lease() time.Duration {
	if d.Lease > 0 {
		return d.Lease
	}
	return DefaultLease
}

func (d *Dispatcher) renewEvery() time.Duration {
	if d.RenewEvery > 0 {
		return d.RenewEvery
	}
	return DefaultRenewEvery
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

func (d *Dispatcher) logf(format string, args ...any) {
	if d.Logger != nil {
		d.Logger.Printf(format, args...)
	}
}

// Decide is the package Decide with this dispatcher's durations and clock.
func (d *Dispatcher) Decide(h Head, st AgentState) Decision {
	return Decide(h, st, d.now(), d.idleFor())
}

// Start fails what a stopped sender left mid-send (a lease that expired). Every
// Tick does the same first; Start does it once before the first.
func (d *Dispatcher) Start(ctx context.Context) error {
	return d.recover(ctx)
}

func (d *Dispatcher) recover(ctx context.Context) error {
	n, err := d.Store.Recover(ctx, d.now())
	if n > 0 {
		d.logf("sendqueue: %d message(s) were being sent by a muster that stopped; marked failed (may have been delivered), not sent again", n)
	}
	return err
}

// expire fails a person's messages queued more than AuthorizedFor ago
// (Store.Expire), so a window that never goes idle neither shows them queued
// past the bound nor keeps their slots.
func (d *Dispatcher) expire(ctx context.Context) error {
	n, err := d.Store.Expire(ctx, d.now())
	if n > 0 {
		d.logf("sendqueue: %d queued message(s) were queued more than %s ago; marked failed as expired, not sent", n, AuthorizedFor)
	}
	return err
}

// Tick makes one pass: fail what a stopped sender left mid-send, fail what
// was queued too long ago to be sent (expire), then for every target whose
// head Decide says to send, claim it, send it (renewing the claim while it
// runs), and record the outcome. It returns how many it sent or failed (an
// expired message is not counted).
func (d *Dispatcher) Tick(ctx context.Context) (int, error) {
	if d.AsOwner == nil {
		// Without it the hooks would run as the system scope: refuse.
		return 0, errors.New("sendqueue: the dispatcher has no AsOwner")
	}
	if err := d.recover(ctx); err != nil {
		return 0, err
	}
	if err := d.expire(ctx); err != nil {
		// The delivery refuses an expired row anyway; carry on sending.
		d.logf("sendqueue: %v", err)
	}
	heads, err := d.Store.Heads(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, h := range heads {
		if ctx.Err() != nil {
			return n, ctx.Err()
		}
		octx := d.AsOwner(ctx, h.Owner)
		st := d.Probe(octx, h)
		if !d.Decide(h, st).Send {
			continue
		}
		it, ok, err := d.Store.Claim(ctx, h.ID, d.now(), Claim{By: d.InstanceID(), Lease: d.lease(), Since: st.Since})
		if err != nil {
			d.logf("sendqueue: claim message %d (%s): %v", h.ID, h.Target, err)
			continue
		}
		if !ok {
			continue
		}
		o := d.sendRenewing(ctx, octx, it, st, d.Send)
		if o.Sent {
			o.MayHaveDelivered = true
		}
		if o.Sent || o.MayHaveDelivered {
			o.Superseded = false
		}
		// The outcome is recorded even if ctx ended during the send: the row
		// must not stay 'sending' (Recover would call it unknown).
		if err := d.Store.Finish(context.WithoutCancel(ctx), it.ID, d.InstanceID(), o, d.now()); err != nil {
			d.logf("sendqueue: record message %d (%s): %v", it.ID, it.Target, err)
		}
		if o.Superseded {
			// Nothing reached the host: queued again, decided on afresh.
			d.logf("sendqueue: message %d (%d bytes) to %s (owner %d) was superseded by a newer turn before it was pasted; queued again",
				it.ID, len(it.Body), it.Target, it.Owner)
			continue
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

// sendRenewing is send(octx, it, st), renewing it's claim every RenewEvery
// until the send returns, so that no other instance's Recover fails a live
// send.
func (d *Dispatcher) sendRenewing(ctx, octx context.Context, it Item, st AgentState, send Sender) Outcome {
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(d.renewEvery())
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				ok, err := d.Store.Renew(context.WithoutCancel(ctx), it.ID, d.InstanceID(), d.lease())
				if err != nil {
					d.logf("sendqueue: renew the claim on message %d (%s): %v", it.ID, it.Target, err)
				} else if !ok {
					d.logf("sendqueue: the claim on message %d (%s) was lost mid-send", it.ID, it.Target)
				}
			}
		}
	}()
	o := send(octx, it, st)
	close(stop)
	<-done
	return o
}

// ErrNotClaimed: SendNow could not claim the message — it is no longer queued
// (sent, cancelled, or claimed by the dispatcher first), or its target already
// has a message being sent. Nothing was sent.
var ErrNotClaimed = errors.New("sendqueue: the message is no longer queued, or another message for its window is being sent")

// SendNow is a person's "Send now" of queued message id: claimed (queued →
// sending) under this dispatcher's instance, exactly as Tick claims a head, so
// the dispatcher can never also send it, and sent through send — renewing the
// claim while it runs — WITHOUT Decide: the person chose not to wait for idle.
// since is the window's status time (host clock) now, recorded as the claim's
// so the next message waits for the turn this one starts. The outcome is
// recorded (Finish) and returned; ErrNotClaimed when the claim failed.
//
// 🔴 ONE CLAIM, ONE SEND. The claim is the same conditional UPDATE the
// dispatcher's is (Store.Claim): whichever of the two claims first sends, the
// other's claim finds the row no longer queued — and a target's one-sending
// index refuses a claim while another of its rows is being sent.
func (d *Dispatcher) SendNow(ctx, octx context.Context, id int64, since int64, send Sender) (Item, Outcome, error) {
	it, ok, err := d.Store.Claim(ctx, id, d.now(), Claim{By: d.InstanceID(), Lease: d.lease(), Since: since})
	if err != nil {
		return Item{}, Outcome{}, err
	}
	if !ok {
		return Item{}, Outcome{}, ErrNotClaimed
	}
	o := d.sendRenewing(ctx, octx, it, AgentState{Since: since}, send)
	if o.Sent {
		o.MayHaveDelivered = true
	}
	// Nothing here expects a turn, so nothing is superseded: the row is never
	// put back to queued once it was claimed for a Send now.
	o.Superseded = false
	if !o.Sent && !o.MayHaveDelivered && o.Detail == "" {
		o.Detail = "Not sent."
	}
	if err := d.Store.Finish(context.WithoutCancel(ctx), it.ID, d.InstanceID(), o, d.now()); err != nil {
		d.logf("sendqueue: record message %d (%s): %v", it.ID, it.Target, err)
	}
	return it, o, nil
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
		// Every Tick recovers again; a message left sending blocks its target
		// (the one-sending index) until then, and is never re-sent either way.
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
