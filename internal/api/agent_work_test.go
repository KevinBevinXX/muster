package api

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// workRig is a server listing the relay's two agents, on a clock the test moves.
type workRig struct {
	t   *testing.T
	s   *Server
	h   http.Handler
	now time.Time
}

func newWorkRig(t *testing.T) *workRig {
	t.Helper()
	s := New(nil, AuthConfig{HookToken: relayHookToken, ServiceToken: relayServiceToken, UIPassword: relayPassword}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Agents: listingRelay{relayAgents{byToken: map[string]agents.Agent{relayTokenA: relayAgentA, relayTokenB: relayAgentB}}},
		SessionLiveness: stubLiveness{}})
	r := &workRig{t: t, s: s, now: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
	s.now = func() time.Time { return r.now }
	r.h = s.Handler()
	return r
}

func (r *workRig) report(token, body string) int {
	req := httptest.NewRequest(http.MethodPost, "/agent/work", strings.NewReader(body))
	req.Header.Set("X-Muster-Token", token)
	w := httptest.NewRecorder()
	r.h.ServeHTTP(w, req)
	return w.Code
}

func (r *workRig) get(path string) *httptest.ResponseRecorder {
	r.t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Cookie", sessionCookieFor(r.s))
	w := httptest.NewRecorder()
	r.h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		r.t.Fatalf("GET %s: %d %s", path, w.Code, w.Body)
	}
	return w
}

// cardBody is agent id's whole <article> on the list.
func cardBody(t *testing.T, list string, id int64) string {
	t.Helper()
	m := regexp.MustCompile(`(?s)<article id="agent-` + strconv.FormatInt(id, 10) + `".*?</article>`).FindString(list)
	if m == "" {
		t.Fatalf("instrument check FAILED: agent %d's card is not on the list", id)
	}
	return m
}

// chipShown is the work chip's visible text on a card, "" for none.
func chipShown(card string) string {
	m := regexp.MustCompile(`data-mirror-work[^>]*><span aria-hidden="true">([^<]*)</span>`).FindStringSubmatch(card)
	if m == nil {
		return ""
	}
	return m[1]
}

// TestAnAgentsWorkReportDrawsTheMirroredChipOnItsCard: ccd's POST /agent/work
// puts the SAME chip a mirrored window's card wears on the reporting agent's
// card — "3 subagents · 2 shells", "1+ shell" — and on no other agent's; a
// report with nothing running takes it off; counts are clamped as a window's
// are.
//
// Red before: no /agent/work route (404) and no chip on an agent card.
func TestAnAgentsWorkReportDrawsTheMirroredChipOnItsCard(t *testing.T) {
	r := newWorkRig(t)
	list := func() string { return r.get("/ui/agents").Body.String() }
	if got := chipShown(cardBody(t, list(), relayAgentA.ID)); got != "" {
		t.Fatalf("control: a card with no report shows a chip %q", got)
	}
	if code := r.report(relayTokenA, `{"subagents":3,"shells":2}`); code != http.StatusNoContent {
		t.Fatalf("POST /agent/work: %d, want 204", code)
	}
	l := list()
	if got := chipShown(cardBody(t, l, relayAgentA.ID)); got != "3 subagents · 2 shells" {
		t.Errorf("the reporting agent's chip reads %q, want %q", got, "3 subagents · 2 shells")
	}
	if got := chipShown(cardBody(t, l, relayAgentB.ID)); got != "" {
		t.Errorf("an agent that reported nothing shows %q", got)
	}
	r.report(relayTokenA, `{"shells":1,"shellsAtLeast":true}`)
	if got := chipShown(cardBody(t, list(), relayAgentA.ID)); got != "1+ shell" {
		t.Errorf("a lower-bound report reads %q, want %q", got, "1+ shell")
	}
	r.report(relayTokenA, `{"subagents":500}`)
	if got := r.s.AgentWork(relayAgentA.ID); got != (mirror.WindowWork{Subagents: mirror.MaxWorkCount}) {
		t.Errorf("an out-of-range count is kept as %+v, want it clamped to %d", got, mirror.MaxWorkCount)
	}
	r.report(relayTokenA, `{"subagents":0,"shells":0}`)
	if got := chipShown(cardBody(t, list(), relayAgentA.ID)); got != "" {
		t.Errorf("a report of nothing running still shows %q", got)
	}
	if code := r.report(relayTokenA, `not json`); code != http.StatusBadRequest {
		t.Errorf("a malformed report: %d, want 400", code)
	}
}

// TestAnAgentsWorkChipExpiresAndItsPollSaysSo: a report not refreshed within
// agentWorkTTL counts for nothing — a dead pod's chip disappears rather than
// freezing — and the card's status poll, which carries the chip it was drawn
// with, asks the list to re-render when that is no longer the server's answer:
// on a new report, and on an expiry that no POST announced.
func TestAnAgentsWorkChipExpiresAndItsPollSaysSo(t *testing.T) {
	r := newWorkRig(t)
	poll := func(drawn mirror.WindowWork) string {
		return r.get(ui.StatusPollPath(relayAgentA.ID, false, drawn)).Header().Get("HX-Trigger")
	}
	drawn := mirror.WindowWork{Subagents: 3, Shells: 2}
	if got := poll(mirror.WindowWork{}); got != "" {
		t.Fatalf("control: a card drawn without a chip, still without one: HX-Trigger %q", got)
	}
	r.report(relayTokenA, `{"subagents":3,"shells":2}`)
	if got := poll(mirror.WindowWork{}); got != ui.AgentsAwaitingEvent {
		t.Errorf("a card drawn without a chip, now reporting: HX-Trigger %q, want %q", got, ui.AgentsAwaitingEvent)
	}
	if got := poll(drawn); got != "" {
		t.Errorf("a card drawn with the current chip: HX-Trigger %q, want none", got)
	}
	// Refreshed inside the TTL, it still counts.
	r.now = r.now.Add(agentWorkTTL - time.Second)
	r.report(relayTokenA, `{"subagents":3,"shells":2}`)
	r.now = r.now.Add(agentWorkTTL - time.Second)
	if got := r.s.AgentWork(relayAgentA.ID); got != drawn {
		t.Fatalf("a report refreshed within the TTL was dropped: %+v", got)
	}
	// Not refreshed: gone, and the poll says so.
	r.now = r.now.Add(2 * time.Second)
	if got := r.s.AgentWork(relayAgentA.ID); !got.IsZero() {
		t.Errorf("a report not refreshed for longer than the TTL still counts: %+v", got)
	}
	if got := poll(drawn); got != ui.AgentsAwaitingEvent {
		t.Errorf("a card drawn with a chip that expired: HX-Trigger %q, want %q", got, ui.AgentsAwaitingEvent)
	}
	if got := chipShown(cardBody(t, r.get("/ui/agents").Body.String(), relayAgentA.ID)); got != "" {
		t.Errorf("the list still draws an expired chip %q", got)
	}
}
