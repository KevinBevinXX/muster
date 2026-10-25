package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/opencodehost"
	"github.com/ZacxDev/muster/internal/opencodehost/opencodetest"
)

const pairFixturePassword = "fixture-server-password-9f8e7d6c5b4a"

// pairEnv is a scratch HOME and XDG_STATE_HOME with the service's
// registration file in it, for srv (synthetic values).
func pairEnv(t *testing.T, srv *opencodetest.Server, password string) (func(string) string, string) {
	t.Helper()
	dir := t.TempDir()
	reg := filepath.Join(dir, "state", "opencode", "service-prod.json")
	if err := os.MkdirAll(filepath.Dir(reg), 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"id": "svc_fixture", "version": "2.0.0-fixture", "url": srv.URL, "pid": 4242, "password": password})
	if err := os.WriteFile(reg, b, 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"HOME": filepath.Join(dir, "home"), "XDG_STATE_HOME": filepath.Join(dir, "state")}
	return func(k string) string { return env[k] }, dir
}

// TestOpenCodePairPrintsWhatItDidNeverTheTokenOrThePassword: run unattended,
// `muster-mirror opencode-pair` finds the registration file under
// $XDG_STATE_HOME, writes ~/.muster/opencode-token 0600, and prints the file
// it read, the server, the new token's FINGERPRINT and expiry, and the file it
// wrote — and neither the token (nor its signature), nor the password.
func TestOpenCodePairPrintsWhatItDidNeverTheTokenOrThePassword(t *testing.T) {
	srv := opencodetest.New(pairFixturePassword)
	defer srv.Close()
	getenv, dir := pairEnv(t, srv, pairFixturePassword)
	var out, errb bytes.Buffer
	if rc := run([]string{"opencode-pair"}, getenv, &out, &errb); rc != 0 {
		t.Fatalf("rc %d: %s", rc, errb.String())
	}
	tokFile := filepath.Join(dir, "home", ".muster", "opencode-token")
	fi, err := os.Stat(tokFile)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file %v %v", fi, err)
	}
	tf, err := opencodehost.ReadTokenFile(tokFile)
	if err != nil {
		t.Fatal(err)
	}
	all := out.String() + errb.String()
	for _, want := range []string{
		filepath.Join(dir, "state", "opencode", "service-prod.json"), srv.URL, tokFile, "mode 0600",
		"token " + opencodehost.Fingerprint(tf.Token) + ", expires ",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("the output does not say %q:\n%s", want, all)
		}
	}
	_, sig, _ := strings.Cut(tf.Token, ".")
	for _, secret := range []string{tf.Token, sig, pairFixturePassword} {
		if strings.Contains(all, secret) {
			t.Fatalf("the output carries a secret:\n%s", all)
		}
	}
	// The daemon's start-up line names it the same way.
	var logb bytes.Buffer
	if c, why := openCodeCredentials(tokFile); c == nil || why != "" {
		t.Fatalf("the daemon cannot read it: %s", why)
	}
	logger := log.New(&logb, "", 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cr, why := openCodeCredentials(tokFile)
	c := startOpenCode(ctx, cr, why, logger)
	if c == nil || !strings.Contains(logb.String(), opencodehost.Fingerprint(tf.Token)) || strings.Contains(logb.String(), sig) {
		t.Fatalf("daemon log %q", logb.String())
	}
}

// TestOpenCodePairFailsWithoutPrintingThePassword: a refused password and a
// missing registration exit 1, write no token file, and print no password.
func TestOpenCodePairFailsWithoutPrintingThePassword(t *testing.T) {
	srv := opencodetest.New(pairFixturePassword)
	defer srv.Close()
	const wrong = "a-wrong-fixture-password-1a2b3c"
	getenv, dir := pairEnv(t, srv, wrong)
	var out, errb bytes.Buffer
	if rc := run([]string{"opencode-pair"}, getenv, &out, &errb); rc != 1 {
		t.Fatalf("rc %d", rc)
	}
	all := out.String() + errb.String()
	if !strings.Contains(all, "refused the password") || strings.Contains(all, wrong) {
		t.Fatalf("output %q", all)
	}
	if _, err := os.Stat(filepath.Join(dir, "home", ".muster", "opencode-token")); err == nil {
		t.Fatal("a token file was written")
	}
	out.Reset()
	errb.Reset()
	if rc := run([]string{"opencode-pair", "-registration", filepath.Join(dir, "none.json")}, getenv, &out, &errb); rc != 1 ||
		!strings.Contains(errb.String(), "no opencode service registration file") {
		t.Fatalf("missing: rc %d %q", rc, errb.String())
	}
	if rc := run([]string{"opencode-pair", "extra"}, getenv, &out, &errb); rc != 2 {
		t.Fatalf("an extra argument: rc %d", rc)
	}
}

// TestTheDaemonSaysWhyItHasNoOpenCodeToken: no file, and a file others can
// read. (A token in the environment is not a source: it could not be renewed.)
func TestTheDaemonSaysWhyItHasNoOpenCodeToken(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{"HOME": dir}
	getenv := func(k string) string { return env[k] }
	p := openCodeTokenFileDefault(getenv)
	if p != filepath.Join(dir, ".muster", "opencode-token") {
		t.Fatalf("default %q", p)
	}
	if c, why := openCodeCredentials(p); c != nil || !strings.Contains(why, "muster-mirror opencode-pair") {
		t.Fatalf("missing: %v %q", c, why)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"url":"http://127.0.0.1:13420","token":"1893456000.c2ln"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if c, why := openCodeCredentials(p); c != nil || !strings.Contains(why, "readable by others") {
		t.Fatalf("0644: %v %q", c, why)
	}
	env[envOpenCodeTokenFile] = "/elsewhere/tok"
	if got := openCodeTokenFileDefault(getenv); got != "/elsewhere/tok" {
		t.Fatalf("env file %q", got)
	}
}
