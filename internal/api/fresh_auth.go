package api

import (
	"time"

	"github.com/ZacxDev/muster/internal/gotrue"
)

// freshAuth reports whether the person behind claims AUTHENTICATED — signed in
// with a password or a passkey — within window of now. It reads the token's
// amr timestamp, not iat: a refresh re-mints the token (new iat) but keeps the
// amr timestamp (measured, v2.189.0 and v2.197.0), so a session kept alive by
// refreshes for a week is not "fresh" however recently its token was minted.
//
// A token with no amr timestamp is never fresh, and neither is one stamped
// further in the future than the verifier's clock skew allows.
//
// ⚠ SHARED ON PURPOSE: it is the one freshness rule for any step-up gate
// (adding a passkey today), so two gates cannot disagree about what "recent"
// means. Extend it here rather than open-coding a second reading of amr.
func freshAuth(c gotrue.Claims, now time.Time, window time.Duration) bool {
	at, ok := c.AuthenticatedAt()
	if !ok {
		return false
	}
	return !at.Before(now.Add(-window)) && !at.After(now.Add(gotrue.DefaultSkew))
}
