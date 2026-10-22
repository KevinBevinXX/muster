package api

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// stoppedListing lists agent A STOPPED (and B running).
type stoppedListing struct{ relayAgents }

func (l stoppedListing) List(context.Context) ([]agents.Agent, error) {
	a := relayAgentA
	a.Status = agents.StatusStopped
	return []agents.Agent{a, relayAgentB}, nil
}

func (l stoppedListing) LastMessageByAgentIDs(context.Context, []int64) (map[int64]time.Time, error) {
	return nil, nil
}

// TestAStoppedAgentsCardShowsNoWorkChip: a stopped agent's pod is gone and
// cannot post its own clear, so whatever it last reported is NOT drawn on its
// card — and its status poll agrees, so it asks for no re-render.
//
// Red before: the card drew the stopped agent's last report until the TTL.
func TestAStoppedAgentsCardShowsNoWorkChip(t *testing.T) {
	stoppedA := relayAgentA
	stoppedA.Status = agents.StatusStopped
	byToken := map[string]agents.Agent{relayTokenA: stoppedA, relayTokenB: relayAgentB}
	s := New(nil, AuthConfig{HookToken: relayHookToken, ServiceToken: relayServiceToken, UIPassword: relayPassword}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Agents: stoppedListing{relayAgents{byToken: byToken}}, SessionLiveness: stubLiveness{}})
	r := &workRig{t: t, s: s, h: s.Handler()}
	for tok, body := range map[string]string{relayTokenA: `{"subagents":3,"shells":2}`, relayTokenB: `{"subagents":1}`} {
		if code := r.report(tok, body); code != http.StatusNoContent {
			t.Fatalf("report: %d", code)
		}
	}
	list := r.get("/ui/agents").Body.String()
	if got := chipShown(cardBody(t, list, relayAgentB.ID)); got != "1 subagent" {
		t.Fatalf("control: the running agent's chip reads %q, want %q", got, "1 subagent")
	}
	if got := chipShown(cardBody(t, list, relayAgentA.ID)); got != "" {
		t.Errorf("a stopped agent's card shows %q", got)
	}
	if got := r.get(ui.StatusPollPath(relayAgentA.ID, false, mirror.WindowWork{})).Header().Get("HX-Trigger"); got != "" {
		t.Errorf("a stopped agent's card drawn without a chip is told to re-render (%q)", got)
	}
}

// TestStoppingOrDestroyingAnAgentForgetsItsWorkReport: the operator's stop and
// delete forget the agent's report at once — a restarted agent does not come
// back wearing its old chip before its pod reports again.
func TestStoppingOrDestroyingAnAgentForgetsItsWorkReport(t *testing.T) {
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/agents/" + strconv.FormatInt(relayAgentA.ID, 10) + "/stop"},
		{http.MethodDelete, "/agents/" + strconv.FormatInt(relayAgentA.ID, 10)},
	} {
		t.Run(c.method, func(t *testing.T) {
			s := New(nil, AuthConfig{HookToken: relayHookToken, ServiceToken: relayServiceToken, UIPassword: relayPassword}, log.New(io.Discard, "", 0))
			s.UseExtensions(Extensions{Agents: listingRelay{relayAgents{byToken: map[string]agents.Agent{relayTokenA: relayAgentA, relayTokenB: relayAgentB}}},
				SessionLiveness: stubLiveness{}, Provisioner: stubProvisioner{}})
			r := &workRig{t: t, s: s, h: s.Handler()}
			r.report(relayTokenA, `{"subagents":3,"shells":2}`)
			r.report(relayTokenB, `{"subagents":1}`)
			if got := s.AgentWork(relayAgentA.ID); got.IsZero() {
				t.Fatal("control: the report did not register")
			}
			req := httptest.NewRequest(c.method, c.path, nil)
			req.Host = "muster.test"
			req.Header.Set("Origin", "http://muster.test")
			req.Header.Set("Cookie", sessionCookieFor(s))
			rec := httptest.NewRecorder()
			func() {
				defer func() { _ = recover() }() // the re-render after it is not under test
				r.h.ServeHTTP(rec, req)
			}()
			if got := s.AgentWork(relayAgentA.ID); !got.IsZero() {
				t.Errorf("%s %s (status %d) kept the agent's report %+v", c.method, c.path, rec.Code, got)
			}
			if got := s.AgentWork(relayAgentB.ID); got != (mirror.WindowWork{Subagents: 1}) {
				t.Errorf("another agent's report went too: %+v", got)
			}
		})
	}
}
