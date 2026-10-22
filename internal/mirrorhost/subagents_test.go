package mirrorhost

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/ccsubagents"
	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/mirror"
)

// SYNTHETIC FIXTURES in the layout Claude Code writes (measured on 2.1.289 and
// 2.1.220, subagents.go): <dir>/<session>.jsonl beside
// <dir>/<session>/subagents/agent-<id>.{jsonl,meta.json}. Every id, time and
// text is invented.

// base is the fixtures' clock origin; every time below is base + seconds, and
// pairwise distinct so a mutant that swaps two of them shows.
var saBase = time.Date(2030, 1, 2, 3, 4, 0, 0, time.UTC)

func at(sec int) string     { return saBase.Add(time.Duration(sec) * time.Second).Format(time.RFC3339Nano) }
func atMS(sec int) int64    { return saBase.Add(time.Duration(sec) * time.Second).UnixMilli() }
func atT(sec int) time.Time { return saBase.Add(time.Duration(sec) * time.Second) }

func row(v map[string]any) string {
	b, _ := json.Marshal(v)
	return string(b) + "\n"
}

// subPrompt, subTool and subText are a subagent's own rows (isSidechain).
func subPrompt(agent string, sec int, text string) string {
	return row(map[string]any{"type": "user", "isSidechain": true, "agentId": agent, "timestamp": at(sec),
		"message": map[string]any{"role": "user", "content": text}})
}

func subTool(agent string, sec int, id, name, input string) string {
	return row(map[string]any{"type": "assistant", "isSidechain": true, "agentId": agent, "timestamp": at(sec),
		"message": map[string]any{"role": "assistant", "stop_reason": "tool_use", "content": []any{
			map[string]any{"type": "tool_use", "id": id, "name": name, "input": json.RawMessage(input)}}}})
}

func subResult(agent string, sec int, id, out string) string {
	return row(map[string]any{"type": "user", "isSidechain": true, "agentId": agent, "timestamp": at(sec),
		"message": map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": id, "content": out}}}})
}

func subText(agent string, sec int, text string) string {
	return row(map[string]any{"type": "assistant", "isSidechain": true, "agentId": agent, "timestamp": at(sec),
		"message": map[string]any{"role": "assistant", "stop_reason": "end_turn", "content": []any{
			map[string]any{"type": "text", "text": text}}}})
}

// parentCall, parentResult and parentNotice are the parent's rows.
func parentCall(id, desc, typ string, background bool) string {
	in := map[string]any{"description": desc, "prompt": "do the thing", "subagent_type": typ}
	if background {
		in["run_in_background"] = true
	}
	b, _ := json.Marshal(in)
	return row(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
		map[string]any{"type": "tool_use", "id": id, "name": "Agent", "input": json.RawMessage(b)}}}})
}

func parentResultRow(id string, sec int, agent string, async, isErr bool) string {
	tur := map[string]any{"agentId": agent, "status": "completed"}
	if async {
		tur = map[string]any{"isAsync": true, "status": "async_launched", "agentId": agent}
	}
	return row(map[string]any{"type": "user", "timestamp": at(sec), "toolUseResult": tur, "message": map[string]any{"content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": id, "content": "launched or done", "is_error": isErr}}}})
}

func parentNoticeRow(toolID string, sec int, status string) string {
	return row(map[string]any{"type": "user", "timestamp": at(sec), "origin": map[string]any{"kind": "task-notification"},
		"message": map[string]any{"role": "user", "content": "<task-notification>\n<task-id>x</task-id>\n<tool-use-id>" + toolID +
			"</tool-use-id>\n<status>" + status + "</status>\n<summary>Agent finished</summary>\n</task-notification>"}})
}

// saFixture is one session's transcript and subagents directory.
type saFixture struct {
	t          *testing.T
	transcript string
	dir        string
}

func newSAFixture(t *testing.T) *saFixture {
	t.Helper()
	root := t.TempDir()
	f := &saFixture{t: t, transcript: filepath.Join(root, "5e55-0001.jsonl")}
	f.dir = filepath.Join(root, "5e55-0001", "subagents")
	if err := os.MkdirAll(f.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *saFixture) appendTo(name, s string) {
	f.t.Helper()
	fh, err := os.OpenFile(name, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := fh.WriteString(s); err != nil {
		f.t.Fatal(err)
	}
	_ = fh.Close()
}

func (f *saFixture) agent(id, s string) { f.appendTo(filepath.Join(f.dir, "agent-"+id+".jsonl"), s) }

func (f *saFixture) meta(id, raw string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, "agent-"+id+".meta.json"), []byte(raw), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// age sets agent id's file mtime to now-d.
func (f *saFixture) age(id string, mt time.Time) {
	f.t.Helper()
	if err := os.Chtimes(filepath.Join(f.dir, "agent-"+id+".jsonl"), mt, mt); err != nil {
		f.t.Fatal(err)
	}
}

// tracker is a tracker of the saFixture that has read parent (the parent's rows).
func (f *saFixture) tracker(now time.Time, parent ...string) *subagentTracker {
	f.t.Helper()
	tr := newSubagentTracker(f.transcript)
	tr.now = func() time.Time { return now }
	var evs []cctranscript.Event
	for _, l := range strings.Split(strings.Join(parent, ""), "\n") {
		if r, ok := cctranscript.DecodeLine([]byte(l)); ok {
			evs = append(evs, cctranscript.Events(r)...)
		}
	}
	tr.observe(evs)
	return tr
}

func byTool(list []mirror.Subagent) map[string]mirror.Subagent {
	m := map[string]mirror.Subagent{}
	for _, s := range list {
		m[s.ToolID] = s
	}
	return m
}

// THE STATUS RULE (ccsubagents.State), each branch with a case no earlier one
// decides. Times are pairwise distinct; "last row" is f.updated.
func TestSubagentStateRule(t *testing.T) {
	now := atT(1000)
	running := ccsubagents.Facts{Exists: true, Mtime: atT(990), Started: atMS(1), Updated: atMS(100)}
	finalOld := ccsubagents.Facts{Exists: true, Mtime: atT(900), Started: atMS(2), Updated: atMS(200), Final: true}
	finalNew := ccsubagents.Facts{Exists: true, Mtime: atT(995), Started: atMS(3), Updated: atMS(300), Final: true}
	cases := []struct {
		name string
		res  *ccsubagents.ParentResult
		note *ccsubagents.ParentNotice
		f    ccsubagents.Facts
		want string
	}{
		{"a sync result decides: done", &ccsubagents.ParentResult{At: atMS(400), OK: true}, nil, running, mirror.SubagentDone},
		{"a sync result decides: an error is failed", &ccsubagents.ParentResult{At: atMS(401), OK: false}, nil, finalOld, mirror.SubagentFailed},
		{"a background LAUNCH decides nothing", &ccsubagents.ParentResult{At: atMS(402), OK: true, Async: true}, nil, running, mirror.SubagentRunning},
		{"a notification decides: completed is done", &ccsubagents.ParentResult{At: atMS(5), OK: true, Async: true}, &ccsubagents.ParentNotice{At: atMS(403), Status: "completed"}, running, mirror.SubagentDone},
		{"a notification decides: killed is failed", nil, &ccsubagents.ParentNotice{At: atMS(404), Status: "killed"}, finalNew, mirror.SubagentFailed},
		{"a notification decides: failed is failed", nil, &ccsubagents.ParentNotice{At: atMS(405), Status: "failed"}, running, mirror.SubagentFailed},
		{"a notification OLDER than the last row (resumed) decides nothing", nil, &ccsubagents.ParentNotice{At: atMS(50), Status: "completed"}, running, mirror.SubagentRunning},
		{"a sync result OLDER than the last row decides nothing", &ccsubagents.ParentResult{At: atMS(60), OK: true}, nil, running, mirror.SubagentRunning},
		{"no signal: a final reply quiet past SubagentQuiet is done", nil, nil, finalOld, mirror.SubagentDone},
		{"no signal: a final reply written just now is still running", nil, nil, finalNew, mirror.SubagentRunning},
		{"no signal: a quiet agent whose last row is a tool call is running", nil, nil, ccsubagents.Facts{Exists: true, Mtime: atT(10), Updated: atMS(7)}, mirror.SubagentRunning},
		{"no file, no signal: running", nil, nil, ccsubagents.Facts{}, mirror.SubagentRunning},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ccsubagents.State(c.res, c.note, c.f, now); got != c.want {
				t.Fatalf("state %q, want %q", got, c.want)
			}
		})
	}
}

// A running, a finished, a background agent, a missing meta and a torn last
// line — through the tracker, from files on disk.
func TestSubagentTrackerScan(t *testing.T) {
	f := newSAFixture(t)
	now := atT(500)

	// RUNNING (foreground, no result yet): its last row is a tool call.
	f.meta("a0001", `{"agentType":"general-purpose","description":"Audit the widget","toolUseId":"toolu_run","spawnDepth":1}`)
	f.agent("a0001", subPrompt("a0001", 10, "audit it")+subTool("a0001", 11, "t1", "Bash", `{"command":"go test ./widget"}`))
	f.age("a0001", atT(11))

	// FINISHED (foreground): a final reply, and the parent's sync result.
	f.meta("a0002", `{"agentType":"Explore","description":"Find the config","toolUseId":"toolu_fin","spawnDepth":"1","requestNonInteractive":"true"}`)
	f.agent("a0002", subPrompt("a0002", 20, "find")+subTool("a0002", 21, "t2", "Grep", `{"pattern":"listen"}`)+
		subResult("a0002", 22, "t2", "x.go")+subText("a0002", 23, "Found it in x.go.\nMore detail."))
	f.age("a0002", atT(23))

	// BACKGROUND (2.1.289 shape, numeric/boolean meta fields): launched, still
	// working.
	f.meta("a0003", `{"agentType":"general-purpose","description":"Background build","toolUseId":"toolu_bg","spawnDepth":1,"requestShape":"background","requestNonInteractive":true}`)
	f.agent("a0003", subPrompt("a0003", 30, "build")+subTool("a0003", 31, "t3", "Read", `{"file_path":"/src/main.go"}`))
	f.age("a0003", atT(31))

	// MISSING META: linked through the parent's result (toolUseResult.agentId).
	f.agent("a0004", subPrompt("a0004", 40, "x")+subTool("a0004", 41, "t4", "Glob", `{"pattern":"**/*.md"}`))
	f.age("a0004", atT(41))

	// MISSING META AND NO RESULT: nothing links it, so no summary.
	f.agent("a0005", subPrompt("a0005", 50, "y"))

	// TORN LAST LINE: a final reply written up to but not including its
	// newline (whole JSON, so only the missing newline marks it unfinished)
	// after a tool call — it is not read, so it is not final and the latest
	// step is the tool call.
	f.meta("a0006", `{"agentType":"general-purpose","description":"Torn","toolUseId":"toolu_torn"}`)
	full := subText("a0006", 62, "All done.")
	f.agent("a0006", subPrompt("a0006", 60, "z")+subTool("a0006", 61, "t6", "Bash", `{"command":"make check"}`)+full[:len(full)-1])
	f.age("a0006", atT(100))

	// NESTED: a subagent's own subagent (parentAgentId) — its toolUseId is in
	// a0001's transcript, never the session's, so even a parent call with the
	// same id would not link it.
	f.meta("a0007", `{"agentType":"general-purpose","description":"Nested","toolUseId":"toolu_nested","parentAgentId":"a0001"}`)
	f.agent("a0007", subPrompt("a0007", 70, "n"))

	tr := f.tracker(now,
		parentCall("toolu_run", "Audit the widget", "general-purpose", false),
		parentCall("toolu_fin", "Find the config", "Explore", false),
		parentResultRow("toolu_fin", 24, "a0002", false, false),
		parentCall("toolu_bg", "Background build", "general-purpose", true),
		parentResultRow("toolu_bg", 32, "a0003", true, false),
		parentCall("toolu_nometa", "No meta", "general-purpose", true),
		parentResultRow("toolu_nometa", 42, "a0004", true, false),
		parentCall("toolu_unlinked", "Unlinked", "general-purpose", false),
		parentCall("toolu_torn", "Torn", "general-purpose", false),
		parentCall("toolu_nested", "Nested", "general-purpose", false),
	)
	list := tr.scan()
	got := byTool(list)
	if len(list) != 5 {
		t.Fatalf("%d summaries, want 5 (run, fin, bg, nometa, torn): %+v", len(list), list)
	}
	want := map[string]mirror.Subagent{
		"toolu_run": {Agent: "a0001", ToolID: "toolu_run", Type: "general-purpose", Description: "Audit the widget",
			State: mirror.SubagentRunning, Started: atMS(10), Updated: atMS(11), Last: "Bash: go test ./widget"},
		"toolu_fin": {Agent: "a0002", ToolID: "toolu_fin", Type: "Explore", Description: "Find the config",
			State: mirror.SubagentDone, Started: atMS(20), Updated: atMS(23), Last: "Found it in x.go."},
		"toolu_bg": {Agent: "a0003", ToolID: "toolu_bg", Type: "general-purpose", Description: "Background build", Background: true,
			State: mirror.SubagentRunning, Started: atMS(30), Updated: atMS(31), Last: "Read: /src/main.go"},
		"toolu_nometa": {Agent: "a0004", ToolID: "toolu_nometa", Background: true,
			State: mirror.SubagentRunning, Started: atMS(40), Updated: atMS(41), Last: "Glob: **/*.md"},
		"toolu_torn": {Agent: "a0006", ToolID: "toolu_torn", Type: "general-purpose", Description: "Torn",
			State: mirror.SubagentRunning, Started: atMS(60), Updated: atMS(61), Last: "Bash: make check"},
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s:\n got %+v\nwant %+v", k, got[k], w)
		}
	}
	// The order is the calls' order in the parent.
	order := []string{"toolu_run", "toolu_fin", "toolu_bg", "toolu_nometa", "toolu_torn"}
	for i, s := range list {
		if s.ToolID != order[i] {
			t.Fatalf("order %v, want %v", list, order)
		}
	}

	// The torn line is finished and goes quiet: now final, and done once quiet.
	f.agent("a0006", full[len(full)-1:])
	f.age("a0006", atT(62))
	// The background agent finishes: a final reply, then its notification.
	f.agent("a0003", subText("a0003", 33, "Built."))
	f.age("a0003", atT(495)) // just written: only the notification can say done
	tr.observe(func() []cctranscript.Event {
		r, _ := cctranscript.DecodeLine([]byte(strings.TrimSpace(parentNoticeRow("toolu_bg", 34, "completed"))))
		return cctranscript.Events(r)
	}())
	got = byTool(tr.scan())
	if s := got["toolu_torn"]; s.State != mirror.SubagentDone || s.Last != "All done." || s.Updated != atMS(62) {
		t.Errorf("torn line completed: %+v, want done with its reply", s)
	}
	if s := got["toolu_bg"]; s.State != mirror.SubagentDone || s.Last != "Built." {
		t.Errorf("background after its notification: %+v, want done", s)
	}
}

// changed sends a list once, then only when it moved.
func TestSubagentTrackerSendsOnlyAChange(t *testing.T) {
	f := newSAFixture(t)
	f.meta("b0001", `{"agentType":"general-purpose","description":"d","toolUseId":"toolu_c"}`)
	f.agent("b0001", subPrompt("b0001", 1, "p"))
	tr := f.tracker(atT(5), parentCall("toolu_c", "d", "general-purpose", false))
	if l, ok := tr.changed(); !ok || len(l) != 1 {
		t.Fatalf("first: %v %v, want the list", l, ok)
	}
	if _, ok := tr.changed(); ok {
		t.Fatal("an unchanged list was sent again")
	}
	f.agent("b0001", subTool("b0001", 2, "x", "Bash", `{"command":"ls"}`))
	if l, ok := tr.changed(); !ok || l[0].Last != "Bash: ls" {
		t.Fatalf("after a step: %v %v, want the new list", l, ok)
	}
	// An empty view sends an empty list once: the host's "I report subagents".
	empty := newSAFixture(t).tracker(atT(5))
	if l, ok := empty.changed(); !ok || l == nil || len(l) != 0 {
		t.Fatalf("empty: %#v %v, want one empty, non-nil list", l, ok)
	}
}

// 🔴 REDACTION: what looks like a secret in a subagent's steps, its latest
// line or its description never leaves the host.
func TestSubagentTextIsRedactedBeforeItLeavesTheHost(t *testing.T) {
	const secret = "ghp_" + "Zq9Xw8Vu7Ts6Rq5Po4Nm3Lk2"
	const bearer = "Bearer " + "abc123def456ghi789"
	f := newSAFixture(t)
	f.meta("c0001", `{"agentType":"general-purpose","description":"use token=`+secret+`","toolUseId":"toolu_s"}`)
	f.agent("c0001", subPrompt("c0001", 1, "p")+
		subTool("c0001", 2, "t1", "Bash", `{"command":"curl -H 'Authorization: `+bearer+`' https://example.test"}`)+
		subText("c0001", 3, "export GITHUB_TOKEN="+secret))
	tr := f.tracker(atT(4), parentCall("toolu_s", "d", "general-purpose", false))
	list := tr.scan()
	tail, err := ReadSubagentTail(f.transcript, "c0001")
	if err != nil {
		t.Fatal(err)
	}
	all, _ := json.Marshal(struct {
		L []mirror.Subagent
		T mirror.SubagentTail
	}{list, tail})
	for _, s := range []string{secret, "abc123def456ghi789"} {
		if strings.Contains(string(all), s) {
			t.Fatalf("a secret left the host: %s", all)
		}
	}
	if !strings.Contains(string(all), "«redacted»") || len(tail.Steps) != 2 {
		t.Fatalf("expected the redaction mark in two steps: %s", all)
	}
	if !strings.HasPrefix(list[0].Last, "GITHUB_TOKEN=«redacted»") && !strings.Contains(list[0].Last, "GITHUB_TOKEN=«redacted»") {
		t.Fatalf("last line %q, want its token redacted", list[0].Last)
	}
}

// The tail: newest steps oldest first, bounded; refused for an id that is not
// one path element, and for an agent the session does not have.
func TestReadSubagentTail(t *testing.T) {
	f := newSAFixture(t)
	var b strings.Builder
	b.WriteString(subPrompt("d0001", 1, "go"))
	for i := range 30 {
		b.WriteString(subTool("d0001", 2+i, "t", "Bash", `{"command":"step `+strconv.Itoa(100+i)+`"}`))
	}
	b.WriteString(`{"type":"assistant","isSidechain":true,"message":{"content":[{"type":"text","text":"half`) // torn
	f.agent("d0001", b.String())
	tail, err := ReadSubagentTail(f.transcript, "d0001")
	if err != nil {
		t.Fatal(err)
	}
	if len(tail.Steps) != mirror.MaxSubagentTailSteps || !tail.More {
		t.Fatalf("%d steps (more=%v), want %d and more", len(tail.Steps), tail.More, mirror.MaxSubagentTailSteps)
	}
	if first, last := tail.Steps[0], tail.Steps[len(tail.Steps)-1]; first.Text != "step 110" || last.Text != "step 129" || last.Tool != "Bash" {
		t.Fatalf("steps run %+v … %+v, want step 110 … step 129 (the newest 20, oldest first, the torn line unread)", first, last)
	}
	for _, bad := range []string{"../d0001", "d0001/../../x", "", ".hidden"} {
		if _, err := ReadSubagentTail(f.transcript, bad); err == nil {
			t.Errorf("agent %q was read", bad)
		}
	}
	if _, err := ReadSubagentTail(f.transcript, "e9999"); err == nil {
		t.Error("an agent the session does not have was read")
	}
}
