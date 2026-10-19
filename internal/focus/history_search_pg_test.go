package focus

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/dbtest"
)

// HISTORY SEARCH IN A TIME WINDOW (history_search.go) against a real
// Postgres. Every fixture is synthetic: node "node-7k", a "cache quorum".

// backdate moves the history rows of sub titled title back by d.
func backdate(t *testing.T, sub Subject, title string, d time.Duration) {
	t.Helper()
	dbtest.OwnerExec(t, dbtest.DSN(t), fmt.Sprintf(`UPDATE agent_focus_history SET created_at = now() - interval '%d seconds'
		WHERE subject_kind = '%s' AND subject_key = '%s' AND title = '%s'`, int(d/time.Second), sub.Kind, sub.Key, title))
}

const replayText = "Re: node-7k upgrade. Please drain node-7k before the reboot and keep the cache quorum above two."

// replayFixture is the shape of the miss: session A wrote a focus naming the
// node-7k drain four hours ago and has since moved to another subtask; B and C
// are on unrelated work. It returns the three subjects.
func replayFixture(t *testing.T, s *PGStore) (a, b, c Subject) {
	t.Helper()
	pool := s.pool
	a = AgentSubject(mustAgent(t, pool, 2, "focus-hs-a").ID)
	b = AgentSubject(mustAgent(t, pool, 2, "focus-hs-b").ID)
	c = AgentSubject(mustAgent(t, pool, 2, "focus-hs-c").ID)
	write(t, s, 2, a, "Upgrading node-7k: drain and reboot", "Cordoned the node; draining workloads next.", "")
	backdate(t, a, "Upgrading node-7k: drain and reboot", 4*time.Hour)
	write(t, s, 2, a, "Auditing open pull requests", "Reviewing three PRs for the release.", "")
	write(t, s, 2, b, "Writing the release notes", "Summarising the changelog.", "")
	write(t, s, 2, c, "Refactoring the billing export", "Splitting the CSV writer.", "")
	return a, b, c
}

func subjectsOf(ms []Match) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.Subject.String())
	}
	return out
}

// 🔴 TestTheReplayMatchesTheSessionOnTheJobAndOnlyIt: the current focus does
// not name node-7k, so a current-focus search finds nothing; the 6 h history
// does, so the history search and the message match find A and only A.
func TestTheReplayMatchesTheSessionOnTheJobAndOnlyIt(t *testing.T) {
	s, _ := pgStore(t)
	a, b, c := replayFixture(t, s)
	live := Live{Agents: []int64{a.AgentID(), b.AgentID(), c.AgentID()}}
	since := time.Now().Add(-HistoryWindow)

	if hits, err := s.Search(as(2), "node-7k", nil, 5); err != nil || len(hits) != 0 {
		t.Fatalf("current-focus search: %+v %v; want nothing (A's focus moved on)", hits, err)
	}
	hits, err := s.SearchHistory(as(2), "node-7k", live, since, 5)
	if err != nil || len(hits) != 1 || hits[0].Subject != a || hits[0].Title != "Upgrading node-7k: drain and reboot" {
		t.Fatalf("history search: %+v %v; want A's older focus", hits, err)
	}
	ms, err := s.MatchHistory(as(2), replayText, live, since, 2, 5)
	if err != nil || fmt.Sprint(subjectsOf(ms)) != fmt.Sprint([]string{a.String()}) {
		t.Fatalf("match: %v %v; want [%s]", subjectsOf(ms), err, a)
	}
	if len(ms[0].Titles) != 1 || ms[0].Titles[0].Title != "Upgrading node-7k: drain and reboot" || ms[0].Terms < 3 {
		t.Fatalf("A's match: %+v; want its node-7k title, sharing node-7k, node, 7k, drain…", ms[0])
	}
	// Another owner reads none of it through any of the three (row-level
	// security), even naming the owner's live session ids. ⚠ Invariant
	// guards: the queries run under the caller's principal and the table's
	// policy does the rest; each is shown sensitive by running the read under
	// the system scope instead (the mutation sweep).
	if ms, err := s.MatchHistory(as(3), replayText, live, since, 1, 5); err != nil || len(ms) != 0 {
		t.Fatalf("another owner's match: %v %v", subjectsOf(ms), err)
	}
	if hits, err := s.SearchHistory(as(3), "node-7k", live, since, 5); err != nil || len(hits) != 0 {
		t.Fatalf("another owner's history search: %+v %v", hits, err)
	}
	if titles, err := s.RecentTitles(as(3), live, since); err != nil || len(titles) != 0 {
		t.Fatalf("another owner's recent titles: %+v %v", titles, err)
	}
	// Control: the owner reads its titles through the same Live.
	if titles, _ := s.RecentTitles(as(2), live, since); len(titles[a]) != 2 {
		t.Fatalf("control: the owner's titles for A: %+v", titles[a])
	}
}

// 🔴 TestTheWindowIsTime: a row older than the window is not matched (7 h
// against 6 h), while the same row inside it is (control).
func TestTheWindowIsTime(t *testing.T) {
	s, pool := pgStore(t)
	d := AgentSubject(mustAgent(t, pool, 2, "focus-hs-old").ID)
	write(t, s, 2, d, "Upgrading node-7k: drain and reboot", "", "")
	live := Live{Agents: []int64{d.AgentID()}}
	backdate(t, d, "Upgrading node-7k: drain and reboot", 5*time.Hour)
	if ms, _ := s.MatchHistory(as(2), replayText, live, time.Now().Add(-HistoryWindow), 2, 5); len(ms) != 1 {
		t.Fatalf("control: a 5 h old row is not matched: %v", subjectsOf(ms))
	}
	backdate(t, d, "Upgrading node-7k: drain and reboot", 7*time.Hour)
	for name, got := range map[string]int{
		"match":  len(must(s.MatchHistory(as(2), replayText, live, time.Now().Add(-HistoryWindow), 2, 5))),
		"search": len(must(s.SearchHistory(as(2), "node-7k", live, time.Now().Add(-HistoryWindow), 5))),
		"titles": len(must(s.RecentTitles(as(2), live, time.Now().Add(-HistoryWindow)))),
	} {
		if got != 0 {
			t.Errorf("%s: a 7 h old row was read through a 6 h window (%d)", name, got)
		}
	}
}

// 🔴 TestTheWindowIsNotARowCount: forty newer rows inside the window (a busy
// session) do not push its older node-7k row out of reach.
func TestTheWindowIsNotARowCount(t *testing.T) {
	s, _ := pgStore(t)
	a, _, _ := replayFixture(t, s)
	for i := 0; i < 40; i++ {
		write(t, s, 2, a, fmt.Sprintf("Reviewing pull request %d", i), fmt.Sprintf("Round %d of comments.", i), "")
	}
	live := Live{Agents: []int64{a.AgentID()}}
	since := time.Now().Add(-HistoryWindow)
	if ms, err := s.MatchHistory(as(2), replayText, live, since, 2, 5); err != nil || len(ms) != 1 || ms[0].Subject != a {
		t.Fatalf("match after 40 newer rows: %v %v", subjectsOf(ms), err)
	}
	if hits, err := s.SearchHistory(as(2), "node-7k", live, since, 5); err != nil || len(hits) != 1 {
		t.Fatalf("search after 40 newer rows: %+v %v", hits, err)
	}
	titles, err := s.RecentTitles(as(2), live, since)
	if err != nil || len(titles[a]) != MaxRecentTitles {
		t.Fatalf("recent titles: %d (%v); want the display bound %d", len(titles[a]), err, MaxRecentTitles)
	}
}

// 🔴 TestOnlyLiveSessionsAreRead: a mirrored session's node-7k row is read
// only while its session is in Live, and an agent's only while its id is —
// decided before the limit, so dead sessions cannot crowd a live one out.
func TestOnlyLiveSessionsAreRead(t *testing.T) {
	s, pool := pgStore(t)
	since := time.Now().Add(-HistoryWindow)
	for i := 0; i < MaxSearchResults+2; i++ {
		write(t, s, 2, MirrorSubject(fmt.Sprintf("0b2c3d4e-dead-%02d", i)), "Upgrading node-7k node-7k: drain drain", "", "")
	}
	liveSub := MirrorSubject("0b2c3d4e-live")
	write(t, s, 2, liveSub, "Errands", "and a node-7k drain note", "")
	agent := AgentSubject(mustAgent(t, pool, 2, "focus-hs-live").ID)
	write(t, s, 2, agent, "Upgrading node-7k: drain and reboot", "", "")

	live := Live{Mirrors: []string{liveSub.Key}}
	ms, err := s.MatchHistory(as(2), replayText, live, since, 2, 3)
	if err != nil || fmt.Sprint(subjectsOf(ms)) != fmt.Sprint([]string{liveSub.String()}) {
		t.Fatalf("match: %v %v; want only the live mirror (the agent is not in Live)", subjectsOf(ms), err)
	}
	hits, err := s.SearchHistory(as(2), "node-7k", live, since, 3)
	if err != nil || len(hits) != 1 || hits[0].Subject != liveSub {
		t.Fatalf("search: %+v %v", hits, err)
	}
	live.Agents = []int64{agent.AgentID()}
	if ms, _ := s.MatchHistory(as(2), replayText, live, since, 2, 5); len(ms) != 2 {
		t.Fatalf("control: with the agent live, both are matched: %v", subjectsOf(ms))
	}
	if ms, _ := s.MatchHistory(as(2), replayText, Live{}, since, 1, 5); len(ms) != 0 {
		t.Fatalf("no live session: %v", subjectsOf(ms))
	}
}

// TestOneSharedWordIsNotAMatch: a session sharing a single term with the
// message — even a distinctive one, "node-7k" — is left out at minTerms 2, and
// found at 1 (control). (It read "the 7k spare parts" while "node-7k" counted
// as three words, node-7k, node and 7k; an identifier is now ONE term, matched
// whole, and its parts are not counted again.)
func TestOneSharedWordIsNotAMatch(t *testing.T) {
	s, pool := pgStore(t)
	w := AgentSubject(mustAgent(t, pool, 2, "focus-hs-weak").ID)
	write(t, s, 2, w, "Labelling the node-7k spare parts", "", "")
	live := Live{Agents: []int64{w.AgentID()}}
	since := time.Now().Add(-HistoryWindow)
	if ms, _ := s.MatchHistory(as(2), replayText, live, since, 2, 5); len(ms) != 0 {
		t.Fatalf("one shared word matched at minTerms 2: %+v", ms)
	}
	if ms, _ := s.MatchHistory(as(2), replayText, live, since, 1, 5); len(ms) != 1 || ms[0].Terms != 1 {
		t.Fatalf("control at minTerms 1: %+v", ms)
	}
}

// TestRecentTitlesAreDistinctNewestFirst: a title written twice is listed once,
// at its newest; order is newest first.
func TestRecentTitlesAreDistinctNewestFirst(t *testing.T) {
	s, pool := pgStore(t)
	a := AgentSubject(mustAgent(t, pool, 2, "focus-hs-titles").ID)
	write(t, s, 2, a, "Porting the importer", "one", "")
	backdate(t, a, "Porting the importer", 3*time.Hour)
	write(t, s, 2, a, "Reviewing the PR", "two", "")
	backdate(t, a, "Reviewing the PR", 2*time.Hour)
	write(t, s, 2, a, "Porting the importer", "three", "")
	got, err := s.RecentTitles(as(2), Live{Agents: []int64{a.AgentID()}}, time.Now().Add(-HistoryWindow))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, ti := range got[a] {
		names = append(names, ti.Title)
	}
	if strings.Join(names, "|") != "Porting the importer|Reviewing the PR" {
		t.Fatalf("titles %q", names)
	}
}

// TestAHistoryReadNeedsAWindow: a zero since is refused, not read unbounded.
func TestAHistoryReadNeedsAWindow(t *testing.T) {
	s, _ := pgStore(t)
	if _, err := s.SearchHistory(as(2), "x", Live{}, time.Time{}, 5); err == nil {
		t.Error("search with no window")
	}
	if _, err := s.MatchHistory(as(2), "x y", Live{}, time.Time{}, 1, 5); err == nil {
		t.Error("match with no window")
	}
	if _, err := s.RecentTitles(as(2), Live{}, time.Time{}); err == nil {
		t.Error("titles with no window")
	}
}

// 🔴 TestGenericOverlapNamesNothing: three sessions on ordinary work (a PR
// review, an invoice export, release planning) and messages made of common
// words — an ordinary mail, a newsletter, a two-word nudge — share several
// lexemes with the sessions, yet name none: a session is named only when it
// shares a DISTINCTIVE term (an identifier, terms.go). The
// two-word nudge is the case a "term in at most one candidate's history" rule
// gets wrong: "review" and "update" are in ONE session's history here, so that
// rule would call them distinctive and name it.
func TestGenericOverlapNamesNothing(t *testing.T) {
	s, pool := pgStore(t)
	p := AgentSubject(mustAgent(t, pool, 2, "focus-hs-pr").ID)
	i := AgentSubject(mustAgent(t, pool, 2, "focus-hs-inv").ID)
	r := AgentSubject(mustAgent(t, pool, 2, "focus-hs-rel").ID)
	write(t, s, 2, p, "Reviewing open pull requests", "Next round of review comments; update the summary.", "")
	write(t, s, 2, i, "Exporting the March invoices", "Splitting the CSV writer next week.", "")
	write(t, s, 2, r, "Planning the next release", "Weekly checklist for the release meeting.", "")
	live := Live{Agents: []int64{p.AgentID(), i.AgentID(), r.AgentID()}}
	since := time.Now().Add(-HistoryWindow)
	for name, text := range map[string]string{
		"ordinary mail": "Quick update on our meeting next week, please review the agenda and send your notes before the release.",
		"newsletter": "This week's newsletter: product updates, the next release, a review of what is new, " +
			"and the invoices feature. Thanks for reading, see you next week.",
		"two-word nudge": "Please review the update.",
	} {
		if ms, err := s.MatchHistory(as(2), text, live, since, 2, 5); err != nil || len(ms) != 0 {
			t.Errorf("%s named %v (%v); generic overlap must name nothing", name, subjectsOf(ms), err)
		}
	}
	// Control: a message naming an identifier one session carries names it.
	write(t, s, 2, p, "Reviewing pull request 4711", "Next round of review comments.", "")
	if ms, _ := s.MatchHistory(as(2), "Please review pull request 4711 before Friday.", live, since, 2, 5); fmt.Sprint(subjectsOf(ms)) != fmt.Sprint([]string{p.String()}) {
		t.Fatalf("control: an identifier match named %v; want [%s]", subjectsOf(ms), p)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// 🔴 TestJunkCannotSteerTheHint (#139 follow-up 7): a sender could name
// sessions with junk. Bare numbers 0–999 plus "please review" named a session
// whose focus said "PR 137" (the tsvector lexeme `137` held a digit, so it was
// "distinctive"); hyphenated common words named a session that also wrote them;
// and a message stuffed with references could carry one for every session.
// Now: a bare number is not distinctive, a hyphenated word made only of
// dictionary words is not, and a message contributes only its first
// MaxDistinctiveTerms. Controls: "#137" still names the PR-137 session (one
// reference, any spelling), and the stuffed message still names a session
// whose reference is among its first twenty.
func TestJunkCannotSteerTheHint(t *testing.T) {
	s, pool := pgStore(t)
	pr := AgentSubject(mustAgent(t, pool, 2, "focus-hs-pr137").ID)
	fu := AgentSubject(mustAgent(t, pool, 2, "focus-hs-followup").ID)
	early := AgentSubject(mustAgent(t, pool, 2, "focus-hs-pr20").ID)
	late := AgentSubject(mustAgent(t, pool, 2, "focus-hs-pr30").ID)
	write(t, s, 2, pr, "Reviewing PR 137 before the release", "Please review the notes and the changelog.", "")
	write(t, s, 2, fu, "Drafting the follow-up email", "Testing the sign-up flow next; please review the copy.", "")
	write(t, s, 2, early, "Reviewing PR 20", "Please review the second round.", "")
	write(t, s, 2, late, "Reviewing PR 30", "Please review the third round.", "")
	live := Live{Agents: []int64{pr.AgentID(), fu.AgentID(), early.AgentID(), late.AgentID()}}
	since := time.Now().Add(-HistoryWindow)
	match := func(text string) []string {
		t.Helper()
		ms, err := s.MatchHistory(as(2), text, live, since, 2, MaxSearchResults)
		if err != nil {
			t.Fatal(err)
		}
		return subjectsOf(ms)
	}

	var numbers strings.Builder
	for i := 0; i <= 999; i++ {
		fmt.Fprintf(&numbers, "%d ", i)
	}
	if got := match(numbers.String() + "please review"); len(got) != 0 {
		t.Errorf("0–999 plus \"please review\" named %v; bare numbers must name nothing", got)
	}
	if got := match("Quick follow-up on the sign-up form"); len(got) != 0 {
		t.Errorf("\"Quick follow-up on the sign-up form\" named %v; hyphenated dictionary words must name nothing", got)
	}
	var refs strings.Builder
	for i := 1; i <= 40; i++ {
		fmt.Fprintf(&refs, "#%d ", i)
	}
	stuffed := match(refs.String() + "please review the round")
	if fmt.Sprint(stuffed) != fmt.Sprint([]string{early.String()}) {
		t.Errorf("a message stuffed with #1…#40 named %v; want only [%s] (PR 20 is among its first twenty, PR 30 is not)", stuffed, early)
	}
	// Control: one reference, in another spelling, still names its session.
	if got := match("Any news on #137? The release is waiting on it."); fmt.Sprint(got) != fmt.Sprint([]string{pr.String()}) {
		t.Errorf("control: \"#137\" named %v; want [%s]", got, pr)
	}
}

// 🔴 TestRealIdentifiersGetAHint (#139 follow-up 8): identifiers people do
// write got no hint — snake_case and camelCase names and file names (hints.go
// has no underscore, so the snake rule cannot stand in for the file rule) hold
// neither a digit nor a hyphen, and "PR-137" became the lexemes `pr` and
// `-137`, which a focus saying "PR 137" (`pr`, `137`) does not share. Each
// message names exactly the session on that identifier.
func TestRealIdentifiersGetAHint(t *testing.T) {
	s, pool := pgStore(t)
	snake := AgentSubject(mustAgent(t, pool, 2, "focus-hs-snake").ID)
	camel := AgentSubject(mustAgent(t, pool, 2, "focus-hs-camel").ID)
	file := AgentSubject(mustAgent(t, pool, 2, "focus-hs-file").ID)
	pr := AgentSubject(mustAgent(t, pool, 2, "focus-hs-prdash").ID)
	write(t, s, 2, snake, "Fixing the match_history ranking", "Tie-breaks are next.", "")
	write(t, s, 2, camel, "Refactoring MatchRows in the store", "Splitting the scan loop.", "")
	write(t, s, 2, file, "Editing hints.go for the routing line", "Adding the cap.", "")
	write(t, s, 2, pr, "Reviewing PR 137 for the release", "Two comments left.", "")
	live := Live{Agents: []int64{snake.AgentID(), camel.AgentID(), file.AgentID(), pr.AgentID()}}
	since := time.Now().Add(-HistoryWindow)
	for _, c := range []struct {
		text string
		want Subject
	}{
		{"Is match_history done? The ranking looked off.", snake},
		{"Did the matchRows refactor land in the store?", camel},
		{"Please look at hints.go before the routing change ships.", file},
		{"Status of PR-137? The release depends on it.", pr},
	} {
		ms, err := s.MatchHistory(as(2), c.text, live, since, 2, 5)
		if err != nil || fmt.Sprint(subjectsOf(ms)) != fmt.Sprint([]string{c.want.String()}) {
			t.Errorf("%q named %v (%v); want [%s]", c.text, subjectsOf(ms), err, c.want)
		}
	}
}
