package mirror_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// The Claude prompt guard's hub half (Hub.PasteWindow). The refusal kind is
// written as its wire literal, "claude-waiting", not the constant: the wire
// value is the contract a host and an older muster agree on.

// claudeGuardHello is an -interactive, pane-paste, status-reporting v2 host
// whose one session has Claude Code in @2 with status (since t2).
func claudeGuardHello(host, status string) string {
	return `{"type":"hello","version":2,"host":"` + host + `","interactive":true,"panePaste":true,"statusReport":true,"windows":` +
		`[{"session":"work","id":"@2","index":1,"claude":true,"claudeSession":"0b2c3d4e-5f60-4718-8a9b-0c1d2e3f4a5b","status":"` + status + `","statusSince":1792000002222}]}`
}

// TestASendToAClaudeWindowWaitingOnThePersonIsRefusedBeforeTheHost: a
// rendered-view paste into a Claude Code window its host reports WAITING (a
// permission prompt, a question) never reaches the host — Enter there would
// answer the prompt. A person's Send is refused with the guard's refusal; a
// queued one (ExpectTurn) is superseded, so the queue puts it back and holds
// it. The Terminal view's composer into the same window is the control: it is
// the person typing into the pane they see, and still reaches the host.
func TestASendToAClaudeWindowWaitingOnThePersonIsRefusedBeforeTheHost(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithTimeout(asUser(1), 5*time.Second)
	defer cancel()
	tok, _ := r.store.mint(t, 1, "on")
	h := connectScripted(t, r, tok, claudeGuardHello("on", "waiting"), okReply)
	r.online(t, 1, "on", "work")

	err := r.svc.PasteWindow(ctx, "on", "work", "@2", "1", mirror.PasteClaude)
	var pr *mirror.PasteRefusedError
	if !errors.As(err, &pr) || pr.Refusal != "claude-waiting" || errors.Is(err, mirror.ErrSuperseded) {
		t.Fatalf("a person's send into a waiting Claude window = %v (%T); want the claude-waiting refusal", err, err)
	}
	err = r.svc.PasteWindow(ctx, "on", "work", "@2", "queued", mirror.PasteClaude, mirror.ExpectTurn(mirror.Turn{Since: 1792000002222}))
	var sup *mirror.SupersededError
	if !errors.As(err, &sup) || !errors.Is(err, mirror.ErrSuperseded) {
		t.Fatalf("a queued send into a waiting Claude window = %v (%T); want superseded (held, decided on again)", err, err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := h.got(); len(got) != 0 {
		t.Fatalf("the host received %+v; want nothing", got)
	}
	if err := r.svc.PasteWindow(ctx, "on", "work", "@2", "typed", mirror.PasteActivePane); err != nil {
		t.Fatalf("control, the Terminal composer into the same window: %v", err)
	}
	if got := h.got(); len(got) != 1 || !got[0].AnyPane {
		t.Fatalf("the host received %+v; want the Terminal composer's one paste", got)
	}
}

// TestAnIdleClaudeWindowIsStillSentTo is the positive control of the test
// above: the same window, idle, takes a person's Send and a queued one.
func TestAnIdleClaudeWindowIsStillSentTo(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithTimeout(asUser(1), 5*time.Second)
	defer cancel()
	tok, _ := r.store.mint(t, 1, "on")
	h := connectScripted(t, r, tok, claudeGuardHello("on", "idle"), okReply)
	r.online(t, 1, "on", "work")
	if err := r.svc.PasteWindow(ctx, "on", "work", "@2", "hello", mirror.PasteClaude); err != nil {
		t.Fatalf("a send into an idle Claude window: %v", err)
	}
	if got := h.got(); len(got) != 1 || got[0].AnyPane {
		t.Fatalf("the host received %+v; want one rendered-view paste", got)
	}
}

// TestAQueuedPasteTheHostRefusesAsWaitingGoesBackToTheQueue: the race the
// host's half closes. muster still believes the window idle (the host has not
// reported the prompt yet), so the paste reaches the host, which refuses it
// at the moment of typing. A QUEUED paste refused so is superseded — nothing
// was typed, and the queue decides on it again — while a person's Send gets
// the host's refusal in its own words.
func TestAQueuedPasteTheHostRefusesAsWaitingGoesBackToTheQueue(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithTimeout(asUser(1), 5*time.Second)
	defer cancel()
	tok, _ := r.store.mint(t, 1, "on")
	h := connectScripted(t, r, tok, claudeGuardHello("on", "idle"), func(mirror.Msg) mirror.Msg {
		return mirror.Msg{Error: "the host's words", Refusal: "claude-waiting"}
	})
	r.online(t, 1, "on", "work")

	err := r.svc.PasteWindow(ctx, "on", "work", "@2", "queued", mirror.PasteClaude, mirror.ExpectTurn(mirror.Turn{Since: 1792000002222}))
	var sup *mirror.SupersededError
	if !errors.As(err, &sup) || !errors.Is(err, mirror.ErrSuperseded) {
		t.Fatalf("a queued paste the host refused as waiting = %v (%T); want superseded", err, err)
	}
	err = r.svc.PasteWindow(ctx, "on", "work", "@2", "a person's", mirror.PasteClaude)
	var pr *mirror.PasteRefusedError
	if !errors.As(err, &pr) || pr.Refusal != "claude-waiting" || pr.Why != "the host's words" || errors.Is(err, mirror.ErrSuperseded) {
		t.Fatalf("a person's paste the host refused as waiting = %v; want the host's refusal", err)
	}
	if got := h.got(); len(got) != 2 {
		t.Fatalf("the host received %d pastes; want both (muster believed the window idle)", len(got))
	}
	// A refused paste started no turn: the next queued decision is on the
	// same turn, and is not superseded by the refusal itself.
	st := r.svc.AgentState(asUser(1), "on", "work", "@2")
	if st.Pasted != 0 {
		t.Fatalf("a refused paste was recorded as a paste (Pasted %d)", st.Pasted)
	}
}

// TestAPastedNotSubmittedCountsAsLanded: when the host withheld the Enter
// (claude-pasted), the text is in Claude Code's input box, and whatever is
// pasted next would be submitted with it. So the hub records it as a paste
// that may have landed — the window's Pasted rises — and the queue holds the
// next message for the turn after it, exactly as after a Send.
func TestAPastedNotSubmittedCountsAsLanded(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithTimeout(asUser(1), 5*time.Second)
	defer cancel()
	tok, _ := r.store.mint(t, 1, "on")
	connectScripted(t, r, tok, claudeGuardHello("on", "idle"), func(mirror.Msg) mirror.Msg {
		return mirror.Msg{Error: "Pasted, not submitted: …", Refusal: "claude-pasted"}
	})
	r.online(t, 1, "on", "work")
	if err := r.svc.PasteWindow(ctx, "on", "work", "@2", "left in the box", mirror.PasteClaude); err == nil {
		t.Fatal("a pasted-not-submitted answer was taken as sent")
	}
	if st := r.svc.AgentState(asUser(1), "on", "work", "@2"); st.Pasted != 1792000002222 {
		t.Fatalf("after a pasted-not-submitted the window's Pasted is %d; want 1792000002222 (recorded as landed)", st.Pasted)
	}
}
