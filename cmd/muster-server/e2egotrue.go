//go:build e2eterm

package main

import (
	"fmt"
	"os"
	"time"
)

// A BROWSER-TEST FIXTURE, COMPILED ONLY WITH `-tags e2eterm`: the GoTrue
// verifier's clock skew from MUSTER_E2E_GOTRUE_SKEW. The specs that outlive an
// access token (gotrue-login, passkeys, invites) run GoTrue with 5 s tokens; at
// the production skew (gotrue.DefaultSkew, 30 s) each had to wait 37 s before
// muster would refuse the token and take the refresh path. With a 1 s skew the
// same path is reached ~7 s after the token was minted. Unset, the default
// stands. A value that is not a positive duration refuses to start.
func init() {
	e2eGoTrueSkew = func() time.Duration {
		v := os.Getenv(envE2EGoTrueSkew)
		if v == "" {
			return 0
		}
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			panic(fmt.Sprintf("muster-server (e2eterm): %s=%q is not a positive duration", envE2EGoTrueSkew, v))
		}
		return d
	}
}
