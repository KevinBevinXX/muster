package api

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentprovision"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/provision"
)

// TestOnlyAnAgentTheAdapterBroughtUpIsReapable is the SEAM between
// agentprovision.Adapter's status writes and migration 0045's first_started_at trigger, on real
// Postgres with the noop driver: the stopped-agent reaper's "it ran" signal is
// whatever that adapter writes, so it is pinned here rather than assumed. (It
// lives in this package because internal/api already has a database;
// agentprovision has none.)
//
// Rows are inserted `provisioning`, as internal/api's dispatch inserts every
// agent. A Dispatch with no kickoff (Save for later) and a Stop leaves the agent
// unreapable however long it stays stopped; a Dispatch with a kickoff then Stop,
// and a saved agent that is Started then Stopped, are reapable once past the
// window.
func TestOnlyAnAgentTheAdapterBroughtUpIsReapable(t *testing.T) {
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), time.Minute)
	defer cancel()
	pool, err := db.Connect(ctx, dbtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool, nil); err != nil {
		t.Fatal(err)
	}
	store := agents.NewPG(pool)
	a, err := agentprovision.New(agentprovision.Config{Driver: provision.MustNewNoop(), Store: store,
		Spec:               agentspec.Config{ImageRepo: "registry.example.test/muster/agent-runtime", APIBaseURL: "http://muster.example.test:8105"},
		KickoffDeliverable: true})
	if err != nil {
		t.Fatal(err)
	}
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	mk := func(name string) agents.Agent {
		t.Helper()
		ag, err := store.Create(ctx, agents.Agent{Name: name + "-" + suffix, Namespace: "devpod-" + name + "-" + suffix,
			PendingNote: "do the thing", Status: agents.StatusProvisioning})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM agents WHERE id = $1`, ag.ID) })
		return ag
	}
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	saved, dispatched, savedThenStarted := mk("seam-saved"), mk("seam-dispatched"), mk("seam-started")
	must("save", a.Dispatch(ctx, saved.ID, false))
	must("dispatch", a.Dispatch(ctx, dispatched.ID, true))
	must("stop dispatched", a.Stop(ctx, dispatched.ID))
	must("save", a.Dispatch(ctx, savedThenStarted.ID, false))
	must("start", a.Start(ctx, savedThenStarted.ID))
	must("stop started", a.Stop(ctx, savedThenStarted.ID))

	ids := []int64{saved.ID, dispatched.ID, savedThenStarted.ID}
	for _, id := range ids {
		got, err := store.Get(ctx, id)
		must("read", err)
		if got.Status != agents.StatusStopped {
			t.Fatalf("fixture: agent %d is %q, want stopped", id, got.Status)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE agents SET stopped_at = now() - interval '15 days' WHERE id = ANY($1)`, ids); err != nil {
		t.Fatal(err)
	}
	due, err := store.ListReapableStopped(ctx, 14*24*time.Hour)
	must("list", err)
	listed := map[int64]bool{}
	for _, d := range due {
		listed[d.ID] = true
	}
	for name, c := range map[string]struct {
		id   int64
		want bool
	}{
		"saved for later and never started": {saved.ID, false},
		"dispatched, then stopped":          {dispatched.ID, true},
		"saved, started, then stopped":      {savedThenStarted.ID, true},
	} {
		if listed[c.id] != c.want {
			t.Errorf("%s: reapable = %v, want %v", name, listed[c.id], c.want)
		}
	}
}
