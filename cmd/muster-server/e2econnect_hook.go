package main

import "github.com/ZacxDev/muster/internal/connectors"

// e2eConnectorPolicy, when non-nil, replaces the connector proxy's strict
// production policy. ONLY a binary built with `-tags e2eterm` sets it
// (e2econnect.go), so the browser suite can proxy to a FAKE upstream on loopback
// over plain HTTP. TestNoDeployableBinaryLoosensTheConnectorPolicy pins that a
// default build leaves it nil.
var e2eConnectorPolicy *connectors.Policy
