package api

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// listingRelay is the relay fixture's agent store, listing its two agents.
type listingRelay struct{ relayAgents }

func (l listingRelay) List(context.Context) ([]agents.Agent, error) {
	return []agents.Agent{relayAgentA, relayAgentB}, nil
}

func (l listingRelay) LastMessageByAgentIDs(context.Context, []int64) (map[int64]time.Time, error) {
	return nil, nil
}

// TestAnAgentAwaitingApprovalIsWaitingOnAnAskOnTheList: an agent blocked at a
// permission prompt (POST /agent/blocked — the "awaiting approval" badge) is
// drawn with data-wait="ask", so the "Waiting on ask" chip finds it; and the
// card's status poll, carrying what the card was drawn with, answers
// HX-Trigger agents:awaiting when that is no longer true — on the way in AND on
// the way out — and nothing when it still is.
//
// Red before: the badge was only the poll's own markup, so the card carried no
// data-wait and the chip hid a blocked agent.
func TestAnAgentAwaitingApprovalIsWaitingOnAnAskOnTheList(t *testing.T) {
	s := New(nil, AuthConfig{HookToken: relayHookToken, ServiceToken: relayServiceToken, UIPassword: relayPassword}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Agents: listingRelay{relayAgents{byToken: map[string]agents.Agent{relayTokenA: relayAgentA, relayTokenB: relayAgentB}}},
		SessionLiveness: stubLiveness{}})
	h := s.Handler()
	get := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Cookie", sessionCookieFor(s))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, w.Code, w.Body)
		}
		return w
	}
	block := func(on bool) {
		r := httptest.NewRequest(http.MethodPost, "/agent/blocked", strings.NewReader(`{"blocked":`+strconv.FormatBool(on)+`}`))
		r.Header.Set("X-Muster-Token", relayTokenA)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code >= 300 {
			t.Fatalf("POST /agent/blocked: %d %s", w.Code, w.Body)
		}
	}
	cardTag := func(id int64) string {
		return listCardAttrs(get("/ui/agents").Body.String())["agent-"+strconv.FormatInt(id, 10)]
	}
	if tag := cardTag(relayAgentA.ID); tag == "" || strings.Contains(tag, "data-wait") {
		t.Fatalf("control: an agent not blocked is waiting on something (or not listed): %q", tag)
	}
	block(true)
	if tag := cardTag(relayAgentA.ID); !strings.Contains(tag, `data-wait="ask"`) {
		t.Errorf("a blocked agent's card does not carry data-wait=\"ask\": %s", tag)
	}
	if tag := cardTag(relayAgentB.ID); strings.Contains(tag, "data-wait") {
		t.Errorf("the other agent is waiting too: %s", tag)
	}
	poll := func(drawn bool) string {
		return get(ui.StatusPollPath(relayAgentA.ID, drawn, mirror.WindowWork{})).Header().Get("HX-Trigger")
	}
	if got := poll(false); got != ui.AgentsAwaitingEvent {
		t.Errorf("a card drawn free, now blocked: HX-Trigger %q, want %q", got, ui.AgentsAwaitingEvent)
	}
	if got := poll(true); got != "" {
		t.Errorf("a card drawn blocked, still blocked: HX-Trigger %q, want none", got)
	}
	block(false)
	if got := poll(true); got != ui.AgentsAwaitingEvent {
		t.Errorf("a card drawn blocked, now free: HX-Trigger %q, want %q", got, ui.AgentsAwaitingEvent)
	}
}
