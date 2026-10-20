// Package push is muster's own Web Push: the VAPID identity, the per-owner
// subscription store (migration 0013, under row-level security), and the
// Service that delivers one owner's notification to that owner's devices and
// prunes the ones the push service reports gone.
//
// It is a port of the permission router's internal/push (Service, Pusher,
// redact), with two deliberate differences:
//
//   - 🔴 SUBSCRIPTIONS ARE OWNED. The router's store is one flat set: every
//     device got every notification, which is correct for a service with one
//     operator and a cross-user leak for a service with members. Here the only
//     send is SendTo(owner, …), and the only read it makes is that owner's rows.
//   - 🔴 THE VAPID SUBJECT IS PASSED AS webpush-go WANTS IT, NOT AS CONFIGURED.
//     webpush-go v1.4.0 prepends "mailto:" to any subject that does not start
//     with "https:", so a configured "mailto:ops@example.com" is signed as
//     "mailto:mailto:ops@example.com" — a malformed JWT `sub` that a strict push
//     service (Apple's) refuses. See WebpushPusher.Send.
//
// muster signs with its OWN keypair (MUSTER_VAPID_*), not the router's: a
// browser subscription is bound to the application-server key it was made
// with, and is per origin, so the router's subscriptions could never have been
// reused here anyway.
package push

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// VAPIDConfig is the application-server identity. PublicKey is handed to
// browsers so they can subscribe; PrivateKey signs each message's JWT; Subject
// is the contact the JWT names: "mailto:<address>" or an https URL.
type VAPIDConfig struct {
	PublicKey  string
	PrivateKey string
	Subject    string
}

// GenerateVAPIDKeys returns a fresh base64url (unpadded) P-256 keypair.
func GenerateVAPIDKeys() (privateKey, publicKey string, err error) {
	return webpush.GenerateVAPIDKeys()
}

// ErrBadVAPID is the refusal for a VAPID configuration that would sign
// messages no push service accepts.
var ErrBadVAPID = errors.New("push: invalid VAPID configuration")

// Validate checks that the three values are a usable identity: both keys
// decode to a P-256 keypair, the private key DERIVES the public key, and the
// subject is a clean mailto: address or an https URL.
//
// 🔴 THE DERIVATION CHECK IS THE ONE THAT MATTERS. Two valid keys from two
// different generations pass every shape check, boot, hand browsers the public
// key, and then every message is signed by a key the subscription was not made
// with — the push service answers 401/403 for every send, forever, and the
// only symptom is silence. So is a pair pasted the wrong way round.
func (c VAPIDConfig) Validate() error {
	pub, err := decodeKey(c.PublicKey)
	if err != nil {
		return fmt.Errorf("%w: the public key does not decode as base64url: %v", ErrBadVAPID, err)
	}
	if _, err := ecdh.P256().NewPublicKey(pub); err != nil {
		return fmt.Errorf("%w: the public key is not an uncompressed P-256 point (%d bytes)", ErrBadVAPID, len(pub))
	}
	priv, err := decodeKey(c.PrivateKey)
	if err != nil {
		return fmt.Errorf("%w: the private key does not decode as base64url", ErrBadVAPID)
	}
	key, err := ecdh.P256().NewPrivateKey(priv)
	if err != nil {
		return fmt.Errorf("%w: the private key is not a P-256 scalar (%d bytes)", ErrBadVAPID, len(priv))
	}
	if !bytes.Equal(key.PublicKey().Bytes(), pub) {
		return fmt.Errorf("%w: the private key does not derive the public key — they are from different "+
			"generations (or swapped); every send would be refused by the push service", ErrBadVAPID)
	}
	if err := validSubject(c.Subject); err != nil {
		return fmt.Errorf("%w: %v", ErrBadVAPID, err)
	}
	return nil
}

func validSubject(s string) error {
	switch {
	case strings.HasPrefix(s, "mailto:"):
		addr := strings.TrimPrefix(s, "mailto:")
		a, err := mail.ParseAddress(addr)
		if err != nil || a.Address != addr || a.Name != "" {
			return fmt.Errorf("the subject %q is not a clean mailto: address (want mailto:someone@example.com)", s)
		}
		return nil
	case strings.HasPrefix(s, "https://"):
		u, err := url.Parse(s)
		if err != nil || u.Host == "" || u.User != nil {
			return fmt.Errorf("the subject %q is not a clean https URL", s)
		}
		return nil
	default:
		return fmt.Errorf("the subject %q must be mailto:<address> or an https:// URL", s)
	}
}

// decodeKey accepts the unpadded base64url form GenerateVAPIDKeys emits, and
// the padded one some generators print.
func decodeKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("empty")
	}
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

// webpushSubscriber is the Subscriber value webpush-go must be handed for the
// JWT's `sub` claim to come out as Subject. See the package doc: v1.4.0 adds
// "mailto:" to anything not starting with "https:", so a mailto: subject is
// passed WITHOUT its scheme. Pinned by TestTheJWTSubjectIsTheConfiguredSubject.
func webpushSubscriber(subject string) string {
	return strings.TrimPrefix(subject, "mailto:")
}

// Subscription is a browser PushSubscription as PushSubscription.toJSON()
// serialises it: the endpoint and the two client keys.
type Subscription struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// maxEndpointLen bounds what a browser may store. Real endpoints are a few
// hundred bytes.
const maxEndpointLen = 2048

// ErrBadSubscription is the refusal for a subscription muster will not store.
var ErrBadSubscription = errors.New("push: invalid subscription")

// Check refuses a subscription that cannot be sent to, or whose endpoint is not
// an https URL on a host (the push service). It does not decide WHICH push
// services are acceptable: browsers choose, and the set changes.
func (s Subscription) Check() error {
	if s.Endpoint == "" || s.Keys.P256dh == "" || s.Keys.Auth == "" {
		return fmt.Errorf("%w: it needs an endpoint and both keys", ErrBadSubscription)
	}
	if len(s.Endpoint) > maxEndpointLen || len(s.Keys.P256dh) > 256 || len(s.Keys.Auth) > 64 {
		return fmt.Errorf("%w: a field is longer than any browser produces", ErrBadSubscription)
	}
	u, err := url.Parse(s.Endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || strings.ContainsAny(s.Endpoint, " \t\r\n") {
		return fmt.Errorf("%w: the endpoint is not an https URL", ErrBadSubscription)
	}
	if u.Path == "" {
		return fmt.Errorf("%w: the endpoint has no path", ErrBadSubscription)
	}
	return nil
}

func (s Subscription) toWebpush() *webpush.Subscription {
	return &webpush.Subscription{Endpoint: s.Endpoint, Keys: webpush.Keys{P256dh: s.Keys.P256dh, Auth: s.Keys.Auth}}
}

// SendResult is one delivery attempt's outcome.
type SendResult struct {
	// StatusCode is the push service's status, or 0 when the request never
	// completed.
	StatusCode int
	// Expired: the push service says the subscription is gone (404/410); prune it.
	Expired bool
	Err     error
}

// Pusher sends one encrypted message to one subscription. An interface so a
// test never touches the network.
type Pusher interface {
	Send(ctx context.Context, sub Subscription, payload []byte) SendResult
}

// WebpushPusher is the production Pusher (webpush-go).
type WebpushPusher struct {
	vapid  VAPIDConfig
	client webpush.HTTPClient
	ttl    int
}

// NewWebpushPusher builds a Pusher signing with vapid. Its HTTP client has a
// short timeout so a stuck push service cannot hold a fan-out open, and it
// never follows a redirect (pushHTTPClient).
func NewWebpushPusher(vapid VAPIDConfig) *WebpushPusher { return newWebpushPusher(vapid, nil) }

// newWebpushPusher is NewWebpushPusher over transport rt (a test's TLS
// server); the redirect rule is the production one.
func newWebpushPusher(vapid VAPIDConfig, rt http.RoundTripper) *WebpushPusher {
	return &WebpushPusher{vapid: vapid, client: pushHTTPClient(rt), ttl: 3600}
}

// pushHTTPClient is the client every push send uses. rt nil is the default
// transport.
//
// 🔴 IT REFUSES EVERY REDIRECT. The host allowlist (Allowlist) checks only
// the endpoint muster was handed, so a client that followed redirects would
// let a listed host bounce the POST to ANY host the pod can reach, which is
// exactly what the allowlist exists to prevent. CheckRedirect returns
// http.ErrUseLastResponse, so the 3xx itself comes back and Send counts it as
// a failed send (ErrRedirectRefused). This is for every push, the browser
// push services included (docs/design/android.md, server change 21).
func pushHTTPClient(rt http.RoundTripper) *http.Client {
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: rt,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// ErrRedirectRefused is a send the push service answered with a redirect.
// muster does not follow it, and the send counts as failed.
var ErrRedirectRefused = errors.New("push: the push service answered with a redirect, which muster does not follow")

// Send encrypts payload for sub and POSTs it. 2xx is success; 404/410 expire;
// a 3xx is a failed send (ErrRedirectRefused), never followed.
func (p *WebpushPusher) Send(ctx context.Context, sub Subscription, payload []byte) SendResult {
	resp, err := webpush.SendNotificationWithContext(ctx, payload, sub.toWebpush(), &webpush.Options{
		HTTPClient:      p.client,
		Subscriber:      webpushSubscriber(p.vapid.Subject),
		VAPIDPublicKey:  p.vapid.PublicKey,
		VAPIDPrivateKey: p.vapid.PrivateKey,
		TTL:             p.ttl,
		Urgency:         webpush.UrgencyHigh,
	})
	if err != nil {
		return SendResult{Err: err}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return SendResult{StatusCode: resp.StatusCode, Err: fmt.Errorf("%w (status %d)", ErrRedirectRefused, resp.StatusCode)}
	}
	return SendResult{
		StatusCode: resp.StatusCode,
		Expired:    resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone,
	}
}
