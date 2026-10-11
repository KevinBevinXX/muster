package api

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
)

// activityStore lists agents in the store's own order (newest CREATED first) and
// answers the batched last-transcript read from a fixed map.
type activityStore struct {
	agents.Store
	rows []agents.Agent
	msgs map[int64]time.Time
}

func (s *activityStore) List(context.Context) ([]agents.Agent, error) {
	return append([]agents.Agent(nil), s.rows...), nil
}

func (s *activityStore) LastMessageByAgentIDs(_ context.Context, ids []int64) (map[int64]time.Time, error) {
	out := map[int64]time.Time{}
	for _, id := range ids {
		if at, ok := s.msgs[id]; ok {
			out[id] = at
		}
	}
	return out, nil
}

// activityFixture: creation order is the REVERSE of activity order, so the
// store's created_at order and the activity order disagree at every position.
// Times are pairwise distinct.
func activityFixture() *activityStore {
	t0 := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	return &activityStore{
		rows: []agents.Agent{
			{ID: 7, Name: "idle-newest", DisplayName: "idle-newest", Status: agents.StatusStopped,
				CreatedAt: t0.Add(5 * time.Hour), UpdatedAt: t0.Add(5 * time.Hour)},
			{ID: 6, Name: "status-moved", DisplayName: "status-moved", Status: agents.StatusRunning,
				CreatedAt: t0.Add(4 * time.Hour), UpdatedAt: t0.Add(8 * time.Hour)},
			{ID: 5, Name: "chatting-oldest", DisplayName: "chatting-oldest", Status: agents.StatusRunning,
				CreatedAt: t0.Add(1 * time.Hour), UpdatedAt: t0.Add(1 * time.Hour)},
		},
		// The oldest agent's transcript is the newest thing on the board.
		msgs: map[int64]time.Time{5: t0.Add(9 * time.Hour)},
	}
}

var agentCardID = regexp.MustCompile(`<article id="agent-(\d+)"`)

// TestTheAgentsListIsOrderedByMostRecentActivity: the operator report "agent
// list should be sorted by most recently active". It renders /ui/agents and reads
// the card order. Red on the tree before agents.SortByLastActivity, which
// rendered the store's created_at order (7, 6, 5).
func TestTheAgentsListIsOrderedByMostRecentActivity(t *testing.T) {
	store := activityFixture()
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(os.Stderr, "", 0))
	s.UseExtensions(Extensions{Agents: store, SessionLiveness: stubLiveness{}})
	h := s.Handler()

	body := getOwed(t, s, h, "/ui/agents").Body.String()
	var order []string
	for _, m := range agentCardID.FindAllStringSubmatch(body, -1) {
		order = append(order, m[1])
	}
	if got, want := strings.Join(order, ","), "5,6,7"; got != want {
		t.Fatalf("/ui/agents card order = %s, want %s (most recently active first: a transcript "+
			"newer than every status change, then the latest status change, then the idle one)", got, want)
	}

	// The footer's "active" time is the sort key, not UpdatedAt: the chatting
	// agent's card must print its TRANSCRIPT time.
	card := body[strings.Index(body, `id="agent-5"`):strings.Index(body, `id="agent-6"`)]
	wantTS := `data-ts="` + itoa(store.msgs[5].UnixMilli()) + `"`
	if !strings.Contains(card, wantTS) {
		t.Errorf("agent 5's card does not print its last activity (%s); the order on screen would "+
			"contradict the label beside it", wantTS)
	}
}

// TestTheMachineAgentListKeepsItsDocumentedOrder is an INVARIANT GUARD: the bug
// never touched it. GET /api/agents documents "newest-first as the store returns
// them" to its producers, and the UI's re-ordering must not leak into it — while
// its lastActivityAt is the SAME definition the UI sorts by.
func TestTheMachineAgentListKeepsItsDocumentedOrder(t *testing.T) {
	store := activityFixture()
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(os.Stderr, "", 0))
	s.UseExtensions(Extensions{Agents: store, SessionLiveness: stubLiveness{}})
	h := s.Handler()

	rec := getOwed(t, s, h, "/api/agents")
	var rows []struct {
		ID             int64     `json:"id"`
		LastActivityAt time.Time `json:"lastActivityAt"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range rows {
		ids = append(ids, itoa(r.ID))
		var a agents.Agent
		for _, row := range store.rows {
			if row.ID == r.ID {
				a = row
			}
		}
		if want := agents.LastActivity(a, store.msgs[r.ID]); !r.LastActivityAt.Equal(want) {
			t.Errorf("agent %d: lastActivityAt %v, want agents.LastActivity = %v", r.ID, r.LastActivityAt, want)
		}
	}
	if got := strings.Join(ids, ","); got != "7,6,5" {
		t.Errorf("GET /api/agents order = %s, want the store's 7,6,5 unchanged", got)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
