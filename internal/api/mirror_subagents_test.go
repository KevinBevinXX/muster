package api

import (
	"context"
	"html"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE SUBAGENT CARDS' server half: the view socket draws an Agent call as a
// card only for a host that said it reports subagents, and relays summaries as
// redraws of the card; the tail route is owner-scoped and always a fragment.

// subagentMirror is fakeMirror plus the plugin's tail request.
type subagentMirror struct {
	*fakeMirror
	smu   sync.Mutex
	tail  mirror.SubagentTail
	err   error
	asked []string
}

func (m *subagentMirror) SubagentTail(ctx context.Context, host, session, window, agent string) (mirror.SubagentTail, error) {
	m.smu.Lock()
	defer m.smu.Unlock()
	u := m.user(ctx)
	m.asked = append(m.asked, strings.Join([]string{itoaUser(u), host, session, window, agent}, " "))
	if u != 1 || host != "ws" {
		return mirror.SubagentTail{}, mirror.ErrHostOffline
	}
	return m.tail, m.err
}

func subagentPath(host, session, agent string) string {
	return ui.MirrorSubagentPath + "?host=" + host + "&session=" + session + "&agent=" + agent
}

func TestTheSubagentTailRouteIsTheOwnersAndAlwaysAFragment(t *testing.T) {
	sm := &subagentMirror{fakeMirror: &fakeMirror{online: true, interactive: true, v2: &fakeMirrorV2{}},
		tail: mirror.SubagentTail{Agent: "a1", Steps: []mirror.SubagentStep{{Tool: "Bash", Text: "go test <b>x</b>"}}}}
	s, ts := viewServer(t, sm.fakeMirror)
	s.ext.Mirror = sm
	as := func(user int64, path string) (int, string) {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: user, Role: authz.RoleMember, Via: authz.ViaSession})
		rec := httptest.NewRecorder()
		s.handleMirrorSubagent(rec, req.WithContext(ctx))
		return rec.Code, rec.Body.String()
	}
	if code, _ := as(2, subagentPath("ws", "main", "a1")); code != http.StatusNotFound {
		t.Fatalf("user 2: %d, want 404", code)
	}
	if code, _ := as(1, subagentPath("ws", "not-listed", "a1")); code != http.StatusNotFound {
		t.Fatalf("an unlisted session: %d, want 404", code)
	}
	if code, _ := as(1, subagentPath("ws", "main", "..%2Fa1")); code != http.StatusBadRequest {
		t.Fatalf("a path as the agent: %d, want 400", code)
	}
	if len(sm.asked) != 0 {
		t.Fatalf("the plugin was asked %q for requests that must be refused first", sm.asked)
	}
	resp, body := mirrorGet(t, s, ts, subagentPath("ws", "main", "a1"))
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "data-subagent-step") || !strings.Contains(body, "go test &lt;b&gt;x&lt;/b&gt;") ||
		!strings.Contains(resp.Header.Get("Cache-Control"), "no-store") {
		t.Fatalf("owner: %d %q %s", resp.StatusCode, resp.Header.Get("Cache-Control"), body)
	}
	if len(sm.asked) != 1 || sm.asked[0] != "1 ws main  a1" {
		t.Fatalf("asked %q", sm.asked)
	}

	for _, c := range []struct {
		name string
		m    func(*fakeMirror) Mirror
		on   bool
		note string
	}{
		{"an older host", func(f *fakeMirror) Mirror { return &subagentMirror{fakeMirror: f, err: mirror.ErrSubagentsUnsupported} }, true, subagentNoteOld},
		{"a plugin without the request", func(f *fakeMirror) Mirror { return f }, true, subagentNoteOld},
		{"an offline host", func(f *fakeMirror) Mirror { return &subagentMirror{fakeMirror: f} }, false, subagentNoteOffline},
		{"a host that timed out", func(f *fakeMirror) Mirror { return &subagentMirror{fakeMirror: f, err: mirror.ErrSubagentTailTimeout} }, true, subagentNoteFailed},
	} {
		f := &fakeMirror{online: c.on, interactive: true, v2: &fakeMirrorV2{}}
		s, ts := viewServer(t, f)
		s.ext.Mirror = c.m(f)
		resp, body := mirrorGet(t, s, ts, subagentPath("ws", "main", "a1"))
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, html.EscapeString(c.note)) {
			t.Errorf("%s: %d %s, want 200 and %q", c.name, resp.StatusCode, body, c.note)
		}
	}
}

// The socket: an Agent call is a card when the Reset batch carries
// SubagentList, and a summaries message redraws that card's summary; without
// SubagentList (an older host) the same call is a tool row.
func TestTheViewSocketDrawsSubagentCardsForAHostThatReportsThem(t *testing.T) {
	call := cctranscript.Event{Kind: cctranscript.KindToolCall, ToolID: "toolu_a", Tool: "Agent", Target: "Survey",
		Input: `{"description":"Survey","prompt":"p","subagent_type":"Explore"}`}
	for _, reports := range []bool{true, false} {
		batches := []mirror.Msg{{Type: mirror.TypeEvents, Reset: true, SubagentList: reports, Gen: "g", Offset: 10, Events: []cctranscript.Event{call}}}
		if reports {
			batches = append(batches, mirror.Msg{Type: mirror.TypeEvents, Gen: "g", Offset: 10, SubagentList: true, Subagents: []mirror.Subagent{
				{Agent: "a1", ToolID: "toolu_a", State: mirror.SubagentRunning, Last: "Grep: TODO", Started: 5}}})
		}
		fm := &fakeMirror{online: true, v2: &fakeMirrorV2{batches: batches}}
		s, ts := viewServer(t, fm)
		c, _, err := dialTerm(t, s, ts, ui.MirrorViewWSPath("ws", "main"))
		if err != nil {
			t.Fatal(err)
		}
		first := readBatch(t, c)
		h := first["ops"].([]any)[0].(map[string]any)["html"].(string)
		if card := strings.Contains(h, "data-subagent-card"); card != reports || strings.Contains(h, "data-tool-row") == reports {
			t.Fatalf("reports=%v: %s", reports, h)
		}
		if reports {
			second := readBatch(t, c)
			ops := second["ops"].([]any)
			op := ops[0].(map[string]any)
			if op["op"] != "patch" || !strings.HasSuffix(op["id"].(string), "-s") || !strings.Contains(op["html"].(string), "Grep: TODO") {
				t.Fatalf("summary ops %v", ops)
			}
		}
		c.CloseNow()
	}
}
