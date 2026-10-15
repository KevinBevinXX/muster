package api

import (
	"encoding/json"
	"testing"

	"github.com/ZacxDev/muster/internal/plugins"
)

// TestTheChiefDraftsAReplyOnTheTurnThatDeliversTheMessage is finding 9 of audit
// round 1, through the real plane (newPluginPlane: real Postgres stores, the real
// deliverer and chatTurn, the real agentgateway.Gateway, a fake runtime): the
// runtime's FIRST response on the delivery turn is a reply_draft call for the
// message being delivered — the chief answering on the turn that tells it about
// the message. The call goes through the real ChatWithTools → dispatch path, and a
// pending draft row for that message must be filed. Before the fix the message
// was `queued` with no agent during the turn, and the draft was refused.
func TestTheChiefDraftsAReplyOnTheTurnThatDeliversTheMessage(t *testing.T) {
	rt := &scriptedRuntime{name: replyDraftToolName}
	pp := newPluginPlane(t, rt)
	m, _, err := pp.ps.Submit(pp.owner, pp.plugin.ID, plugins.MessageIn{ExternalID: "seam-reply-1", Body: "Are you free at noon?"})
	if err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{"message_id": m.ID, "body": "Yes, noon works."})
	rt.mu.Lock()
	rt.args = string(args)
	rt.mu.Unlock()

	if r := pp.srv.pluginDeliverer.Deliver(pp.owner, pp.plugin.ID, m.ID, pp.chief); r.Outcome != plugins.OutcomeDelivered {
		t.Fatalf("delivery: %+v; want delivered", r)
	}
	sent := rt.seen()
	if len(sent) != 2 {
		t.Fatalf("the runtime saw %d request(s); want 2 (the call, then its output)", len(sent))
	}
	cards, err := pp.ps.DraftCards(pp.owner, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 1 {
		t.Fatalf("%d draft(s) after the chief's reply_draft call; want 1 — the tool answered: %s", len(cards), sent[1])
	}
	d := cards[0].Draft
	if d.MessageID != m.ID || d.Body != "Yes, noon works." || d.Status != plugins.DraftPending ||
		d.AgentID == nil || *d.AgentID != pp.chief.ID {
		t.Fatalf("the filed draft %+v; want a pending reply to message %d by the chief", d, m.ID)
	}
	parts, _ := pp.as.ListChatMessages(pp.owner, *mustPlugin(t, pp).ChiefSessionID)
	ok := false
	for _, part := range parts {
		if part.Kind == "tool_result" && part.ToolName == replyDraftToolName && part.ToolOK {
			ok = true
		}
	}
	if !ok {
		t.Fatal("the session stores no successful reply_draft result")
	}
}
