package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePane is a cliPane whose process exits on the first poll after each start,
// with a scripted status — a CLI that dies at once, every time.
type fakePane struct {
	mu       sync.Mutex
	starts   [][]string // argv of Start, then of each Respawn
	running  bool
	status   int
	onStart  func(n int) // after the n-th start (1-based)
	failResp bool
	failN    int // fail this many Respawns, then succeed
}

func (p *fakePane) Start(_ context.Context, _ string, argv []string) (paneRef, error) {
	p.mu.Lock()
	p.starts = append(p.starts, argv)
	p.running = true
	n := len(p.starts)
	p.mu.Unlock()
	if p.onStart != nil {
		p.onStart(n)
	}
	return fakeRef, nil
}

// fakeRef is the pane the fakes start.
var fakeRef = paneRef{Server: "1 1", Pane: "%0"}

func (p *fakePane) Respawn(ctx context.Context, _ paneRef, dir string, argv []string) error {
	if p.failResp {
		return os.ErrPermission
	}
	p.mu.Lock()
	if p.failN > 0 {
		p.failN--
		p.mu.Unlock()
		return os.ErrPermission
	}
	p.mu.Unlock()
	_, err := p.Start(ctx, dir, argv)
	return err
}

func (p *fakePane) Reap(context.Context) error { return nil }

func (p *fakePane) Exited(context.Context, paneRef) (paneExit, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.running = false
	return paneExit{Dead: true, Known: true, Status: p.status}, nil
}

// testSupervisor uses a fake clock: `after` fires at once and records every wait
// that is not a poll, and each wait advances the clock by its duration plus
// `extra` (time the CLI spent running).
func testSupervisor(t *testing.T, pane cliPane, cfg, ws string, extra time.Duration) (*supervisor, *[]time.Duration) {
	t.Helper()
	s := newSupervisor(pane, "claude", cfg, ws)
	var mu sync.Mutex
	clock := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	var delays []time.Duration
	s.poll = time.Nanosecond
	s.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	s.after = func(d time.Duration) <-chan time.Time {
		mu.Lock()
		defer mu.Unlock()
		if d != s.poll {
			delays = append(delays, d)
		}
		clock = clock.Add(d + extra)
		c := make(chan time.Time, 1)
		c <- clock
		return c
	}
	return s, &delays
}

func writeTranscript(t *testing.T, cfg, projDir string) {
	t.Helper()
	d := filepath.Join(cfg, "projects", projDir)
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "11111111-2222-4333-8444-555555555555.jsonl"), []byte(cliTranscript), 0o600); err != nil {
		t.Fatal(err)
	}
}

// 🔴 FRESH vs RESUME IS DECIDED BY LOOKING, AND ONLY AT THIS WORKSPACE'S
// PROJECT DIRECTORY. A resume names the conversation by id, never a bare
// --continue (supervisor.argv). The encoded names are written out literally (not derived
// from nonAlnum) so a wrong encoding cannot agree with itself.
func TestTheFirstStartContinuesOnlyWhenThisWorkspaceHasATranscript(t *testing.T) {
	cases := []struct {
		name string
		prep func(t *testing.T, cfg string)
		want []string
	}{
		{"fresh volume", func(*testing.T, string) {}, []string{"claude"}},
		{"transcript for this workspace", func(t *testing.T, cfg string) { writeTranscript(t, cfg, "-data-my-ws-v2") }, []string{"claude", "--resume", "11111111-2222-4333-8444-555555555555"}},
		{"transcript for ANOTHER workspace only", func(t *testing.T, cfg string) { writeTranscript(t, cfg, "-data-other") }, []string{"claude"}},
		{"project dir with no transcript", func(t *testing.T, cfg string) {
			if err := os.MkdirAll(filepath.Join(cfg, "projects", "-data-my-ws-v2"), 0o700); err != nil {
				t.Fatal(err)
			}
		}, []string{"claude"}},
		{"slash-only encoding would miss it", func(t *testing.T, cfg string) { writeTranscript(t, cfg, "-data-my_ws.v2") }, []string{"claude"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := t.TempDir()
			c.prep(t, cfg)
			s := newSupervisor(nil, "claude", cfg, "/data/my_ws.v2")
			if got, _ := s.argv(); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("argv %q, want %q", got, c.want)
			}
		})
	}
}

// /exit (or any exit) is followed by a restart IN THE PANE that resumes the
// conversation the first run created — never a second, fresh one.
func TestARestartResumesTheConversationTheFirstRunCreated(t *testing.T) {
	cfg := t.TempDir()
	pane := &fakePane{}
	s, _ := testSupervisor(t, pane, cfg, "/data/workspace", time.Hour) // runs far apart: no crash loop
	ctx, cancel := context.WithCancel(context.Background())
	// The first run (fresh) writes a transcript, as the CLI does on its first prompt.
	pane.onStart = func(n int) {
		if n == 1 {
			writeTranscript(t, cfg, "-data-workspace")
		}
		if n == 3 {
			cancel()
		}
	}
	if err := s.run(ctx); err != nil {
		t.Fatal(err)
	}
	const id = "11111111-2222-4333-8444-555555555555"
	want := [][]string{{"claude"}, {"claude", "--resume", id}, {"claude", "--resume", id}}
	if !reflect.DeepEqual(pane.starts, want) {
		t.Fatalf("starts %q, want %q", pane.starts, want)
	}
	if st := s.state(); st.CLI != cliRunning || st.Mode != modeContinue || st.Starts != 3 {
		t.Fatalf("state %+v", st)
	}
}

// 🔴 THE BACKOFF AND THE CRASH-LOOP THRESHOLD. A CLI that dies at once: waits of
// 1s, 2s, 4s, 8s between starts, then on the 5th exit within 2 minutes the
// supervisor stops and reports crash_loop.
func TestFiveExitsWithinTheWindowIsACrashLoopAfterExponentialBackoff(t *testing.T) {
	pane := &fakePane{status: 7}
	s, delays := testSupervisor(t, pane, t.TempDir(), "/data/workspace", 0)
	// A supervisor that never declares the crash loop would restart for ever;
	// stop it well past the threshold so the assertions below report it.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pane.onStart = func(n int) {
		if n == 20 {
			cancel()
		}
	}
	if err := s.run(ctx); err != nil {
		t.Fatal(err)
	}
	st := s.state()
	if st.CLI != cliCrashLoop || st.Starts != 5 || len(pane.starts) != 5 || st.LastExit == nil || *st.LastExit != 7 {
		t.Fatalf("state %+v, %d starts", st, len(pane.starts))
	}
	if strings.Contains(st.Detail, restartNotDone) {
		t.Fatalf("a crash loop of the CLI itself blames a restart: %q", st.Detail)
	}
	if want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}; !reflect.DeepEqual(*delays, want) {
		t.Fatalf("backoff %v, want %v", *delays, want)
	}
}

// The window is real: exits further apart than it never accumulate to a crash
// loop, and each restart waits only the minimum.
func TestExitsSpreadBeyondTheWindowNeverCrashLoop(t *testing.T) {
	pane := &fakePane{}
	s, delays := testSupervisor(t, pane, t.TempDir(), "/data/workspace", 3*time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	pane.onStart = func(n int) {
		if n == 12 {
			cancel()
		}
	}
	if err := s.run(ctx); err != nil {
		t.Fatal(err)
	}
	if st := s.state(); st.CLI == cliCrashLoop || st.Starts != 12 {
		t.Fatalf("state %+v", st)
	}
	for _, d := range *delays {
		if d != time.Second {
			t.Fatalf("backoff %v grew although every exit was alone in its window", *delays)
		}
	}
}

// The backoff is capped.
func TestBackoffIsCapped(t *testing.T) {
	s := newSupervisor(nil, "claude", t.TempDir(), "/w")
	s.crashExits = 100
	clock := time.Unix(0, 0)
	s.now = func() time.Time { return clock }
	var last time.Duration
	for i := 0; i < 10; i++ {
		last, _ = s.recordExit(paneExit{Dead: true, Known: true, Status: 1})
	}
	if last != s.backoffMax {
		t.Fatalf("10th backoff %v, want the cap %v", last, s.backoffMax)
	}
}

// A respawn that keeps failing leaves the pane dead, so it too ends in crash_loop.
func TestARespawnThatKeepsFailingEndsInCrashLoop(t *testing.T) {
	pane := &fakePane{failResp: true}
	s, _ := testSupervisor(t, pane, t.TempDir(), "/data/workspace", 0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.run(ctx); err != nil {
		t.Fatal(err)
	}
	st := s.state()
	if st.CLI != cliCrashLoop || st.Starts != 1 {
		t.Fatalf("state %+v", st)
	}
	if !strings.Contains(st.Detail, restartNotDone+"respawn: ") {
		t.Fatalf("crash_loop detail does not name the failed respawn: %q", st.Detail)
	}
}

// restartNotDone is the clause a crash_loop detail carries when its exits were
// restarts ccd could not perform rather than the CLI exiting.
const restartNotDone = "the last in-pod restart did not happen: "

// A settings.json corrupted between starts stops every in-pod restart (the strip
// refuses it). The crash_loop that follows must name that refusal, not read as
// the CLI exiting five times — the CLI ran once.
func TestACrashLoopFromAFailedRestartNamesWhyTheRestartDidNotHappen(t *testing.T) {
	cfg := t.TempDir()
	pane := &fakePane{}
	pane.onStart = func(n int) {
		if n == 1 {
			if err := os.WriteFile(filepath.Join(cfg, "settings.json"), []byte("not json"), 0o600); err != nil {
				t.Error(err)
			}
		}
	}
	s, _ := testSupervisor(t, pane, cfg, "/data/workspace", 0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.run(ctx); err != nil {
		t.Fatal(err)
	}
	st := s.state()
	if st.CLI != cliCrashLoop || st.Starts != 1 || len(pane.starts) != 1 {
		t.Fatalf("state %+v, %d starts", st, len(pane.starts))
	}
	if !strings.Contains(st.Detail, restartNotDone+"before restarting the CLI: ") {
		t.Fatalf("crash_loop detail does not name the refused restart: %q", st.Detail)
	}
}

// seqPane answers Exited from a script of reads, then (once the script runs out)
// reports a live pane; Respawn ends the test by cancelling.
type seqPane struct {
	mu     sync.Mutex
	reads  []paneExit
	starts int
	reaps  int
	cancel func()
}

func (p *seqPane) Start(context.Context, string, []string) (paneRef, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.starts++
	return fakeRef, nil
}

func (p *seqPane) Respawn(context.Context, paneRef, string, []string) error {
	p.cancel()
	return nil
}

func (p *seqPane) Reap(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reaps++
	return nil
}

func (p *seqPane) Exited(context.Context, paneRef) (paneExit, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.reads) == 0 {
		return paneExit{}, nil
	}
	e := p.reads[0]
	p.reads = p.reads[1:]
	return e, nil
}

// 🔴 F3: DEAD IS NOT REAPED. A pane read as dead with no status yet is polled
// again — within statusGrace — and the status that then arrives is the one
// recorded, never a 0 read off the empty field; a signal death is 128+N and
// named; a dead pane that never reports either is recorded as exitUnknown after
// the grace instead of being waited on for ever.
func TestTheSupervisorWaitsForTheExitStatusButNotForEver(t *testing.T) {
	unreaped := paneExit{Dead: true}
	cases := []struct {
		name   string
		reads  []paneExit
		want   int
		detail string
	}{
		{"status arrives on a later poll", []paneExit{unreaped, unreaped, {Dead: true, Known: true, Status: 3}}, 3, "exited with status 3"},
		{"signal death, known as soon as Reap ran", []paneExit{unreaped, {Dead: true, Known: true, Status: 137, Signal: 9}},
			137, "killed by signal 9"},
		{"never reported", []paneExit{unreaped, unreaped, unreaped, unreaped, unreaped, unreaped, unreaped, unreaped,
			unreaped, unreaped, unreaped, unreaped}, exitUnknown, "status unknown"},
		// Each unknown poll reads twice (before and after Reap). The live read on
		// poll 2 restarts the wait; without that reset, poll 5 (2s after poll 1)
		// would give up as unknown before poll 7's status arrives.
		{"a live read in between resets the wait", []paneExit{unreaped, unreaped, unreaped, {}, unreaped, unreaped,
			unreaped, unreaped, unreaped, unreaped, unreaped, unreaped, {Dead: true, Known: true, Status: 5}}, 5,
			"exited with status 5"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			pane := &seqPane{reads: c.reads, cancel: cancel}
			s, _ := testSupervisor(t, pane, t.TempDir(), "/data/workspace", 0)
			s.poll = 500 * time.Millisecond // statusGrace (2s) spans 4 polls on the fake clock
			if err := s.run(ctx); err != nil {
				t.Fatal(err)
			}
			st := s.state()
			if pane.reaps == 0 {
				t.Fatal("a dead pane with no status was never Reaped")
			}
			if st.LastExit == nil || *st.LastExit != c.want || !strings.Contains(st.Detail, c.detail) {
				t.Fatalf("state %+v (last exit %v), want %d and %q", st, derefIntPtr(st.LastExit), c.want, c.detail)
			}
		})
	}
}

func derefIntPtr(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// lostPane starts once; its Exited and Respawn answer with the errors given.
type lostPane struct {
	fakePane
	exitedErr  error // nil: a dead pane with status 1
	respawnErr error
	respawns   int
}

func (p *lostPane) Exited(ctx context.Context, ref paneRef) (paneExit, error) {
	if p.exitedErr != nil {
		return paneExit{}, p.exitedErr
	}
	return p.fakePane.Exited(ctx, ref)
}

func (p *lostPane) Respawn(ctx context.Context, ref paneRef, dir string, argv []string) error {
	p.respawns++
	if p.respawnErr != nil {
		return p.respawnErr
	}
	return p.fakePane.Respawn(ctx, ref, dir, argv)
}

// 🔴 R2-F1 (supervisor half): A PANE THE SUPERVISOR CAN NO LONGER READ AS ITS OWN
// IS NOT `running`. Whether tmux says the pane is on another server
// (errTerminalLost), tmux cannot be read at all, or a respawn finds the pane gone,
// the supervisor latches terminal_lost, names no input pane, and starts nothing
// more — it never keeps reporting `running` over a pane it cannot see.
func TestASupervisorThatLosesItsPaneLatchesTerminalLost(t *testing.T) {
	cases := []struct {
		name                  string
		exitedErr, respawnErr error
		wantRespawns          int
	}{
		{"pane on another server", fmt.Errorf("%w: %%0 now resolves to pane \"%%0\" on tmux server \"7 7\"", errTerminalLost), nil, 0},
		{"tmux not answering", errors.New("tmux display-message: exit status 1: no server running on /tmp/x"), nil, 0},
		{"respawn finds the pane gone", nil, fmt.Errorf("%w: gone", errTerminalLost), 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pane := &lostPane{exitedErr: c.exitedErr, respawnErr: c.respawnErr}
			pane.status = 1
			s, _ := testSupervisor(t, pane, t.TempDir(), "/data/workspace", time.Hour)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := s.run(ctx); err != nil {
				t.Fatal(err)
			}
			if ctx.Err() != nil {
				t.Fatalf("run kept going until the test's deadline: %+v", s.state())
			}
			st := s.state()
			if st.CLI != "terminal_lost" || st.Starts != 1 || pane.respawns != c.wantRespawns ||
				!strings.Contains(st.Detail, "Kubernetes restarts the pod") {
				t.Fatalf("state %+v, %d respawns", st, pane.respawns)
			}
			if p, ok := s.inputPane(); ok {
				t.Fatalf("terminal_lost still names input pane %v", p)
			}
		})
	}
}

// 🔴 R2-F2: THE env/apiKeyHelper STRIP RUNS BEFORE EVERY START, not only when the
// pod starts. A settings.json `env` block (and `apiKeyHelper`) written before the
// first start, and again between starts, is gone by the time each CLI starts;
// every other key is kept.
func TestEveryCLIStartIsPrecededByTheSettingsStrip(t *testing.T) {
	cfg := t.TempDir()
	path := filepath.Join(cfg, "settings.json")
	plant := func() {
		b := `{"env":{"ANTHROPIC_BASE_URL":"https://planted.invalid"},"apiKeyHelper":"/bin/echo planted","hooks":{"Stop":[]},"theme":"kept"}`
		if err := os.WriteFile(path, []byte(b), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	plant()
	pane := &fakePane{}
	s, _ := testSupervisor(t, pane, cfg, "/data/workspace", time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	var seen []string
	pane.onStart = func(n int) {
		// What the n-th CLI would read as it starts.
		b, err := os.ReadFile(path)
		if err != nil {
			t.Error(err)
		}
		seen = append(seen, string(b))
		plant() // the session writes them again while it runs
		if n == 3 {
			cancel()
		}
	}
	if err := s.run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 3 {
		t.Fatalf("%d starts, want 3", len(seen))
	}
	for i, b := range seen {
		var m map[string]any
		if err := json.Unmarshal([]byte(b), &m); err != nil {
			t.Fatalf("start %d: settings.json %q: %v", i+1, b, err)
		}
		if _, ok := m["env"]; ok {
			t.Errorf("start %d: the CLI started with an env block in settings.json: %s", i+1, b)
		}
		if _, ok := m["apiKeyHelper"]; ok {
			t.Errorf("start %d: the CLI started with apiKeyHelper in settings.json: %s", i+1, b)
		}
		if m["theme"] != "kept" || m["hooks"] == nil {
			t.Errorf("start %d: other keys were not kept: %s", i+1, b)
		}
	}
}

// The strip refuses a settings.json that is not a JSON object rather than moving
// it aside (which would leave the CLI without its hook template): the first start
// is an error, a restart is not attempted.
func TestTheSupervisorDoesNotStartACLIOverACorruptSettingsFile(t *testing.T) {
	cfg := t.TempDir()
	if err := os.WriteFile(filepath.Join(cfg, "settings.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pane := &fakePane{onStart: func(int) { cancel() }} // a start is already the failure; end the run there
	s, _ := testSupervisor(t, pane, cfg, "/data/workspace", time.Hour)
	if err := s.run(ctx); err == nil || len(pane.starts) != 0 {
		t.Fatalf("run over a corrupt settings.json: err %v, %d starts", err, len(pane.starts))
	}
	if b, _ := os.ReadFile(filepath.Join(cfg, "settings.json")); string(b) != "not json" {
		t.Fatalf("the corrupt file was changed: %q", b)
	}
}

// A restart that failed once and then succeeded is history: a later crash loop
// of the CLI itself must not still blame it.
func TestASucceededRestartClearsTheEarlierRestartFailure(t *testing.T) {
	pane := &fakePane{status: 4, failN: 1}
	s, _ := testSupervisor(t, pane, t.TempDir(), "/data/workspace", 0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.run(ctx); err != nil {
		t.Fatal(err)
	}
	st := s.state()
	if st.CLI != cliCrashLoop || st.Starts < 2 {
		t.Fatalf("state %+v (the failed respawn must have been followed by a successful one)", st)
	}
	if strings.Contains(st.Detail, restartNotDone) {
		t.Fatalf("a crash loop after a successful restart still blames the old failure: %q", st.Detail)
	}
}

// 🔴 TestTheCLIsPIDWaitsForItsFirstStart: a SessionStart hook can reach ccd
// before tmux new-session has returned the pane (the CLI runs, and hooks, before
// tmux prints the id). cliPID must wait for that start, not read "no pane yet" —
// which left the primary's session id unrecorded and was
// TestTmuxLaneTwoSessionsRunInTheirOwnCLIsOnTheirOwnModels' CI flake. Red when
// cliPID reads ref() directly.
func TestTheCLIsPIDWaitsForItsFirstStart(t *testing.T) {
	sup := newSupervisor(nil, "claude", t.TempDir(), "/w")
	pane := paneRef{Server: "4242 1791600000", Pane: "%7"}
	panePID := func(_ context.Context, ref paneRef) (int, error) {
		if ref != pane {
			return 0, fmt.Errorf("pane %+v, want %+v", ref, pane)
		}
		return 4343, nil
	}

	// No start yet: the caller's deadline ends the wait.
	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if pid, err := sup.cliPID(short, panePID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("before any start: pid %d, err %v; want the caller's deadline", pid, err)
	}

	// The hook asks first; the start lands while it waits.
	type answer struct {
		pid int
		err error
	}
	got := make(chan answer, 1)
	ctx, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	go func() {
		pid, err := sup.cliPID(ctx, panePID)
		got <- answer{pid, err}
	}()
	select {
	case a := <-got:
		t.Fatalf("answered before the CLI's first start: pid %d, err %v", a.pid, a.err)
	case <-time.After(200 * time.Millisecond):
	}
	sup.started(pane, modeFresh)
	if a := <-got; a.err != nil || a.pid != 4343 {
		t.Fatalf("after the start: pid %d, err %v; want 4343", a.pid, a.err)
	}
	// Once started, startedRef answers at once, even on a spent deadline: the
	// started check is not a coin-flip against ctx.Done(). Many asks, since one
	// select over two ready cases picks each half the time. (This panePID
	// ignores ctx; the real one, tmuxTerminal.PanePID, can fail on a spent one.)
	if pid, err := sup.cliPID(context.Background(), panePID); err != nil || pid != 4343 {
		t.Fatalf("a later ask: pid %d, err %v", pid, err)
	}
	spent, cancel3 := context.WithCancel(context.Background())
	cancel3()
	for i := range 200 {
		if pid, err := sup.cliPID(spent, panePID); err != nil || pid != 4343 {
			t.Fatalf("ask %d on a cancelled context after the start: pid %d, err %v; want 4343", i, pid, err)
		}
	}
}
