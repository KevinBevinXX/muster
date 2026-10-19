package focus

import (
	"context"
	"strings"
	"time"
)

// SEARCHING FOCUS HISTORY IN A TIME WINDOW (migration 0022's rows): what a
// session was working on over the last few hours, not only what its current
// focus says.
//
// 🔴 WHY HISTORY, AND WHY A TIME WINDOW RATHER THAN THE NEWEST N ROWS. Focus is
// rewritten every refresh, so a session that has worked one long job for hours
// can have a CURRENT focus about some other subtask of it — a search of the
// current focus alone then misses it entirely. A count-capped tail misses it
// too, in a different way: a busy session writes many rows an hour, so its
// newest N reach back only as far as its own churn, and a quiet one's reach
// back days. Only a time window is the same span for every session. So every
// read here takes a `since`, and nothing here takes a row count per session
// until the rows are already inside the window and deduplicated.
//
// 🔴 THE LIVENESS FILTER IS IN SQL, BEFORE THE LIMIT, as Store.Search's is: a
// dead session's rows must neither be returned nor crowd a live one out of
// the result. Live names the sessions the caller may be handed — mirrored
// sessions by Claude session id, agents by id — and a row of any other
// session is never a hit.
//
// Like every Store method but the sweeps, these run under the caller's
// principal: agent_focus_history is under FORCE row-level security.

// HistoryWindow is how far back the chief's session hint and sessions_overview
// read focus history: six hours, the span that, replayed over a real miss,
// matched the session that was on the job and no other.
const HistoryWindow = 6 * time.Hour

// The bounds of a history read.
const (
	// MaxMatchTextBytes bounds the text MatchHistory matches against (an
	// inbound message can be long; its first few kilobytes name its subject).
	MaxMatchTextBytes = 8 << 10
	// MaxMatchTitles bounds the matching titles MatchHistory returns per session.
	MaxMatchTitles = 3
	// MaxRecentTitles bounds RecentTitles per session, AFTER the window and the
	// deduplication: a display bound, not a reach-back bound.
	MaxRecentTitles = 12
)

// Live is the sessions a history read may return: mirrored sessions by Claude
// session id, agents by id. A row of any other session is never returned.
type Live struct {
	Mirrors []string
	Agents  []int64
}

// TitleAt is one focus title and when it was (last) written.
type TitleAt struct {
	Title string
	At    time.Time
}

// HistoryHit is one session's best history row for a query.
type HistoryHit struct {
	Subject Subject
	Title   string
	Snippet string
	At      time.Time
	Rank    float32
}

// Match is one session whose recent history shares words with a text: how
// many distinct searchable words it shares (Terms), and its titles that share
// any, newest first.
type Match struct {
	Subject Subject
	Terms   int
	Titles  []TitleAt
}

// liveHistorySQL is the liveness predicate over agent_focus_history h:
// ($m mirrored Claude session ids, $a agent ids). It is spelled ONCE so the
// three reads cannot disagree about which sessions are live.
func liveHistorySQL(m, a string) string {
	return `((h.subject_kind = 'mirror' AND h.subject_key = ANY(` + m + `::text[]))
		OR (h.subject_kind = 'agent' AND h.agent_id = ANY(` + a + `::bigint[])))`
}

// nonNil is live with nil slices as empty ones (pgx sends nil as NULL, and
// "= ANY(NULL)" is NULL — false, but say so explicitly).
func (l Live) nonNil() ([]string, []int64) {
	m, a := l.Mirrors, l.Agents
	if m == nil {
		m = []string{}
	}
	if a == nil {
		a = []int64{}
	}
	return m, a
}

// distinctiveTermSQL is the predicate over a shared lexeme t that makes it
// DISTINCTIVE: identifier-like — it holds a digit or a hyphen ("node-7k",
// "7k", a PR or ticket number, a kebab-case name). Plain words never are.
// ⚠ A hyphenated common word ("follow-up") counts too; it must still be in
// both the message and the session's focus.
const distinctiveTermSQL = `t ~ '[0-9-]'`

// errNoWindow refuses a history read with no window: an unbounded read is not
// what any caller here means.
var errNoWindow = errf("a history read needs a since")

// SearchHistory is focus_search over history: each live session's best row
// written at or after since that matches query (websearch syntax, as Search),
// best first, at most limit (bounded by MaxSearchResults). Title weighs A and
// description B, as in agent_focus's generated column.
func (s *PGStore) SearchHistory(ctx context.Context, query string, live Live, since time.Time, limit int) ([]HistoryHit, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return []HistoryHit{}, nil
	}
	if since.IsZero() {
		return nil, errNoWindow
	}
	if limit <= 0 || limit > MaxSearchResults {
		limit = MaxSearchResults
	}
	m, a := live.nonNil()
	rows, err := s.pool.Query(ctx, `SELECT subject_kind, subject_key, title, description, created_at, rank FROM (
			SELECT DISTINCT ON (h.subject_kind, h.subject_key) h.subject_kind, h.subject_key, h.title, h.description,
				h.created_at, ts_rank(h.v, q) AS rank
			FROM (SELECT h.*, setweight(to_tsvector('english', h.title), 'A') ||
					setweight(to_tsvector('english', h.description), 'B') AS v
				FROM agent_focus_history h
				WHERE h.created_at >= $2 AND `+liveHistorySQL("$3", "$4")+`) h,
				websearch_to_tsquery('english', $1) q
			WHERE h.v @@ q
			ORDER BY h.subject_kind, h.subject_key, rank DESC, h.created_at DESC, h.id DESC) best
		ORDER BY rank DESC, created_at DESC LIMIT $5`, query, since, m, a, limit)
	if err != nil {
		return nil, errf("search history: %w", err)
	}
	defer rows.Close()
	out := []HistoryHit{}
	for rows.Next() {
		var h HistoryHit
		var kind, desc string
		if err := rows.Scan(&kind, &h.Subject.Key, &h.Title, &desc, &h.At, &h.Rank); err != nil {
			return nil, errf("search history: %w", err)
		}
		h.Subject.Kind, h.Snippet = Kind(kind), Snippet(desc)
		out = append(out, h)
	}
	return out, rows.Err()
}

// MatchHistory is the live sessions whose history written at or after since
// shares at least minTerms distinct searchable words (english lexemes, stop
// words dropped) with text — any of them, not all: text is a whole message,
// not a query — AT LEAST ONE OF THEM DISTINCTIVE (distinctiveTermSQL); most
// distinctive shared words first, then most shared words, then most recently
// written; at most limit sessions, each with up to MaxMatchTitles of its titles
// that share any word.
//
// minTerms < 1 is 1. An identifier like "node-7k" counts as three words
// ("node-7k", "node", "7k"), all distinctive, so naming one is enough.
//
// 🔴 GENERIC OVERLAP ALONE NAMES NOTHING. Measured: an ordinary mail ("quick
// update on our meeting next week, please review…") and a newsletter each
// shared two or more common words with unrelated sessions' focus — "update",
// "next", "review" — and named them. A message's sender can also add common
// words at will. So a match needs a shared identifier-like term, which a mail
// about some other subject does not carry by accident.
//
// ⚠ WHY NOT "A TERM IN AT MOST ONE CANDIDATE'S HISTORY": in a small fleet a
// common word is often in exactly one session's history ("review" in the one
// reviewing PRs), so that rule calls it distinctive and names the session for
// "please review the update" (TestGenericOverlapNamesNothing's nudge case).
func (s *PGStore) MatchHistory(ctx context.Context, text string, live Live, since time.Time, minTerms, limit int) ([]Match, error) {
	if len(text) > MaxMatchTextBytes {
		text = strings.ToValidUTF8(text[:MaxMatchTextBytes], "")
	}
	if strings.TrimSpace(text) == "" {
		return []Match{}, nil
	}
	if since.IsZero() {
		return nil, errNoWindow
	}
	if minTerms < 1 {
		minTerms = 1
	}
	if limit <= 0 || limit > MaxSearchResults {
		limit = MaxSearchResults
	}
	m, a := live.nonNil()
	rows, err := s.pool.Query(ctx, `WITH terms AS (
			SELECT DISTINCT t FROM unnest(tsvector_to_array(to_tsvector('english', $1))) t),
		rows AS (
			SELECT h.id, h.subject_kind, h.subject_key, h.title, h.created_at,
				tsvector_to_array(to_tsvector('english', h.title || ' ' || h.description)) AS lx
			FROM agent_focus_history h
			WHERE h.created_at >= $2 AND `+liveHistorySQL("$3", "$4")+`),
		hits AS (SELECT r.subject_kind, r.subject_key, r.title, r.created_at, terms.t
			FROM rows r JOIN terms ON terms.t = ANY(r.lx)),
		subj AS (SELECT subject_kind, subject_key, count(DISTINCT t) AS n,
				count(DISTINCT t) FILTER (WHERE `+distinctiveTermSQL+`) AS nd, max(created_at) AS newest
			FROM hits GROUP BY subject_kind, subject_key
			HAVING count(DISTINCT t) >= $5 AND count(DISTINCT t) FILTER (WHERE `+distinctiveTermSQL+`) >= 1
			ORDER BY nd DESC, n DESC, newest DESC, subject_kind, subject_key LIMIT $6)
		SELECT s.subject_kind, s.subject_key, s.n, x.title, x.at
		FROM subj s JOIN LATERAL (
			SELECT title, max(created_at) AS at FROM hits
			WHERE hits.subject_kind = s.subject_kind AND hits.subject_key = s.subject_key
			GROUP BY title ORDER BY at DESC LIMIT $7) x ON true
		ORDER BY s.nd DESC, s.n DESC, s.newest DESC, s.subject_kind, s.subject_key, x.at DESC`,
		text, since, m, a, minTerms, limit, MaxMatchTitles)
	if err != nil {
		return nil, errf("match history: %w", err)
	}
	defer rows.Close()
	out := []Match{}
	for rows.Next() {
		var kind, key, title string
		var n int
		var at time.Time
		if err := rows.Scan(&kind, &key, &n, &title, &at); err != nil {
			return nil, errf("match history: %w", err)
		}
		sub := Subject{Kind: Kind(kind), Key: key}
		if len(out) == 0 || out[len(out)-1].Subject != sub {
			out = append(out, Match{Subject: sub, Terms: n})
		}
		out[len(out)-1].Titles = append(out[len(out)-1].Titles, TitleAt{Title: title, At: at})
	}
	return out, rows.Err()
}

// RecentTitles is each live session's distinct focus titles written at or
// after since, newest first (a title written twice is listed once, at its
// newest), at most MaxRecentTitles per session.
func (s *PGStore) RecentTitles(ctx context.Context, live Live, since time.Time) (map[Subject][]TitleAt, error) {
	if since.IsZero() {
		return nil, errNoWindow
	}
	m, a := live.nonNil()
	rows, err := s.pool.Query(ctx, `SELECT h.subject_kind, h.subject_key, h.title, max(h.created_at) AS at
		FROM agent_focus_history h
		WHERE h.created_at >= $1 AND `+liveHistorySQL("$2", "$3")+`
		GROUP BY h.subject_kind, h.subject_key, h.title
		ORDER BY h.subject_kind, h.subject_key, at DESC`, since, m, a)
	if err != nil {
		return nil, errf("recent titles: %w", err)
	}
	defer rows.Close()
	out := map[Subject][]TitleAt{}
	for rows.Next() {
		var kind, key string
		var t TitleAt
		if err := rows.Scan(&kind, &key, &t.Title, &t.At); err != nil {
			return nil, errf("recent titles: %w", err)
		}
		sub := Subject{Kind: Kind(kind), Key: key}
		if len(out[sub]) < MaxRecentTitles {
			out[sub] = append(out[sub], t)
		}
	}
	return out, rows.Err()
}
