package mirror_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
)

// 🔴 THE FRAME BUDGET: a subagent list and a tail, every field at its bound and
// written in the character JSON escapes six-fold, each fit one message — and
// do so with the events a batch can carry beside them.
func TestTheLargestSubagentMessagesFitOneFrame(t *testing.T) {
	worst := strings.Repeat("<", mirror.MaxSubagentText)
	id := "a" + strings.Repeat("b", 63)
	var list []mirror.Subagent
	for i := range mirror.MaxSubagents + 5 {
		tid := "t" + strings.Repeat("0", 60) + string(rune('A'+i%26)) + string(rune('a'+i/26))
		list = append(list, mirror.Subagent{Agent: id, ToolID: tid, Type: worst, Description: worst, Last: worst,
			State: mirror.SubagentRunning, Started: 1 << 50, Updated: 1 << 50, Background: true})
	}
	norm := mirror.NormalizeSubagents(list)
	if len(norm) > mirror.MaxSubagents {
		t.Fatalf("%d summaries kept, bound %d", len(norm), mirror.MaxSubagents)
	}
	msg, _ := json.Marshal(mirror.Msg{Type: mirror.TypeEvents, ID: 1 << 31, Gen: strings.Repeat("g", 64), Offset: 1 << 50,
		SubagentList: true, Subagents: norm})
	t.Logf("worst-case subagent list: %d summaries, %d bytes of %d", len(norm), len(msg), mirror.MaxFrame)
	if len(msg) > mirror.MaxSubagentListBytes+4096 || len(msg) > mirror.MaxFrame {
		t.Fatalf("a worst-case list is %d bytes", len(msg))
	}

	var steps []mirror.SubagentStep
	for range 3 * mirror.MaxSubagentTailSteps {
		steps = append(steps, mirror.SubagentStep{Tool: strings.Repeat("<", 200), Text: worst, At: 1 << 50})
	}
	tail := mirror.NormalizeSubagentTail(mirror.SubagentTail{Agent: id, Steps: steps})
	tmsg, _ := json.Marshal(mirror.Msg{Type: mirror.TypeSubagentTailed, ID: 1 << 31, Tail: &tail})
	t.Logf("worst-case tail: %d steps, %d bytes", len(tail.Steps), len(tmsg))
	if len(tail.Steps) > mirror.MaxSubagentTailSteps || !tail.More || len(tmsg) > mirror.MaxSubagentTailBytes+4096 {
		t.Fatalf("a worst-case tail: %d steps, more=%v, %d bytes", len(tail.Steps), tail.More, len(tmsg))
	}
}

func TestNormalizeSubagents(t *testing.T) {
	in := []mirror.Subagent{
		{Agent: "../x", ToolID: "toolu_1", State: mirror.SubagentDone},             // bad agent id
		{Agent: "a1", ToolID: "", State: mirror.SubagentDone},                      // no tool id
		{Agent: "a2", ToolID: "toolu_2", State: "exploded", Description: "x\x1by"}, // unknown state, control char
		{Agent: "a3", ToolID: "toolu_2", State: mirror.SubagentDone},               // duplicate tool id
		{Agent: "a4", ToolID: "toolu_4", State: mirror.SubagentFailed, Started: -5},
	}
	got := mirror.NormalizeSubagents(in)
	want := []mirror.Subagent{
		{Agent: "a2", ToolID: "toolu_2", State: mirror.SubagentRunning, Description: "x y"},
		{Agent: "a4", ToolID: "toolu_4", State: mirror.SubagentFailed},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	if !mirror.IsSubagentTool("Agent") || !mirror.IsSubagentTool("Task") || mirror.IsSubagentTool("Bash") {
		t.Fatal("IsSubagentTool")
	}
}

// connectWith runs a REAL muster-mirror with cfg's test switches against r.
func (r *rig) connectWith(t *testing.T, host, token string, b mirrorhost.Backend, noSubagents bool) {
	t.Helper()
	c, err := mirrorhost.New(mirrorhost.Config{URL: r.srv.URL, Token: token, Host: host, Backend: b, AllowHTTP: true,
		Refresh: 30 * time.Millisecond, ViewPoll: 20 * time.Millisecond, Logger: log.New(io.Discard, "", 0), NoSubagents: noSubagents})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = c.RunOnce(ctx) }()
	t.Cleanup(cancel)
}

func subagentRow(typ, ts string, content any) string {
	b, _ := json.Marshal(map[string]any{"type": typ, "isSidechain": true, "agentId": "a77", "timestamp": ts,
		"message": map[string]any{"role": typ, "content": content}})
	return string(b) + "\n"
}

// END TO END through a real host, tunnel and hub: a view of a session whose
// transcript has an Agent call gets SubagentList on its Reset batch, then the
// agent's summary as it moves; a card's tail is asked for and answered — and
// only for the host's own owner.
func TestSubagentsCrossTheWire(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "ws")
	tr := newTranscript(t, promptLine("start an agent"),
		toolLine("toolu_e2e", "Agent", `{"description":"Survey the repo","prompt":"p","subagent_type":"Explore"}`))
	dir := filepath.Join(strings.TrimSuffix(tr.path, ".jsonl"), "subagents")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent-a77.meta.json"), []byte(`{"agentType":"Explore","description":"Survey the repo","toolUseId":"toolu_e2e"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	agentFile := filepath.Join(dir, "agent-a77.jsonl")
	write := func(s string) {
		f, err := os.OpenFile(agentFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString(s)
		_ = f.Close()
	}
	write(subagentRow("user", "2030-01-01T00:00:01Z", "survey"))
	b := newClaudeBackend("main")
	b.setPane("main", mirrorhost.ClaudePane{Pane: "%3", PID: 42, Transcript: tr.path, Gen: "gen-s"})
	r.connectWith(t, "ws", tok, b, false)
	r.online(t, 1, "ws", "main")
	eventually(t, "main is a Claude pane", func() bool { h := r.svc.Hosts(asUser(1)); return len(h) == 1 && h[0].Rendered })

	rec, _, _ := watchAs(t, asUser(1), r.svc, "ws", "main", "", 0)
	eventually(t, "the backfill", func() bool { return len(rec.all()) > 0 })
	if first := rec.all()[0]; !first.Reset || !first.SubagentList {
		t.Fatalf("the Reset batch %+v does not say the host reports subagents", first)
	}
	latest := func() []mirror.Subagent {
		var l []mirror.Subagent
		for _, m := range rec.all() {
			if m.SubagentList && len(m.Subagents) > 0 {
				l = m.Subagents
			}
		}
		return l
	}
	eventually(t, "the agent's summary", func() bool {
		l := latest()
		return len(l) == 1 && l[0].Agent == "a77" && l[0].Type == "Explore" && l[0].State == mirror.SubagentRunning
	})

	// It takes a step: the summary moves.
	write(subagentRow("assistant", "2030-01-01T00:00:02Z", []any{map[string]any{"type": "tool_use", "id": "x1", "name": "Grep", "input": map[string]any{"pattern": "TODO"}}}))
	eventually(t, "the moved summary", func() bool { l := latest(); return len(l) == 1 && l[0].Last == "Grep: TODO" })

	// The parent's result arrives: done.
	tr.write(resultLine("toolu_e2e", "the survey", false))
	eventually(t, "done", func() bool { l := latest(); return len(l) == 1 && l[0].State == mirror.SubagentDone })

	tail, err := r.svc.SubagentTail(asUser(1), "ws", "main", "", "a77")
	if err != nil || len(tail.Steps) != 1 || tail.Steps[0].Tool != "Grep" {
		t.Fatalf("tail %+v, %v; want the Grep step", tail, err)
	}
	if _, err := r.svc.SubagentTail(asUser(2), "ws", "main", "", "a77"); !errors.Is(err, mirror.ErrHostOffline) {
		t.Fatalf("another owner's tail: %v, want ErrHostOffline", err)
	}
	if _, err := r.svc.SubagentTail(asUser(1), "ws", "main", "", "../a77"); !errors.Is(err, mirror.ErrBadSubagent) {
		t.Fatalf("a path as the agent: %v, want ErrBadSubagent", err)
	}
	if _, err := r.svc.SubagentTail(asUser(1), "ws", "main", "", "a99"); !errors.Is(err, mirror.ErrSubagentTailRefused) {
		t.Fatalf("an agent the session lacks: %v, want the host's refusal", err)
	}
}

// AN OLDER HOST (no subagent cards): its stream never says SubagentList, and a
// tail is refused without asking it.
func TestAnOlderHostReportsNoSubagents(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "ws")
	tr := newTranscript(t, promptLine("q"), toolLine("toolu_old", "Agent", `{"description":"d","prompt":"p","subagent_type":"Explore"}`))
	b := newClaudeBackend("main")
	b.setPane("main", mirrorhost.ClaudePane{Pane: "%3", PID: 42, Transcript: tr.path, Gen: "gen-o"})
	r.connectWith(t, "ws", tok, b, true)
	r.online(t, 1, "ws", "main")
	eventually(t, "main is a Claude pane", func() bool { h := r.svc.Hosts(asUser(1)); return len(h) == 1 && h[0].Rendered })
	rec, _, _ := watchAs(t, asUser(1), r.svc, "ws", "main", "", 0)
	eventually(t, "the backfill", func() bool { return len(rec.all()) > 0 })
	tr.write(textLine("more"))
	eventually(t, "a live batch", func() bool { return len(rec.events()) >= 3 })
	for _, m := range rec.all() {
		if m.SubagentList || len(m.Subagents) > 0 {
			t.Fatalf("an older host sent subagents: %+v", m)
		}
	}
	if _, err := r.svc.SubagentTail(asUser(1), "ws", "main", "", "a1"); !errors.Is(err, mirror.ErrSubagentsUnsupported) {
		t.Fatalf("tail of an older host: %v, want ErrSubagentsUnsupported", err)
	}
}
