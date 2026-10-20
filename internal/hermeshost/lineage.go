package hermeshost

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Session is one sessions row, the columns this reader uses.
type Session struct {
	ID     string
	Parent string
	Source string
	// Ended: ended_at is set; EndReason is end_reason ("" for NULL).
	Ended     bool
	EndReason string
	Archived  bool
	Hidden    bool
}

// EndReasonCompression is the end_reason of a session that continued in a
// compression child (the legacy rotation, compression.in_place: false).
const EndReasonCompression = "compression"

// ErrNoSession: state.db has no session with the id asked for.
var ErrNoSession = errors.New("state.db has no such session")

// Lineage is a conversation's sessions, root first, tip last: the session
// asked for, its compression ancestors and its compression continuations.
type Lineage []Session

// IDs is the lineage's session ids, root first.
func (l Lineage) IDs() []string {
	out := make([]string, len(l))
	for i, s := range l {
		out[i] = s.ID
	}
	return out
}

// Root is the lineage's first session: the logical id the turn lease is
// keyed by (session_turn_leases.conversation_id, S2/S4) and the rendered
// view's Gen. "" for an empty lineage.
func (l Lineage) Root() string {
	if len(l) == 0 {
		return ""
	}
	return l[0].ID
}

// Tip is the lineage's last session.
func (l Lineage) Tip() Session {
	if len(l) == 0 {
		return Session{}
	}
	return l[len(l)-1]
}

// maxLineage bounds a lineage walk (Hermes measured real lineages ~180 deep,
// P:hermes_state_compression.py, _CHAIN_CAP's comment).
const maxLineage = 1024

// jsonMarker is a NON-THROWING json_extract of a model_config marker, the
// way Hermes spells it (P:hermes_state_common.py, _sql_json_extract): invalid
// JSON reads as no marker rather than failing the query.
func jsonMarker(col, path string) string {
	return "json_extract(CASE WHEN json_valid(" + col + ") THEN " + col + " ELSE json_object() END, '" + path + "')"
}

// notAForkChild is the SQL predicate (on alias c) that a child session is a
// compression CONTINUATION and not a branch, a delegation, a reset fork or a
// tool session that happens to share its parent link
// (P:hermes_state_compression.py, _CHAIN_STEP_SQL).
//
// ⚠ Hermes's own predicate also recognises a LEGACY reset child by its end
// reason (_legacy_reset_child_sql); this one does not. A legacy reset child
// of a compression-ended parent would be read as its continuation — a view
// with the parent's transcript above it, never a different pane's.
func notAForkChild(c string) string {
	return jsonMarker(c+".model_config", "$._branched_from") + " IS NULL" +
		" AND " + jsonMarker(c+".model_config", "$._delegate_from") + " IS NULL" +
		" AND " + jsonMarker(c+".model_config", "$._reset_from") + " IS NULL" +
		" AND COALESCE(" + c + ".source, '') != 'tool'"
}

// childSQL is the one compression continuation of a session (Hermes's chain
// step, P:hermes_state_compression.py _CHAIN_STEP_SQL, its ORDER BY included).
var childSQL = `SELECT child.id FROM sessions parent JOIN sessions child ON child.parent_session_id = parent.id
WHERE parent.id = ? AND parent.end_reason = 'compression' AND ` + notAForkChild("child") + `
ORDER BY CASE WHEN child.end_reason = 'compression' THEN 0 WHEN child.ended_at IS NULL THEN 1 ELSE 2 END,
  COALESCE(child.last_activity_at, child.started_at) DESC, child.started_at DESC, child.id DESC LIMIT 1`

// parentSQL is a session's parent when the session is that parent's
// compression continuation.
var parentSQL = `SELECT parent.id FROM sessions child JOIN sessions parent ON parent.id = child.parent_session_id
WHERE child.id = ? AND parent.end_reason = 'compression' AND ` + notAForkChild("child")

const sessionSQL = `SELECT id, COALESCE(parent_session_id, ''), COALESCE(source, ''), ended_at IS NOT NULL,
COALESCE(end_reason, ''), COALESCE(archived, 0), COALESCE(hidden, 0) FROM sessions WHERE id = ?`

// session reads one sessions row.
func session(ctx context.Context, tx *sql.Tx, id string) (Session, error) {
	var s Session
	err := tx.QueryRowContext(ctx, sessionSQL, id).Scan(&s.ID, &s.Parent, &s.Source, &s.Ended, &s.EndReason, &s.Archived, &s.Hidden)
	if errors.Is(err, sql.ErrNoRows) {
		return s, fmt.Errorf("%w (%s)", ErrNoSession, id)
	}
	return s, err
}

// lineage is id's lineage, read in tx.
//
// 🔴 A PARENT LINK IS FOLLOWED ONLY WHEN THE PARENT ENDED BY COMPRESSION.
// parent_session_id is shared by compression and branching (S3): following
// every link would merge a /branch's parent transcript into the branch's view.
func lineage(ctx context.Context, tx *sql.Tx, id string) (Lineage, error) {
	first, err := session(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{first.ID: true}
	var up []Session
	cur := first.ID
	for len(up) < maxLineage {
		var p string
		if err := tx.QueryRowContext(ctx, parentSQL, cur).Scan(&p); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				break
			}
			return nil, err
		}
		if seen[p] {
			break
		}
		ps, err := session(ctx, tx, p)
		if err != nil {
			return nil, err
		}
		seen[p] = true
		up = append(up, ps)
		cur = p
	}
	out := make(Lineage, 0, len(up)+1)
	for i := len(up) - 1; i >= 0; i-- {
		out = append(out, up[i])
	}
	out = append(out, first)
	cur = first.ID
	for len(out) < maxLineage {
		var c string
		if err := tx.QueryRowContext(ctx, childSQL, cur).Scan(&c); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				break
			}
			return nil, err
		}
		if seen[c] {
			break
		}
		cs, err := session(ctx, tx, c)
		if err != nil {
			return nil, err
		}
		seen[c] = true
		out = append(out, cs)
		cur = c
	}
	return out, nil
}

// Snapshot is what one read of state.db says about a conversation: its
// lineage, the turn lease of its root, and its newest message.
type Snapshot struct {
	Lineage Lineage
	// LeaseExpires is the root's session_turn_leases.expires_at (Unix
	// seconds), 0 when it holds none.
	LeaseExpires float64
	// NewestMessage is the newest messages.timestamp in the lineage (Unix
	// seconds), 0 for none.
	NewestMessage float64
}

// LeaseLive: a turn lease on the root that has not expired at now (Unix
// seconds). A live lease means a turn is in flight, possibly waiting on a
// person (it is held through approval and clarify waits, S4) — never
// "working".
func (s Snapshot) LeaseLive(now float64) bool { return s.LeaseExpires > now }

// Snapshot reads id's lineage, its root's lease and its newest message in ONE
// read transaction.
func (s *State) Snapshot(ctx context.Context, id string) (Snapshot, error) {
	var snap Snapshot
	err := s.read(ctx, func(tx *sql.Tx) error {
		l, err := lineage(ctx, tx, id)
		if err != nil {
			return err
		}
		snap = Snapshot{Lineage: l}
		var exp sql.NullFloat64
		err = tx.QueryRowContext(ctx, "SELECT expires_at FROM session_turn_leases WHERE conversation_id = ?", l.Root()).Scan(&exp)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		snap.LeaseExpires = exp.Float64
		ids := l.IDs()
		var newest sql.NullFloat64
		if err := tx.QueryRowContext(ctx, "SELECT MAX(timestamp) FROM messages WHERE session_id IN ("+placeholders(len(ids))+")",
			anys(ids)...).Scan(&newest); err != nil {
			return err
		}
		snap.NewestMessage = newest.Float64
		return nil
	})
	return snap, err
}
