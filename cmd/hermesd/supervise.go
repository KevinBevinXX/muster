package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// The supervisor keeps ONE `hermes gateway run` running, in the pod: an exit is
// followed by a restart with backoff, and every start — the first and every
// restart — is followed by start-up recovery before a turn is accepted.
//
// 🔴 RECOVERY RUNS AFTER EVERY START OF THE GATEWAY, NOT ONLY hermesd's. The
// likely SIGKILL is the gateway alone being OOM-killed while hermesd lives on;
// it leaves the in-flight run `unknown` and parks every later run on that
// session behind it (S6). So each start goes starting → (Hermes's /health
// answers) → recovering → ready, and /healthz names the state; a turn is
// accepted only in `ready`.
//
// 🔴 A CRASH LOOP IS A POD PROBLEM, AND IS HANDED TO KUBERNETES, as ccd's is:
// crashExits exits within crashWindow latch `crash_loop`, /healthz fails, and the
// liveness probe restarts the pod.
//
// 🔴 AN EXIT hermesd ASKED FOR IS NOT A CRASH. On SIGTERM the gateway drains for
// 30 s, interrupts what is still running and exits 1 (S6). An exit after
// hermesd itself sent SIGTERM is recorded as a stop, never counted toward the
// latch: otherwise a rollout during a crash-prone hour would flip /healthz to
// failing while the pod drains.

// gatewayProc is one started gateway process.
type gatewayProc interface {
	// Wait blocks until the process exits and returns its exit status (128+N
	// for a death by signal N).
	Wait() int
	Signal(os.Signal) error
}

const (
	stWaitingCheckout = "waiting_checkout"
	stStarting        = "starting"
	stRecovering      = "recovering"
	stReady           = "ready"
	stRestarting      = "restarting"
	stCrashLoop       = "crash_loop"
	stEnvForbidden    = "env_forbidden"
	stStopped         = "stopped"
)

// supervisorState is what /healthz reports about the gateway.
type supervisorState struct {
	State      string `json:"state"`
	Detail     string `json:"detail,omitempty"`
	Starts     int    `json:"gateway_starts"`
	Recoveries int    `json:"recoveries"`
	LastExit   *int   `json:"gateway_last_exit,omitempty"`
}

// failed reports whether the state fails /healthz: the latches only.
func (s supervisorState) failed() bool {
	return s.State == stCrashLoop || s.State == stEnvForbidden
}

type supervisor struct {
	start   func() (gatewayProc, error)
	prepare func() error                    // before every start: the .env check and the rendered config
	healthy func(ctx context.Context) error // Hermes's GET /health
	recover func(ctx context.Context) error // start-up recovery, after every start

	healthTimeout          time.Duration
	healthPoll             time.Duration
	recoverRetry           time.Duration
	stopGrace              time.Duration
	backoffMin, backoffMax time.Duration
	crashExits             int
	crashWindow            time.Duration
	now                    func() time.Time
	logf                   func(string, ...any)

	mu       sync.Mutex
	st       supervisorState
	exits    []time.Time
	termSent bool // hermesd sent the current gateway SIGTERM
	// epoch is cancelled when the current gateway exits: work tied to one
	// gateway's life (recovery's watchers) stops with it.
	epoch context.Context
}

func newSupervisor(start func() (gatewayProc, error), prepare func() error,
	healthy, recover func(context.Context) error, logf func(string, ...any)) *supervisor {
	return &supervisor{
		start: start, prepare: prepare, healthy: healthy, recover: recover, logf: logf,
		healthTimeout: 2 * time.Minute, healthPoll: 250 * time.Millisecond, recoverRetry: 2 * time.Second,
		stopGrace: 40 * time.Second, backoffMin: time.Second, backoffMax: 30 * time.Second,
		crashExits: 5, crashWindow: 2 * time.Minute, now: time.Now,
		st: supervisorState{State: stStarting}, epoch: context.Background(),
	}
}

func (s *supervisor) state() supervisorState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.st
	if st.LastExit != nil {
		v := *st.LastExit
		st.LastExit = &v
	}
	return st
}

func (s *supervisor) set(state, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.State, s.st.Detail = state, detail
}

// currentEpoch is the context of the gateway running now.
func (s *supervisor) currentEpoch() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.epoch
}

// recordExit counts one unrequested exit and decides what follows it: the
// delay before the next start, or (crash = true) none at all.
func (s *supervisor) recordExit(status int, what string) (delay time.Duration, crash bool) {
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
	s.st.LastExit = &status
	k := len(s.exits)
	if k >= s.crashExits {
		s.st.State = stCrashLoop
		s.st.Detail = fmt.Sprintf("the Hermes gateway exited %d times within %s (last: %s); not restarting it — "+
			"/healthz now fails so Kubernetes restarts the pod", k, s.crashWindow, what)
		return 0, true
	}
	delay = s.backoffMin
	for i := 1; i < k && delay < s.backoffMax; i++ {
		delay *= 2
	}
	delay = min(delay, s.backoffMax)
	s.st.State = stRestarting
	s.st.Detail = fmt.Sprintf("the Hermes gateway %s; restarting in %s", what, delay)
	return delay, false
}

// run starts the gateway and keeps it running until ctx ends (hermesd is
// shutting down: the gateway is asked to stop and waited for) or a latch.
func (s *supervisor) run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := s.prepare(); err != nil {
			if errors.Is(err, errDotEnv) {
				s.set(stEnvForbidden, err.Error())
				s.logf("%s", err)
				return
			}
			if s.afterExit(ctx, -1, "could not be prepared: "+err.Error()) {
				return
			}
			continue
		}
		p, err := s.start()
		if err != nil {
			if s.afterExit(ctx, -1, "could not be started: "+err.Error()) {
				return
			}
			continue
		}
		epoch, endEpoch := context.WithCancel(ctx)
		s.mu.Lock()
		s.st.Starts++
		s.st.State, s.st.Detail = stStarting, ""
		s.termSent = false
		s.epoch = epoch
		s.mu.Unlock()

		var status int
		done := make(chan struct{})
		go func() { status = p.Wait(); close(done) }()
		go s.bringUp(epoch, p, done)

		select {
		case <-done:
		case <-ctx.Done():
			s.stop(p, done)
		}
		endEpoch()
		s.mu.Lock()
		asked := s.termSent
		s.mu.Unlock()
		if asked {
			s.set(stStopped, fmt.Sprintf("the Hermes gateway exited %d after hermesd asked it to stop (SIGTERM); not a crash", status))
			s.logf("hermesd: the gateway stopped (status %d) after SIGTERM", status)
			return
		}
		if s.afterExit(ctx, status, fmt.Sprintf("exited with status %d", status)) {
			return
		}
	}
}

// afterExit records an unrequested exit (or a failed start) and waits out the
// backoff. It reports whether the supervisor must stop: a latch, or shutdown.
func (s *supervisor) afterExit(ctx context.Context, status int, what string) bool {
	delay, crash := s.recordExit(status, what)
	s.logf("hermesd: %s", s.state().Detail)
	if crash {
		return true
	}
	select {
	case <-ctx.Done():
		return true
	case <-time.After(delay):
		return false
	}
}

// bringUp waits for Hermes's API to answer, then recovers, then marks the
// gateway ready. It gives up when the gateway exits (done) or its epoch ends.
// A gateway whose API never answers within healthTimeout is killed: the exit
// that follows is counted like any other.
func (s *supervisor) bringUp(ctx context.Context, p gatewayProc, done <-chan struct{}) {
	deadline := s.now().Add(s.healthTimeout)
	for {
		hctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := s.healthy(hctx)
		cancel()
		if err == nil {
			break
		}
		if s.now().After(deadline) {
			s.logf("hermesd: the gateway's API did not answer within %s (%v); killing it", s.healthTimeout, err)
			_ = p.Signal(syscall.SIGKILL)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-time.After(s.healthPoll):
		}
	}
	s.set(stRecovering, "resolving runs a previous gateway left unsettled before accepting a turn")
	for {
		err := s.recover(ctx)
		if err == nil {
			break
		}
		s.logf("hermesd: start-up recovery: %v; retrying in %s", err, s.recoverRetry)
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-time.After(s.recoverRetry):
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx.Err() != nil {
		return
	}
	s.st.Recoveries++
	s.st.State, s.st.Detail = stReady, ""
}

// stop asks the gateway to stop (SIGTERM, which it drains for 30 s) and waits
// for it, escalating to SIGKILL after stopGrace.
func (s *supervisor) stop(p gatewayProc, done <-chan struct{}) {
	s.mu.Lock()
	s.termSent = true
	s.st.State, s.st.Detail = stStopped, "hermesd is shutting down; the gateway is draining"
	s.mu.Unlock()
	_ = p.Signal(syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(s.stopGrace):
		s.logf("hermesd: the gateway did not stop within %s of SIGTERM; killing it", s.stopGrace)
		_ = p.Signal(syscall.SIGKILL)
		<-done
	}
}

// execProc is a gateway started as a real process, in its own process group.
type execProc struct{ cmd *exec.Cmd }

// gatewayArgs is the gateway's command line. `--external-supervisor` declares
// hermesd the process manager that owns this foreground gateway: an in-chat
// restart or update then exits back to hermesd instead of spawning a detached
// replacement it would not supervise (P:hermes_cli/gateway.py, _cmd_run).
var gatewayArgs = []string{"gateway", "run", "--external-supervisor"}

// startGateway starts `hermes gateway run` with env.
func startGateway(bin, dir string, env []string) (gatewayProc, error) {
	cmd := exec.Command(bin, gatewayArgs...)
	cmd.Env = env
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	// Its own process group, so what it leaves behind when it dies (terminal
	// tool subprocesses) can be killed with it. The image's init (tini) reaps
	// them.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &execProc{cmd: cmd}, nil
}

func (p *execProc) Signal(sig os.Signal) error { return p.cmd.Process.Signal(sig) }

func (p *execProc) Wait() int {
	_ = p.cmd.Wait()
	// Whatever the gateway's process group still holds goes with it.
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	ws, ok := p.cmd.ProcessState.Sys().(syscall.WaitStatus)
	switch {
	case !ok:
		return p.cmd.ProcessState.ExitCode()
	case ws.Signaled():
		return 128 + int(ws.Signal())
	}
	return ws.ExitStatus()
}
