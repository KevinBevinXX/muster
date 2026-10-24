package opencodehost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// PAIRING (`muster-mirror opencode-pair`): the step that mints this host's
// first token. It is the ONE place muster-mirror reads the opencode server's
// password, and only when it is run.
//
// WHERE THE PASSWORD IS (upstream at 6ec5682, read in source, never by
// opening the file here): the background service advertises itself in a
// REGISTRATION FILE, {id, version, url, pid, password}, mode 0600 —
// packages/client/src/effect/service.ts ("That file is the complete discovery
// contract"). Its path is <state>/opencode/<name>, where <state> is
// $XDG_STATE_HOME, else ~/.local/state (packages/util/src/global-roots.ts),
// and <name> is "service-<channel>.json" — "service.json" for the latest,
// dev, beta and next channels (packages/cli/src/services/service-config.ts,
// filename()). A Nix-built opencode is channel "prod": service-prod.json.
//
// What Pair does: read that file's url and password; refuse a url that is not
// loopback; POST /api/pair with the password; redeem the code at
// GET /auth/connect/{code}; write {url, token} to the token file, 0600. The
// password is used for that one request and kept nowhere.

// RegistrationNames are the registration file's names Pair looks for, in
// order: the Nix build's channel, then the default channels'.
var RegistrationNames = []string{"service-prod.json", "service.json"}

// DefaultRegistrationPaths is where the registration file can be: each of
// RegistrationNames under $XDG_STATE_HOME/opencode, else
// ~/.local/state/opencode. nil when neither base is known.
func DefaultRegistrationPaths(getenv func(string) string) []string {
	base := getenv("XDG_STATE_HOME")
	if base == "" {
		home := getenv("HOME")
		if home == "" {
			return nil
		}
		base = filepath.Join(home, ".local", "state")
	}
	out := make([]string, 0, len(RegistrationNames))
	for _, n := range RegistrationNames {
		out = append(out, filepath.Join(base, "opencode", n))
	}
	return out
}

// registration is the part of the registration file Pair reads.
type registration struct {
	URL      string `json:"url"`
	Password string `json:"password"`
}

// readRegistration reads the registration file at path: a regular file,
// never a symlink, at most 64 KiB, with a url and a password.
func readRegistration(path string) (registration, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return registration{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return registration{}, err
	}
	if !fi.Mode().IsRegular() {
		return registration{}, fmt.Errorf("%s is not a regular file", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return registration{}, err
	}
	var r registration
	if json.Unmarshal(b, &r) != nil {
		return registration{}, fmt.Errorf("%s is not an opencode service registration", path)
	}
	if r.URL == "" {
		return registration{}, fmt.Errorf("%s names no server url", path)
	}
	if r.Password == "" {
		return registration{}, fmt.Errorf("%s holds no server password (a service with no password needs no token)", path)
	}
	return r, nil
}

// PairResult is what Pair did, for a person: nothing in it is a secret.
type PairResult struct {
	Registration string
	URL          string
	TokenFile    string
	Fingerprint  string
	Expires      time.Time
	// Replaced is the fingerprint of the token the file held before, "" for
	// none.
	Replaced string
}

// Pair mints a token from the server named by the first of registrations
// that exists and writes it to tokenFile (WriteTokenFile).
func Pair(ctx context.Context, registrations []string, tokenFile string) (PairResult, error) {
	if tokenFile == "" {
		return PairResult{}, errors.New("no token file to write (set -token-file)")
	}
	var (
		reg  registration
		path string
		err  error
	)
	for _, p := range registrations {
		if reg, err = readRegistration(p); err == nil {
			path = p
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return PairResult{}, err
		}
	}
	if path == "" {
		return PairResult{}, fmt.Errorf("no opencode service registration file at %q: is the opencode background service running (opencode service status)?", registrations)
	}
	password := reg.Password
	// NewClient refuses a url that is not loopback, before any request.
	cl, err := NewClient(reg.URL, func() string { return password })
	if err != nil {
		return PairResult{}, fmt.Errorf("%s: %w", path, err)
	}
	tok, err := cl.mint(ctx)
	if errors.Is(err, ErrUnauthorized) {
		return PairResult{}, fmt.Errorf("the opencode server at %s refused the password in %s (was the service restarted with a new one?)", reg.URL, path)
	}
	if err != nil {
		return PairResult{}, err
	}
	res := PairResult{Registration: path, URL: reg.URL, TokenFile: tokenFile, Fingerprint: Fingerprint(tok)}
	res.Expires, _ = Expiry(tok)
	if old, err := ReadTokenFile(tokenFile); err == nil {
		res.Replaced = Fingerprint(old.Token)
	}
	if err := WriteTokenFile(tokenFile, TokenFile{URL: reg.URL, Token: tok}); err != nil {
		return PairResult{}, fmt.Errorf("write %s: %w", tokenFile, err)
	}
	return res, nil
}
