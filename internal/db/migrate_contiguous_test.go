package db_test

import (
	"fmt"
	"testing"

	"github.com/ZacxDev/muster/internal/db"
)

// TestEmbeddedMigrationsAreContiguousFrom0001: the embedded migrations are
// numbered 0001, 0002, … with no gap and no duplicate.
//
// 🔴 WHY A GAP IS A BRICK, NOT A NIT. The runner applies only versions above
// max(applied) and refuses to boot when an embedded migration sits below it
// unapplied (checkNoSkippedMigration). Branches written in parallel each take
// a number; if one with a HIGHER number merges and deploys first, the lower
// ones that merge after it can never be applied and the next deploy does not
// start. A tree whose numbers are contiguous cannot be in that state, so this
// fails the branch — before merge — that would put it there: a 0026 landing
// while 0023–0025 are missing is red here. (Duplicates are refused by the
// loader itself, so they are red here too.)
func TestEmbeddedMigrationsAreContiguousFrom0001(t *testing.T) {
	ms, err := db.EmbeddedMigrationsForTest()
	if err != nil {
		t.Fatalf("the embedded migrations do not load: %v", err)
	}
	if len(ms) < 2 {
		t.Fatalf("instrument check: %d embedded migrations found, want many", len(ms))
	}
	for i, m := range ms {
		want := i + 1
		if m.Version != want {
			t.Fatalf("migration %q is version %d, want %d: the embedded migrations must be numbered contiguously "+
				"from 0001 (a gap means a later number reached the tree before an earlier one; renumber it to the next free number)",
				m.Name, m.Version, want)
		}
		if p := fmt.Sprintf("%04d_", want); len(m.Name) < len(p) || m.Name[:len(p)] != p {
			t.Errorf("migration %q is not named %sdescription.sql", m.Name, p)
		}
	}
}
