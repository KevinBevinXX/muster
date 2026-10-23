package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/jobs"
)

// A JOB'S `model` AND `config_ref` (jobs.go): the model is the job's own Claude
// Code session's, so its turn — and only its turn — carries
// agents.HeaderCCModel; config_ref reaches the worktree's overlay
// (PrepareJobWorktree, whose script tests are in internal/agentprovision); and a
// replayed idempotency key with a different value of either is a 409, not the
// old job.

// testConfigRef is a config_ref a job may name, distinct from testFullSHA.
const testConfigRef = "fedcba9876543210fedcba9876543210fedcba98"

func (g *jobGateway) seenModels() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.models...)
}

func (s *jobAgents) sessionList() []agents.ChatSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agents.ChatSession(nil), s.sessions...)
}

// TestAJobsModelRidesOnlyItsOwnTurn: a job with a model opens its own Claude
// Code session WITH that model, and its turn carries it as
// agents.HeaderCCModel (ccd's --model); a job without one carries no header at
// all (today's behaviour), and neither does a later job on the same agent — the
// model is the job session's, not the agent's.
func TestAJobsModelRidesOnlyItsOwnTurn(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	f.gw.reply = "done"
	out := f.await(t, f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "review", "model": "claude-sonnet-4-5"}))
	if out["status"] != jobs.StatusSucceeded || out["model"] != "claude-sonnet-4-5" {
		t.Fatalf("job = %v (model %v), want succeeded with model claude-sonnet-4-5", out["status"], out["model"])
	}
	out = f.await(t, f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "review"}))
	if out["status"] != jobs.StatusSucceeded || out["model"] != "" {
		t.Fatalf("job = %v (model %v), want succeeded with no model", out["status"], out["model"])
	}
	if got := f.gw.seenModels(); len(got) != 2 || got[0] != "claude-sonnet-4-5" || got[1] != "" {
		t.Fatalf("the turns' %s = %q, want [claude-sonnet-4-5 \"\"]", agents.HeaderCCModel, got)
	}
	sess := f.ags.sessionList()
	if len(sess) != 2 || sess[0].Model != "claude-sonnet-4-5" || sess[1].Model != "" ||
		sess[0].Runtime != agents.SessionRuntimeClaudeCode || sess[1].Runtime != agents.SessionRuntimeClaudeCode {
		t.Fatalf("sessions = %+v, want two claude-code sessions, the first with the model", sess)
	}
	if sess[0].SessionKey == sess[1].SessionKey {
		t.Fatalf("both jobs ran in session %q", sess[0].SessionKey)
	}
}

// TestAModelJobNeverReachesTheAgentsOwnSession: on a store that cannot open a
// Claude Code session of the job's own, a job with a model fails before any
// turn — its turn would otherwise go to the agent's primary CLI, the session
// the model must never reach. A job without a model still runs there.
func TestAModelJobNeverReachesTheAgentsOwnSession(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	f.s.ext.Agents = legacyJobAgents{f.ags}
	f.gw.reply = "done"
	out := f.await(t, f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "review", "model": "opus"}))
	if out["status"] != jobs.StatusFailed || out["reason"] != jobs.ReasonInternal || out["error"] != JobModelNoOwnSessionError {
		t.Fatalf("job = %v/%v %q, want failed/internal %q", out["status"], out["reason"], out["error"], JobModelNoOwnSessionError)
	}
	if got := f.gw.seen(); len(got) != 0 {
		t.Fatalf("turns = %q, want none", got)
	}
	if out := f.await(t, f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "review"})); out["status"] != jobs.StatusSucceeded {
		t.Fatalf("a job without a model = %v, want succeeded", out)
	}
	if got := f.gw.seenModels(); len(got) != 1 || got[0] != "" {
		t.Fatalf("the turns' %s = %q, want one turn with none", agents.HeaderCCModel, got)
	}
}

// TestAJobsConfigRefReachesTheWorktree: config_ref is handed to the worktree
// with the job's sha, in its own argument; a sha job without one is made from
// the default branch as before (no config ref).
func TestAJobsConfigRefReachesTheWorktree(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	f.gw.reply = "done"
	out := f.await(t, f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "review", "repo": "acme/widgets",
		"sha": testFullSHA, "pr": 7, "config_ref": testConfigRef}))
	if out["status"] != jobs.StatusSucceeded || out["config_ref"] != testConfigRef {
		t.Fatalf("job = %v (config_ref %v), want succeeded with config_ref %s", out["status"], out["config_ref"], testConfigRef)
	}
	f.await(t, f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "review", "repo": "acme/widgets", "sha": testFullSHA, "pr": 7}))
	made, _ := f.ws.snapshot()
	want := []string{
		"keen-wren#1 acme/widgets#7@" + testFullSHA + " config=" + testConfigRef,
		"keen-wren#2 acme/widgets#7@" + testFullSHA,
	}
	if strings.Join(made, "|") != strings.Join(want, "|") {
		t.Fatalf("worktrees made = %q, want %q", made, want)
	}
}

// TestModelAndConfigRefSubmissionsTheAPIRefuses: each is a 400 with its reason,
// before anything is recorded.
func TestModelAndConfigRefSubmissionsTheAPIRefuses(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	sha := map[string]any{"repo": "acme/widgets", "sha": testFullSHA, "pr": 7}
	with := func(extra map[string]any) map[string]any {
		body := map[string]any{"agent": "keen-wren", "prompt": "x"}
		for k, v := range extra {
			body[k] = v
		}
		return body
	}
	withSHA := func(k string, v any) map[string]any {
		b := with(sha)
		b[k] = v
		return b
	}
	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"model with a space", with(map[string]any{"model": "opus --dangerously-skip-permissions"}), "model: "},
		{"model upper case", with(map[string]any{"model": "Opus"}), "model: "},
		{"an OpenRouter model", with(map[string]any{"model": "openrouter/anthropic/claude-opus-4"}), "model: "},
		{"model with a leading dash", with(map[string]any{"model": "-opus"}), "model: "},
		{"config_ref abbreviated", withSHA("config_ref", testConfigRef[:12]), "config_ref must be a full commit id"},
		{"config_ref 39 hex", withSHA("config_ref", testConfigRef[:39]), "config_ref must be a full commit id"},
		{"config_ref upper case", withSHA("config_ref", strings.ToUpper(testConfigRef)), "config_ref must be a full commit id"},
		{"config_ref a branch", withSHA("config_ref", "main"), "config_ref must be a full commit id"},
		{"config_ref without a sha", with(map[string]any{"config_ref": testConfigRef}), "config_ref is only for a job with a sha"},
	}
	for _, tc := range cases {
		code, out := f.call(t, http.MethodPost, "/api/jobs", tc.body, true)
		if msg, _ := out["error"].(string); code != http.StatusBadRequest || !strings.HasPrefix(msg, tc.want) {
			t.Errorf("%s: POST = %d %v, want 400 starting %q", tc.name, code, out, tc.want)
		}
	}
	if n := f.store.count(); n != 0 {
		t.Fatalf("refused submissions recorded %d job(s)", n)
	}
	// Positive controls: the aliases, a full id with the [1m] suffix, and a
	// well-formed config_ref on a sha job are accepted.
	for _, body := range []map[string]any{
		with(map[string]any{"model": "haiku"}),
		with(map[string]any{"model": "claude-opus-4-1[1m]"}),
		withSHA("config_ref", testConfigRef),
	} {
		f.submit(t, body)
	}
}

// noKeyReadJobs is memJobs whose idempotency-key READ never finds a job, so a
// replay reaches Create, which answers the existing job (created == false) — the
// path a race between two first submissions takes.
type noKeyReadJobs struct{ *memJobs }

func (noKeyReadJobs) ByIdempotencyKey(context.Context, string) (jobs.Job, error) {
	return jobs.Job{}, jobs.ErrNotFound
}

// TestAReplayWithADifferentModelOrConfigRefIsRefused: a key already used, sent
// again with a different model or config_ref — including one absent where the
// first had it, or present where it had none — is a 409 naming the difference
// and the first job, with nothing recorded or run; the same values are still
// the deduplicated 200. Both places a replay is answered are covered: the read
// before the job is placed, and the store's own answer from Create.
func TestAReplayWithADifferentModelOrConfigRefIsRefused(t *testing.T) {
	for _, path := range []string{"key read", "create"} {
		t.Run(path, func(t *testing.T) {
			f := newJobFixture(t, testServiceToken)
			if path == "create" {
				f.s.ext.Jobs = noKeyReadJobs{f.store}
			}
			f.gw.reply = "done"
			first := map[string]any{"agent": "keen-wren", "prompt": "review", "repo": "acme/widgets", "sha": testFullSHA, "pr": 7,
				"model": "opus", "config_ref": testConfigRef, "idempotency_key": "review-42"}
			id := f.submit(t, first)
			f.await(t, id)
			change := func(k string, v any) map[string]any {
				b := map[string]any{}
				for kk, vv := range first {
					b[kk] = vv
				}
				if v == nil {
					delete(b, k)
				} else {
					b[k] = v
				}
				return b
			}
			for _, tc := range []struct {
				name string
				body map[string]any
				want string
			}{
				{"another model", change("model", "sonnet"), `has model "opus", not "sonnet"`},
				{"no model", change("model", nil), `has model "opus", not ""`},
				{"another config_ref", change("config_ref", strings.Repeat("a", 40)), `has config_ref "` + testConfigRef + `", not "` + strings.Repeat("a", 40) + `"`},
				{"no config_ref", change("config_ref", nil), `has config_ref "` + testConfigRef + `", not ""`},
			} {
				code, out := f.call(t, http.MethodPost, "/api/jobs", tc.body, true)
				msg, _ := out["error"].(string)
				if code != http.StatusConflict || out["idempotency_mismatch"] != true || out["deduplicated"] != nil ||
					out["job_id"] != float64(id) || !strings.Contains(msg, tc.want) {
					t.Errorf("%s: replay = %d %v, want 409 idempotency_mismatch naming job %d and %q", tc.name, code, out, id, tc.want)
				}
			}
			code, out := f.call(t, http.MethodPost, "/api/jobs", first, true)
			if code != http.StatusOK || out["deduplicated"] != true || out["job_id"] != float64(id) {
				t.Fatalf("the same body again = %d %v, want 200 deduplicated job %d", code, out, id)
			}
			if n := f.store.count(); n != 1 {
				t.Fatalf("%d jobs recorded, want 1", n)
			}
			if n := len(f.gw.seen()); n != 1 {
				t.Fatalf("%d turns ran, want 1", n)
			}
		})
	}
	// A key first used WITHOUT either field: a replay that adds one is refused.
	f := newJobFixture(t, testServiceToken)
	f.gw.reply = "done"
	id := f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "review", "idempotency_key": "plain"})
	f.await(t, id)
	if code, out := f.call(t, http.MethodPost, "/api/jobs", map[string]any{"agent": "keen-wren", "prompt": "review",
		"idempotency_key": "plain", "model": "opus"}, true); code != http.StatusConflict {
		t.Fatalf("adding a model on replay = %d %v, want 409", code, out)
	}
}
