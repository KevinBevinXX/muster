package mirrorhost

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/mirror"
)

// THE PANE MAP (ClaudeLocator.PaneMapDir): resolution order, the validation
// that keeps a stale entry from naming the wrong conversation, and the
// rendered stream following an entry that changes.
//
// Fixture pids, start times and ids are PAIRWISE DISTINCT, so a mutant that
// compares the wrong process's field cannot pass by coincidence.

const (
	fxServer = 9000 // the tmux server: every pane root's parent
	fxCwd    = "/work/repo"
)

// addStarted is fakeProc.add with a chosen start time (stat field 22).
func (f *fakeProc) addStarted(pid, ppid int, comm, cwd, start string, argv ...string) {
	f.t.Helper()
	f.add(pid, ppid, comm, cwd, argv...)
	stat := fmt.Sprintf("%d (%s) S %d 1 1 0 -1 4194560 0 0 0 0 0 0 0 0 20 0 1 0 %s 0 0", pid, comm, ppid, start)
	must(f.t, os.WriteFile(filepath.Join(f.root, fmt.Sprint(pid), "stat"), []byte(stat), 0o644))
}

// rawSession writes sessions/<pid>.json verbatim.
func (h *fakeHome) rawSession(pid int, body map[string]any) {
	h.t.Helper()
	body["pid"] = pid
	b, _ := json.Marshal(body)
	must(h.t, os.MkdirAll(filepath.Join(h.root, "sessions"), 0o755))
	must(h.t, os.WriteFile(filepath.Join(h.root, "sessions", fmt.Sprintf("%d.json", pid)), b, 0o644))
}

// writeMap writes the pane map file for pane, the record a hook writes, with
// overrides applied (a nil value deletes the key).
func writeMap(t *testing.T, dir, pane string, rec map[string]any) {
	t.Helper()
	must(t, os.MkdirAll(dir, 0o755))
	b, _ := json.Marshal(rec)
	must(t, os.WriteFile(filepath.Join(dir, "claude-p"+strings.TrimPrefix(pane, "%")+".json"), append(b, '\n'), 0o644))
}

func mapRec(pane, session, transcript string, claudePID int, start string, over map[string]any) map[string]any {
	r := map[string]any{
		"schema": 1, "runtime": "claude", "pane_id": pane, "window_id": "@" + strings.TrimPrefix(pane, "%"),
		"tmux_pid": fmt.Sprint(fxServer), "session_id": session, "transcript_path": transcript,
		"claude_pid": fmt.Sprint(claudePID), "claude_start": start, "last_activity_ts": "2000-01-01T00:00:00Z",
	}
	for k, v := range over {
		if v == nil {
			delete(r, k)
		} else {
			r[k] = v
		}
	}
	return r
}

// liveShape is the MEASURED live shape: pane %46's CLI (400) still has
// sessions/400.json naming the session it started with (sess-a-stale), while
// the conversation it shows is another (sess-b-live). A second Claude pane,
// %47, runs in the SAME cwd.
type liveShape struct {
	proc               *fakeProc
	cfg                *fakeHome
	mapDir             string
	stale, live, other string
	panes              []pane
}

func newLiveShape(t *testing.T) *liveShape {
	s := &liveShape{proc: newFakeProc(t), cfg: newFakeHome(t), mapDir: filepath.Join(t.TempDir(), "ledger")}
	s.proc.addStarted(fxServer, 1, "tmux: server", "/", "100001", "tmux")
	s.proc.addStarted(100, fxServer, "zsh", fxCwd, "100002", "-zsh")
	s.proc.addStarted(400, 100, ".claude-wrapped", fxCwd, "8424894", "claude")
	s.proc.addStarted(200, fxServer, "zsh", fxCwd, "100003", "-zsh")
	s.proc.addStarted(500, 200, ".claude-wrapped", fxCwd, "7000002", "claude")
	now := time.Now()
	s.stale = s.cfg.transcript(fxCwd, "sess-a-stale", now.Add(-3*time.Hour))
	s.live = s.cfg.transcript(fxCwd, "sess-b-live", now.Add(-time.Minute))
	s.other = s.cfg.transcript(fxCwd, "sess-c-other", now) // the NEWEST in the cwd
	s.cfg.rawSession(400, map[string]any{"sessionId": "sess-a-stale", "cwd": fxCwd, "status": "idle", "procStart": "8424894", "kind": "interactive"})
	s.cfg.rawSession(500, map[string]any{"sessionId": "sess-c-other", "cwd": fxCwd, "status": "busy", "procStart": "7000002", "kind": "interactive"})
	s.panes = []pane{{Session: "scratch13", ID: "%46", PID: 100}, {Session: "scratch13", ID: "%47", PID: 200}}
	return s
}

func (s *liveShape) loc(mapDir string) ClaudeLocator {
	return ClaudeLocator{Proc: s.proc.root, Home: s.cfg.root, PaneMapDir: mapDir}
}

func (s *liveShape) goodEntry(over map[string]any) map[string]any {
	return mapRec("%46", "sess-b-live", s.live, 400, "8424894", over)
}

// TestPaneMapWinsOverAStaleSessionsFile is THE live shape: the sessions file
// says A, the pane map says B for the same pane and the same process → B.
// Positive control in the same test: with the map OFF the same tree resolves
// to A, so the map — and nothing else — is what moved the answer.
func TestPaneMapWinsOverAStaleSessionsFile(t *testing.T) {
	s := newLiveShape(t)
	writeMap(t, s.mapDir, "%46", s.goodEntry(nil))

	off := s.loc("").LocatePanes(s.panes)["%46"]
	if off.Gen != "sess-a-stale" || off.Source != SourceSessions {
		t.Fatalf("control: with no pane map %%46 = %+v, want the sessions file's sess-a-stale", off)
	}
	on := s.loc(s.mapDir).LocatePanes(s.panes)
	got := on["%46"]
	if got.Gen != "sess-b-live" || got.Transcript != s.live || got.Source != SourcePaneMap || got.PID != 400 || got.Pane != "%46" {
		t.Fatalf("%%46 = %+v, want the pane map's sess-b-live %s from pid 400", got, s.live)
	}
	// The other Claude pane in the SAME cwd has no map entry: its own sessions
	// file, never %46's conversation.
	if o := on["%47"]; o.Gen != "sess-c-other" || o.Transcript != s.other || o.Source != SourceSessions || !o.Busy {
		t.Fatalf("%%47 = %+v, want its own sess-c-other (busy)", o)
	}
}

// TestPaneMapBusyComesFromTheMappedSessionsLiveFile: the busy flag is the
// MAPPED session's, not the pane CLI's stale file's.
func TestPaneMapBusyComesFromTheMappedSessionsLiveFile(t *testing.T) {
	s := newLiveShape(t)
	writeMap(t, s.mapDir, "%46", s.goodEntry(nil))
	s.proc.addStarted(600, 1, ".claude-wrapped", fxCwd, "8455239", "claude", "--resume")
	s.cfg.rawSession(600, map[string]any{"sessionId": "sess-b-live", "cwd": fxCwd, "status": "busy", "procStart": "8455239", "kind": "bg", "jobId": "b32ad683"})
	if got := s.loc(s.mapDir).LocatePanes(s.panes)["%46"]; !got.Busy || got.Gen != "sess-b-live" {
		t.Fatalf("%%46 = %+v, want sess-b-live busy (its live file says busy; the CLI's says idle)", got)
	}
}

// TestAStaleOrForeignPaneMapEntryFallsThrough: each clause of
// paneMapEntry.valid, broken ALONE on an otherwise-good entry, sends the pane to
// the next source (the sessions file's sess-a-stale) — never to the entry's
// session and never to nothing.
func TestAStaleOrForeignPaneMapEntryFallsThrough(t *testing.T) {
	for name, over := range map[string]map[string]any{
		"the claude pid is dead (not in /proc)":               {"claude_pid": "4999"},
		"the claude pid is not under this pane (it is %47's)": {"claude_pid": "500", "claude_start": "7000002"},
		"the pid was reused: start time differs":              {"claude_start": "8424895"},
		"no start time recorded (a pre-binding writer)":       {"claude_start": nil},
		"no claude pid recorded (a pre-binding writer)":       {"claude_pid": nil},
		"another tmux server wrote it":                        {"tmux_pid": "9001"},
		"another pane's entry":                                {"pane_id": "%47"},
		"another runtime":                                     {"runtime": "opencode"},
		"the transcript does not exist":                       {"session_id": "sess-gone", "transcript_path": "@P/-work-repo/sess-gone.jsonl"},
		"the transcript is not <session>.jsonl":               {"session_id": "sess-other-id"},
	} {
		t.Run(name, func(t *testing.T) {
			s := newLiveShape(t)
			for k, v := range over { // "@P/": inside <home>/projects, so only ONE clause breaks
				if str, ok := v.(string); ok && strings.HasPrefix(str, "@P/") {
					over[k] = filepath.Join(s.cfg.root, "projects", strings.TrimPrefix(str, "@P/"))
				}
			}
			writeMap(t, s.mapDir, "%46", s.goodEntry(over))
			got := s.loc(s.mapDir).LocatePanes(s.panes)["%46"]
			if got.Gen != "sess-a-stale" || got.Source != SourceSessions {
				t.Fatalf("%%46 = %+v, want a fall-through to the sessions file (sess-a-stale)", got)
			}
		})
	}
}

// TestAPaneMapTranscriptOutsideProjectsIsRefused: the map is data, not a path
// to follow. A file named <session>.jsonl that exists OUTSIDE
// <home>/projects is not streamed.
func TestAPaneMapTranscriptOutsideProjectsIsRefused(t *testing.T) {
	s := newLiveShape(t)
	outside := filepath.Join(t.TempDir(), "sess-b-live.jsonl")
	must(t, os.WriteFile(outside, []byte("{}\n"), 0o644))
	writeMap(t, s.mapDir, "%46", s.goodEntry(map[string]any{"transcript_path": outside}))
	if got := s.loc(s.mapDir).LocatePanes(s.panes)["%46"]; got.Transcript == outside || got.Source != SourceSessions {
		t.Fatalf("%%46 = %+v, an entry pointing outside projects was followed", got)
	}
}

// TestTwoClaudePanesInOneCwdEachGetTheirOwnMappedTranscript: both panes mapped,
// both in one cwd, neither with a usable sessions file — each its own.
func TestTwoClaudePanesInOneCwdEachGetTheirOwnMappedTranscript(t *testing.T) {
	s := newLiveShape(t)
	must(t, os.RemoveAll(filepath.Join(s.cfg.root, "sessions")))
	writeMap(t, s.mapDir, "%46", s.goodEntry(nil))
	writeMap(t, s.mapDir, "%47", mapRec("%47", "sess-a-stale", s.stale, 500, "7000002", nil))
	got := s.loc(s.mapDir).LocatePanes(s.panes)
	if a, b := got["%46"], got["%47"]; a.Gen != "sess-b-live" || b.Gen != "sess-a-stale" || a.Source != SourcePaneMap || b.Source != SourcePaneMap {
		t.Fatalf("got %%46=%+v %%47=%+v, want each pane's own mapped transcript", a, b)
	}
	// Control: with the map off, both GUESS the cwd's newest — the same file —
	// and are both dropped rather than shown one conversation.
	if off := s.loc("").LocatePanes(s.panes); len(off) != 0 {
		t.Fatalf("control: with no map and no sessions files got %+v, want both dropped", off)
	}
}

// TestTwoPanesValidlyMappedToOneTranscriptBothShowIt: the "never share" rule is
// for GUESSES. Two panes whose own processes each prove they show one
// conversation (two CLIs resumed into it) both show it.
func TestTwoPanesValidlyMappedToOneTranscriptBothShowIt(t *testing.T) {
	s := newLiveShape(t)
	writeMap(t, s.mapDir, "%46", s.goodEntry(nil))
	writeMap(t, s.mapDir, "%47", mapRec("%47", "sess-b-live", s.live, 500, "7000002", nil))
	got := s.loc(s.mapDir).LocatePanes(s.panes)
	if a, b := got["%46"], got["%47"]; a.Gen != "sess-b-live" || b.Gen != "sess-b-live" {
		t.Fatalf("got %%46=%+v %%47=%+v, want both to show sess-b-live", a, b)
	}
}

// TestAParkedBackgroundJobIsWhatThePaneShows: without a pane map, a CLI whose
// sessions file parks a job resolves to the job's session (its live file),
// not to its own stale sessionId. Dead or ambiguous jobs are not followed.
func TestAParkedBackgroundJobIsWhatThePaneShows(t *testing.T) {
	setup := func(t *testing.T) *liveShape {
		s := newLiveShape(t)
		s.cfg.rawSession(400, map[string]any{"sessionId": "sess-a-stale", "cwd": fxCwd, "status": "busy", "procStart": "8424894", "kind": "interactive", "parkedJobId": "b32ad683"})
		s.proc.addStarted(600, 1, ".claude-wrapped", fxCwd, "8455239", "claude", "--resume")
		s.cfg.rawSession(600, map[string]any{"sessionId": "sess-b-live", "cwd": fxCwd, "status": "idle", "procStart": "8455239", "kind": "bg", "jobId": "b32ad683"})
		return s
	}
	t.Run("followed", func(t *testing.T) {
		s := setup(t)
		got := s.loc("").LocatePanes(s.panes)["%46"]
		if got.Gen != "sess-b-live" || got.Transcript != s.live || got.Busy || got.Source != SourceSessions {
			t.Fatalf("%%46 = %+v, want the parked job's sess-b-live, idle (the job's status, not the CLI's)", got)
		}
	})
	t.Run("the job is dead (start time differs)", func(t *testing.T) {
		s := setup(t)
		s.cfg.rawSession(600, map[string]any{"sessionId": "sess-b-live", "cwd": fxCwd, "status": "idle", "procStart": "1", "kind": "bg", "jobId": "b32ad683"})
		if got := s.loc("").LocatePanes(s.panes)["%46"]; got.Gen != "sess-a-stale" {
			t.Fatalf("%%46 = %+v, want the CLI's own session: a dead job is not followed", got)
		}
	})
	t.Run("two live jobs claim the id", func(t *testing.T) {
		s := setup(t)
		s.proc.addStarted(601, 1, ".claude-wrapped", fxCwd, "8455240", "claude")
		s.cfg.rawSession(601, map[string]any{"sessionId": "sess-c-other", "cwd": fxCwd, "status": "idle", "procStart": "8455240", "kind": "bg", "jobId": "b32ad683"})
		if got := s.loc("").LocatePanes(s.panes)["%46"]; got.Gen != "sess-a-stale" {
			t.Fatalf("%%46 = %+v, want the CLI's own session: an ambiguous job is not followed", got)
		}
	})
	t.Run("a non-bg file with that jobId is not a job", func(t *testing.T) {
		s := setup(t)
		s.cfg.rawSession(600, map[string]any{"sessionId": "sess-b-live", "cwd": fxCwd, "status": "idle", "procStart": "8455239", "kind": "interactive", "jobId": "b32ad683"})
		if got := s.loc("").LocatePanes(s.panes)["%46"]; got.Gen != "sess-a-stale" {
			t.Fatalf("%%46 = %+v, want the CLI's own session", got)
		}
	})
}

// wsPair is a hostSide whose sends arrive on msgs, over a real websocket.
func wsPair(t *testing.T, backend Backend) (*hostSide, <-chan mirror.Msg) {
	t.Helper()
	msgs := make(chan mirror.Msg, 256)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		for {
			_, b, err := c.Read(r.Context())
			if err != nil {
				return
			}
			var m mirror.Msg
			if json.Unmarshal(b, &m) == nil {
				msgs <- m
			}
		}
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	h := &hostSide{c: &Client{cfg: Config{Backend: backend, ViewPoll: 10 * time.Millisecond}, log: log.New(io.Discard, "", 0)},
		ws: conn, ctx: ctx, version: 2, watches: map[uint32]context.CancelFunc{}}
	return h, msgs
}

// TestARenderedStreamSwitchesWhenThePaneMapEntryChanges: an open view of %46
// is streaming sess-b-live; the hook then records a NEW id for that pane (a
// resume / compact / another /bg). Within a re-resolve the stream starts over
// on the new transcript: a Reset batch stamped with the new Gen — no reopen.
func TestARenderedStreamSwitchesWhenThePaneMapEntryChanges(t *testing.T) {
	s := newLiveShape(t)
	writeMap(t, s.mapDir, "%46", s.goodEntry(nil))
	loc := s.loc(s.mapDir)
	h, msgs := wsPair(t, nil)
	paneFn := func(context.Context) (ClaudePane, bool) {
		p, ok := loc.LocatePanes(s.panes)["%46"]
		return p, ok
	}
	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.tail(ctx, paneFn, mirror.Msg{ID: 7}) }()

	next := func(what string, ok func(mirror.Msg) bool) mirror.Msg {
		t.Helper()
		deadline := time.After(10 * time.Second)
		for {
			select {
			case m := <-msgs:
				if ok(m) {
					return m
				}
			case err := <-done:
				t.Fatalf("waiting for %s: the stream ended: %v", what, err)
			case <-deadline:
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}
	first := next("the first batch", func(m mirror.Msg) bool { return m.ID == 7 })
	if first.Gen != "sess-b-live" || !first.Reset {
		t.Fatalf("first batch = %+v, want a Reset of sess-b-live", first)
	}
	newer := s.cfg.transcript(fxCwd, "sess-d-newer", time.Now())
	writeMap(t, s.mapDir, "%46", s.goodEntry(map[string]any{"session_id": "sess-d-newer", "transcript_path": newer}))
	sw := next("the switch to sess-d-newer", func(m mirror.Msg) bool { return m.ID == 7 && m.Gen == "sess-d-newer" })
	if !sw.Reset {
		t.Fatalf("the first sess-d-newer batch = %+v, want a Reset (the view starts over)", sw)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("tail after cancel: %v", err)
	}
}
