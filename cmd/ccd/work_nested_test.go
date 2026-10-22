package main

import (
	"context"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/cctasks"
)

// nestedRig is a supervised PRIMARY CLI (pid 100, its pane's process) busy
// with the shared work fixture's 2 subagents.
func nestedRig(t *testing.T) (*server, *podCounter, string) {
	t.Helper()
	config := t.TempDir()
	root := fakeProcFS(t, map[int]fakeProcess{100: cliProc(1, "777")})
	sup := newSupervisor(&stillPane{}, "claude", config, "/data/workspace")
	sup.started(paneRef{Server: "1 1", Pane: "%1"}, modeFresh)
	s := newServer(serverConfig{ConfigDir: config}, nil, newAuthTracker())
	s.supervisePrimary(sup, func(context.Context, paneRef) (int, error) { return 100, nil })
	tr := projectTranscript(t, config, fixtureSession)
	deliver(t, s, hookFixture(t, "hook_SessionStart_2.1.296.json", map[string]string{"session_id": q(fixtureSession), "transcript_path": q(tr)}))
	sessionFile(t, config, 100, fixtureSession, cctasks.StatusBusy, "777")
	c := &podCounter{configDir: config, root: root, now: func() time.Time { return workFixtureBase.Add(100 * time.Second) }}
	return s, c, config
}

// TestANestedClaudeDoesNotTakeOverThePrimarysCount: a `claude` a tool call runs
// inside the primary inherits its hook settings, so ITS SessionStart — and,
// when it exits, its SessionEnd — reach the primary's server (proctree.go).
// The card still counts the PRIMARY's work: 2 subagents, read through the
// primary's own process and sessions file, not through the session the last
// hook named. The nested CLI's sessions file is a dead one (pid 300 runs no
// process).
//
// Red at 2ea86c7d: the count followed the last hook's session, so it read
// nothing ({0 0} ok=false) after either hook.
func TestANestedClaudeDoesNotTakeOverThePrimarysCount(t *testing.T) {
	const nested = "99999999-2222-4333-8444-555555555555"
	for _, ev := range []string{"SessionStart", "SessionEnd"} {
		t.Run(ev, func(t *testing.T) {
			s, c, config := nestedRig(t)
			if w, ok := c.work(context.Background(), []*server{s}); !ok || w != (cctasks.Work{Subagents: 2}) {
				t.Fatalf("control: the primary counts %+v (ok=%v), want 2 subagents", w, ok)
			}
			sessionFile(t, config, 300, nested, cctasks.StatusIdle, "1")
			deliver(t, s, hookFixture(t, "hook_SessionStart_2.1.296.json", map[string]string{
				"hook_event_name": q(ev), "session_id": q(nested),
				"transcript_path": q(config + "/projects/-data-workspace-sub/" + nested + ".jsonl")}))
			if w, ok := c.work(context.Background(), []*server{s}); !ok || w != (cctasks.Work{Subagents: 2}) {
				t.Fatalf("after a nested CLI's %s the primary counts %+v (ok=%v), want 2 subagents", ev, w, ok)
			}
		})
	}
}

// pidPane is a lane terminal that names its pane's process.
type pidPane struct {
	stillPane
	pid int
}

func (p *pidPane) PanePID(context.Context, paneRef) (int, error) { return p.pid, nil }

// TestALaneIsCountedByItsOwnPanesProcess: a lane's server is wired to its own
// pane's process (newLane), so a nested CLI inside a lane — which inherits
// CCD_LANE — cannot take its count over either; before its first start it has
// none.
func TestALaneIsCountedByItsOwnPanesProcess(t *testing.T) {
	l := &lanes{newTerm: func(string) laneTerminal { return &pidPane{pid: 4242} },
		newServer: func(term terminal) *server { return newServer(serverConfig{}, term, newAuthTracker()) },
		bin: "claude", configDir: t.TempDir(), workspace: "/data/workspace", max: 1, now: time.Now, set: map[string]*lane{}}
	ln := l.newLane("lane-1", "key-1", "", "")
	if ln.srv.cliPID == nil {
		t.Fatal("a lane's server has no process to be counted by")
	}
	if pid, err := ln.srv.cliPID(context.Background()); err == nil {
		t.Fatalf("before its first start a lane named process %d", pid)
	}
	ln.sup.started(paneRef{Server: "1 1", Pane: "%2"}, modeFresh)
	if pid, err := ln.srv.cliPID(context.Background()); err != nil || pid != 4242 {
		t.Fatalf("a started lane names process %d (%v), want its pane's, 4242", pid, err)
	}
}
