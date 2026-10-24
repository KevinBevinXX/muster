package api

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/plugins"
)

// A PLUGIN MESSAGE THE CHIEF DID NOT ACT ON BECOMES AN ASK (unrouted.go,
// internal/plugins/unrouted.go, migration 0050), through the real server and
// stores against a real Postgres: the attention sweep raises exactly one ask
// for a delivered, repliable message nobody routed, drafted or settled within
// plugins.UnroutedAskAfter — the sweep's clock injected, never slept — and
// none for one dispositioned in time; a disposition after the ask closes it;
// the owner's answer settles the message; and plugin_message_settle is the
// chief's alone, for its own messages.

// unroutedSweep runs one unrouted-message sweep as if d had passed since now.
func unroutedSweep(t *testing.T, g *gatedFixture, d time.Duration) {
	t.Helper()
	g.srv.unroutedNow = func() time.Time { return time.Now().Add(d) }
	g.srv.sweepUnroutedMessages(context.Background())
}

// unroutedAsks is every ask raised for message id, oldest first.
func unroutedAsks(t *testing.T, g *gatedFixture, id int64) []attention.Ask {
	t.Helper()
	rows, err := g.pool.Query(authz.System(context.Background()), `SELECT id FROM attention_asks WHERE plugin_message_id = $1 ORDER BY id`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []attention.Ask
	for rows.Next() {
		var ask int64
		if err := rows.Scan(&ask); err != nil {
			t.Fatal(err)
		}
		a, err := attention.NewPG(g.pool).Get(g.asOwner, ask)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

// settleReason is message id's settle_reason ("" for none), read as the system.
func settleReason(t *testing.T, g *gatedFixture, id int64) string {
	t.Helper()
	var r *string
	if err := g.pool.QueryRow(authz.System(context.Background()), `SELECT settle_reason FROM plugin_messages WHERE id = $1`, id).Scan(&r); err != nil {
		t.Fatal(err)
	}
	if r == nil {
		return ""
	}
	return *r
}

// callTool calls tool name over POST /mcp with token: the tool's text, whether
// MCP flagged it an error, or the JSON-RPC error (the tool not listed).
func (g *gatedFixture) callTool(t *testing.T, token, name string, args any) (string, bool, *mcpError) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args}})
	r, code := g.mcp(t, token, string(raw), nil)
	if r == nil {
		t.Fatalf("/mcp answered HTTP %d", code)
	}
	if r.Error != nil {
		return "", false, r.Error
	}
	text, isErr := toolText(t, r)
	return text, isErr, nil
}

func (g *gatedFixture) settle(t *testing.T, id int64, reason string) {
	t.Helper()
	if text, isErr, rpc := g.callTool(t, g.chiefTok, pluginMessageSettleToolName, map[string]any{"message_id": id, "reason": reason}); rpc != nil || isErr {
		t.Fatalf("settle %d: rpc=%+v text=%s", id, rpc, text)
	}
}

// TestAnUnactedMessageBecomesExactlyOneAskAfterTheDelay: before the delay no
// ask; after it ONE, raised by muster, naming the sender with the UNVERIFIED
// mark, the subject, the body, the chief and the message, with the
// "No action needed" option; sweeps after that raise no second one.
func TestAnUnactedMessageBecomesExactlyOneAskAfterTheDelay(t *testing.T) {
	g := newGatedFixture(t)
	m := g.deliveredMessage(t, g.owner, g.chief.ID, "unrouted-1")

	unroutedSweep(t, g, plugins.UnroutedAskAfter-time.Minute)
	if asks := unroutedAsks(t, g, m.ID); len(asks) != 0 {
		t.Fatalf("%d ask(s) a minute before the delay; want none", len(asks))
	}

	unroutedSweep(t, g, plugins.UnroutedAskAfter+time.Minute)
	asks := unroutedAsks(t, g, m.ID)
	if len(asks) != 1 {
		t.Fatalf("%d ask(s) after the delay; want exactly 1", len(asks))
	}
	a := asks[0]
	if a.Status != attention.Open || !a.Asker.System || a.Asker.AgentID != 0 || len(a.Options) != 1 || a.Options[0] != "No action needed" {
		t.Fatalf("the ask %+v; want an open system ask offering \"No action needed\"", a)
	}
	for _, want := range []string{"From: someone@example.test " + plugins.UnverifiedSenderMark, "Subject: (none)",
		"please do a thing", "your chief (chief)", "Message: " + itoa(m.ID), "Example inbox"} {
		if !strings.Contains(a.Question, want) {
			t.Errorf("the question does not carry %q:\n%s", want, a.Question)
		}
	}
	open, err := attention.NewPG(g.pool).Open(g.asOwner)
	if err != nil || len(open) != 1 || open[0].ID != a.ID {
		t.Fatalf("the owner's open asks %+v (%v); want just the raised one", open, err)
	}

	unroutedSweep(t, g, plugins.UnroutedAskAfter+time.Minute)
	unroutedSweep(t, g, 2*plugins.UnroutedAskAfter)
	if asks := unroutedAsks(t, g, m.ID); len(asks) != 1 {
		t.Fatalf("%d ask(s) after two more sweeps; want still 1", len(asks))
	}
	// …and the sweep no longer selects it at all: a message already asked about
	// that stayed in the due set would be re-read every sweep, and enough of
	// them would fill the batch and starve every newer message.
	due, err := g.plugins.UnroutedDue(context.Background(), time.Now().Add(3*plugins.UnroutedAskAfter), 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range due {
		if d.ID == m.ID {
			t.Fatalf("message %d, already asked about, is still due an ask", m.ID)
		}
	}
}

// TestTheAttentionSweepRaisesTheAsk: the wiring — RunAttentionSweep itself, not
// only its step, raises the ask.
func TestTheAttentionSweepRaisesTheAsk(t *testing.T) {
	g := newGatedFixture(t)
	m := g.deliveredMessage(t, g.owner, g.chief.ID, "wired-1")
	g.srv.unroutedNow = func() time.Time { return time.Now().Add(plugins.UnroutedAskAfter + time.Minute) }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); g.srv.RunAttentionSweep(ctx, 20*time.Millisecond) }()
	deadline := time.Now().Add(10 * time.Second)
	for len(unroutedAsks(t, g, m.ID)) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if asks := unroutedAsks(t, g, m.ID); len(asks) != 1 {
		t.Fatalf("RunAttentionSweep raised %d ask(s); want 1", len(asks))
	}
}

// TestTheRaiseRechecksUnderTheLock: a message dispositioned after the sweep
// read it as due, but before its ask was raised, gets none — RaiseUnrouted
// re-checks, under the message's lock, and does not call raise.
func TestTheRaiseRechecksUnderTheLock(t *testing.T) {
	g := newGatedFixture(t)
	m := g.deliveredMessage(t, g.owner, g.chief.ID, "recheck-1")
	asOf := time.Now().Add(plugins.UnroutedAskAfter + time.Minute)
	due, err := g.plugins.UnroutedDue(context.Background(), asOf, 1000)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range due {
		found = found || d.ID == m.ID
	}
	if !found {
		t.Fatalf("control: message %d is not due before it is settled", m.ID)
	}
	g.settle(t, m.ID, "handled")
	called := false
	raised, err := g.plugins.RaiseUnrouted(g.asOwner, m.ID, asOf, func(plugins.Message) error { called = true; return nil })
	if err != nil || raised || called {
		t.Fatalf("raise of a message settled since it was read: raised=%v called=%v err=%v; want neither", raised, called, err)
	}
}

// TestAMessageActedOnInTimeRaisesNoAsk: routed, drafted, settled before the
// delay — no ask for any of them — while an untouched one in the SAME sweep
// gets one (the positive control: the sweep did run and could raise).
func TestAMessageActedOnInTimeRaisesNoAsk(t *testing.T) {
	g := newGatedFixture(t)
	routed := g.deliveredMessage(t, g.owner, g.chief.ID, "acted-routed")
	drafted := g.deliveredMessage(t, g.owner, g.chief.ID, "acted-drafted")
	settled := g.deliveredMessage(t, g.owner, g.chief.ID, "acted-settled")
	control := g.deliveredMessage(t, g.owner, g.chief.ID, "acted-control")

	if _, err := g.plugins.CreateRoute(g.asOwner, g.chief.ID, g.owner, routed.ID,
		plugins.RouteTarget{Kind: "mirror", Key: routeSID, Label: "w"}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if text, isErr, rpc := g.callTool(t, g.chiefTok, replyDraftToolName, map[string]any{"message_id": drafted.ID, "body": "Thanks."}); rpc != nil || isErr {
		t.Fatalf("reply_draft: %+v %s", rpc, text)
	}
	g.settle(t, settled.ID, "a newsletter; nothing to do")

	unroutedSweep(t, g, plugins.UnroutedAskAfter+time.Minute)
	for name, id := range map[string]int64{"routed": routed.ID, "drafted": drafted.ID, "settled": settled.ID} {
		if asks := unroutedAsks(t, g, id); len(asks) != 0 {
			t.Errorf("the %s message raised %d ask(s); want none", name, len(asks))
		}
	}
	if asks := unroutedAsks(t, g, control.ID); len(asks) != 1 {
		t.Fatalf("control: the untouched message raised %d ask(s); want 1", len(asks))
	}
	if got := settleReason(t, g, settled.ID); got != "a newsletter; nothing to do" {
		t.Fatalf("the settled message's reason is %q", got)
	}
}

// TestConcurrentSweepsRaiseOneAsk: four sweeps at once over one due message —
// two processes sweeping, say — leave exactly one ask for it.
func TestConcurrentSweepsRaiseOneAsk(t *testing.T) {
	g := newGatedFixture(t)
	m := g.deliveredMessage(t, g.owner, g.chief.ID, "concurrent-1")
	g.srv.unroutedNow = func() time.Time { return time.Now().Add(plugins.UnroutedAskAfter + time.Minute) }
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g.srv.sweepUnroutedMessages(context.Background())
		}()
	}
	wg.Wait()
	if asks := unroutedAsks(t, g, m.ID); len(asks) != 1 {
		t.Fatalf("%d ask(s) after four concurrent sweeps; want exactly 1", len(asks))
	}
}

// TestADispositionAfterTheAskClosesIt: an ask raised for each of three messages;
// the chief then settles one, drafts a reply to one, routes one — the next
// sweep cancels all three asks — and an untouched fourth's ask stays open.
func TestADispositionAfterTheAskClosesIt(t *testing.T) {
	g := newGatedFixture(t)
	settled := g.deliveredMessage(t, g.owner, g.chief.ID, "late-settled")
	drafted := g.deliveredMessage(t, g.owner, g.chief.ID, "late-drafted")
	routed := g.deliveredMessage(t, g.owner, g.chief.ID, "late-routed")
	control := g.deliveredMessage(t, g.owner, g.chief.ID, "late-control")
	unroutedSweep(t, g, plugins.UnroutedAskAfter+time.Minute)
	for _, id := range []int64{settled.ID, drafted.ID, routed.ID, control.ID} {
		if asks := unroutedAsks(t, g, id); len(asks) != 1 || asks[0].Status != attention.Open {
			t.Fatalf("message %d: asks %+v; want one open", id, asks)
		}
	}

	g.settle(t, settled.ID, "dealt with by phone")
	if text, isErr, rpc := g.callTool(t, g.chiefTok, replyDraftToolName, map[string]any{"message_id": drafted.ID, "body": "On it."}); rpc != nil || isErr {
		t.Fatalf("reply_draft: %+v %s", rpc, text)
	}
	if _, err := g.plugins.CreateRoute(g.asOwner, g.chief.ID, g.owner, routed.ID,
		plugins.RouteTarget{Kind: "mirror", Key: routeSID, Label: "w"}, time.Hour); err != nil {
		t.Fatal(err)
	}
	unroutedSweep(t, g, plugins.UnroutedAskAfter+2*time.Minute)
	for name, id := range map[string]int64{"settled": settled.ID, "drafted": drafted.ID, "routed": routed.ID} {
		if asks := unroutedAsks(t, g, id); len(asks) != 1 || asks[0].Status != attention.Cancelled {
			t.Errorf("the %s message's asks %+v; want its one ask cancelled", name, asks)
		}
	}
	if asks := unroutedAsks(t, g, control.ID); len(asks) != 1 || asks[0].Status != attention.Open {
		t.Fatalf("control: the untouched message's asks %+v; want one, still open", asks)
	}
	// Closed asks leave the closing set: one that stayed would be re-read every
	// sweep, and enough of them would starve every newer close.
	left, err := g.plugins.UnroutedAsksToClose(context.Background(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range left {
		if u.MessageID == settled.ID || u.MessageID == drafted.ID || u.MessageID == routed.ID {
			t.Fatalf("ask %d (message %d) is still to be closed after it was closed", u.AskID, u.MessageID)
		}
	}
}

// TestANoReplyMessageNeverRaisesAnAsk: a message its plugin marked no_reply is
// never put to the owner, however long it waits; a repliable one beside it is.
func TestANoReplyMessageNeverRaisesAnAsk(t *testing.T) {
	g := newGatedFixture(t)
	control := g.deliveredMessage(t, g.owner, g.chief.ID, "noreply-control")
	m, _, err := g.plugins.Submit(g.asOwner, control.PluginID, plugins.MessageIn{ExternalID: "noreply-1", Sender: "alerts@example.test",
		Body: "your build passed", NoReply: true})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := g.plugins.BeginDelivery(g.asOwner, m.ID, g.chief.ID); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if ok, err := g.plugins.RecordDelivery(g.asOwner, m.ID, g.chief.ID, plugins.StatusDelivered); err != nil || !ok {
		t.Fatal(ok, err)
	}
	unroutedSweep(t, g, 10*plugins.UnroutedAskAfter)
	if asks := unroutedAsks(t, g, m.ID); len(asks) != 0 {
		t.Fatalf("the no_reply message raised %d ask(s); want none", len(asks))
	}
	if asks := unroutedAsks(t, g, control.ID); len(asks) != 1 {
		t.Fatalf("control: the repliable message raised %d ask(s); want 1", len(asks))
	}
}

// TestAnUndeliveredMessageRaisesNoAsk: a message still queued (never reached
// the chief) is not the chief's to have acted on; nor is one whose delivery
// is unconfirmed — the chief may never have seen it, and it is already on
// /plugins as a delivery for the owner to retry.
func TestAnUndeliveredMessageRaisesNoAsk(t *testing.T) {
	g := newGatedFixture(t)
	queued := g.deliveredMessage(t, g.owner, 0, "undelivered-1")
	unconfirmed := g.deliveringMessage(t, g.owner, g.chief.ID, "undelivered-unconfirmed")
	if ok, err := g.plugins.RecordDelivery(g.asOwner, unconfirmed.ID, g.chief.ID, plugins.StatusUnconfirmed); err != nil || !ok {
		t.Fatal(ok, err)
	}
	control := g.deliveredMessage(t, g.owner, g.chief.ID, "undelivered-control")
	unroutedSweep(t, g, 10*plugins.UnroutedAskAfter)
	if asks := unroutedAsks(t, g, queued.ID); len(asks) != 0 {
		t.Fatalf("a queued message raised %d ask(s); want none", len(asks))
	}
	if asks := unroutedAsks(t, g, unconfirmed.ID); len(asks) != 0 {
		t.Fatalf("an unconfirmed message raised %d ask(s); want none", len(asks))
	}
	if asks := unroutedAsks(t, g, control.ID); len(asks) != 1 {
		t.Fatalf("control: %d ask(s); want 1", len(asks))
	}
}

// TestAMessageSettledPreExistingRaisesNoAsk: what migration 0050's backfill
// leaves on a message delivered before it (settle_reason 'pre-existing') keeps
// it out of the sweep for good (the backfill itself:
// internal/db's TestMigration0050SettlesThePreExistingMessages).
func TestAMessageSettledPreExistingRaisesNoAsk(t *testing.T) {
	g := newGatedFixture(t)
	old := g.deliveredMessage(t, g.owner, g.chief.ID, "pre-existing-1")
	control := g.deliveredMessage(t, g.owner, g.chief.ID, "pre-existing-control")
	if _, err := g.pool.Exec(authz.System(context.Background()), `UPDATE plugin_messages SET settle_reason = 'pre-existing' WHERE id = $1`, old.ID); err != nil {
		t.Fatal(err)
	}
	unroutedSweep(t, g, 10*plugins.UnroutedAskAfter)
	if asks := unroutedAsks(t, g, old.ID); len(asks) != 0 {
		t.Fatalf("the pre-existing message raised %d ask(s); want none", len(asks))
	}
	if asks := unroutedAsks(t, g, control.ID); len(asks) != 1 {
		t.Fatalf("control: %d ask(s); want 1", len(asks))
	}
}

// TestTheOwnersAnswerSettlesTheMessage: answering the ask settles its message
// with the answer as the reason; the ask is answered, and no sweep raises
// another. Answering an ordinary ask settles nothing.
func TestTheOwnersAnswerSettlesTheMessage(t *testing.T) {
	g := newGatedFixture(t)
	m := g.deliveredMessage(t, g.owner, g.chief.ID, "answered-1")
	other := g.deliveredMessage(t, g.owner, g.chief.ID, "answered-other")
	unroutedSweep(t, g, plugins.UnroutedAskAfter+time.Minute)
	asks := unroutedAsks(t, g, m.ID)
	if len(asks) != 1 {
		t.Fatalf("%d asks; want 1", len(asks))
	}
	if _, err := g.srv.answerAskFrom(g.asOwner, asks[0].ID, "No action needed", attention.AnswerSource{}); err != nil {
		t.Fatal(err)
	}
	if got := settleReason(t, g, m.ID); got != "No action needed" {
		t.Fatalf("the answered message's settle reason is %q; want the answer", got)
	}
	if got := settleReason(t, g, other.ID); got != "" {
		t.Fatalf("another message was settled too: %q", got)
	}
	unroutedSweep(t, g, 3*plugins.UnroutedAskAfter)
	if asks := unroutedAsks(t, g, m.ID); len(asks) != 1 || asks[0].Status != attention.Answered {
		t.Fatalf("after the answer and a sweep: %+v; want the one ask, answered", asks)
	}
	// An ordinary ask's answer settles no message.
	plain, err := attention.NewPG(g.pool).Create(g.asOwner, attention.New{Asker: attention.Asker{System: true, Label: "muster"}, Question: "unrelated?"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.srv.answerAskFrom(g.asOwner, plain.ID, "yes", attention.AnswerSource{}); err != nil {
		t.Fatal(err)
	}
	if got := settleReason(t, g, other.ID); got != "" {
		t.Fatalf("an unrelated ask's answer settled message %d: %q", other.ID, got)
	}
}

// TestSettleIsTheChiefsAloneForItsOwnDeliveredMessages: a worker is not offered
// plugin_message_settle (and its call is refused); the chief cannot settle
// another owner's message (row-level security) nor one of its owner's that was
// never delivered to it; a blank reason is refused; a second settle keeps the
// first reason.
func TestSettleIsTheChiefsAloneForItsOwnDeliveredMessages(t *testing.T) {
	g := newGatedFixture(t)
	mine := g.deliveredMessage(t, g.owner, g.chief.ID, "settle-mine")

	if _, _, rpc := g.callTool(t, g.workerTok, pluginMessageSettleToolName, map[string]any{"message_id": mine.ID, "reason": "x"}); rpc == nil {
		t.Fatal("a worker called plugin_message_settle over /mcp")
	}
	if text := g.srv.dispatchAgentTool(g.asOwner, g.worker, pluginMessageSettleToolName, `{"message_id":1,"reason":"x"}`); !strings.Contains(text, "chief agent only") {
		t.Fatalf("the worker's gateway call: %s", text)
	}
	if got := settleReason(t, g, mine.ID); got != "" {
		t.Fatalf("a refused worker settled the message: %q", got)
	}

	otherOwner := g.newMember(t)
	otherTok := "settle-other-chief-" + itoa(time.Now().UnixNano())
	otherChief := g.newAgent(t, otherOwner, "stother-"+itoa(time.Now().UnixNano()), "chief", otherTok)
	theirs := g.deliveredMessage(t, otherOwner, otherChief.ID, "settle-theirs")
	text, isErr, rpc := g.callTool(t, g.chiefTok, pluginMessageSettleToolName, map[string]any{"message_id": theirs.ID, "reason": "not mine"})
	if rpc != nil || !isErr || !strings.Contains(text, "not a plugin message delivered to you") {
		t.Fatalf("settling another owner's message: rpc=%+v isErr=%v %s", rpc, isErr, text)
	}
	if got := settleReason(t, g, theirs.ID); got != "" {
		t.Fatalf("another owner's message was settled: %q", got)
	}

	queued := g.deliveredMessage(t, g.owner, 0, "settle-queued")
	if text, isErr, _ := g.callTool(t, g.chiefTok, pluginMessageSettleToolName, map[string]any{"message_id": queued.ID, "reason": "early"}); !isErr ||
		!strings.Contains(text, "not a plugin message delivered to you") {
		t.Fatalf("settling an undelivered message: %v %s", isErr, text)
	}
	if text, isErr, _ := g.callTool(t, g.chiefTok, pluginMessageSettleToolName, map[string]any{"message_id": mine.ID, "reason": "   "}); !isErr ||
		!strings.Contains(text, "a reason is required") {
		t.Fatalf("a blank reason: %v %s", isErr, text)
	}

	g.settle(t, mine.ID, "first reason")
	text, isErr, _ = g.callTool(t, g.chiefTok, pluginMessageSettleToolName, map[string]any{"message_id": mine.ID, "reason": "second reason"})
	if isErr || !strings.Contains(text, `"already_settled":true`) {
		t.Fatalf("a second settle: %v %s", isErr, text)
	}
	if got := settleReason(t, g, mine.ID); got != "first reason" {
		t.Fatalf("the settle reason is %q; want the first one kept", got)
	}
}
