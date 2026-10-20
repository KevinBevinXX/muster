package hermeshost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ZacxDev/muster/internal/atomicfile"

	"github.com/ZacxDev/muster/internal/mirror"
)

// THE STATUS FILES (docs/design/hermes.md, "Busy, idle and waiting"). Hermes
// has no status file or IPC for its CLI, so muster installs a shell hook,
// `muster-mirror hermes-hook` (hook.go), that writes one file per PHYSICAL
// session id — the session a turn ran in: the logical `local-…` id, or after
// a rotation the child's 32-hex id (S2) — in muster's own directory
// (DefaultStatusDir), never in the Hermes home.
//
// 🔴 A FILE HOLDS STATUS, TIME, A REQUEST'S KIND AND ID, AND WHO WROTE IT —
// NEVER A PROMPT, A COMMAND OR A TOOL'S ARGUMENTS. The hook's payload carries
// them (extra.prompt, redacted by Hermes but still the command); the file
// does not.

// StatusFile is one status file.
type StatusFile struct {
	V         int    `json:"v"`
	SessionID string `json:"session_id"`
	// Status is mirror.StatusBusy, StatusIdle or StatusWaiting.
	Status string `json:"status"`
	// Since is when the hook wrote it, Unix milliseconds.
	Since int64 `json:"since"`
	// Kind is a waiting request's kind (approval, clarify, sudo), "" otherwise;
	// RequestID its id.
	Kind      string `json:"kind"`
	RequestID string `json:"request_id"`
	// WriterPID and WriterStart are the process that ran the hook — the Hermes
	// GATEWAY, the hook's parent (S2: hooks run in the gateway) — and its
	// /proc start time: a busy file whose writer is gone is stale.
	WriterPID   int    `json:"writer_pid"`
	WriterStart string `json:"writer_start"`
}

// statusVersion is StatusFile.V.
const statusVersion = 1

// DefaultStatusDir is where the status files live unless the daemon says
// otherwise: muster's own directory, 0700.
const DefaultStatusDir = "~/.muster/hermes-status"

// The waiting kinds the hook records (all three measured, S2).
var waitingKinds = map[string]bool{"approval": true, "clarify": true, "sudo": true}

// requestIDPattern bounds a request id: Hermes mints uuid4().hex.
var requestIDPattern = regexp.MustCompile(`^[0-9A-Za-z_-]{1,64}$`)

// valid: f is a status file this reader believes the shape of.
func (f StatusFile) valid() bool {
	if f.V != statusVersion || !mirror.ValidAgentSession(f.SessionID) || f.Since <= 0 {
		return false
	}
	switch f.Status {
	case mirror.StatusBusy, mirror.StatusIdle:
		return f.Kind == "" && f.RequestID == ""
	case mirror.StatusWaiting:
		return waitingKinds[f.Kind] && (f.RequestID == "" || requestIDPattern.MatchString(f.RequestID))
	}
	return false
}

// StatusDir is a directory of status files.
type StatusDir string

// path is session id's status file. 🔴 THE ID IS VALIDATED FIRST: it becomes
// a file name, and the hook fires for every session the gateway runs.
func (d StatusDir) path(id string) (string, error) {
	if d == "" {
		return "", errors.New("no status directory")
	}
	if !mirror.ValidAgentSession(id) {
		return "", fmt.Errorf("%q is not a session id", id)
	}
	return filepath.Join(string(d), id+".json"), nil
}

// maxStatusFile bounds a status file read.
const maxStatusFile = 4 << 10

// Read is session id's status file, os.ErrNotExist when there is none, an
// error for one that is not a valid status file for id.
func (d StatusDir) Read(id string) (StatusFile, error) {
	p, err := d.path(id)
	if err != nil {
		return StatusFile{}, err
	}
	b, err := readBounded(p, maxStatusFile)
	if err != nil {
		return StatusFile{}, err
	}
	var f StatusFile
	if json.Unmarshal(b, &f) != nil {
		// Never the decoder's error: it quotes the file's bytes.
		return StatusFile{}, fmt.Errorf("%s is not JSON", p)
	}
	if !f.valid() || f.SessionID != id {
		return StatusFile{}, fmt.Errorf("%s is not a valid status file for %s", p, id)
	}
	return f, nil
}

// lockWait bounds how long a write waits for the directory's lock: the hook
// runs synchronously in Hermes's gateway (a 60 s default timeout), so it must
// never block it.
const lockWait = time.Second

// Write writes f as its session's status file — under the directory's lock,
// through a temp file and a rename, mode 0600 in a 0700 directory.
//
// 🔴 THE LATER ARRIVAL WINS, AND since ONLY GOES UP. Hermes runs a session's
// hooks synchronously, one after another, in its gateway, so the order writes
// arrive in is the order the events happened in. Two in one millisecond (an
// on_session_end then an on_session_finalize, or a resolve then a turn end)
// would otherwise tie; and a clock stepped back would make an older status
// look newer. So when the file already holds a since at or after f's, f is
// written at that since + 1 ms: deterministic, and the newest event's.
func (d StatusDir) Write(f StatusFile) error {
	f.V = statusVersion
	if !f.valid() {
		return errors.New("not a valid status")
	}
	p, err := d.path(f.SessionID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(string(d), 0o700); err != nil {
		return err
	}
	unlock, err := lockFile(filepath.Join(string(d), ".lock"), lockWait)
	if err != nil {
		return err
	}
	defer unlock()
	if old, err := d.Read(f.SessionID); err == nil && old.Since >= f.Since {
		f.Since = old.Since + 1
	}
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return atomicfile.Write(p, append(b, '\n'))
}

// lockFile takes an exclusive flock on path (created 0600), waiting at most
// wait; unlock releases it.
func lockFile(path string, wait time.Duration) (unlock func(), err error) {
	fh, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		err = unix.Flock(int(fh.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() {
				_ = unix.Flock(int(fh.Fd()), unix.LOCK_UN)
				_ = fh.Close()
			}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) || time.Now().After(deadline) {
			_ = fh.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Newest is the newest valid status file among ids (a lineage: the hook
// writes the PHYSICAL id, so a rotated conversation's status is in its
// child's file), and false when none of them has one — or when ctx ends
// first (the listing's own deadline bounds every read).
func (d StatusDir) Newest(ctx context.Context, ids []string) (StatusFile, bool) {
	var best StatusFile
	found := false
	for _, id := range ids {
		if ctx.Err() != nil {
			return StatusFile{}, false
		}
		f, err := d.Read(id)
		if err != nil {
			continue
		}
		if !found || f.Since > best.Since {
			best, found = f, true
		}
	}
	return best, found
}
