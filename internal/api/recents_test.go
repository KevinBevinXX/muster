package api

import (
	"context"
	"io"
	"log"
	"net/http/httptest"
	"regexp"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/pins"
	"github.com/ZacxDev/muster/internal/recents"
	"github.com/ZacxDev/muster/internal/ui"
)

// memRecents is recents.Store in memory, for ONE viewer: a list of visits,
// newest first, deduplicated as the real upsert is. The Postgres behaviour
// (RLS, the bound, the cascade) is internal/recents' own tests'.
type memRecents struct {
	mu sync.Mutex
	es []recents.Entry
}

func (m *memRecents) put(e recents.Entry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []recents.Entry{e}
	for _, x := range m.es {
		if x.AgentID != e.AgentID || x.Mirror != e.Mirror {
			out = append(out, x)
		}
	}
	m.es = out
}

func (m *memRecents) List(_ context.Context, limit int) ([]recents.Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit > len(m.es) {
		limit = len(m.es)
	}
	return append([]recents.Entry(nil), m.es[:limit]...), nil
}

func (m *memRecents) VisitAgent(_ context.Context, id int64) error {
	m.put(recents.Entry{AgentID: id})
	return nil
}

func (m *memRecents) VisitMirror(_ context.Context, k pins.MirrorKey) error {
	m.put(recents.Entry{Mirror: k})
	return nil
}

// labels is the store's content as "a<id>" / "<session><window>", newest first.
func (m *memRecents) labels() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, e := range m.es {
		if e.IsMirror() {
			out = append(out, e.Mirror.Session+e.Mirror.Window)
		} else {
			out = append(out, "a"+itoa(e.AgentID))
		}
	}
	return out
}

// TestOpeningAnEntryRecordsAVisit: the agent detail page, a window's rendered
// view and its terminal page each record the entry they RENDERED — by the
// address the card links by — and a redirect records nothing (the page it lands
// on does). Red before: no page recorded anything.
func TestOpeningAnEntryRecordsAVisit(t *testing.T) {
	mem := &memRecents{}
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Notes: stubNotesStore{}, Agents: newSendAgents(), SessionLiveness: stubLiveness{},
		Provisioner: newFakeTerminal(), Mirror: goldHost(), Recents: mem})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	steps := []struct {
		path string
		want []string
	}{
		// A bare open redirects to ?session=; only the rendered page records.
		{"/agents/keen-wren", nil},
		{"/agents/keen-wren?session=41", []string{"a7"}},
		// The view of a Claude window.
		{ref("scratch2", "@9").ViewPath(), []string{"scratch2@9", "a7"}},
		// The view of a NON-Claude window redirects to its terminal: nothing yet.
		{ref("scratch2", "@4").ViewPath(), []string{"scratch2@9", "a7"}},
		{ref("scratch2", "@4").TerminalPath(), []string{"scratch2@4", "scratch2@9", "a7"}},
		// Only the session: redirected to its active window, which records.
		{ui.MirrorTerminalPath("ws", "scratch2"), []string{"scratch2@4", "scratch2@9", "a7"}},
		{ref("scratch2", "@7").TerminalPath(), []string{"scratch2@7", "scratch2@4", "scratch2@9", "a7"}},
		// Revisiting moves the entry to the front; it is not a second entry.
		{"/agents/keen-wren?session=41", []string{"a7", "scratch2@7", "scratch2@4", "scratch2@9"}},
		// An entry the viewer cannot open (404) records nothing.
		{"/agents/no-such-agent?session=1", []string{"a7", "scratch2@7", "scratch2@4", "scratch2@9"}},
		{ui.MirrorRef{Host: "not-mine", Session: "x"}.TerminalPath(), []string{"a7", "scratch2@7", "scratch2@4", "scratch2@9"}},
	}
	for _, st := range steps {
		mirrorGet(t, s, ts, st.path)
		if got := mem.labels(); !eqStrings(got, st.want) {
			t.Fatalf("after GET %s: recents = %v, want %v", st.path, got, st.want)
		}
	}
}

func eqStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var recentEntryHref = regexp.MustCompile(`<a href="([^"]+)" hx-boost="false" data-recent-entry="(agent|mirror)"`)

// TestTheListDrawsTheRecentStrip: GET /ui/agents sends the strip OUT OF BAND,
// resolved against the cards it renders: newest first, pinned entries and
// entries that are gone skipped. Fixture: agents 5, 6, 7 and windows main@1,
// chief@2 (pinWindows); agent 6 and chief@2 are pinned; agent 99 and window
// main@9 do not exist.
func TestTheListDrawsTheRecentStrip(t *testing.T) {
	mem := &memRecents{}
	for _, v := range []recents.Entry{
		{AgentID: 5},
		{Mirror: pins.MirrorKey{Host: "ws", Session: "chief", Window: "@2"}},
		{AgentID: 99},
		{Mirror: pins.MirrorKey{Host: "ws", Session: "main", Window: "@1"}},
		{AgentID: 6},
		{Mirror: pins.MirrorKey{Host: "ws", Session: "main", Window: "@9"}},
		{AgentID: 7},
	} {
		mem.put(v) // put prepends: 7 ends up newest
	}
	mp := &memPins{}
	s, h := pinServer(t, &fakeMirror{online: true, windows: pinWindows()}, mp)
	s.ext.Recents = mem
	mp.pinFor(1).Agents[6] = true
	mp.pinFor(1).Mirrors[pins.MirrorKey{Host: "ws", Session: "chief", Window: "@2"}] = true

	body := getOwed(t, s, h, "/ui/agents").Body.String()
	if !regexp.MustCompile(`<nav id="agents-recent" aria-label="Recently viewed" hx-swap-oob="true"`).MatchString(body) {
		t.Fatalf("no out-of-band strip in the list:\n%s", body)
	}
	var got []string
	for _, m := range recentEntryHref.FindAllStringSubmatch(body, -1) {
		got = append(got, m[1])
	}
	want := []string{"/agents/idle-newest", amp(ref("main", "@1").TerminalPath()), "/agents/chatting-oldest"}
	if !eqStrings(got, want) {
		t.Errorf("strip = %v, want %v (newest first, without the pinned 6 / chief@2 and the gone 99 / main@9)", got, want)
	}
	// Control: with nothing pinned the pinned two are drawn, in their places.
	mp.pinFor(1).Agents[6] = false
	delete(mp.pinFor(1).Mirrors, pins.MirrorKey{Host: "ws", Session: "chief", Window: "@2"})
	body = getOwed(t, s, h, "/ui/agents").Body.String()
	got = nil
	for _, m := range recentEntryHref.FindAllStringSubmatch(body, -1) {
		got = append(got, m[1])
	}
	want = []string{"/agents/idle-newest", "/agents/status-moved", amp(ref("main", "@1").TerminalPath()),
		amp(ref("chief", "@2").TerminalPath()), "/agents/chatting-oldest"}
	if !eqStrings(got, want) {
		t.Errorf("unpinned strip = %v, want %v", got, want)
	}
}
