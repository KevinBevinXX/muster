package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"
)

// The supervisor keeps the interactive CLI running in its tmux pane, IN THE POD:
// an exit (an operator's /exit, a crash) is followed by a restart in the same
// pane, resuming the same conversation, with no pod restart.
//
// 🔴 RESUME, NEVER FORK. Every start is `claude --resume <id>` when the config dir
// already holds a transcript for this workspace (argv: the primary's recorded
// session, else its newest), and plain `claude` only when it holds none (a
// genuinely fresh volume). The decision is made by LOOKING at the transcripts,
// never by running a resume and reading its exit code: measured on the pinned
// CLI, `claude --continue` with nothing to continue
// prints "No conversation found to continue" and exits 1 — but so does any other
// failure, and `--continue || claude` turns every one of them into a NEW
// conversation that silently abandons the old one.
//
// 🔴 A CRASH LOOP IS A POD PROBLEM, AND IS HANDED TO KUBERNETES. Each restart
// waits backoffMin·2^(k-1) (capped at backoffMax), k being the number of exits
// within crashWindow. When crashExits exits fall within crashWindow, the
// supervisor stops restarting and reports `cli: crash_loop`; /healthz then
// answers non-200, so the liveness probe restarts the pod and the restart count
// is visible in `kubectl get pod`. It latches: without a liveness probe (a bare
// `docker run`) nothing restarts it.
//
// 🔴 A LOST TERMINAL IS HANDED TO KUBERNETES TOO, FOR THE SAME REASON. When the
// pane ccd started can no longer be read as ITS pane — the tmux server went away
// (`kill-server`, the last `kill-session`), or one started again on the socket now
// owns that pane id (tmuxTerminal.state) — the supervisor reports
// `cli: terminal_lost`, refuses input, and /healthz fails. It does NOT start a
// new CLI in a new session of its own, because:
//   - it can no longer see the old CLI process: it is nobody's pane, so no tmux
//     will report its exit, and a second `claude --resume` started beside a CLI
//     that survived would be two writers of one conversation — a fork in all but
//     name. A container restart ends every process in the container, so the
//     resume that follows is the only one;
//   - a session named `cc` that ccd did not create belongs to whoever created it,
//     and ccd would have to kill it or work around it to start its own.
//
// The pod's next start resumes the conversation exactly as an in-pod restart
// would: argv() resumes whenever a transcript exists. Like
// crash_loop it latches, and without a liveness probe nothing restarts it.
//
// 🔴 EVERY START IS PRECEDED BY THE SEED'S STRIP of settingsForbidden (seed.go
// stripSettingsFile): the entrypoint's `ccd seed` runs once per POD, and the
// in-pod restarts here are where a key written into settings.json since then (an
// operator's /config, the session editing its own config) would otherwise reach
// the next CLI.
type cliPane interface {
	// Start creates the session running argv in dir, with the pane kept (dead)
	// after its process exits, and returns the pane's ref.
	Start(ctx context.Context, dir string, argv []string) (paneRef, error)
	// Respawn runs argv in the (dead) pane again; errTerminalLost when the pane
	// is no longer ref's.
	Respawn(ctx context.Context, ref paneRef, dir string, argv []string) error
	// Exited reports whether the pane's process has exited and, once known, how;
	// an error (errTerminalLost, or tmux not answering) means the supervisor can
	// no longer see its CLI.
	Exited(ctx context.Context, ref paneRef) (paneExit, error)
	// Reap makes the terminal collect an exited pane process whose exit it has
	// not noticed yet (see run).
	Reap(ctx context.Context) error
}

// paneExit is one read of a pane's process state.
type paneExit struct {
	Dead bool // the process has exited
	// Known: how it exited is known. A dead pane can be not-Known (see
	// parsePaneExit); the supervisor Reaps and waits statusGrace before giving up.
	Known  bool
	Status int // the exit status; 128+Signal for a signal death (exitUnknown when only SignalText is known)
	Signal int // the killing signal's number, or 0
	// SignalText is pane_dead_signal as tmux printed it: the number on Linux, a
	// name where tmux has sys_signame.
	SignalText string
}

// describe names how the process ended, for Detail and the log.
func (e paneExit) describe() string {
	switch {
	case !e.Known:
		return "exited, status unknown (tmux reported the pane dead with neither a status nor a signal)"
	case e.Signal != 0:
		return fmt.Sprintf("was killed by signal %d (recorded as status %d)", e.Signal, e.Status)
	case e.SignalText != "":
		return fmt.Sprintf("was killed by signal %s (status recorded as unknown)", e.SignalText)
	}
	return fmt.Sprintf("exited with status %d", e.Status)
}

// exitUnknown is cli_last_exit for an exit whose status never became known.
const exitUnknown = -1

const (
	cliStarting   = "starting"
	cliRunning    = "running"
	cliRestarting = "restarting"
	cliCrashLoop  = "crash_loop"
	cliLost       = "terminal_lost"

	modeContinue = "continue"
	modeFresh    = "fresh"
)

// cliState is what /healthz reports about the supervised CLI.
type cliState struct {
	CLI      string `json:"cli"`
	Mode     string `json:"cli_mode,omitempty"` // the last start: "continue" or "fresh"
	Starts   int    `json:"cli_starts"`
	LastExit *int   `json:"cli_last_exit,omitempty"`
	Detail   string `json:"cli_detail,omitempty"`
}

type supervisor struct {
	pane      cliPane
	bin       string
	configDir string
	workspace string

	poll                   time.Duration
	statusGrace            time.Duration // how long a dead pane may report no status (see run)
	backoffMin, backoffMax time.Duration
	crashExits             int
	crashWindow            time.Duration
	now                    func() time.Time
	after                  func(time.Duration) <-chan time.Time
	// argvFn, when set, replaces argv's choice of command line: a lane's
	// (lanes.go), which resumes its own session by id.
	argvFn func() ([]string, string)
	// onExit, when set, is told of every exit the supervisor records, BEFORE it
	// is recorded and so before any restart: the server served by this CLI
	// clears its SessionStart (server.cliExited). Set before run.
	onExit func()

	mu       sync.Mutex
	st       cliState
	paneID   paneRef     // the CLI's pane, once Start has returned it
	lastExit string      // paneExit.describe() of the latest exit
	exits    []time.Time // within crashWindow of the latest
	// restartErr is why the latest in-pod restart did not happen, cleared by a
	// successful start. A failed restart leaves the pane dead, so the next poll
	// counts another exit; without this the crash_loop detail would blame the
	// CLI for exits that were really a restart ccd could not perform.
	restartErr string

	// firstStart is closed by the first started(): from then on paneID is the
	// CLI's pane (startedRef).
	firstStart chan struct{}
}

func newSupervisor(pane cliPane, bin, configDir, workspace string) *supervisor {
	return &supervisor{
		pane: pane, bin: bin, configDir: configDir, workspace: workspace,
		poll: time.Second, statusGrace: 2 * time.Second, backoffMin: time.Second, backoffMax: 30 * time.Second,
		crashExits: 5, crashWindow: 2 * time.Minute,
		now: time.Now, after: time.After,
		st:         cliState{CLI: cliStarting},
		firstStart: make(chan struct{}),
	}
}

func (s *supervisor) state() cliState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.st
	if st.LastExit != nil {
		v := *st.LastExit
		st.LastExit = &v
	}
	return st
}

// nonAlnum is the CLI's own encoding of a working directory into its project
// directory name: every byte outside [A-Za-z0-9] becomes '-'. Measured on the
// pinned CLI (/data/workspace → projects/-data-workspace) and on a workstation
// CLI, where a cwd ending in /.claude became a name ending in --claude ('.' is
// replaced too, not only '/').
var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]`)

// argv is the primary's command line for its next start, chosen by LOOKING at
// the transcripts (measured on the pinned CLI: no transcript exists after
// startup alone; the first prompt writes one, and so does a typed /exit — so a
// session that was started and never touched is, correctly, still fresh):
//
//   - the recorded primary session (primaryRecordFile), by id, while its
//     transcript exists;
//   - else, by id, the newest transcript that may be the primary's
//     (newestPrimaryTranscript: not a lane's; an interactive CLI's if any is,
//     else the newest of any stamp);
//   - else a fresh start.
//
// 🔴 NEVER A BARE `--continue`. Lanes write their transcripts in the same
// project directory, and `--continue` is "the newest conversation in this
// directory" as the CLI reads it WHEN IT BOOTS — after this choice is made. A
// lane that writes its FIRST transcript in that window (a lane may be running,
// with no transcript yet, while the primary restarts) is then the newest, and
// the primary resumes the lane's conversation. No check made here, of the disk
// or of the running lanes, can see a file written after it; an id chosen here
// cannot move. (TestThePrimaryRespawnDoesNotContinueALaneThatWritesDuringItsBoot.)
// Resuming by id is what the primary has done since #145 once its id is
// recorded; this removes the unrecorded case's `--continue`.
func (s *supervisor) argv() ([]string, string) {
	if s.argvFn != nil {
		return s.argvFn()
	}
	if id := recordedPrimary(s.configDir, s.workspace); id != "" {
		return []string{s.bin, "--resume", id}, modeContinue
	}
	if newest, tier := newestPrimaryTranscript(s.configDir, s.workspace); newest != "" {
		log.Printf("ccd: resuming %s: newest primary transcript (no record, %s)", newest, tier)
		return []string{s.bin, "--resume", newest}, modeContinue
	}
	log.Printf("ccd: starting fresh: no primary transcript")
	return []string{s.bin}, modeFresh
}

// inputPane is the pane the server pastes into: the CLI's paneRef, and only while
// the supervisor believes the CLI in it is running. Before the first start,
// between an exit and its restart, in crash_loop and in terminal_lost it reports
// none, and the server answers not_ready rather than typing into a pane with no
// CLI in it. (An exit or a lost server the next poll has not seen yet is caught by
// tmuxTerminal.Paste's own read of the pane's server, id and pane_dead.)
func (s *supervisor) inputPane() (paneRef, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.paneID, s.paneID.Pane != "" && s.st.CLI == cliRunning
}

// ref is the CLI's paneRef in any state (zero before the first start), for
// /healthz's terminal check.
func (s *supervisor) ref() paneRef {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.paneID
}

// cliPID is the pid of the CLI's pane process, read with panePID
// (tmuxTerminal.PanePID) — waiting, until ctx ends, for the CLI's first start
// when it has not happened yet (startedRef).
func (s *supervisor) cliPID(ctx context.Context, panePID func(context.Context, paneRef) (int, error)) (int, error) {
	ref, err := s.startedRef(ctx)
	if err != nil {
		return 0, err
	}
	return panePID(ctx, ref)
}

// startedRef is ref once the CLI has been started, waiting for that first start
// (until ctx ends) when it has not been yet.
//
// 🔴 THE CLI CAN SPEAK BEFORE ccd KNOWS ITS PANE. tmux new-session starts the
// CLI and only then prints the pane id that Start returns, so the CLI's first
// SessionStart hook can reach ccd before started() has recorded the pane. Read
// with ref(), that hook's pane was "no pane yet": the primary's session id went
// unrecorded (primaryRecordFile), and the next primary restart fell back to
// the newest transcript (then any non-lane one; now newestPrimaryTranscript's
// choice, which prefers an interactive CLI's — still not proof it is the
// primary's). Measured as
// TestTmuxLaneTwoSessionsRunInTheirOwnCLIsOnTheirOwnModels' CI flake ("no
// primary session id recorded").
//
// Once started, startedRef itself answers at once, even on a spent ctx: the
// started check runs first, on its own — in one select with ctx.Done() Go would
// pick between two ready cases at random. (cliPID then reads the pane's pid with
// panePID(ctx, …), which a spent ctx can still fail.)
func (s *supervisor) startedRef(ctx context.Context) (paneRef, error) {
	select {
	case <-s.firstStart:
		return s.ref(), nil
	default:
	}
	select {
	case <-s.firstStart:
		return s.ref(), nil
	case <-ctx.Done():
		return paneRef{}, fmt.Errorf("the CLI has not been started: %w", ctx.Err())
	}
}

// lose latches terminal_lost (see the type comment): the supervisor can no longer
// see the CLI it started.
func (s *supervisor) lose(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.CLI = cliLost
	s.st.Detail = fmt.Sprintf("ccd can no longer see the CLI's pane %s (%v); not starting another CLI "+
		"in this pod — /healthz now fails so Kubernetes restarts the pod", s.paneID, err)
}

// started records a (re)start in pane. A previous exit stays named in Detail.
func (s *supervisor) started(pane paneRef, mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.CLI, s.st.Mode, s.paneID = cliRunning, mode, pane
	s.st.Detail = ""
	if s.lastExit != "" {
		s.st.Detail = "previous run " + s.lastExit
	}
	s.st.Starts++
	s.restartErr = ""
	if s.st.Starts == 1 {
		close(s.firstStart)
	}
}

// restartFailed records and logs why an in-pod restart did not happen.
func (s *supervisor) restartFailed(why string) {
	log.Printf("ccd: not restarting the CLI: %s", why)
	s.mu.Lock()
	s.restartErr = why
	s.mu.Unlock()
	s.setDetail(why)
}

func (s *supervisor) setDetail(d string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Detail = d
}

// recordExit counts one exit and decides what follows it: the delay before the
// next start, or (crash = true) no next start at all.
//
// cli_last_exit is the exit status, 128+N for a process killed by signal N (the
// shell's convention), and exitUnknown (-1) when tmux never reported either;
// Detail names which.
func (s *supervisor) recordExit(e paneExit) (delay time.Duration, crash bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	kept := s.exits[:0]
	for _, t := range s.exits {
		if now.Sub(t) < s.crashWindow {
			kept = append(kept, t)
		}
	}
	s.exits = append(kept, now)
	status := e.Status
	if !e.Known {
		status = exitUnknown
	}
	s.st.LastExit = &status
	s.lastExit = e.describe()
	k := len(s.exits)
	if k >= s.crashExits {
		s.st.CLI = cliCrashLoop
		s.st.Detail = fmt.Sprintf("the CLI exited %d times within %s (last: %s); not restarting it — "+
			"/healthz now fails so Kubernetes restarts the pod", k, s.crashWindow, s.lastExit)
		if s.restartErr != "" {
			s.st.Detail += "; the last in-pod restart did not happen: " + s.restartErr
		}
		return 0, true
	}
	delay = s.backoffMin
	for i := 1; i < k && delay < s.backoffMax; i++ {
		delay *= 2
	}
	delay = min(delay, s.backoffMax)
	s.st.CLI = cliRestarting
	s.st.Detail = fmt.Sprintf("the CLI %s; restarting in %s", s.lastExit, delay)
	return delay, false
}

// run starts the CLI and keeps it running until ctx ends, it crash-loops, or the
// terminal is lost. Its error is a CLI that could not be started at all.
func (s *supervisor) run(ctx context.Context) error {
	if err := stripSettingsFile(s.configDir, log.Writer()); err != nil {
		return fmt.Errorf("before starting the CLI: %w", err)
	}
	argv, mode := s.argv()
	pane, err := s.pane.Start(ctx, s.workspace, argv)
	if err != nil {
		return err
	}
	s.started(pane, mode)
	log.Printf("ccd: started %q in pane %s (%s)", strings.Join(argv, " "), pane, mode)
	var deadSince time.Time // first poll that saw the pane dead with no status yet
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return nil
		case <-s.after(s.poll):
		}
		e, err := s.pane.Exited(ctx, pane)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// The pane is gone, belongs to another tmux server, or tmux cannot be
			// read: the supervisor can no longer see its CLI (see the type comment).
			s.lose(err)
			log.Printf("ccd: %s", s.state().Detail)
			return nil
		}
		// Dead but not yet reaped (see parsePaneExit). First make tmux reap: it can
		// MISS a pane process's SIGCHLD and leave it a zombie it never waits for
		// (measured on tmux 3.3a, the image's: in about 1 serve-test run in 10,
		// the pane's `sh` sat <defunct> under the tmux server and its status was
		// still unreported 12s later). Any later SIGCHLD in the server runs its
		// waitpid(WAIT_ANY) loop, which reaps the zombie and records the status —
		// so Reap runs a job (measured: the read right after it had the status in
		// all 6 such runs observed). Then poll again, for at most statusGrace, and
		// count it as an exit of unknown status rather than waiting for ever.
		if e.Dead && !e.Known {
			if err := s.pane.Reap(ctx); err == nil {
				if again, err := s.pane.Exited(ctx, pane); err == nil {
					if again.Known {
						log.Printf("ccd: tmux had not reaped the CLI's exited pane process; after Reap it reports: %s", again.describe())
					}
					e = again
				}
			}
		}
		if !e.Dead {
			deadSince = time.Time{}
			continue
		}
		if !e.Known {
			if deadSince.IsZero() {
				deadSince = s.now()
			}
			if s.now().Sub(deadSince) < s.statusGrace {
				continue
			}
		}
		deadSince = time.Time{}
		// 🔴 THE DEAD CLI'S SessionStart DIES WITH IT. Nothing else clears it
		// when the CLI ends without a SessionEnd (SIGKILL, OOM): the server
		// would still say "started" once the respawn below is running, before
		// the NEW CLI's SessionStart, and a turn would paste into a CLI still
		// booting (504 not_submitted, which muster does not re-send) instead of
		// answering 503 not_ready (which it does). Cleared on the EXIT, never on
		// a start: a CLI's SessionStart can arrive before started() records its
		// pane (startedRef). And this runs before the Respawn below, in this
		// goroutine, so it cannot wipe the NEXT CLI's SessionStart.
		if s.onExit != nil {
			s.onExit()
		}
		delay, crash := s.recordExit(e)
		if crash {
			log.Printf("ccd: %s", s.state().Detail)
			return nil
		}
		log.Printf("ccd: the CLI %s; restarting in %s", e.describe(), delay)
		select {
		case <-ctx.Done():
			return nil
		case <-s.after(delay):
		}
		if ctx.Err() != nil {
			return nil
		}
		// The pane stays dead after either failure below, so the next poll counts
		// it as another exit: a restart that keeps failing ends in crash_loop like
		// a CLI that does.
		if err := stripSettingsFile(s.configDir, log.Writer()); err != nil {
			s.restartFailed("before restarting the CLI: " + err.Error())
			continue
		}
		argv, mode = s.argv()
		if err := s.pane.Respawn(ctx, pane, s.workspace, argv); err != nil {
			if errors.Is(err, errTerminalLost) {
				s.lose(err)
				log.Printf("ccd: %s", s.state().Detail)
				return nil
			}
			s.restartFailed("respawn: " + err.Error())
			continue
		}
		s.started(pane, mode)
		log.Printf("ccd: restarted %q (%s)", strings.Join(argv, " "), mode)
	}
	return nil
}
