package agents

import (
	"context"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestAppendPendingNoteOnlyWhileTheFirstTurnIsOwed (task routing, audit round
// 2): a forward rides an agent's first turn only while that turn is owed — a
// non-empty first message and kicked_off false. An empty note, or a stamped
// one, refuses (false) and changes nothing; the dwell clock is not moved.
func TestAppendPendingNoteOnlyWhileTheFirstTurnIsOwed(t *testing.T) {
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 30*time.Second)
	defer cancel()
	store := requirePGStore(t, ctx)
	mk := func(suffix, note string) Agent {
		t.Helper()
		name := "append-note-" + suffix + "-" + time.Now().Format("150405.000000")
		a, err := store.Create(ctx, Agent{Name: name, Namespace: "muster-agent-" + name, Status: StatusProvisioning, PendingNote: note})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		t.Cleanup(func() { cleanupAgent(ctx, store.pool, a.ID) })
		if _, err := store.pool.Exec(ctx, `UPDATE agents SET updated_at = now() - interval '14 minutes' WHERE id=$1`, a.ID); err != nil {
			t.Fatal(err)
		}
		got, _ := store.Get(ctx, a.ID)
		return got
	}
	owed := mk("owed", "first")
	if ok, err := store.AppendPendingNote(ctx, owed.ID, " + forward"); err != nil || !ok {
		t.Fatalf("an owed first turn: %v %v; want appended", ok, err)
	}
	if got, _ := store.Get(ctx, owed.ID); got.PendingNote != "first + forward" || !got.UpdatedAt.Equal(owed.UpdatedAt) {
		t.Fatalf("after the append: note %q, updated_at moved %v; want the note grown, the clock still", got.PendingNote, !got.UpdatedAt.Equal(owed.UpdatedAt))
	}
	empty := mk("empty", "")
	if ok, err := store.AppendPendingNote(ctx, empty.ID, " + forward"); err != nil || ok {
		t.Fatalf("no first message owed: %v %v; want refused", ok, err)
	}
	if err := store.SetKickedOff(ctx, owed.ID, true); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.AppendPendingNote(ctx, owed.ID, " + late"); err != nil || ok {
		t.Fatalf("after the stamp: %v %v; want refused", ok, err)
	}
	if got, _ := store.Get(ctx, owed.ID); got.PendingNote != "first + forward" {
		t.Fatalf("a refused append changed the note: %q", got.PendingNote)
	}
}
