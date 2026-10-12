package gotrue_test

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/gotrue"
	"github.com/ZacxDev/muster/internal/gotrue/gotruetest"
)

var secret = []byte(strings.Repeat("k", 40))

// TestEveryCallCarriesTheClientIP: GoTrue serves a request with no
// X-Muster-Client-IP (measured), so the CLIENT refuses to send one. Every
// method, with an empty address, fails with ErrNoClientIP and reaches GoTrue
// zero times; with an address, every request GoTrue sees carries it.
func TestEveryCallCarriesTheClientIP(t *testing.T) {
	f := gotruetest.New(t, secret)
	c := f.Client()
	ctx := context.Background()
	id := f.AddUser("a@example.com", "a-long-enough-password")

	calls := map[string]func(ip string) error{
		"PasswordGrant": func(ip string) error {
			_, err := c.PasswordGrant(ctx, ip, "a@example.com", "a-long-enough-password")
			return err
		},
		"Refresh": func(ip string) error {
			_, rt, _ := f.Mint(id)
			_, err := c.Refresh(ctx, ip, rt)
			return err
		},
		"Logout": func(ip string) error {
			at, _, _ := f.Mint(id)
			return c.Logout(ctx, ip, at, gotrue.ScopeLocal)
		},
		"AdminCreateUser": func(ip string) error {
			_, err := c.AdminCreateUser(ctx, ip, "new-"+ip+"@example.com", "another-long-password")
			return err
		},
		"AdminSetBanned": func(ip string) error { return c.AdminSetBanned(ctx, ip, id, false) },
		"AdminDeleteUser": func(ip string) error {
			victim := f.AddUser("victim-"+ip+"@example.com", "a-long-enough-password")
			return c.AdminDeleteUser(ctx, ip, victim)
		},
		// accounts PR 5: the reset link.
		"AdminRecoveryLink": func(ip string) error {
			_, err := c.AdminRecoveryLink(ctx, ip, "a@example.com")
			return err
		},
		"VerifyRecovery": func(ip string) error {
			_, err := c.VerifyRecovery(ctx, ip, f.RecoveryHash(id))
			return err
		},
		"AdminSetPassword":       func(ip string) error { return c.AdminSetPassword(ctx, ip, id, "a-long-enough-password") },
		"AdminSignOutEverywhere": func(ip string) error { return c.AdminSignOutEverywhere(ctx, ip, id) },
	}
	for name, call := range calls {
		before := len(f.Calls())
		if err := call(""); !errors.Is(err, gotrue.ErrNoClientIP) {
			t.Errorf("%s with no client IP: got %v, want ErrNoClientIP", name, err)
		}
		if n := len(f.Calls()) - before; n != 0 {
			t.Errorf("%s with no client IP still reached GoTrue %d time(s)", name, n)
		}
		before = len(f.Calls())
		if err := call("198.51.100.7"); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		got := f.Calls()[before:]
		if len(got) == 0 {
			t.Errorf("%s: positive control — no request reached the fake", name)
		}
		for _, cl := range got {
			if cl.ClientIP != "198.51.100.7" {
				t.Errorf("%s: %s %s carried %s=%q", name, cl.Method, cl.Path, gotrue.ClientIPHeader, cl.ClientIP)
			}
		}
	}
}

// TestAdminCreateUserEnforcesTheLengthFloorAndConfirmsTheEmail: GoTrue's admin
// path checks neither (measured), so both are the client's job.
func TestAdminCreateUserEnforcesTheLengthFloorAndConfirmsTheEmail(t *testing.T) {
	f := gotruetest.New(t, secret)
	c := f.Client()
	ctx := context.Background()
	if _, err := c.AdminCreateUser(ctx, "ip", "short@example.com", strings.Repeat("x", gotrue.MinPasswordLength-1)); !errors.Is(err, gotrue.ErrWeakPassword) {
		t.Fatalf("11 characters: got %v, want ErrWeakPassword", err)
	}
	if f.CountCalls("POST", "/admin/users") != 0 || f.UserCount() != 0 {
		t.Fatal("a refused password still reached GoTrue")
	}
	id, err := c.AdminCreateUser(ctx, "ip", "ok@example.com", strings.Repeat("x", gotrue.MinPasswordLength))
	if err != nil {
		t.Fatalf("12 characters: %v", err)
	}
	u, ok := f.User(id)
	if !ok || !u.Confirmed {
		t.Fatalf("the created user is not confirmed (email_confirm not sent): %+v", u)
	}
	if _, err := c.PasswordGrant(ctx, "ip", "ok@example.com", strings.Repeat("x", gotrue.MinPasswordLength)); err != nil {
		t.Fatalf("the created user cannot sign in: %v", err)
	}
}

// TestTheServiceRoleTokenLivesSixtySeconds pins the admin token's shape: HS256,
// role service_role, our issuer, and a 60 s life.
func TestTheServiceRoleTokenLivesSixtySeconds(t *testing.T) {
	now := time.Unix(946684800, 0)
	c := &gotrue.Client{Secret: secret, Issuer: "muster-auth", Now: func() time.Time { return now }}
	tok, err := c.ServiceToken()
	if err != nil {
		t.Fatal(err)
	}
	// The service token carries no audience, so the session verifier refuses
	// it: it can never pass as a browser session.
	sess := gotrue.Verifier{Secret: secret, Issuer: "muster-auth", Audience: "authenticated", Now: func() time.Time { return now }}
	if _, err := sess.Verify(tok); !errors.Is(err, gotrue.ErrAudience) {
		t.Fatalf("service token verified as a session: %v", err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatal("not a JWS")
	}
	payload := decodeSegment(t, parts[1])
	for _, want := range []string{`"role":"service_role"`, `"iss":"muster-auth"`, `"exp":946684860`, `"iat":946684800`} {
		if !strings.Contains(payload, want) {
			t.Errorf("service token payload %s lacks %s", payload, want)
		}
	}
}

func decodeSegment(t *testing.T, seg string) string {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestTheRecoveryExchangeIsSingleUseAndTheNewPasswordHasAFloor: a recovery
// token_hash exchanges for a session once; AdminSetPassword refuses a short
// password without calling GoTrue (its admin path does not check the length).
func TestTheRecoveryExchangeIsSingleUseAndTheNewPasswordHasAFloor(t *testing.T) {
	f := gotruetest.New(t, secret)
	c := f.Client()
	ctx := context.Background()
	id := f.AddUser("r@example.com", "a-long-enough-password")
	h, err := c.AdminRecoveryLink(ctx, "198.51.100.9", "r@example.com")
	if err != nil || h == "" {
		t.Fatalf("recovery link: %q %v", h, err)
	}
	sess, err := c.VerifyRecovery(ctx, "198.51.100.9", h)
	if err != nil || sess.User.ID != id || sess.AccessToken == "" {
		t.Fatalf("verify: %+v %v", sess, err)
	}
	if _, err := c.VerifyRecovery(ctx, "198.51.100.9", h); !gotrue.IsClientError(err) {
		t.Fatalf("second exchange: %v, want a 4xx", err)
	}
	before := len(f.Calls())
	if err := c.AdminSetPassword(ctx, "198.51.100.9", id, "elevenchars"); !errors.Is(err, gotrue.ErrWeakPassword) {
		t.Fatalf("11 characters: %v", err)
	}
	if len(f.Calls()) != before {
		t.Fatal("a refused password reached GoTrue")
	}
	if err := c.AdminSetPassword(ctx, "198.51.100.9", id, "twelve-chars"); err != nil {
		t.Fatal(err)
	}
	if u, _ := f.User(id); u.Password != "twelve-chars" {
		t.Fatal("the password did not change")
	}
}

// TestSignOutEverywhereRevokesEverySessionOfTheUser: the minted-token global
// sign-out ends all of the user's sessions and no other user's.
func TestSignOutEverywhereRevokesEverySessionOfTheUser(t *testing.T) {
	f := gotruetest.New(t, secret)
	c := f.Client()
	a := f.AddUser("a@example.com", "a-long-enough-password")
	b := f.AddUser("b@example.com", "a-long-enough-password")
	_, _, s1 := f.Mint(a)
	_, _, s2 := f.Mint(a)
	_, _, other := f.Mint(b)
	if err := c.AdminSignOutEverywhere(context.Background(), "198.51.100.1", a); err != nil {
		t.Fatal(err)
	}
	if !f.SessionRevoked(s1) || !f.SessionRevoked(s2) {
		t.Fatal("a session of the user survived")
	}
	if f.SessionRevoked(other) {
		t.Fatal("another user's session was revoked")
	}
}
