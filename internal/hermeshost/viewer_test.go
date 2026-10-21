package hermeshost

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/hermeshost/hermestest"
)

const viewSession = "local-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func attach(t *testing.T, dir string) (*Viewer, []ApprovalPrompt) {
	t.Helper()
	v, ps, err := NewHome(dir).Attach(gctx(t), viewSession)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(v.Close)
	return v, ps
}

func TestAttachListsTheSessionsPendingApprovals(t *testing.T) {
	dir := shortHome(t)
	g := hermestest.NewGateway(t, dir)
	g.AddPrompt("38e56375cfb240e2a3c84ed68712afdb", 3, "rm -r ./victim", "recursive delete")
	v, ps := attach(t, dir)
	if v.Session != viewSession {
		t.Fatalf("session = %q", v.Session)
	}
	want := []ApprovalPrompt{{PromptID: "38e56375cfb240e2a3c84ed68712afdb", Generation: 3, Command: "rm -r ./victim",
		Description: "recursive delete", Choices: []string{"once", "deny", "session", "always"}}}
	if !reflect.DeepEqual(ps, want) {
		t.Fatalf("prompts = %+v\nwant %+v", ps, want)
	}
	if got := g.Resumes; len(got) != 1 || got[0] != viewSession {
		t.Fatalf("resumed %q", got)
	}
}

// An approval.respond is EXACTLY {session_id, execution_generation, prompt_id,
// choice} (the CLI's own body, P:hermes_cli/gateway_agent.py, _on_approval).
func TestRespondSendsTheCLIsOwnBody(t *testing.T) {
	dir := shortHome(t)
	g := hermestest.NewGateway(t, dir)
	g.AddPrompt("p1", 2, "rm -r x", "")
	v, ps := attach(t, dir)
	if err := v.Respond(gctx(t), ps[0], "deny"); err != nil {
		t.Fatal(err)
	}
	got := g.RespondsSeen()
	if len(got) != 1 {
		t.Fatalf("responds = %v", got)
	}
	keys := []string{}
	for k := range got[0] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, []string{"choice", "execution_generation", "prompt_id", "session_id"}) ||
		got[0]["choice"] != "deny" || got[0]["prompt_id"] != "p1" || got[0]["session_id"] != viewSession || got[0]["execution_generation"] != float64(2) {
		t.Fatalf("respond = %v", got[0])
	}
}

// muster answers once or deny, and only a choice the approval offers:
// session and always are never sent (open question 1).
func TestRespondNeverSendsSessionOrAlways(t *testing.T) {
	dir := shortHome(t)
	g := hermestest.NewGateway(t, dir)
	g.AddPrompt("p1", 1, "rm -r x", "")
	v, ps := attach(t, dir)
	for _, c := range []string{"session", "always", "", "ONCE", "approve"} {
		if err := v.Respond(gctx(t), ps[0], c); err == nil || !strings.Contains(err.Error(), "once or deny only") {
			t.Fatalf("choice %q: err = %v", c, err)
		}
	}
	narrow := ps[0]
	narrow.Choices = []string{"deny"}
	if err := v.Respond(gctx(t), narrow, "once"); err == nil {
		t.Fatal("a choice the approval does not offer was sent")
	}
	if got := g.RespondsSeen(); len(got) != 0 {
		t.Fatalf("sent %v", got)
	}
}

func TestRespondReportsTheGatewaysRefusal(t *testing.T) {
	dir := shortHome(t)
	g := hermestest.NewGateway(t, dir)
	g.AddPrompt("p1", 1, "rm -r x", "")
	v, ps := attach(t, dir)
	g.RespondError = "approval_not_pending"
	err := v.Respond(gctx(t), ps[0], "once")
	if !errors.Is(err, ErrApprovalRefused) || !strings.Contains(err.Error(), "approval_not_pending") {
		t.Fatalf("err = %v", err)
	}
	// A free-text refusal is not quoted.
	g.RespondError = "unknown method: <" + hermestest.DecoyMarker + ">"
	err = v.Respond(gctx(t), ps[0], "once")
	if !errors.Is(err, ErrApprovalRefused) || strings.Contains(err.Error(), "DECOY") {
		t.Fatalf("err = %v", err)
	}
}

func TestTheViewerFollowsItsSessionsApprovals(t *testing.T) {
	dir := shortHome(t)
	g := hermestest.NewGateway(t, dir)
	v, ps := attach(t, dir)
	if len(ps) != 0 {
		t.Fatalf("prompts = %v", ps)
	}
	other := "local-ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	g.Event(other, "approval.request", map[string]any{"prompt_id": "pX", "command": "rm x", "choices": []any{"once", "deny"}})
	g.Event(viewSession, "approval.request", map[string]any{"prompt_id": "p2", "execution_generation": 4, "command": "rm -r y",
		"description": "recursive delete", "choices": []any{"once", "deny", "bogus"}})
	g.Event(viewSession, "approval.settled", map[string]any{"prompt_id": "p2"})
	want := []ViewerEvent{
		{Kind: EventApprovalRequest, PromptID: "p2", Prompt: ApprovalPrompt{PromptID: "p2", Generation: 4, Command: "rm -r y", Description: "recursive delete", Choices: []string{"once", "deny"}}},
		{Kind: EventApprovalSettled, PromptID: "p2"},
	}
	for i, w := range want {
		select {
		case e := <-v.Events():
			if !reflect.DeepEqual(e, w) {
				t.Fatalf("event %d = %+v\nwant %+v", i, e, w)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("event %d never came", i)
		}
	}
}

// 🔴 HYGIENE: every secret-bearing frame the gateway streams — the
// conversation and pending[].text in session.resume, session.info's
// system_prompt and credential_hint, deltas, tool results, and the sudo,
// secret and clarify server requests — carries the canary, and none of it
// reaches anything the viewer hands out; no server request is answered.
func TestTheViewerDropsEverySecretBearingFrame(t *testing.T) {
	dir := shortHome(t)
	g := hermestest.NewGateway(t, dir)
	g.AddPrompt("p1", 1, "rm -r x", "recursive delete")
	v, ps := attach(t, dir)
	g.SecretFrames(viewSession)
	g.Event(viewSession, "approval.settled", map[string]any{"prompt_id": "p1"})
	var seen []ViewerEvent
	for len(seen) == 0 {
		select {
		case e := <-v.Events():
			seen = append(seen, e)
		case <-time.After(5 * time.Second):
			t.Fatal("the settle never came: the viewer stopped reading")
		}
	}
	err := v.Respond(gctx(t), ps[0], "deny")
	all := fmt.Sprintf("%+v %+v %+v %v", ps, seen, v, err)
	if strings.Contains(all, "DECOY") {
		t.Fatalf("a secret-bearing field left the viewer: %s", all)
	}
	// The positive control: the approval's own fields DO come through.
	if !strings.Contains(all, "recursive delete") || seen[0].Kind != EventApprovalSettled {
		t.Fatalf("the approval itself did not come through: %s", all)
	}
	if n := g.ServerAnswersSeen(); n != 0 {
		t.Fatalf("the viewer answered %d server request(s)", n)
	}
}

// The structural half: the only values a viewer hands out are these fields.
// A field added here is a design change (docs/design/hermes.md, Secret
// hygiene).
func TestTheViewerKeepsOnlyTheApprovalFields(t *testing.T) {
	fields := func(v any) []string {
		var out []string
		rt := reflect.TypeOf(v)
		for i := 0; i < rt.NumField(); i++ {
			out = append(out, rt.Field(i).Name)
		}
		return out
	}
	if got := fields(ApprovalPrompt{}); !reflect.DeepEqual(got, []string{"PromptID", "Generation", "Command", "Description", "Choices", "Unshown"}) {
		t.Fatalf("ApprovalPrompt fields = %v", got)
	}
	if got := fields(ViewerEvent{}); !reflect.DeepEqual(got, []string{"Kind", "Prompt", "PromptID"}) {
		t.Fatalf("ViewerEvent fields = %v", got)
	}
	if got := fields(resumeWire{}); !reflect.DeepEqual(got, []string{"SessionID", "Generation", "Prompts"}) {
		t.Fatalf("resumeWire fields = %v", got)
	}
	if got := fields(promptWire{}); !reflect.DeepEqual(got, []string{"Kind", "PromptID", "Generation", "Command", "Description", "Choices", "extra"}) {
		t.Fatalf("promptWire fields = %v", got)
	}
}

func TestTheViewerEndsWhenTheGatewayHangsUp(t *testing.T) {
	dir := shortHome(t)
	g := hermestest.NewGateway(t, dir)
	g.AddPrompt("p1", 1, "rm -r x", "")
	v, ps := attach(t, dir)
	g.Close()
	select {
	case <-v.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the viewer did not notice the gateway left")
	}
	if err := v.Respond(gctx(t), ps[0], "deny"); err == nil {
		t.Fatal("an answer went through a viewer that had ended")
	}
}

// A ticket is good once: each attach asks for its own.
func TestEachAttachUsesItsOwnTicket(t *testing.T) {
	dir := shortHome(t)
	hermestest.NewGateway(t, dir)
	attach(t, dir)
	attach(t, dir)
}

// Attach refuses a gateway that is not the pin before it asks for a ticket.
func TestAttachRefusesAGatewayThatIsNotThePin(t *testing.T) {
	dir := shortHome(t)
	g := hermestest.NewGateway(t, dir)
	g.Set("code_sha", "d923a5eb487a17c0a68801bba9ef0f56c81040a8")
	if _, _, err := NewHome(dir).Attach(gctx(t), viewSession); !errors.Is(err, ErrGatewayNotPin) {
		t.Fatalf("err = %v", err)
	}
	if len(g.Tickets) != 0 {
		t.Fatalf("a ticket was minted for a gateway that is not the pin")
	}
}

// The viewer attaches only to the gateway it identified, over its protocol:
// a ticket naming another instance, or a WebSocket that does not speak
// hermes-gateway-v1, is refused before anything is asked of it.
func TestAttachRefusesAnotherInstanceOrProtocol(t *testing.T) {
	t.Run("a ticket for another instance", func(t *testing.T) {
		dir := shortHome(t)
		g := hermestest.NewGateway(t, dir)
		g.TicketInstance = "someoneelse0001"
		if _, _, err := NewHome(dir).Attach(gctx(t), viewSession); !errors.Is(err, ErrGatewayProtocol) || !strings.Contains(err.Error(), "ticket") {
			t.Fatalf("err = %v", err)
		}
		if len(g.Resumes) != 0 {
			t.Fatal("it resumed through a ticket for another gateway")
		}
	})
	t.Run("another WebSocket protocol", func(t *testing.T) {
		dir := shortHome(t)
		g := hermestest.NewGateway(t, dir)
		g.NoSubprotocol = true
		if _, _, err := NewHome(dir).Attach(gctx(t), viewSession); !errors.Is(err, ErrGatewayProtocol) || !strings.Contains(err.Error(), "another protocol") {
			t.Fatalf("err = %v", err)
		}
		time.Sleep(100 * time.Millisecond)
		g.Event(viewSession, "noop", nil)
		if len(g.Resumes) != 0 {
			t.Fatal("it resumed over another protocol")
		}
	})
}

// A same-uid gateway answering one request id several times never blocks the
// viewer's reader: the first answer is kept, the rest dropped, so Close
// always returns (audit #206).
func TestARepeatedAnswerNeverBlocksTheReader(t *testing.T) {
	v := &Viewer{waiting: map[int64]chan rpcAnswer{1: make(chan rpcAnswer, 1)}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 3; i++ {
			v.answer(frameWire{ID: json.RawMessage("1"), Result: json.RawMessage(`{"status":"resolved"}`)})
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the reader blocked on a repeated answer")
	}
	if a := <-v.waiting[1]; a.result == nil {
		t.Fatalf("the first answer was not kept: %+v", a)
	}
}

// End to end: the gateway answers approval.respond three times with one id,
// and the viewer still closes.
func TestCloseReturnsAfterARepeatedAnswer(t *testing.T) {
	dir := shortHome(t)
	g := hermestest.NewGateway(t, dir)
	g.AddPrompt("p1", 1, "rm -r x", "")
	g.RespondRepeat = 3
	v, ps, err := NewHome(dir).Attach(gctx(t), viewSession)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Respond(gctx(t), ps[0], "deny"); err != nil {
		t.Fatal(err)
	}
	// The duplicates are the next reads; give them time to land.
	time.Sleep(200 * time.Millisecond)
	closed := make(chan struct{})
	go func() { v.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close hung behind a repeated answer")
	}
}

// 🔴 A file-edit approval (command "Edit <path>" beside an `edit` diff, or any
// field muster does not render) is marked Unshown, from the resume and from
// an approval.request alike (audit #212).
func TestAnApprovalCarryingAnEditIsUnshown(t *testing.T) {
	dir := shortHome(t)
	g := hermestest.NewGateway(t, dir)
	g.AddPrompt("p1", 1, "rm -r x", "")
	g.Prompts = append(g.Prompts, map[string]any{"kind": "approval", "prompt_id": "p2", "execution_generation": 1,
		"command": "Edit src/main.go", "description": "Approve proposed file change", "choices": []any{"once", "deny"},
		"edit": map[string]any{"path": "src/main.go", "diff": "+rm -rf /"}})
	v, ps := attach(t, dir)
	if len(ps) != 2 || ps[0].Unshown || !ps[1].Unshown {
		t.Fatalf("prompts = %+v", ps)
	}
	g.Event(viewSession, "approval.request", map[string]any{"prompt_id": "p3", "execution_generation": 1, "command": "Edit a",
		"choices": []any{"once", "deny"}, "edit": map[string]any{"diff": "x"}})
	select {
	case e := <-v.Events():
		if !e.Prompt.Unshown {
			t.Fatalf("%+v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
	}
}
