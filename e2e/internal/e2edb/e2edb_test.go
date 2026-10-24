package e2edb

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestCheckRefusesAnythingButAnE2EDatabase pins the guard that keeps every e2e
// fixture command, the browser suite AND the manifest screenshots on fixture
// data: `npm run capture` shares global-setup, which runs e2e/seed (and so this
// check) before it starts the server it photographs.
func TestCheckRefusesAnythingButAnE2EDatabase(t *testing.T) {
	// pgconn fills an unnamed database from the environment; keep the cases
	// about the DSN alone.
	for _, k := range []string{"PGDATABASE", "PGSERVICE", "PGSERVICEFILE"} {
		t.Setenv(k, "")
	}
	for _, c := range []struct {
		dsn string
		ok  bool
	}{
		{"postgres://muster:muster@127.0.0.1:5432/muster_e2e?sslmode=disable", true},
		{"postgres://u:p@db/x_e2e", true},
		{"host=db user=u dbname=x_e2e", true},
		{"", false},
		{"postgres://muster:muster@postgres.muster.svc:5432/muster?sslmode=disable", false},
		{"postgres://muster:muster@127.0.0.1:55432/muster_test?sslmode=disable", false},
		{"postgres://u:p@db/muster_e2e_old", false},
		{"postgres://u:p@db/", false},
		{"host=db user=u dbname=postgres", false},
		// 🔴 pgx connects to the QUERY's database, not the path's: the guard
		// must refuse these, and accept the reverse.
		{"postgres://u:p@db/x_e2e?dbname=postgres", false},
		{"postgres://u:p@db/x_e2e?database=postgres", false},
		{"postgres://u:p@db/muster?dbname=muster_e2e", true},
	} {
		err := Check(c.dsn)
		if (err == nil) != c.ok {
			t.Errorf("Check(%q) = %v, want ok=%v", c.dsn, err, c.ok)
		}
	}
}

// fixtureCommands is every e2e command that reads DATABASE_URL. An asserted
// ledger: the test fails when the set grows OR shrinks, so a new fixture is a
// decision to route it through Check, not a copy of the guard.
var fixtureCommands = []string{"devicefix/main.go", "heldfix/main.go", "mkchat/main.go", "mkmember/main.go", "pluginfix/main.go", "seed/main.go", "sqfix/main.go"}

// TestEveryE2ECommandUsesTheOneGuard: each command that reads DATABASE_URL
// calls Check, and none parses the DSN itself (the path check this package
// replaced).
func TestEveryE2ECommandUsesTheOneGuard(t *testing.T) {
	root := filepath.Join("..", "..") // e2e/
	var got []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == "node_modules" {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		src := string(b)
		if !strings.Contains(src, `"DATABASE_URL"`) {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		got = append(got, filepath.ToSlash(rel))
		if !strings.Contains(src, "e2edb.Check(") {
			t.Errorf("%s reads DATABASE_URL but never calls e2edb.Check", rel)
		}
		for _, line := range strings.Split(src, "\n") {
			if strings.Contains(line, "url.Parse(") || (strings.Contains(line, "HasSuffix(") && strings.Contains(line, "_e2e")) {
				t.Errorf("%s checks the database name itself (%q); route it through e2edb.Check", rel, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	// Positive control: the walk found the commands at all.
	if strings.Join(got, ",") != strings.Join(fixtureCommands, ",") {
		t.Fatalf("commands reading DATABASE_URL = %v, want the ledger %v", got, fixtureCommands)
	}
}
