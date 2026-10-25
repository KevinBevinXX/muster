package opencodehost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/opencodehost/opencodetest"
)

// fixturePassword is the fake server's password: synthetic.
const fixturePassword = "fixture-password-0123456789abcdef"

// writeRegistration writes a registration file shaped like the service's,
// {id, version, url, pid, password}, 0600 — synthetic values.
func writeRegistration(t *testing.T, path, url, password string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"id": "svc_fixture", "version": "2.0.0-fixture", "url": url, "pid": 4242, "password": password})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestPairMintsATokenFromTheRegistrationAndWritesIt0600: the pairing step
// reads the registration file's url and password, asks the server for a
// pairing code with the password, redeems it, and writes {url, token} 0600 —
// a token the server then accepts. It makes exactly those two requests.
func TestPairMintsATokenFromTheRegistrationAndWritesIt0600(t *testing.T) {
	srv := opencodetest.New(fixturePassword)
	defer srv.Close()
	dir := t.TempDir()
	reg := filepath.Join(dir, "state", "opencode", "service-prod.json")
	writeRegistration(t, reg, srv.URL, fixturePassword)
	tokFile := filepath.Join(dir, "muster", "opencode-token")
	res, err := Pair(context.Background(), []string{reg}, tokFile)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(tokFile)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode %v %v", fi.Mode(), err)
	}
	tf, err := ReadTokenFile(tokFile)
	if err != nil {
		t.Fatal(err)
	}
	if tf.URL != srv.URL || res.URL != srv.URL || res.Registration != reg || res.Fingerprint != Fingerprint(tf.Token) || res.Replaced != "" {
		t.Fatalf("result %+v, file %+v", res, tf)
	}
	if tf.Token == fixturePassword || strings.Contains(string(mustRead(t, tokFile)), fixturePassword) {
		t.Fatal("the token file holds the server password")
	}
	if d := time.Until(res.Expires); d < TokenTTL-time.Minute || d > TokenTTL+time.Minute {
		t.Fatalf("expires in %v", d)
	}
	want := []opencodetest.Request{{Method: "POST", Path: "/api/pair", Authorized: true}, {Method: "GET", Path: "/auth/connect/", Authorized: false}}
	got := srv.Requests()
	if len(got) != 2 || got[0] != want[0] || got[1].Method != "GET" || !strings.HasPrefix(got[1].Path, "/auth/connect/") {
		t.Fatalf("requests %+v", got)
	}
	// The token works on its own.
	cl, _ := NewClient(tf.URL, func() string { return tf.Token })
	if _, err := cl.Info(context.Background()); err != nil {
		t.Fatalf("the minted token is refused: %v", err)
	}
	// Pairing again replaces it, and says which token it replaced.
	res2, err := Pair(context.Background(), []string{reg}, tokFile)
	if err != nil || res2.Replaced != res.Fingerprint {
		t.Fatalf("re-pair %+v %v", res2, err)
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestPairRefusesWithoutSendingThePasswordAnywhereButLoopback: a
// registration whose url is not this host's own is refused before a request
// is made, and so is a missing file or one with no password.
func TestPairRefusesWithoutSendingThePasswordAnywhereButLoopback(t *testing.T) {
	srv := opencodetest.New(fixturePassword)
	defer srv.Close()
	dir := t.TempDir()
	reg := filepath.Join(dir, "service-prod.json")
	tokFile := filepath.Join(dir, "tok")
	writeRegistration(t, reg, "http://192.0.2.7:13420", fixturePassword)
	if _, err := Pair(context.Background(), []string{reg}, tokFile); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("a foreign url: %v", err)
	}
	writeRegistration(t, reg, srv.URL, "")
	if _, err := Pair(context.Background(), []string{reg}, tokFile); err == nil || !strings.Contains(err.Error(), "no server password") {
		t.Fatalf("no password: %v", err)
	}
	if _, err := Pair(context.Background(), []string{filepath.Join(dir, "missing.json")}, tokFile); err == nil || !strings.Contains(err.Error(), "no opencode service registration file") {
		t.Fatalf("missing: %v", err)
	}
	if n := len(srv.Requests()); n != 0 {
		t.Fatalf("%d requests were made", n)
	}
	// The wrong password is the server's refusal, with no token written.
	writeRegistration(t, reg, srv.URL, "not-the-password")
	if _, err := Pair(context.Background(), []string{reg}, tokFile); err == nil || !strings.Contains(err.Error(), "refused the password") ||
		strings.Contains(err.Error(), "not-the-password") {
		t.Fatalf("a refused password: %v", err)
	}
	if _, err := os.Stat(tokFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a token file was written: %v", err)
	}
}

// TestPairTakesTheFirstRegistrationThatExists: the Nix build's channel
// first (service-prod.json), then the default channels' (service.json).
func TestPairTakesTheFirstRegistrationThatExists(t *testing.T) {
	srv := opencodetest.New(fixturePassword)
	defer srv.Close()
	dir := t.TempDir()
	paths := DefaultRegistrationPaths(func(k string) string {
		return map[string]string{"XDG_STATE_HOME": filepath.Join(dir, "st"), "HOME": "/nonexistent-home"}[k]
	})
	if len(paths) != 2 || paths[0] != filepath.Join(dir, "st", "opencode", "service-prod.json") || paths[1] != filepath.Join(dir, "st", "opencode", "service.json") {
		t.Fatalf("paths %q", paths)
	}
	if got := DefaultRegistrationPaths(func(k string) string { return map[string]string{"HOME": "/h"}[k] }); got[0] != "/h/.local/state/opencode/service-prod.json" {
		t.Fatalf("HOME paths %q", got)
	}
	writeRegistration(t, paths[1], srv.URL, fixturePassword)
	res, err := Pair(context.Background(), paths, filepath.Join(dir, "tok"))
	if err != nil || res.Registration != paths[1] {
		t.Fatalf("%+v %v", res, err)
	}
}

// pairedCredentials is a host paired with srv: the token file and its
// credentials.
func pairedCredentials(t *testing.T, srv *opencodetest.Server) (*Credentials, string) {
	t.Helper()
	dir := t.TempDir()
	reg := filepath.Join(dir, "service-prod.json")
	writeRegistration(t, reg, srv.URL, fixturePassword)
	tokFile := filepath.Join(dir, "opencode-token")
	if _, err := Pair(context.Background(), []string{reg}, tokFile); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCredentials(tokFile)
	if err != nil {
		t.Fatal(err)
	}
	return c, tokFile
}

// TestRenewReplacesTheTokenBeforeItExpiresAndKeepsItInTheSameFile0600: not
// due, nothing is asked; within RenewBefore of the expiry, the token mints its
// own replacement, which outlives it, is written to the same file 0600 and is
// what the next request sends.
func TestRenewReplacesTheTokenBeforeItExpiresAndKeepsItInTheSameFile0600(t *testing.T) {
	srv := opencodetest.New(fixturePassword)
	defer srv.Close()
	c, tokFile := pairedCredentials(t, srv)
	before := len(srv.Requests())
	old, oldExp := c.Token(), c.Expires()
	if renewed, err := c.Renew(context.Background(), time.Now()); renewed || err != nil {
		t.Fatalf("a fresh token renewed: %v %v", renewed, err)
	}
	if n := len(srv.Requests()); n != before {
		t.Fatalf("a token not due made %d requests", n-before)
	}
	// 25 days on (5 left, inside RenewBefore), on the server's clock too.
	later := time.Now().Add(25 * 24 * time.Hour)
	srv.SetNow(func() time.Time { return later })
	if err := os.Chmod(tokFile, 0o600); err != nil {
		t.Fatal(err)
	}
	renewed, err := c.Renew(context.Background(), later)
	if !renewed || err != nil {
		t.Fatalf("renew: %v %v", renewed, err)
	}
	if c.Token() == old || !c.Expires().After(oldExp) {
		t.Fatalf("the token did not move: %s → %s", oldExp, c.Expires())
	}
	fi, _ := os.Stat(tokFile)
	tf, err := ReadTokenFile(tokFile)
	if err != nil || tf.Token != c.Token() || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file %+v mode %v %v", tf, fi.Mode(), err)
	}
	// The renewal was the mint and nothing else, authenticated by the OLD
	// token (the fake accepts no password here: the test never gave it one).
	reqs := srv.Requests()[before:]
	if len(reqs) != 2 || reqs[0].Method != "POST" || reqs[0].Path != "/api/pair" || !reqs[0].Authorized || !strings.HasPrefix(reqs[1].Path, "/auth/connect/") {
		t.Fatalf("renewal requests %+v", reqs)
	}
	cl, _ := c.Client()
	if _, err := cl.Info(context.Background()); err != nil {
		t.Fatalf("the renewed token is refused: %v", err)
	}
}

// TestARefusedOrExpiredTokenIsNotRenewedAndTheFileIsLeftAlone: a rotated
// server password (which revokes every token) makes the renewal fail with
// the file untouched; an expired token is not even tried; a token from the
// environment cannot be renewed.
func TestARefusedOrExpiredTokenIsNotRenewedAndTheFileIsLeftAlone(t *testing.T) {
	srv := opencodetest.New(fixturePassword)
	defer srv.Close()
	c, tokFile := pairedCredentials(t, srv)
	orig := mustRead(t, tokFile)
	srv.Rotate("a-rotated-fixture-password")
	later := time.Now().Add(25 * 24 * time.Hour)
	if renewed, err := c.Renew(context.Background(), later); renewed || !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("a revoked token: %v %v", renewed, err)
	}
	if !bytes.Equal(mustRead(t, tokFile), orig) {
		t.Fatal("a failed renewal changed the token file")
	}
	before := len(srv.Requests())
	if renewed, err := c.Renew(context.Background(), c.Expires().Add(time.Hour)); renewed || err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("an expired token: %v %v", renewed, err)
	}
	if len(srv.Requests()) != before {
		t.Fatal("an expired token was sent")
	}
	env, err := EnvCredentials(srv.URL, srv.Issue())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Renew(context.Background(), later); !errors.Is(err, ErrNotRenewable) {
		t.Fatalf("an environment token: %v", err)
	}
}

// TestKeepRenewedLogsByFingerprintOnly: the renewal's log names the old and
// new tokens by fingerprint, and a failure's by fingerprint — never the
// token, nor the server password.
func TestKeepRenewedLogsByFingerprintOnly(t *testing.T) {
	srv := opencodetest.New(fixturePassword)
	defer srv.Close()
	c, _ := pairedCredentials(t, srv)
	old := c.Token()
	later := time.Now().Add(25 * 24 * time.Hour)
	srv.SetNow(func() time.Time { return later })
	var buf syncBuf
	logger := log.New(&buf, "", 0)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { KeepRenewed(ctx, c, logger, time.Hour, func() time.Time { return later }); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for c.Token() == old && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	out := buf.String()
	if c.Token() == old || !strings.Contains(out, "renewed the opencode token: "+Fingerprint(old)+" → "+Fingerprint(c.Token())) {
		t.Fatalf("log %q", out)
	}
	for _, secret := range []string{old, c.Token(), fixturePassword, strings.SplitN(c.Token(), ".", 2)[1]} {
		if strings.Contains(out, secret) {
			t.Fatalf("the log carries a secret: %q", out)
		}
	}
	// A failure is logged by fingerprint too.
	srv.Rotate("another-rotated-fixture-password")
	buf.Reset()
	later2 := later.Add(25 * 24 * time.Hour)
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() { KeepRenewed(ctx2, c, logger, time.Hour, func() time.Time { return later2 }); close(done2) }()
	for !strings.Contains(buf.String(), "retrying") && time.Now().Before(deadline.Add(5*time.Second)) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel2()
	<-done2
	if out := buf.String(); !strings.Contains(out, Fingerprint(c.Token())) || strings.Contains(out, c.Token()) {
		t.Fatalf("failure log %q", out)
	}
}

// syncBuf is a bytes.Buffer safe to read while a logger writes it.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }
func (s *syncBuf) Reset()                      { s.mu.Lock(); defer s.mu.Unlock(); s.b.Reset() }
