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
// MUSTER_MIRROR_TOKEN_FILE …), which a Claude Code hook does not inherit. So:
//
//   - the TOKEN FILE has one default for both: MUSTER_MIRROR_TOKEN_FILE, else
//     DefaultTokenFile (~/.muster/mirror-token) — the path the host's service
//     already uses;
//   - the URL is RECORDED by the daemon when it starts (hookConfig, written to
//     hookConfigPath, 0600, best-effort) and read by the hook when neither -url
//     nor MUSTER_MIRROR_URL is given. What the hook uses is therefore exactly
//     what the running daemon resolved, not a second guess at it.
//
// A flag or environment variable always wins over the recorded file.

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

// tokenFileOr is the token file a command reads: the flag/env value given, else
// DefaultTokenFile — unless the token itself is in MUSTER_MIRROR_TOKEN.
func tokenFileOr(given string, getenv func(string) string) string {
	if given != "" {
		return given
	}
	if strings.TrimSpace(getenv(envToken)) != "" {
		return ""
	}
	return DefaultTokenFile
}

// writeHookConfig records c for the hook (best-effort: an error is the
// caller's to log, never fatal).
func writeHookConfig(path string, c hookConfig) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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
