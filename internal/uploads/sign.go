package uploads

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// MinSigningKeyBytes is the shortest signing key accepted. A link's whole
// strength is this key's: 32 bytes is HMAC-SHA256's own block of security.
const MinSigningKeyBytes = 32

// LinkPath is the public route a signed link names: GET /f/{id}.
const LinkPath = "/f/"

// Errors Verify answers. A handler answers every one of them identically (404),
// so a caller cannot learn which check failed; they are distinct for tests and
// the log.
var (
	ErrBadLink     = errors.New("uploads: malformed link")
	ErrBadSig      = errors.New("uploads: link signature does not match")
	ErrLinkExpired = errors.New("uploads: link has expired")
)

// Signer mints and checks the muster-signed links GET /f/{id} serves.
type Signer struct{ key []byte }

// NewSigner returns a Signer over key, which must be at least
// MinSigningKeyBytes long.
func NewSigner(key string) (Signer, error) {
	if len(key) < MinSigningKeyBytes {
		return Signer{}, errors.New("uploads: the signing key is shorter than 32 bytes")
	}
	return Signer{key: []byte(key)}, nil
}

// linkDomain separates this HMAC from any other use of the same key.
const linkDomain = "muster-upload-link-v1"

// mac is the link's MAC over (id, exp).
func (s Signer) mac(id string, exp int64) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(linkDomain + "\n" + id + "\n" + strconv.FormatInt(exp, 10)))
	return m.Sum(nil)
}

// Sign is the link signature for public id id, valid until exp (Unix seconds):
// 43 characters of unpadded base64url.
func (s Signer) Sign(id string, exp int64) string {
	return base64.RawURLEncoding.EncodeToString(s.mac(id, exp))
}

// URL is the signed link to id on base (MUSTER_PUBLIC_URL), valid until exp.
func (s Signer) URL(base, id string, exp time.Time) string {
	e := exp.Unix()
	return strings.TrimRight(base, "/") + LinkPath + id + "?exp=" + strconv.FormatInt(e, 10) + "&sig=" + s.Sign(id, e)
}

// Verify checks a link's id, its exp and sig query values against now.
//
// 🔴 THE SIGNATURE IS COMPARED IN CONSTANT TIME (hmac.Equal), and it is checked
// BEFORE the expiry is believed: an exp is only a number until the MAC over it
// matches.
func (s Signer) Verify(id string, q url.Values, now time.Time) error {
	if len(s.key) == 0 || !ValidPublicID(id) {
		return ErrBadLink
	}
	expStr, sig := q.Get("exp"), q.Get("sig")
	if len(q["exp"]) != 1 || len(q["sig"]) != 1 || expStr == "" || len(expStr) > 12 {
		return ErrBadLink
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || exp <= 0 || strconv.FormatInt(exp, 10) != expStr {
		return ErrBadLink
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return ErrBadSig
	}
	if !hmac.Equal(got, s.mac(id, exp)) {
		return ErrBadSig
	}
	if now.Unix() >= exp {
		return ErrLinkExpired
	}
	return nil
}
