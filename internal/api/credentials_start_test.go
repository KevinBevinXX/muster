package api

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/credentials"
)

// oneAgentStore answers Get for one gateway agent (and List with it, for the
// panel re-render after a start).
type oneAgentStore struct {
	agents.Store
	a agents.Agent
}

func (s oneAgentStore) Get(context.Context, int64) (agents.Agent, error) { return s.a, nil }
func (s oneAgentStore) List(context.Context) ([]agents.Agent, error)     { return []agents.Agent{s.a}, nil }

// TestStartingAnAgentOfAKindTheCallerHasNoCredentialForIs409 (round-1 audit on
// #69): a member who deleted their OpenRouter key and presses Start on their
// own gateway agent is refused at the door with the same 409 and sentence as a
// dispatch, and the provisioner is never asked; the control — the same Start
// with the key present — reaches the provisioner.
func TestStartingAnAgentOfAKindTheCallerHasNoCredentialForIs409(t *testing.T) {
	for _, c := range []struct {
		unavailable map[string]string
		refused     bool
	}{
		{map[string]string{agents.KindGateway: credentials.NeedOpenRouter}, true},
		{nil, false},
	} {
		prov := newPreflightProvisioner("")
		s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
		s.UseExtensions(Extensions{
			Agents:          oneAgentStore{a: agents.Agent{ID: 77, Name: "keen-otter", Kind: agents.KindGateway}},
			SessionLiveness: stubLiveness{},
			Provisioner:     prov,
			Kinds:           stubKinds{unavailable: c.unavailable},
		})
		req := httptest.NewRequest("POST", "/agents/77/start", nil)
		req.Host = "muster.test"
		req.Header.Set("Origin", "http://muster.test")
		admit(s, req)
		rec := httptest.NewRecorder()
		func() {
			defer func() { _ = recover() }()
			s.Handler().ServeHTTP(rec, req)
		}()
		if c.refused {
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), credentials.NeedOpenRouter) {
				t.Fatalf("start with no key: %d %s; want 409 naming what to add", rec.Code, rec.Body)
			}
			time.Sleep(100 * time.Millisecond)
			prov.mu.Lock()
			n := len(prov.starts)
			prov.mu.Unlock()
			if n != 0 {
				t.Fatalf("a refused start still reached the provisioner %d time(s)", n)
			}
			continue
		}
		select {
		case <-prov.startC:
		case <-time.After(3 * time.Second):
			t.Fatalf("control: the start with a key never reached the provisioner (%d %s)", rec.Code, rec.Body)
		}
	}
}
