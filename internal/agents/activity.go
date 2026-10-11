package agents

import (
	"sort"
	"time"
)

// LastActivity is THE definition of when an agent was last active: the newer of
// its row's UpdatedAt and lastMessage, the created_at of its most recent persisted
// transcript message (the zero time when it has none).
//
// It is the ONE rule both tiers use. GET /api/agents publishes it as
// `lastActivityAt` (internal/api's newAgentJSON, whose field comment is the
// full contract: what moves each half, and what neither can see), and the Agents
// list orders its cards by it ([SortByLastActivity]) and prints it in each card's
// footer. Before this function the API computed it inline while the list was
// ordered by CREATION time and labelled UpdatedAt "active" — three answers to one
// question, which is how the list read as unsorted.
//
// Why these two and not created_at: UpdatedAt already starts AT created_at (the
// row is inserted with both set to now()) and only moves forward, so created_at
// can never be the newer one; and a transcript message is the only durable trace
// of an agent WORKING, which UpdatedAt deliberately does not follow (see the API
// field comment — an agent running a long task has a frozen UpdatedAt).
func LastActivity(a Agent, lastMessage time.Time) time.Time {
	if lastMessage.After(a.UpdatedAt) {
		return lastMessage
	}
	return a.UpdatedAt
}

// SortByLastActivity orders list most-recently-active first by [LastActivity],
// in place. lastMessage maps an agent ID to its newest transcript time; a missing
// entry is "no transcript" (the zero time), so a failed lookup degrades to
// ordering by UpdatedAt rather than to no order at all.
//
// Ties — two rows touched in the same microsecond, which a batch writer produces —
// break by ID descending (newest agent first), so an idiomorph re-render never
// swaps two cards for a reason nobody can see.
func SortByLastActivity(list []Agent, lastMessage map[int64]time.Time) {
	sort.SliceStable(list, func(i, j int) bool {
		ai, aj := LastActivity(list[i], lastMessage[list[i].ID]), LastActivity(list[j], lastMessage[list[j].ID])
		if !ai.Equal(aj) {
			return ai.After(aj)
		}
		return list[i].ID > list[j].ID
	})
}
