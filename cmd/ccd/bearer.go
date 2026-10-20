package main

import (
	"net/http"

	"github.com/ZacxDev/muster/internal/podagent"
)

// resolveBearer decides the credential /v1/responses accepts: podagent's rule
// (the hooks-sha256 derivation is muster's own agentgateway function, imported,
// not a copy), shared with hermesd.
func resolveBearer(hooksToken, gatewayBearer string) (string, error) {
	return podagent.ResolveBearer("ccd", hooksToken, gatewayBearer)
}

// bearerOK reports whether r carries `Authorization: Bearer <want>`, compared in
// constant time (podagent.BearerOK).
func bearerOK(r *http.Request, want string) bool { return podagent.BearerOK(r, want) }
