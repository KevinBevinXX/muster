//go:build !e2eterm

package main

import "testing"

// TestTheDefaultBuildCarriesNoFakeTerminal: without the e2eterm tag — the
// build the Dockerfile ships — nothing sets the e2e terminal hook, so the noop
// driver is returned unwrapped and cannot exec.
func TestTheDefaultBuildCarriesNoFakeTerminal(t *testing.T) {
	if e2eFakeTerminal != nil {
		t.Fatal("e2eFakeTerminal is set in a build without -tags e2eterm; a deployable binary would carry a fake terminal")
	}
}
