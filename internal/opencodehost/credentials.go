package opencodehost

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

// Credentials is the pair token muster-mirror reads opencode with, and the
// file it lives in. Renew replaces it before it expires — a token can mint
// its own replacement (POST /api/pair is accepted with a token wherever the
// password is) — and writes the new one to the same file, 0600.
type Credentials struct {
	// Path is the token file; "" for a token from the environment, which
	// cannot be renewed (there is nowhere to keep the new one).
	Path string

	mu sync.Mutex
	tf TokenFile
}

// LoadCredentials reads the token file at path (ReadTokenFile).
func LoadCredentials(path string) (*Credentials, error) {
	tf, err := ReadTokenFile(path)
	if err != nil {
		return nil, err
	}
	return &Credentials{Path: path, tf: tf}, nil
}

// EnvCredentials is a token handed over in the environment, for the server at
// base (CheckLocal): read-only, never renewed.
func EnvCredentials(base, token string) (*Credentials, error) {
	if err := CheckLocal(base); err != nil {
		return nil, err
	}
	if _, ok := Expiry(token); !ok {
		return nil, errors.New("the opencode token in the environment is not a pair token")
	}
	return &Credentials{tf: TokenFile{URL: base, Token: token}}, nil
}

// Token is the current token.
func (c *Credentials) Token() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tf.Token
}

// URL is the server the token was minted by.
func (c *Credentials) URL() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tf.URL
}

// Expires is when the current token stops being accepted.
func (c *Credentials) Expires() time.Time {
	t, _ := Expiry(c.Token())
	return t
}

// Client is a client of the token's server that authenticates with the
// current token at each request.
func (c *Credentials) Client() (*Client, error) { return NewClient(c.URL(), c.Token) }

// ErrNotRenewable: a token from the environment has no file to keep a new one
// in.
var ErrNotRenewable = errors.New("an opencode token from the environment is not renewed: pair the host (muster-mirror opencode-pair) to get a token file")

// Renew replaces the token when it expires within RenewBefore of now: it
// mints a new one with the current token, checks it outlives the old one,
// writes it to the token file (0600, whole) and only then uses it. renewed is
// false, with no error, when the token is not yet due.
//
// 🔴 THE FILE IS WRITTEN BEFORE THE NEW TOKEN IS USED: a muster-mirror that
// dies between the two restarts on the new token, never on one it has
// already replaced. Both tokens stay valid until their own expiry (tokens are
// stateless upstream), so the order costs nothing.
func (c *Credentials) Renew(ctx context.Context, now time.Time) (renewed bool, err error) {
	old := c.Token()
	exp, _ := Expiry(old)
	if exp.Sub(now) > RenewBefore {
		return false, nil
	}
	if c.Path == "" {
		return false, ErrNotRenewable
	}
	if !exp.After(now) {
		return false, fmt.Errorf("the opencode token (fingerprint %s) expired %s; pair the host again (muster-mirror opencode-pair)",
			Fingerprint(old), exp.UTC().Format(time.RFC3339))
	}
	cl, err := c.Client()
	if err != nil {
		return false, err
	}
	tok, err := cl.mint(ctx)
	if err != nil {
		return false, fmt.Errorf("renew the opencode token: %w", err)
	}
	if nexp, _ := Expiry(tok); !nexp.After(exp) {
		return false, errors.New("renew the opencode token: the new token does not outlive the old one")
	}
	tf := TokenFile{URL: c.URL(), Token: tok}
	if err := WriteTokenFile(c.Path, tf); err != nil {
		return false, fmt.Errorf("renew the opencode token: write %s: %w", c.Path, err)
	}
	c.mu.Lock()
	c.tf = tf
	c.mu.Unlock()
	return true, nil
}

// RenewEvery is how often KeepRenewed checks the token.
const RenewEvery = time.Hour

// KeepRenewed renews c (Renew) now and every RenewEvery until ctx ends,
// logging each renewal and each failure by fingerprint only. It returns at
// once for a token that cannot be renewed (logging so).
func KeepRenewed(ctx context.Context, c *Credentials, logger *log.Logger, every time.Duration, now func() time.Time) {
	if c.Path == "" {
		logger.Printf("muster-mirror: %v", ErrNotRenewable)
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		old := c.Token()
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		renewed, err := c.Renew(rctx, now())
		cancel()
		switch {
		case err != nil && ctx.Err() == nil:
			logger.Printf("muster-mirror: opencode token %s expires %s; %v (retrying in %s)",
				Fingerprint(old), c.Expires().UTC().Format(time.RFC3339), err, every)
		case renewed:
			logger.Printf("muster-mirror: renewed the opencode token: %s → %s, expires %s, written to %s (0600)",
				Fingerprint(old), Fingerprint(c.Token()), c.Expires().UTC().Format(time.RFC3339), c.Path)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
