package agentstatus

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/provision"
)

const prefix = "promo-test-"

// instances is a scripted InstanceLister. before, when set, runs inside the
// list call — i.e. AFTER the promoter has read the rows and BEFORE it writes —
// which is exactly the window a concurrent Stop/Start lands in.
type instances struct {
	list   []provision.Instance
	err    error
	before func()
}

func (f *instances) Instances(context.Context) ([]provision.Instance, error) {
	if f.before != nil {
		f.before()
	}
	return f.list, f.err
}

func ready(name string) provision.Instance {
	return provision.Instance{Ref: provision.Ref{Name: name}, InstanceID: name + "-pod-1", Phase: provision.PhaseRunning, Ready: true}
}

func newStore(t *testing.T) (agents.Store, func(string) agents.Agent, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 30*time.Second)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dbtest.DSN(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM agents WHERE namespace LIKE $1 OR namespace LIKE 'elsewhere-%'`, prefix+"%"); err != nil {
		t.Fatalf("clean: %v", err)
	}
	store := agents.NewPG(pool)
	get := func(name string) agents.Agent {
		t.Helper()
		a, err := store.GetByName(ctx, name)
		if err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		return a
	}
	return store, get, ctx
}

func mk(t *testing.T, ctx context.Context, store agents.Store, name, ns, status string) agents.Agent {
	t.Helper()
	a, err := store.Create(ctx, agents.Agent{Name: name, DisplayName: name, Namespace: ns, Status: status})
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	return a
}

// TestThePromoterPromotesOnlyAReadyProvisioningRowItOwns is the decision, against
// real Postgres: exactly one of these six rows may move, and it must reach
// `running` with the change announced.
func TestThePromoterPromotesOnlyAReadyProvisioningRowItOwns(t *testing.T) {
	store, get, ctx := newStore(t)
	ns := func(n string) string { return agents.NamespaceFor(prefix, n) }
	mk(t, ctx, store, "ready-wren", ns("ready-wren"), agents.StatusProvisioning)
	mk(t, ctx, store, "booting-wren", ns("booting-wren"), agents.StatusProvisioning)
	mk(t, ctx, store, "absent-wren", ns("absent-wren"), agents.StatusProvisioning)
	mk(t, ctx, store, "looping-wren", ns("looping-wren"), agents.StatusProvisioning)
	mk(t, ctx, store, "stopped-wren", ns("stopped-wren"), agents.StatusStopped)
	mk(t, ctx, store, "foreign-wren", "elsewhere-foreign-wren", agents.StatusProvisioning)

	looping := ready("looping-wren")
	looping.Reason = "CrashLoopBackOff"
	booting := ready("booting-wren")
	booting.Ready = false
	insts := &instances{list: []provision.Instance{
		ready("ready-wren"), booting, looping, ready("stopped-wren"), ready("foreign-wren"),
	}}
	var mu sync.Mutex
	var changed []string
	p, err := New(Config{Store: store, Instances: insts, NamespacePrefix: prefix, OnChange: func(n string) {
		mu.Lock()
		defer mu.Unlock()
		changed = append(changed, n)
	}})
	if err != nil {
		t.Fatal(err)
	}
	before := get("ready-wren")
	n, err := p.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	want := map[string]string{
		"ready-wren":   agents.StatusRunning,
		"booting-wren": agents.StatusProvisioning, // instance not ready yet
		"absent-wren":  agents.StatusProvisioning, // no instance at all
		"looping-wren": agents.StatusProvisioning, // crash-looping: not this loop's verdict
		"stopped-wren": agents.StatusStopped,      // a Stop is never overruled
		"foreign-wren": agents.StatusProvisioning, // another deployment's row
	}
	for name, st := range want {
		if got := get(name).Status; got != st {
			t.Errorf("%s: status %q after one tick, want %q", name, got, st)
		}
	}
	if n != 1 {
		t.Errorf("Tick promoted %d row(s), want 1", n)
	}
	if after := get("ready-wren"); !after.UpdatedAt.After(before.UpdatedAt) {
		t.Errorf("promotion did not move updated_at (%v -> %v): agents.LastActivity would not see the "+
			"transition", before.UpdatedAt, after.UpdatedAt)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(changed) != 1 || changed[0] != "ready-wren" {
		t.Errorf("OnChange calls %v, want [ready-wren]: open lists would not re-render", changed)
	}
}

// TestAPromotionLosesToARowThatMovedAfterItWasRead: the verdict is taken from a
// row read, and a Stop or a fresh Start landing before the write must win.
func TestAPromotionLosesToARowThatMovedAfterItWasRead(t *testing.T) {
	store, get, ctx := newStore(t)
	ns := func(n string) string { return agents.NamespaceFor(prefix, n) }
	stopped := mk(t, ctx, store, "stopped-midtick", ns("stopped-midtick"), agents.StatusProvisioning)
	restarted := mk(t, ctx, store, "restarted-midtick", ns("restarted-midtick"), agents.StatusProvisioning)
	// updated_at has microsecond resolution; make sure the rewrite below lands on
	// a different value than the one the promoter read.
	time.Sleep(5 * time.Millisecond)
	insts := &instances{
		list: []provision.Instance{ready("stopped-midtick"), ready("restarted-midtick")},
		before: func() {
			if err := store.UpdateStatus(ctx, stopped.ID, agents.StatusStopped, "", ""); err != nil {
				t.Errorf("concurrent stop: %v", err)
			}
			if err := store.UpdateStatus(ctx, restarted.ID, agents.StatusProvisioning, "", ""); err != nil {
				t.Errorf("concurrent start: %v", err)
			}
		},
	}
	p, err := New(Config{Store: store, Instances: insts, NamespacePrefix: prefix})
	if err != nil {
		t.Fatal(err)
	}
	n, err := p.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if n != 0 {
		t.Errorf("Tick promoted %d row(s) decided from a stale read, want 0", n)
	}
	if got := get("stopped-midtick").Status; got != agents.StatusStopped {
		t.Errorf("a Stop written mid-tick was overwritten: status %q, want stopped", got)
	}
	if got := get("restarted-midtick").Status; got != agents.StatusProvisioning {
		t.Errorf("a Start written mid-tick was overwritten from the older read: status %q", got)
	}
	// The row that was re-started is promoted on the NEXT tick, from a fresh read:
	// losing the race delays the promotion, it does not lose it.
	insts.before = nil
	if n, err := p.Tick(ctx); err != nil || n != 1 {
		t.Fatalf("second Tick = %d, %v; want 1 promotion", n, err)
	}
	if got := get("restarted-midtick").Status; got != agents.StatusRunning {
		t.Errorf("restarted row after a fresh tick: %q, want running", got)
	}
	if got := get("stopped-midtick").Status; got != agents.StatusStopped {
		t.Errorf("stopped row after a fresh tick: %q, want stopped", got)
	}
}

// TestABlindInstanceListTakesNoAction: an unobserved backend is not evidence.
func TestABlindInstanceListTakesNoAction(t *testing.T) {
	store, get, ctx := newStore(t)
	mk(t, ctx, store, "blind-wren", agents.NamespaceFor(prefix, "blind-wren"), agents.StatusProvisioning)
	p, err := New(Config{Store: store, Instances: &instances{err: errors.New("backend unreachable")}, NamespacePrefix: prefix})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Tick(ctx); err == nil {
		t.Error("Tick over a failed instance list returned nil")
	}
	if got := get("blind-wren").Status; got != agents.StatusProvisioning {
		t.Errorf("status %q after a blind tick, want provisioning", got)
	}
}

// TestMarkRunningOnlyMovesAProvisioningRow pins the store write's OWN status
// guard, which the promoter's tick can never reach (it only offers provisioning
// rows, and every status write moves updated_at): a caller holding a row's
// current updated_at still cannot promote a row that is not provisioning.
func TestMarkRunningOnlyMovesAProvisioningRow(t *testing.T) {
	store, get, ctx := newStore(t)
	for _, st := range []string{agents.StatusStopped, agents.StatusError, agents.StatusPending} {
		name := "guard-" + st
		a := mk(t, ctx, store, name, agents.NamespaceFor(prefix, name), st)
		applied, err := store.MarkRunning(ctx, a.ID, a.UpdatedAt)
		if err != nil {
			t.Fatalf("%s: MarkRunning: %v", st, err)
		}
		if applied || get(name).Status != st {
			t.Errorf("MarkRunning promoted a %s row (applied=%v, now %q)", st, applied, get(name).Status)
		}
	}
	// Control: the same call on a provisioning row applies.
	a := mk(t, ctx, store, "guard-provisioning", agents.NamespaceFor(prefix, "guard-provisioning"), agents.StatusProvisioning)
	if applied, err := store.MarkRunning(ctx, a.ID, a.UpdatedAt); err != nil || !applied {
		t.Fatalf("control: MarkRunning on a provisioning row = %v, %v; want applied", applied, err)
	}
	if got := get("guard-provisioning").Status; got != agents.StatusRunning {
		t.Errorf("control: status %q, want running", got)
	}
}
