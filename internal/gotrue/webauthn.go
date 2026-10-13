package gotrue

// --- what muster checks in a WebAuthn credential before GoTrue sees it ------
//
// 🔴 USER VERIFICATION IS ENFORCED HERE, NOT BY GoTrue. GoTrue v2.197.0 builds
// its WebAuthn config with UserVerification "preferred" (passkey_webauthn.go,
// not configurable), so a credential whose authenticator did NOT verify the
// person — no PIN, biometric or device unlock, just presence — verifies there.
// For muster a passkey replaces a password, so a passkey without UV is "anyone
// holding the device". muster therefore reads the authenticator-data flags of
// every credential and refuses one without UV (0x04) BEFORE forwarding it
// (TestAPasskeyWithoutUserVerificationIsRefused).
//
// 🔴 WHAT IS CHECKED IS WHAT IS FORWARDED. A JSON object can name a key twice,
// and two parsers can disagree about which one wins; a check made on one
// reading and a signature verified on another would be no check. So the
// credential is decoded into the fields WebAuthn defines and RE-ENCODED, and
// that re-encoding — not the browser's bytes — is what goes to GoTrue. The
// authenticatorData muster read the flags from is then the authenticatorData
// GoTrue verifies the signature over (TestTheForwardedCredentialIsTheCheckedOne).

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

// Authenticator-data flag bits (WebAuthn §6.1).
const (
	flagUP = 0x01 // user present
	flagUV = 0x04 // user verified
	flagBE = 0x08 // backup eligible (a credential that CAN sync)
	flagBS = 0x10 // backed up (a credential that IS synced)
)

// AuthFlags are the authenticator-data flags muster acts on.
type AuthFlags struct {
	UserPresent, UserVerified, BackupEligible, BackedUp bool
}

func flagsOf(authData []byte) (AuthFlags, error) {
	// rpIdHash (32) + flags (1) + signCount (4).
	if len(authData) < 37 {
		return AuthFlags{}, ErrBadCredential
	}
	f := authData[32]
	return AuthFlags{UserPresent: f&flagUP != 0, UserVerified: f&flagUV != 0,
		BackupEligible: f&flagBE != 0, BackedUp: f&flagBS != 0}, nil
}

// ErrBadCredential: the browser's credential is not a WebAuthn credential
// muster can read. Nothing is forwarded.
var ErrBadCredential = errors.New("gotrue: the passkey credential could not be read")

// b64url is a WebAuthn binary field: base64url, padding tolerated on input
// (as go-webauthn tolerates it), never written.
type b64url []byte

func (b *b64url) UnmarshalJSON(raw []byte) error {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return err
	}
	v, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	if err != nil {
		return err
	}
	*b = v
	return nil
}

func (b b64url) MarshalJSON() ([]byte, error) {
	return json.Marshal(base64.RawURLEncoding.EncodeToString(b))
}

type assertionResponse struct {
	ClientDataJSON    b64url `json:"clientDataJSON"`
	AuthenticatorData b64url `json:"authenticatorData"`
	Signature         b64url `json:"signature"`
	UserHandle        b64url `json:"userHandle,omitempty"`
}

type attestationResponse struct {
	ClientDataJSON    b64url   `json:"clientDataJSON"`
	AttestationObject b64url   `json:"attestationObject"`
	Transports        []string `json:"transports,omitempty"`
}

type credential[R any] struct {
	ID                      string          `json:"id"`
	RawID                   b64url          `json:"rawId"`
	Type                    string          `json:"type"`
	AuthenticatorAttachment string          `json:"authenticatorAttachment,omitempty"`
	Response                R               `json:"response"`
	ClientExtensionResults  json.RawMessage `json:"clientExtensionResults,omitempty"`
}

func decodeCredential[R any](raw json.RawMessage) (credential[R], error) {
	var c credential[R]
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, ErrBadCredential
	}
	if c.Type != "public-key" || c.ID == "" || len(c.RawID) == 0 {
		return c, ErrBadCredential
	}
	if len(c.ClientExtensionResults) == 0 || string(c.ClientExtensionResults) == "null" {
		c.ClientExtensionResults = json.RawMessage(`{}`)
	}
	return c, nil
}

// CanonicalAssertion reads a sign-in credential (PublicKeyCredential JSON) and
// returns its flags and the re-encoding to forward to GoTrue.
func CanonicalAssertion(raw json.RawMessage) (AuthFlags, json.RawMessage, error) {
	c, err := decodeCredential[assertionResponse](raw)
	if err != nil {
		return AuthFlags{}, nil, err
	}
	if len(c.Response.ClientDataJSON) == 0 || len(c.Response.Signature) == 0 {
		return AuthFlags{}, nil, ErrBadCredential
	}
	f, err := flagsOf(c.Response.AuthenticatorData)
	if err != nil {
		return AuthFlags{}, nil, err
	}
	out, err := json.Marshal(c)
	if err != nil {
		return AuthFlags{}, nil, ErrBadCredential
	}
	return f, out, nil
}

// CanonicalAttestation reads a registration credential and returns the flags
// of the authenticator data inside its attestation object, and the
// re-encoding to forward to GoTrue.
func CanonicalAttestation(raw json.RawMessage) (AuthFlags, json.RawMessage, error) {
	c, err := decodeCredential[attestationResponse](raw)
	if err != nil {
		return AuthFlags{}, nil, err
	}
	if len(c.Response.ClientDataJSON) == 0 {
		return AuthFlags{}, nil, ErrBadCredential
	}
	authData, err := attestationAuthData(c.Response.AttestationObject)
	if err != nil {
		return AuthFlags{}, nil, ErrBadCredential
	}
	f, err := flagsOf(authData)
	if err != nil {
		return AuthFlags{}, nil, err
	}
	out, err := json.Marshal(c)
	if err != nil {
		return AuthFlags{}, nil, ErrBadCredential
	}
	return f, out, nil
}

// RequireUserVerification rewrites GoTrue's WebAuthn options so the browser
// asks for user verification: options.userVerification for a sign-in,
// options.authenticatorSelection.userVerification for a registration. It is
// the browser-side half only — what enforces UV is the flag check above.
func RequireUserVerification(options json.RawMessage, registration bool) (json.RawMessage, error) {
	var o map[string]any
	if err := json.Unmarshal(options, &o); err != nil || o == nil {
		return nil, errors.New("gotrue: unreadable WebAuthn options")
	}
	if registration {
		sel, _ := o["authenticatorSelection"].(map[string]any)
		if sel == nil {
			sel = map[string]any{}
		}
		sel["userVerification"] = "required"
		o["authenticatorSelection"] = sel
	} else {
		o["userVerification"] = "required"
	}
	return json.Marshal(o)
}

// --- the attestation object: just enough CBOR to find authData ---------------
//
// An attestation object is a CBOR map {"fmt": text, "attStmt": map,
// "authData": bytes}. This reads definite-length items only (WebAuthn's CTAP2
// canonical CBOR has no indefinite lengths) and refuses anything else, bounded
// in depth so a hostile nesting cannot recurse without limit.

const cborMaxDepth = 16

var errCBOR = errors.New("gotrue: malformed attestation object")

// cborHead reads the item head at b[i]: major type, argument, and the index
// just past the head.
func cborHead(b []byte, i int) (byte, uint64, int, error) {
	if i >= len(b) {
		return 0, 0, 0, errCBOR
	}
	major, info := b[i]>>5, b[i]&0x1f
	i++
	var n int
	switch {
	case info < 24:
		return major, uint64(info), i, nil
	case info == 24:
		n = 1
	case info == 25:
		n = 2
	case info == 26:
		n = 4
	case info == 27:
		n = 8
	default: // 28-30 reserved, 31 indefinite length
		return 0, 0, 0, errCBOR
	}
	if i+n > len(b) {
		return 0, 0, 0, errCBOR
	}
	var v uint64
	for _, c := range b[i : i+n] {
		v = v<<8 | uint64(c)
	}
	return major, v, i + n, nil
}

// cborSkip returns the index just past the item at b[i].
func cborSkip(b []byte, i, depth int) (int, error) {
	if depth > cborMaxDepth {
		return 0, errCBOR
	}
	major, v, next, err := cborHead(b, i)
	if err != nil {
		return 0, err
	}
	switch major {
	case 0, 1, 7: // unsigned, negative, simple/float (the head held it)
		return next, nil
	case 2, 3: // byte string, text string
		if v > uint64(len(b)-next) {
			return 0, errCBOR
		}
		return next + int(v), nil
	case 4, 5: // array, map
		items := v
		if major == 5 {
			items *= 2
		}
		if items > uint64(len(b)) {
			return 0, errCBOR
		}
		for k := uint64(0); k < items; k++ {
			if next, err = cborSkip(b, next, depth+1); err != nil {
				return 0, err
			}
		}
		return next, nil
	case 6: // tag: one tagged item follows
		return cborSkip(b, next, depth+1)
	}
	return 0, errCBOR
}

// attestationAuthData returns the authData byte string of an attestation object.
func attestationAuthData(obj []byte) ([]byte, error) {
	major, pairs, i, err := cborHead(obj, 0)
	if err != nil || major != 5 || pairs > uint64(len(obj)) {
		return nil, errCBOR
	}
	var found []byte
	for p := uint64(0); p < pairs; p++ {
		km, kl, ki, err := cborHead(obj, i)
		if err != nil || km != 3 || kl > uint64(len(obj)-ki) {
			return nil, errCBOR
		}
		key := string(obj[ki : ki+int(kl)])
		i = ki + int(kl)
		if key == "authData" {
			vm, vl, vi, err := cborHead(obj, i)
			if err != nil || vm != 2 || vl > uint64(len(obj)-vi) {
				return nil, errCBOR
			}
			if found != nil {
				return nil, errCBOR // a second authData: refuse rather than pick one
			}
			found = obj[vi : vi+int(vl)]
			i = vi + int(vl)
			continue
		}
		if i, err = cborSkip(obj, i, 1); err != nil {
			return nil, err
		}
	}
	if i != len(obj) || found == nil {
		return nil, errCBOR
	}
	return found, nil
}
