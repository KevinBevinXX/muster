package mirrorhost

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/ccsubagents"
	"github.com/ZacxDev/muster/internal/cctasks"
	"github.com/ZacxDev/muster/internal/mirror"
)

// SYNTHETIC records in the shapes Claude Code 2.1.289 writes (shape only:
// every id, time and text here is invented). The helpers beside them are
// subagents_test.go's.

// timedCall is the parent's Agent call at base+sec.
func timedCall(id string, sec int, background bool) string {
	in := map[string]any{"description": "fixture job " + id, "prompt": "do the thing", "subagent_type": "general-purpose"}
	if background {
		in["run_in_background"] = true
	}
	b, _ := json.Marshal(in)
	return row(map[string]any{"type": "assistant", "timestamp": at(sec), "message": map[string]any{"content": []any{
		map[string]any{"type": "tool_use", "id": id, "name": "Agent", "input": json.RawMessage(b)}}}})
}

// enqueueNotice is a finished task's notification as most arrive: the content
// of a queue-operation enqueue (written when the task finishes).
func enqueueNotice(toolID string, sec int, status string) string {
	return row(map[string]any{"type": "queue-operation", "operation": "enqueue", "timestamp": at(sec),
		"content": "<task-notification>\n<task-id>a00000000000000f1</task-id>\n<tool-use-id>" + toolID +
			"</tool-use-id>\n<status>" + status + "</status>\n<summary>Agent finished</summary>\n</task-notification>"})
}

// taskStopRow is a TaskStop result naming task id.
func taskStopRow(callID string, sec int, task, typ string) string {
	return row(map[string]any{"type": "user", "timestamp": at(sec), "toolUseResult": map[string]any{"task_id": task, "task_type": typ},
		"message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": callID, "content": "stopped"}}}})
}

func agentsKilledRow(sec int) string {
	return row(map[string]any{"type": "system", "subtype": "agents_killed", "timestamp": at(sec)})
}

// ledgerAt reads f's transcript into a fresh ledger (or l) and returns its
// running count at base+nowSec.
func ledgerRunning(l *ccsubagents.Ledger, nowSec int) int {
	l.Read()
	return l.Running(atT(nowSec))
}

// TestTheLedgerCountsRunningAgentCalls: each way an Agent call stops running,
// one call at a time, against the subagent cards' own state rule. The counts
// at each step are distinct (3, 2, …) so a mutant that ignores a signal shows.
func TestTheLedgerCountsRunningAgentCalls(t *testing.T) {
	f := newSAFixture(t)
	l := ccsubagents.NewLedger(f.transcript, atMS(1))
	// Three calls: a sync one, two background ones (launched).
	f.appendTo(f.transcript, timedCall("toolu_sync", 10, false))
	f.appendTo(f.transcript, timedCall("toolu_bg1", 11, true))
	f.appendTo(f.transcript, parentResultRow("toolu_bg1", 12, "a00000000000000b1", true, false))
	f.appendTo(f.transcript, timedCall("toolu_bg2", 13, true))
	f.appendTo(f.transcript, parentResultRow("toolu_bg2", 14, "a00000000000000b2", true, false))
	if got := ledgerRunning(l, 20); got != 3 {
		t.Fatalf("three calls, none finished: running = %d, want 3", got)
	}
	// The sync call's result ends it.
	f.appendTo(f.transcript, parentResultRow("toolu_sync", 21, "a00000000000000a1", false, false))
	if got := ledgerRunning(l, 22); got != 2 {
		t.Fatalf("after the sync result: running = %d, want 2", got)
	}
	// A background agent's notification, as a queue-operation enqueue.
	f.appendTo(f.transcript, enqueueNotice("toolu_bg1", 23, "completed"))
	if got := ledgerRunning(l, 24); got != 1 {
		t.Fatalf("after the enqueued notification: running = %d, want 1", got)
	}
	// The other one's, inline in a user record.
	f.appendTo(f.transcript, parentNoticeRow("toolu_bg2", 25, "completed"))
	if got := ledgerRunning(l, 26); got != 0 {
		t.Fatalf("after the inline notification: running = %d, want 0", got)
	}
}

// TestTheLedgerClosesKilledAgents: a TaskStop naming an agent closes its call;
// one naming a Bash task closes nothing; agents_killed closes every call
// launched before it — but not one launched after.
func TestTheLedgerClosesKilledAgents(t *testing.T) {
	f := newSAFixture(t)
	l := ccsubagents.NewLedger(f.transcript, atMS(1))
	for i, id := range []string{"toolu_k1", "toolu_k2", "toolu_k3"} {
		f.appendTo(f.transcript, timedCall(id, 10+2*i, true))
		f.appendTo(f.transcript, parentResultRow(id, 11+2*i, fmt.Sprintf("a00000000000000c%d", i+1), true, false))
	}
	if got := ledgerRunning(l, 20); got != 3 {
		t.Fatalf("instrument check: running = %d, want 3", got)
	}
	f.appendTo(f.transcript, taskStopRow("toolu_stop_bash", 21, "b0000000a", "local_bash"))
	if got := ledgerRunning(l, 22); got != 3 {
		t.Fatalf("a Bash TaskStop changed the agent count: running = %d, want 3", got)
	}
	f.appendTo(f.transcript, taskStopRow("toolu_stop1", 23, "a00000000000000c1", "local_agent"))
	if got := ledgerRunning(l, 24); got != 2 {
		t.Fatalf("after the TaskStop: running = %d, want 2", got)
	}
	f.appendTo(f.transcript, agentsKilledRow(25))
	f.appendTo(f.transcript, timedCall("toolu_after", 26, true))
	if got := ledgerRunning(l, 27); got != 1 {
		t.Fatalf("after agents_killed and one new call: running = %d, want 1 (the new one)", got)
	}
}

// TestTheLedgerDropsCallsOlderThanTheCLI: a resumed CLI cannot host its
// predecessor's in-process agents, so a call made before its startedAt is
// not counted; one made after is.
func TestTheLedgerDropsCallsOlderThanTheCLI(t *testing.T) {
	f := newSAFixture(t)
	f.appendTo(f.transcript, timedCall("toolu_old", 10, true))
	f.appendTo(f.transcript, timedCall("toolu_new", 30, true))
	if got := ledgerRunning(ccsubagents.NewLedger(f.transcript, atMS(20)), 31); got != 1 {
		t.Fatalf("with the CLI started between them: running = %d, want 1", got)
	}
	// Control: a CLI that started before both hosts both.
	if got := ledgerRunning(ccsubagents.NewLedger(f.transcript, atMS(5)), 31); got != 2 {
		t.Fatalf("with the CLI started before both: running = %d, want 2", got)
	}
}

// TestTheLedgerNeverReadsATornLine: a line with no newline yet (the CLI
// mid-write) is not read; it is read whole once its newline lands.
func TestTheLedgerNeverReadsATornLine(t *testing.T) {
	f := newSAFixture(t)
	l := ccsubagents.NewLedger(f.transcript, atMS(1))
	f.appendTo(f.transcript, timedCall("toolu_t1", 10, true))
	whole := timedCall("toolu_t2", 11, true)
	f.appendTo(f.transcript, whole[:len(whole)/2])
	if got := ledgerRunning(l, 12); got != 1 {
		t.Fatalf("with the second call torn: running = %d, want 1", got)
	}
	f.appendTo(f.transcript, whole[len(whole)/2:])
	if got := ledgerRunning(l, 13); got != 2 {
		t.Fatalf("once it is whole: running = %d, want 2", got)
	}
}

// TestTheLedgerStartsWithinTheBackfillWindow: a transcript bigger than
// mirror.BackfillWindow is read from its last window, at a line boundary — a
// call before it is not seen (the documented under-count), one inside it is.
func TestTheLedgerStartsWithinTheBackfillWindow(t *testing.T) {
	f := newSAFixture(t)
	f.appendTo(f.transcript, timedCall("toolu_early", 10, true))
	pad := row(map[string]any{"type": "user", "timestamp": at(11), "isMeta": true, "message": map[string]any{"content": strings.Repeat("p", 4000)}})
	for i := 0; i < mirror.BackfillWindow/len(pad)+2; i++ {
		f.appendTo(f.transcript, pad)
	}
	f.appendTo(f.transcript, timedCall("toolu_late", 12, true))
	if got := ledgerRunning(ccsubagents.NewLedger(f.transcript, atMS(1)), 13); got != 1 {
		t.Fatalf("running = %d, want 1 (only the call inside the backfill window)", got)
	}
}

// claudeWindow is a window whose Claude pane has raw status and shells.
func claudeWindow(id, gen, transcript, raw string, sh cctasks.Shells) WindowInfo {
	return WindowInfo{Session: "s", ID: id, Claude: &ClaudePane{PID: 9, Gen: gen, Transcript: transcript,
		RawStatus: raw, StartedAt: atMS(1), Shells: sh}}
}

// TestWindowWorkAppliesTheRuleAndReadsTranscriptsOnlyWhenBusy: per window,
// cctasks.Decide over its status, its shells and its ledger. An idle window
// whose transcript still shows a running agent reports nothing (the CLI's
// idle overrules a stale ledger) and its transcript is never opened; a window
// with no status, or no Claude pane, has no entry; a session that is no
// longer listed drops its ledger.
func TestWindowWorkAppliesTheRuleAndReadsTranscriptsOnlyWhenBusy(t *testing.T) {
	f := newSAFixture(t)
	for i, id := range []string{"toolu_w1", "toolu_w2", "toolu_w3"} {
		f.appendTo(f.transcript, timedCall(id, 10+i, true))
	}
	// An unreadable transcript (a directory) for the idle window: opening it
	// would be a read this test must not see — its ledger stays empty.
	idleT := filepath.Join(t.TempDir(), "idle.jsonl")
	w := &workCounter{now: func() time.Time { return atT(20) }}
	got := w.windowWork([]WindowInfo{
		claudeWindow("@1", "sess-busy", f.transcript, cctasks.StatusBusy, cctasks.Shells{N: 2}),
		claudeWindow("@2", "sess-idle", idleT, cctasks.StatusIdle, cctasks.Shells{N: 4}),
		claudeWindow("@3", "sess-shell", f.transcript, cctasks.StatusShell, cctasks.Shells{N: 3}),
		claudeWindow("@4", "sess-none", f.transcript, "", cctasks.Shells{N: 5}),
		{Session: "s", ID: "@5"},
	})
	want := map[string]mirror.WindowWork{
		"@1": {Subagents: 3, Shells: 1, ShellsAtLeast: true},
		"@2": {},
		"@3": {Shells: 3},
	}
	if !maps.Equal(got, want) {
		t.Fatalf("windowWork = %v, want %v", got, want)
	}
	keys := slices.Sorted(maps.Keys(w.ledgers))
	if !slices.Equal(keys, []string{"sess-busy"}) {
		t.Fatalf("ledgers = %v, want only the busy window's (no transcript read for idle, shell or no status)", keys)
	}
	// The busy session ends: its ledger goes with it.
	w.windowWork([]WindowInfo{claudeWindow("@2", "sess-idle", idleT, cctasks.StatusIdle, cctasks.Shells{})})
	if len(w.ledgers) != 0 {
		t.Fatalf("an unlisted session kept its ledger: %v", maps.Keys(w.ledgers))
	}
}

// TestTmuxCountsAClaudeWindowsShellsFromItsProcessTree: the real backend's
// listing (one tmux call, a script here) over a fake /proc and a fake config
// dir. A CLI whose sessions file says "shell" with one background `sleep`
// on its task file reports exactly 1 shell (positive control); with the sleep
// gone it reports "1+" — never 0 — because the CLI still says shell; an idle
// CLI's tree is not read at all.
func TestTmuxCountsAClaudeWindowsShellsFromItsProcessTree(t *testing.T) {
	proc, home := newFakeProc(t), newFakeHome(t)
	const cwd = "/work/fixture-repo"
	proc.add(300, 1, "zsh", cwd, "-zsh")
	proc.add(310, 300, "claude", cwd, "claude")
	proc.add(320, 310, "zsh", cwd, "/bin/zsh", "-c", "sleep 600")
	fd := filepath.Join(proc.root, "320", "fd")
	must(t, os.MkdirAll(fd, 0o755))
	must(t, os.Symlink("/fx/tmp/claude-1000/-work-fixture-repo/sess-1/tasks/b0000000a.output", filepath.Join(fd, "1")))
	home.transcript(cwd, "5e55-0002", time.Now())
	write := func(status string) {
		home.rawSession(310, map[string]any{"sessionId": "5e55-0002", "cwd": cwd, "status": status, "startedAt": atMS(1)})
	}
	bin := filepath.Join(t.TempDir(), "tmux")
	listing := filepath.Join(t.TempDir(), "listing")
	must(t, os.WriteFile(listing, []byte("1\t@3\t1\t1\t%3\t300\t1791903000\t1791903000\tclaude\t/dev/pts/3\t\t\twork\n"), 0o644))
	must(t, os.WriteFile(bin, []byte("#!/bin/sh\ncat '"+listing+"'\n"), 0o755))
	tm := Tmux{Bin: bin, Claude: ClaudeLocator{Proc: proc.root, Home: home.root}}
	work := func() map[string]mirror.WindowWork {
		ws, err := tm.Windows(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return (&workCounter{}).windowWork(ws)
	}
	write("shell")
	if got := work(); !maps.Equal(got, map[string]mirror.WindowWork{"@3": {Shells: 1}}) {
		t.Fatalf("with the background sleep: %v, want exactly 1 shell", got)
	}
	must(t, os.Remove(filepath.Join(fd, "1")))
	if got := work(); !maps.Equal(got, map[string]mirror.WindowWork{"@3": {Shells: 1, ShellsAtLeast: true}}) {
		t.Fatalf("with the sleep gone but the CLI still saying shell: %v, want 1+", got)
	}
	write("idle")
	if got := work(); !maps.Equal(got, map[string]mirror.WindowWork{"@3": {}}) {
		t.Fatalf("idle: %v, want an entry with nothing running", got)
	}
}

// TestWindowColorTakesTheFirstValidField: the expanded value, then the raw
// one (an uppercase literal's #D/#F collides with tmux's aliases under E:),
// lowercased with its '#'. Neither valid — or a value tmux printed a control
// character in (the format turns it into '!') — is no colour.
func TestWindowColorTakesTheFirstValidField(t *testing.T) {
	for _, c := range []struct{ expanded, raw, want string }{
		{"#aabbcc", "#{?#{==:#{session_name},alpha},##aabbcc,}", "#aabbcc"},
		{"%079921", "#D79921", "#d79921"},
		{"*abd2f", "#Fabd2f", "#fabd2f"},
		{"3A7BD5", "3A7BD5", "#3a7bd5"},
		{"", "", ""},
		{"red", "red", ""},
		{"#aa!bbcc", "#aa!bbcc", ""},
		{"#abc", "#abc", ""},
		{"#aabbccdd", "#aabbccdd", ""},
	} {
		if got := windowColor(c.expanded, c.raw); got != c.want {
			t.Errorf("windowColor(%q, %q) = %q, want %q", c.expanded, c.raw, got, c.want)
		}
	}
}

// TestParseWindowsReadsTheColourAndKeepsTheSessionNameWhole: the colour fields
// sit BEFORE the session name, which stays the last field — so a session name
// holding spaces and colons is still taken whole, and its window still gets
// its colour. (A name with a tab is no valid session at all: ValidSession.)
func TestParseWindowsReadsTheColourAndKeepsTheSessionNameWhole(t *testing.T) {
	out := "1\t@1\t0\t1\t%1\t101\t1\t1\tzsh\t/dev/pts/1\t%079921\t#D79921\ta b: c\n" +
		"1\t@2\t1\t0\t%2\t102\t1\t1\tzsh\t/dev/pts/2\t#3a7bd5\t#{?…}\tplain\n" +
		"1\t@3\t2\t0\t%3\t103\t1\t1\tzsh\t/dev/pts/3\t\t\tplain\n"
	got := parseWindows(out)
	if len(got) != 3 {
		t.Fatalf("parseWindows = %+v, want 3 windows", got)
	}
	if got[0].Session != "a b: c" || got[0].Color != "#d79921" {
		t.Fatalf("window @1 = session %q colour %q, want %q and #d79921", got[0].Session, got[0].Color, "a b: c")
	}
	if got[1].Color != "#3a7bd5" || got[2].Color != "" {
		t.Fatalf("colours = %q, %q; want #3a7bd5 and none", got[1].Color, got[2].Color)
	}
}

// TestTheListingReadsTheMusterColorOption: the one listing's format names
// @muster-color, each field passed through the control-character
// substitution — and nothing else.
func TestTheListingReadsTheMusterColorOption(t *testing.T) {
	dir := t.TempDir()
	args := filepath.Join(dir, "args")
	bin := filepath.Join(dir, "tmux")
	must(t, os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > '"+args+"'\n"), 0o755))
	if _, err := (Tmux{Bin: bin, Claude: ClaudeLocator{Proc: t.TempDir(), Home: t.TempDir()}}).Windows(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(args)
	want := "\t#{s/[^ -~]/!/;E:@muster-color}\t#{s/[^ -~]/!/:@muster-color}\t#{session_name}\n"
	if !strings.HasSuffix(string(b), want) {
		t.Fatalf("the listing's format is %q, want it to end %q", b, want)
	}
}

// TestAPerWindowHostSendsItsWorkAndColoursAfterItsWindows: after the hello,
// one work message and one colors message, restricted to listed windows and
// sent again only when they CHANGE; an empty work map is sent when the work
// stops (it clears muster's).
func TestAPerWindowHostSendsItsWorkAndColoursAfterItsWindows(t *testing.T) {
	d := newDrivenMuster(t)
	b := &windowsBackend{windows: []WindowInfo{
		{Session: "scratch2", ID: "@7", Index: 3, Active: true, Pane: "%21", PanePID: 1, Command: "claude", Color: "#3a7bd5",
			Claude: &ClaudePane{Pane: "%21", PID: 9, Gen: "sess-a", RawStatus: cctasks.StatusShell, Shells: cctasks.Shells{N: 2}}},
		{Session: "scratch2", ID: "@4", Index: 1, Pane: "%14", PanePID: 2, Command: "zsh", Color: "#d79921"},
	}}
	d.run(t, b, nil, false)
	waitUntil(t, "the first work and colors messages", func() bool {
		return len(d.of(mirror.TypeWork)) == 1 && len(d.of(mirror.TypeColors)) == 1
	})
	if got := d.of(mirror.TypeWork)[0].Work; !maps.Equal(got, map[string]mirror.WindowWork{"@7": {Shells: 2}}) {
		t.Fatalf("work = %v", got)
	}
	if got := d.of(mirror.TypeColors)[0].Colors; !maps.Equal(got, map[string]string{"@7": "#3a7bd5", "@4": "#d79921"}) {
		t.Fatalf("colors = %v", got)
	}
	time.Sleep(150 * time.Millisecond)
	if nw, nc := len(d.of(mirror.TypeWork)), len(d.of(mirror.TypeColors)); nw != 1 || nc != 1 {
		t.Fatalf("%d work and %d colors messages with nothing changed, want 1 each", nw, nc)
	}
	// The shells end: the work map is sent again, empty.
	b.mu.Lock()
	w := slices.Clone(b.windows)
	b.mu.Unlock()
	c := *w[0].Claude
	c.RawStatus, c.Shells = cctasks.StatusIdle, cctasks.Shells{}
	w[0].Claude = &c
	b.set(w...)
	waitUntil(t, "the second work message", func() bool { return len(d.of(mirror.TypeWork)) == 2 })
	if got := d.of(mirror.TypeWork)[1].Work; len(got) != 0 {
		t.Fatalf("work after the shells ended = %v, want empty", got)
	}
	if n := len(d.of(mirror.TypeColors)); n != 1 {
		t.Fatalf("the colours were re-sent with no change: %d messages", n)
	}
}

// TestTheLargestWorkAndColorsMessagesFitOneFrame: MaxWindows entries each, the
// longest window id, every count at its bound: each must fit a frame. Inline,
// either would have overrun the window list's measured worst case together.
func TestTheLargestWorkAndColorsMessagesFitOneFrame(t *testing.T) {
	var windows []mirror.Window
	work := map[string]mirror.WindowWork{}
	colors := map[string]string{}
	for i := 0; i < mirror.MaxWindows; i++ {
		id := "@" + fmt.Sprint(9_999_999_999-i)
		windows = append(windows, mirror.Window{Session: "s", ID: id, Index: i})
		work[id] = mirror.WindowWork{Subagents: 1000, Shells: 1000, ShellsAtLeast: true}
		colors[id] = "#ffffff"
	}
	nw := mirror.NormalizeWindows(windows, 0)
	keptW, keptC := mirror.NormalizeWork(work, nw), mirror.NormalizeColors(colors, nw)
	if len(keptW) != mirror.MaxWindows || len(keptC) != mirror.MaxWindows {
		t.Fatalf("instrument check: kept %d work and %d colours, want %d each", len(keptW), len(keptC), mirror.MaxWindows)
	}
	for _, m := range []mirror.Msg{{Type: mirror.TypeWork, Work: keptW}, {Type: mirror.TypeColors, Colors: keptC}} {
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if len(b) > mirror.MaxFrame {
			t.Fatalf("the largest %s message is %d bytes, over MaxFrame", m.Type, len(b))
		}
		t.Logf("largest %s message: %d entries in %d bytes (%.1f%% of MaxFrame)", m.Type, mirror.MaxWindows, len(b), 100*float64(len(b))/float64(mirror.MaxFrame))
	}
	const windowListWorst = 1_030_215
	inline := mirror.MaxWindows * (len(`,"subagents":99,"shells":99,"shellsAtLeast":true`) + len(`,"color":"#ffffff"`))
	if windowListWorst+inline <= mirror.MaxFrame {
		t.Fatalf("instrument check: both inline (%d bytes) would have fitted the window list", inline)
	}
}
