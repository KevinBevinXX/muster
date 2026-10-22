package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/cctasks"
	"github.com/ZacxDev/muster/internal/mirror"
)

const fixtureSession = "11111111-2222-4333-8444-555555555555"

// hookFixture is a recorded hook payload with fields replaced: name -> raw
// JSON value.
func hookFixture(t *testing.T, name string, set map[string]string) []byte {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(fixture(t, name), &m); err != nil {
		t.Fatal(err)
	}
	for k, v := range set {
		m[k] = json.RawMessage(v)
	}
	b, _ := json.Marshal(m)
	return b
}

// deliver sends one payload through ccd's own hook handler (its decoding
// included), as `ccd hook <Event>` does.
func deliver(t *testing.T, s *server, body []byte) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/hook/x", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	s.handleHook(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("hook: %d %s", w.Code, w.Body)
	}
}

func q(s string) string { return strconv.Quote(s) }

// The shared work fixture (internal/ccsubagents/testdata/work_parent.jsonl):
// its time 0, the CLI's start 5 s in, and the count every reader of it must
// get — 2 subagents running (see internal/ccsubagents' ledger_test.go).
var (
	workFixtureBase    = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	workFixtureStarted = workFixtureBase.Add(5 * time.Second).UnixMilli()
)

// projectTranscript copies the shared work fixture to the session's
// transcript under config/projects, where the CLI keeps it.
func projectTranscript(t *testing.T, config, session string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "ccsubagents", "testdata", "work_parent.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(config, "projects", "-data-workspace")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, session+".jsonl")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// startedServer is the server of the CLI running as pid, after its (recorded)
// SessionStart named session and transcript.
func startedServer(t *testing.T, config, session, transcript string, pid int) *server {
	t.Helper()
	s := newServer(serverConfig{ConfigDir: config}, nil, newAuthTracker())
	s.cliPID = func(context.Context) (int, error) { return pid, nil }
	deliver(t, s, hookFixture(t, "hook_SessionStart_2.1.296.json", map[string]string{"session_id": q(session), "transcript_path": q(transcript)}))
	return s
}

// fakeProcFS is a /proc with the processes a test names: pid -> ppid, start
// time, argv and fd links.
type fakeProcess struct {
	ppid  int
	start string
	argv  []string
	fds   map[int]string
}

func fakeProcFS(t *testing.T, procs map[int]fakeProcess) string {
	t.Helper()
	root := t.TempDir()
	for pid, p := range procs {
		dir := filepath.Join(root, strconv.Itoa(pid))
		if err := os.MkdirAll(filepath.Join(dir, "fd"), 0o755); err != nil {
			t.Fatal(err)
		}
		// Fields 3.. of a stat line; field 22 is the start time.
		fields := []string{"S", strconv.Itoa(p.ppid)}
		for len(fields) < 19 {
			fields = append(fields, "0")
		}
		fields = append(fields, p.start, "0")
		stat := strconv.Itoa(pid) + " (" + filepath.Base(p.argv[0]) + ") " + strings.Join(fields, " ") + "\n"
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(strings.Join(p.argv, "\x00")+"\x00"), 0o644); err != nil {
			t.Fatal(err)
		}
		for fd, target := range p.fds {
			if err := os.Symlink(target, filepath.Join(dir, "fd", strconv.Itoa(fd))); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

// sessionFile writes <config>/sessions/<pid>.json.
func sessionFile(t *testing.T, config string, pid int, session, status, procStart string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"pid": pid, "sessionId": session, "cwd": "/data/workspace", "startedAt": workFixtureStarted,
		"procStart": procStart, "kind": "interactive", "status": status, "statusUpdatedAt": workFixtureStarted + 1000})
	dir := filepath.Join(config, "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(pid)+".json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

const shellTasks = "/tmp/claude-1000/-data-workspace/" + fixtureSession + "/tasks/"

func cliProc(ppid int, start string) fakeProcess {
	return fakeProcess{ppid: ppid, start: start, argv: []string{"claude"}}
}

func taskProc(ppid int, id string) fakeProcess {
	return fakeProcess{ppid: ppid, start: "800", argv: []string{"/bin/bash", "-c", "sleep 600"},
		fds: map[int]string{1: shellTasks + id + ".output", 2: shellTasks + id + ".output"}}
}

// TestADispatchedCardCountsTheSharedWorkFixtureAsTheMirrorDoes: THE PARITY
// TEST. The shared work fixture, as a busy CLI's transcript in a pod, counts
// what internal/mirrorhost's TestAMirroredWindowCountsTheSharedWorkFixture
// counts on a mirrored window: 2 subagents — the one ledger
// (internal/ccsubagents), through ccd's own caller.
func TestADispatchedCardCountsTheSharedWorkFixtureAsTheMirrorDoes(t *testing.T) {
	config := t.TempDir()
	root := fakeProcFS(t, map[int]fakeProcess{100: cliProc(1, "777")})
	s := startedServer(t, config, fixtureSession, projectTranscript(t, config, fixtureSession), 100)
	sessionFile(t, config, 100, fixtureSession, cctasks.StatusBusy, "777")
	c := &podCounter{configDir: config, root: root, now: func() time.Time { return workFixtureBase.Add(100 * time.Second) }}
	if w, ok := c.work(context.Background(), []*server{s}); !ok || w != (cctasks.Work{Subagents: 2}) {
		t.Fatalf("the shared fixture counts %+v (ok=%v) on a dispatched card, want 2 subagents", w, ok)
	}
}

// TestPodWorkIsTheMirrorsRuleOverEveryCLI: what ccd reports is cctasks.Decide
// — the mirror's rule — over each CLI's RAW status from its own sessions file,
// its process-tree shells and, while busy, the shared ledger over its
// transcript; summed over the primary and its lanes.
func TestPodWorkIsTheMirrorsRuleOverEveryCLI(t *testing.T) {
	const laneSession = "22222222-2222-4333-8444-555555555555"
	root := fakeProcFS(t, map[int]fakeProcess{
		100: cliProc(1, "777"), 101: taskProc(100, "b0000000a"), 102: taskProc(100, "b0000000b"), 103: taskProc(101, "b0000000c"),
		104: {ppid: 100, start: "801", argv: []string{"node", "mcp-server.js"}, fds: map[int]string{1: "socket:[1]", 2: "socket:[2]"}},
		200: cliProc(1, "778"), 201: taskProc(200, "b0000000d"),
	})
	config := t.TempDir()
	primary := startedServer(t, config, fixtureSession, projectTranscript(t, config, fixtureSession), 100)
	lane := startedServer(t, config, laneSession, projectTranscript(t, config, laneSession), 200)
	c := &podCounter{configDir: config, root: root, now: func() time.Time { return workFixtureBase.Add(100 * time.Second) }}
	work := func(servers ...*server) (cctasks.Work, bool) { return c.work(context.Background(), servers) }

	// No sessions file at all: nothing may be claimed.
	if w, ok := work(primary, lane); ok || w != (cctasks.Work{}) {
		t.Fatalf("with no sessions file: %+v ok=%v, want nothing", w, ok)
	}
	// "shell": no subagent (its transcript is NOT read), and the three task
	// ids — a grandchild's included, the MCP server's socket child not — exactly.
	sessionFile(t, config, 100, fixtureSession, cctasks.StatusShell, "777")
	if w, ok := work(primary); !ok || w != (cctasks.Work{Shells: 3}) {
		t.Fatalf("primary at shell: %+v ok=%v, want exactly 3 shells", w, ok)
	}
	if len(c.ledgers) != 0 {
		t.Fatalf("a CLI at shell had its transcript read: %v", c.ledgers)
	}
	// The lane, busy: its 2 running subagents and a shell that may be a
	// foreground call; the pod's sum is "2 subagents · 4+ shells".
	sessionFile(t, config, 200, laneSession, cctasks.StatusBusy, "778")
	if w, _ := work(lane); w != (cctasks.Work{Subagents: 2, Shells: 1, ShellsAtLeast: true}) {
		t.Fatalf("busy lane: %+v, want 2 subagents and 1+ shell", w)
	}
	if w, _ := work(primary, lane); w != (cctasks.Work{Subagents: 2, Shells: 4, ShellsAtLeast: true}) {
		t.Fatalf("the pod: %+v, want 2 subagents and 4+ shells", w)
	}
	// A sessions file whose process is not the one it names (a reused pid after
	// a restart) is not live: the lane says nothing, and its ledger goes.
	sessionFile(t, config, 200, laneSession, cctasks.StatusBusy, "999")
	if w, ok := work(lane); ok || w != (cctasks.Work{}) {
		t.Fatalf("a stale sessions file counted: %+v ok=%v", w, ok)
	}
	if len(c.ledgers) != 0 {
		t.Fatalf("a session no longer live kept its ledger: %v", c.ledgers)
	}
	// Idle: nothing runs, whatever the transcript still shows.
	sessionFile(t, config, 200, laneSession, cctasks.StatusIdle, "778")
	if w, ok := work(lane); !ok || w != (cctasks.Work{}) {
		t.Fatalf("idle lane: %+v ok=%v, want nothing running", w, ok)
	}
	// The transcript is the one the CLI's OWN sessions file names, under
	// <config>/projects — never one a hook named: a session with no transcript
	// there has no subagents, even when a hook pointed at a real copy of the
	// fixture elsewhere (which would count 2 if it were read).
	const bare = "44444444-2222-4333-8444-555555555555"
	elsewhere := t.TempDir()
	outsideTr := filepath.Join(elsewhere, bare+".jsonl")
	if err := os.Rename(projectTranscript(t, elsewhere, bare), outsideTr); err != nil {
		t.Fatal(err)
	}
	other := startedServer(t, config, bare, outsideTr, 200)
	sessionFile(t, config, 200, bare, cctasks.StatusBusy, "778")
	if w, _ := work(other); w != (cctasks.Work{Shells: 1, ShellsAtLeast: true}) {
		t.Fatalf("a transcript a hook named outside projects was read: %+v", w)
	}
}

// TestTheReporterPostsWorkOnChangeAndHeartbeatsWhileRunning: the body is
// muster's own mirror.WindowWork (decoded with muster's type), sent with the
// agent's own token on a change, re-sent every heartbeat only while something
// runs, and a zero is sent once — the clear.
func TestTheReporterPostsWorkOnChangeAndHeartbeatsWhileRunning(t *testing.T) {
	var mu sync.Mutex
	var got []mirror.WindowWork
	var tokens []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agent/work" || r.Method != http.MethodPost {
			t.Errorf("reported to %s %s", r.Method, r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		var ww mirror.WindowWork
		if err := json.Unmarshal(b, &ww); err != nil {
			t.Errorf("body %s: %v", b, err)
		}
		mu.Lock()
		got, tokens = append(got, ww), append(tokens, r.Header.Get("X-Muster-Token"))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()
	var smu sync.Mutex
	var cur cctasks.Work
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	r := newWorkReporter(ts.URL, "agent-token", func() cctasks.Work { smu.Lock(); defer smu.Unlock(); return cur }, t.Logf)
	r.interval = 2 * time.Millisecond
	r.now = func() time.Time { smu.Lock(); defer smu.Unlock(); return now }
	set := func(w cctasks.Work, adv time.Duration) {
		smu.Lock()
		cur, now = w, now.Add(adv)
		smu.Unlock()
	}
	posts := func() int { mu.Lock(); defer mu.Unlock(); return len(got) }
	// settle waits for n posts, then for several more samples, and checks no
	// other post came.
	settle := func(n int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for posts() < n && time.Now().Before(deadline) {
			time.Sleep(2 * time.Millisecond)
		}
		time.Sleep(40 * time.Millisecond)
		if p := posts(); p != n {
			t.Fatalf("%d posts, want %d: %+v", p, n, got)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.run(ctx)
	settle(0) // nothing running from the start: nothing to clear
	set(cctasks.Work{Subagents: 3, Shells: 2}, 0)
	settle(1) // and unchanged, inside the heartbeat: no second post
	set(cctasks.Work{Subagents: 3, Shells: 2}, workHeartbeat)
	settle(2) // the heartbeat
	set(cctasks.Work{Shells: 1, ShellsAtLeast: true}, time.Second)
	settle(3)
	set(cctasks.Work{}, time.Second)
	settle(4) // the clear
	set(cctasks.Work{}, 3*workHeartbeat)
	settle(4) // nothing running is not re-sent
	want := []mirror.WindowWork{{Subagents: 3, Shells: 2}, {Subagents: 3, Shells: 2}, {Shells: 1, ShellsAtLeast: true}, {}}
	mu.Lock()
	defer mu.Unlock()
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("post %d: %+v, want %+v", i, got[i], want[i])
		}
		if tokens[i] != "agent-token" {
			t.Errorf("post %d sent token %q", i, tokens[i])
		}
	}
}

// TestTheReporterWithdrawsItsReportWhenCcdStops: ccd shutting down (the agent
// being stopped) posts a last, empty report when it had something running —
// best effort, beside muster's own clear on stop.
func TestTheReporterWithdrawsItsReportWhenCcdStops(t *testing.T) {
	var mu sync.Mutex
	var got []mirror.WindowWork
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var ww mirror.WindowWork
		_ = json.Unmarshal(b, &ww)
		mu.Lock()
		got = append(got, ww)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()
	r := newWorkReporter(ts.URL, "agent-token", func() cctasks.Work { return cctasks.Work{Subagents: 3, Shells: 2} }, t.Logf)
	r.interval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	want := []mirror.WindowWork{{Subagents: 3, Shells: 2}, {}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("posts %+v, want %+v (the report, then its withdrawal)", got, want)
	}
}
