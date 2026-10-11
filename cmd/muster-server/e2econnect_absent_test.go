//go:build !e2eterm

package main

import "testing"

// TestNoDeployableBinaryLoosensTheConnectorPolicy: a default build must carry the
// strict (zero) connector policy — HTTPS only, no loopback, the system resolver.
func TestNoDeployableBinaryLoosensTheConnectorPolicy(t *testing.T) {
	if e2eConnectorPolicy != nil {
		t.Fatal("e2eConnectorPolicy is set in a build without -tags e2eterm; a deployable binary " +
			"would proxy connector credentials to loopback over plain HTTP")
	}
}
