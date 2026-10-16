package api

import (
	"context"
	"io"
	"log"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/pins"
)

// SESSION FOCUS ACROSS THE MIRROR SEAM: a window's Claude session id (the wire
// field) → the focus row keyed by it → the card; and a mirrored session's tail
// read through the host's transcript stream.

const focusSID = "0b2c3d4e-5f60-4718-8a9b-0c1d2e3f4a5b"

// memFocus is focus.Store in memory (one owner).
type memFocus struct {
	mu   sync.Mutex
	rows map[focus.Subject]focus.Focus
}

func (m *memFocus) Get(_ context.Context, s focus.Subject) (focus.Focus, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.rows[s]
	return f, ok, nil
}
func (m *memFocus) List(context.Context) ([]focus.Focus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []focus.Focus
	for _, f := range m.rows {
		out = append(out, f)
	}
	return out, nil
}
func (m *memFocus) Search(context.Context, string, int) ([]focus.Hit, error) { return nil, nil }
func (m *memFocus) Upsert(_ context.Context, s focus.Subject, w focus.Write) (focus.Focus, error) {
	t, d, err := focus.Clean(w.Title, w.Description)
	if err != nil {
		return focus.Focus{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	f := focus.Focus{Subject: s, Title: t, Description: d, Source: w.Source, Fingerprint: w.Fingerprint, UpdatedAt: time.Now()}
	m.rows[s] = f
	return f, nil
}
func (m *memFocus) RecordError(_ context.Context, s focus.Subject, msg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	f := m.rows[s]
	f.Subject, f.LastError, f.ErrorAt = s, msg, time.Now()
	m.rows[s] = f
	return nil
}
func (m *memFocus) Owners(context.Context) ([]int64, error) { return []int64{1}, nil }

func focusMirrorServer(t *testing.T, fm *fakeMirror, store *memFocus, summ focus.Summarizer) (*Server, *httptest.Server) {
	t.Helper()
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Notes: stubNotesStore{}, Agents: mirrorAgents{}, SessionLiveness: stubLiveness{},
		Mirror: fm, Focus: store, FocusSummarizer: summ})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func operatorCtx() context.Context {
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: authz.ViaSession})
}

// claudeWindows is a per-window host with one Claude window that reports a
// session id (@1) and one, as an older muster-mirror lists it, that does not (@2).
func claudeWindows() []mirror.Window {
	return []mirror.Window{
		{Session: "main", ID: "@1", Index: 1, Active: true, Claude: true, ClaudeSession: focusSID, Activity: 1_700_000_000},
		{Session: "main", ID: "@2", Index: 2, Claude: true, Activity: 1_700_000_000},
	}
}

// TestAMirrorCardShowsTheFocusOfItsClaudeSession: the agents list draws the
// focus keyed by the window's Claude session id on THAT window's card, and
// none on the window whose host reported no id.
func TestAMirrorCardShowsTheFocusOfItsClaudeSession(t *testing.T) {
	store := &memFocus{rows: map[focus.Subject]focus.Focus{
		focus.MirrorSubject(focusSID): {Subject: focus.MirrorSubject(focusSID), Title: "Porting the importer", Description: "Dates next.", UpdatedAt: time.Now()},
	}}
	fm := &fakeMirror{online: true, v2: &fakeMirrorV2{}, windows: claudeWindows()}
	s, ts := focusMirrorServer(t, fm, store, nil)
	_, body := mirrorGet(t, s, ts, "/ui/agents")
	if n := strings.Count(body, "data-card-focus"); n != 1 {
		t.Fatalf("%d focus chips, want exactly 1 (only @1 reports a session id)", n)
	}
	if !strings.Contains(body, "Porting the importer") || !strings.Contains(body, "Dates next.") {
		t.Fatal("the focus text is not on the card")
	}
	cards := s.mirrorCards(operatorCtx(), pins.Set{}, s.focusIndex(operatorCtx()))
	for _, c := range cards {
		if (c.Window == "@1") != (c.Focus != nil) {
			t.Fatalf("window %s: focus %+v", c.Window, c.Focus)
		}
	}
}

type recordingSummarizer struct {
	mu    sync.Mutex
	tails []string
}

func (r *recordingSummarizer) Summarize(_ context.Context, _ int64, tail string) (focus.Summary, error) {
	r.mu.Lock()
	r.tails = append(r.tails, tail)
	r.mu.Unlock()
	return focus.Summary{Title: "Fixing the mirror reconnect", Description: "Backoff is the suspect."}, nil
}

// TestAMirroredSessionsFocusIsRefreshedFromItsTranscriptStream: a refresh of
// mirror:<sid> reads the window's backfill through WatchWindow (prompts,
// replies and tool calls — not tool output), stops at the prompt index, and
// stores the summary under the session id; the card then shows it.
func TestAMirroredSessionsFocusIsRefreshedFromItsTranscriptStream(t *testing.T) {
	store := &memFocus{rows: map[focus.Subject]focus.Focus{}}
	summ := &recordingSummarizer{}
	fm := &fakeMirror{online: true, v2: &fakeMirrorV2{batches: []mirror.Msg{
		{Type: mirror.TypeEvents, Reset: true, Events: []cctranscript.Event{
			{Kind: cctranscript.KindPrompt, Text: "why does the tunnel drop?"},
			{Kind: cctranscript.KindToolCall, Tool: "Bash", Target: "go test ./internal/mirror"},
			{Kind: cctranscript.KindToolResult, Output: "SECRET-TOOL-OUTPUT"},
			{Kind: cctranscript.KindText, Text: "The backoff resets too early."},
		}},
		{Type: mirror.TypeEvents, PromptIndex: true},
	}}, windows: claudeWindows()}
	s, ts := focusMirrorServer(t, fm, store, summ)
	start := time.Now()
	o, err := s.refresher().Refresh(operatorCtx(), 1, focus.MirrorSubject(focusSID))
	if err != nil || o != focus.OutcomeUpdated {
		t.Fatalf("refresh: %v %v", o, err)
	}
	if time.Since(start) > focusTailQuiet {
		t.Errorf("the tail read took %s: it did not stop at the prompt index", time.Since(start))
	}
	want := "user: why does the tunnel drop?\ntool: Bash go test ./internal/mirror\nassistant: The backoff resets too early."
	if len(summ.tails) != 1 || summ.tails[0] != want {
		t.Fatalf("tail = %q, want %q", summ.tails, want)
	}
	if fm.asked[len(fm.asked)-1] != "@1" {
		t.Fatalf("watched window %q, want @1", fm.asked)
	}
	_, body := mirrorGet(t, s, ts, "/ui/agents")
	if !strings.Contains(body, "Fixing the mirror reconnect") {
		t.Fatal("the refreshed focus is not on the card")
	}
}

// TestAMirroredTailFromAnOlderHostEndsOnQuiet: a host that sends no prompt
// index (an older muster-mirror) still yields its backfill, once the stream
// has been quiet for focusTailQuiet.
func TestAMirroredTailFromAnOlderHostEndsOnQuiet(t *testing.T) {
	fm := &fakeMirror{online: true, v2: &fakeMirrorV2{batches: []mirror.Msg{
		{Type: mirror.TypeEvents, Reset: true, Events: []cctranscript.Event{{Kind: cctranscript.KindPrompt, Text: "hello"}}},
	}}, windows: claudeWindows()}
	s, _ := focusMirrorServer(t, fm, &memFocus{rows: map[focus.Subject]focus.Focus{}}, nil)
	e, ok := s.mirrorEntryForSession(operatorCtx(), focusSID)
	if !ok {
		t.Fatal("no entry for the session id")
	}
	start := time.Now()
	got, err := s.mirrorTail(operatorCtx(), e)
	if err != nil || len(got) != 1 || got[0].Text != "hello" {
		t.Fatalf("tail = %+v, %v", got, err)
	}
	if d := time.Since(start); d < focusTailQuiet || d > focusTailTimeout {
		t.Fatalf("took %s, want about %s", d, focusTailQuiet)
	}
	if _, ok := s.mirrorEntryForSession(operatorCtx(), "no-such-session"); ok {
		t.Fatal("an unknown session id resolved to a window")
	}
}

// TestThePeriodicPassRunsAsEachOwner is the SEAM the tests above skip: they
// call Refresh with a principal the test built, while the real pass
// (RunFocusRefresh → Refresher.Pass) builds its own through focusPrincipal for
// each owner Store.Owners names. That principal's door must be one
// authz.WithPrincipal accepts — measured: authz.ViaFocus missing from its
// request doors made the FIRST periodic pass panic (in a bare goroutine, so
// the whole server) — and it must act as THAT owner: the fake host answers
// only user 1, so a pass acting as anyone else reads no tail and writes nothing.
func TestThePeriodicPassRunsAsEachOwner(t *testing.T) {
	store := &memFocus{rows: map[focus.Subject]focus.Focus{}}
	summ := &recordingSummarizer{}
	fm := &fakeMirror{online: true, v2: &fakeMirrorV2{batches: []mirror.Msg{
		{Type: mirror.TypeEvents, Reset: true, Events: []cctranscript.Event{{Kind: cctranscript.KindPrompt, Text: "fix the reconnect"}}},
		{Type: mirror.TypeEvents, PromptIndex: true},
	}}, windows: claudeWindows()}
	s, _ := focusMirrorServer(t, fm, store, summ)
	// No dispatched agents: this pass is about the mirrored owner (the stub the
	// other tests use lists agents it cannot read chats for).
	s.ext.Agents = noAgents{}
	if err := s.refresher().Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	f, ok := store.rows[focus.MirrorSubject(focusSID)]
	if !ok || f.Title != "Fixing the mirror reconnect" || len(summ.tails) != 1 {
		t.Fatalf("the pass did not refresh the owner's session: %+v (summarizer saw %q)", f, summ.tails)
	}
}

// noAgents is an agents.Store that lists none.
type noAgents struct{ agents.Store }

func (noAgents) List(context.Context) ([]agents.Agent, error) { return nil, nil }

type failingSummarizer struct{ err error }

func (f failingSummarizer) Summarize(context.Context, int64, string) (focus.Summary, error) {
	return focus.Summary{}, f.err
}

// TestAFailedRefreshShowsItsReasonOnTheCard: the summarizer's failure reaches
// the mirror card as its short reason — never the provider's text — first as
// "no focus", then, after a success, as a stale one.
func TestAFailedRefreshShowsItsReasonOnTheCard(t *testing.T) {
	store := &memFocus{rows: map[focus.Subject]focus.Focus{}}
	fm := &fakeMirror{online: true, v2: &fakeMirrorV2{batches: []mirror.Msg{
		{Type: mirror.TypeEvents, Reset: true, Events: []cctranscript.Event{{Kind: cctranscript.KindPrompt, Text: "hello"}}},
		{Type: mirror.TypeEvents, PromptIndex: true},
	}}, windows: claudeWindows()}
	s, ts := focusMirrorServer(t, fm, store, failingSummarizer{err: &focus.HTTPError{Status: 502, Body: "sk-or-v1-provider-secret"}})
	if _, err := s.refresher().Refresh(operatorCtx(), 1, focus.MirrorSubject(focusSID)); err == nil {
		t.Fatal("the failing refresh succeeded")
	}
	_, body := mirrorGet(t, s, ts, "/ui/agents")
	if !strings.Contains(body, "No focus: summarizer error (HTTP 502)") || !strings.Contains(body, "data-focus-stale") {
		t.Fatalf("the card does not say why there is no focus:\n%s", body)
	}
	if strings.Contains(body, "sk-or-v1") {
		t.Fatal("the provider's error text reached the page")
	}
	// A focus, then a later failure: the card shows both.
	store.mu.Lock()
	f := store.rows[focus.MirrorSubject(focusSID)]
	f.Title, f.UpdatedAt = "Fixing the reconnect", f.ErrorAt.Add(-time.Minute)
	store.rows[focus.MirrorSubject(focusSID)] = f
	store.mu.Unlock()
	_, body = mirrorGet(t, s, ts, "/ui/agents")
	if !strings.Contains(body, "Fixing the reconnect") || !strings.Contains(body, "Last refresh failed: summarizer error (HTTP 502)") {
		t.Fatalf("a stale focus does not say its refresh failed:\n%s", body)
	}
}
