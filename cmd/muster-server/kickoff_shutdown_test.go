package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agentkickoff"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/api"
	"github.com/ZacxDev/muster/internal/provision"
)

// shutdownStore serves ONE owed row to the deliverer and records the kickoff error
// it is handed. Its SetKickoffError takes recordDelay to land — a real UPDATE is
// not instantaneous — and refuses a done ctx as pgx does.
//
// ⚠ IT EMBEDS agents.Store, so a deliverer that grew a new store call panics here
// instead of reading a zero value.
type shutdownStore struct {
	agents.Store
	row         agents.Agent
	recordDelay time.Duration

	mu       sync.Mutex
	recorded string
}

func (s *shutdownStore) List(context.Context) ([]agents.Agent, error) {
	return []agents.Agent{s.row}, nil
}
func (s *shutdownStore) Get(context.Context, int64) (agents.Agent, error) { return s.row, nil }

// GetByName serves api.Server.agentOwner: an agent.changed nudge reaches its
// agent's OWNER only (accounts PR 3b), so the server reads the row by name.
func (s *shutdownStore) GetByName(context.Context, string) (agents.Agent, error) { return s.row, nil }
func (s *shutdownStore) ClaimKickoff(context.Context, int64, string, time.Duration) (bool, error) {
	return true, nil
}
func (s *shutdownStore) ReleaseKickoffClaim(context.Context, int64, string) error { return nil }
func (s *shutdownStore) LatestOrCreateSession(_ context.Context, id int64, name string) (agents.ChatSession, error) {
	return agents.ChatSession{ID: id * 10, AgentID: id, SessionKey: "sk-" + name}, nil
}
func (s *shutdownStore) SetKickedOff(context.Context, int64, bool) error { return nil }
func (s *shutdownStore) RecordKickoffDelivery(context.Context, int64, string, int32) error {
	return nil
}
func (s *shutdownStore) AddChatMessage(_ context.Context, m agents.ChatMessage) (agents.ChatMessage, error) {
	return m, nil
}
func (s *shutdownStore) SetKickoffError(ctx context.Context, _ int64, msg string) error {
	select {
	case <-time.After(s.recordDelay):
	case <-ctx.Done():
		return ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recorded = msg
	return nil
}
func (s *shutdownStore) recordedError() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recorded
}

// heldGateway blocks a turn until its ctx is done, as a real turn in flight does.
type heldGateway struct{ entered chan struct{} }

func (g *heldGateway) Resolve(context.Context, agents.Agent) (agentgateway.Target, error) {
	return agentgateway.Target{}, nil
}

// Checkout is never reached for the gateway agent these tests deliver to.
func (g *heldGateway) Checkout(context.Context, agentgateway.Target) (agents.CheckoutStatus, error) {
	return agents.CheckoutStatus{}, errors.New("heldGateway: Checkout is not expected for a gateway agent")
}

func (g *heldGateway) Send(ctx context.Context, _ agentgateway.Target, _, _ string, _ func(string)) (string, error) {
	close(g.entered)
	<-ctx.Done()
	return "", fmt.Errorf("post responses: %w", ctx.Err())
}

type readyLister struct{ inst provision.Instance }

func (l readyLister) Instances(context.Context) ([]provision.Instance, error) {
	return []provision.Instance{l.inst}, nil
}

// TestShutdownWaitsForACancelledKickoffToBeRecorded pins the round-0 shutdown fix.
//
// 🔴 THE DEFECT IT CLOSES: the deliverer's ctx is the signal context, so SIGTERM
// cancels a first turn AFTER its row was stamped kicked_off, and the deliverer then
// records that on a detached budget. app.shutdown used to return without waiting
// for it; main's deferred Close shut the pool and the process exited with the
// record in flight — the row read as delivered, with no error, on both tiers.
//
// SHAPE: a real agentkickoff.Deliverer, a turn held in flight, the signal context
// cancelled, then app.shutdown. When shutdown RETURNS the record must already be
// there. recordDelay (250ms) makes the record measurably slower than an unwaited
// shutdown, so a shutdown that does not wait returns before it lands.
func TestShutdownWaitsForACancelledKickoffToBeRecorded(t *testing.T) {
	const prefix = "shutdown-test-"
	row := agents.Agent{
		ID: 9113, Name: "late-wigeon", Namespace: agents.NamespaceFor(prefix, "late-wigeon"),
		Status: agents.StatusProvisioning, PendingNote: "tally the failing checks",
		HooksToken: "tok-wigeon-3a7e", UpdatedAt: time.Now(),
	}
	store := &shutdownStore{row: row, recordDelay: 250 * time.Millisecond}
	gw := &heldGateway{entered: make(chan struct{})}
	d, err := agentkickoff.New(agentkickoff.Config{
		Store: store, Gateway: gw, NamespacePrefix: prefix, Owner: "shutdown-test/1",
		Instances: readyLister{provision.Instance{
			Ref: provision.Ref{Name: "late-wigeon"}, InstanceID: "late-wigeon-pod", Phase: provision.PhaseRunning,
			Ready: true, Replicas: 1,
		}},
	})
	if err != nil {
		t.Fatalf("deliverer: %v", err)
	}
	logger := log.New(io.Discard, "", 0)
	a := &app{
		logger: logger, kickoff: d,
		http: &http.Server{}, srv: api.New(nil, api.AuthConfig{}, logger),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startBackgroundLoops(ctx)
	select {
	case <-gw.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("instrument check FAILED: the deliverer never put a turn in flight")
	}

	cancel() // SIGTERM
	a.shutdown()
	got := store.recordedError()
	if !strings.HasPrefix(got, agentkickoff.ShutdownCancelledReason) {
		t.Fatalf("app.shutdown returned before the cancelled first turn was recorded "+
			"(kickoff_error = %q). main closes the pool right after, so on a real "+
			"redeploy this record is LOST and the row reads as a delivered kickoff.", got)
	}
}

// slowRouter is a RouterPort whose PublishEvent takes publishDelay to land. It
// EMBEDS api.RouterPort, so any other router call panics instead of passing.
type slowRouter struct {
	api.RouterPort
	publishDelay time.Duration

	mu        sync.Mutex
	published []string
}

func (r *slowRouter) PublishEvent(ctx context.Context, name, _ string) error {
	select {
	case <-time.After(r.publishDelay):
	case <-ctx.Done():
		return ctx.Err()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.published = append(r.published, name)
	return nil
}

func (r *slowRouter) publishedEvents() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.published...)
}

// TestShutdownWaitsForTheCardChangeAShutdownCancelledKickoffBroadcasts pins the
// round-2 shutdown ORDER.
//
// 🔴 THE DEFECT IT CLOSES: a turn cancelled by shutdown is recorded and then
// broadcast (Config.OnChange -> BroadcastAgentChanged -> a router publish, which
// registers on the server's fan-out set). app.shutdown used to wait for that set
// BEFORE the deliverer, so a publish the deliverer registered afterwards was never
// waited for and was lost at exit.
//
// SHAPE: the round-0 test above, plus a router whose publish takes 300ms. When
// shutdown RETURNS, the failure's agent.changed publish must already have landed.
func TestShutdownWaitsForTheCardChangeAShutdownCancelledKickoffBroadcasts(t *testing.T) {
	const prefix = "shutdown-order-"
	row := agents.Agent{
		ID: 9227, Name: "brisk-teal", Namespace: agents.NamespaceFor(prefix, "brisk-teal"),
		Status: agents.StatusProvisioning, PendingNote: "sweep the stale branches",
		HooksToken: "tok-teal-81c4", UpdatedAt: time.Now(),
		// The operator's agent: an event about it is mirrored to the router.
		OwnerID: 1,
	}
	store := &shutdownStore{row: row, recordDelay: 20 * time.Millisecond}
	gw := &heldGateway{entered: make(chan struct{})}
	logger := log.New(io.Discard, "", 0)
	srv := api.New(nil, api.AuthConfig{}, logger)
	srv.UseExtensions(api.Extensions{Agents: store})
	router := &slowRouter{publishDelay: 300 * time.Millisecond}
	srv.UseRouter(router)
	d, err := agentkickoff.New(agentkickoff.Config{
		Store: store, Gateway: gw, NamespacePrefix: prefix, Owner: "shutdown-order/1",
		Instances: readyLister{provision.Instance{
			Ref: provision.Ref{Name: "brisk-teal"}, InstanceID: "brisk-teal-pod", Phase: provision.PhaseRunning,
			Ready: true, Replicas: 1,
		}},
		OnChange: srv.BroadcastAgentChanged,
	})
	if err != nil {
		t.Fatalf("deliverer: %v", err)
	}
	a := &app{logger: logger, kickoff: d, http: &http.Server{}, srv: srv}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startBackgroundLoops(ctx)
	select {
	case <-gw.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("instrument check FAILED: the deliverer never put a turn in flight")
	}
	// The stamp broadcast once while the turn was in flight; let that publish
	// land so only the post-cancel one is under test.
	deadline := time.Now().Add(5 * time.Second)
	for len(router.publishedEvents()) < 1 {
		if time.Now().After(deadline) {
			t.Fatal("instrument check FAILED: the stamp's agent.changed never reached the router")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel() // SIGTERM
	a.shutdown()
	if got := store.recordedError(); !strings.HasPrefix(got, agentkickoff.ShutdownCancelledReason) {
		t.Fatalf("instrument check FAILED: the cancelled turn was not recorded (kickoff_error = %q)", got)
	}
	if got := router.publishedEvents(); len(got) != 2 {
		t.Fatalf("app.shutdown returned before the card change of the shutdown-cancelled "+
			"kickoff reached the router: published %v, want 2 events (the stamp's and the "+
			"failure's). The fan-out set was waited on before the deliverer stopped adding to it.", got)
	}
}
