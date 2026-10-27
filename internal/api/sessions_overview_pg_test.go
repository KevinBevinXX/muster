package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
)

// sessions_overview, the session hint and focus_search's history window, on
// both transports, against a real Postgres. Synthetic fixtures only: node
// "node-7k", a "cache quorum", hosts "box-1"/"box-2".

// overviewMirror is a mirror with fixed hosts whose windows each answer a
// transcript watch with their own events — or, for a window in hang, never
// answer until the watch's context ends (a host gone quiet).
type overviewMirror struct {
	Mirror
	hosts  []mirror.HostView
	sids   map[string]string // window id → Claude session id
	events map[string][]cctranscript.Event
	hang   map[string]bool
}

func (m overviewMirror) Hosts(context.Context) []mirror.HostView { return m.hosts }
func (m overviewMirror) AgentState(context.Context, string, string, string) mirror.AgentState {
	return mirror.AgentState{Status: mirror.StatusIdle}
}
func (m overviewMirror) WatchWindow(ctx context.Context, _, _, window, _ string, _ int64, out func(mirror.Msg) error) error {
	if m.hang[window] {
		<-ctx.Done()
		return ctx.Err()
	}
	sid := m.sids[window]
	if err := out(mirror.Msg{Type: mirror.TypeEvents, Gen: sid, Reset: true, Events: m.events[window]}); err != nil {
		return err
	}
	return out(mirror.Msg{Type: mirror.TypeEvents, Gen: sid, PromptIndex: true})
}

// overviewHost is one per-window host listing ws.
func overviewHost(name string, online bool, ws ...mirror.Window) mirror.HostView {
	ws = mirror.NormalizeWindows(ws, 0)
	h := mirror.HostView{Host: name, Windows: ws, Online: online, Rendered: true}
	h.Sessions, h.Claude, h.Labels, h.Activity = mirror.SessionsOf(ws)
	return h
}

// backdateHistory moves sub's history rows titled title back by d.
func backdateHistory(t *testing.T, sub focus.Subject, title string, d time.Duration) {
	t.Helper()
	dbtest.OwnerExec(t, dbtest.DSN(t), fmt.Sprintf(`UPDATE agent_focus_history SET created_at = now() - interval '%d seconds'
		WHERE subject_kind = '%s' AND subject_key = '%s' AND title = '%s'`, int(d/time.Second), sub.Kind, sub.Key, title))
}

func (g *gatedFixture) setFocus(t *testing.T, sub focus.Subject, title, desc string) {
	t.Helper()
	if _, err := g.focus.Upsert(g.asOwner, sub, focus.Write{Title: title, Description: desc, Source: focus.SourceSummarizer}); err != nil {
		t.Fatal(err)
	}
}

func (g *gatedFixture) chat(t *testing.T, a agents.Agent, msgs ...agents.ChatMessage) {
	t.Helper()
	sess, err := g.srv.ext.Agents.CreateSession(g.asOwner, a.ID, a.Name)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		m.AgentID, m.SessionID = a.ID, sess.ID
		if _, err := g.srv.ext.Agents.AddChatMessage(g.asOwner, m); err != nil {
			t.Fatal(err)
		}
	}
}

const (
	sidLive    = "0b2c3d4e-ov-live"
	sidOffline = "0b2c3d4e-ov-offline"
	sidQuiet   = "0b2c3d4e-ov-quiet"
)

// overviewReplay is the miss's shape: the worker (A) wrote a node-7k focus 4 h
// ago and is now auditing PRs; B and C are unrelated agents; D's node-7k
// focus is 7 h old; a STOPPED agent and an OFFLINE mirrored session both have
// a fresh node-7k focus; the agent's own focus mentions node-7k too. Only A is
// live and on the job inside the window.
func overviewReplay(t *testing.T) (g *gatedFixture, b, c, d, stopped agents.Agent) {
	g = newGatedFixture(t)
	sfx := strconv.FormatInt(time.Now().UnixNano(), 36)
	b = g.newAgent(t, g.owner, "ovb-"+sfx, "notes writer", "ovb-tok-"+sfx)
	c = g.newAgent(t, g.owner, "ovc-"+sfx, "billing", "ovc-tok-"+sfx)
	d = g.newAgent(t, g.owner, "ovd-"+sfx, "old runner", "ovd-tok-"+sfx)
	stopped = g.newAgent(t, g.owner, "ovs-"+sfx, "stopped runner", "ovs-tok-"+sfx)
	if err := g.srv.ext.Agents.UpdateStatus(g.asOwner, stopped.ID, agents.StatusStopped, "", ""); err != nil {
		t.Fatal(err)
	}
	a := focus.AgentSubject(g.worker.ID)
	g.setFocus(t, a, "Upgrading node-7k: drain and reboot", "Cordoned the node; draining workloads next.")
	backdateHistory(t, a, "Upgrading node-7k: drain and reboot", 4*time.Hour)
	g.setFocus(t, a, "Auditing open pull requests", "Reviewing three PRs for the release.")
	g.setFocus(t, focus.AgentSubject(b.ID), "Writing the release notes", "Summarising the changelog.")
	g.setFocus(t, focus.AgentSubject(c.ID), "Refactoring the billing export", "Splitting the CSV writer.")
	g.setFocus(t, focus.AgentSubject(d.ID), "Upgrading node-7k: drain and reboot", "")
	backdateHistory(t, focus.AgentSubject(d.ID), "Upgrading node-7k: drain and reboot", 7*time.Hour)
	g.setFocus(t, focus.AgentSubject(d.ID), "Idle", "")
	g.setFocus(t, focus.AgentSubject(stopped.ID), "Upgrading node-7k: drain and reboot", "")
	g.setFocus(t, focus.AgentSubject(g.agent.ID), "Triage of node-7k upgrade messages", "The node-7k drain inbox.")
	g.setFocus(t, focus.MirrorSubject(sidOffline), "Upgrading node-7k: drain and reboot", "")
	g.setFocus(t, focus.MirrorSubject(sidLive), "Reading the cache dashboards", "")
	now := time.Now().Unix()
	g.srv.ext.Mirror = overviewMirror{
		hosts: []mirror.HostView{
			overviewHost("box-1", true, mirror.Window{Session: "work", ID: "@1", Index: 1, Active: true, Claude: true, ClaudeSession: sidLive, Activity: now}),
			overviewHost("box-2", false, mirror.Window{Session: "work", ID: "@1", Index: 1, Active: true, Claude: true, ClaudeSession: sidOffline, Activity: now}),
		},
		sids: map[string]string{"@1": sidLive},
		events: map[string][]cctranscript.Event{"@1": {
			{Kind: cctranscript.KindPrompt, Text: "check the dashboards"},
			{Kind: cctranscript.KindText, Text: "Looking at the cache panel."},
		}},
	}
	return g, b, c, d, stopped
}

const overviewMessage = "Operational note for the node-7k upgrade: drain node-7k before the reboot and keep the cache quorum above two."

// 🔴 TestTheHintListsOnlyTheLiveSessionOnTheJob: the replay at the hint's own
// seam — A only; and current-focus search finds nothing, a 6 h history search
// finds A, on both transports.
func TestTheHintListsOnlyTheLiveSessionOnTheJob(t *testing.T) {
	g, _, _, d, stopped := overviewReplay(t)
	a := "agent:" + strconv.FormatInt(g.worker.ID, 10)
	h := g.srv.pluginSessionHints(g.asOwner, g.agent, plugins.Message{ID: 1, Subject: "node-7k upgrade", Body: overviewMessage})
	if len(h.Sessions) != 1 || h.Sessions[0].ID != a || h.Window != focus.HistoryWindow ||
		len(h.Sessions[0].Titles) != 1 || h.Sessions[0].Titles[0].Title != "Upgrading node-7k: drain and reboot" {
		t.Fatalf("hint %+v; want exactly %s with its node-7k focus", h, a)
	}
	// Ordinary mail and a newsletter share common words with these sessions
	// ("release", "cache", "notes") and name none of them.
	for _, body := range []string{
		"Quick update on our meeting next week: please review the release notes and the cache dashboards before then.",
		"This week's newsletter: billing export tips, the next release, cache dashboards and release notes. See you next week.",
	} {
		if h := g.srv.pluginSessionHints(g.asOwner, g.agent, plugins.Message{ID: 2, Subject: "news", Body: body}); len(h.Sessions) != 0 {
			t.Errorf("generic mail named %+v: %q", h.Sessions, body)
		}
	}
	for surface, text := range g.surfaces(t, g.agent, g.chiefTok, "focus_search", `{"query":"node-7k"}`) {
		if strings.Contains(text, a) {
			t.Errorf("%s: the current-focus search found A, so the fixture does not reproduce the miss: %s", surface, text)
		}
	}
	for surface, text := range g.surfaces(t, g.agent, g.chiefTok, "focus_search", `{"query":"node-7k","since_hours":6}`) {
		var res struct {
			Results []struct{ ID, Title, At string } `json:"results"`
		}
		if err := json.Unmarshal([]byte(text), &res); err != nil {
			t.Fatalf("%s: %s", surface, text)
		}
		var ids []string
		for _, r := range res.Results {
			ids = append(ids, r.ID)
		}
		// Exactly A and the stopped agent: focus_search keeps every agent (as
		// the current-focus search does); D's node-7k row is 7 h old and the
		// offline mirror is not live; the agent's own focus says node-7k too
		// but focus_search does not leave the caller out.
		want := map[string]bool{a: true, "agent:" + strconv.FormatInt(stopped.ID, 10): true, "agent:" + strconv.FormatInt(g.agent.ID, 10): true}
		if len(ids) != len(want) {
			t.Errorf("%s: history search %v; want %v", surface, ids, want)
		}
		for _, id := range ids {
			if !want[id] {
				t.Errorf("%s: history search returned %s: %s", surface, id, text)
			}
		}
		if strings.Contains(text, sidOffline) || strings.Contains(text, "agent:"+strconv.FormatInt(d.ID, 10)+`"`) {
			t.Errorf("%s: an offline mirror or a 7 h old row is a hit: %s", surface, text)
		}
	}
	// since_hours is a recent-work window, at most a day: not a trawl through
	// the 30 days of history.
	for surface, text := range g.surfaces(t, g.agent, g.chiefTok, "focus_search", `{"query":"node-7k","since_hours":25}`) {
		if text != toolErr("since_hours is 1 to 24 (omit it to search the current focus)") {
			t.Errorf("%s: since_hours 25 was not refused: %s", surface, text)
		}
	}
	for surface, text := range g.surfaces(t, g.agent, g.chiefTok, "focus_search", `{"query":"node-7k","since_hours":24}`) {
		if isToolErr(text) || !strings.Contains(text, a) {
			t.Errorf("%s: control, since_hours 24: %s", surface, text)
		}
	}
}

// overviewOf decodes a sessions_overview answer.
type overviewOut struct {
	Untrusted string            `json:"untrusted"`
	Window    string            `json:"window"`
	Sessions  []json.RawMessage `json:"sessions"`
	Truncated string            `json:"truncated"`
}

func decodeOverview(t *testing.T, text string) (overviewOut, map[string]overviewSessionView) {
	t.Helper()
	var out overviewOut
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("not an overview: %v\n%s", err, text)
	}
	by := map[string]overviewSessionView{}
	for _, raw := range out.Sessions {
		var v overviewSessionView
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		v.raw = string(raw)
		by[v.ID] = v
	}
	return out, by
}

type overviewSessionView struct {
	ID, Kind, Name, Host, Status, Focus string
	RecentFocus                         []struct{ Title, At string } `json:"recent_focus"`
	Attention                           *struct{ Kind, Text, Since string }
	Transcript                          json.RawMessage
	raw                                 string
}

func (v overviewSessionView) lines(t *testing.T) []overviewLine {
	t.Helper()
	var ls []overviewLine
	if err := json.Unmarshal(v.Transcript, &ls); err != nil {
		t.Fatalf("%s: transcript is not a tail: %s", v.ID, v.Transcript)
	}
	return ls
}

// TestSessionsOverviewSaysWhatEachLiveSessionIsDoing: live sessions only (not
// the stopped agent, not the offline window, not the agent), each with its
// current focus, its distinct 6 h titles, its pending attention item and its
// last user/assistant messages — on both transports.
func TestSessionsOverviewSaysWhatEachLiveSessionIsDoing(t *testing.T) {
	g, b, c, d, _ := overviewReplay(t)
	g.chat(t, g.worker,
		agents.ChatMessage{Role: "user", Content: "first"},
		agents.ChatMessage{Role: "user", Content: "audit the open PRs"},
		agents.ChatMessage{Role: "assistant", Kind: "tool_call", ToolName: "Bash", ToolID: "t1", Content: `{"command":"gh pr list"}`},
		agents.ChatMessage{Role: "assistant", Content: "Three PRs are open."},
		agents.ChatMessage{Role: "user", Content: "start with the oldest"})
	if text := g.srv.AgentToolDispatch(g.asOwner, g.worker)("attention_ask", `{"question":"Merge the oldest PR now?"}`); isToolErr(text) {
		t.Fatal(text)
	}
	a := "agent:" + strconv.FormatInt(g.worker.ID, 10)
	for surface, text := range g.surfaces(t, g.agent, g.chiefTok, "sessions_overview", `{}`) {
		out, by := decodeOverview(t, text)
		if out.Untrusted != overviewUntrusted || out.Window != "6h0m0s" || out.Truncated != "" {
			t.Fatalf("%s: envelope %+v", surface, out)
		}
		want := []string{a, "agent:" + strconv.FormatInt(b.ID, 10), "agent:" + strconv.FormatInt(c.ID, 10),
			"agent:" + strconv.FormatInt(d.ID, 10), "mirror:" + sidLive}
		if len(by) != len(want) {
			t.Fatalf("%s: %d sessions; want %v:\n%s", surface, len(by), want, text)
		}
		for _, id := range want {
			if _, ok := by[id]; !ok {
				t.Fatalf("%s: %s missing:\n%s", surface, id, text)
			}
		}
		w := by[a]
		if w.Focus != "Auditing open pull requests" || w.Status != agents.StatusRunning || w.Kind == "" || w.Name == "" {
			t.Errorf("%s: worker row %s", surface, w.raw)
		}
		if len(w.RecentFocus) != 2 || w.RecentFocus[0].Title != "Auditing open pull requests" ||
			w.RecentFocus[1].Title != "Upgrading node-7k: drain and reboot" {
			t.Errorf("%s: worker's recent focus %+v; want the two titles, newest first", surface, w.RecentFocus)
		}
		if w.Attention == nil || w.Attention.Kind != attentionKindAsk || w.Attention.Text != "Merge the oldest PR now?" || w.Attention.Since == "" {
			t.Errorf("%s: worker's attention %+v", surface, w.Attention)
		}
		got := w.lines(t)
		if fmt.Sprint(got) != fmt.Sprint([]overviewLine{{"user", "audit the open PRs"}, {"assistant", "Three PRs are open."},
			{"user", "start with the oldest"}}) {
			t.Errorf("%s: worker's tail %+v; want its last three prose messages, no tool call", surface, got)
		}
		if strings.Contains(w.raw, "gh pr list") {
			t.Errorf("%s: a tool call reached the tail: %s", surface, w.raw)
		}
		m := by["mirror:"+sidLive]
		if m.Host != "box-1" || m.Kind != "mirror" || m.Focus != "Reading the cache dashboards" ||
			fmt.Sprint(m.lines(t)) != fmt.Sprint([]overviewLine{{"user", "check the dashboards"}, {"assistant", "Looking at the cache panel."}}) {
			t.Errorf("%s: mirror row %s", surface, m.raw)
		}
		// D's only node-7k row is 7 h old: outside the window.
		if dd := by["agent:"+strconv.FormatInt(d.ID, 10)]; len(dd.RecentFocus) != 1 || dd.RecentFocus[0].Title != "Idle" {
			t.Errorf("%s: D's recent focus %+v; want only the in-window title", surface, dd.RecentFocus)
		}
	}
}

// 🔴 TestTheOverviewTranscriptIsRedacted: a token-like string in a session's
// messages is replaced by the attention queue's redactor, and a long message
// is cut to its bound.
func TestTheOverviewTranscriptIsRedacted(t *testing.T) {
	g := newGatedFixture(t)
	g.chat(t, g.worker,
		agents.ChatMessage{Role: "user", Content: "push with GITHUB_TOKEN=ghp_FAKE0123abcd please"},
		agents.ChatMessage{Role: "assistant", Content: "Using Authorization: Bearer FAKEbearer0099 for it. " + strings.Repeat("x", 400)})
	text := g.srv.AgentToolDispatch(g.asOwner, g.agent)("sessions_overview", `{}`)
	_, by := decodeOverview(t, text)
	got := by["agent:"+strconv.FormatInt(g.worker.ID, 10)].lines(t)
	if len(got) != 2 || got[0].Text != "push with GITHUB_TOKEN=«redacted» please" ||
		!strings.HasPrefix(got[1].Text, "Using Authorization: «redacted» for it. xxx") ||
		len([]rune(got[1].Text)) != overviewTranscriptRunes || !strings.HasSuffix(got[1].Text, "…") {
		t.Fatalf("tail %+v", got)
	}
	for _, secret := range []string{"ghp_FAKE", "FAKEbearer"} {
		if strings.Contains(text, secret) {
			t.Fatalf("%s left in the overview:\n%s", secret, text)
		}
	}
}

// 🔴 TestTheOverviewIsByteCappedWithAMarker: many sessions with long histories
// and messages stay under the cap, newest-active first, and say how many were
// left out.
func TestTheOverviewIsByteCappedWithAMarker(t *testing.T) {
	g := newGatedFixture(t)
	sfx := strconv.FormatInt(time.Now().UnixNano(), 36)
	long := strings.Repeat("y", 290)
	var ids []string
	for i := 0; i < overviewMaxSessions; i++ {
		a := g.newAgent(t, g.owner, fmt.Sprintf("ovcap%02d-%s", i, sfx), "", fmt.Sprintf("ovcap%02d-tok-%s", i, sfx))
		for j := 0; j < focus.MaxRecentTitles; j++ {
			g.setFocus(t, focus.AgentSubject(a.ID), fmt.Sprintf("Long running task number %02d of session %02d, step", j, i), "")
		}
		g.chat(t, a, agents.ChatMessage{Role: "user", Content: long}, agents.ChatMessage{Role: "assistant", Content: long},
			agents.ChatMessage{Role: "user", Content: long})
		ids = append(ids, "agent:"+strconv.FormatInt(a.ID, 10))
	}
	text := g.srv.AgentToolDispatch(g.asOwner, g.agent)("sessions_overview", `{"limit":40}`)
	if len(text) > overviewMaxBytes {
		t.Fatalf("overview is %d bytes; cap %d", len(text), overviewMaxBytes)
	}
	out, by := decodeOverview(t, text)
	if len(out.Sessions) == 0 || len(out.Sessions) >= overviewMaxSessions || !strings.Contains(out.Truncated, "more session(s) not shown") {
		t.Fatalf("%d sessions, truncated %q; want a cut with its marker", len(out.Sessions), out.Truncated)
	}
	n, _ := strconv.Atoi(strings.Fields(out.Truncated)[0])
	if n+len(out.Sessions) != overviewMaxSessions+1 { // + the worker
		t.Fatalf("marker says %d left out beside %d shown; want %d in all", n, len(out.Sessions), overviewMaxSessions+1)
	}
	// Newest-active first: the last agent written is listed, the first is not.
	if _, ok := by[ids[len(ids)-1]]; !ok {
		t.Fatalf("the newest-active session was cut")
	}
	if _, ok := by[ids[0]]; ok {
		t.Fatalf("the oldest-active session was kept over newer ones")
	}
}

// 🔴 TestAQuietHostDoesNotStallTheOverview: a window whose host never answers
// the transcript watch costs the call one per-session timeout and is marked
// unavailable; the other sessions' tails are still read.
func TestAQuietHostDoesNotStallTheOverview(t *testing.T) {
	g := newGatedFixture(t)
	prev := overviewTranscriptTimeout
	overviewTranscriptTimeout = 300 * time.Millisecond
	t.Cleanup(func() { overviewTranscriptTimeout = prev })
	g.chat(t, g.worker, agents.ChatMessage{Role: "user", Content: "hello worker"})
	now := time.Now().Unix()
	g.srv.ext.Mirror = overviewMirror{
		hosts: []mirror.HostView{overviewHost("box-1", true,
			mirror.Window{Session: "work", ID: "@1", Index: 1, Active: true, Claude: true, ClaudeSession: sidQuiet, Activity: now},
			mirror.Window{Session: "work", ID: "@2", Index: 2, Claude: true, ClaudeSession: sidLive, Activity: now})},
		sids:   map[string]string{"@1": sidQuiet, "@2": sidLive},
		events: map[string][]cctranscript.Event{"@2": {{Kind: cctranscript.KindPrompt, Text: "still here"}}},
		hang:   map[string]bool{"@1": true},
	}
	start := time.Now()
	text := g.srv.AgentToolDispatch(g.asOwner, g.agent)("sessions_overview", `{}`)
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("the overview took %s with one quiet host; the per-session timeout is %s", took, overviewTranscriptTimeout)
	}
	_, by := decodeOverview(t, text)
	if q := by["mirror:"+sidQuiet]; string(q.Transcript) != `"unavailable (timed out)"` {
		t.Fatalf("quiet session: %s", q.raw)
	}
	if l := by["mirror:"+sidLive].lines(t); len(l) != 1 || l[0].Text != "still here" {
		t.Fatalf("the answering session's tail %+v", l)
	}
	if l := by["agent:"+strconv.FormatInt(g.worker.ID, 10)].lines(t); len(l) != 1 || l[0].Text != "hello worker" {
		t.Fatalf("the worker's tail %+v", l)
	}
}

// 🔴 TestTheOverviewHasATotalBudget: with many stalled hosts the call ends
// at its total budget, not after (sessions / parallel) per-session timeouts;
// every session the budget cut — mid-read or never reached — says so, and a
// session read before it keeps its tail.
func TestTheOverviewHasATotalBudget(t *testing.T) {
	g := newGatedFixture(t)
	prevT, prevB := overviewTranscriptTimeout, overviewBudget
	overviewTranscriptTimeout, overviewBudget = time.Second, 400*time.Millisecond
	t.Cleanup(func() { overviewTranscriptTimeout, overviewBudget = prevT, prevB })
	g.chat(t, g.worker, agents.ChatMessage{Role: "user", Content: "hello worker"})
	// The windows were active a minute ago, so the worker (active now) is read first.
	ago := time.Now().Add(-time.Minute).Unix()
	om := overviewMirror{sids: map[string]string{}, hang: map[string]bool{}}
	var ws []mirror.Window
	for i := 1; i <= overviewParallel+2; i++ {
		id := "@" + strconv.Itoa(i)
		sid := fmt.Sprintf("0b2c3d4e-ov-stall-%d", i)
		ws = append(ws, mirror.Window{Session: "work", ID: id, Index: i, Claude: true, ClaudeSession: sid, Activity: ago})
		om.sids[id], om.hang[id] = sid, true
	}
	om.hosts = []mirror.HostView{overviewHost("box-1", true, ws...)}
	g.srv.ext.Mirror = om
	start := time.Now()
	text := g.srv.AgentToolDispatch(g.asOwner, g.agent)("sessions_overview", `{}`)
	if took := time.Since(start); took > 900*time.Millisecond {
		t.Fatalf("the overview took %s; its total budget is %s (per session %s)", took, overviewBudget, overviewTranscriptTimeout)
	}
	_, by := decodeOverview(t, text)
	for i := 1; i <= overviewParallel+2; i++ {
		if q := by[fmt.Sprintf("mirror:0b2c3d4e-ov-stall-%d", i)]; string(q.Transcript) != `"unavailable (overview budget)"` {
			t.Errorf("stalled session %d: %s", i, q.raw)
		}
	}
	if l := by["agent:"+strconv.FormatInt(g.worker.ID, 10)].lines(t); len(l) != 1 || l[0].Text != "hello worker" {
		t.Fatalf("the worker's tail %+v", l)
	}
}

// TestSessionsOverviewAnswersAWorkerToo: it was the labelled agent's alone — a
// worker got the rank refusal on the gateway and "unknown tool" on /mcp. muster
// has no privileged agent, so a worker is answered on both surfaces, over its
// OWN OWNER's sessions (row-level security, not rank, is the scope).
func TestSessionsOverviewAnswersAWorkerToo(t *testing.T) {
	g := newGatedFixture(t)
	for surface, text := range g.surfaces(t, g.worker, g.workerTok, "sessions_overview", `{}`) {
		if isToolErr(text) || !strings.Contains(text, `"sessions":[`) {
			t.Errorf("%s: a worker was not answered: %s", surface, text)
		}
	}
	for surface, text := range g.surfaces(t, g.agent, g.chiefTok, "sessions_overview", `{}`) {
		if isToolErr(text) || !strings.Contains(text, `"sessions":[`) {
			t.Errorf("%s: the agent was not answered: %s", surface, text)
		}
	}
}
