package agentkickoff

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

// fakeStore is an in-memory agents.Store that records every call in ONE shared
// transcript — the gateway below writes into the same one, so ORDER between a store
// write and the model turn is observable.
//
// ⚠ IT EMBEDS agents.Store (nil) SO A METHOD THE DELIVERER WAS NOT EXPECTED TO CALL
// PANICS rather than returning a quiet zero value.
type fakeStore struct {
	agents.Store
	mu   sync.Mutex
	rows map[int64]*agents.Agent
	// getOverride, when set for an id, is what Get returns — the "another replica
	// changed the row between the list and the claim" shape.
	getOverride map[int64]agents.Agent
	claimLoses  bool
	claimErr    error
	sessionErr  error
	// provenanceErr makes RecordKickoffDelivery fail WITHOUT writing — and so
	// without clearing kickoff_error, which is what that write normally does.
	provenanceErr error
	// listSkew makes List report updated_at this much EARLIER than the stored row:
	// the row moved on after the list read, which MarkKickoffStuck must notice.
	listSkew time.Duration
	log      *recorder
	nextMsg  int64
}

// recorder is the ONE transcript the store and the gateway both write, under one
// lock, so cross-object ORDER is observable and -race stays quiet.
type recorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *recorder) add(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, line)
}

func (r *recorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}

func newFakeStore(log *recorder, rows ...agents.Agent) *fakeStore {
	s := &fakeStore{rows: map[int64]*agents.Agent{}, getOverride: map[int64]agents.Agent{}, log: log}
	for i := range rows {
		r := rows[i]
		s.rows[r.ID] = &r
	}
	return s
}

func (s *fakeStore) rec(format string, args ...any) {
	s.log.add(fmt.Sprintf(format, args...))
}

func (s *fakeStore) List(context.Context) ([]agents.Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []agents.Agent
	for _, r := range s.rows {
		c := *r
		c.UpdatedAt = c.UpdatedAt.Add(-s.listSkew)
		out = append(out, c)
	}
	return out, nil
}

func (s *fakeStore) Get(_ context.Context, id int64) (agents.Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec("Get(%d)", id)
	if o, ok := s.getOverride[id]; ok {
		return o, nil
	}
	r, ok := s.rows[id]
	if !ok {
		return agents.Agent{}, errors.New("no such row")
	}
	return *r, nil
}

func (s *fakeStore) ClaimKickoff(_ context.Context, id int64, owner string, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec("ClaimKickoff(%d,%s)", id, owner)
	if s.claimErr != nil {
		return false, s.claimErr
	}
	return !s.claimLoses, nil
}

func (s *fakeStore) ReleaseKickoffClaim(_ context.Context, id int64, owner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec("ReleaseKickoffClaim(%d,%s)", id, owner)
	return nil
}

func (s *fakeStore) SetKickedOff(_ context.Context, id int64, v bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec("SetKickedOff(%d,%t)", id, v)
	s.rows[id].KickedOff = v
	return nil
}

func (s *fakeStore) RecordKickoffDelivery(_ context.Context, id int64, pod string, restarts int32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec("RecordKickoffDelivery(%d,%s,%d)", id, pod, restarts)
	if s.provenanceErr != nil {
		return s.provenanceErr
	}
	r := s.rows[id]
	r.KickoffPod, r.KickoffRestarts, r.KickoffError = pod, restarts, ""
	r.KickoffAttempts++
	return nil
}

// SetKickoffError REFUSES a done ctx without writing, exactly as pgx does: a query
// on a cancelled context never reaches the server. Without that, a deliverer that
// recorded a shutdown-cancelled turn on the turn's own (cancelled) ctx would pass
// here and lose the record in production.
func (s *fakeStore) SetKickoffError(ctx context.Context, id int64, msg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		s.rec("SetKickoffError-REFUSED(%d,%v)", id, err)
		return err
	}
	s.rec("SetKickoffError(%d,%s)", id, msg)
	s.rows[id].KickoffError = msg
	return nil
}

func (s *fakeStore) UpdateStatus(_ context.Context, id int64, status, lastOutput, errMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec("UpdateStatus(%d,%s,%s)", id, status, errMsg)
	s.rows[id].Status, s.rows[id].ErrorMessage = status, errMsg
	return nil
}

// MarkKickoffStuck mirrors the PG predicate's updated_at equality; the claim
// conjunct is pinned against real Postgres (TestAStuckVerdictDoesNotOverwriteARowThatMoved).
func (s *fakeStore) MarkKickoffStuck(_ context.Context, id int64, seen time.Time, errMsg string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.rows[id]
	if !r.UpdatedAt.Equal(seen) {
		s.rec("MarkKickoffStuck-REFUSED(%d)", id)
		return false, nil
	}
	s.rec("MarkKickoffStuck(%d,%s)", id, errMsg)
	r.Status, r.ErrorMessage = agents.StatusError, errMsg
	return true, nil
}

func (s *fakeStore) LatestOrCreateSession(_ context.Context, agentID int64, name string) (agents.ChatSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec("LatestOrCreateSession(%d)", agentID)
	if s.sessionErr != nil {
		return agents.ChatSession{}, s.sessionErr
	}
	return agents.ChatSession{ID: agentID * 10, AgentID: agentID, SessionKey: "sk-" + name}, nil
}

func (s *fakeStore) AddChatMessage(_ context.Context, m agents.ChatMessage) (agents.ChatMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextMsg++
	m.ID = s.nextMsg
	s.rec("AddChatMessage(%d,%d,%s,%s)", m.AgentID, m.SessionID, m.Role, m.Content)
	return m, nil
}

func (s *fakeStore) row(id int64) agents.Agent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return *s.rows[id]
}

// fakeGateway records each turn into the shared transcript.
type fakeGateway struct {
	mu    sync.Mutex
	log   *recorder
	reply string
	err   error
	calls int
	// resolveErr makes Resolve fail: the k8s API / driver failure that costs nothing.
	resolveErr error
	// resolved is the name of the agent last resolved; Send logs it, since a
	// Target from outside agentgateway carries nothing a fake can read.
	resolved string
	// hold, when non-nil, makes Send signal it and then block until the turn's ctx
	// is done, returning ctx.Err() — a turn in flight when its ctx is cancelled
	// (shutdown) or expires (turn budget), which is what the real gateway's HTTP
	// client returns.
	hold chan struct{}
	// checkout / checkoutErr are what Checkout answers (ccd's /healthz repo).
	checkout    agents.CheckoutStatus
	checkoutErr error
}

func (g *fakeGateway) Checkout(context.Context, agentgateway.Target) (agents.CheckoutStatus, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.log.add(fmt.Sprintf("Checkout(%s)", g.resolved))
	return g.checkout, g.checkoutErr
}

func (g *fakeGateway) setCheckout(st agents.CheckoutStatus, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.checkout, g.checkoutErr = st, err
}

func (g *fakeGateway) Resolve(_ context.Context, a agents.Agent) (agentgateway.Target, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.log.add(fmt.Sprintf("Resolve(%s)", a.Name))
	if g.resolveErr != nil {
		return agentgateway.Target{}, g.resolveErr
	}
	g.resolved = a.Name
	return agentgateway.Target{}, nil
}

func (g *fakeGateway) Send(ctx context.Context, _ agentgateway.Target, sessionKey, message string, _ func(string)) (string, error) {
	g.mu.Lock()
	g.calls++
	g.log.add(fmt.Sprintf("Chat(%s,%s,%s)", g.resolved, sessionKey, message))
	hold, reply, err := g.hold, g.reply, g.err
	g.mu.Unlock()
	if hold != nil {
		close(hold)
		<-ctx.Done()
		return "", fmt.Errorf("post responses: %w", ctx.Err())
	}
	return reply, err
}

func (g *fakeGateway) callCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

// fakeInstances returns a fixed instance set, or an error.
type fakeInstances struct {
	insts []provision.Instance
	err   error
	calls int
}

func (f *fakeInstances) Instances(context.Context) ([]provision.Instance, error) {
	f.calls++
	return f.insts, f.err
}

// transcript renders the shared log for failure messages.
func transcript(log []string) string { return "\n    " + strings.Join(log, "\n    ") }

// indexOf returns the first entry with the given prefix, or -1.
func indexOf(log []string, prefix string) int {
	for i, l := range log {
		if strings.HasPrefix(l, prefix) {
			return i
		}
	}
	return -1
}

func countPrefix(log []string, prefix string) int {
	n := 0
	for _, l := range log {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}
