package api

import (
	"testing"

	"github.com/ZacxDev/muster/internal/jobs"
)

// TestAModelJobAnsweredFromTheAgentsOwnCLIIsRefused: a ccd that ignores the
// session and model headers (noEcho: it answers from the agent's primary CLI,
// without agents.HeaderCCWorkdirUsed, as a ccd from before session CLIs does)
// fails a model job model_unconfirmed, its reply discarded — it ran on the
// primary's model, not the job's. A job with neither a model nor a sha is not
// affected by the same ccd. The control is the same model job against a ccd
// that answers from the session's CLI (the echo), which succeeds.
func TestAModelJobAnsweredFromTheAgentsOwnCLIIsRefused(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	f.gw.reply = "the primary CLI's answer"
	f.gw.noEcho = true
	out := f.await(t, f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "review", "model": "sonnet"}))
	if out["status"] != jobs.StatusFailed || out["reason"] != jobs.ReasonModelUnconfirmed || out["error"] != JobModelUnconfirmedError {
		t.Fatalf("job = %v/%v %q, want failed/%s %q", out["status"], out["reason"], out["error"], jobs.ReasonModelUnconfirmed, JobModelUnconfirmedError)
	}
	if _, ok := out["result"]; ok {
		t.Fatalf("an unconfirmed model job carries a result: %v", out["result"])
	}
	if out := f.await(t, f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "review"})); out["status"] != jobs.StatusSucceeded ||
		out["result"] != "the primary CLI's answer" {
		t.Fatalf("a job with neither field on the same ccd = %v, want succeeded with the reply", out)
	}
	f.gw.mu.Lock()
	f.gw.noEcho = false
	f.gw.mu.Unlock()
	if out := f.await(t, f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "review", "model": "sonnet"})); out["status"] != jobs.StatusSucceeded {
		t.Fatalf("control: a model job answered from its own session CLI = %v, want succeeded", out)
	}
}
