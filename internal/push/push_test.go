package push

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// stubHTTPClient is a webpush.HTTPClient that answers a fixed status and
// keeps the request.
type stubHTTPClient struct {
	status int
	err    error
	got    *http.Request
}

func (s *stubHTTPClient) Do(req *http.Request) (*http.Response, error) {
	s.got = req
	if s.err != nil {
		return nil, s.err
	}
	return &http.Response{StatusCode: s.status, Body: io.NopCloser(bytes.NewReader(nil)), Header: http.Header{}}, nil
}

// realSubscription has a genuine P-256 client key, so webpush-go's ECDH
// encryption succeeds.
func realSubscription(t *testing.T, endpoint string) Subscription {
	t.Helper()
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	if _, err := rand.Read(auth); err != nil {
		t.Fatal(err)
	}
	var s Subscription
	s.Endpoint = endpoint
	s.Keys.P256dh = base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes())
	s.Keys.Auth = base64.RawURLEncoding.EncodeToString(auth)
	return s
}

func realVAPID(t *testing.T, subject string) VAPIDConfig {
	t.Helper()
	priv, pub, err := GenerateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}
	return VAPIDConfig{PublicKey: pub, PrivateKey: priv, Subject: subject}
}

// jwtClaims decodes the claims of the VAPID JWT on a captured request
// (`Authorization: vapid t=<jwt>, k=<key>`).
func jwtClaims(t *testing.T, req *http.Request) map[string]any {
	t.Helper()
	h := req.Header.Get("Authorization")
	_, rest, ok := strings.Cut(h, "t=")
	if !ok {
		t.Fatalf("no VAPID token in Authorization %q", h)
	}
	tok, _, _ := strings.Cut(rest, ",")
	parts := strings.Split(strings.TrimSpace(tok), ".")
	if len(parts) != 3 {
		t.Fatalf("the VAPID token is not a JWT: %q", tok)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	return claims
}

// TestTheJWTSubjectIsTheConfiguredSubject: the `sub` claim a push service sees
// is exactly MUSTER_VAPID_SUBJECT. webpush-go v1.4.0 prepends "mailto:" to a
// subject that does not start with "https:", so handing it the configured
// "mailto:…" verbatim signs "mailto:mailto:…" — which Apple's push service
// refuses. Measured red with Subscriber: p.vapid.Subject.
func TestTheJWTSubjectIsTheConfiguredSubject(t *testing.T) {
	for _, subject := range []string{"mailto:ops@example.com", "https://muster.example.com/contact"} {
		stub := &stubHTTPClient{status: http.StatusCreated}
		p := NewWebpushPusher(realVAPID(t, subject))
		p.client = stub
		res := p.Send(context.Background(), realSubscription(t, "https://fcm.googleapis.com/fcm/send/abc"), []byte(`{}`))
		if res.Err != nil || res.StatusCode != http.StatusCreated || res.Expired {
			t.Fatalf("send = %+v", res)
		}
		if got := jwtClaims(t, stub.got)["sub"]; got != subject {
			t.Errorf("the VAPID JWT names sub %q; configured %q", got, subject)
		}
		if aud := jwtClaims(t, stub.got)["aud"]; aud != "https://fcm.googleapis.com" {
			t.Errorf("aud = %q, want the push service's origin", aud)
		}
	}
}

func TestWebpushPusherStatusMapping(t *testing.T) {
	for status, wantExpired := range map[int]bool{201: false, 200: false, 404: true, 410: true, 429: false, 500: false} {
		stub := &stubHTTPClient{status: status}
		p := NewWebpushPusher(realVAPID(t, "mailto:ops@example.com"))
		p.client = stub
		res := p.Send(context.Background(), realSubscription(t, "https://fcm.googleapis.com/fcm/send/abc"), []byte(`{}`))
		if res.StatusCode != status || res.Expired != wantExpired || res.Err != nil {
			t.Errorf("status %d: %+v, want expired=%v", status, res, wantExpired)
		}
	}
	stub := &stubHTTPClient{err: errors.New("dial: refused")}
	p := NewWebpushPusher(realVAPID(t, "mailto:ops@example.com"))
	p.client = stub
	if res := p.Send(context.Background(), realSubscription(t, "https://fcm.googleapis.com/fcm/send/abc"), []byte(`{}`)); res.Err == nil {
		t.Error("a transport error was not reported")
	}
}

// TestVAPIDValidate: a fresh pair passes; a pair from two generations, a
// swapped pair, junk keys and an unclean subject are each refused.
func TestVAPIDValidate(t *testing.T) {
	good := realVAPID(t, "mailto:ops@example.com")
	if err := good.Validate(); err != nil {
		t.Fatalf("a fresh pair is refused: %v", err)
	}
	padded := good
	padded.PublicKey += "="
	if err := padded.Validate(); err != nil {
		t.Errorf("a padded public key is refused: %v", err)
	}
	other := realVAPID(t, "mailto:ops@example.com")
	cases := map[string]VAPIDConfig{
		"two generations": {PublicKey: good.PublicKey, PrivateKey: other.PrivateKey, Subject: good.Subject},
		"swapped":         {PublicKey: good.PrivateKey, PrivateKey: good.PublicKey, Subject: good.Subject},
		"junk public":     {PublicKey: "not-a-key", PrivateKey: good.PrivateKey, Subject: good.Subject},
		"junk private":    {PublicKey: good.PublicKey, PrivateKey: "AAAA", Subject: good.Subject},
		"bare address":    {PublicKey: good.PublicKey, PrivateKey: good.PrivateKey, Subject: "ops@example.com"},
		"named mailto":    {PublicKey: good.PublicKey, PrivateKey: good.PrivateKey, Subject: "mailto:Ops <ops@example.com>"},
		"double mailto":   {PublicKey: good.PublicKey, PrivateKey: good.PrivateKey, Subject: "mailto:mailto:ops@example.com"},
		"http":            {PublicKey: good.PublicKey, PrivateKey: good.PrivateKey, Subject: "http://example.com"},
		"empty subject":   {PublicKey: good.PublicKey, PrivateKey: good.PrivateKey},
	}
	for name, c := range cases {
		err := c.Validate()
		if !errors.Is(err, ErrBadVAPID) {
			t.Errorf("%s: Validate = %v, want ErrBadVAPID", name, err)
		}
		if err != nil && strings.Contains(err.Error(), good.PrivateKey) {
			t.Errorf("%s: the refusal prints the private key", name)
		}
	}
}

func TestSubscriptionCheckAndPushServiceAllowlist(t *testing.T) {
	ok := realSubscription(t, "https://fcm.googleapis.com/fcm/send/abc")
	if err := ok.Check(); err != nil {
		t.Fatal(err)
	}
	for name, ep := range map[string]string{
		"http":     "http://fcm.googleapis.com/fcm/send/abc",
		"no path":  "https://fcm.googleapis.com",
		"userinfo": "https://u:p@fcm.googleapis.com/x",
		"space":    "https://fcm.googleapis.com/a b",
		"long":     "https://fcm.googleapis.com/" + strings.Repeat("a", maxEndpointLen),
	} {
		s := ok
		s.Endpoint = ep
		if err := s.Check(); !errors.Is(err, ErrBadSubscription) {
			t.Errorf("%s: Check = %v, want ErrBadSubscription", name, err)
		}
	}
	noKey := ok
	noKey.Keys.Auth = ""
	if err := noKey.Check(); !errors.Is(err, ErrBadSubscription) {
		t.Error("a subscription with no auth key passed")
	}

	for ep, want := range map[string]bool{
		"https://fcm.googleapis.com/fcm/send/x":                true,
		"https://updates.push.services.mozilla.com/wpush/v2/x": true,
		"https://web.push.apple.com/QH0x":                      true,
		"https://api.push.apple.com/3/device/x":                true,
		"https://wns2-bl2p.notify.windows.com/w/?token=x":      true,
		"https://push.apple.com/x":                             false, // the suffix needs a label before it
		"https://fcm.googleapis.com:8443/fcm/send/x":           false,
		"https://fcm.googleapis.com.example.com/x":             false,
		"https://evilpush.apple.com.example.com/x":             false,
		"https://kubernetes.default.svc/api/v1/namespaces":     false,
		"http://fcm.googleapis.com/fcm/send/x":                 false,
		"https://notify.windows.com/x":                         false,
		"https://xnotify.windows.com/x":                        false,
		"https://FCM.GOOGLEAPIS.COM/fcm/send/x":                true,
		"https://169.254.169.254/latest/meta-data/":            false,
		"https://muster.example/api/push/vapid-public-key":     false,
	} {
		if got := AllowedPushService(ep); got != want {
			t.Errorf("AllowedPushService(%q) = %v, want %v", ep, got, want)
		}
	}
}
