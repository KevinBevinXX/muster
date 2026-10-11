//go:build e2eterm

package main

import (
	"context"
	"net/netip"
	"strings"

	"github.com/ZacxDev/muster/internal/connectors"
)

// A BROWSER-TEST FIXTURE, COMPILED ONLY WITH `-tags e2eterm`. It points every
// connector host under `.e2e.test` at loopback and speaks plain HTTP to it, so
// e2e/tests/connectors.spec.ts can run a FAKE upstream beside the server. The
// allowlist, the grant check, the header handling and the redaction are the real
// ones; only the network policy is loosened, and only for loopback.
func init() {
	e2eConnectorPolicy = &connectors.Policy{
		AllowLoopback: true,
		InsecureHTTP:  true,
		Resolve: func(_ context.Context, host string) ([]netip.Addr, error) {
			if strings.HasSuffix(host, ".e2e.test") {
				return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
			}
			return nil, &e2eResolveErr{host}
		},
	}
}

type e2eResolveErr struct{ host string }

func (e *e2eResolveErr) Error() string {
	return "e2e resolver: " + e.host + " is not an .e2e.test host"
}
