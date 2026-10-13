// Package stepup is the "prove it's you again" gate: an action that is worth
// more than an ordinary signed-in request — typing into a mirrored host's tmux,
// which is a shell on that host — needs a sign-in that happened RECENTLY, not
// merely a session that is still valid.
//
// The request's authentication time is put on its context by internal/api's
// session wrapper (WithAuthTime) and read by whoever gates an action (Fresh):
// today internal/api's mirror terminal socket and its two send routes, and,
// independently, the mirror plugin's own Attach and Paste (internal/mirror).
// Neither trusts the other to have checked.
//
// 🔴 THE TIME IS THE SIGN-IN'S, NEVER THE TOKEN'S. A GoTrue access token is
// re-minted by every refresh, so its `iat` says nothing about when the person
// last proved who they are; its `amr` timestamp does (gotrue.Claims.
// AuthenticatedAt, measured on v2.189.0 and v2.197.0). In password mode it is the session
// cookie's issue time, which only a sign-in mints.
//
// 🔴 FRESHNESS BELONGS TO ONE SESSION — ONE BROWSER ON ONE DEVICE — NEVER TO
// THE USER. The time rides in that session's own credential (the GoTrue
// session's `amr`, carried by its access cookie; in password mode the session
// cookie itself), and nothing here or in internal/api keeps a per-user "last
// confirmed" anywhere. Confirming on a laptop therefore leaves the same user's
// phone exactly as stale as it was (TestAConfirmOnOneSessionLeavesAnotherStale,
// internal/api). Keep it that way: a per-user clock would let one device's
// confirm unlock every other signed-in device, including a lost one.
//
// 🔴 UNKNOWN IS STALE. A context with no authentication time — a token without
// `amr`, a machine token, no session at all — is never fresh.
package stepup

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// The window: how long a sign-in stays fresh enough for a step-up action.
//
// It is ONE value for the whole process, read through Window by every gate and
// by every sentence that names it to a person (WindowText) — there is no second
// copy to drift. DefaultWindow unless the deployment configures one
// (MUSTER_STEPUP_WINDOW, read by cmd/muster-server's config → SetWindow).
const (
	// DefaultWindow is the window when nothing configures one.
	DefaultWindow = 12 * time.Hour
	// MinWindow and MaxWindow bound a configured window (ParseWindow). Below a
	// minute the confirm dialog could not be answered before it expired again;
	// above a day the gate would no longer mean "recently".
	MinWindow = time.Minute
	MaxWindow = 24 * time.Hour
)

// CredentialMaxAge is the SHORTER, FIXED window for an action that changes how
// the account can be signed in to — today, adding a passkey (internal/api).
//
// 🔴 IT IS DELIBERATELY NOT THE CONFIGURABLE WINDOW. The window above was
// lengthened so that typing into one's own session is not a password wall on
// every message; a new credential is a different stake — it outlives the
// session that minted it — so a session left open for hours must not be able to
// mint one. It does not follow the configured window.
const CredentialMaxAge = 5 * time.Minute

// window holds the configured window in nanoseconds; 0 means DefaultWindow.
var window atomic.Int64

// Window is how long a sign-in stays fresh enough for a step-up action.
func Window() time.Duration {
	if d := time.Duration(window.Load()); d > 0 {
		return d
	}
	return DefaultWindow
}

// SetWindow configures the window for this process. It refuses anything
// outside [MinWindow, MaxWindow] and leaves the window as it was.
func SetWindow(d time.Duration) error {
	if err := checkWindow(d); err != nil {
		return err
	}
	window.Store(int64(d))
	return nil
}

func checkWindow(d time.Duration) error {
	if d < MinWindow || d > MaxWindow {
		return fmt.Errorf("stepup: a window of %s is outside %s–%s", d, MinWindow, MaxWindow)
	}
	return nil
}

// ParseWindow reads a configured window: "" is DefaultWindow; anything else
// must be a Go duration ("12h", "45m", "1h30m") within [MinWindow, MaxWindow].
// Zero, a negative duration, a bare number and an over-long window are all
// errors — a deployment that typed the variable wrong must fail to start rather
// than run with a gate it did not choose.
func ParseWindow(v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return DefaultWindow, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration (use e.g. 12h, 45m)", v)
	}
	if err := checkWindow(d); err != nil {
		return 0, fmt.Errorf("%q must be between %s and %s", v, HumanDuration(MinWindow), HumanDuration(MaxWindow))
	}
	return d, nil
}

// WindowText is the window as a person reads it ("12 hours"): what every
// user-facing sentence about the gate is built from.
func WindowText() string { return HumanDuration(Window()) }

// HumanDuration writes d in whole hours, minutes and seconds, largest first,
// omitting the zero parts: "12 hours", "1 hour 30 minutes", "5 minutes".
func HumanDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d <= 0 {
		return "0 seconds"
	}
	var parts []string
	unit := func(n int64, name string) {
		if n == 1 {
			parts = append(parts, "1 "+name)
		} else if n > 1 {
			parts = append(parts, strconv.FormatInt(n, 10)+" "+name+"s")
		}
	}
	unit(int64(d/time.Hour), "hour")
	unit(int64(d%time.Hour/time.Minute), "minute")
	unit(int64(d%time.Minute/time.Second), "second")
	return strings.Join(parts, " ")
}

// staleError is ErrStale's type: its text names the CURRENT window, so it is
// computed when read rather than frozen when the package loaded.
type staleError struct{}

func (staleError) Error() string {
	return "confirm it's you: typing into this session needs a sign-in from the last " + WindowText()
}

// ErrStale is the refusal for a step-up action on a session that did not sign
// in within Window. Its text is shown to the person, so it names the remedy.
//
// ⚠ WRAP IT WITH A TYPE THAT UNWRAPS TO IT, NOT WITH fmt.Errorf("%w"): Errorf
// formats its message once, so a wrapper built at package load would keep
// naming the default window after SetWindow changed it (mirror.ErrStepUpRequired
// is the worked example).
var ErrStale error = staleError{}

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

// Fresh reports whether the request's person authenticated within Window of
// now. Unknown is not fresh.
func Fresh(ctx context.Context, now time.Time) bool {
	return FreshWithin(ctx, now, Window())
}

// FreshWithin reports whether the request's person authenticated within maxAge
// of now. Unknown is not fresh. Fresh is this with Window; the one other caller
// passes CredentialMaxAge.
func FreshWithin(ctx context.Context, now time.Time, maxAge time.Duration) bool {
	t, ok := AuthTime(ctx)
	return ok && now.Sub(t) <= maxAge
}

// FreshUntil is the instant the request's sign-in stops being fresh — zero when
// there is no authentication time. The terminal page uses it to decide whether
// to ask before attaching; the server re-checks regardless.
func FreshUntil(ctx context.Context) time.Time {
	t, ok := AuthTime(ctx)
	if !ok {
		return time.Time{}
	}
	return t.Add(Window())
}
