//go:build !e2eterm

package main

import (
	"errors"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/gotrue"
)

// TestTheDefaultBuildCarriesNoFakeTerminal: without the e2eterm tag — the
// build the Dockerfile ships — nothing sets the e2e terminal hook, so the noop
// driver is returned unwrapped and cannot exec.
func TestTheDefaultBuildCarriesNoFakeTerminal(t *testing.T) {
	if e2eFakeTerminal != nil {
		t.Fatal("e2eFakeTerminal is set in a build without -tags e2eterm; a deployable binary would carry a fake terminal")
	}
}

// TestTheDefaultBuildCarriesNoUpstreamHooks: without the e2eterm tag, the hooks
// that point GitHub and OpenRouter at a browser test's fakes
// (e2eupstreams.go) are not compiled in. Its init sets e2eUpstreamHooks, so a
// lost `//go:build e2eterm` line turns this red rather than shipping a binary
// whose upstreams an environment variable can redirect.
func TestTheDefaultBuildCarriesNoUpstreamHooks(t *testing.T) {
	if e2eUpstreamHooks {
		t.Fatal("e2eupstreams.go's init ran in a build without -tags e2eterm; a deployable binary would let " +
			"MUSTER_E2E_GITHUB_API / MUSTER_E2E_OPENROUTER_MODELS redirect its upstreams")
	}
	if e2eSuggestGen != nil {
		t.Fatal("e2eSuggestGen is set in a build without -tags e2eterm; a deployable binary would let " +
			"MUSTER_E2E_SUGGEST_URL replace the suggested replies' model")
	}
}

// TestTheDefaultBuildCannotChangeTheGoTrueSkew: without the e2eterm tag the
// GoTrue verifier's skew cannot be changed — MUSTER_E2E_GOTRUE_SKEW has no
// reader — so a deployed muster verifies with gotrue.DefaultSkew, 30 s. The
// behavioural half: a token that expired 29 s ago still verifies through the
// verifier goTrueAuth builds, with the variable set to 1s.
func TestTheDefaultBuildCannotChangeTheGoTrueSkew(t *testing.T) {
	t.Setenv(envE2EGoTrueSkew, "1s")
	if e2eGoTrueSkew != nil {
		t.Fatal("e2egotrue.go's init ran in a build without -tags e2eterm; a deployable binary would take its " +
			"GoTrue clock skew from " + envE2EGoTrueSkew)
	}
	if gotrue.DefaultSkew != 30*time.Second {
		t.Fatalf("gotrue.DefaultSkew = %s, want 30s", gotrue.DefaultSkew)
	}
	v := goTrueAuth(config{GoTrueSecret: "a-test-secret-that-is-long-enough-000000"}, nil).Verifier
	if v.Skew != 0 {
		t.Fatalf("goTrueAuth's verifier Skew = %s in a default build, want 0 (gotrue.DefaultSkew)", v.Skew)
	}
	now := time.Now()
	tok, err := gotrue.Sign(v.Secret, gotrue.Claims{Subject: "u1", Issuer: gotrueIssuer, Audience: []string{gotrueAudience},
		ExpiresAt: now.Add(-29 * time.Second).Unix(), IssuedAt: now.Add(-60 * time.Second).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(tok); err != nil {
		t.Fatalf("a token 29 s past exp: %v; want it accepted within the 30 s production skew", err)
	}
	// CONTROL: 31 s past exp is refused, so the case above sat inside the skew.
	tok, _ = gotrue.Sign(v.Secret, gotrue.Claims{Subject: "u1", Issuer: gotrueIssuer, Audience: []string{gotrueAudience},
		ExpiresAt: now.Add(-31 * time.Second).Unix(), IssuedAt: now.Add(-60 * time.Second).Unix()})
	if _, err := v.Verify(tok); !errors.Is(err, gotrue.ErrExpired) {
		t.Fatalf("CONTROL: a token 31 s past exp: %v; want gotrue.ErrExpired", err)
	}
}
