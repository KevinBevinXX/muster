package jobs

import "testing"

// TestThePGStoreRoundTripsAJobsModelAndConfigRef: model and config_ref (0044)
// are written and read back as given, by id and by idempotency key, and empty
// when a job names neither; migration 0044's CHECKs refuse a model outside the
// Claude Code session pattern, a config_ref that is not a full object id, and
// a config_ref on a job without a sha. Each refusal has a control that differs
// from it in that one value and is accepted.
func TestThePGStoreRoundTripsAJobsModelAndConfigRef(t *testing.T) {
	s, pool := pgStore(t)
	a := mustAgent(t, pool, 1, "job-model")
	ctx := as(1)
	const cfg = "fedcba9876543210fedcba9876543210fedcba98"
	j, created, err := s.Create(ctx, NewJob{AgentID: a.ID, Agent: a.Name, Prompt: "review", Repo: "acme/widgets",
		SHA: fullSHA, PR: 7, TimeoutSeconds: 90, IdempotencyKey: "model-cfg", Model: "claude-opus-4-1[1m]", ConfigRef: cfg})
	if err != nil || !created {
		t.Fatalf("create = %v, %v", created, err)
	}
	for _, read := range []func() (Job, error){
		func() (Job, error) { return j, nil },
		func() (Job, error) { return s.Get(ctx, j.ID) },
		func() (Job, error) { return s.ByIdempotencyKey(ctx, "model-cfg") },
	} {
		got, err := read()
		if err != nil || got.Model != "claude-opus-4-1[1m]" || got.ConfigRef != cfg {
			t.Fatalf("read = model %q config_ref %q (%v), want %q / %q", got.Model, got.ConfigRef, err, "claude-opus-4-1[1m]", cfg)
		}
	}
	plain, _, err := s.Create(ctx, NewJob{AgentID: a.ID, Agent: a.Name, Prompt: "review", TimeoutSeconds: 90})
	if err != nil || plain.Model != "" || plain.ConfigRef != "" {
		t.Fatalf("a job naming neither = model %q config_ref %q (%v), want both empty", plain.Model, plain.ConfigRef, err)
	}
	for _, tc := range []struct {
		name string
		n    NewJob
	}{
		{"model upper case", NewJob{Model: "Opus"}},
		{"model with a space", NewJob{Model: "opus x"}},
		{"config_ref abbreviated", NewJob{SHA: fullSHA, ConfigRef: cfg[:12]}},
		{"config_ref without a sha", NewJob{ConfigRef: cfg}},
	} {
		n := tc.n
		n.AgentID, n.Agent, n.Prompt, n.TimeoutSeconds = a.ID, a.Name, "review", 90
		if _, _, err := s.Create(ctx, n); err == nil {
			t.Errorf("%s: recorded, want the CHECK to refuse it", tc.name)
		}
	}
	// Controls: one value away from a refusal each. Two more jobs: with the two
	// above, the agent's MaxActivePerAgent (4) exactly.
	for _, n := range []NewJob{{Model: "opus"}, {SHA: fullSHA, ConfigRef: cfg}} {
		n.AgentID, n.Agent, n.Prompt, n.TimeoutSeconds = a.ID, a.Name, "review", 90
		if _, _, err := s.Create(ctx, n); err != nil {
			t.Errorf("control %+v: %v", n, err)
		}
	}
}
