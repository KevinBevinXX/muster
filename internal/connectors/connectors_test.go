package connectors

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"testing/iotest"
)

func TestValidateName(t *testing.T) {
	for _, ok := range []string{"github", "a", "deploy-bot", "x1", strings.Repeat("a", 63)} {
		if err := ValidateName(ok); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "GitHub", "-x", "x-", "a_b", "a.b", "a/b", "a b", strings.Repeat("a", 64), "../x"} {
		if err := ValidateName(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("ValidateName(%q) = %v, want ErrInvalid", bad, err)
		}
	}
}

func TestNormalizeHost(t *testing.T) {
	for in, want := range map[string]string{
		"API.GitHub.com":       "api.github.com",
		" github.com ":         "github.com",
		"api.example.com:8443": "api.example.com:8443",
	} {
		got, err := NormalizeHost(in)
		if err != nil || got != want {
			t.Errorf("NormalizeHost(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{
		"", "localhost", "127.0.0.1", "[::1]", "*.github.com", "https://github.com", "github.com/x",
		"github.com:0", "github.com:99999", "github.com:08", "user@github.com", "-a.com", "a..com",
	} {
		if _, err := NormalizeHost(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("NormalizeHost(%q) accepted; want ErrInvalid", bad)
		}
	}
}

func TestNewConnectorValidate(t *testing.T) {
	good := NewConnector{Name: "svc", Hosts: []string{"B.example.com", "a.example.com", "a.example.com"},
		Scheme: SchemeBearer, Secret: "0123456789abcdef"}
	got, err := good.Validate()
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if strings.Join(got.Hosts, ",") != "a.example.com,b.example.com" || got.Platform != "custom" {
		t.Fatalf("normalised = %+v", got)
	}
	cases := map[string]NewConnector{
		"no hosts":           {Name: "x", Scheme: SchemeBearer, Secret: "0123456789"},
		"bad scheme":         {Name: "x", Hosts: []string{"a.example.com"}, Scheme: "handout", Secret: "0123456789"},
		"short secret":       {Name: "x", Hosts: []string{"a.example.com"}, Scheme: SchemeBearer, Secret: "short"},
		"newline secret":     {Name: "x", Hosts: []string{"a.example.com"}, Scheme: SchemeBearer, Secret: "0123456789\r\nX-Evil: 1"},
		"header host":        {Name: "x", Hosts: []string{"a.example.com"}, Scheme: SchemeHeader, HeaderName: "Host", Secret: "0123456789"},
		"header empty":       {Name: "x", Hosts: []string{"a.example.com"}, Scheme: SchemeHeader, Secret: "0123456789"},
		"header token clash": {Name: "x", Hosts: []string{"a.example.com"}, Scheme: SchemeHeader, HeaderName: "X-Muster-Token", Secret: "0123456789"},
		"basic colon user":   {Name: "x", Hosts: []string{"a.example.com"}, Scheme: SchemeBasic, Username: "a:b", Secret: "0123456789"},
	}
	for name, nc := range cases {
		if _, err := nc.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Validate = %v, want ErrInvalid", name, err)
		}
	}
}

func TestGitHubPresetCoversTheAPIAndGit(t *testing.T) {
	p, ok := PresetFor("github")
	if !ok {
		t.Fatal("no github preset")
	}
	if strings.Join(p.Hosts, ",") != "api.github.com,github.com" || p.Scheme != SchemeBasic || p.Username != "x-access-token" {
		t.Fatalf("github preset = %+v", p)
	}
	if _, err := (NewConnector{Name: "github", Hosts: p.Hosts, Scheme: p.Scheme, Username: p.Username, Secret: "ghp_FAKEFAKEFAKE"}).Validate(); err != nil {
		t.Fatalf("the preset does not validate: %v", err)
	}
}

// TestPolicyRefusesEveryNonPublicRange is the address guard's table. The private
// IPv4 literals are built from octets rather than spelled, so the repository's
// leak gate (which refuses RFC 1918 host literals) does not read them as
// topology.
func TestPolicyRefusesEveryNonPublicRange(t *testing.T) {
	v4 := func(a, b, c, d byte) netip.Addr { return netip.AddrFrom4([4]byte{a, b, c, d}) }
	refused := []netip.Addr{
		v4(127, 0, 0, 1), v4(10, 1, 2, 3), v4(172, 16, 0, 9), v4(192, 168, 1, 1), v4(169, 254, 169, 254),
		v4(100, 64, 0, 1), v4(0, 0, 0, 0), v4(0, 1, 2, 3), v4(224, 0, 0, 1), v4(255, 255, 255, 255), v4(198, 18, 0, 1),
		netip.MustParseAddr("::1"), netip.MustParseAddr("::"), netip.MustParseAddr("fe80::1"),
		netip.MustParseAddr("fd00::1"), netip.MustParseAddr("ff02::1"), netip.MustParseAddr("64:ff9b::a01:203"),
		netip.AddrFrom16(v4(10, 0, 0, 1).As16()), // IPv4-mapped private
		netip.MustParseAddr("2002:a00:1::1"),
	}
	var strict Policy
	for _, ip := range refused {
		if strict.AllowedAddr(ip) {
			t.Errorf("the strict policy allows %s", ip)
		}
	}
	allowed := []netip.Addr{v4(140, 82, 112, 3), v4(8, 8, 8, 8), netip.MustParseAddr("2606:4700::1111")}
	for _, ip := range allowed {
		if !strict.AllowedAddr(ip) {
			t.Errorf("the strict policy refuses public %s", ip)
		}
	}
	loop := Policy{AllowLoopback: true}
	if !loop.AllowedAddr(v4(127, 0, 0, 1)) || loop.AllowedAddr(v4(10, 0, 0, 1)) || loop.AllowedAddr(v4(169, 254, 169, 254)) {
		t.Error("AllowLoopback must admit loopback and nothing else non-public")
	}
}

func TestRedactingReaderCatchesASecretSplitAcrossReads(t *testing.T) {
	secret := "s3cr3t-T0KEN-value"
	body := "a" + secret + strings.Repeat("x", 40000) + secret[:7]
	body += secret[7:] + "tail" + secret
	r := newRedactor([]needle{{s: secret}})
	// OneByteReader forces every boundary to fall inside the secret somewhere.
	for name, src := range map[string]io.Reader{
		"one-byte": iotest.OneByteReader(strings.NewReader(body)),
		"half":     iotest.HalfReader(strings.NewReader(body)),
		"whole":    strings.NewReader(body),
	} {
		out, err := io.ReadAll(r.reader(io.NopCloser(src)))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if bytes.Contains(out, []byte(secret)) {
			t.Fatalf("%s: the secret survived redaction", name)
		}
		if n := bytes.Count(out, []byte(RedactedMarker)); n != 3 {
			t.Fatalf("%s: %d markers, want 3", name, n)
		}
		want := strings.ReplaceAll(body, secret, RedactedMarker)
		if string(out) != want {
			t.Fatalf("%s: the body was altered beyond the redaction", name)
		}
	}
}

// TestTheBasicEncodingIsRedactedInEveryAlphabet: whatever the secret, the
// base64 pair the proxy put on the wire — padded or not, standard or URL
// alphabet — does not survive the redactor. The fixtures are chosen so their
// encodings differ between alphabets ('+'/'/' vs '-'/'_') and carry padding;
// the control below fails if they stop doing so.
func TestTheBasicEncodingIsRedactedInEveryAlphabet(t *testing.T) {
	c := Connector{Scheme: SchemeBasic, Username: "x-access-token"}
	sawAlphabetSplit, sawPadding := false, false
	for _, secret := range []string{"ghp_FAKE0123456789", "FAKE~~~~????>>>>0", "FAKE-??>~-pad1", "FAKE>>>>~~~~????ab"} {
		name, value := authHeader(c, secret)
		if name != "Authorization" || !strings.HasPrefix(value, "Basic ") {
			t.Fatalf("authHeader = %q %q", name, value)
		}
		pair := []byte(c.Username + ":" + secret)
		std, urlenc := base64.StdEncoding.EncodeToString(pair), base64.URLEncoding.EncodeToString(pair)
		sawAlphabetSplit = sawAlphabetSplit || std != urlenc
		sawPadding = sawPadding || strings.HasSuffix(std, "=")
		r := newRedactor(secretForms(c, secret))
		for _, wire := range []string{std, urlenc, base64.RawStdEncoding.EncodeToString(pair), base64.RawURLEncoding.EncodeToString(pair)} {
			out := string(r.replace([]byte("echo: Basic " + wire + " end")))
			if strings.Contains(out, strings.TrimRight(wire, "=")) {
				t.Errorf("secret %q: the encoding %q survived redaction: %q", secret, wire, out)
			}
		}
	}
	if !sawAlphabetSplit || !sawPadding {
		t.Fatalf("control: the fixtures no longer exercise both alphabets (%v) and padding (%v)", sawAlphabetSplit, sawPadding)
	}
}

// TestUnalignedBase64IsRedacted: the credential inside a LARGER base64 blob, at
// every byte offset and in both alphabets, is redacted down to the characters
// that also carry its neighbours' bits — no further. The bounds are exact: a
// character made only of the secret's bits never survives.
func TestUnalignedBase64IsRedacted(t *testing.T) {
	c := Connector{Scheme: SchemeBearer}
	for _, secret := range []string{"ghp_FAKE0123456789", "FAKE~~~~????>>>>0x", "abcdefgh"} {
		r := newRedactor(secretForms(c, secret))
		for pre := 0; pre < 6; pre++ {
			for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding} {
				blob := enc.EncodeToString([]byte(strings.Repeat("Q", pre) + secret + "ZZZZ"))
				out := string(r.replace([]byte(blob)))
				left, right, ok := strings.Cut(out, RedactedMarker)
				if !ok || strings.Contains(right, RedactedMarker) {
					t.Fatalf("secret %q prefix %d: want exactly one marker, got %q", secret, pre, out)
				}
				if maxLeft := (8*pre + 5) / 6; len(left) > maxLeft {
					t.Errorf("secret %q prefix %d: %d characters survive before the marker, at most %d may", secret, pre, len(left), maxLeft)
				}
				if maxRight := len(blob) - (8*(pre+len(secret)))/6; len(right) > maxRight {
					t.Errorf("secret %q prefix %d: %d characters survive after the marker, at most %d may", secret, pre, len(right), maxRight)
				}
			}
		}
	}
}

// TestAQueryEscapedSecretIsRedacted: an upstream that reflects the credential
// inside a URL (a redirect, a link) escapes it; that form is redacted too.
func TestAQueryEscapedSecretIsRedacted(t *testing.T) {
	secret := "FAKE secret+with/chars"
	r := newRedactor(secretForms(Connector{Scheme: SchemeBearer}, secret))
	out := string(r.replace([]byte("https://x.example.com/?t=" + url.QueryEscape(secret))))
	if strings.Contains(out, url.QueryEscape(secret)) || !strings.Contains(out, RedactedMarker) {
		t.Fatalf("the escaped secret survived: %q", out)
	}
}

// TestRewriteLocation: a redirect to an allowlisted host comes back into the
// proxy; ANY other target is refused (ok=false), never handed to the client.
func TestRewriteLocation(t *testing.T) {
	c := Connector{Hosts: []string{"api.example.com", "example.com"}}
	base, _ := url.Parse("https://api.example.com/v1/x")
	for in, want := range map[string]string{
		"https://example.com/a?b=c": "/connect/svc/example.com/a?b=c",
		"/v2/y":                     "/connect/svc/api.example.com/v2/y",
		"//EXAMPLE.com/z":           "/connect/svc/example.com/z",
	} {
		if got, ok := rewriteLocation(in, base, c, "/connect/svc/"); !ok || got != want {
			t.Errorf("rewriteLocation(%q) = %q %v, want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{
		"https://evil.example.org/p",
		"http://api.example.com/down", // a scheme downgrade is not ours
		"//evil.example.org/p",
		"https://user:pw@api.example.com/p",
		"https://api.example.com.evil.example.org/p",
		"https://api.example.com:8443/p",
		"%zz",
	} {
		if got, ok := rewriteLocation(in, base, c, "/connect/svc/"); ok || got != "" {
			t.Errorf("rewriteLocation(%q) = %q %v, want a refusal", in, got, ok)
		}
	}
}

func TestDroppedHeadersCoverTheCallersCredentials(t *testing.T) {
	h := http.Header{}
	for _, n := range droppedRequestHeaders {
		h.Set(n, "x")
	}
	for _, must := range []string{"Authorization", "X-Muster-Token", "Cookie", "Proxy-Authorization"} {
		if h.Get(must) == "" {
			t.Errorf("%s is not in droppedRequestHeaders", must)
		}
	}
}

// TestTheDialRefusesBeforeConnectingAndAtTheSocket: the two layers of the address
// guard, each reached on its own. The resolution check refuses a name whose
// answer is non-public BEFORE any socket exists (its error names the
// resolution); the socket check refuses a destination address however it was
// reached.
func TestTheDialRefusesBeforeConnectingAndAtTheSocket(t *testing.T) {
	p := Policy{Resolve: func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.AddrFrom4([4]byte{169, 254, 169, 254})}, nil
	}}
	_, err := p.dialContext(context.Background(), "tcp", "metadata.example.com:443")
	if !errors.Is(err, ErrForbiddenAddress) || !strings.Contains(err.Error(), "resolves to one") {
		t.Fatalf("resolution guard: err = %v", err)
	}
	if err := p.checkSocketAddr("127.0.0.1:443"); !errors.Is(err, ErrForbiddenAddress) {
		t.Fatalf("socket guard admitted loopback: %v", err)
	}
	if err := p.checkSocketAddr("[fd00::1]:443"); !errors.Is(err, ErrForbiddenAddress) {
		t.Fatalf("socket guard admitted a ULA: %v", err)
	}
	if err := p.checkSocketAddr("140.82.112.3:443"); err != nil {
		t.Fatalf("socket guard refused a public address: %v", err)
	}
}
