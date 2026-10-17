package main

import "github.com/ZacxDev/muster/internal/connectors"

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
