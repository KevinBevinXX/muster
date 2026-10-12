// Package stepup is the "prove it's you again" gate: an action that is worth
// more than an ordinary signed-in request — typing into a mirrored host's tmux,
// which is a shell on that host — needs a sign-in that happened RECENTLY, not
// merely a session that is still valid.
//
// The request's authentication time is put on its context by internal/api's
// session wrapper (WithAuthTime) and read by whoever gates an action (Fresh):
// today internal/api's mirror terminal socket and, independently, the mirror
// plugin's own Attach (internal/mirror). Neither trusts the other to have
// checked.
//
// 🔴 THE TIME IS THE SIGN-IN'S, NEVER THE TOKEN'S. A GoTrue access token is
// re-minted by every refresh, so its `iat` says nothing about when the person
// last typed a password; its `amr` timestamp does (gotrue.Claims.
// AuthenticatedAt, measured on v2.189.0). In password mode it is the session
// cookie's issue time, which only a sign-in mints.
//
// 🔴 UNKNOWN IS STALE. A context with no authentication time — a token without
// `amr`, a machine token, no session at all — is never fresh.
package stepup

import (
	"context"
	"errors"
	"time"
)

// MaxAge is how long a sign-in stays fresh enough for a step-up action.
const MaxAge = 5 * time.Minute

// ErrStale is the refusal for a step-up action on a session that did not sign
// in within MaxAge. Its text is shown to the person, so it names the remedy.
var ErrStale = errors.New("confirm it's you: typing into this session needs a sign-in from the last 5 minutes")

type authTimeKey struct{}

// WithAuthTime records that the request's person authenticated at t.
func WithAuthTime(ctx context.Context, t time.Time) context.Context {
	return context.WithValue(ctx, authTimeKey{}, t)
}

// AuthTime is the request's authentication time, and false when none was
// recorded.
func AuthTime(ctx context.Context) (time.Time, bool) {
	t, ok := ctx.Value(authTimeKey{}).(time.Time)
	if !ok || t.IsZero() {
		return time.Time{}, false
	}
	return t, true
}

// Fresh reports whether the request's person authenticated within MaxAge of
// now. Unknown is not fresh.
func Fresh(ctx context.Context, now time.Time) bool {
	t, ok := AuthTime(ctx)
	return ok && now.Sub(t) <= MaxAge
}

// FreshUntil is the instant the request's sign-in stops being fresh — zero when
// there is no authentication time. The terminal page uses it to decide whether
// to ask before attaching; the server re-checks regardless.
func FreshUntil(ctx context.Context) time.Time {
	t, ok := AuthTime(ctx)
	if !ok {
		return time.Time{}
	}
	return t.Add(MaxAge)
}
