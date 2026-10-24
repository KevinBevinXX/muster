// Package opencodehost is muster-mirror's side of opencode v2's local server
// (docs/design/opencode.md): the pair token muster-mirror holds, the one
// client that reads the server's REST API and event stream with it, and the
// pairing step that mints the token in the first place.
//
// 🔴 READ-ONLY, BY CONSTRUCTION. Every request this package makes is a GET
// (Client.get, Client.stream), except the two calls that mint a token —
// POST /api/pair and the unauthenticated GET /auth/connect/{code} — which only
// Pair and Credentials.Renew make (Client.mint). A test in this package
// reads its source and fails when any other request is spelled
// (TestTheClientMakesOnlyGETsExceptTheMint), and another drives every path
// against a fake server and fails on any request outside that set.
//
// 🔴 THE TOKEN AND THE SERVER PASSWORD ARE NEVER PRINTED OR LOGGED. What a
// person reads names a token by Fingerprint only.
package opencodehost

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ZacxDev/muster/internal/atomicfile"
)

// TokenFile is what the token file holds: the server it was minted by and
// the token itself. JSON, mode 0600, written whole (atomicfile).
//
// The URL is written by Pair from the server's own registration file, so the
// daemon finds the server without ever reading that file (which also holds
// the server password).
type TokenFile struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

// maxTokenFile bounds a token file read.
const maxTokenFile = 4096

// TokenTTL is a pairing token's lifetime upstream (SESSION_TTL_SECONDS in
// packages/server/src/auth.ts at 6ec5682): 30 days.
const TokenTTL = 30 * 24 * time.Hour

// RenewBefore is how long before its expiry a token is replaced
// (Credentials.Renew): a third of its life, so a host that is off for a week
// still renews in time.
const RenewBefore = 10 * 24 * time.Hour

// Expiry is when token stops being accepted: its "<expires>." prefix, Unix
// seconds (the upstream token is "<expires>.<base64url HMAC>"). false when
// token is not of that shape.
func Expiry(token string) (time.Time, bool) {
	exp, sig, ok := strings.Cut(token, ".")
	if !ok || sig == "" || strings.Contains(sig, ".") {
		return time.Time{}, false
	}
	n, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}, false
	}
	return time.Unix(n, 0), true
}

// Fingerprint names a token for a person without revealing it: the first 8
// hex digits of its SHA-256. The only thing about a token any output of
// muster-mirror prints.
func Fingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])[:8]
}

// ReadTokenFile reads the token file at path. It refuses a symlink, anything
// that is not a regular file, and a file group or others can read: the token
// reaches the whole opencode API.
func ReadTokenFile(path string) (TokenFile, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, syscall.ELOOP) {
		return TokenFile{}, fmt.Errorf("%s is a symlink; the opencode token file must be a regular file", path)
	}
	if err != nil {
		return TokenFile{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return TokenFile{}, err
	}
	if !fi.Mode().IsRegular() {
		return TokenFile{}, fmt.Errorf("%s is not a regular file", path)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return TokenFile{}, fmt.Errorf("%s is readable by others (mode %v); chmod 600 it", path, fi.Mode().Perm())
	}
	b, err := io.ReadAll(io.LimitReader(f, maxTokenFile+1))
	if err != nil {
		return TokenFile{}, err
	}
	if len(b) > maxTokenFile {
		return TokenFile{}, fmt.Errorf("%s is too large to be a token file", path)
	}
	var tf TokenFile
	if err := json.Unmarshal(b, &tf); err != nil {
		return TokenFile{}, fmt.Errorf("%s is not an opencode token file (want {\"url\", \"token\"})", path)
	}
	if _, ok := Expiry(tf.Token); !ok {
		return TokenFile{}, fmt.Errorf("%s holds no opencode pair token", path)
	}
	if err := CheckLocal(tf.URL); err != nil {
		return TokenFile{}, fmt.Errorf("%s: %w", path, err)
	}
	return tf, nil
}

// WriteTokenFile replaces the token file at path with tf, whole and 0600
// (atomicfile), creating its directory 0700 when missing.
func WriteTokenFile(path string, tf TokenFile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(tf)
	if err != nil {
		return err
	}
	return atomicfile.Write(path, append(b, '\n'))
}
