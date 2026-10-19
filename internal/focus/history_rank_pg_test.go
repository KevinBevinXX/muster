package focus

import (
	"fmt"
	"testing"
	"time"
)

// 🔴 TestMoreIdentifiersOutrankMoreWords (#139 follow-up 9): MatchHistory's
// order is distinctive terms first, then all terms, then the newest. X shares
// TWO identifiers (node-7k, PR 4711) and one word; Y shares one identifier and
// seven words; Z one identifier and one word, written AFTER Y (so a rank that
// skipped the term count would put Z above Y). X must outrank Y although Y
// shares more terms, and limit 1 must keep X. Two sessions identical but for
// when they wrote rank newest first.
func TestMoreIdentifiersOutrankMoreWords(t *testing.T) {
	s, pool := pgStore(t)
	x := AgentSubject(mustAgent(t, pool, 2, "focus-hs-rank-x").ID)
	y := AgentSubject(mustAgent(t, pool, 2, "focus-hs-rank-y").ID)
	z := AgentSubject(mustAgent(t, pool, 2, "focus-hs-rank-z").ID)
	write(t, s, 2, x, "Draining node-7k for PR 4711", "Cordoned.", "")
	write(t, s, 2, y, "Upgrading node-7k: drain and reboot", "Keep the cache quorum above two after the upgrade.", "")
	backdate(t, y, "Upgrading node-7k: drain and reboot", time.Hour)
	write(t, s, 2, z, "Labelling the node-7k spare parts", "Before the drain.", "")
	live := Live{Agents: []int64{x.AgentID(), y.AgentID(), z.AgentID()}}
	since := time.Now().Add(-HistoryWindow)
	const text = "Re: node-7k upgrade and PR 4711. Please drain node-7k before the reboot and keep the cache quorum above two."

	ms, err := s.MatchHistory(as(2), text, live, since, 2, 5)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(subjectsOf(ms)) != fmt.Sprint([]string{x.String(), y.String(), z.String()}) {
		t.Fatalf("order %v; want [%s %s %s] (X: 2 identifiers; Y: 1 and more words; Z: 1 and fewer, though newer)", subjectsOf(ms), x, y, z)
	}
	if !(ms[0].Terms < ms[1].Terms && ms[2].Terms < ms[1].Terms) {
		t.Fatalf("CONTROL: the fixture must give Y the most terms (terms %d, %d, %d)", ms[0].Terms, ms[1].Terms, ms[2].Terms)
	}
	if top, _ := s.MatchHistory(as(2), text, live, since, 2, 1); fmt.Sprint(subjectsOf(top)) != fmt.Sprint([]string{x.String()}) {
		t.Fatalf("limit 1 kept %v; want [%s]", subjectsOf(top), x)
	}

	older := AgentSubject(mustAgent(t, pool, 2, "focus-hs-rank-old").ID)
	newer := AgentSubject(mustAgent(t, pool, 2, "focus-hs-rank-new").ID)
	write(t, s, 2, older, "Draining node-9q", "Cordoned.", "")
	backdate(t, older, "Draining node-9q", 2*time.Hour)
	write(t, s, 2, newer, "Draining node-9q", "Cordoned.", "")
	pair := Live{Agents: []int64{older.AgentID(), newer.AgentID()}}
	got, _ := s.MatchHistory(as(2), "Drain node-9q now.", pair, since, 2, 5)
	if fmt.Sprint(subjectsOf(got)) != fmt.Sprint([]string{newer.String(), older.String()}) {
		t.Fatalf("tie: %v; want the newer first [%s %s]", subjectsOf(got), newer, older)
	}
}
