package gotrue

// --- passkeys (GoTrue v2.197.0's native /passkeys API) -------------------------
//
// muster PROXIES these: the browser talks to muster, muster talks to GoTrue
// server-side, exactly as for the password grant. Every call carries the
// client IP (ClientIPHeader), like every other call this client makes.
//
// The browser-facing WebAuthn options are handed through as raw JSON (muster
// edits one field, userVerification — see RequireUserVerification), and the
// credential the browser returns is CANONICALISED before it is forwarded (see
// webauthn.go): GoTrue verifies exactly the bytes muster checked.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"
)

// PasskeyOptions is GoTrue's answer to an options call: a single-use challenge
// (bound server-side to the WebAuthn session data) and the options to hand to
// navigator.credentials.get / create.
type PasskeyOptions struct {
	ChallengeID string          `json:"challenge_id"`
	Options     json.RawMessage `json:"options"`
	ExpiresAt   int64           `json:"expires_at"`
}

// Passkey is one registered passkey as GoTrue lists it.
type Passkey struct {
	ID           string     `json:"id"`
	FriendlyName string     `json:"friendly_name,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	LastUsedAt   *time.Time `json:"last_used_at,omitempty"`
}

// Settings is the part of GoTrue's public /settings muster reads.
type Settings struct {
	PasskeysEnabled bool `json:"passkeys_enabled"`
}

// Settings reads GoTrue's /settings.
func (c *Client) Settings(ctx context.Context, clientIP string) (Settings, error) {
	var s Settings
	err := c.do(ctx, clientIP, http.MethodGet, "/settings", "", nil, &s)
	return s, err
}

func (o PasskeyOptions) check(what string) error {
	if o.ChallengeID == "" || len(o.Options) == 0 {
		return errors.New("gotrue: " + what + " answered with no challenge")
	}
	return nil
}

// PasskeySignInOptions starts a usernameless (discoverable-credential) sign-in.
func (c *Client) PasskeySignInOptions(ctx context.Context, clientIP string) (PasskeyOptions, error) {
	var o PasskeyOptions
	if err := c.do(ctx, clientIP, http.MethodPost, "/passkeys/authentication/options", "", map[string]any{}, &o); err != nil {
		return o, err
	}
	return o, o.check("/passkeys/authentication/options")
}

// PasskeySignInVerify finishes a passkey sign-in: GoTrue verifies the
// assertion against the challenge and, for the user its userHandle names,
// issues a session. credential must already be canonical (CanonicalAssertion).
func (c *Client) PasskeySignInVerify(ctx context.Context, clientIP, challengeID string, credential json.RawMessage) (Session, error) {
	var s Session
	err := c.do(ctx, clientIP, http.MethodPost, "/passkeys/authentication/verify", "",
		map[string]any{"challenge_id": challengeID, "credential": credential}, &s)
	if err == nil && s.AccessToken == "" {
		err = errors.New("gotrue: /passkeys/authentication/verify answered with no session")
	}
	return s, err
}

// PasskeyRegisterOptions starts adding a passkey to the account accessToken
// belongs to.
func (c *Client) PasskeyRegisterOptions(ctx context.Context, clientIP, accessToken string) (PasskeyOptions, error) {
	var o PasskeyOptions
	if err := c.do(ctx, clientIP, http.MethodPost, "/passkeys/registration/options", accessToken, map[string]any{}, &o); err != nil {
		return o, err
	}
	return o, o.check("/passkeys/registration/options")
}

// PasskeyRegisterVerify finishes adding a passkey. credential must already be
// canonical (CanonicalAttestation).
func (c *Client) PasskeyRegisterVerify(ctx context.Context, clientIP, accessToken, challengeID string, credential json.RawMessage) (Passkey, error) {
	var p Passkey
	err := c.do(ctx, clientIP, http.MethodPost, "/passkeys/registration/verify", accessToken,
		map[string]any{"challenge_id": challengeID, "credential": credential}, &p)
	if err == nil && p.ID == "" {
		err = errors.New("gotrue: /passkeys/registration/verify answered with no passkey id")
	}
	return p, err
}

// Passkeys lists the passkeys of the account accessToken belongs to.
func (c *Client) Passkeys(ctx context.Context, clientIP, accessToken string) ([]Passkey, error) {
	var ps []Passkey
	err := c.do(ctx, clientIP, http.MethodGet, "/passkeys", accessToken, nil, &ps)
	return ps, err
}

// DeletePasskey deletes one of the passkeys of the account accessToken
// belongs to. GoTrue answers 404 for a passkey that is not that account's.
func (c *Client) DeletePasskey(ctx context.Context, clientIP, accessToken, passkeyID string) error {
	return c.do(ctx, clientIP, http.MethodDelete, "/passkeys/"+url.PathEscape(passkeyID), accessToken, nil, nil)
}

// AdminPasskeys lists user userID's passkeys.
func (c *Client) AdminPasskeys(ctx context.Context, clientIP, userID string) ([]Passkey, error) {
	var ps []Passkey
	err := c.admin(ctx, clientIP, http.MethodGet, "/admin/users/"+url.PathEscape(userID)+"/passkeys", nil, &ps)
	return ps, err
}

// AdminDeletePasskey deletes one of user userID's passkeys.
func (c *Client) AdminDeletePasskey(ctx context.Context, clientIP, userID, passkeyID string) error {
	return c.admin(ctx, clientIP, http.MethodDelete,
		"/admin/users/"+url.PathEscape(userID)+"/passkeys/"+url.PathEscape(passkeyID), nil, nil)
}

// AdminDeleteAllPasskeys deletes every passkey user userID has and returns how
// many it deleted. GoTrue has no bulk delete: it lists, then deletes each, and
// stops at the first failure — a caller that must not leave a passkey behind
// treats any error as "some may remain".
func (c *Client) AdminDeleteAllPasskeys(ctx context.Context, clientIP, userID string) (int, error) {
	ps, err := c.AdminPasskeys(ctx, clientIP, userID)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range ps {
		if err := c.AdminDeletePasskey(ctx, clientIP, userID, p.ID); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// AdminAppMetadata reads user userID's app_metadata (admin-writable only; a
// user cannot change it).
func (c *Client) AdminAppMetadata(ctx context.Context, clientIP, userID string) (map[string]any, error) {
	var u struct {
		AppMetadata map[string]any `json:"app_metadata"`
	}
	err := c.admin(ctx, clientIP, http.MethodGet, "/admin/users/"+url.PathEscape(userID), nil, &u)
	return u.AppMetadata, err
}

// AdminSetAppMetadata sets ONE top-level key of user userID's app_metadata.
// GoTrue merges app_metadata by top-level key, so the other keys are kept; a
// nil value removes the key.
func (c *Client) AdminSetAppMetadata(ctx context.Context, clientIP, userID, key string, value any) error {
	return c.admin(ctx, clientIP, http.MethodPut, "/admin/users/"+url.PathEscape(userID),
		map[string]any{"app_metadata": map[string]any{key: value}}, nil)
}
