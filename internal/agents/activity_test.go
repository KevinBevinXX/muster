package agents

import (
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/provision"
)

// TestLastActivityIsTheNewerOfTheRowAndItsTranscript pins the definition with
// times that are pairwise distinct, so a mutant that always returns one side —
// or that reads CreatedAt — answers differently in at least one row.
func TestLastActivityIsTheNewerOfTheRowAndItsTranscript(t *testing.T) {
	t0 := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	a := Agent{CreatedAt: t0, UpdatedAt: t0.Add(10 * time.Minute)}
	cases := []struct {
		name string
		msg  time.Time
		want time.Time
	}{
		{"no transcript", time.Time{}, t0.Add(10 * time.Minute)},
		{"older transcript", t0.Add(3 * time.Minute), t0.Add(10 * time.Minute)},
		{"newer transcript", t0.Add(42 * time.Minute), t0.Add(42 * time.Minute)},
	}
	for _, c := range cases {
		if got := LastActivity(a, c.msg); !got.Equal(c.want) {
			t.Errorf("%s: LastActivity = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestSortByLastActivityOrdersMostRecentFirst: the order the Agents list shows.
// Creation order is deliberately the REVERSE of activity order here, so a sort
// that fell back to created_at (the store's order) would fail every position.
func TestSortByLastActivityOrdersMostRecentFirst(t *testing.T) {
	t0 := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	list := []Agent{
		// store order: newest CREATED first
		{ID: 50, Name: "newest-created-idle", CreatedAt: t0.Add(5 * time.Hour), UpdatedAt: t0.Add(5 * time.Hour)},
		{ID: 40, Name: "status-moved", CreatedAt: t0.Add(4 * time.Hour), UpdatedAt: t0.Add(7 * time.Hour)},
		{ID: 30, Name: "chatting", CreatedAt: t0.Add(3 * time.Hour), UpdatedAt: t0.Add(3 * time.Hour)},
		{ID: 20, Name: "tie-low-id", CreatedAt: t0.Add(1 * time.Hour), UpdatedAt: t0.Add(6 * time.Hour)},
		{ID: 21, Name: "tie-high-id", CreatedAt: t0.Add(2 * time.Hour), UpdatedAt: t0.Add(6 * time.Hour)},
	}
	msgs := map[int64]time.Time{30: t0.Add(9 * time.Hour), 50: t0.Add(1 * time.Hour)}
	SortByLastActivity(list, msgs)
	var got []string
	for _, a := range list {
		got = append(got, a.Name)
	}
	want := "chatting,status-moved,tie-high-id,tie-low-id,newest-created-idle"
	if strings.Join(got, ",") != want {
		t.Errorf("order = %s\n   want %s", strings.Join(got, ","), want)
	}
}

// TestPromoteToRunningTruthTable: only a stored `provisioning` row over a live,
// ready, healthy instance is promoted.
func TestPromoteToRunningTruthTable(t *testing.T) {
	ready := &provision.Instance{Ref: provision.Ref{Name: "x"}, Phase: provision.PhaseRunning, Ready: true}
	notReady := &provision.Instance{Ref: provision.Ref{Name: "x"}, Phase: provision.PhaseRunning}
	pending := &provision.Instance{Ref: provision.Ref{Name: "x"}, Phase: provision.PhasePending}
	looping := &provision.Instance{Ref: provision.Ref{Name: "x"}, Phase: provision.PhaseRunning, Ready: true, Reason: "CrashLoopBackOff"}
	scaledDown := &provision.Instance{Ref: provision.Ref{Name: "x"}, Phase: provision.PhaseStopped}
	cases := []struct {
		name   string
		status string
		owed   bool
		inst   *provision.Instance
		want   bool
	}{
		{"provisioning + ready", StatusProvisioning, false, ready, true},
		{"provisioning + ready + first turn still owed", StatusProvisioning, true, ready, true},
		{"provisioning + not ready", StatusProvisioning, false, notReady, false},
		{"provisioning + pending", StatusProvisioning, false, pending, false},
		{"provisioning + crash-looping", StatusProvisioning, false, looping, false},
		{"provisioning + scaled to zero", StatusProvisioning, false, scaledDown, false},
		{"provisioning + no instance", StatusProvisioning, false, nil, false},
		{"stopped + ready", StatusStopped, false, ready, false},
		{"pending + ready", StatusPending, false, ready, false},
		{"error + ready", StatusError, false, ready, false},
		{"running + ready", StatusRunning, false, ready, false},
	}
	for _, c := range cases {
		a := Agent{Name: "x", Status: c.status}
		if c.owed {
			a.PendingNote = "do the thing"
		}
		if got := PromoteToRunning(a, c.inst); got != c.want {
			t.Errorf("%s: PromoteToRunning = %v, want %v", c.name, got, c.want)
		}
	}
}
