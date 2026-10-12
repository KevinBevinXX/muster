package db

import (
	"os"
	"strings"
	"testing"
)

// TestTheAppRoleBlockIsMigration0007s: Migrate's every-boot role block and
// migration 0007's are ONE rule written twice; this pins them byte-identical so
// they cannot drift.
func TestTheAppRoleBlockIsMigration0007s(t *testing.T) {
	m, err := os.ReadFile("migrations/0007_row_level_security.sql")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(m), appRoleSQL) {
		t.Fatal("appRoleSQL is not verbatim migration 0007's role block; change both together")
	}
	if !strings.Contains(appRoleSQL, "CREATE ROLE muster_app") || !strings.Contains(appRoleSQL, "ALTER DEFAULT PRIVILEGES") {
		t.Fatalf("appRoleSQL lost a part: %q", appRoleSQL)
	}
}
