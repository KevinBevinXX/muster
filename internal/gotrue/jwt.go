// Package gotrue is muster's client for its own GoTrue instance (accounts PR 4):
// the password and refresh grants, logout, the admin user calls, and — the part
// every request depends on — Verify, which decides whether an access token
// GoTrue minted is one muster will act on.
//
// 🔴 THE BROWSER NEVER TALKS TO GoTrue. GoTrue is ClusterIP-only; muster calls
// it server-side and keeps the tokens in HttpOnly cookies. That is why muster,
// not GoTrue, is the verifier of record, and why Verify is strict.
//
// 🔴 HS256 IS THE ONLY ALGORITHM, AND THE CHECK IS AN EQUALITY, NOT A LOOKUP.
// GoTrue signs with one shared secret; muster holds the same secret. A verifier
// that consults the token's own `alg` header to choose how to check it is the
// classic JWT hole — `alg: none` skips the check, and an asymmetric `alg` makes
// the shared secret a public key. Verify refuses any header whose alg is not the
// literal "HS256" BEFORE it computes anything (TestVerifyRejectsEveryOtherAlg).
//
// No JWT library is used: the format muster accepts is one algorithm and a
// handful of claims, and a library's flexibility is exactly what Verify must not
// have.
package gotrue

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Algorithm is the one JWS algorithm Verify accepts.
const Algorithm = "HS256"

// DefaultSkew is the clock tolerance Verify applies to exp (and nbf/iat).
const DefaultSkew = 30 * time.Second

// Verify's refusals. They are coarse on purpose: a caller treats every one as
// "no session", and only ErrExpired is acted on differently (a refresh is
// attempted). None of them is ever shown to the client.
var (
	ErrMalformed = errors.New("gotrue: malformed token")
	ErrAlgorithm = errors.New("gotrue: token algorithm is not " + Algorithm)
	ErrSignature = errors.New("gotrue: token signature does not verify")
	ErrIssuer    = errors.New("gotrue: token issuer is not the expected one")
	ErrAudience  = errors.New("gotrue: token audience is not the expected one")
	ErrExpired   = errors.New("gotrue: token expired")
	ErrNotYet    = errors.New("gotrue: token not valid yet")
	ErrNoSubject = errors.New("gotrue: token names no subject")
)

// Audience is a JWT `aud`, which the spec allows to be a string or an array.
type Audience []string

// UnmarshalJSON accepts both shapes.
func (a *Audience) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*a = Audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

// MarshalJSON writes a single audience as a string, as GoTrue does.
func (a Audience) MarshalJSON() ([]byte, error) {
	if len(a) == 1 {
		return json.Marshal(a[0])
	}
	return json.Marshal([]string(a))
}

// Claims is what muster reads from a GoTrue access token.
type Claims struct {
	Subject   string   `json:"sub,omitempty"`
	Issuer    string   `json:"iss,omitempty"`
	Audience  Audience `json:"aud,omitempty"`
	ExpiresAt int64    `json:"exp,omitempty"`
	IssuedAt  int64    `json:"iat,omitempty"`
	NotBefore int64    `json:"nbf,omitempty"`
	Role      string   `json:"role,omitempty"`
	Email     string   `json:"email,omitempty"`
	// SessionID is GoTrue's session the token belongs to. muster's own logout
	// revocation is keyed on it (see internal/api's sessionRevocations).
	SessionID string `json:"session_id,omitempty"`
	// AMR is how — and WHEN — the session's user authenticated: GoTrue's
	// `amr` claim, one entry per method used ({"method":"password",
	// "timestamp":…}). It is what the step-up gate reads (AuthenticatedAt).
	AMR AMR `json:"amr,omitempty"`
}

// AMREntry is one authentication event of a session: the method ("password",
// "otp", a passkey/webauthn method, …) and the unix second it happened.
type AMREntry struct {
	Method    string `json:"method"`
	Timestamp int64  `json:"timestamp"`
}

// AMR is GoTrue's `amr` claim.
//
// MEASURED on GoTrue v2.189.0 (the deployment's pin): a password grant mints
// `[{"method":"password","timestamp":<sign-in second>}]`, and a refresh grant
// carries the SAME entry forward unchanged while `iat` moves on — so the
// timestamp is when the person last proved who they are, not when this token
// was minted. A new sign-in is a new session with a new timestamp.
//
// 🔴 PARSING IT NEVER FAILS A TOKEN. Verify decodes the whole payload in one
// json.Unmarshal; an `amr` shaped differently from what was measured (RFC 8176's
// plain strings, a future GoTrue's extra fields, garbage) must not turn every
// signed-in browser into ErrMalformed — i.e. sign everyone out. Anything that is
// not an object with a positive integer timestamp is dropped, and an absent or
// unreadable claim is simply an empty AMR: "authentication time unknown", which
// the step-up gate treats as NOT fresh (fail closed for that gate only).
type AMR []AMREntry

// UnmarshalJSON keeps every well-formed entry and drops the rest; it never
// returns an error.
func (a *AMR) UnmarshalJSON(b []byte) error {
	*a = nil
	var raw []json.RawMessage
	if json.Unmarshal(b, &raw) != nil {
		return nil
	}
	for _, r := range raw {
		var e AMREntry
		if json.Unmarshal(r, &e) != nil || e.Timestamp <= 0 {
			continue
		}
		*a = append(*a, e)
	}
	return nil
}

// AuthenticatedAt is the most recent authentication the token records — the
// latest `amr` timestamp, whatever its method — and false when it records none.
//
// ANY METHOD COUNTS, by design: the gate asks "did this person prove who they
// are recently", and a passkey (webauthn) sign-in answers that as well as a
// password does. A method muster has never heard of still counts, because GoTrue
// signed it.
func (c Claims) AuthenticatedAt() (time.Time, bool) {
	var latest int64
	for _, e := range c.AMR {
		if e.Timestamp > latest {
			latest = e.Timestamp
		}
	}
	if latest == 0 {
		return time.Time{}, false
	}
	return time.Unix(latest, 0), true
}

// Verifier checks access tokens against one secret, issuer and audience.
type Verifier struct {
	Secret   []byte
	Issuer   string
	Audience string
	// Skew is the clock tolerance; zero means DefaultSkew.
	Skew time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

type header struct {
	Alg string `json:"alg"`
	Typ string `json:"typ,omitempty"`
}

// Verify parses and checks token, returning its claims.
//
// Order, and why it is this order: the alg is checked first (nothing else is
// computed for a token muster would never accept); then the signature, with
// hmac.Equal, BEFORE any claim is trusted; then iss, aud, sub; then time.
// A token that is merely expired returns its claims WITH ErrExpired, so the
// caller can log who it was — it must still not act on them.
func (v Verifier) Verify(token string) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return Claims{}, ErrMalformed
	}
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Claims{}, ErrMalformed
	}
	var h header
	if err := json.Unmarshal(hb, &h); err != nil {
		return Claims{}, ErrMalformed
	}
	if h.Alg != Algorithm {
		return Claims{}, ErrAlgorithm
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, ErrMalformed
	}
	if len(v.Secret) == 0 || !hmac.Equal(sig, mac(v.Secret, parts[0]+"."+parts[1])) {
		return Claims{}, ErrSignature
	}
	pb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, ErrMalformed
	}
	var c Claims
	if err := json.Unmarshal(pb, &c); err != nil {
		return Claims{}, ErrMalformed
	}
	if c.Issuer != v.Issuer {
		return Claims{}, ErrIssuer
	}
	if !c.hasAudience(v.Audience) {
		return Claims{}, ErrAudience
	}
	if c.Subject == "" {
		return Claims{}, ErrNoSubject
	}
	now := time.Now()
	if v.Now != nil {
		now = v.Now()
	}
	skew := v.Skew
	if skew <= 0 {
		skew = DefaultSkew
	}
	// exp is REQUIRED: a token with no expiry would be a credential for ever.
	if c.ExpiresAt == 0 {
		return Claims{}, ErrMalformed
	}
	if !now.Before(time.Unix(c.ExpiresAt, 0).Add(skew)) {
		return c, ErrExpired
	}
	if c.NotBefore != 0 && now.Add(skew).Before(time.Unix(c.NotBefore, 0)) {
		return Claims{}, ErrNotYet
	}
	if c.IssuedAt != 0 && now.Add(skew).Before(time.Unix(c.IssuedAt, 0)) {
		return Claims{}, ErrNotYet
	}
	return c, nil
}

func (c Claims) hasAudience(want string) bool {
	for _, a := range c.Audience {
		if a == want {
			return true
		}
	}
	return false
}

// Sign mints an HS256 token over claims. muster uses it for the 60-second
// service_role token its admin calls carry; the fake issuer uses it for
// everything it hands out.
func Sign(secret []byte, claims any) (string, error) {
	if len(secret) == 0 {
		return "", errors.New("gotrue: refusing to sign with an empty secret")
	}
	hb, err := json.Marshal(header{Alg: Algorithm, Typ: "JWT"})
	if err != nil {
		return "", err
	}
	pb, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("gotrue: encode claims: %w", err)
	}
	signing := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(pb)
	return signing + "." + base64.RawURLEncoding.EncodeToString(mac(secret, signing)), nil
}

func mac(secret []byte, signing string) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(signing))
	return m.Sum(nil)
}
