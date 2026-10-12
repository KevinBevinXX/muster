package credentials

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// KeySize is the length of the key [NewPG] takes: the CONNECTOR store's key
// (MUSTER_CONNECTOR_ENCRYPTION_KEY), which this store derives its own from.
const KeySize = 32

// ErrDecrypt is returned when a stored secret does not open with the
// configured key and the row's binding — a different key, or a ciphertext that
// was moved to another owner's row (or another credential's).
var ErrDecrypt = errors.New("credentials: the stored secret could not be decrypted with the configured key")

// hkdfInfo is the derivation label. It is ALSO the domain separator from the
// connector store: the connector seal uses the master key itself, this one a key
// derived from it, so a connector ciphertext can never open here and vice versa
// — even before the associated data (which differs too) is compared.
const hkdfInfo = "muster-user-credentials-v1"

// sealer is the same construction as internal/connectors' seal — AES-256-GCM, a
// random nonce prepended to the ciphertext, and the OWNER bound into the
// associated data — over a key derived (HKDF-SHA256) from the connector key.
//
// 🔴 THE OWNER IS IN THE ASSOCIATED DATA, SO A CIPHERTEXT IS BOUND TO ITS ROW'S
// OWNER. Copying user A's sealed setup-token into user B's row (a bad UPDATE, a
// restore into the wrong place, a future bug that writes owner_id from the
// wrong variable) yields a row that does NOT open: [ErrDecrypt], never A's
// token handed to B's agent. TestACiphertextMovedToAnotherOwnerDoesNotOpen.
type sealer struct{ aead cipher.AEAD }

func newSealer(master []byte) (sealer, error) {
	if len(master) != KeySize {
		return sealer{}, fmt.Errorf("credentials: encryption key must be %d bytes, got %d", KeySize, len(master))
	}
	key, err := hkdf.Key(sha256.New, master, nil, hkdfInfo, 32)
	if err != nil {
		return sealer{}, err
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

// binding is everything a ciphertext is bound to: whose it is, which kind of
// credential, and its name.
type binding struct {
	Owner int64
	Kind  string
	Name  string
}

func (b binding) aad() []byte {
	out := []byte(hkdfInfo)
	out = binary.BigEndian.AppendUint64(out, uint64(b.Owner))
	for _, s := range []string{b.Kind, b.Name} {
		out = binary.BigEndian.AppendUint32(out, uint32(len(s)))
		out = append(out, s...)
	}
	return out
}

func (s sealer) seal(b binding, secret string) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("credentials: nonce: %w", err)
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
