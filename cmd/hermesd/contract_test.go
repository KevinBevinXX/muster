package main

import (
	"context"
	"errors"
	"net"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

// THE CONTRACT TEST: hermesd's /v1/responses driven by muster's OWN client —
// agentgateway.Gateway.Chat, the path muster's chat box and agentkickoff use,
// with the real HooksSHA256 runtime deriving the bearer from the agent row's
// token, as for every kind. Nothing here re-implements muster's side.

type staticEndpoint provision.Endpoint

func (s staticEndpoint) Endpoint(context.Context, provision.Ref) (provision.Endpoint, error) {
	return provision.Endpoint(s), nil
}

const contractHooksToken = "hooks-token-for-the-contract-test-7c1d"

func mustersGateway(t *testing.T, s *server) *agentgateway.Gateway {
	t.Helper()
	derived := agentgateway.HooksSHA256().Bearer(contractHooksToken)
	s.bearer = derived
	hs := httptest.NewServer(s.handler())
	t.Cleanup(hs.Close)
	host, port, err := net.SplitHostPort(strings.TrimPrefix(hs.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	p, _ := strconv.Atoi(port)
	gw, err := agentgateway.New(agentgateway.Config{
		Driver: staticEndpoint{Host: host, Port: p}, Runtime: agentgateway.HooksSHA256(), Model: "hermesd-ignores-this",
	})
	if err != nil {
		t.Fatal(err)
	}
	return gw
}

var contractAgent = agents.Agent{ID: 9, Name: "hermes-agent", Kind: "hermes", HooksToken: contractHooksToken}

func TestMustersClientReceivesTheReplyAndTheSessionMapsToOneHermesSession(t *testing.T) {
	fake := newFakeHermes(t, testAPIKey)
	s := newTestServer(t, fake, testAPIKey)
	gw := mustersGateway(t, s)
	var deltas []string
	reply, err := gw.Chat(context.Background(), contractAgent, "session-1", "list the workspace", func(d string) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatal(err)
	}
	if reply != "echo: list the workspace" || strings.Join(deltas, "") != reply {
		t.Fatalf("muster assembled %q (deltas %q)", reply, deltas)
	}
	// muster's session key is the map's key: a second turn on it goes to the
	// same Hermes session, another key to another.
	if _, err := gw.Chat(context.Background(), contractAgent, "session-1", "again", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := gw.Chat(context.Background(), contractAgent, "session-2", "elsewhere", nil); err != nil {
		t.Fatal(err)
	}
	a, _ := s.store.get("session-1")
	b, _ := s.store.get("session-2")
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.runs["run_1"].session != a.HermesID || fake.runs["run_2"].session != a.HermesID || fake.runs["run_3"].session != b.HermesID || a.HermesID == b.HermesID {
		t.Fatalf("sessions: %s %s; runs on %s %s %s", a.HermesID, b.HermesID,
			fake.runs["run_1"].session, fake.runs["run_2"].session, fake.runs["run_3"].session)
	}
}

// A failed turn is an ERROR in muster's client, typed — never ("", nil).
func TestAFailedRunIsATypedErrorInMustersClient(t *testing.T) {
	for _, tc := range []struct{ input, typ string }{
		{"FAIL the provider broke", failRuntime},
		{"EMPTY", failEmptyReply},
	} {
		fake := newFakeHermes(t, testAPIKey)
		s := newTestServer(t, fake, testAPIKey)
		gw := mustersGateway(t, s)
		reply, err := gw.Chat(context.Background(), contractAgent, "s", tc.input, nil)
		var rt *agents.RuntimeError
		if err == nil || !errors.As(err, &rt) || rt.Type != tc.typ || reply != "" {
			t.Fatalf("%q: muster got (%q, %v); want a %s RuntimeError", tc.input, reply, err, tc.typ)
		}
	}
}
