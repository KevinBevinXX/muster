package mirror_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/mirror"
)

// scriptedHost is a JSON host whose answer to each paste is reply's.
type scriptedHost struct {
	mu     sync.Mutex
	pastes []mirror.Msg
}

func (h *scriptedHost) got() []mirror.Msg {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]mirror.Msg(nil), h.pastes...)
}

func connectScripted(t *testing.T, r *rig, token, hello string, reply func(mirror.Msg) mirror.Msg) *scriptedHost {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+token)
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(r.srv.URL, "http")+mirror.ConnectPath, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	if err := ws.Write(ctx, websocket.MessageText, []byte(hello)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ws.Read(ctx); err != nil {
		t.Fatal(err)
	}
	h := &scriptedHost{}
	go func() {
		for {
			typ, data, err := ws.Read(context.Background())
			if err != nil {
				return
			}
			var m mirror.Msg
			if typ != websocket.MessageText || json.Unmarshal(data, &m) != nil || m.Type != mirror.TypePaste {
				continue
			}
			h.mu.Lock()
			h.pastes = append(h.pastes, m)
			h.mu.Unlock()
			a := reply(m)
			a.Type, a.ID = mirror.TypePasted, m.ID
			b, _ := json.Marshal(a)
			_ = ws.Write(context.Background(), websocket.MessageText, b)
		}
	}()
	return h
}

func okReply(mirror.Msg) mirror.Msg { return mirror.Msg{} }

// TestAHermesSendCarriesItsRuntimeAndSince: with the switch open, the
// rendered view's send (PasteClaude) into an idle Hermes window reaches the
// host as the GATED send — Runtime "hermes" and the window's StatusSince —
// while the Terminal composer's (PasteActivePane) stays a plain paste for the
// host's guard, and a Claude Code window's send is unchanged.
func TestAHermesSendCarriesItsRuntimeAndSince(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithTimeout(asUser(1), 5*time.Second)
	defer cancel()
	tok, _ := r.store.mint(t, 1, "on")
	h := connectScripted(t, r, tok, hermesHello("on", "idle", `,"hermesSend":true`), okReply)
	r.online(t, 1, "on", "work")
	for _, c := range []struct {
		window string
		target mirror.PasteTarget
	}{{"@1", mirror.PasteClaude}, {"@1", mirror.PasteActivePane}, {"@2", mirror.PasteClaude}} {
		if err := r.svc.PasteWindow(ctx, "on", "work", c.window, "hello", c.target); err != nil {
			t.Fatalf("%s/%d: %v", c.window, c.target, err)
		}
	}
	got := h.got()
	if len(got) != 3 {
		t.Fatalf("the host received %d pastes", len(got))
	}
	if m := got[0]; m.Runtime != mirror.RuntimeHermes || m.Since != 1792000001111 || m.AnyPane || m.Window != "@1" {
		t.Errorf("the Hermes send: %+v; want Runtime hermes, the window's since, no AnyPane", m)
	}
	if m := got[1]; m.Runtime != "" || m.Since != 0 || !m.AnyPane {
		t.Errorf("the Terminal composer's paste into the Hermes window: %+v; want a plain AnyPane paste (the host's guard)", m)
	}
	if m := got[2]; m.Runtime != "" || m.Since != 0 || m.AnyPane || m.Window != "@2" {
		t.Errorf("the Claude Code send: %+v; want it unchanged", m)
	}
}

// TestAHermesSendIsRefusedUnlessIdle: busy, waiting and unknown are refused
// at the hub with their own words, and NOTHING reaches the host; the Claude
// Code window beside it, busy, is pasted into as before (it queues input).
func TestAHermesSendIsRefusedUnlessIdle(t *testing.T) {
	for _, c := range []struct{ status, text string }{
		{"busy", mirror.HermesBusyText}, {"waiting", mirror.HermesWaitingText}, {"", mirror.HermesUnknownText},
	} {
		t.Run("status "+c.status, func(t *testing.T) {
			r := newRig(t)
			ctx, cancel := context.WithTimeout(asUser(1), 5*time.Second)
			defer cancel()
			tok, _ := r.store.mint(t, 1, "on")
			hello := hermesHello("on", c.status, `,"hermesSend":true`)
			if c.status == "" {
				hello = strings.Replace(hello, `"status":"","statusSince":1792000001111`, `"statusSince":0`, 1)
			}
			h := connectScripted(t, r, tok, hello, okReply)
			r.online(t, 1, "on", "work")
			err := r.svc.PasteWindow(ctx, "on", "work", "@1", "hello", mirror.PasteClaude)
			var ni *mirror.HermesNotIdleError
			if !errors.As(err, &ni) || !errors.Is(err, mirror.ErrHermesNotIdle) || err.Error() != c.text || errors.Is(err, mirror.ErrPasteUnconfirmed) {
				t.Fatalf("PasteWindow = %v; want %q", err, c.text)
			}
			if err := r.svc.PasteWindow(ctx, "on", "work", "@2", "to claude", mirror.PasteClaude); err != nil {
				t.Fatalf("control, the Claude Code window: %v", err)
			}
			time.Sleep(50 * time.Millisecond)
			if got := h.got(); len(got) != 1 || got[0].Window != "@2" {
				t.Fatalf("the host received %+v; want only the Claude Code paste", got)
			}
		})
	}
}

// TestTheGatedSendsOutcomesReachTheCaller: how the host's gated send ended
// decides what the caller may do next — "may have been submitted" is
// ErrPasteUnconfirmed (never a plain "not sent"), "superseded" is
// ErrSuperseded (a queued message is decided on again), and "pasted, not
// submitted" and "not submitted" are refusals that are neither.
func TestTheGatedSendsOutcomesReachTheCaller(t *testing.T) {
	for _, c := range []struct {
		refusal                  string
		unconfirmed, superseded bool
	}{
		{mirror.PasteRefusalHermesUnconfirmed, true, false},
		{mirror.PasteRefusalHermesSuperseded, false, true},
		{mirror.PasteRefusalHermesPasted, false, false},
		{mirror.PasteRefusalHermesNotSubmitted, false, false},
	} {
		t.Run(c.refusal, func(t *testing.T) {
			r := newRig(t)
			ctx, cancel := context.WithTimeout(asUser(1), 5*time.Second)
			defer cancel()
			tok, _ := r.store.mint(t, 1, "on")
			connectScripted(t, r, tok, hermesHello("on", "idle", `,"hermesSend":true`), func(mirror.Msg) mirror.Msg {
				return mirror.Msg{Error: "the host's words", Refusal: c.refusal}
			})
			r.online(t, 1, "on", "work")
			err := r.svc.PasteWindow(ctx, "on", "work", "@1", "hello", mirror.PasteClaude)
			var pr *mirror.PasteRefusedError
			var sup *mirror.SupersededError
			if err == nil || !errors.As(err, &pr) || pr.Refusal != c.refusal || pr.Why != "the host's words" {
				t.Fatalf("PasteWindow = %v", err)
			}
			if errors.Is(err, mirror.ErrPasteUnconfirmed) != c.unconfirmed {
				t.Errorf("unconfirmed = %t, want %t", !c.unconfirmed, c.unconfirmed)
			}
			if errors.As(err, &sup) != c.superseded || errors.Is(err, mirror.ErrSuperseded) != c.superseded {
				t.Errorf("superseded = %t, want %t", !c.superseded, c.superseded)
			}
		})
	}
}

// TestAQueuedSendToAHermesWindowThatTurnedBusyIsSuperseded: a QUEUED paste
// (ExpectTurn) into a Hermes window that is no longer idle is refused as
// SUPERSEDED — the queue puts the row back and decides on it afresh, as for
// Claude Code — never as the person-facing "not idle" refusal, which the queue
// would finish as a failure. A person's own Send (no turn) still gets the
// not-idle words. Nothing reaches the host either way.
func TestAQueuedSendToAHermesWindowThatTurnedBusyIsSuperseded(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithTimeout(asUser(1), 5*time.Second)
	defer cancel()
	tok, _ := r.store.mint(t, 1, "on")
	h := connectScripted(t, r, tok, hermesHello("on", "busy", `,"hermesSend":true`), okReply)
	r.online(t, 1, "on", "work")
	err := r.svc.PasteWindow(ctx, "on", "work", "@1", "queued", mirror.PasteClaude, mirror.ExpectTurn(mirror.Turn{Since: 1792000000000}))
	var sup *mirror.SupersededError
	var ni *mirror.HermesNotIdleError
	if !errors.As(err, &sup) || !errors.Is(err, mirror.ErrSuperseded) || errors.As(err, &ni) {
		t.Fatalf("a queued paste into a busy Hermes window = %v (%T); want superseded, not the not-idle refusal", err, err)
	}
	if err := r.svc.PasteWindow(ctx, "on", "work", "@1", "a person's", mirror.PasteClaude); !errors.As(err, &ni) {
		t.Fatalf("a person's send into a busy Hermes window = %v; want HermesNotIdleError", err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := h.got(); len(got) != 0 {
		t.Fatalf("the host received %+v", got)
	}
}

// TestAnIdleHermesWindowWithNoStatusTimeIsNotSentTo: "idle" with no since
// (statusSince 0) cannot be checked by the host's gated send (it re-checks
// idle AT that time), so the hub refuses it as unknown.
func TestAnIdleHermesWindowWithNoStatusTimeIsNotSentTo(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithTimeout(asUser(1), 5*time.Second)
	defer cancel()
	tok, _ := r.store.mint(t, 1, "on")
	hello := strings.Replace(hermesHello("on", "idle", `,"hermesSend":true`), `"status":"idle","statusSince":1792000001111`, `"status":"idle","statusSince":0`, 1)
	if hello == hermesHello("on", "idle", `,"hermesSend":true`) {
		t.Fatal("instrument check: the hello was not rewritten")
	}
	h := connectScripted(t, r, tok, hello, okReply)
	r.online(t, 1, "on", "work")
	st := r.svc.AgentState(asUser(1), "on", "work", "@1")
	err := r.svc.PasteWindow(ctx, "on", "work", "@1", "hello", mirror.PasteClaude)
	var ni *mirror.HermesNotIdleError
	if !errors.As(err, &ni) || err.Error() != mirror.HermesUnknownText {
		t.Fatalf("idle with no since (agent state %+v) = %v; want the unknown refusal", st, err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := h.got(); len(got) != 0 {
		t.Fatalf("the host received %+v", got)
	}
}
