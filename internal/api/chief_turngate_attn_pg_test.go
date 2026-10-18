package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
)

// THE SEAM BETWEEN THE CHIEF REPLACEMENT'S TURN GATE AND #134's
// ANSWERED-IN-SESSION HOOK, on both of the operator's doors (the POST send and
// the chat socket): a message the closed gate refuses starts no turn, stores
// nothing and answers NONE of the agent's asks; once the gate is open again the
// same message answers them, as #134 specifies.

// TestAMessageTheTurnGateRefusesAnswersNoAsk_Post: the POST send.
func TestAMessageTheTurnGateRefusesAnswersNoAsk_Post(t *testing.T) {
	f := newAttnFixture(t, func(ext *Extensions, _ *pgxpool.Pool) { ext.Gateway = &turnGateway{} })
	id := f.ask(t, f.opTok, "Which branch, gated?")
	if !f.srv.turns.close(f.op.ID) {
		t.Fatal("control: could not close an idle gate")
	}
	rd, hdr := form(url.Values{"text": {"use main"}, "nonce": {"gated-nonce-000000001"}})
	w := f.call(t, http.MethodPost, "/agents/"+f.op.Name+"/send", rd, merge(f.operator(), hdr))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), ErrAgentBeingReplaced.Error()) {
		t.Fatalf("a send while the agent is being replaced: %d %s; want 409", w.Code, w.Body)
	}
	time.Sleep(100 * time.Millisecond) // the answered-in-session write is synchronous; this only catches a stray goroutine
	if st := f.askStatus(t, id); st.Status != attention.Open {
		t.Fatalf("a refused message answered the agent's ask: %s", st.Status)
	}
	// Reopened, the SAME nonce is accepted (finish(false) released it) and answers it.
	f.srv.turns.reopen(f.op.ID)
	rd, hdr = form(url.Values{"text": {"use main"}, "nonce": {"gated-nonce-000000001"}})
	if w := f.call(t, http.MethodPost, "/agents/"+f.op.Name+"/send", rd, merge(f.operator(), hdr)); w.Code != http.StatusNoContent {
		t.Fatalf("the send once reopened: %d %s", w.Code, w.Body)
	}
	if st := f.askStatus(t, id); st.Status != attention.AnsweredInSession {
		t.Fatalf("control: the admitted message did not answer the ask: %s", st.Status)
	}
	// The admitted turn LEAVES the gate when it ends, or the chief could never be
	// replaced again.
	eventuallyAPI(t, "the posted turn to leave the gate", func() bool {
		if f.srv.turns.close(f.op.ID) {
			f.srv.turns.reopen(f.op.ID)
			return true
		}
		return false
	})
}

// TestAMessageTheTurnGateRefusesAnswersNoAsk_Socket: the chat socket.
func TestAMessageTheTurnGateRefusesAnswersNoAsk_Socket(t *testing.T) {
	f := newAttnFixture(t, func(ext *Extensions, _ *pgxpool.Pool) { ext.Gateway = &turnGateway{} })
	id := f.ask(t, f.opTok, "Which region, gated?")
	ts := httptest.NewServer(f.h)
	t.Cleanup(ts.Close)
	h := http.Header{}
	h.Set("Cookie", sessionCookieFor(f.srv))
	h.Set("Origin", ts.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/agents/"+f.op.Name+"/ws", &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	send := func(text string) map[string]string {
		raw, _ := json.Marshal(map[string]string{"type": "message", "text": text})
		if err := c.Write(ctx, websocket.MessageText, raw); err != nil {
			t.Fatal(err)
		}
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]string
			_ = json.Unmarshal(data, &m)
			if m["type"] == "error" || m["type"] == "done" {
				return m
			}
		}
	}
	if !f.srv.turns.close(f.op.ID) {
		t.Fatal("control: could not close an idle gate")
	}
	if m := send("eu please"); m["type"] != "error" || m["content"] != ErrAgentBeingReplaced.Error() {
		t.Fatalf("a socket message while the agent is being replaced: %v", m)
	}
	if st := f.askStatus(t, id); st.Status != attention.Open {
		t.Fatalf("a refused socket message answered the agent's ask: %s", st.Status)
	}
	sessions, _ := agents.NewPG(f.pool).ListSessions(asUser(1), f.op.ID)
	for _, s := range sessions {
		if msgs, _ := agents.NewPG(f.pool).ListChatMessages(asUser(1), s.ID); len(msgs) != 0 {
			t.Fatalf("the refused socket message was stored: %d message(s)", len(msgs))
		}
	}
	f.srv.turns.reopen(f.op.ID)
	if m := send("eu please"); m["type"] != "done" {
		t.Fatalf("control: the admitted socket message: %v", m)
	}
	if st := f.askStatus(t, id); st.Status != attention.AnsweredInSession {
		t.Fatalf("control: the admitted socket message did not answer the ask: %s", st.Status)
	}
	if !f.srv.turns.close(f.op.ID) {
		t.Fatal("the socket's finished turn did not leave the gate")
	}
	f.srv.turns.reopen(f.op.ID)
}
