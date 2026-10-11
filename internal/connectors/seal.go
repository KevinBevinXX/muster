package connectors

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// KeySize is the encryption key's length: AES-256.
const KeySize = 32

// ErrDecrypt is returned when a stored secret does not open under the configured
// key — the wrong key, a ciphertext that was moved to another row, or a row whose
// bound fields (binding) were changed after the secret was sealed.
//
// 🔴 IT FAILS CLOSED: no fallback key, no plaintext column, no "try without
// additional data". The proxy answers 502 and forwards nothing.
var ErrDecrypt = errors.New("connectors: the stored secret could not be decrypted with the configured key")

// sealer encrypts connector secrets with AES-256-GCM.
//
// 🔴 EVERYTHING THAT DECIDES WHERE AND HOW THE SECRET IS SENT IS ADDITIONAL
// AUTHENTICATED DATA — the name, the allowlisted hosts, the auth scheme, the
// header name and the basic-auth username (binding). Someone who can write the
// database but does not hold the key could otherwise leave the ciphertext alone
// and point the row elsewhere: add a host they control, or switch the scheme or
// header so the secret goes out in a form they can read. Any such edit now makes
// the ciphertext fail to open, and the proxy sends nothing. A row-level swap
// (one connector's ciphertext copied onto another's row) fails the same way.
//
// ⚠ THERE IS NO EDIT PATH FOR THE BOUND FIELDS. Changing a connector's hosts or
// auth shape means deleting it and creating it again, which seals afresh.
// RotateSecret re-seals under the CURRENT binding only after the old ciphertext
// has opened under it, so a rotation cannot launder an out-of-band edit.
type sealer struct{ aead cipher.AEAD }

func newSealer(key []byte) (sealer, error) {
	if len(key) != KeySize {
		return sealer{}, fmt.Errorf("connectors: encryption key must be %d bytes, got %d", KeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return sealer{}, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return sealer{}, err
	}
	return sealer{aead: aead}, nil
}

// binding is the part of a connector its ciphertext is bound to.
type binding struct {
	Name       string
	Hosts      []string
	Scheme     Scheme
	HeaderName string
	Username   string
}

func bindingOf(c Connector) binding {
	return binding{Name: c.Name, Hosts: c.Hosts, Scheme: c.Scheme, HeaderName: c.HeaderName, Username: c.Username}
}

// aad is the binding's canonical encoding: hosts lowercased and sorted (neither
// their order nor their case changes where the secret goes), the header name
// lowercased (HTTP header names are case-insensitive), and every field
// length-prefixed so no two different bindings encode alike.
func (b binding) aad() []byte {
	hosts := make([]string, len(b.Hosts))
	for i, h := range b.Hosts {
		hosts[i] = strings.ToLower(h)
	}
	sort.Strings(hosts)
	out := []byte("muster-connector-v2")
	put := func(s string) {
		out = binary.BigEndian.AppendUint32(out, uint32(len(s)))
		out = append(out, s...)
	}
	put(b.Name)
	put(string(b.Scheme))
	put(strings.ToLower(b.HeaderName))
	put(b.Username)
	out = binary.BigEndian.AppendUint32(out, uint32(len(hosts)))
	for _, h := range hosts {
		put(h)
	}
	return out
}

// seal returns nonce||ciphertext||tag.
func (s sealer) seal(b binding, secret string) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("connectors: nonce: %w", err)
	}
	return s.aead.Seal(nonce, nonce, []byte(secret), b.aad()), nil
}

func (s sealer) open(b binding, blob []byte) (string, error) {
	n := s.aead.NonceSize()
	if len(blob) < n+s.aead.Overhead() {
		return "", ErrDecrypt
	}
	pt, err := s.aead.Open(nil, blob[:n], blob[n:], b.aad())
	if err != nil {
		return "", ErrDecrypt
	}
	return string(pt), nil
}
