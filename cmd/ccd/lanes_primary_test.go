package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// primaryLaneRig is a supervised primary whose lanes are wired the way main.go wires
// them (server.runLanes), each CLI on a killablePane of its own.
type primaryLaneRig struct {
	s        *server
	sup      *supervisor
	primary  *killablePane
	lanePane *killablePane
	l        *lanes
}

func newPrimaryLaneRig(t *testing.T) *primaryLaneRig {
	t.Helper()
	cfg := t.TempDir()
	r := &primaryLaneRig{primary: newKillablePane(), lanePane: newKillablePane()}
	r.s = newServer(serverConfig{Bearer: "b", ConfigDir: cfg, SubmitTimeout: 200 * time.Millisecond}, r.primary, newAuthTracker())
	r.sup = newSupervisor(r.primary, "claude", cfg, "/data/workspace")
	fastRestarts(r.sup)
	r.s.supervisePrimary(r.sup, noPrimaryPID)
	r.l = testLanes(t, 2)
	r.l.startTimeout = 300 * time.Millisecond
	r.l.newTerm = func(string) laneTerminal { return r.lanePane }
	r.l.newServer = func(term terminal) *server {
		return newServer(serverConfig{Bearer: "b", ConfigDir: cfg, SubmitTimeout: 200 * time.Millisecond}, term, newAuthTracker())
	}
	r.l.tweak = fastRestarts
	r.s.runLanes(r.l)
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan struct{})
	go func() { defer close(ran); _ = r.sup.run(ctx) }()
	t.Cleanup(func() { cancel(); <-ran })
	waitUntil(t, "the primary's first start", func() bool { _, ok := r.sup.inputPane(); return ok })
	return r
}

func (r *primaryLaneRig) laneStarts() int {
	r.lanePane.mu.Lock()
	defer r.lanePane.mu.Unlock()
	return r.lanePane.starts
}

// 🟢 cliExited clears busy too. A lane whose CLI is killed mid-turn — the
// prompt submitted, no Stop, and no ccd turn holding it (typed in its terminal,
// or the ccd turn already gave up) — is idle while its CLI restarts, so a new
// session at the cap may stop it (its conversation is on disk) and /healthz
// does not report a dead CLI as busy. Without the clear the lane stayed busy
// until the NEW CLI's SessionStart, refusing the new session 503 capacity.
func TestALaneKilledMidTurnIsEvictableWhileItRestarts(t *testing.T) {
	pane := newKillablePane()
	l := testLanes(t, 1)
	l.newTerm = func(string) laneTerminal { return pane }
	l.tweak = fastRestarts
	ln, f := l.acquire(context.Background(), "sess-a", "")
	if f != nil {
		t.Fatal(f)
	}
	t.Cleanup(ln.stop)
	ln.srv.onHook(hookEvent{Event: "SessionStart", SessionID: laneUUID("sess-a")})
	ln.srv.onHook(hookEvent{Event: "UserPromptSubmit", Prompt: "typed in the terminal"})
	l.release(ln)
	if ln.srv.idle() {
		t.Fatal("control: a lane with a submitted prompt and no Stop is not idle")
	}

	pane.kill()
	select {
	case <-pane.respawning:
	case <-time.After(5 * time.Second):
		t.Fatal("the supervisor never respawned the killed lane CLI")
	}
	waitUntil(t, "the killed lane CLI's exit recorded", func() bool { return ln.sup.state().Starts == 1 && !ln.srv.started() })
	if _, _, busy := ln.srv.snapshot(); busy || !ln.srv.idle() {
		t.Fatalf("BUSY AFTER DEATH: a lane killed mid-turn, held in its respawn: busy=%v idle=%v; want not busy and idle", busy, ln.srv.idle())
	}
	l.mu.Lock()
	evict, stuck := l.pickEvictLocked()
	l.mu.Unlock()
	if evict != ln || stuck {
		t.Fatalf("a lane killed mid-turn is not chosen for eviction while it restarts (got %v, stuck=%v)", evict, stuck)
	}
	close(pane.gate)
}

// 🔴 THE PRIMARY'S RESPAWN NEVER TAKES A LANE'S CONVERSATION. The failure
// sequence (#157 audit): the primary's SessionStart arrived but its id was
// never recorded (notFiredByPrimary's pid lookup timed out, or the ancestry
// check missed — modelled here by noPrimaryPID: no record file); a lane is
// running but has written no transcript yet; the primary's CLI is SIGKILLed.
// The respawn's command line is chosen NOW, while the only transcript on disk
// is the primary's — so the on-disk "any lane transcript?" check sees none. A
// bare `--continue` is resolved by the CLI later, as it boots, and if the lane
// writes its first transcript in that window, "the newest" is the lane's: the
// primary would resume the LANE's conversation. The respawn must name the
// primary's own conversation by id.
func TestThePrimaryRespawnDoesNotContinueALaneThatWritesDuringItsBoot(t *testing.T) {
	r := newPrimaryLaneRig(t)
	const primary = "0f0e0d0c-0b0a-4908-8706-050403020100"
	dir := projectDir(r.sup.configDir, r.sup.workspace)
	writeAt := func(id string, at time.Time) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, id+".jsonl")
		if err := os.WriteFile(p, []byte(cliTranscript), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
	}
	// The primary's first prompt wrote its transcript; its SessionStart recorded
	// nothing.
	writeAt(primary, time.Now().Add(-time.Hour))
	r.s.onHook(hookEvent{Event: "SessionStart", SessionID: primary})
	if _, err := os.Stat(filepath.Join(r.sup.configDir, primaryRecordFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("control: the primary's id must be unrecorded here (err=%v)", err)
	}

	ln, f := r.l.acquire(context.Background(), "sess-a", "")
	if f != nil {
		t.Fatalf("control: the lane starts: %+v", f)
	}
	t.Cleanup(ln.stop)
	r.l.release(ln)
	waitUntil(t, "the lane's CLI started", func() bool { return r.laneStarts() == 1 })
	if transcriptExists(r.sup.configDir, r.sup.workspace, laneUUID("sess-a")) {
		t.Fatal("control: the lane must have no transcript yet")
	}

	r.primary.kill()
	select {
	case <-r.primary.respawning:
	case <-time.After(5 * time.Second):
		t.Fatal("the supervisor never respawned the killed primary")
	}
	r.primary.mu.Lock()
	argv := r.primary.argvs[len(r.primary.argvs)-1]
	r.primary.mu.Unlock()
	// The lane's first prompt, while the primary's CLI boots.
	writeAt(laneUUID("sess-a"), time.Now().Add(time.Minute))
	close(r.primary.gate)

	for _, a := range argv {
		if a == "--continue" {
			t.Errorf("BARE --continue WITH A LANE RUNNING: the primary's respawn ran %q", argv)
		}
	}
	// What the CLI resumes, read the way the fake CLI reads it as it starts.
	var got string
	switch {
	case len(argv) == 3 && argv[1] == "--resume":
		got = argv[2]
	case len(argv) == 2 && argv[1] == "--continue":
		got = fakeContinueTarget(dir)
	}
	if got != primary {
		t.Fatalf("THE PRIMARY RESUMED %q (argv %q), want its own conversation %s (the lane's is %s)",
			got, argv, primary, laneUUID("sess-a"))
	}
}

// 🔴 A LANE IS NOT HOSTAGE TO A PRIMARY THAT NEVER STARTS. A lane turn is
// served while the primary has not sent SessionStart in this process at all:
// its CLI is up but never reaches its prompt (no SessionStart ever comes), or
// it is down and held in its respawn (a crash loop). (A primary that is not
// logged in is NOT this case for long: lanes share its config dir, so a lane is
// not logged in either.) The primary resumes by id, never a
// bare --continue (supervisor.argv), so there is nothing for a lane started
// first to take from it — and muster's interactive chat does not re-send a 503
// not_ready, so refusing the turn loses the user's message.
func TestALaneTurnIsServedWhileThePrimaryHasNeverStarted(t *testing.T) {
	for _, c := range []struct {
		name string
		down func(r *primaryLaneRig)
	}{
		{"never reaches its prompt", func(*primaryLaneRig) {}},
		{"crash-looping", func(r *primaryLaneRig) {
			r.primary.kill()
			select {
			case <-r.primary.respawning:
			case <-time.After(5 * time.Second):
				t.Fatal("the supervisor never respawned the killed primary")
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newPrimaryLaneRig(t)
			r.l.startTimeout = 5 * time.Second
			c.down(r)
			if r.s.started() {
				t.Fatal("control: the primary has never sent SessionStart")
			}
			type res struct {
				code int
				body string
			}
			done := make(chan res, 1)
			go func() { code, body := turnStatus(t, r.s, "sess-a"); done <- res{code, body} }()
			// The lane's CLI starts for this turn and sends its SessionStart.
			var ln *lane
			deadline := time.Now().Add(5 * time.Second)
			for ln == nil && time.Now().Before(deadline) {
				select {
				case got := <-done:
					t.Fatalf("LANE HOSTAGE TO A PRIMARY THAT NEVER STARTED (%s): status %d, %d lane start(s), body %s; "+
						"want the lane started and the turn pasted into it", c.name, got.code, r.laneStarts(), got.body)
				default:
				}
				ln = r.l.get(laneID("sess-a"))
				time.Sleep(5 * time.Millisecond)
			}
			if ln == nil {
				t.Fatal("the lane was never started")
			}
			t.Cleanup(ln.stop)
			waitUntil(t, "the lane's CLI started", func() bool { return r.laneStarts() == 1 })
			ln.srv.onHook(hookEvent{Event: "SessionStart", SessionID: laneUUID("sess-a")})
			got := <-done
			if got.code != http.StatusGatewayTimeout || r.lanePane.pasted() != 1 {
				t.Fatalf("LANE HOSTAGE TO A PRIMARY THAT NEVER STARTED (%s): status %d, %d paste(s) into the lane, body %s; "+
					"want the turn pasted into the lane (504 unsubmitted here)", c.name, got.code, r.lanePane.pasted(), got.body)
			}
			if c.name == "crash-looping" {
				close(r.primary.gate)
			}
		})
	}
}
