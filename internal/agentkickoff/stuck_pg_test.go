package agentkickoff

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// movingPG is the REAL Postgres store with one seam: right after List returns its
// snapshot, it runs move — the row changing between the deliverer's read and its
// stuck write, which is the window failStuck's verdict is taken across.
type movingPG struct {
	*agents.PGStore
	move func(ctx context.Context)
}

func (m *movingPG) List(ctx context.Context) ([]agents.Agent, error) {
	rows, err := m.PGStore.List(ctx)
	if err == nil && m.move != nil {
		m.move(ctx)
	}
	return rows, err
}

// TestAStuckVerdictDoesNotOverwriteARowThatMoved (F2) pins the conditional stuck
// write against the REAL SQL. Each case backdates an owed row past the dwell bound
// with no instance (the table says ActionError), then moves the row between the
// list read and the write:
//
//   - stamped:   a delivery stamped it and recorded the recipient — a paid turn.
//   - restarted: the operator's Start set it `provisioning` and wrote last_output.
//   - claimed:   another delivery holds the kickoff claim and may be about to stamp.
//
// CONTROL in the same harness: an unmoved row IS marked `error`, so "the row was
// left alone" is not "the write never runs".
func TestAStuckVerdictDoesNotOverwriteARowThatMoved(t *testing.T) {
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 30*time.Second)
	defer cancel()
	pool, err := db.Connect(ctx, dbtest.DSN(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pg := agents.NewPG(pool)

	for i, c := range []struct {
		label      string
		name       string
		move       func(ctx context.Context, s *agents.PGStore, id int64) error
		wantStatus string
		wantOutput string
	}{
		{label: "control: unmoved", name: "rapid-egret", wantStatus: agents.StatusError, wantOutput: ""},
		{label: "stamped meanwhile", name: "amber-teal",
			move: func(ctx context.Context, s *agents.PGStore, id int64) error {
				if err := s.SetKickedOff(ctx, id, true); err != nil {
					return err
				}
				return s.RecordKickoffDelivery(ctx, id, "amber-teal-pod", 3)
			},
			wantStatus: agents.StatusProvisioning, wantOutput: "booting zt66"},
		{label: "restarted meanwhile", name: "dusky-plover",
			move: func(ctx context.Context, s *agents.PGStore, id int64) error {
				return s.UpdateStatus(ctx, id, agents.StatusProvisioning, "restarted by operator kc04", "")
			},
			wantStatus: agents.StatusProvisioning, wantOutput: "restarted by operator kc04"},
		{label: "claimed meanwhile", name: "pale-gannet",
			move: func(ctx context.Context, s *agents.PGStore, id int64) error {
				_, err := s.ClaimKickoff(ctx, id, "other-replica/7", time.Minute)
				return err
			},
			wantStatus: agents.StatusProvisioning, wantOutput: "booting zt66"},
	} {
		t.Run(c.label, func(t *testing.T) {
			prefix := "stuck-" + time.Now().Format("150405") + "-" + string(rune('a'+i)) + "-"
			row, err := pg.Create(ctx, agents.Agent{
				Name: c.name, Namespace: prefix + c.name, Status: agents.StatusProvisioning,
				PendingNote: "list the flaky tests for " + c.name, HooksToken: "tok-" + c.name,
			})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			t.Cleanup(func() {
				_, _ = pool.Exec(dbtest.AsOperator(context.Background()), `DELETE FROM agents WHERE id=$1`, row.ID)
			})
			if _, err := pool.Exec(ctx, `UPDATE agents SET last_output='booting zt66',
				updated_at = now() - $2::interval WHERE id=$1`,
				row.ID, (agents.ProvisioningStuckTimeout + time.Minute).String()); err != nil {
				t.Fatalf("backdate: %v", err)
			}

			store := &movingPG{PGStore: pg}
			if c.move != nil {
				moved := false
				store.move = func(ctx context.Context) {
					if moved {
						return
					}
					moved = true
					if err := c.move(ctx, pg, row.ID); err != nil {
						t.Errorf("move: %v", err)
					}
				}
			}
			var changed []string
			d, err := New(Config{
				Store: store, Gateway: &fakeGateway{log: &recorder{}}, NamespacePrefix: prefix,
				Owner: "stuck-test/1", Instances: &fakeInstances{},
				OnChange: func(name string) { changed = append(changed, name) },
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if err := d.Tick(ctx); err != nil {
				t.Fatalf("Tick: %v", err)
			}
			d.Wait()

			got, err := pg.Get(ctx, row.ID)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if got.Status != c.wantStatus || got.LastOutput != c.wantOutput {
				t.Errorf("after the tick: status=%q last_output=%q, want %q / %q.\n    A stuck verdict "+
					"taken from a snapshot was written over a row that had moved on.",
					got.Status, got.LastOutput, c.wantStatus, c.wantOutput)
			}
			if c.move == nil {
				if !strings.Contains(got.ErrorMessage, "kickoff never delivered") || len(changed) != 1 {
					t.Errorf("control: error_message=%q, OnChange calls=%v; want the stuck verdict and "+
						"one broadcast", got.ErrorMessage, changed)
				}
			} else if len(changed) != 0 {
				t.Errorf("a stuck write that did not apply was broadcast: %v", changed)
			}
		})
	}
}
