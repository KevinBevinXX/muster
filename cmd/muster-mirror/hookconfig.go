package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// WHERE `muster-mirror ask` FINDS MUSTER: the same token and URL the host
// daemon runs with, so the hook entry in Claude Code's settings is just
// `muster-mirror ask`.
//
// The daemon is configured by its service's environment (MUSTER_MIRROR_URL,
// MUSTER_MIRROR_TOKEN_FILE …), which a Claude Code hook does not inherit. So
// the managed service runs the daemon with MUSTER_MIRROR_WRITE_HOOK_CONFIG=1
// (or -write-hook-config), and the daemon then RECORDS its URL and token file
// (hookConfig, at hookConfigPath, 0600) when it starts. Only a daemon told to
// writes it: an ad-hoc or development daemon never repoints the hook.
//
// The hook reads the record when neither -url nor MUSTER_MIRROR_URL is given.
// Its TOKEN is then the recorded token file, else DefaultTokenFile — the hook's
// own default, and the path the managed service's MUSTER_MIRROR_TOKEN_FILE
// names (the daemon itself has no default token file). 🔴 A URL given by flag
// or environment never gets that token: it needs its own (ask.go).

// DefaultTokenFile is the mirror token's default path, for the daemon and the
// hook alike.
const DefaultTokenFile = "~/.muster/mirror-token"

const (
	envHookConfig = "MUSTER_MIRROR_HOOK_CONFIG"
	// defaultHookConfig is where the daemon records what the hook needs.
	defaultHookConfig = "~/.muster/mirror-hook.json"
)

// hookConfig is what the daemon records for `muster-mirror ask`. No secret:
// the token stays in its own file, named here.
type hookConfig struct {
	URL       string `json:"url"`
	AllowHTTP bool   `json:"allowHttp,omitempty"`
	TokenFile string `json:"tokenFile,omitempty"`
}

// hookConfigPath is where the hook config lives: MUSTER_MIRROR_HOOK_CONFIG,
// else defaultHookConfig.
func hookConfigPath(getenv func(string) string) string {
	if p := getenv(envHookConfig); p != "" {
		return expandHome(p)
	}
	return expandHome(defaultHookConfig)
}

// writeHookConfig records c for the hook (best-effort: an error is the
// caller's to log, never fatal). Through a temp file UNIQUE to this write and
// a rename, so concurrent writers never interleave: the record is always one
// whole write — the last one's.
func writeHookConfig(path string, c hookConfig) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp") // 0600
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(append(b, '\n'))
	cerr := f.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp, path)
	}
	if werr != nil {
		_ = os.Remove(tmp)
	}
	return werr
}

// readHookConfig reads what the daemon recorded.
func readHookConfig(path string) (hookConfig, error) {
	var c hookConfig
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// expandHome expands a leading ~/ (a path from an environment variable or a
// default, which no shell expanded).
func expandHome(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, rest)
		}
	}
	return p
}
