package uploads

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testKey = "test-signing-key-0123456789abcdef-not-real"

func mustSigner(t *testing.T, key string) Signer {
	t.Helper()
	s, err := NewSigner(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

const sid = "AAAAAAAAAAAAAAAAAAAAAA" // a well-formed public id

func linkQuery(t *testing.T, link string) (string, url.Values) {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimPrefix(u.Path, LinkPath), u.Query()
}

// TestASignedLinkVerifiesUntilItsExpiry: the link minted for (id, exp) is
// accepted one second before exp and refused AT exp and after it — with the
// expiry error, not a signature one (the MAC over exp still matches).
func TestASignedLinkVerifiesUntilItsExpiry(t *testing.T) {
	s := mustSigner(t, testKey)
	exp := time.Unix(1_900_000_000, 0)
	link := s.URL("https://muster.example.test/", sid, exp)
	if want := "https://muster.example.test/f/" + sid + "?exp=1900000000&sig="; !strings.HasPrefix(link, want) {
		t.Fatalf("link = %q; want prefix %q", link, want)
	}
	id, q := linkQuery(t, link)
	if err := s.Verify(id, q, exp.Add(-time.Second)); err != nil {
		t.Errorf("one second before expiry: %v; want accepted", err)
	}
	if err := s.Verify(id, q, exp.Add(-time.Hour)); err != nil {
		t.Errorf("an hour before expiry: %v; want accepted", err)
	}
	for _, at := range []time.Time{exp, exp.Add(time.Second), exp.Add(48 * time.Hour)} {
		if err := s.Verify(id, q, at); !errors.Is(err, ErrLinkExpired) {
			t.Errorf("at %v (exp %v): %v; want ErrLinkExpired", at.Unix(), exp.Unix(), err)
		}
	}
}

// TestATamperedLinkIsRefused: every part of a link is bound by the MAC — the
// signature, the expiry (extending it), the id (pointing it at another
// upload) and the key.
func TestATamperedLinkIsRefused(t *testing.T) {
	s := mustSigner(t, testKey)
	now := time.Unix(1_800_000_000, 0)
	exp := now.Add(time.Hour)
	id, q := linkQuery(t, s.URL("https://m.example.test", sid, exp))
	sig := q.Get("sig")

	flip := func(c byte) byte {
		if c == 'A' {
			return 'B'
		}
		return 'A'
	}
	tamperedSig := sig[:len(sig)-2] + string(flip(sig[len(sig)-2])) + sig[len(sig)-1:]
	extended := url.Values{"exp": {strconv.FormatInt(exp.Add(24*time.Hour).Unix(), 10)}, "sig": {sig}}
	other := "BBBBBBBBBBBBBBBBBBBBBB"

	for name, tc := range map[string]struct {
		id string
		q  url.Values
		s  Signer
	}{
		"signature flipped":      {id, url.Values{"exp": q["exp"], "sig": {tamperedSig}}, s},
		"expiry extended":        {id, extended, s},
		"another id":             {other, q, s},
		"another server's key":   {id, q, mustSigner(t, testKey+"-other")},
		"signature not base64":   {id, url.Values{"exp": q["exp"], "sig": {"!!!" + sig[3:]}}, s},
		"signature truncated":    {id, url.Values{"exp": q["exp"], "sig": {sig[:20]}}, s},
		"signature of the empty": {id, url.Values{"exp": q["exp"], "sig": {""}}, s},
	} {
		err := tc.s.Verify(tc.id, tc.q, now)
		if err == nil {
			t.Errorf("%s: accepted; want refused", name)
			continue
		}
		if !errors.Is(err, ErrBadSig) && !errors.Is(err, ErrBadLink) {
			t.Errorf("%s: %v; want a signature or malformed-link refusal", name, err)
		}
	}
	// The control: the untampered link, at the same instant, is accepted.
	if err := s.Verify(id, q, now); err != nil {
		t.Fatalf("CONTROL: the untouched link was refused: %v", err)
	}
}

// TestAMalformedLinkIsRefusedBeforeTheMAC: shapes that are not a link at all.
func TestAMalformedLinkIsRefusedBeforeTheMAC(t *testing.T) {
	s := mustSigner(t, testKey)
	now := time.Unix(1_800_000_000, 0)
	good := s.Sign(sid, 1_900_000_000)
	for name, tc := range map[string]struct {
		id string
		q  url.Values
	}{
		"no exp":             {sid, url.Values{"sig": {good}}},
		"no sig":             {sid, url.Values{"exp": {"1900000000"}}},
		"two exps":           {sid, url.Values{"exp": {"1900000000", "1"}, "sig": {good}}},
		"two sigs":           {sid, url.Values{"exp": {"1900000000"}, "sig": {good, good}}},
		"exp with a leading": {sid, url.Values{"exp": {"01900000000"}, "sig": {good}}},
		"exp not a number":   {sid, url.Values{"exp": {"soon"}, "sig": {good}}},
		"negative exp":       {sid, url.Values{"exp": {"-1"}, "sig": {good}}},
		"id too short":       {"AAAA", url.Values{"exp": {"1900000000"}, "sig": {good}}},
		"id with a slash":    {"AAAAAAAAAAAAAAAAAAAA/A", url.Values{"exp": {"1900000000"}, "sig": {good}}},
	} {
		if err := s.Verify(tc.id, tc.q, now); !errors.Is(err, ErrBadLink) {
			t.Errorf("%s: %v; want ErrBadLink", name, err)
		}
	}
	if err := (Signer{}).Verify(sid, url.Values{"exp": {"1900000000"}, "sig": {good}}, now); !errors.Is(err, ErrBadLink) {
		t.Errorf("a zero Signer verified a link: %v", err)
	}
}

// TestTheSigningKeyHasAFloor: a key under 32 bytes is refused.
func TestTheSigningKeyHasAFloor(t *testing.T) {
	if _, err := NewSigner(strings.Repeat("k", MinSigningKeyBytes-1)); err == nil {
		t.Error("a 31-byte key was accepted")
	}
	if _, err := NewSigner(strings.Repeat("k", MinSigningKeyBytes)); err != nil {
		t.Errorf("a 32-byte key was refused: %v", err)
	}
}

// TestTheSignatureIsComparedInConstantTime is STRUCTURAL: Verify's body
// compares the MAC with hmac.Equal and nothing else — no ==, bytes.Equal or
// string comparison of the signature, which would leak its prefix by timing.
// A timing measurement in a unit test would be a flake; the call is the
// property.
func TestTheSignatureIsComparedInConstantTime(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "sign.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var verify *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "Verify" {
			verify = fd
		}
	}
	if verify == nil {
		t.Fatal("no Verify in sign.go")
	}
	hmacEqual := 0
	ast.Inspect(verify.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok {
					switch id.Name + "." + sel.Sel.Name {
					case "hmac.Equal":
						hmacEqual++
					case "bytes.Equal", "bytes.Compare", "strings.Compare", "strings.EqualFold", "reflect.DeepEqual":
						t.Errorf("Verify calls %s.%s: not a constant-time comparison", id.Name, sel.Sel.Name)
					}
				}
			}
		case *ast.BinaryExpr:
			if x.Op == token.EQL || x.Op == token.NEQ {
				for _, side := range []ast.Expr{x.X, x.Y} {
					if id, ok := side.(*ast.Ident); ok && (id.Name == "sig" || id.Name == "got") {
						t.Errorf("Verify compares %s with %s: not constant-time", id.Name, x.Op)
					}
				}
			}
		}
		return true
	})
	if hmacEqual != 1 {
		t.Errorf("Verify calls hmac.Equal %d time(s); want exactly 1", hmacEqual)
	}
}
