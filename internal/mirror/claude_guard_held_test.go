package mirror_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestAHostsGuardRefusalHoldsTheWindowWithItsReason: when the host refuses a
// paste on what it read at the moment of typing (here, text in the box) while
// the status says idle, the window's agent state carries the hold and the
// host's words (AgentState.GuardHeld) — what the send queue shows and waits
// on, instead of re-sending at every pass with no reason.
func TestAHostsGuardRefusalHoldsTheWindowWithItsReason(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithTimeout(asUser(1), 5*time.Second)
	defer cancel()
	tok, _ := r.store.mint(t, 1, "on")
	connectScripted(t, r, tok, claudeGuardHello("on", "idle"), func(mirror.Msg) mirror.Msg {
		return mirror.Msg{Error: mirror.ClaudeBoxText, Refusal: mirror.PasteRefusalClaudeBox}
	})
	r.online(t, 1, "on", "work")
	if st := r.svc.AgentState(asUser(1), "on", "work", "@2"); st.GuardHeld != "" {
		t.Fatalf("held before any refusal: %q", st.GuardHeld)
	}
	err := r.svc.PasteWindow(ctx, "on", "work", "@2", "queued", mirror.PasteClaude, mirror.ExpectTurn(mirror.Turn{Since: 1792000002222}))
	var sup *mirror.SupersededError
	if !errors.As(err, &sup) {
		t.Fatalf("a queued paste the host refused for a full box = %v; want superseded", err)
	}
	st := r.svc.AgentState(asUser(1), "on", "work", "@2")
	if !strings.Contains(st.GuardHeld, "input box already has text") {
		t.Fatalf("after the refusal the window's hold is %q; want the host's reason", st.GuardHeld)
	}
}
