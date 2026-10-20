package githubapp

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParsePrivateKey reads the App's private key: the PEM file GitHub's "Generate a
// private key" button downloads (PKCS#1, `BEGIN RSA PRIVATE KEY`), or the same
// key re-encoded as PKCS#8 (`BEGIN PRIVATE KEY`).
//
// A value whose line breaks were flattened to the two characters `\n` (a common
// result of pasting a PEM into a one-line field) is accepted too: those escapes
// cannot occur inside base64, so turning them back into newlines cannot change a
// well-formed key.
//
// 🔴 A KEY THAT DOES NOT PARSE IS A BOOT REFUSAL, NOT A LOG LINE (see
// cmd/muster-server's loadConfig): every token mint would fail with it, and the
// failure would surface hours later on an agent card instead of at deploy time.
func ParsePrivateKey(v string) (*rsa.PrivateKey, error) {
	v = strings.TrimSpace(v)
	if !strings.Contains(v, "\n") && strings.Contains(v, `\n`) {
		v = strings.ReplaceAll(v, `\n`, "\n")
	}
	block, rest := pem.Decode([]byte(v))
	if block == nil {
		return nil, errors.New("not a PEM block (expected the .pem file GitHub generated for the App)")
	}
	if strings.TrimSpace(string(rest)) != "" {
		return nil, errors.New("more than one PEM block: give exactly the App's private key")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("PKCS#1 key: %w", err)
		}
		return k, nil
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("PKCS#8 key: %w", err)
		}
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("the key is a %T, and a GitHub App key is RSA", k)
		}
		return rk, nil
	default:
		return nil, fmt.Errorf("PEM block is %q, not an RSA private key", block.Type)
	}
}

// JWT lifetime. GitHub refuses an App JWT whose exp is more than 10 minutes after
// its iat; iat is backdated to absorb a clock running ahead of GitHub's (GitHub's
// own recommendation is 60 s).
const (
	jwtBackdate = 60 * time.Second
	jwtLifetime = 9 * time.Minute // from now: exp − iat = 10 min, the maximum
)

// appJWT signs the App's own credential (RS256), used ONLY to call /app/...
// endpoints: list the installations and mint installation tokens. It never
// leaves muster.
//
// Claims: iss = the App ID (a string; GitHub accepts the App ID or client ID),
// iat = now − 60 s, exp = now + 9 min.
func appJWT(appID int64, key *rsa.PrivateKey, now time.Time) (string, error) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(struct {
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
		Iss string `json:"iss"`
	}{
		Iat: now.Add(-jwtBackdate).Unix(),
		Exp: now.Add(jwtLifetime).Unix(),
		Iss: strconv.FormatInt(appID, 10),
	})
	if err != nil {
		return "", err
	}
	signing := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("githubapp: sign the App JWT: %w", err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}
