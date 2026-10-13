package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirrorhost"
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

// listBackend is a host's tmux for -list: fixed sessions and active windows.
type listBackend struct{}

func (listBackend) List(context.Context) ([]string, error) {
	return []string{"0", "infra-work", "scratch13", "scratch2"}, nil
}
func (listBackend) Attach(context.Context, string, bool, uint16, uint16) (mirrorhost.Terminal, error) {
	return nil, errors.New("not in -list")
}
func (listBackend) ActiveWindows(context.Context) (map[string]string, error) {
	return map[string]string{"scratch13": "1", "scratch2": "8", "infra-work": "2", "0": "0"}, nil
}

func withListBackend(t *testing.T) {
	t.Helper()
	prev := e2eBackend
	e2eBackend = func() mirrorhost.Backend { return listBackend{} }
	t.Cleanup(func() { e2eBackend = prev })
}

// TestListShowsTheLabelAsALastColumn: -list prints what would be registered,
// and a labelled session's line ends in `label=<codename>:<window>`; the
// operator's own examples. Sessions with no codename print as before.
func TestListShowsTheLabelAsALastColumn(t *testing.T) {
	withListBackend(t)
	f := filepath.Join(t.TempDir(), "labels.json")
	if err := os.WriteFile(f, []byte(`{"scratch13":"mango","scratch2":"Gold"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	rc, out, errs := runWith(t, map[string]string{envLabelsFile: f}, "-list")
	if rc != 0 {
		t.Fatalf("-list: rc %d: %s", rc, errs)
	}
	if want := "0\ninfra-work\nscratch13\tlabel=mango:1\nscratch2\tlabel=Gold:8\n"; out != want {
		t.Fatalf("-list printed %q, want %q", out, want)
	}
}

// TestABadLabelsFileIsNeverFatal: a missing or malformed labels file is logged
// and the host runs exactly as with none — -list still succeeds, unlabelled.
func TestABadLabelsFileIsNeverFatal(t *testing.T) {
	withListBackend(t)
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte(`["not","an","object"]`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{filepath.Join(t.TempDir(), "absent.json"), bad} {
		rc, out, errs := runWith(t, nil, "-list", "-labels-file", f)
		if rc != 0 || out != "0\ninfra-work\nscratch13\nscratch2\n" {
			t.Errorf("%s: rc %d, out %q, want 0 and the raw list", f, rc, out)
		}
		if strings.Count(errs, "no session labels") != 1 {
			t.Errorf("%s: want the failure logged exactly once, got: %s", f, errs)
		}
	}
}

func TestVersion(t *testing.T) {
	if rc, out, _ := runWith(t, nil, "-version"); rc != 0 || !strings.Contains(out, "tunnel protocol 2") {
		t.Fatalf("-version: rc %d %q", rc, out)
	}
}
