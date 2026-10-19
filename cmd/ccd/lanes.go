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
// recorded when its SessionStart arrives (primaryRecordFile) — only when the
// primary's own process fired it, never a `claude` running inside it
// (proctree.go) — and every later
// primary start is `--resume <that id>`; and no lane starts until the primary
// has started in this process, so on the first start after an upgrade (no
// record yet) the only transcripts `--continue` can see are the primary's own.
//
// 🔴 MEMORY IS WHY THERE IS A CAP. A Claude Code CLI is a node process: measured
// at about 250MiB (median) to 550MiB (largest) on workstation CLIs, against the
// pod's 3Gi limit. At most maxLanes lanes run at once (CCD_MAX_SESSIONS, default
// 3). A lane needed beyond that stops the least recently used IDLE lane that no
// turn holds (lane.reserved): its conversation is on disk, and its next turn
// resumes it. When no lane is free to stop the new one is refused (503
// capacity), never queued.
//
// A lane at a permission prompt shows on the agent's "awaiting approval" badge
// like the primary (blockedReporter), and one left there past stuckAfter may be
// stopped for a new session (pickEvictLocked).
//
// ⚠ WHAT A LANE DOES NOT HAVE, STATED: /healthz's ok is the primary's alone — a
// lane that crash-loops is dropped and restarted by its next turn, it does not
// fail the pod.

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
	// reserved counts the turns holding this lane, from acquire until their
	// runTurn has returned (lanes.release). Guarded by lanes.mu.
	//
	// 🔴 A RESERVED LANE IS NEVER EVICTED, HOWEVER IDLE ITS SERVER LOOKS. From
	// acquire until runTurn takes the lane's turn slot — waitStarted included,
	// up to laneStartTimeout for a lane that is starting — the server's own
	// idle() is true: no slot held, nothing pending, nothing submitted. Counting
	// that as idle let two sessions at the cap evict each other's starting CLI
	// in turn, and let a paste land just before the eviction's kill-session (a
	// 504 and an unanswered prompt). The count is read and written under
	// lanes.mu, the lock eviction chooses under, so a turn that has its lane
	// cannot lose it.
	reserved int
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
	// stuckAfter is how long a lane may sit at a permission prompt before it
	// may be stopped like an idle one (stuckLaneAfter; tests shorten it).
	stuckAfter time.Duration
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

// acquire returns key's lane, starting it (and stopping an idle, unreserved one
// when at the cap) if it is not running. model must match a running lane's. The
// lane comes back RESERVED (lane.reserved): the caller releases it once its turn
// is over.
func (l *lanes) acquire(ctx context.Context, key, model string) (*lane, *failure) {
	if l.primaryStarted != nil && !l.primaryStarted() {
		return nil, &failure{Status: http.StatusServiceUnavailable, Type: failNotReady,
			Message: "the original Claude Code session has not started yet; no other session starts before it (lanes.go)"}
	}
	id := laneID(key)
	l.mu.Lock()
	if ln, ok := l.set[id]; ok {
		if ln.model != model {
			l.mu.Unlock()
			return nil, &failure{Status: http.StatusBadRequest, Type: failBadRequest,
				Message: fmt.Sprintf("session %q runs on model %q, not %q: a session's model is fixed", key, ln.model, model)}
		}
		ln.reserved++
		l.mu.Unlock()
		return ln, nil
	}
	var evict *lane
	stuck := false
	if len(l.set) >= l.max {
		evict, stuck = l.pickEvictLocked()
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
	ln.reserved = 1
	l.set[id] = ln
	l.mu.Unlock()
	if evict != nil {
		if stuck {
			since, _ := evict.srv.awaitingSince()
			log.Printf("ccd: WARNING stopping session %s (%s), stuck at a permission prompt since %s, to start %s: "+
				"at most %d run at once; the prompt was never answered and its turn is abandoned",
				evict.id, evict.key, since.UTC().Format(time.RFC3339), id, l.max)
		} else {
			log.Printf("ccd: stopping idle session %s (%s) to start %s: at most %d run at once", evict.id, evict.key, id, l.max)
		}
		evict.stop()
	}
	go ln.run(runCtx, l)
	return ln, nil
}

// stuckLaneAfter is stuckAfter's default.
const stuckLaneAfter = 10 * time.Minute

// pickEvictLocked chooses the lane to stop for a new one: the least recently
// used IDLE lane no turn holds; failing that, the lane no turn holds that has
// sat at a permission prompt longest, once that is past stuckAfter. Caller
// holds l.mu.
//
// 🔴 A LANE AT A PERMISSION PROMPT IS BUSY FOR EVER WITHOUT THIS. Its prompt was
// submitted and no Stop follows until someone answers in its terminal, so its
// server is never idle(); and the turn that drove it gives up (the caller's
// timeout) and releases the lane long before. CCD_MAX_SESSIONS such lanes
// refused every new session with 503 until the pod restarted. Answering is the
// operator's — the "awaiting approval" badge now shows a lane's prompt as well
// as the primary's (blockedReporter) — but after stuckAfter unanswered the lane
// is fair game: its conversation is on disk and its next turn resumes it.
//
// ⚠ NOT A TIMEOUT TO IDLE: the CLI would still be showing the dialog, and a
// paste into it would answer the dialog rather than reach the model.
//
// ⚠ A LANE A TURN STILL HOLDS IS NEVER TAKEN, stuck or not (lane.reserved).
func (l *lanes) pickEvictLocked() (evict *lane, stuck bool) {
	for _, ln := range l.set {
		if ln.reserved == 0 && ln.srv.idle() && (evict == nil || ln.lastUsed.Before(evict.lastUsed)) {
			evict = ln
		}
	}
	if evict != nil {
		return evict, false
	}
	var oldest time.Time
	for _, ln := range l.set {
		if ln.reserved != 0 {
			continue
		}
		since, waiting := ln.srv.awaitingSince()
		if waiting && l.now().Sub(since) >= l.stuckAfterOr() && (evict == nil || since.Before(oldest)) {
			evict, oldest = ln, since
		}
	}
	return evict, evict != nil
}

func (l *lanes) stuckAfterOr() time.Duration {
	if l.stuckAfter > 0 {
		return l.stuckAfter
	}
	return stuckLaneAfter
}

// release ends one reservation acquire took.
func (l *lanes) release(ln *lane) {
	l.mu.Lock()
	if ln.reserved > 0 {
		ln.reserved--
	}
	l.mu.Unlock()
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
	srv.blockedSrc = id
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
		// 🔴 OWNERSHIP FIRST, THEN THE KILL. The session is killed BY NAME, and
		// a lane started again for this key (after this one was taken out of
		// the set) has the SAME name: killing it unconditionally here killed the
		// replacement's CLI. A lane no longer in the set was removed by whoever
		// took it out (an eviction stops it itself).
		l.mu.Lock()
		owned := l.set[ln.id] == ln
		if owned {
			delete(l.set, ln.id)
		}
		l.mu.Unlock()
		if !owned {
			return
		}
		ln.clearBlocked()
		kctx, kcancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = ln.term.KillSession(kctx)
		kcancel()
	}
}

// clearBlocked withdraws the lane's "awaiting approval" from the agent's badge:
// a stopped lane's prompt is gone with its CLI.
func (ln *lane) clearBlocked() {
	if ln.srv.blocked != nil {
		ln.srv.blocked.setFrom(ln.id, false)
	}
}

// stop ends the lane's CLI and its tmux session.
func (ln *lane) stop() {
	if ln.cancel != nil {
		ln.cancel()
	}
	ln.clearBlocked()
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

// waitStarted waits until the lane's CLI is ready for a turn, its supervisor
// gave up, or d passes. Ready is BOTH halves:
//   - the CLI has sent SessionStart (it is at its prompt), and
//   - the supervisor has recorded the start (its pane is the input pane).
//
// 🔴 EITHER CAN COME FIRST. tmux new-session runs the CLI before it prints the
// pane id that Start returns, so the CLI's SessionStart can land while the
// supervisor is still `starting` (startedRef) — and a turn then has no pane to
// paste into: runTurn answers 503 not_ready. Waiting on SessionStart alone made
// a lane's first turn fail whenever new-session was slow to return.
func (ln *lane) waitStarted(ctx context.Context, d time.Duration) *failure {
	deadline := time.NewTimer(d)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		hooked := ln.srv.started()
		_, running := ln.sup.inputPane()
		if hooked && running {
			return nil
		}
		select {
		case <-ln.done:
			st := ln.sup.state()
			return &failure{Status: http.StatusServiceUnavailable, Type: failNotReady,
				Message: fmt.Sprintf("the session's Claude Code CLI did not stay up (%s: %s)", st.CLI, st.Detail)}
		case <-deadline.C:
			why := "no SessionStart"
			if hooked {
				why = "SessionStart seen, but the supervisor has not recorded the CLI as running: " + ln.sup.state().CLI
			}
			return &failure{Status: http.StatusServiceUnavailable, Type: failNotReady,
				Message: fmt.Sprintf("the session's Claude Code CLI did not reach its prompt within %s (%s)", d, why)}
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
	// AwaitingApproval: the lane's CLI is at a permission prompt (since
	// AwaitingSince); Stuck: for longer than stuckAfter, so a new session at
	// the cap may stop it (pickEvictLocked).
	AwaitingApproval bool   `json:"awaiting_approval,omitempty"`
	AwaitingSince    string `json:"awaiting_since,omitempty"`
	Stuck            bool   `json:"stuck,omitempty"`
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
		h := laneHealth{ID: ln.id, Model: ln.model, CLI: ln.sup.state().CLI,
			Session: ln.srv.sessionState(started, ended), Busy: busy}
		if since, waiting := ln.srv.awaitingSince(); waiting {
			h.AwaitingApproval, h.AwaitingSince = true, since.UTC().Format(time.RFC3339)
			h.Stuck = l.now().Sub(since) >= l.stuckAfterOr()
		}
		out = append(out, h)
	}
	return out
}
