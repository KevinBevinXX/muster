package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runWith(t *testing.T, env map[string]string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	rc := run(args, func(k string) string { return env[k] }, &out, &errb)
	return rc, out.String(), errb.String()
}

// TestTheTokenNeverHasToBeAnArgument: there is no -token flag (argv is visible
// to every process on the host); the token comes from a file or the environment.
func TestTheTokenNeverHasToBeAnArgument(t *testing.T) {
	if rc, _, errs := runWith(t, nil, "-token", "mmt_x"); rc != 2 || !strings.Contains(errs, "flag provided but not defined: -token") {
		t.Fatalf("-token was accepted (rc %d): %s", rc, errs)
	}
}

// TestMissingURLOrTokenIsAUsageError, and a non-mirror token or a cleartext URL
// is refused before anything dials.
func TestMissingURLOrTokenIsAUsageError(t *testing.T) {
	if rc, _, errs := runWith(t, map[string]string{envURL: "https://m.example"}); rc != 2 || !strings.Contains(errs, "are required") {
		t.Fatalf("no token: rc %d %s", rc, errs)
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "token")
	if err := os.WriteFile(f, []byte("hook-token-not-a-mirror-one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if rc, _, errs := runWith(t, map[string]string{envURL: "https://m.example", envHost: "laptop"}, "-token-file", f); rc != 2 || !strings.Contains(errs, "not a mirror token") {
		t.Fatalf("hook token in the file: rc %d %s", rc, errs)
	}
	if rc, _, errs := runWith(t, map[string]string{envURL: "http://192.0.2.1:30306", envToken: "mmt_x", envHost: "laptop"}); rc != 2 || !strings.Contains(errs, "cleartext") {
		t.Fatalf("http without -allow-http: rc %d %s", rc, errs)
	}
}

func TestVersion(t *testing.T) {
	if rc, out, _ := runWith(t, nil, "-version"); rc != 0 || !strings.Contains(out, "tunnel protocol 1") {
		t.Fatalf("-version: rc %d %q", rc, out)
	}
}
