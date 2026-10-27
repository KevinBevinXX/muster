package api

import "context"

// LatestSessionIDs for the agent-store fakes that embed a NIL agents.Store (any
// method they do not define panics). Each answers "no session" (an empty map):
// what Server.latestSessionIDs also gives on a failed read, so every link these
// fixtures render is the bare /agents/{name} — the behaviour they were written
// against. The tests of the session-naming links use the Postgres-backed store
// (agent_href_pg_test.go).
//
// stubAgentsStore (registration_test.go) covers every fake built on it
// (sendAgents, jobAgents, mirrorAgents, termAgents, …).

func (*namespaceCapturingStore) LatestSessionIDs(context.Context, []int64) (map[int64]int64, error) {
	return map[int64]int64{}, nil
}

func (listAgents) LatestSessionIDs(context.Context, []int64) (map[int64]int64, error) {
	return map[int64]int64{}, nil
}

func (listedAgents) LatestSessionIDs(context.Context, []int64) (map[int64]int64, error) {
	return map[int64]int64{}, nil
}

func (*modelStore) LatestSessionIDs(context.Context, []int64) (map[int64]int64, error) {
	return map[int64]int64{}, nil
}

func (*owedStore) LatestSessionIDs(context.Context, []int64) (map[int64]int64, error) {
	return map[int64]int64{}, nil
}

func (relayAgents) LatestSessionIDs(context.Context, []int64) (map[int64]int64, error) {
	return map[int64]int64{}, nil
}

func (*activityStore) LatestSessionIDs(context.Context, []int64) (map[int64]int64, error) {
	return map[int64]int64{}, nil
}

func (gtAgents) LatestSessionIDs(context.Context, []int64) (map[int64]int64, error) {
	return map[int64]int64{}, nil
}

func (oneAgentStore) LatestSessionIDs(context.Context, []int64) (map[int64]int64, error) {
	return map[int64]int64{}, nil
}

func (*preflightStore) LatestSessionIDs(context.Context, []int64) (map[int64]int64, error) {
	return map[int64]int64{}, nil
}

func (*seamStore) LatestSessionIDs(context.Context, []int64) (map[int64]int64, error) {
	return map[int64]int64{}, nil
}

func (stubAgentsStore) LatestSessionIDs(context.Context, []int64) (map[int64]int64, error) {
	return map[int64]int64{}, nil
}
