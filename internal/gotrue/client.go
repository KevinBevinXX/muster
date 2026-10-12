package gotrue

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ClientIPHeader is the header GoTrue buckets its rate limits on
// (GOTRUE_RATE_LIMIT_HEADER in the deployment).
//
// 🔴 EVERY CALL THIS CLIENT MAKES CARRIES IT, AND A CALL WITHOUT ONE IS REFUSED
// HERE RATHER THAN SENT. Measured against GoTrue v2.189.0: a request with no such
// header is NOT rejected, it is served — and bucketed with every other headerless
// request, so one guesser exhausts everyone's budget, or (worse) nobody is limited
// at all. GoTrue cannot enforce that muster sets it; this client does
// (TestEveryCallCarriesTheClientIP).
const ClientIPHeader = "X-Muster-Client-IP"

// MinPasswordLength is the shortest password muster will hand GoTrue to set.
//
// 🔴 ENFORCED HERE BECAUSE GoTrue DOES NOT ENFORCE IT ON THE ADMIN PATH. Measured
// against v2.189.0: POST /admin/users accepted an 11-character password with
// GOTRUE_PASSWORD_MIN_LENGTH=12 set — only the user self-service path checks it.
// Every password muster sets goes through the admin path.
const MinPasswordLength = 12

// ServiceTokenTTL is how long the service_role token an admin call carries
// lives. Each admin call mints a fresh one.
const ServiceTokenTTL = 60 * time.Second

// ErrWeakPassword is AdminCreateUser's refusal of a password shorter than
// MinPasswordLength. Nothing is sent to GoTrue.
var ErrWeakPassword = fmt.Errorf("gotrue: password must be at least %d characters", MinPasswordLength)

// ErrNoClientIP is the refusal of a call with no client IP (see ClientIPHeader).
var ErrNoClientIP = errors.New("gotrue: refusing a call with no " + ClientIPHeader + " value")

// APIError is a non-2xx answer from GoTrue.
type APIError struct {
	Status int
	// Code is GoTrue's machine-readable code (error_code, or the OAuth `error`).
	Code string
	Msg  string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("gotrue: HTTP %d %s: %s", e.Status, e.Code, e.Msg)
}

// IsClientError reports whether err is a 4xx from GoTrue: the credential or the
// request was refused, as opposed to GoTrue being unreachable or failing.
func IsClientError(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status >= 400 && ae.Status < 500
}

// Session is a token pair GoTrue issued.
type Session struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	User         struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	} `json:"user"`
}

// LogoutScope is GoTrue's /logout scope.
type LogoutScope string

const (
	// ScopeLocal revokes the session this access token belongs to.
	ScopeLocal LogoutScope = "local"
	// ScopeGlobal revokes every session of the user ("sign out everywhere").
	ScopeGlobal LogoutScope = "global"
)

// Client talks to one GoTrue instance.
type Client struct {
	// BaseURL is GoTrue's root, e.g. http://auth.example.com:9999.
	BaseURL string
	// Secret is the shared HS256 secret; admin calls sign their service_role
	// token with it.
	Secret []byte
	// Issuer is put on the service_role token (GOTRUE_JWT_ISSUER).
	Issuer string
	// HTTP is the transport; nil means a client with a 10 s timeout.
	HTTP *http.Client
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

// PasswordGrant signs a user in.
func (c *Client) PasswordGrant(ctx context.Context, clientIP, email, password string) (Session, error) {
	var s Session
	err := c.do(ctx, clientIP, http.MethodPost, "/token?grant_type=password", "",
		map[string]string{"email": email, "password": password}, &s)
	return s, err
}

// Refresh exchanges a refresh token for a new pair.
//
// ⚠ GoTrue ROTATES on every refresh: the token passed in is spent. Measured
// (rotation on, no reuse interval): replaying the PARENT of the current token is
// accepted, replaying an older one revokes the whole session. internal/api
// coalesces concurrent refreshes of one token so two tabs do not walk a session
// into that revocation.
func (c *Client) Refresh(ctx context.Context, clientIP, refreshToken string) (Session, error) {
	var s Session
	err := c.do(ctx, clientIP, http.MethodPost, "/token?grant_type=refresh_token", "",
		map[string]string{"refresh_token": refreshToken}, &s)
	return s, err
}

// Logout revokes the session accessToken belongs to (ScopeLocal) or every
// session of its user (ScopeGlobal).
func (c *Client) Logout(ctx context.Context, clientIP, accessToken string, scope LogoutScope) error {
	return c.do(ctx, clientIP, http.MethodPost, "/logout?scope="+url.QueryEscape(string(scope)), accessToken, nil, nil)
}

// AdminCreateUser creates a CONFIRMED user and returns its id (the `sub`).
//
// 🔴 email_confirm IS SENT, ALWAYS. Measured against v2.189.0: /admin/users
// ignores GOTRUE_MAILER_AUTOCONFIRM, and a user created without it cannot sign
// in (400 email_not_confirmed). muster has no SMTP, so nobody would ever confirm.
func (c *Client) AdminCreateUser(ctx context.Context, clientIP, email, password string) (string, error) {
	if len(password) < MinPasswordLength {
		return "", ErrWeakPassword
	}
	var u struct {
		ID string `json:"id"`
	}
	if err := c.admin(ctx, clientIP, http.MethodPost, "/admin/users",
		map[string]any{"email": email, "password": password, "email_confirm": true}, &u); err != nil {
		return "", err
	}
	if u.ID == "" {
		return "", errors.New("gotrue: /admin/users answered with no user id")
	}
	return u.ID, nil
}

// banForever is the ban GoTrue applies for a disabled muster user. GoTrue takes
// a Go duration; "none" lifts a ban.
const banForever = "876000h"

// AdminSetBanned bans (or un-bans) a user. A ban also makes GoTrue refuse that
// user's refresh tokens.
func (c *Client) AdminSetBanned(ctx context.Context, clientIP, userID string, banned bool) error {
	d := "none"
	if banned {
		d = banForever
	}
	return c.admin(ctx, clientIP, http.MethodPut, "/admin/users/"+url.PathEscape(userID),
		map[string]any{"ban_duration": d}, nil)
}

// AdminDeleteUser deletes a user. muster uses it only to compensate a GoTrue
// user whose muster-side binding failed to commit.
func (c *Client) AdminDeleteUser(ctx context.Context, clientIP, userID string) error {
	return c.admin(ctx, clientIP, http.MethodDelete, "/admin/users/"+url.PathEscape(userID), nil, nil)
}

// ServiceToken mints the service_role token an admin call carries.
func (c *Client) ServiceToken() (string, error) {
	now := c.now()
	return Sign(c.Secret, Claims{
		Subject:   "muster-server",
		Issuer:    c.Issuer,
		Role:      "service_role",
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(ServiceTokenTTL).Unix(),
	})
}

func (c *Client) admin(ctx context.Context, clientIP, method, path string, body, out any) error {
	tok, err := c.ServiceToken()
	if err != nil {
		return err
	}
	return c.do(ctx, clientIP, method, path, tok, body, out)
}

func (c *Client) do(ctx context.Context, clientIP, method, path, bearer string, body, out any) error {
	if strings.TrimSpace(clientIP) == "" {
		return ErrNoClientIP
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set(ClientIPHeader, clientIP)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("gotrue: %s %s: %w", method, pathOnly(path), err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("gotrue: read %s %s: %w", method, pathOnly(path), err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return decodeError(resp.StatusCode, raw)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("gotrue: decode %s %s: %w", method, pathOnly(path), err)
		}
	}
	return nil
}

func pathOnly(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		return p[:i]
	}
	return p
}

// decodeError reads either of GoTrue's error shapes: v2's
// {"code","error_code","msg"} and the OAuth {"error","error_description"}.
func decodeError(status int, raw []byte) error {
	var e struct {
		ErrorCode   string `json:"error_code"`
		Msg         string `json:"msg"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
		Message     string `json:"message"`
	}
	_ = json.Unmarshal(raw, &e)
	ae := &APIError{Status: status, Code: e.ErrorCode, Msg: e.Msg}
	if ae.Code == "" {
		ae.Code = e.Error
	}
	if ae.Msg == "" {
		ae.Msg = e.Description
	}
	if ae.Msg == "" {
		ae.Msg = e.Message
	}
	if ae.Msg == "" && ae.Code == "" {
		ae.Msg = strings.TrimSpace(string(raw))
		if len(ae.Msg) > 200 {
			ae.Msg = ae.Msg[:200]
		}
	}
	return ae
}

// --- accounts PR 5: the password-reset link -----------------------------------
//
// muster has no SMTP, so a reset is a link the ADMIN hands over: GoTrue's
// admin/generate_link mints a recovery token for the user, muster puts its
// HASHED form in the fragment of `<public>/auth/reset#<hash>`, and the reset
// page posts it back. muster — never the browser — exchanges it at GoTrue's
// /verify for a session, sets the new password through the admin path (where
// MinPasswordLength is enforced here, as for AdminCreateUser), and signs in.

// AdminRecoveryLink mints a single-use recovery token for email and returns its
// hashed form (GoTrue's `hashed_token`), which /verify accepts as token_hash.
func (c *Client) AdminRecoveryLink(ctx context.Context, clientIP, email string) (string, error) {
	var out struct {
		HashedToken string `json:"hashed_token"`
		Properties  struct {
			HashedToken string `json:"hashed_token"`
		} `json:"properties"`
	}
	if err := c.admin(ctx, clientIP, http.MethodPost, "/admin/generate_link",
		map[string]any{"type": "recovery", "email": email}, &out); err != nil {
		return "", err
	}
	h := out.HashedToken
	if h == "" {
		h = out.Properties.HashedToken
	}
	if h == "" {
		return "", errors.New("gotrue: /admin/generate_link answered with no hashed_token")
	}
	return h, nil
}

// VerifyRecovery exchanges a recovery token_hash for a session. GoTrue spends
// the token: a second exchange is refused.
func (c *Client) VerifyRecovery(ctx context.Context, clientIP, tokenHash string) (Session, error) {
	var s Session
	err := c.do(ctx, clientIP, http.MethodPost, "/verify", "",
		map[string]string{"type": "recovery", "token_hash": tokenHash}, &s)
	if err == nil && s.AccessToken == "" {
		err = errors.New("gotrue: /verify answered with no session")
	}
	return s, err
}

// AdminSignOutEverywhere revokes every GoTrue session (refresh token) of user
// userID, as a disable must.
//
// 🔴 GoTrue HAS NO ADMIN "SIGN OUT USER" ENDPOINT, AND A BAN DOES NOT END A
// SESSION — measured on v2.189.0: a refresh token from before a ban is
// accepted again once the ban is lifted. What does end them is the user's own
// global sign-out, and muster holds the HS256 secret GoTrue verifies with, so
// it mints a short-lived token FOR the user (no session_id: GoTrue refuses a
// session_id that is not a real session's UUID) and calls /logout?scope=global
// with it. Measured: 204, and the user's refresh tokens are then
// refresh_token_not_found.
func (c *Client) AdminSignOutEverywhere(ctx context.Context, clientIP, userID string) error {
	now := c.now()
	tok, err := Sign(c.Secret, Claims{
		Subject:   userID,
		Issuer:    c.Issuer,
		Audience:  Audience{"authenticated"},
		Role:      "authenticated",
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(ServiceTokenTTL).Unix(),
	})
	if err != nil {
		return err
	}
	return c.Logout(ctx, clientIP, tok, ScopeGlobal)
}

// AdminSetPassword sets userID's password through the admin path.
func (c *Client) AdminSetPassword(ctx context.Context, clientIP, userID, password string) error {
	if len(password) < MinPasswordLength {
		return ErrWeakPassword
	}
	return c.admin(ctx, clientIP, http.MethodPut, "/admin/users/"+url.PathEscape(userID),
		map[string]any{"password": password}, nil)
}
