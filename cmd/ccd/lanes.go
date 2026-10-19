package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// LANES: ONE CLAUDE CODE CLI PER EXPLICIT MUSTER SESSION.
//
// ccd has always run ONE interactive CLI (the "primary", tmux session `cc`), and
// every muster chat session landed in it. A muster session started on the
// claude-code runtime with a model of its own (muster's chat_sessions.runtime,
// migration 0030) needs a conversation and a model of its own, so ccd runs it in
// a CLI of its own — a LANE:
//
//   - a request carrying X-Muster-CC-Session (the muster session key) is that
//     lane's turn; one WITHOUT it is the primary's, exactly as before, so every
//     session muster had before 0030 keeps landing in the one conversation it
//     always had;
//   - each lane is a tmux session `cc-<laneID(key)>` on ccd's socket, running
//     `env CCD_LANE=<id> claude [--model <m>] --session-id <laneUUID(key)>` the
//     first time and `… --resume <laneUUID(key)>` once its transcript exists —
//     the same resume-never-fork rule the primary keeps (supervise.go);
//   - each lane is its own *server (busy, the pending turn, the turn slot, the
//     session hooks) and its own *supervisor (restarts, crash loop), so two
//     lanes run turns at the same time and one lane's turn never answers
//     another's;
//   - a lane's hooks say which lane they are from: the CLI's environment holds
//     CCD_LANE, `ccd hook` sends it as ?lane=, and the hook listener hands the
//     event to that lane. A hook with no lane is the primary's.
//
// 🔴 THE PRIMARY RESUMES ITS OWN CONVERSATION BY ID, NOT "THE NEWEST". Lanes
// write their transcripts in the same project directory (same workspace), so
// `claude --continue` — "the most recent conversation in this directory" — could
// resume a LANE's conversation in the primary. So the primary's session id is
// recorded when its SessionStart arrives (primaryRecordFile) and every later
// primary start is `--resume <that id>`; and no lane starts until the primary
// has started in this process, so on the first start after an upgrade (no
// record yet) the only transcripts `--continue` can see are the primary's own.
//
// 🔴 MEMORY IS WHY THERE IS A CAP. A Claude Code CLI is a node process: measured
// at about 250MiB (median) to 550MiB (largest) on workstation CLIs, against the
// pod's 3Gi limit. At most maxLanes lanes run at once (CCD_MAX_SESSIONS, default
// 3). A lane needed beyond that stops the least recently used IDLE lane (its
// conversation is on disk, and its next turn resumes it); when every lane is
// mid-turn the new one is refused (503 capacity), never queued.
//
// ⚠ WHAT A LANE DOES NOT HAVE, STATED: the "awaiting approval" badge reports
// the primary's permission prompts only (blocked.go), and /healthz's ok is the
// primary's alone — a lane that crash-loops is dropped and restarted by its next
// turn, it does not fail the pod.

// The request headers a lane turn carries. A WIRE CONTRACT with muster's
// internal/agents (turnroute.go), which this binary does not import;
// TestTheLaneHeadersAreMustersHeaders pins the two together.
const (
	headerCCSession = "X-Muster-CC-Session"
	headerCCModel   = "X-Muster-CC-Model"
)

// laneModelPattern is what a lane's --model may be: an alias ("opus") or a model
// id, optionally with the CLI's "[1m]" suffix. It is argv, so it is a closed set.
// The same set as muster's (internal/agents/session_runtime.go), pinned by test.
var laneModelPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,79}(\[1m\])?$`)

// defaultMaxLanes is CCD_MAX_SESSIONS's default (see the memory note above).
const defaultMaxLanes = 3

// laneStartTimeout bounds how long a lane's first turn waits for its new CLI's
// SessionStart. The CLI is starting because of THIS turn, so a not_ready on the
// first poll would refuse every first turn to a new session.
const laneStartTimeout = 60 * time.Second

// laneID is the lane's short name: tmux session `cc-<it>`, CCD_LANE, ?lane=.
// Hex, so it is safe in all three places whatever the session key holds.
func laneID(key string) string {
	sum := sha256.Sum256([]byte("ccd-lane:" + key))
	return hex.EncodeToString(sum[:6])
}

// isLaneID reports whether s has laneID's shape.
func isLaneID(s string) bool {
	if len(s) != 12 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// laneUUIDMarker begins every lane's session id, so a transcript file can be
// told to be a lane's by its name alone.
const laneUUIDMarker = "ccd11a9e"

// laneUUID is the lane's Claude Code session id: deterministic from the key, so
// a lane started again (after an eviction, a crash, a pod restart) resumes the
// same conversation. Shaped as a version-4, variant-1 UUID, because the pinned
// CLI checks --session-id against UUID patterns, some of which require that
// version and variant; the first 32 bits are laneUUIDMarker.
func laneUUID(key string) string {
	sum := sha256.Sum256([]byte("ccd-lane-uuid:" + key))
	b := sum[:16]
	copy(b[:4], []byte{0xcc, 0xd1, 0x1a, 0x9e})
	b[6] = 0x40 | (b[6] & 0x0f)
	b[8] = 0x80 | (b[8] & 0x3f)
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// projectDir is the CLI's transcript directory for workspace (supervise.go's
// encoding, nonAlnum).
func projectDir(configDir, workspace string) string {
	return filepath.Join(configDir, "projects", nonAlnum.ReplaceAllString(workspace, "-"))
}

func transcriptExists(configDir, workspace, sessionID string) bool {
	fi, err := os.Stat(filepath.Join(projectDir(configDir, workspace), sessionID+".jsonl"))
	return err == nil && fi.Mode().IsRegular()
}

// primaryRecordFile holds the primary CLI's session id (see the type comment).
// On the volume, beside the CLI's own config, so it survives the pod.
const primaryRecordFile = "ccd-primary-session"

// sessionIDPattern is what a recorded id must look like before it becomes argv.
var sessionIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// recordPrimary writes the primary's session id, replacing the file atomically.
// A lane's id is never recorded (the caller is the primary's hook path; the
// marker check is the second line of defence).
func recordPrimary(configDir, sessionID string) error {
	if !sessionIDPattern.MatchString(sessionID) || strings.HasPrefix(sessionID, laneUUIDMarker) {
		return fmt.Errorf("not recording %q as the primary session id", sessionID)
	}
	path := filepath.Join(configDir, primaryRecordFile)
	if b, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(b)) == sessionID {
		return nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(sessionID+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// recordedPrimary is the recorded primary session id when its transcript still
// exists, else "".
func recordedPrimary(configDir, workspace string) string {
	b, err := os.ReadFile(filepath.Join(configDir, primaryRecordFile))
	if err != nil {
		return ""
	}
	id := strings.TrimSpace(string(b))
	if !sessionIDPattern.MatchString(id) || strings.HasPrefix(id, laneUUIDMarker) || !transcriptExists(configDir, workspace, id) {
		return ""
	}
	return id
}

// newestPrimaryTranscript returns the id of the newest transcript in the
// workspace's project directory that is not a lane's, and whether any lane
// transcript is there at all.
func newestPrimaryTranscript(configDir, workspace string) (newest string, lanesSeen bool) {
	entries, err := os.ReadDir(projectDir(configDir, workspace))
	if err != nil {
		return "", false
	}
	var at time.Time
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".jsonl")
		if !ok || !e.Type().IsRegular() {
			continue
		}
		if strings.HasPrefix(name, laneUUIDMarker) {
			lanesSeen = true
			continue
		}
		if fi, err := e.Info(); err == nil && sessionIDPattern.MatchString(name) && (newest == "" || fi.ModTime().After(at)) {
			newest, at = name, fi.ModTime()
		}
	}
	return newest, lanesSeen
}

// laneTerminal is what a lane needs of tmux beyond what its supervisor does:
// ending its session when the lane is stopped.
type laneTerminal interface {
	cliPane
	terminal
	KillSession(ctx context.Context) error
}

// lane is one explicit session's CLI.
type lane struct {
	id, key, model string
	srv            *server
	sup            *supervisor
	term           laneTerminal
	cancel         context.CancelFunc
	done           chan struct{} // closed when the supervisor's run returns
	lastUsed       time.Time
}

// lanes is the set of running lanes.
type lanes struct {
	// newTerm builds the terminal for a lane's tmux session.
	newTerm func(target string) laneTerminal
	// newServer builds a lane's server (the primary's configuration).
	newServer                 func(term terminal) *server
	bin, configDir, workspace string
	max                       int
	startTimeout              time.Duration
	// primaryStarted reports whether the primary has started in this process
	// (see the type comment on why no lane may start before it).
	primaryStarted func() bool
	now            func() time.Time
	// tweak, when set, adjusts a new lane's supervisor (tests shorten its poll).
	tweak func(*supervisor)

	mu  sync.Mutex
	set map[string]*lane // by laneID
}

// errLanesFull: every lane is mid-turn and the cap is reached.
var errLanesFull = errors.New("every Claude Code session ccd may run at once is mid-turn")

// get returns the running lane for id, if any (the hook path).
func (l *lanes) get(id string) *lane {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.set[id]
}

// acquire returns key's lane, starting it (and stopping an idle one when at the
// cap) if it is not running. model must match a running lane's.
func (l *lanes) acquire(ctx context.Context, key, model string) (*lane, *failure) {
	if l.primaryStarted != nil && !l.primaryStarted() {
		return nil, &failure{Status: http.StatusServiceUnavailable, Type: failNotReady,
			Message: "the original Claude Code session has not started yet; no other session starts before it (lanes.go)"}
	}
	id := laneID(key)
	l.mu.Lock()
	if ln, ok := l.set[id]; ok {
		l.mu.Unlock()
		if ln.model != model {
			return nil, &failure{Status: http.StatusBadRequest, Type: failBadRequest,
				Message: fmt.Sprintf("session %q runs on model %q, not %q: a session's model is fixed", key, ln.model, model)}
		}
		return ln, nil
	}
	var evict *lane
	if len(l.set) >= l.max {
		for _, ln := range l.set {
			if ln.srv.idle() && (evict == nil || ln.lastUsed.Before(evict.lastUsed)) {
				evict = ln
			}
		}
		if evict == nil {
			l.mu.Unlock()
			return nil, &failure{Status: http.StatusServiceUnavailable, Type: failCapacity,
				Message: fmt.Sprintf("%v (%d; CCD_MAX_SESSIONS) — retry when one finishes", errLanesFull, l.max)}
		}
		delete(l.set, evict.id)
	}
	ln := l.newLane(id, key, model)
	// Detached from the turn's request: the CLI outlives the turn that started it.
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	ln.cancel = cancel
	l.set[id] = ln
	l.mu.Unlock()
	if evict != nil {
		log.Printf("ccd: stopping idle session %s (%s) to start %s: at most %d run at once", evict.id, evict.key, id, l.max)
		evict.stop()
	}
	go ln.run(runCtx, l)
	return ln, nil
}

func (l *lanes) newLane(id, key, model string) *lane {
	term := l.newTerm("cc-" + id)
	sup := newSupervisor(term, l.bin, l.configDir, l.workspace)
	if l.tweak != nil {
		l.tweak(sup)
	}
	uuid := laneUUID(key)
	sup.argvFn = func() ([]string, string) {
		argv := []string{"env", "CCD_LANE=" + id, l.bin}
		if model != "" {
			argv = append(argv, "--model", model)
		}
		if transcriptExists(l.configDir, l.workspace, uuid) {
			return append(argv, "--resume", uuid), modeContinue
		}
		return append(argv, "--session-id", uuid), modeFresh
	}
	srv := l.newServer(term)
	srv.sup, srv.inputPane = sup, sup.inputPane
	return &lane{id: id, key: key, model: model, srv: srv, sup: sup, term: term,
		done: make(chan struct{}), lastUsed: l.now()}
}

// run supervises the lane's CLI until it is stopped, crash-loops or loses its
// terminal; then the lane is dropped (its next turn starts it again).
func (ln *lane) run(ctx context.Context, l *lanes) {
	defer close(ln.done)
	err := ln.sup.run(ctx)
	if ctx.Err() == nil {
		st := ln.sup.state()
		log.Printf("ccd: session %s (%s) ended: %s %s (err=%v); its next turn starts it again", ln.id, ln.key, st.CLI, st.Detail, err)
		l.mu.Lock()
		if l.set[ln.id] == ln {
			delete(l.set, ln.id)
		}
		l.mu.Unlock()
		kctx, kcancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = ln.term.KillSession(kctx)
		kcancel()
	}
}

// stop ends the lane's CLI and its tmux session.
func (ln *lane) stop() {
	if ln.cancel != nil {
		ln.cancel()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = ln.term.KillSession(ctx)
}

// touch marks the lane used now.
func (l *lanes) touch(ln *lane) {
	l.mu.Lock()
	ln.lastUsed = l.now()
	l.mu.Unlock()
}

// waitStarted waits until the lane's CLI has sent SessionStart, its supervisor
// gave up, or d passes.
func (ln *lane) waitStarted(ctx context.Context, d time.Duration) *failure {
	deadline := time.NewTimer(d)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		if ln.srv.started() {
			return nil
		}
		select {
		case <-ln.done:
			st := ln.sup.state()
			return &failure{Status: http.StatusServiceUnavailable, Type: failNotReady,
				Message: fmt.Sprintf("the session's Claude Code CLI did not stay up (%s: %s)", st.CLI, st.Detail)}
		case <-deadline.C:
			return &failure{Status: http.StatusServiceUnavailable, Type: failNotReady,
				Message: fmt.Sprintf("the session's Claude Code CLI did not reach its prompt within %s (no SessionStart)", d)}
		case <-ctx.Done():
			return &failure{Status: http.StatusGatewayTimeout, Type: failTurnTimeout, Message: "caller went away while the session's CLI started"}
		case <-tick.C:
		}
	}
}

// laneHealth is one lane on /healthz.
type laneHealth struct {
	ID      string `json:"id"`
	Model   string `json:"model,omitempty"`
	CLI     string `json:"cli"`
	Session string `json:"session"`
	Busy    bool   `json:"busy"`
}

func (l *lanes) health() []laneHealth {
	l.mu.Lock()
	list := make([]*lane, 0, len(l.set))
	for _, ln := range l.set {
		list = append(list, ln)
	}
	l.mu.Unlock()
	out := make([]laneHealth, 0, len(list))
	for _, ln := range list {
		started, ended, busy := ln.srv.snapshot()
		out = append(out, laneHealth{ID: ln.id, Model: ln.model, CLI: ln.sup.state().CLI,
			Session: ln.srv.sessionState(started, ended), Busy: busy})
	}
	return out
}
