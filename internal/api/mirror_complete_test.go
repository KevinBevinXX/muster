package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE COMPOSER MENU'S ROUTE (mirror_complete.go): owner-scoped, always the
// built-ins, the host's catalog merged in when it reports one, and every
// string encoded so it cannot be markup.

// catalogMirror is fakeMirror plus the plugin's catalog request: it answers
// cat (or err), and records every request as "<user> <host> <session>
// <window>".
type catalogMirror struct {
	*fakeMirror
	cmu   sync.Mutex
	cat   mirror.Catalog
	err   error
	asked []string
}

func (m *catalogMirror) CatalogWindow(ctx context.Context, host, session, window string) (mirror.Catalog, error) {
	m.cmu.Lock()
	defer m.cmu.Unlock()
	u := m.user(ctx)
	m.asked = append(m.asked, strings.Join([]string{itoaUser(u), host, session, window}, " "))
	if u != 1 || host != "ws" {
		return mirror.Catalog{}, mirror.ErrHostOffline
	}
	return m.cat, m.err
}

func itoaUser(u int64) string { b, _ := json.Marshal(u); return string(b) }

func completePath(host, session string) string {
	return ui.MirrorCompletePath + "?host=" + host + "&session=" + session
}

// menuOf GETs the route as the signed-in operator (user 1).
func menuOf(t *testing.T, s *Server, ts *httptest.Server, path string) (int, composerMenu, string) {
	t.Helper()
	resp, body := mirrorGet(t, s, ts, path)
	var m composerMenu
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal([]byte(body), &m); err != nil {
			t.Fatalf("%s: not JSON: %v: %s", path, err, body)
		}
		if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
			t.Fatalf("%s: Cache-Control %q: the owner's host content must not be cached", path, cc)
		}
	}
	return resp.StatusCode, m, body
}

func menuNames(m composerMenu) map[string]string {
	out := map[string]string{}
	for _, e := range m.Commands {
		out[e.Name] = e.Source
	}
	return out
}

// TestTheMenuMergesTheHostsCatalogWithTheBuiltIns: the host's skills, the
// window's project commands and the snippets, beside the built-ins — and a
// host command named like a built-in shadows it.
func TestTheMenuMergesTheHostsCatalogWithTheBuiltIns(t *testing.T) {
	cm := &catalogMirror{fakeMirror: &fakeMirror{online: true, interactive: true, v2: &fakeMirrorV2{}}, cat: mirror.Catalog{
		Commands: []mirror.CatalogEntry{
			{Name: "handoff", Description: "write the handoff", Source: mirror.SourceUser, Kind: mirror.KindSkill},
			{Name: "compact", Description: "the project's own compact", Source: mirror.SourceProject, Kind: mirror.KindCommand},
		},
		Snippets: []mirror.Snippet{{Trigger: ":eos", Label: "End of session", Body: "reflect and hand off"}},
	}}
	s, ts := viewServer(t, cm.fakeMirror)
	s.ext.Mirror = cm
	code, m, _ := menuOf(t, s, ts, completePath("ws", "main"))
	if code != http.StatusOK || !m.Host || m.Note != "" {
		t.Fatalf("%d host=%v note=%q", code, m.Host, m.Note)
	}
	n := menuNames(m)
	if n["handoff"] != mirror.SourceUser || n["clear"] != mirror.SourceBuiltin || n["compact"] != mirror.SourceProject {
		t.Fatalf("commands: %v", n)
	}
	if len(m.Snippets) != 1 || m.Snippets[0].Trigger != ":eos" {
		t.Fatalf("snippets: %+v", m.Snippets)
	}
	if got := cm.asked; len(got) != 1 || got[0] != "1 ws main " {
		t.Fatalf("the plugin was asked %q, want once, for user 1's ws/main", got)
	}
}

// TestAnotherOwnerGetsA404AndTheHostIsNeverAsked: the route resolves the
// window through the REQUEST principal's hosts; user 2 — who has none called
// "ws" — gets a 404 before the plugin is asked anything, as does a session
// user 1's host does not list.
func TestAnotherOwnerGetsA404AndTheHostIsNeverAsked(t *testing.T) {
	cm := &catalogMirror{fakeMirror: &fakeMirror{online: true, interactive: true, v2: &fakeMirrorV2{}}}
	s, ts := viewServer(t, cm.fakeMirror)
	s.ext.Mirror = cm
	as := func(user int64, path string) int {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: user, Role: authz.RoleMember, Via: authz.ViaSession})
		rec := httptest.NewRecorder()
		s.handleMirrorComplete(rec, req.WithContext(ctx))
		return rec.Code
	}
	if code := as(2, completePath("ws", "main")); code != http.StatusNotFound {
		t.Fatalf("user 2: %d, want 404", code)
	}
	if code, _, _ := menuOf(t, s, ts, completePath("ws", "not-listed")); code != http.StatusNotFound {
		t.Fatalf("an unlisted session: %d, want 404", code)
	}
	if len(cm.asked) != 0 {
		t.Fatalf("the plugin was asked %q for a request that must 404 first", cm.asked)
	}
	// The positive control: the owner, the same address, IS answered.
	if code := as(1, completePath("ws", "main")); code != http.StatusOK || len(cm.asked) != 1 {
		t.Fatalf("user 1: %d, asked %q", code, cm.asked)
	}
}

// TestAnOlderHostGetsTheBuiltInsAlone: a host that does not report a catalog
// (the plugin says ErrCatalogUnsupported), a plugin with no catalog request at
// all, an offline host and a host that failed each answer 200 with the
// built-ins and a note — never an error page, never the host's entries.
func TestAnOlderHostGetsTheBuiltInsAlone(t *testing.T) {
	for _, c := range []struct {
		name    string
		mirror  func(*fakeMirror) Mirror
		online  bool
		note    string
		wantAsk bool
	}{
		{"a host that predates the catalog", func(f *fakeMirror) Mirror { return &catalogMirror{fakeMirror: f, err: mirror.ErrCatalogUnsupported} }, true, composerMenuNoteOld, true},
		{"a plugin with no catalog request", func(f *fakeMirror) Mirror { return f }, true, composerMenuNoteOld, false},
		{"an offline host", func(f *fakeMirror) Mirror { return &catalogMirror{fakeMirror: f} }, false, composerMenuNoteOffline, false},
		{"a host that timed out", func(f *fakeMirror) Mirror { return &catalogMirror{fakeMirror: f, err: mirror.ErrCatalogTimeout} }, true, composerMenuNoteFailed, true},
	} {
		f := &fakeMirror{online: c.online, interactive: true, v2: &fakeMirrorV2{}}
		s, ts := viewServer(t, f)
		mm := c.mirror(f)
		s.ext.Mirror = mm
		code, m, _ := menuOf(t, s, ts, completePath("ws", "main"))
		if code != http.StatusOK || m.Host || m.Note != c.note {
			t.Errorf("%s: %d host=%v note=%q, want 200, false, %q", c.name, code, m.Host, m.Note, c.note)
		}
		n := menuNames(m)
		if len(n) != len(mirror.BuiltinCommands) || n["compact"] != mirror.SourceBuiltin {
			t.Errorf("%s: %d commands %v, want the %d built-ins", c.name, len(n), n, len(mirror.BuiltinCommands))
		}
		if cm, ok := mm.(*catalogMirror); ok && (len(cm.asked) > 0) != c.wantAsk {
			t.Errorf("%s: asked %q", c.name, cm.asked)
		}
	}
}

// TestTheMenusStringsCannotBeMarkup: a host's description is host file
// content; the route's JSON writes `<`, `>` and `&` escaped, so not one raw
// angle bracket of it reaches the page (which draws it as text besides).
func TestTheMenusStringsCannotBeMarkup(t *testing.T) {
	evil := `</script><img src=x onerror="alert(1)">&amp;`
	cm := &catalogMirror{fakeMirror: &fakeMirror{online: true, interactive: true, v2: &fakeMirrorV2{}}, cat: mirror.Catalog{
		Commands: []mirror.CatalogEntry{{Name: "evil", Description: evil, Source: mirror.SourceUser, Kind: mirror.KindSkill}},
		Snippets: []mirror.Snippet{{Trigger: ":evil", Label: evil, Body: evil}},
	}}
	s, ts := viewServer(t, cm.fakeMirror)
	s.ext.Mirror = cm
	code, m, body := menuOf(t, s, ts, completePath("ws", "main"))
	if code != http.StatusOK {
		t.Fatalf("%d", code)
	}
	if strings.ContainsAny(body, "<>") || strings.Contains(body, "&amp;") {
		t.Fatalf("the JSON carries raw markup characters: %s", body)
	}
	if !strings.Contains(body, `\u003c/script\u003e`) {
		t.Fatalf("the description is not in the body, escaped: %s", body)
	}
	// And it decodes back to exactly what the host wrote: escaped, not mangled.
	for _, e := range m.Commands {
		if e.Name == "evil" && e.Description != evil {
			t.Fatalf("the description decodes to %q, want %q", e.Description, evil)
		}
	}
	if m.Snippets[0].Body != evil {
		t.Fatalf("the snippet decodes to %q", m.Snippets[0].Body)
	}
}
