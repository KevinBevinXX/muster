//go:build !e2emirror

package main

import (
	"testing"
	"time"
)

// TestTheShippedBuildCannotChangeItsRefresh: without the e2emirror tag — the
// build anyone installs — nothing sets the e2e refresh hook, so the host
// re-reads its windows at mirrorhost's default whatever the environment says.
// A lost `//go:build e2emirror` line on e2emirror.go turns this red.
func TestTheShippedBuildCannotChangeItsRefresh(t *testing.T) {
	t.Setenv("MUSTER_MIRROR_E2E_REFRESH", "50ms")
	if e2eRefresh != nil {
		t.Fatal("e2eRefresh is set in a build without -tags e2emirror; a shipped muster-mirror would take its refresh from MUSTER_MIRROR_E2E_REFRESH")
	}
	if got := refreshOverride(); got != time.Duration(0) {
		t.Fatalf("refreshOverride() = %s in a shipped build, want 0 (mirrorhost's default)", got)
	}
}

// TestTheShippedBuildAlwaysOffersItsCatalog: without the e2emirror tag nothing
// can make a shipped muster-mirror play an older host that hides its catalog.
func TestTheShippedBuildAlwaysOffersItsCatalog(t *testing.T) {
	t.Setenv("MUSTER_MIRROR_E2E_NO_CATALOG", "1")
	if e2eNoCatalog != nil {
		t.Fatal("e2eNoCatalog is set in a build without -tags e2emirror; a shipped muster-mirror could advertise no catalog")
	}
}
