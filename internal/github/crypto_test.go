package github

import (
	"bytes"
	"crypto/rand"
	"strings"
	"testing"
)

// key32 returns a deterministic 32-byte AES-256 key for tests.
func key32() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i + 1)
	}
	return k
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := key32()
	for _, pt := range []string{"", "gho_shortish", strings.Repeat("token-material-", 50)} {
		blob, err := encrypt(key, []byte(pt), nil)
		if err != nil {
			t.Fatalf("encrypt(%q): %v", pt, err)
		}
		// Ciphertext must not equal plaintext (unless empty, still has nonce+tag).
		if pt != "" && bytes.Contains(blob, []byte(pt)) {
			t.Errorf("ciphertext leaked plaintext for %q", pt)
		}
		got, err := decrypt(key, blob, nil)
		if err != nil {
			t.Fatalf("decrypt round-trip(%q): %v", pt, err)
		}
		if string(got) != pt {
			t.Errorf("round-trip = %q, want %q", got, pt)
		}
	}
}

func TestEncryptNonceIsRandom(t *testing.T) {
	key := key32()
	a, err := encrypt(key, []byte("same-plaintext"), nil)
	if err != nil {
		t.Fatalf("encrypt a: %v", err)
	}
	b, err := encrypt(key, []byte("same-plaintext"), nil)
	if err != nil {
		t.Fatalf("encrypt b: %v", err)
	}
	// A random nonce means two seals of the same plaintext differ.
	if bytes.Equal(a, b) {
		t.Error("two encryptions of the same plaintext are identical (nonce not random)")
	}
}

func TestEncryptRejectsBadKeySize(t *testing.T) {
	// AES accepts 16/24/32-byte keys only; a 10-byte key must error, not panic.
	if _, err := encrypt(make([]byte, 10), []byte("x"), nil); err == nil {
		t.Error("encrypt with a 10-byte key = nil error, want failure")
	}
}

func TestDecryptRejectsBadKeySize(t *testing.T) {
	if _, err := decrypt(make([]byte, 10), make([]byte, 64), nil); err == nil {
		t.Error("decrypt with a 10-byte key = nil error, want failure")
	}
}

func TestDecryptShortBlob(t *testing.T) {
	key := key32()
	// A blob shorter than the GCM nonce cannot carry a nonce → explicit error.
	if _, err := decrypt(key, []byte("tiny"), nil); err == nil {
		t.Error("decrypt of a too-short blob = nil error, want 'ciphertext too short'")
	} else if !strings.Contains(err.Error(), "too short") {
		t.Errorf("decrypt short blob error = %v, want 'ciphertext too short'", err)
	}
}

func TestDecryptWrongKeyFails(t *testing.T) {
	blob, err := encrypt(key32(), []byte("secret"), nil)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	other := make([]byte, 32) // all-zero, different key
	if _, err := decrypt(other, blob, nil); err == nil {
		t.Error("decrypt with the wrong key succeeded, want auth failure")
	}
}

func TestDecryptTamperedCiphertextFails(t *testing.T) {
	key := key32()
	blob, err := encrypt(key, []byte("secret-token"), nil)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	// Flip a bit in the tag/ciphertext region (past the nonce) — GCM must reject it.
	tampered := append([]byte(nil), blob...)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := decrypt(key, tampered, nil); err == nil {
		t.Error("decrypt of tampered ciphertext succeeded, want GCM auth failure")
	}
}

func TestEncryptUsesFreshRandomKeyRoundTrips(t *testing.T) {
	// Smoke test with a real random key, not just the deterministic fixture.
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	blob, err := encrypt(key, []byte("hello"), nil)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	got, err := decrypt(key, blob, nil)
	if err != nil || string(got) != "hello" {
		t.Fatalf("round-trip = %q, %v", got, err)
	}
}
