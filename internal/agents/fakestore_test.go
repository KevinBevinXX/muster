package agents

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

// fakeStore is an in-memory agents.Store for tests.
//
// 🔴 THE CLAIM METHODS IMPLEMENT REAL SEMANTICS, NOT STUBS, and that is the one
// thing to preserve if this file is ever trimmed. ClaimKickoff /
// RenewKickoffClaim / ReleaseKickoffClaim model owner plus expiry with
// owner-scoped renew and release. A stub that always won would make every
// kickoff test blind to the claim — the opposite of what those tests are for;
// a stub that always lost would silently disable the funnel. One fakeStore
// stands in for one database, so two callers sharing a fakeStore contend
// exactly as two processes sharing Postgres do.

// statusWrite records one UpdateStatus call so tests can assert exactly what
// changed and how many writes occurred.
type statusWrite struct {
	id     int64
	status string
}

// fakeStore is an in-test agents.Store. Only the methods the reconciler touches
// (List, Get, UpdateStatus, SetKickedOff) carry behaviour; the rest satisfy the
// interface so a *Provisioner can be constructed against it.
type fakeStore struct {
	mu      sync.Mutex
	agents  map[int64]Agent
	order   []int64       // stable List ordering
	writes  []statusWrite // recorded UpdateStatus calls
	listErr error

	// nameExistsFn, when set, backs NameExists so tests can drive the
	// unique-name generation loop (collisions, store errors). Nil ⇒ no name is
	// ever taken (the reconciler tests don't touch NameExists).
	nameExistsFn func(name string) (bool, error)

	// chatMsgs records AddChatMessage calls in order (for transcript persistence
	// assertions).
	chatMsgs []ChatMessage

	// deleted records Delete calls in order, and onDelete (when set) runs inside
	// Delete. The destroy tests use onDelete to model the agent_privileges FK
	// cascade — deleting the agent row makes its grants unreadable — which is what
	// makes Destroy's grants-before-delete ordering observable.
	deleted  []int64
	onDelete func(id int64)

	// claims backs the cross-process kickoff claim (ClaimKickoff and friends) with
	// real owner+TTL semantics; claimErr, when set, makes ClaimKickoff fail so the
	// fail-closed arm is reachable.
	claims        map[int64]fakeClaim
	claimErr      error
	releaseErr    error
	renewals      int // successful re-fences
	renewAttempts int // renewals attempted, successful or not
}

// fakeClaim mirrors the (kickoff_claim_owner, kickoff_claim_expires_at) pair.
type fakeClaim struct {
	owner   string
	expires time.Time
}

func newFakeStore(as ...Agent) *fakeStore {
	f := &fakeStore{agents: make(map[int64]Agent)}
	for _, a := range as {
		f.agents[a.ID] = a
		f.order = append(f.order, a.ID)
	}
	return f
}

func (f *fakeStore) List(ctx context.Context) ([]Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]Agent, 0, len(f.order))
	for _, id := range f.order {
		out = append(out, f.agents[id])
	}
	return out, nil
}

func (f *fakeStore) Get(ctx context.Context, id int64) (Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.agents[id]
	if !ok {
		return Agent{}, context.Canceled // any non-nil error; reconciler logs+skips
	}
	return a, nil
}

func (f *fakeStore) UpdateStatus(ctx context.Context, id int64, status, lastOutput, errMsg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, statusWrite{id: id, status: status})
	if a, ok := f.agents[id]; ok {
		a.Status = status
		a.ErrorMessage = errMsg
		a.UpdatedAt = time.Now()
		f.agents[id] = a
	}
	return nil
}

// MarkKickoffStuck is not driven by this package's tests; it exists so the fake
// still satisfies Store. Its semantics are pinned against real Postgres in
// internal/agentkickoff.
func (f *fakeStore) MarkKickoffStuck(context.Context, int64, time.Time, string) (bool, error) {
	return false, errors.New("fakeStore: MarkKickoffStuck is not modelled")
}

// MarkRunning: same as MarkKickoffStuck — pinned against real Postgres in
// internal/agentstatus.
func (f *fakeStore) MarkRunning(context.Context, int64, time.Time) (bool, error) {
	return false, errors.New("fakeStore: MarkRunning is not modelled")
}

func (f *fakeStore) SetKickedOff(ctx context.Context, id int64, v bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a, ok := f.agents[id]; ok {
		a.KickedOff = v
		f.agents[id] = a
	}
	return nil
}

// RecordKickoffDelivery mirrors the PG update: stamp the recipient, burn one unit
// of the re-send budget, clear the previous send error.
func (f *fakeStore) RecordKickoffDelivery(ctx context.Context, id int64, pod string, restarts int32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a, ok := f.agents[id]; ok {
		a.KickoffPod = pod
		a.KickoffRestarts = restarts
		a.KickoffAttempts++
		a.KickoffError = ""
		f.agents[id] = a
	}
	return nil
}

func (f *fakeStore) ClearKickoffDelivery(ctx context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a, ok := f.agents[id]; ok {
		a.KickoffPod = ""
		a.KickoffRestarts = 0
		f.agents[id] = a
	}
	return nil
}

// ClaimKickoff / RenewKickoffClaim / ReleaseKickoffClaim implement the REAL
// claim semantics in memory (owner + expiry, owner-scoped renew/release) rather
// than stubbing them true. A stub that always won would make every kickoff test
// blind to the claim, which is the opposite of what these tests are for; a stub
// that always lost would silently disable the funnel. One fakeStore stands in for
// one database, so two Provisioners sharing a fakeStore contend exactly as two
// pods sharing Postgres do.
func (f *fakeStore) ClaimKickoff(_ context.Context, id int64, owner string, ttl time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claimErr != nil {
		return false, f.claimErr
	}
	if f.claims == nil {
		f.claims = map[int64]fakeClaim{}
	}
	if c, ok := f.claims[id]; ok && c.expires.After(time.Now()) {
		return false, nil
	}
	f.claims[id] = fakeClaim{owner: owner, expires: time.Now().Add(ttl)}
	return true, nil
}

func (f *fakeStore) RenewKickoffClaim(_ context.Context, id int64, owner string, ttl time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// 🔴 ATTEMPTS are counted before the predicate, successes after. The two are
	// not interchangeable: a LEAKED renewal ticker (one whose kickoff finished and
	// released) renews a row it no longer owns, so it never increments the success
	// counter — which is exactly why the first version of the leaked-ticker test
	// could not see the leak, and the mutation that removes stopRenew() survived.
	f.renewAttempts++
	c, ok := f.claims[id]
	if !ok || c.owner != owner {
		return false, nil
	}
	c.expires = time.Now().Add(ttl)
	f.claims[id] = c
	f.renewals++
	return true, nil
}

// renewCount reports how many times a claim was successfully re-fenced.
func (f *fakeStore) renewCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.renewals
}

// renewAttemptCount reports how many renewals were ATTEMPTED, successful or not.
// This is the counter that can see a leaked ticker (see RenewKickoffClaim).
func (f *fakeStore) renewAttemptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.renewAttempts
}

// claimExpiry reports the current fence for an agent's claim (zero if unclaimed).
func (f *fakeStore) claimExpiry(id int64) time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.claims[id].expires
}

func (f *fakeStore) ReleaseKickoffClaim(_ context.Context, id int64, owner string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.releaseErr != nil {
		return f.releaseErr
	}
	if c, ok := f.claims[id]; ok && c.owner == owner {
		// Mirror the SQL: the row survives, the claim columns are cleared. A
		// released row therefore has owner "" and a ZERO expiry, which is what
		// makes "renewing a released row must fail" observable.
		f.claims[id] = fakeClaim{}
	}
	return nil
}

func (f *fakeStore) ClearKickoffClaimsByOwnerPrefix(_ context.Context, prefix string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if prefix == "" {
		return 0, errors.New("empty owner prefix would clear every pod's claims")
	}
	var n int64
	for id, c := range f.claims {
		if c.owner != "" && strings.HasPrefix(c.owner, prefix) {
			f.claims[id] = fakeClaim{}
			n++
		}
	}
	return n, nil
}

func (f *fakeStore) SetKickoffError(ctx context.Context, id int64, msg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a, ok := f.agents[id]; ok {
		a.KickoffError = msg
		f.agents[id] = a
	}
	return nil
}

func (f *fakeStore) kickedOff(id int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.agents[id].KickedOff
}

func (f *fakeStore) writeSet() map[int64]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := make(map[int64]string, len(f.writes))
	for _, w := range f.writes {
		m[w.id] = w.status
	}
	return m
}

func (f *fakeStore) writeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.writes)
}

// Remaining Store methods — present but minimal (unused by the reconciler).
func (f *fakeStore) Create(ctx context.Context, a Agent) (Agent, error)        { return a, nil }
func (f *fakeStore) GetByName(ctx context.Context, name string) (Agent, error) { return Agent{}, nil }
func (f *fakeStore) GetByHooksToken(ctx context.Context, token string) (Agent, error) {
	return Agent{}, nil
}
func (f *fakeStore) NameExists(ctx context.Context, name string) (bool, error) {
	if f.nameExistsFn != nil {
		return f.nameExistsFn(name)
	}
	return false, nil
}
func (f *fakeStore) AgentsByNoteIDs(ctx context.Context, ids []int64) (map[int64]Agent, error) {
	return map[int64]Agent{}, nil
}
func (f *fakeStore) SetModel(ctx context.Context, id int64, model string) (Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.agents[id]
	if !ok {
		return Agent{}, context.Canceled
	}
	a.Model = model
	f.agents[id] = a
	return a, nil
}
func (f *fakeStore) SetDisplayName(ctx context.Context, id int64, name string) (Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.agents[id]
	if !ok {
		return Agent{}, context.Canceled
	}
	if name == "" {
		name = a.Name
	}
	a.DisplayName = name
	f.agents[id] = a
	return a, nil
}
func (f *fakeStore) SetHooksToken(ctx context.Context, id int64, token string) error { return nil }
func (f *fakeStore) SetGatewaySecret(ctx context.Context, id int64, secret string) error {
	return nil
}
func (f *fakeStore) Delete(ctx context.Context, id int64) error {
	f.mu.Lock()
	f.deleted = append(f.deleted, id)
	fn := f.onDelete
	f.mu.Unlock()
	if fn != nil {
		fn(id)
	}
	return nil
}
func (f *fakeStore) CreateSession(ctx context.Context, agentID int64, agentName string) (ChatSession, error) {
	return ChatSession{ID: agentID, AgentID: agentID, SessionKey: "agent:" + agentName + ":webchat:new"}, nil
}
func (f *fakeStore) ListSessions(ctx context.Context, agentID int64) ([]ChatSession, error) {
	return nil, nil
}

// SearchSessions is unused by the reconciler under test here, so it returns an
// EMPTY slice rather than nil: this fake exists to satisfy the interface, and a
// nil-vs-empty difference is the kind of thing a future caller would read as "no
// matches" either way. The behaviour that matters lives in PGStore and is pinned
// against a real Postgres in pgstore_search_test.go.
func (f *fakeStore) SearchSessions(ctx context.Context, agentID int64, q string, limit int) ([]SessionMatch, error) {
	return []SessionMatch{}, nil
}
func (f *fakeStore) GetSession(ctx context.Context, sessionID int64) (ChatSession, error) {
	return ChatSession{ID: sessionID}, nil
}
func (f *fakeStore) LatestOrCreateSession(ctx context.Context, agentID int64, agentName string) (ChatSession, error) {
	return ChatSession{ID: agentID, AgentID: agentID, SessionKey: "agent:" + agentName + ":webchat"}, nil
}
func (f *fakeStore) AddChatMessage(ctx context.Context, m ChatMessage) (ChatMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m.Kind == "" {
		m.Kind = "text"
	}
	m.ID = int64(len(f.chatMsgs) + 1)
	f.chatMsgs = append(f.chatMsgs, m)
	return m, nil
}
func (f *fakeStore) ListChatMessages(ctx context.Context, sessionID int64) ([]ChatMessage, error) {
	return nil, nil
}
func (f *fakeStore) ListRecentChatMessages(ctx context.Context, sessionID int64, limit int) ([]ChatMessage, error) {
	return nil, nil
}
func (f *fakeStore) ListChatMessagesPage(ctx context.Context, sessionID, beforeID int64, limit int) ([]ChatMessage, error) {
	return nil, nil
}
func (f *fakeStore) DeleteChatMessagesOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	return 0, nil
}
func (f *fakeStore) MarkSessionRead(ctx context.Context, sessionID int64) error { return nil }
func (f *fakeStore) UnreadByAgent(ctx context.Context) ([]AgentUnread, error)   { return nil, nil }

// LastMessageByAgentIDs mirrors the grouped PG query over f.chatMsgs (the
// reconciler never reads it, but the Store contract requires it).
func (f *fakeStore) LastMessageByAgentIDs(ctx context.Context, ids []int64) (map[int64]time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	want := make(map[int64]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	out := map[int64]time.Time{}
	for _, m := range f.chatMsgs {
		if want[m.AgentID] && m.CreatedAt.After(out[m.AgentID]) {
			out[m.AgentID] = m.CreatedAt
		}
	}
	return out, nil
}

var _ Store = (*fakeStore)(nil)
