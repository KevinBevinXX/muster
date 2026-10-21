package api

import (
	"strings"
	"testing"
)

// flippingProvisioner is recordingProvisioner whose deployment stops taking
// kickoffs once undeliverableNow says so — here, once a reservation exists:
// exactly the window between task_dispatch's reservation and its call.
type flippingProvisioner struct {
	*recordingProvisioner
	undeliverableNow func() bool
}

func (p *flippingProvisioner) KickoffUndeliverableReason() string {
	if p.undeliverableNow != nil && p.undeliverableNow() {
		return "the fixture deployment stopped taking kickoffs"
	}
	return ""
}

// TestADeploymentRefusalAfterTheReservationReleasesTheStart (#152 r2): the
// deployment's re-check AFTER the reservation refuses (it stopped taking
// kickoffs while the reservation ran). Nothing was created, so the task's one
// start is released: the next call, once the deployment takes kickoffs again,
// STARTS an agent instead of answering already_dispatched with none.
func TestADeploymentRefusalAfterTheReservationReleasesTheStart(t *testing.T) {
	var fp *flippingProvisioner
	f := newTaskRouteFixture(t, true, true, func(ext *Extensions) {
		fp = &flippingProvisioner{recordingProvisioner: &recordingProvisioner{}}
		ext.Provisioner = fp
	})
	f.prov = fp.recordingProvisioner
	n := f.task(t, "flip after reserve")
	fp.undeliverableNow = func() bool { return f.reservations(t, n.ID) > 0 }

	text := f.call(f.chief, taskDispatchTool, map[string]any{"task_id": n.ID})
	if !strings.Contains(text, `"error"`) || !strings.Contains(text, "stopped taking kickoffs") {
		t.Fatalf("the refused start: %s", text)
	}
	if got := f.agentsOn(t, n.ID); len(got) != 0 {
		t.Fatalf("a refused start created %d agent(s)", len(got))
	}

	// The deployment takes kickoffs again.
	fp.undeliverableNow = nil
	text = f.call(f.chief, taskDispatchTool, map[string]any{"task_id": n.ID})
	if strings.Contains(text, `"error"`) || strings.Contains(text, "already_dispatched") || !strings.Contains(text, `"agent_id"`) {
		t.Fatalf("the call after the refusal: %s; want a started agent", text)
	}
	if got := f.agentsOn(t, n.ID); len(got) != 1 {
		t.Fatalf("agents on the task after the retry: %d, want 1", len(got))
	}
	// The released start does not free a SECOND one: the next call is the
	// already_dispatched answer for the agent just started.
	if again := f.call(f.chief, taskDispatchTool, map[string]any{"task_id": n.ID}); !strings.Contains(again, `"already_dispatched":true`) {
		t.Fatalf("a third call: %s", again)
	}
}
