package main

import (
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentspec"
)

// TestJobBypassAcceptsExactlyOneValue: unset is the default posture, exactly
// bypassPermissions is bypass, and EVERY other value is an error naming the
// variable — a near-miss must never read as either (fail closed, jobmode.go).
func TestJobBypassAcceptsExactlyOneValue(t *testing.T) {
	if on, err := jobBypass(""); on || err != nil {
		t.Fatalf(`"" = %v, %v; want the default posture`, on, err)
	}
	if on, err := jobBypass("bypassPermissions"); !on || err != nil {
		t.Fatalf(`"bypassPermissions" = %v, %v; want bypass`, on, err)
	}
	for _, v := range []string{"bypass", "BypassPermissions", "bypassPermissions ", " bypassPermissions", "true", "1", "yes",
		"dontAsk", "acceptEdits", "default", "auto", "plan", "--dangerously-skip-permissions"} {
		on, err := jobBypass(v)
		if on || err == nil || !strings.Contains(err.Error(), envJobPermissionMode) {
			t.Errorf("%q = %v, %v; want a refusal naming %s", v, on, err, envJobPermissionMode)
		}
	}
}

// laneArgv builds a lane the way acquireIn does and returns its fresh-start argv.
func laneArgv(t *testing.T, bypass bool, workdir string) string {
	t.Helper()
	l := &lanes{newTerm: func(string) laneTerminal { return &pidPane{pid: 4242} },
		newServer: func(term terminal) *server { return newServer(serverConfig{}, term, newAuthTracker()) },
		bin:       "claude", configDir: t.TempDir(), workspace: "/data/workspace", max: 1, now: time.Now,
		set: map[string]*lane{}, jobBypass: bypass}
	ln := l.newLane("lane-1", "key-1", "opus", workdir)
	argv, mode := ln.sup.argv()
	if mode != modeFresh {
		t.Fatalf("fixture: mode %q, want a fresh start", mode)
	}
	return strings.Join(argv, " ")
}

// 🔴 THE BYPASS FLAG IS ON JOB LANES ONLY. With the mode set, a lane in a job's
// worktree starts with `--permission-mode bypassPermissions`; a lane in ccd's own
// workspace (an explicit chat session) does not; and with the mode unset no lane
// does. The whole argv is compared, so the flag cannot move or be duplicated
// unnoticed. (The primary's argv never carries one:
// TestTheSupervisorPassesNoPermissionFlag.)
func TestOnlyAJobLaneOnABypassPodGetsThePermissionFlag(t *testing.T) {
	const wt = "/data/workspace/.muster/jobs/41"
	uuid := laneUUID("key-1")
	for _, c := range []struct {
		name    string
		bypass  bool
		workdir string
		want    string
	}{
		{"job lane, bypass on", true, wt, "env CCD_LANE=lane-1 claude --model opus --permission-mode bypassPermissions --session-id " + uuid},
		{"workspace lane, bypass on", true, "", "env CCD_LANE=lane-1 claude --model opus --session-id " + uuid},
		{"job lane, bypass off", false, wt, "env CCD_LANE=lane-1 claude --model opus --session-id " + uuid},
		{"workspace lane, bypass off", false, "", "env CCD_LANE=lane-1 claude --model opus --session-id " + uuid},
	} {
		if got := laneArgv(t, c.bypass, c.workdir); got != c.want {
			t.Errorf("%s:\n got  %s\n want %s", c.name, got, c.want)
		}
	}
}

// The variable muster writes into a bypass pool's pods (internal/agentspec
// claudeCodeEnv) is the one ccd reads, with the one value ccd accepts. A rename
// on either side would leave the pool's lanes at default permissions, timing
// out on prompts nobody sees — or, the other way, ccd refusing to start.
func TestCcdReadsTheJobPermissionModeAgentspecWrites(t *testing.T) {
	if envJobPermissionMode != agentspec.EnvCCDJobPermissionMode {
		t.Errorf("ccd reads %q, agentspec writes %q", envJobPermissionMode, agentspec.EnvCCDJobPermissionMode)
	}
	if on, err := jobBypass(agentspec.CCDJobPermissionBypass); !on || err != nil {
		t.Errorf("agentspec writes %q, which ccd reads as bypass=%v (err %v)", agentspec.CCDJobPermissionBypass, on, err)
	}
}

// 🔴 THE LANE SET serve RUNS TAKES THE MODE FROM THE ENVIRONMENT. newLaneSet is
// the whole of serve's lane wiring (serve passes it os.Getenv and its own
// terminal and server builders), so a lane set that dropped the mode — the
// pool's job lanes back at default permissions, stalling on prompts nobody sees
// — reddens here: with the mode set a JOB lane's argv carries the flag, with it
// unset it does not. An invalid mode is refused (ccd does not start), even when
// no lane would run.
func TestServesLaneSetTakesTheJobPermissionModeFromTheEnvironment(t *testing.T) {
	build := func(env map[string]string, maxLanes int) (*lanes, error) {
		return newLaneSet(func(k string) string { return env[k] }, maxLanes,
			func(string) laneTerminal { return &pidPane{pid: 4242} },
			func(term terminal) *server { return newServer(serverConfig{}, term, newAuthTracker()) },
			"claude", t.TempDir(), "/data/workspace")
	}
	uuid := laneUUID("key-1")
	for _, c := range []struct {
		mode, want string
	}{
		{"bypassPermissions", "env CCD_LANE=lane-1 claude --permission-mode bypassPermissions --session-id " + uuid},
		{"", "env CCD_LANE=lane-1 claude --session-id " + uuid},
	} {
		l, err := build(map[string]string{envJobPermissionMode: c.mode}, 3)
		if err != nil || l == nil {
			t.Fatalf("mode %q: lanes %v, err %v", c.mode, l, err)
		}
		argv, _ := l.newLane("lane-1", "key-1", "", "/data/workspace/.muster/jobs/41").sup.argv()
		if got := strings.Join(argv, " "); got != c.want {
			t.Errorf("mode %q: job lane argv\n got  %s\n want %s", c.mode, got, c.want)
		}
	}
	for _, maxLanes := range []int{3, 0} {
		if l, err := build(map[string]string{envJobPermissionMode: "bypass"}, maxLanes); err == nil || l != nil ||
			!strings.Contains(err.Error(), envJobPermissionMode) {
			t.Errorf("max %d, invalid mode: lanes %v, err %v; want a refusal naming %s", maxLanes, l, err, envJobPermissionMode)
		}
	}
	if l, err := build(nil, 0); l != nil || err != nil {
		t.Errorf("max 0: lanes %v, err %v; want none", l, err)
	}
}
