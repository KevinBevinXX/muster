package mirrorhost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/ZacxDev/muster/internal/mirror"
)

// PRETTY NAMES. A host may give some of its sessions a CODENAME (a JSON file,
// muster-mirror's -labels-file / MUSTER_MIRROR_LABELS_FILE: {"scratch13":
// "mango"}), and muster then shows such a session as "<codename>:<active window
// index>" — "mango:1" — instead of its raw tmux name. Display only: the session
// is still addressed by its name (mirror.Msg.Labels).

// LoadCodenames reads a labels file: one JSON object, session name → codename.
// An entry whose key is not a valid session name or whose codename is not a
// valid label (mirror.ValidSession / mirror.ValidLabel) is DROPPED and named in
// the returned skipped list; only an unreadable file or one that is not a JSON
// object of strings is an error. The caller decides what an error costs —
// muster-mirror logs it once and runs without labels, never exits.
func LoadCodenames(path string) (codenames map[string]string, skipped []string, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read the labels file: %w", err)
	}
	var raw map[string]string
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, nil, fmt.Errorf("the labels file %s is not a JSON object of strings: %w", path, err)
	}
	if raw == nil {
		// `null` decodes to a nil map without error; it is not an object.
		return nil, nil, fmt.Errorf("the labels file %s is not a JSON object of strings", path)
	}
	codenames = make(map[string]string, len(raw))
	for s, name := range raw {
		if mirror.ValidSession(s) != nil || mirror.ValidLabel(name) != nil {
			skipped = append(skipped, s)
			continue
		}
		codenames[s] = name
	}
	return codenames, skipped, nil
}

// WindowIndexer is a Backend that can say which window each session has
// ACTIVE. Tmux is one; a backend that is not labels a session by its codename
// alone.
type WindowIndexer interface {
	// ActiveWindows is session name → its active window's index ("1").
	ActiveWindows(ctx context.Context) (map[string]string, error)
}

// WindowIndexerOf is b's WindowIndexer, looking through wrappers (see ClaudeOf).
func WindowIndexerOf(b Backend) (WindowIndexer, bool) { return capabilityOf[WindowIndexer](b) }

// ActiveWindows implements WindowIndexer with one `tmux list-sessions`.
//
// In list-sessions' format context #{window_index} is the session's CURRENT
// window — measured on tmux 3.7c: a session with windows 0-2 and window 1
// selected lists 1, and after `select-window` to 2 lists 2.
func (t Tmux) ActiveWindows(ctx context.Context) (map[string]string, error) {
	cmd := exec.CommandContext(ctx, t.bin(), t.query("list-sessions", "-F", "#{window_index}\t#{session_name}")...)
	cmd.Env = tmuxEnv()
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("tmux list-sessions: %v: %s", err, strings.TrimSpace(errb.String()))
	}
	return parseActiveWindows(out.String()), nil
}

// parseActiveWindows reads ActiveWindows' output. The session name is the LAST
// field, so it is taken whole whatever it holds; a line whose index is not a
// plain decimal number is skipped.
func parseActiveWindows(out string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		idx, name, ok := strings.Cut(strings.TrimRight(line, "\r"), "\t")
		if !ok || name == "" || idx == "" || strings.Trim(idx, "0123456789") != "" {
			continue
		}
		m[name] = idx
	}
	return m
}

// BuildLabels is the display label of each of sessions that has a codename:
// "<codename>:<active window index>", or the codename alone when the index is
// unknown. A session without a codename gets no label (muster shows its name);
// so does one whose label would not be valid (mirror.NormalizeLabels). nil
// when nothing is labelled.
func BuildLabels(codenames, windows map[string]string, sessions []string) map[string]string {
	if len(codenames) == 0 {
		return nil
	}
	labels := map[string]string{}
	for _, s := range sessions {
		name, ok := codenames[s]
		if !ok {
			continue
		}
		if idx, ok := windows[s]; ok {
			name += ":" + idx
		}
		labels[s] = name
	}
	return mirror.NormalizeLabels(labels, sessions)
}

// SessionLabels is BuildLabels over b's live active windows. With no codenames
// it asks the backend nothing. A failed window read is not fatal: the labels
// are then the codenames alone, and the error is returned for the caller to
// log.
func SessionLabels(ctx context.Context, b Backend, codenames map[string]string, sessions []string) (map[string]string, error) {
	if len(codenames) == 0 {
		return nil, nil
	}
	var windows map[string]string
	var err error
	if wi, ok := WindowIndexerOf(b); ok {
		windows, err = wi.ActiveWindows(ctx)
	}
	return BuildLabels(codenames, windows, sessions), err
}
