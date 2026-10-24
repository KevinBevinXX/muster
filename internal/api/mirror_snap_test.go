package api

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/ui"
)

// snapServer is the real mux with a per-window mirrored host (goldHost), one
// agent and a recents store.
func snapServer(t *testing.T) (*Server, *httptest.Server, *fakeMirror, *memRecents) {
	t.Helper()
	mem := &memRecents{}
	fm := goldHost()
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Notes: stubNotesStore{}, Agents: newSendAgents(), SessionLiveness: stubLiveness{},
		Provisioner: newFakeTerminal(), Mirror: fm, Recents: mem})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts, fm, mem
}

// Every page that reads or warms snapshots — the rendered view, the Agents
// list, an agent's page — carries the store's tag naming the REQUEST'S
// principal and the running build (shellFeatures). Red before: none did.
func TestThePagesThatReadOrWarmSnapshotsCarryThePrincipalAndBuild(t *testing.T) {
	s, ts, _, _ := snapServer(t)
	tag := regexp.MustCompile(`<script src="/static/mirror-snap\.js\?v=[0-9a-f]+" defer(?:="")? data-snap-user="([0-9]+)" data-snap-build="([^"]*)"`)
	pages := map[string]string{}
	for _, path := range []string{ref("scratch2", "@9").ViewPath(), "/agents/keen-wren?session=41"} {
		resp, body := mirrorGet(t, s, ts, path)
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s: %d %s", path, resp.StatusCode, body)
		}
		pages[path] = body
	}
	// The Agents list, over the list's own fixture server.
	ls, h, _ := listServer(t)
	req := httptest.NewRequest(http.MethodGet, "/agents", nil)
	admit(ls, req)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /agents: %d", rec.Code)
	}
	pages["/agents"] = rec.Body.String()
	for path, body := range pages {
		m := tag.FindStringSubmatch(body)
		if m == nil {
			t.Errorf("GET %s: no snapshot store tag", path)
			continue
		}
		if m[1] == "0" || m[2] != BuildVersion {
			t.Errorf("GET %s: user %s build %q, want the principal (non-zero) and %q", path, m[1], m[2], BuildVersion)
		}
	}
}

// Sign-in and every other auth page delete the snapshots (ui.SnapPurgeHTML).
// Red before: /login carried no script.
func TestTheLoginPageDeletesTheSnapshots(t *testing.T) {
	_, ts, _, _ := snapServer(t)
	resp, err := ts.Client().Get(ts.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), ui.SnapPurgeHTML()) {
		t.Fatalf("/login does not delete the snapshots:\n%s", b)
	}
}

// 🔴 INVARIANT GUARD, NOT REGRESSION COVERAGE: the view's socket never
// recorded a visit, before this change or after. It is pinned because
// warming (web/static/mirror-snap.js) relies on it — it opens the socket of
// a window the viewer has NOT opened, and that must not move the window in
// Recent. The positive control is the page itself, which does record.
func TestTheViewSocketRecordsNoVisitAndThePageDoes(t *testing.T) {
	s, ts, fm, mem := snapServer(t)
	c, _, err := dialTerm(t, s, ts, ref("scratch2", "@9").ViewWSPath())
	if err != nil {
		t.Fatal(err)
	}
	// The handler is past its lookups once the host has been asked to watch.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if w, _ := fm.v2.snapshot(); len(w) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the socket never asked the host to watch")
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.CloseNow()
	time.Sleep(50 * time.Millisecond)
	if got := mem.labels(); len(got) != 0 {
		t.Fatalf("the view socket recorded a visit: %v", got)
	}
	mirrorGet(t, s, ts, ref("scratch2", "@9").ViewPath())
	if got := mem.labels(); !eqStrings(got, []string{"scratch2@9"}) {
		t.Fatalf("POSITIVE CONTROL: the page recorded %v, want [scratch2@9]", got)
	}
}
