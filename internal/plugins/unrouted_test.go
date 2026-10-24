package plugins

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheBackfillIsTheSweepsPredicate pins the RELATIONSHIP between migration
// 0051's backfill and the sweep: the backfill settles exactly the messages the
// sweep would judge (awaitsDisposition), so a message the sweep would ask about
// cannot be left out of the backfill and fire at the deploy. The migration is
// found by its name, not its number, which moves as other branches land.
func TestTheBackfillIsTheSweepsPredicate(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "db", "migrations", "*_plugin_message_unrouted.sql"))
	if err != nil || len(files) != 1 {
		t.Fatalf("the unrouted-message migration: %v %v; want exactly one file", files, err)
	}
	body, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	norm := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	want := norm(`UPDATE public.plugin_messages m SET settle_reason = 'pre-existing' WHERE m.settle_reason IS NULL AND ` + awaitsDisposition + `;`)
	if !strings.Contains(norm(string(body)), want) {
		t.Fatalf("%s's backfill is not the sweep's predicate; want:\n%s", filepath.Base(files[0]), want)
	}
}
