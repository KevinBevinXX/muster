package gotrue

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

var (
	testSecret = []byte("0123456789abcdef0123456789abcdef-test-only")
	testNow    = time.Unix(946684800, 0) // 2000-01-01, a fixed instant
)

func testVerifier() Verifier {
	return Verifier{Secret: testSecret, Issuer: "muster-auth", Audience: "authenticated", Now: func() time.Time { return testNow }}
}

func goodClaims() Claims {
	return Claims{
		Subject: "7d0c1c8e-1111-4222-a333-444455556666", Issuer: "muster-auth",
		Audience: Audience{"authenticated"}, IssuedAt: testNow.Unix(), ExpiresAt: testNow.Add(15 * time.Minute).Unix(),
		Role: "authenticated", SessionID: "s1",
	}
}

func sign(t *testing.T, c Claims) string {
	t.Helper()
	tok, err := Sign(testSecret, c)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// forge builds a token with an arbitrary header, signed (HMAC-SHA256) with key
// — the shape an attacker controls.
func forge(t *testing.T, hdr map[string]any, c Claims, key []byte, sigOverride *string) string {
	t.Helper()
	hb, _ := json.Marshal(hdr)
	pb, _ := json.Marshal(c)
	signing := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(pb)
	if sigOverride != nil {
		return signing + "." + *sigOverride
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte(signing))
	return signing + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func TestVerifyAcceptsAGoodToken(t *testing.T) {
	c, err := testVerifier().Verify(sign(t, goodClaims()))
	if err != nil {
		t.Fatalf("a well-formed token was refused: %v", err)
	}
	if c.Subject != goodClaims().Subject || c.SessionID != "s1" {
		t.Fatalf("claims did not round-trip: %+v", c)
	}
}

// TestVerifyRejectsEveryOtherAlg: alg is an EQUALITY with HS256, checked before
// anything is computed. Each case is signed with the REAL secret (HMAC-SHA256)
// so a verifier that ignored the header would accept it — the refusal must come
// from the alg check alone.
func TestVerifyRejectsEveryOtherAlg(t *testing.T) {
	empty := ""
	for _, tc := range []struct {
		name string
		hdr  map[string]any
		sig  *string
	}{
		{"none, empty signature", map[string]any{"alg": "none", "typ": "JWT"}, &empty},
		{"none, valid hmac", map[string]any{"alg": "none"}, nil},
		{"lower-case hs256", map[string]any{"alg": "hs256"}, nil},
		{"HS384", map[string]any{"alg": "HS384"}, nil},
		{"HS512", map[string]any{"alg": "HS512"}, nil},
		{"RS256 key confusion", map[string]any{"alg": "RS256"}, nil},
		{"ES256", map[string]any{"alg": "ES256"}, nil},
		{"no alg", map[string]any{"typ": "JWT"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tok := forge(t, tc.hdr, goodClaims(), testSecret, tc.sig)
			_, err := testVerifier().Verify(tok)
			// An empty signature segment is refused one step earlier, as
			// malformed; every other case must be refused BY the alg check.
			if !errors.Is(err, ErrAlgorithm) && !(tc.sig != nil && errors.Is(err, ErrMalformed)) {
				t.Fatalf("alg %v: got %v, want ErrAlgorithm", tc.hdr["alg"], err)
			}
		})
	}
	// Positive control: the forge helper with alg HS256 produces a token
	// Verify ACCEPTS, so the refusals above are about the header and nothing else.
	if _, err := testVerifier().Verify(forge(t, map[string]any{"alg": "HS256"}, goodClaims(), testSecret, nil)); err != nil {
		t.Fatalf("positive control: a forged-but-correct HS256 token was refused: %v", err)
	}
}

func TestVerifyRejectsATamperedSignature(t *testing.T) {
	tok := sign(t, goodClaims())
	parts := strings.Split(tok, ".")

	// Swap in a different payload (another subject) under the original signature.
	other := goodClaims()
	other.Subject = "00000000-0000-4000-a000-000000000000"
	pb, _ := json.Marshal(other)
	swapped := parts[0] + "." + base64.RawURLEncoding.EncodeToString(pb) + "." + parts[2]
	if _, err := testVerifier().Verify(swapped); !errors.Is(err, ErrSignature) {
		t.Fatalf("payload swap: got %v, want ErrSignature", err)
	}
	// Signed with another secret.
	wrong := forge(t, map[string]any{"alg": "HS256"}, goodClaims(), []byte("a-different-secret-of-enough-length!!"), nil)
	if _, err := testVerifier().Verify(wrong); !errors.Is(err, ErrSignature) {
		t.Fatalf("wrong secret: got %v, want ErrSignature", err)
	}
	// An empty verifier secret verifies nothing (HMAC with an empty key is
	// computable by anyone).
	// An empty verifier secret verifies nothing — not even a token HMAC'd with
	// the empty key, which anyone can compute.
	v := testVerifier()
	v.Secret = nil
	emptyKeyed := forge(t, map[string]any{"alg": "HS256"}, goodClaims(), nil, nil)
	if _, err := v.Verify(emptyKeyed); !errors.Is(err, ErrSignature) {
		t.Fatalf("empty secret: got %v, want ErrSignature", err)
	}
}

func TestVerifyRejectsTheWrongIssuerOrAudience(t *testing.T) {
	c := goodClaims()
	c.Issuer = "someone-else"
	if _, err := testVerifier().Verify(sign(t, c)); !errors.Is(err, ErrIssuer) {
		t.Fatalf("issuer: got %v, want ErrIssuer", err)
	}
	c = goodClaims()
	c.Audience = Audience{"service_role"}
	if _, err := testVerifier().Verify(sign(t, c)); !errors.Is(err, ErrAudience) {
		t.Fatalf("audience: got %v, want ErrAudience", err)
	}
	c.Audience = nil
	if _, err := testVerifier().Verify(sign(t, c)); !errors.Is(err, ErrAudience) {
		t.Fatalf("no audience: got %v, want ErrAudience", err)
	}
	// An array audience containing the expected value is accepted.
	c.Audience = Audience{"other", "authenticated"}
	if _, err := testVerifier().Verify(sign(t, c)); err != nil {
		t.Fatalf("array audience: %v", err)
	}
	c = goodClaims()
	c.Subject = ""
	if _, err := testVerifier().Verify(sign(t, c)); !errors.Is(err, ErrNoSubject) {
		t.Fatalf("no subject: got %v, want ErrNoSubject", err)
	}
}

// TestVerifyAppliesThirtySecondsOfSkewToExpiry measures the boundary on both
// sides: 29 s past exp still verifies, 30 s and 31 s past do not.
func TestVerifyAppliesThirtySecondsOfSkewToExpiry(t *testing.T) {
	for _, tc := range []struct {
		past time.Duration
		ok   bool
	}{{0, true}, {29 * time.Second, true}, {30 * time.Second, false}, {31 * time.Second, false}, {10 * time.Minute, false}} {
		c := goodClaims()
		c.ExpiresAt = testNow.Add(-tc.past).Unix()
		_, err := testVerifier().Verify(sign(t, c))
		if tc.ok && err != nil {
			t.Errorf("exp %s ago: refused (%v), want accepted within skew", tc.past, err)
		}
		if !tc.ok && !errors.Is(err, ErrExpired) {
			t.Errorf("exp %s ago: got %v, want ErrExpired", tc.past, err)
		}
	}
	c := goodClaims()
	c.ExpiresAt = 0
	if _, err := testVerifier().Verify(sign(t, c)); !errors.Is(err, ErrMalformed) {
		t.Fatalf("a token with no exp must be refused: got %v", err)
	}
	c = goodClaims()
	c.NotBefore = testNow.Add(31 * time.Second).Unix()
	if _, err := testVerifier().Verify(sign(t, c)); !errors.Is(err, ErrNotYet) {
		t.Fatalf("nbf 31s ahead: got %v, want ErrNotYet", err)
	}
	c.NotBefore = testNow.Add(29 * time.Second).Unix()
	if _, err := testVerifier().Verify(sign(t, c)); err != nil {
		t.Fatalf("nbf 29s ahead is within skew: %v", err)
	}
	// iat in the future: a token minted by a clock running ahead (or forged
	// ahead to outlive a not-before) is refused past the skew, accepted inside.
	c = goodClaims()
	c.IssuedAt = testNow.Add(31 * time.Second).Unix()
	if _, err := testVerifier().Verify(sign(t, c)); !errors.Is(err, ErrNotYet) {
		t.Fatalf("iat 31s ahead: got %v, want ErrNotYet", err)
	}
	c.IssuedAt = testNow.Add(29 * time.Second).Unix()
	if _, err := testVerifier().Verify(sign(t, c)); err != nil {
		t.Fatalf("iat 29s ahead is within skew: %v", err)
	}
}

func TestVerifyRejectsMalformedTokens(t *testing.T) {
	for _, tok := range []string{"", "a.b", "a.b.c.d", "!!!.e30.sig", sign(t, goodClaims()) + "x!"} {
		if _, err := testVerifier().Verify(tok); err == nil {
			t.Errorf("%q verified", tok)
		}
	}
}

// signRaw signs an arbitrary JSON payload, so a test can put any `amr` shape
// into a token that otherwise verifies.
func signRaw(t *testing.T, payload map[string]any) string {
	t.Helper()
	tok, err := Sign(testSecret, payload)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func goodPayload(amr any) map[string]any {
	c := goodClaims()
	p := map[string]any{"sub": c.Subject, "iss": c.Issuer, "aud": "authenticated", "iat": c.IssuedAt,
		"exp": c.ExpiresAt, "role": c.Role, "session_id": c.SessionID}
	if amr != nil {
		p["amr"] = amr
	}
	return p
}

// TestAMRIsReadAsTheLatestSignIn: GoTrue's measured shape (v2.189.0 and v2.197.0:
// [{"method":"password","timestamp":…}]) yields its timestamp; several entries
// (a password and a later passkey/MFA step) yield the LATEST, whatever the
// method — the timestamps below are pairwise distinct and none is the token's
// iat, so neither "first entry" nor "iat" can pass.
func TestAMRIsReadAsTheLatestSignIn(t *testing.T) {
	for name, tc := range map[string]struct {
		amr  any
		want int64
	}{
		"password (measured shape)": {[]any{map[string]any{"method": "password", "timestamp": 946684000}}, 946684000},
		"later passkey wins":        {[]any{map[string]any{"method": "password", "timestamp": 946684000}, map[string]any{"method": "webauthn", "timestamp": 946684500}}, 946684500},
		"order does not matter":     {[]any{map[string]any{"method": "totp", "timestamp": 946684700}, map[string]any{"method": "password", "timestamp": 946684100}}, 946684700},
		"an unknown method counts":  {[]any{map[string]any{"method": "something-new", "timestamp": 946684321}}, 946684321},
	} {
		c, err := testVerifier().Verify(signRaw(t, goodPayload(tc.amr)))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, ok := c.AuthenticatedAt()
		if !ok || got.Unix() != tc.want {
			t.Fatalf("%s: AuthenticatedAt = %v (%v), want %d", name, got.Unix(), ok, tc.want)
		}
		if got.Unix() == c.IssuedAt {
			t.Fatalf("%s: fixture: the sign-in time equals iat, so the test cannot tell them apart", name)
		}
	}
}

// TestAnUnreadableAMRIsUnknownAndNeverFailsTheToken: an absent `amr`, RFC
// 8176's plain strings, entries with no or a bad timestamp, and outright
// garbage all leave the token VALID (a session must not end over this claim)
// with no sign-in time.
func TestAnUnreadableAMRIsUnknownAndNeverFailsTheToken(t *testing.T) {
	for name, amr := range map[string]any{
		"absent":             nil,
		"strings (RFC 8176)": []any{"pwd", "mfa"},
		"no timestamp":       []any{map[string]any{"method": "password"}},
		"zero timestamp":     []any{map[string]any{"method": "password", "timestamp": 0}},
		"negative":           []any{map[string]any{"method": "password", "timestamp": -5}},
		"string timestamp":   []any{map[string]any{"method": "password", "timestamp": "946684000"}},
		"an object":          map[string]any{"method": "password", "timestamp": 946684000},
		"a number":           42,
	} {
		c, err := testVerifier().Verify(signRaw(t, goodPayload(amr)))
		if err != nil {
			t.Fatalf("%s: the token was refused (%v); an amr shape must never fail a token", name, err)
		}
		if at, ok := c.AuthenticatedAt(); ok {
			t.Fatalf("%s: AuthenticatedAt = %v, want unknown", name, at)
		}
	}
	// A mixed list keeps the good entry.
	c, err := testVerifier().Verify(signRaw(t, goodPayload([]any{"pwd", map[string]any{"method": "password", "timestamp": 946684222}})))
	if err != nil {
		t.Fatal(err)
	}
	if at, ok := c.AuthenticatedAt(); !ok || at.Unix() != 946684222 {
		t.Fatalf("a mixed amr lost its good entry: %v %v", at, ok)
	}
}
