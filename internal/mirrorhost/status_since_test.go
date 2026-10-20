package mirrorhost

import "testing"

// THE STATUS'S TIME (ClaudePane.StatusSince): the send queue reads a window as
// idle SINCE Claude Code's own statusUpdatedAt, so each way a pane is resolved
// must carry the time of the SAME sessions file its status came from — never
// the pane CLI's stale file's when the pane shows another session. The values
// are pairwise distinct so a path that reads the wrong file shows it.
const (
	at400 = int64(1792000000400) // pid 400's own sessions file (sess-a-stale)
	at500 = int64(1792000000500) // pid 500's (sess-c-other)
	at600 = int64(1792000000600) // pid 600's: the session %46 really shows (sess-b-live)
)

func statusShape(t *testing.T, parked bool) *liveShape {
	s := newLiveShape(t)
	own := map[string]any{"sessionId": "sess-a-stale", "cwd": fxCwd, "status": "busy", "statusUpdatedAt": at400,
		"procStart": "8424894", "kind": "interactive"}
	if parked {
		own["parkedJobId"] = "b32ad683"
	}
	s.cfg.rawSession(400, own)
	s.cfg.rawSession(500, map[string]any{"sessionId": "sess-c-other", "cwd": fxCwd, "status": "busy", "statusUpdatedAt": at500,
		"procStart": "7000002", "kind": "interactive"})
	s.proc.addStarted(600, 1, ".claude-wrapped", fxCwd, "8455239", "claude", "--resume")
	s.cfg.rawSession(600, map[string]any{"sessionId": "sess-b-live", "cwd": fxCwd, "status": "idle", "statusUpdatedAt": at600,
		"procStart": "8455239", "kind": "bg", "jobId": "b32ad683"})
	return s
}

func TestThePaneStatusCarriesTheTimeOfTheSessionsFileItCameFrom(t *testing.T) {
	t.Run("the pane CLI's own sessions file", func(t *testing.T) {
		s := statusShape(t, false)
		got := s.loc("").LocatePanes(s.panes)
		if a := got["%46"]; a.Gen != "sess-a-stale" || a.Status != "busy" || a.StatusSince != at400 {
			t.Fatalf("%%46 = %+v, want sess-a-stale busy since %d", a, at400)
		}
		if b := got["%47"]; b.Gen != "sess-c-other" || b.StatusSince != at500 {
			t.Fatalf("%%47 = %+v, want since %d", b, at500)
		}
	})
	t.Run("a parked background job", func(t *testing.T) {
		s := statusShape(t, true)
		if a := s.loc("").LocatePanes(s.panes)["%46"]; a.Gen != "sess-b-live" || a.Status != "idle" || a.StatusSince != at600 {
			t.Fatalf("%%46 = %+v, want the job's sess-b-live idle since %d (not the CLI's %d)", a, at600, at400)
		}
	})
	t.Run("the pane map", func(t *testing.T) {
		s := statusShape(t, false)
		writeMap(t, s.mapDir, "%46", s.goodEntry(nil))
		if a := s.loc(s.mapDir).LocatePanes(s.panes)["%46"]; a.Gen != "sess-b-live" || a.Source != SourcePaneMap ||
			a.Status != "idle" || a.StatusSince != at600 {
			t.Fatalf("%%46 = %+v, want the mapped sess-b-live idle since %d (not the CLI's %d)", a, at600, at400)
		}
	})
}

// TestNoStatusNoTime: a time without a status this side knows, and a
// status without a time (a Claude Code whose sessions file has no
// statusUpdatedAt), each carry NO time — which the send queue reads as
// unknown, never as idle since some guess. The control is the same file with
// both. "shell" (idle, a background shell running) keeps its time: it is idle
// since then.
func TestNoStatusNoTime(t *testing.T) {
	for _, c := range []struct {
		name string
		body map[string]any
		want int64
	}{
		{"both (control)", map[string]any{"status": "idle", "statusUpdatedAt": at400}, at400},
		{"shell (idle with a background shell)", map[string]any{"status": "shell", "statusUpdatedAt": at400}, at400},
		{"an unknown status", map[string]any{"status": "compacting", "statusUpdatedAt": at400}, 0},
		{"no statusUpdatedAt", map[string]any{"status": "idle"}, 0},
		{"a negative one", map[string]any{"status": "idle", "statusUpdatedAt": -5}, 0},
	} {
		s := newLiveShape(t)
		body := map[string]any{"sessionId": "sess-a-stale", "cwd": fxCwd, "procStart": "8424894", "kind": "interactive"}
		for k, v := range c.body {
			body[k] = v
		}
		s.cfg.rawSession(400, body)
		if a := s.loc("").LocatePanes(s.panes)["%46"]; a.Gen != "sess-a-stale" || a.StatusSince != c.want {
			t.Errorf("%s: %%46 = %+v, want StatusSince %d", c.name, a, c.want)
		}
	}
}
