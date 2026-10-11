package connectors

import (
	"bytes"
	"errors"
	"testing"
)

func testKey(b byte) []byte { return bytes.Repeat([]byte{b}, KeySize) }

func TestSealRoundTripsAndHidesThePlaintext(t *testing.T) {
	s, err := newSealer(testKey(1))
	if err != nil {
		t.Fatal(err)
	}
	const secret = "ghp_FAKE-plaintext-0123456789"
	ct, err := s.seal(bind("github"), secret)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, []byte(secret)) || bytes.Contains(ct, []byte("plaintext")) {
		t.Fatal("the ciphertext contains the plaintext")
	}
	got, err := s.open(bind("github"), ct)
	if err != nil || got != secret {
		t.Fatalf("open = %q, %v", got, err)
	}
	ct2, _ := s.seal(bind("github"), secret)
	if bytes.Equal(ct, ct2) {
		t.Fatal("two seals of one secret are identical: the nonce is not random")
	}
}

func TestTheWrongKeyFailsClosed(t *testing.T) {
	a, _ := newSealer(testKey(1))
	b, _ := newSealer(testKey(2))
	ct, _ := a.seal(bind("github"), "ghp_FAKE-0123456789")
	if got, err := b.open(bind("github"), ct); !errors.Is(err, ErrDecrypt) || got != "" {
		t.Fatalf("open with the wrong key = %q, %v; want ErrDecrypt and nothing", got, err)
	}
	if _, err := a.open(bind("github"), ct[:5]); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("a truncated ciphertext = %v, want ErrDecrypt", err)
	}
}

// TestACiphertextIsBoundToItsConnectorName: a row-level swap (one connector's
// ciphertext copied onto another connector's row) must not open.
func TestACiphertextIsBoundToItsConnectorName(t *testing.T) {
	s, _ := newSealer(testKey(1))
	ct, _ := s.seal(bind("github"), "ghp_FAKE-0123456789")
	if _, err := s.open(bind("deploy-bot"), ct); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("a ciphertext opened under another connector's name: %v", err)
	}
}

func bind(name string) binding {
	return binding{Name: name, Hosts: []string{"api.example.com", "example.com"}, Scheme: SchemeBasic, Username: "x-access-token"}
}

// TestACiphertextIsBoundToWhereAndHowItIsSent: changing ANY of the fields that
// decide where the secret goes or how it is presented makes the ciphertext fail
// to open. Hosts are bound as a canonical set: reordering or re-casing them is
// the same binding (control), adding, removing or replacing one is not.
func TestACiphertextIsBoundToWhereAndHowItIsSent(t *testing.T) {
	s, _ := newSealer(testKey(1))
	base := bind("github")
	ct, _ := s.seal(base, "ghp_FAKE-0123456789")
	same := base
	same.Hosts = []string{"EXAMPLE.com", "api.example.com"}
	if got, err := s.open(same, ct); err != nil || got != "ghp_FAKE-0123456789" {
		t.Fatalf("control: the same host set in another order/case did not open: %q %v", got, err)
	}
	for name, mut := range map[string]func(b *binding){
		"host added":     func(b *binding) { b.Hosts = append(append([]string{}, b.Hosts...), "evil.example.org") },
		"host replaced":  func(b *binding) { b.Hosts = []string{"api.example.com", "evil.example.org"} },
		"host removed":   func(b *binding) { b.Hosts = []string{"example.com"} },
		"scheme":         func(b *binding) { b.Scheme = SchemeHeader },
		"header":         func(b *binding) { b.HeaderName = "X-Leak" },
		"username":       func(b *binding) { b.Username = "someone-else" },
		"name":           func(b *binding) { b.Name = "github2" },
		"field boundary": func(b *binding) { b.Name, b.Scheme = "githubbasic", "" },
	} {
		b := base
		mut(&b)
		if got, err := s.open(b, ct); !errors.Is(err, ErrDecrypt) || got != "" {
			t.Errorf("%s: opened after the change (%q, %v)", name, got, err)
		}
	}
}

func TestTheKeyMustBe32Bytes(t *testing.T) {
	if _, err := newSealer(make([]byte, 16)); err == nil {
		t.Fatal("a 16-byte key was accepted")
	}
}
