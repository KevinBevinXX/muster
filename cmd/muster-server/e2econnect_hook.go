package main

import (
	"time"

	"github.com/ZacxDev/muster/internal/connectors"
)

// e2eConnectorPolicy, when non-nil, replaces the connector proxy's strict
// production policy. ONLY a binary built with `-tags e2eterm` sets it
// (e2econnect.go), so the browser suite can proxy to a FAKE upstream on loopback
// over plain HTTP. TestNoDeployableBinaryLoosensTheConnectorPolicy pins that a
// default build leaves it nil.
var e2eConnectorPolicy *connectors.Policy

// e2eUpstreamHooks is true when the e2e upstream hooks (e2eupstreams.go: a fake
// GitHub API and a fake OpenRouter catalogue) are compiled in. ONLY a binary
// built with `-tags e2eterm` sets it; TestTheDefaultBuildCarriesNoUpstreamHooks
// pins that a default build leaves it false.
var e2eUpstreamHooks bool

// e2eGoTrueSkew, when non-nil, is the GoTrue verifier's clock skew (0 =
// gotrue.DefaultSkew). ONLY a binary built with `-tags e2eterm` sets it
// (e2egotrue.go, from MUSTER_E2E_GOTRUE_SKEW), so the browser specs that
// outlive an access token need not wait out 30 s of production skew.
// TestTheDefaultBuildCannotChangeTheGoTrueSkew pins that a default build
// leaves it nil — and so verifies with gotrue.DefaultSkew whatever the
// environment says.
var e2eGoTrueSkew func() time.Duration

// goTrueSkew is the verifier's skew: an e2e build's override, else 0
// (gotrue.DefaultSkew).
func goTrueSkew() time.Duration {
	if e2eGoTrueSkew == nil {
		return 0
	}
	return e2eGoTrueSkew()
}
