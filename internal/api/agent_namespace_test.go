package api

import (
	"context"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
)

// ---------------------------------------------------------------------------
// THE BEHAVIOURAL HALF OF THE NAMESPACE PAIR.
//
// 🔴 cmd/muster-server's TestTheStoredNamespacePrefixIsWhatTheDriverIsConfiguredWith
// COMPARES A CONFIGURED PREFIX WITH WHERE THE DRIVER PUTS AN INSTANCE; THIS
// WATCHES A ROW GET WRITTEN. A structural check type-checks past a handler that
// computes the right namespace and then stores something else —
// createAndDispatchAgent builds an agents.Agent with a dozen fields and Namespace
// is one assignment among them. The defect that put this file here (a row saying
// `devpod-lively-newt` while the driver had created `muster-agent-lively-newt`)
// was visible only in the STORED value.
// ---------------------------------------------------------------------------

// namespaceCapturingStore records the Agent handed to Create.
//
// ⚠ IT EMBEDS agents.Store, so any other method a test reaches is a nil-interface
// panic rather than a zero value flowing into an assertion.
type namespaceCapturingStore struct {
	agents.Store
	created agents.Agent
}

func (s *namespaceCapturingStore) Create(_ context.Context, a agents.Agent) (agents.Agent, error) {
	a.ID = 77
	s.created = a
	return a, nil
}

// dispatchSignalProvisioner reports the dispatch the handler fires from its
// background goroutine, so the test can end with nothing in flight.
type dispatchSignalProvisioner struct {
	stubProvisioner
	dispatched chan int64
}

func (p dispatchSignalProvisioner) Dispatch(id int64, _ bool) error {
	p.dispatched <- id
	return nil
}

// TestTheCreatedAgentRowCarriesTheConfiguredNamespace drives the row-writing path
// under a NON-DEFAULT prefix.
//
// ⚠ EVERY EXPECTATION IS A LITERAL. Asserting against s.ext.AgentNamespace(name)
// would be the implementation under test answering its own question — it would
// pass over a handler that stored agents.NamespacePrefix+name and an
// AgentNamespace that returned the same, which is precisely the shipped bug.
func TestTheCreatedAgentRowCarriesTheConfiguredNamespace(t *testing.T) {
	cases := []struct {
		label  string
		prefix string
		name   string
		want   string
	}{
		// The deployed configuration: MUSTER_AGENT_NAMESPACE_PREFIX=muster-agent-.
		{label: "deployed", prefix: "muster-agent-", name: "lively-newt", want: "muster-agent-lively-newt"},
		// An arbitrary prefix, so nothing here is pinned to the one value this
		// installation happens to use.
		{label: "arbitrary", prefix: "qx7-pen-", name: "harbour-kestrel", want: "qx7-pen-harbour-kestrel"},
		// And the default still holds when the wiring carries no prefix.
		{label: "default", prefix: "", name: "brave-heron", want: "devpod-brave-heron"},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			store := &namespaceCapturingStore{}
			prov := dispatchSignalProvisioner{dispatched: make(chan int64, 1)}
			srv := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
			srv.UseExtensions(Extensions{
				Agents:               store,
				Provisioner:          prov,
				AgentNamespacePrefix: tc.prefix,
			})

			rec, err := srv.createAndDispatchAgent(context.Background(), dispatchParams{
				Name: tc.name,
			})
			if err != nil {
				t.Fatalf("createAndDispatchAgent: %v", err)
			}

			// What the STORE was asked to persist — the agents.namespace column.
			if store.created.Namespace != tc.want {
				t.Errorf("the row written for %q says namespace %q, want %q.\n"+
					"    Nothing fails when this disagrees with where the driver puts the "+
					"instance: the row names a namespace that holds nothing, which reads as "+
					"\"never provisioned\" while the instance runs one namespace over.", tc.name,
					store.created.Namespace, tc.want)
			}
			// And what the handler HANDED BACK, which is what the privilege applier
			// and the redirect that follows a dispatch both read.
			if rec.Namespace != tc.want {
				t.Errorf("the returned record says namespace %q, want %q", rec.Namespace, tc.want)
			}

			select {
			case got := <-prov.dispatched:
				if got != 77 {
					t.Errorf("dispatched agent id %d, want the created row's 77", got)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the dispatch goroutine never reached Provisioner.Dispatch: this test's " +
					"fixture is not exercising the create-and-dispatch path it claims to")
			}
		})
	}
}

// TestAForgottenAgentNamespacePrefixIsAWiringDefect is what stops the fix being
// undone by omission.
//
// 🔴 A DEFAULT THAT MAKES FIXTURES CONVENIENT IS ALSO WHAT LETS A PRODUCTION
// WIRING DROP THE FIELD AND LOOK FINE. Extensions.AgentNamespace resolves an empty
// prefix to agents.NamespacePrefix — which is the whole reason the ~20
// struct-literal config fixtures in cmd/muster-server need no update — so an empty
// prefix beside a wired Agents store and Provisioner is indistinguishable, from
// the code, from the defect that shipped. defects() is where that becomes loud:
// /readyz refuses and prints the reason, which is the loudest signal that is not
// also an outage.
//
// ⚠ IT ASSERTS THE THREE NEGATIVE CASES TOO, because a defect that fires whenever
// the prefix is empty would pull every provisioner-less deployment out of
// rotation — and the default configuration is provisioner-less.
func TestAForgottenAgentNamespacePrefixIsAWiringDefect(t *testing.T) {
	const want = "AgentNamespacePrefix is empty"

	// The defect: agents creatable, driver present, prefix dropped.
	got := Extensions{
		Agents:      stubAgentsStore{},
		Provisioner: stubProvisioner{},
	}.defects()
	if !containsSubstring(got, want) {
		t.Errorf("Agents+Provisioner wired with an empty AgentNamespacePrefix reported defects "+
			"%q, want one mentioning %q. Without it, dropping the prefix from newApp's "+
			"api.Extensions literal re-opens the shipped defect with nothing failing.", got, want)
	}

	// Wired: not a defect.
	got = Extensions{
		Agents:               stubAgentsStore{},
		Provisioner:          stubProvisioner{},
		AgentNamespacePrefix: "muster-agent-",
	}.defects()
	if containsSubstring(got, want) {
		t.Errorf("a wired prefix still reported the namespace defect: %q", got)
	}

	// No provisioner: nothing to disagree with, and no route that can create an
	// agent. Refusing here would take out the DEFAULT configuration.
	got = Extensions{Agents: stubAgentsStore{}}.defects()
	if containsSubstring(got, want) {
		t.Errorf("a provisioner-less deployment reported the namespace defect and would be "+
			"pulled from its Service for a value nothing reads: %q", got)
	}

	// No agents store: nothing writes a row at all.
	got = Extensions{Provisioner: stubProvisioner{}}.defects()
	if containsSubstring(got, want) {
		t.Errorf("a store-less deployment reported the namespace defect: %q", got)
	}
}

func containsSubstring(haystack []string, needle string) bool {
	for _, s := range haystack {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}
