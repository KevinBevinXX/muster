// Package github holds the single connected GitHub account: the OAuth access
// token (encrypted at rest with AES-256-GCM), the login, and granted scopes.
// The token is injected into dispatched agent pods (GITHUB_TOKEN, HTTPS clone)
// and used to list repos for the Repos tab and the dispatch modal.
package github

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"time"
)

// Connection is the stored GitHub account. Token is plaintext in memory; it is
// only ever persisted encrypted.
type Connection struct {
	Login       string    `json:"login"`
	Scopes      string    `json:"scopes"`
	Token       string    `json:"-"`
	ConnectedAt time.Time `json:"connectedAt"`
}

// Store is the GitHub-connection persistence behaviour. Implementations encrypt
// the token at rest and return it decrypted from Get.
type Store interface {
	Save(ctx context.Context, login, scopes, token string) error
	Get(ctx context.Context) (Connection, bool, error)
	Clear(ctx context.Context) error
}

// OAuthStateKey derives the key muster MACs GitHub OAuth `state` values with
// (accounts PR 3b) from the GitHub store's encryption key
// (MUSTER_GITHUB_ENCRYPTION_KEY), by HKDF-SHA256 under its own label.
//
// 🔴 WHY THIS KEY AND NOT THE UI PASSWORD. The first cut keyed the MAC on
// sha256(prefix + MUSTER_UI_PASSWORD); in GoTrue mode (PR 4) that password is
// not required, so the key could degrade to a public constant and the MAC to
// nothing. The encryption key exists exactly when the OAuth flow can complete
// (no store, no connection — every GitHub route refuses without it), is a
// 32-byte secret, and HKDF's distinct label means the MAC key reveals nothing
// about the sealing key and cannot collide with any other use of it.
//
// It refuses an empty input rather than deriving from nothing.
func OAuthStateKey(encKey []byte) ([]byte, error) {
	if len(encKey) == 0 {
		return nil, errors.New("github: no encryption key to derive the OAuth state key from")
	}
	return hkdf.Key(sha256.New, encKey, nil, "muster github oauth state v1", 32)
}

// encrypt seals plaintext with AES-256-GCM under key, binding it to aad (the
// owner; see ownerAAD), and returns nonce||ciphertext||tag.
func encrypt(key, plaintext, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("github: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("github: new gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("github: nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, plaintext, aad), nil
}

// decrypt opens a nonce||ciphertext||tag blob produced by encrypt.
func decrypt(key, blob, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("github: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("github: new gcm: %w", err)
	}
	if len(blob) < gcm.NonceSize() {
		return nil, errors.New("github: ciphertext too short")
	}
	nonce, ct := blob[:gcm.NonceSize()], blob[gcm.NonceSize():]
	return gcm.Open(nil, nonce, ct, aad)
}
