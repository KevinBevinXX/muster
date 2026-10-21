// Package hermeshost is the host half of Hermes mirroring (docs/design/hermes.md,
// Phase 2): what muster-mirror reads from a Hermes home — the terminal
// breadcrumbs, the SQLite session store, the shell-hook allowlist — and the
// status files its own `muster-mirror hermes-hook` writes.
//
// 🔴 IT NEVER WRITES HERMES DATA, WITH ONE EXCEPTION THAT IS NOT THE DAEMON:
// the shell-hook allowlist writer (allowlist.go), which only the host's Home
// Manager activation runs (`muster-mirror hermes-allowlist`), never the
// mirroring daemon. ⚠ "Never writes data" is not "creates no file": SQLite's
// read-only connection to a WAL database creates state.db-wal and
// state.db-shm when they do not exist (a WAL reader needs the shared-memory
// index), as any Hermes reader does. It writes no row.
//
// 🔴 AND ONLY THESE FILES. state.db holds whole conversations; the same home
// holds auth.json, .env, config.yaml (which can carry inline keys), pastes/
// (Hermes's on-disk copies of large pastes, muster's messages included) and
// more. Every path under a Hermes home is built HERE, from Home.dir, which no
// other file of this package may read (TestOnlyHomeGoReadsTheHermesHome), and
// TestTheHermesHomeLedger watches, with inotify, every file under the home
// that the exercised read paths open (the locator, the transcript reader,
// the consent check — listed in its exerciseReadPath) and fails when one is
// outside ReadPaths. A reader added without an exercise there is not watched.
//
// 🔴 NO READ FOLLOWS A SYMLINK OR WAITS ON A FIFO: every file is opened with
// O_NOFOLLOW|O_NONBLOCK|O_NOCTTY and must be a regular file (openRegular), so
// a planted `terminal-sessions/tty-…` → `../auth.json` is refused before a
// byte is read, and a FIFO cannot hang the listing. The one symlink allowed
// is the allowlist's into the Nix store (a Home Manager file). And no error
// ever quotes a file's content.
package hermeshost

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// Home is one Hermes home ($HERMES_HOME, else ~/.hermes).
type Home struct {
	// dir is the home's directory. 🔴 ONLY home.go READS IT: every path under
	// the home is one of the allowlisted ones built below.
	dir string
}

// NewHome is the Hermes home at dir ("" is no home: every read fails).
func NewHome(dir string) Home { return Home{dir: filepath.Clean(dir)} }

// DefaultHomeDir is the Hermes home a Hermes CLI started with getenv's
// environment uses: $HERMES_HOME, else ~/.hermes. "" when neither can be found.
func DefaultHomeDir(getenv func(string) string) string {
	if d := getenv("HERMES_HOME"); d != "" {
		return d
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(h, ".hermes")
}

// String is the home's directory, for a log line or a diagnostic.
func (h Home) String() string { return h.dir }

// valid: the home names a directory at all.
func (h Home) valid() error {
	if h.dir == "" || h.dir == "." {
		return errors.New("no Hermes home configured")
	}
	return nil
}

// THE ALLOWLIST. ReadPaths is every path under a Hermes home that
// muster-mirror's read path opens, as globs relative to the home (SQLite opens
// state.db's -wal and -shm itself, as part of opening state.db).
//
// 🔴 NEVER auth.json, .env, config.yaml, profiles/*/.env, memories/, skills/,
// logs/, telemetry/, pastes/, or runtime/active_sessions.json (not
// authoritative: S3). A new entry here is a design change: it is reviewed
// against docs/design/hermes.md's "Secret hygiene", and the ledger test that
// holds this list must change with it.
var ReadPaths = []string{
	"state.db",
	"state.db-wal",
	"state.db-shm",
	"terminal-sessions/tty-*",
	"shell-hooks-allowlist.json",
}

// WritePaths is every path under a Hermes home the allowlist WRITER (not the
// daemon) opens to write: Hermes's own lock file, a temp file, and the
// allowlist it renames into place.
var WritePaths = []string{
	"shell-hooks-allowlist.json.lock",
	".shell-hooks-allowlist.json.*.tmp",
	"shell-hooks-allowlist.json",
}

const (
	stateDBName       = "state.db"
	breadcrumbDir     = "terminal-sessions"
	allowlistName     = "shell-hooks-allowlist.json"
	allowlistLockName = allowlistName + ".lock"
)

// stateDBURI is state.db as a read-only SQLite URI.
//
// 🔴 mode=ro, NEVER immutable=1: immutable tells SQLite the file cannot
// change, so it ignores the WAL — and every turn Hermes writes is in the WAL
// until a checkpoint. A read-only connection still needs the -shm, which is
// why muster-mirror runs as the uid that owns the home.
func (h Home) stateDBURI() (string, error) {
	if err := h.valid(); err != nil {
		return "", err
	}
	u := url.URL{Scheme: "file", Path: filepath.Join(h.dir, stateDBName)}
	q := url.Values{}
	q.Set("mode", "ro")
	u.RawQuery = q.Encode()
	// _pragma is the driver's, applied on every new connection.
	return u.String() + "&_pragma=busy_timeout(2000)&_pragma=query_only(1)", nil
}

// stateDBPath is state.db's path, for a stat (the database being replaced).
func (h Home) stateDBPath() string { return filepath.Join(h.dir, stateDBName) }

// stateDBRegular is nil when state.db is a regular file — not a symlink
// (which could point the reader at another database), not a FIFO.
func (h Home) stateDBRegular() error {
	fi, err := os.Lstat(h.stateDBPath())
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s: %w", h.stateDBPath(), errNotRegular)
	}
	return nil
}

// breadcrumbNamePattern is a breadcrumb file name Hermes writes for a tty:
// "tty-" and the device path sanitised (BreadcrumbName).
var breadcrumbNamePattern = regexp.MustCompile(`^tty-[A-Za-z0-9._-]{1,120}$`)

// BreadcrumbName is the file name Hermes gives the breadcrumb of the terminal
// whose device is tty (/dev/pts/3 → tty-dev-pts-3): the device path stripped
// of whitespace and of leading and trailing '/', every character outside
// [A-Za-z0-9._-] replaced by '-', at most 120 characters
// (P:hermes_cli/terminal_breadcrumbs.py, _sanitize and get_terminal_id). ""
// for a tty that is not a device path.
func BreadcrumbName(tty string) string {
	if !strings.HasPrefix(tty, "/dev/") {
		return ""
	}
	s := strings.Trim(strings.TrimSpace(tty), "/")
	b := []byte(s)
	for i, c := range b {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			b[i] = '-'
		}
	}
	// Python slices code points and this slices bytes; every byte is ASCII
	// here, so the two agree.
	if len(b) > 120 {
		b = b[:120]
	}
	name := "tty-" + string(b)
	if !breadcrumbNamePattern.MatchString(name) {
		return ""
	}
	return name
}

// maxBreadcrumb bounds a breadcrumb read: Hermes writes a few hundred bytes.
const maxBreadcrumb = 16 << 10

// readBreadcrumb is the breadcrumb file for tty, read whole (bounded).
func (h Home) readBreadcrumb(tty string) ([]byte, error) {
	if err := h.valid(); err != nil {
		return nil, err
	}
	name := BreadcrumbName(tty)
	if name == "" {
		return nil, fmt.Errorf("%q is not a terminal device", tty)
	}
	return readBounded(filepath.Join(h.dir, breadcrumbDir, name), maxBreadcrumb)
}

// maxAllowlist bounds an allowlist read.
const maxAllowlist = 1 << 20

// readAllowlist is the shell-hook allowlist, read whole (bounded);
// os.ErrNotExist when there is none. storeLink: it is a Home Manager link
// into the Nix store (readStoreLinkOrRegular), which a writer replaces.
func (h Home) readAllowlist() (b []byte, storeLink bool, err error) {
	if err := h.valid(); err != nil {
		return nil, false, err
	}
	return readStoreLinkOrRegular(filepath.Join(h.dir, allowlistName), maxAllowlist)
}

// configName is Hermes's config file, which only the diagnostic opens
// (config.go).
const configName = "config.yaml"

// readConfig is config.yaml, read whole (bounded; a Home Manager link into
// the Nix store is followed, any other symlink refused); nil and no error when
// there is none. Never the daemon's: only `-list -hermes` calls it.
func (h Home) readConfig() ([]byte, error) {
	if err := h.valid(); err != nil {
		return nil, err
	}
	b, _, err := readStoreLinkOrRegular(filepath.Join(h.dir, configName), maxConfig)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

// allowlistPaths are the allowlist writer's three paths: the lock, the file,
// and the directory its temp file is made in.
func (h Home) allowlistPaths() (lock, file, dir string, err error) {
	if err := h.valid(); err != nil {
		return "", "", "", err
	}
	return filepath.Join(h.dir, allowlistLockName), filepath.Join(h.dir, allowlistName), h.dir, nil
}

// errNotRegular: a path is a symlink, a FIFO, a device or a directory.
var errNotRegular = errors.New("not a regular file (a symlink, a FIFO or a device is never read)")

// openRegular opens path for reading WITHOUT following a symlink and without
// blocking on a FIFO, and only when it is a regular file.
func openRegular(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("%s: %w", path, errNotRegular)
		}
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%s: %w", path, errNotRegular)
	}
	return f, nil
}

// readBounded reads a regular file of at most max bytes (openRegular).
func readBounded(path string, max int64) ([]byte, error) {
	f, err := openRegular(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Size() > max {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, max)
	}
	b := make([]byte, st.Size())
	n, err := f.ReadAt(b, 0)
	if err != nil && n != len(b) {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return b[:n], nil
}

// readStoreLinkOrRegular reads path when it is a regular file, or a symlink
// whose target is an absolute path in the Nix store (nixStore) that is itself
// a regular file: a Home Manager file. Any other symlink is refused.
func readStoreLinkOrRegular(path string, max int64) (b []byte, storeLink bool, err error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, false, err
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		b, err = readBounded(path, max)
		return b, false, err
	}
	target, err := os.Readlink(path)
	if err != nil {
		return nil, false, err
	}
	if !filepath.IsAbs(target) || !strings.HasPrefix(filepath.Clean(target), nixStore) {
		return nil, false, fmt.Errorf("%s is a symlink out of the Nix store: %w", path, errNotRegular)
	}
	b, err = readBounded(filepath.Clean(target), max)
	return b, true, err
}
