package attention

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/authz"
)

// Human-only approvals (migration 0045).

const hoPayload = "[dispatch md-0a1b2c]\n\nSummarise the reading list.\n"

func humanOnly(payload string) New {
	return New{Asker: Asker{System: true, Label: "api token (dispatch)"}, Question: "Approve this muse dispatch?",
		HumanOnly: true, Payload: payload}
}

// TestAHumanOnlyAskCarriesItsPayloadDigestAndTheTwoOptions: the server
// computes the digest (the client only supplies the text), and the options are
// forced, whatever the caller passed.
func TestAHumanOnlyAskCarriesItsPayloadDigestAndTheTwoOptions(t *testing.T) {
	s, _, _ := pgStore(t)
	n := humanOnly(hoPayload)
	n.Options = []string{"yes please"}
	a := mustAsk(t, s, 2, n)
	if !a.HumanOnly || a.Payload != hoPayload || a.PayloadSHA256 != PayloadSHA256(hoPayload) || len(a.PayloadSHA256) != 64 {
		t.Fatalf("ask = %+v", a)
	}
	if strings.Join(a.Options, "|") != ApproveAnswer+"|"+DenyAnswer {
		t.Fatalf("options = %v; want exactly Approve|Deny", a.Options)
	}
	// Refused shapes: no payload; a non-system asker; a payload on a normal ask.
	for name, bad := range map[string]New{
		"no payload":         humanOnly("  "),
		"an agent asker":     {Asker: Asker{AgentID: 1}, Question: "q", HumanOnly: true, Payload: "p"},
		"payload, not human": {Asker: Asker{System: true}, Question: "q", Payload: "p"},
	} {
		if _, err := s.Create(as(2), bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Create = %v; want ErrInvalid", name, err)
		}
	}
}

// TestAnMCPAnswerIsRefusedOnAHumanOnlyAsk: attention_answer (a mirrored
// session, ViaMCP) cannot answer a human-only ask — from any host — and the
// ask stays open; the view (Answer) can. CONTROL: the same MCP answer on an
// ordinary system ask lands, so the refusal is about human_only.
func TestAnMCPAnswerIsRefusedOnAHumanOnlyAsk(t *testing.T) {
	s, _, _ := pgStore(t)
	ho := mustAsk(t, s, 2, humanOnly(hoPayload))
	if _, err := s.AnswerFromSession(as(2), ho.ID, ApproveAnswer, "otherhost", session.MirrorSession); !errors.Is(err, ErrHumanOnly) {
		t.Fatalf("an MCP answer on a human-only ask = %v; want ErrHumanOnly", err)
	}
	if got, _ := s.Get(as(2), ho.ID); got.Status != Open {
		t.Fatalf("after the refused MCP answer the ask is %s; want open", got.Status)
	}
	plain := mustAsk(t, s, 2, New{Asker: Asker{System: true}, Question: "an ordinary question"})
	if a, err := s.AnswerFromSession(as(2), plain.ID, "yes", "otherhost", session.MirrorSession); err != nil || a.Status != Answered {
		t.Fatalf("CONTROL: an MCP answer on an ordinary ask = %+v %v; want answered", a, err)
	}
	a, err := s.Answer(as(2), ho.ID, ApproveAnswer)
	if err != nil || !a.Approved() || a.AnsweredVia.Via != "" {
		t.Fatalf("the view's Approve = %+v %v; want approved, answered on the view", a, err)
	}
}

// TestTheCheckRefusesANonHumanAnswerOnAHumanOnlyAsk: 0045's CHECK states the
// rule again, so a store bug cannot write an MCP provenance onto one.
func TestTheCheckRefusesANonHumanAnswerOnAHumanOnlyAsk(t *testing.T) {
	s, _, dsn := pgStore(t)
	ho := mustAsk(t, s, 2, humanOnly(hoPayload))
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	_, err = conn.Exec(context.Background(), `UPDATE attention_asks SET status = 'answered', answer = 'Approve', answered_by = 'operator',
		resolved_at = now(), answered_via = 'mcp', answered_via_host = 'h', answered_via_session = 's' WHERE id = $1`, ho.ID)
	if err == nil || !strings.Contains(err.Error(), "attention_asks_human_only_check") {
		t.Fatalf("a raw MCP answer on a human-only ask = %v; want the human_only CHECK", err)
	}
}

// TestAMessageInSessionNeverSettlesAHumanOnlyAsk: AnswerInSession matches by
// asker; a human-only ask is a system ask with no session, and is excluded
// outright as well.
func TestAMessageInSessionNeverSettlesAHumanOnlyAsk(t *testing.T) {
	s, _, _ := pgStore(t)
	ho := mustAsk(t, s, 2, humanOnly(hoPayload))
	settled, err := s.AnswerInSession(as(2), Asker{System: true}, "approve it", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range settled {
		if a.ID == ho.ID {
			t.Fatalf("AnswerInSession settled the human-only ask %d", ho.ID)
		}
	}
	if got, _ := s.Get(as(2), ho.ID); got.Status != Open {
		t.Fatalf("the human-only ask is %s; want open", got.Status)
	}
}

// TestOnlyTheExactApproveAnswerApproves: Deny, a typed "approve", and an
// ordinary ask answered "Approve" are not approvals.
func TestOnlyTheExactApproveAnswerApproves(t *testing.T) {
	s, _, _ := pgStore(t)
	for _, ans := range []string{DenyAnswer, "approve", "Approve!"} {
		a := mustAsk(t, s, 2, humanOnly(hoPayload))
		got, err := s.Answer(as(2), a.ID, ans)
		if err != nil || got.Approved() {
			t.Errorf("answer %q: approved=%v %v; want not approved", ans, got.Approved(), err)
		}
	}
	plain := mustAsk(t, s, 2, New{Asker: Asker{System: true}, Question: "q"})
	if got, _ := s.Answer(as(2), plain.ID, ApproveAnswer); got.Approved() {
		t.Fatal("an ordinary ask answered Approve reads as a human-only approval")
	}
	_ = authz.OperatorUserID
}
