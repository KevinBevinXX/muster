//go:build !e2eterm

package main

import "testing"

// TestTheDefaultBuildCarriesNoFakeStepUp: without the e2eterm tag — the build
// the Dockerfile ships — nothing sets the e2e step-up hook, so no file on disk
// can make a rendered-view send pass the step-up.
func TestTheDefaultBuildCarriesNoFakeStepUp(t *testing.T) {
	if e2eMirrorStepUp != nil {
		t.Fatal("e2eMirrorStepUp is set in a build without -tags e2eterm; a deployable binary would carry a fake step-up")
	}
}
