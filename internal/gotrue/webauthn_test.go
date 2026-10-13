package gotrue_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ZacxDev/muster/internal/gotrue"
	"github.com/ZacxDev/muster/internal/gotrue/gotruetest"
)

// TestAssertionFlagsAreReadFromTheAuthenticatorData: each flag combination
// reads back as exactly what was set — fixtures chosen so every flag is both
// set and unset somewhere, and UV is set without BE/BS and vice versa.
func TestAssertionFlagsAreReadFromTheAuthenticatorData(t *testing.T) {
	cases := []struct {
		flags byte
		want  gotrue.AuthFlags
	}{
		{0x01, gotrue.AuthFlags{UserPresent: true}},
		{0x05, gotrue.AuthFlags{UserPresent: true, UserVerified: true}},
		{0x04, gotrue.AuthFlags{UserVerified: true}},
		{0x19, gotrue.AuthFlags{UserPresent: true, BackupEligible: true, BackedUp: true}},
		{0x1d, gotrue.AuthFlags{UserPresent: true, UserVerified: true, BackupEligible: true, BackedUp: true}},
		{0x0d, gotrue.AuthFlags{UserPresent: true, UserVerified: true, BackupEligible: true}},
	}
	for _, c := range cases {
		got, _, err := gotrue.CanonicalAssertion(gotruetest.Assertion("cred", c.flags))
		if err != nil || got != c.want {
			t.Errorf("assertion flags %#x: got %+v (%v), want %+v", c.flags, got, err, c.want)
		}
		got, _, err = gotrue.CanonicalAttestation(gotruetest.Attestation("cred", c.flags))
		if err != nil || got != c.want {
			t.Errorf("attestation flags %#x: got %+v (%v), want %+v", c.flags, got, err, c.want)
		}
	}
}

// TestTheForwardedCredentialIsTheCheckedOne: the bytes muster forwards are a
// re-encoding of what it parsed. A credential that names authenticatorData
// TWICE (UV set in the first, clear in the second) is forwarded with ONE
// authenticatorData — the one whose flags muster reported — and unknown fields
// are dropped.
func TestTheForwardedCredentialIsTheCheckedOne(t *testing.T) {
	uv := base64.RawURLEncoding.EncodeToString(gotruetest.AuthData(gotruetest.FlagUPUV))
	noUV := base64.RawURLEncoding.EncodeToString(gotruetest.AuthData(gotruetest.FlagUP))
	raw := []byte(`{"id":"c","rawId":"Yw","type":"public-key","smuggled":"x","response":{` +
		`"clientDataJSON":"e30","authenticatorData":"` + uv + `","signature":"c2ln","authenticatorData":"` + noUV + `"}}`)
	flags, fwd, err := gotrue.CanonicalAssertion(raw)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Response map[string]json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal(fwd, &out); err != nil {
		t.Fatal(err)
	}
	if bytes.Count(fwd, []byte(`"authenticatorData"`)) != 1 || bytes.Contains(fwd, []byte("smuggled")) {
		t.Fatalf("the forwarded credential is not canonical: %s", fwd)
	}
	var ad string
	_ = json.Unmarshal(out.Response["authenticatorData"], &ad)
	forwardedUV := ad == uv
	if flags.UserVerified != forwardedUV {
		t.Fatalf("muster read UV=%v but forwards authenticatorData with UV=%v", flags.UserVerified, forwardedUV)
	}
}

// TestUnreadableCredentialsAreRefused: every way a credential can be
// malformed is ErrBadCredential, never a guess.
func TestUnreadableCredentialsAreRefused(t *testing.T) {
	short := base64.RawURLEncoding.EncodeToString(make([]byte, 36))
	for name, raw := range map[string]string{
		"not JSON":       `{`,
		"wrong type":     `{"id":"c","rawId":"Yw","type":"password","response":{"clientDataJSON":"e30","authenticatorData":"` + short + `","signature":"c2ln"}}`,
		"no id":          `{"rawId":"Yw","type":"public-key","response":{"clientDataJSON":"e30","authenticatorData":"` + short + `","signature":"c2ln"}}`,
		"short authData": `{"id":"c","rawId":"Yw","type":"public-key","response":{"clientDataJSON":"e30","authenticatorData":"` + short + `","signature":"c2ln"}}`,
		"no signature":   `{"id":"c","rawId":"Yw","type":"public-key","response":{"clientDataJSON":"e30","authenticatorData":"` + base64.RawURLEncoding.EncodeToString(gotruetest.AuthData(5)) + `"}}`,
		"bad base64":     `{"id":"c","rawId":"!!","type":"public-key","response":{}}`,
	} {
		if _, _, err := gotrue.CanonicalAssertion(json.RawMessage(raw)); !errors.Is(err, gotrue.ErrBadCredential) {
			t.Errorf("%s: got %v, want ErrBadCredential", name, err)
		}
	}
}

func attestationWith(obj []byte) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"id": "c", "rawId": "Yw", "type": "public-key",
		"response": map[string]any{"clientDataJSON": "e30", "attestationObject": base64.RawURLEncoding.EncodeToString(obj)}})
	return b
}

// TestTheAttestationObjectParserRefusesWhatItCannotTrust: duplicate authData,
// indefinite lengths, trailing bytes, a non-map, truncation and a nesting bomb
// are all refused — and a well-formed object with a NESTED attStmt (as the
// "packed" format has: alg, sig, an x5c array) is read.
func TestTheAttestationObjectParserRefusesWhatItCannotTrust(t *testing.T) {
	ad := gotruetest.AuthData(gotruetest.FlagUPUV)
	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	h, txt := gotruetest.CBORHead, gotruetest.CBORText
	adItem := cat(txt("authData"), h(2, uint64(len(ad))), ad)
	packed := cat(h(5, 3), txt("fmt"), txt("packed"), txt("attStmt"),
		h(5, 3), txt("alg"), h(1, 6), txt("sig"), h(2, 3), []byte("sig"), txt("x5c"), h(4, 1), h(2, 4), []byte("cert"),
		adItem)
	if f, _, err := gotrue.CanonicalAttestation(attestationWith(packed)); err != nil || !f.UserVerified {
		t.Fatalf("CONTROL: a packed attestation object did not parse: %+v %v", f, err)
	}
	bomb := h(5, 2)
	bomb = append(bomb, txt("attStmt")...)
	for i := 0; i < 40; i++ {
		bomb = append(bomb, h(4, 1)...)
	}
	bomb = append(bomb, h(0, 0)...)
	bomb = append(bomb, adItem...)
	for name, obj := range map[string][]byte{
		"duplicate authData": cat(h(5, 2), adItem, adItem),
		"indefinite map":     cat([]byte{0xbf}, adItem, []byte{0xff}),
		"trailing bytes":     cat(h(5, 1), adItem, []byte{0x00}),
		"not a map":          cat(h(4, 1), h(2, 1), []byte{1}),
		"truncated":          cat(h(5, 1), txt("authData"), h(2, 40), ad[:10]),
		"no authData":        cat(h(5, 1), txt("fmt"), txt("none")),
		"authData not bytes": cat(h(5, 1), txt("authData"), txt("x")),
		"nesting bomb":       bomb,
	} {
		if _, _, err := gotrue.CanonicalAttestation(attestationWith(obj)); !errors.Is(err, gotrue.ErrBadCredential) {
			t.Errorf("%s: got %v, want ErrBadCredential", name, err)
		}
	}
}

// TestRequireUserVerificationSetsTheRightField: a sign-in's options get
// userVerification at the top level, a registration's inside
// authenticatorSelection — and the rest is kept.
func TestRequireUserVerificationSetsTheRightField(t *testing.T) {
	out, err := gotrue.RequireUserVerification(json.RawMessage(`{"challenge":"abc","userVerification":"preferred","rpId":"localhost"}`), false)
	if err != nil {
		t.Fatal(err)
	}
	var signIn map[string]any
	_ = json.Unmarshal(out, &signIn)
	if signIn["userVerification"] != "required" || signIn["challenge"] != "abc" || signIn["rpId"] != "localhost" {
		t.Fatalf("sign-in options: %s", out)
	}
	out, err = gotrue.RequireUserVerification(json.RawMessage(`{"challenge":"abc","authenticatorSelection":{"residentKey":"required","userVerification":"preferred"}}`), true)
	if err != nil {
		t.Fatal(err)
	}
	var reg struct {
		Sel              map[string]any `json:"authenticatorSelection"`
		UserVerification any            `json:"userVerification"`
	}
	_ = json.Unmarshal(out, &reg)
	if reg.Sel["userVerification"] != "required" || reg.Sel["residentKey"] != "required" || reg.UserVerification != nil {
		t.Fatalf("registration options: %s", out)
	}
}
