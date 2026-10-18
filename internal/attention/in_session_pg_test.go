package attention

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/authz"
)

// TestAnAskIsAnsweredInSessionOnlyForItsAskerAndItsOwner: AnswerInSession
// settles exactly the caller's open asks from THAT asker raised before it —
// the message, capped and trimmed, as the answer, by the operator — and
// nothing of another asker's (another session on the same host, the same
// session id on another host, an agent), another owner's (row-level
// security), an expired one, or one already settled. A blank message settles
// nothing.
func TestAnAskIsAnsweredInSessionOnlyForItsAskerAndItsOwner(t *testing.T) {
	s, pool, _ := pgStore(t)
	var agent int64
	if err := pool.QueryRow(as(2), `INSERT INTO agents (name, namespace) VALUES ($1, $1) RETURNING id`,
		"attn-insess-"+time.Now().Format("150405.000000")).Scan(&agent); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(authz.System(context.Background()), `DELETE FROM agents WHERE id = $1`, agent)
	})
	mine1 := mustAsk(t, s, 2, New{Asker: session, Question: "one?"})
	mine2 := mustAsk(t, s, 2, New{Asker: session, Question: "two?"})
	answered := mustAsk(t, s, 2, New{Asker: session, Question: "done?"})
	if _, err := s.Answer(as(2), answered.ID, "yes"); err != nil {
		t.Fatal(err)
	}
	expired := mustAsk(t, s, 2, New{Asker: session, Question: "lapsed?"})
	if _, err := pool.Exec(authz.System(context.Background()), `UPDATE attention_asks SET created_at = now() - interval '2 hours', expires_at = now() - interval '1 second' WHERE id = $1`, expired.ID); err != nil {
		t.Fatal(err)
	}
	otherSession := mustAsk(t, s, 2, New{Asker: Asker{MirrorHost: session.MirrorHost, MirrorSession: "9f8e7d6c-5b4a-4321-8fed-cba987654321"}, Question: "other session?"})
	otherHost := mustAsk(t, s, 2, New{Asker: Asker{MirrorHost: "desk", MirrorSession: session.MirrorSession}, Question: "other host?"})
	agentAsk := mustAsk(t, s, 2, New{Asker: Asker{AgentID: agent}, Question: "agent?"})
	otherOwner := mustAsk(t, s, 3, New{Asker: session, Question: "member 3's?"})
	// An ask stamped after the message's statement (raised while it was in
	// flight): the operator could not have seen it, so it is not answered.
	inFlight := mustAsk(t, s, 2, New{Asker: session, Question: "raised as you typed?"})
	if _, err := pool.Exec(authz.System(context.Background()), `UPDATE attention_asks SET created_at = now() + interval '1 minute' WHERE id = $1`, inFlight.ID); err != nil {
		t.Fatal(err)
	}

	if got, err := s.AnswerInSession(as(2), session, " \n\t ", 0); err != nil || len(got) != 0 {
		t.Fatalf("a blank message settled %d asks: %v", len(got), err)
	}
	msg := "  " + strings.Repeat("é", MaxAnswerRunes+10) + "  "
	got, err := s.AnswerInSession(as(2), session, msg, 0)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[int64]bool{}
	for _, a := range got {
		ids[a.ID] = true
		if a.Status != AnsweredInSession || a.AnsweredBy != ByOperator || a.ResolvedAt.IsZero() ||
			utf8.RuneCountInString(a.Answer) != MaxAnswerRunes || strings.TrimSpace(a.Answer) != a.Answer {
			t.Fatalf("settled %+v (answer %d runes)", a, utf8.RuneCountInString(a.Answer))
		}
	}
	if len(got) != 2 || !ids[mine1.ID] || !ids[mine2.ID] {
		t.Fatalf("settled %v, want exactly the asker's two open asks %d and %d", ids, mine1.ID, mine2.ID)
	}
	for _, c := range []struct {
		name string
		uid  int64
		id   int64
		want Status
	}{
		{"already answered", 2, answered.ID, Answered},
		{"expired", 2, expired.ID, Expired},
		{"another session", 2, otherSession.ID, Open},
		{"the same session id on another host", 2, otherHost.ID, Open},
		{"an agent", 2, agentAsk.ID, Open},
		{"another owner's", 3, otherOwner.ID, Open},
		{"raised after the message", 2, inFlight.ID, Open},
	} {
		if a, err := s.Get(as(c.uid), c.id); err != nil || a.Status != c.want {
			t.Errorf("%s: %v %s, want %s", c.name, err, a.Status, c.want)
		}
	}
	// The agent's own asker answers the agent's ask, and only that.
	if got, err := s.AnswerInSession(as(2), Asker{AgentID: agent}, "go", 0); err != nil || len(got) != 1 || got[0].ID != agentAsk.ID {
		t.Fatalf("the agent asker: %v %+v", err, got)
	}
	// An ask raised after the message is not answered by it.
	later := mustAsk(t, s, 2, New{Asker: session, Question: "later?"})
	if a, _ := s.Get(as(2), later.ID); a.Status != Open {
		t.Fatalf("an ask raised after: %s", a.Status)
	}
}
