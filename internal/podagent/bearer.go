package podagent

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	"github.com/ZacxDev/muster/internal/agentgateway"
)

// ResolveBearer decides the credential a pod supervisor's /v1/responses accepts.
// who names the supervisor in the error ("ccd").
//
// 🔴 THE DERIVATION IS muster's OWN FUNCTION, IMPORTED, NOT A COPY. The
// supervisors live in muster's module precisely so the `hooks-sha256` formula
// cannot drift between the side that sends the bearer (agentgateway.Gateway)
// and the side that checks it.
//
// muster's agent spec ships BOTH the raw token (HOOKS_TOKEN) and the derived
// value (MUSTER_GATEWAY_BEARER). Either is enough; when both are present they
// must agree, because a pod holding two credentials that disagree would accept
// one and 401 the other, and which one muster sends is not this process's
// decision.
func ResolveBearer(who, hooksToken, gatewayBearer string) (string, error) {
	derived := ""
	if hooksToken != "" {
		derived = agentgateway.HooksSHA256().Bearer(hooksToken)
	}
	switch {
	case gatewayBearer == "" && derived == "":
		return "", errors.New(who + ": neither MUSTER_GATEWAY_BEARER nor HOOKS_TOKEN is set, so no request " +
			"could ever be authenticated; refusing to serve an endpoint nobody can call (or that a " +
			"later default would open to everyone)")
	case gatewayBearer == "":
		return derived, nil
	case derived != "" && derived != gatewayBearer:
		return "", errors.New(who + ": MUSTER_GATEWAY_BEARER does not equal the hooks-sha256 derivation of " +
			"HOOKS_TOKEN; muster derives one from the other, so this pod's secrets are mismatched and " +
			"every turn would 401")
	default:
		return gatewayBearer, nil
	}
}

// BearerOK reports whether r carries `Authorization: Bearer <want>`.
//
// The comparison is over SHA-256 digests with subtle.ConstantTimeCompare, so it
// takes the same time whatever the presented value's length or content.
func BearerOK(r *http.Request, want string) bool {
	h := r.Header.Get("Authorization")
	const prefix = "bearer "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return false
	}
	got := strings.TrimSpace(h[len(prefix):])
	if got == "" || want == "" {
		return false
	}
	g := sha256.Sum256([]byte(got))
	w := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(g[:], w[:]) == 1
}
