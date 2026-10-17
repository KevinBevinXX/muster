package searchrank

import (
	"encoding/json"
	"os"
	"testing"
)

// TestTiersMatchTheSharedTable holds Tier to testdata/cases.json — the table
// the browser's copy (window.musterRank) is held to as well, by
// e2e/tests/pickers.spec.ts. Every tier, -1 included, has a row.
func TestTiersMatchTheSharedTable(t *testing.T) {
	b, err := os.ReadFile("testdata/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var table struct {
		Cases []struct {
			Label, Q, Why string
			Tier          int
		}
	}
	if err := json.Unmarshal(b, &table); err != nil {
		t.Fatal(err)
	}
	seen := map[int]bool{}
	for _, c := range table.Cases {
		seen[c.Tier] = true
		if got := Tier(c.Label, c.Q); got != c.Tier {
			t.Errorf("Tier(%q, %q) = %d, want %d (%s)", c.Label, c.Q, got, c.Tier, c.Why)
		}
	}
	for _, tier := range []int{-1, 0, 1, 2, 3} {
		if !seen[tier] {
			t.Errorf("the table has no row for tier %d", tier)
		}
	}
}
