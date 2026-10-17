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

// TestTheDefaultBuildCarriesNoUpstreamHooks: without the e2eterm tag, the hooks
// that point GitHub and OpenRouter at a browser test's fakes
// (e2eupstreams.go) are not compiled in. Its init sets e2eUpstreamHooks, so a
// lost `//go:build e2eterm` line turns this red rather than shipping a binary
// whose upstreams an environment variable can redirect.
func TestTheDefaultBuildCarriesNoUpstreamHooks(t *testing.T) {
	if e2eUpstreamHooks {
		t.Fatal("e2eupstreams.go's init ran in a build without -tags e2eterm; a deployable binary would let " +
			"MUSTER_E2E_GITHUB_API / MUSTER_E2E_OPENROUTER_MODELS redirect its upstreams")
	}
}
