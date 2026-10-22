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
