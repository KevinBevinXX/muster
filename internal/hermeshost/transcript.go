package hermeshost

import (
	"context"
	"database/sql"
	"slices"
	"strconv"

	"github.com/ZacxDev/muster/internal/cctranscript"
)

// THE TRANSCRIPT (docs/design/hermes.md, "Transcript reading"): a Hermes
// conversation's messages rows, read incrementally by messages.id.
//
//   - Backfill: the lineage's display rows (active=1 OR compacted=1, the
//     scrollback), copies of one message collapsed by message_uid (in-place
//     compaction re-inserts the retained context as new active rows with the
//     same uid: the live copy wins, then the newest), the last n of them.
//   - Tail: every row with id > the cursor, in id order. Only active rows are
//     drawn; every row moves the cursor.
//   - Fingerprint: COUNT(active), MAX(id) of the active rows and
//     SUM(sessions.transcript_revision) over the lineage, upstream's own
//     (P:hermes_state_messages.py, conversation_revision). A change that the
//     appended rows do not explain is a rewind, a compaction or an in-place
//     rewrite: the view starts over with a Reset backfill.
//
// Never selected: reasoning*, api_content, codex_*, display_metadata. muster
// never shows thinking.

// Row is one messages row, the columns the renderer reads.
type Row struct {
	ID           int64
	SessionID    string
	Role         string
	Content      string
	ToolCallID   string
	ToolCalls    string
	ToolName     string
	FinishReason string
	DisplayKind  string
	UID          string
	Timestamp    float64
	Active       bool
	Compacted    bool
}

const rowColumns = `id, session_id, role, COALESCE(content, ''), COALESCE(tool_call_id, ''), COALESCE(tool_calls, ''),
COALESCE(tool_name, ''), COALESCE(finish_reason, ''), COALESCE(display_kind, ''), COALESCE(message_uid, ''),
timestamp, active, compacted`

func scanRows(rows *sql.Rows) ([]Row, error) {
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.ID, &r.SessionID, &r.Role, &r.Content, &r.ToolCallID, &r.ToolCalls, &r.ToolName,
			&r.FinishReason, &r.DisplayKind, &r.UID, &r.Timestamp, &r.Active, &r.Compacted); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Fingerprint is the lineage's transcript fingerprint.
type Fingerprint struct {
	Active   int64
	MaxID    int64
	Revision int64
}

// After is f with rows (a tail read past f) appended: what the fingerprint
// must be if nothing but those appends happened.
func (f Fingerprint) After(rows []Row) Fingerprint {
	for _, r := range rows {
		if r.Active {
			f.Active++
			f.MaxID = max(f.MaxID, r.ID)
		}
	}
	return f
}

func fingerprint(ctx context.Context, tx *sql.Tx, ids []string) (Fingerprint, error) {
	var f Fingerprint
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MAX(id), 0), (SELECT COALESCE(SUM(transcript_revision), 0)
FROM sessions WHERE id IN (`+placeholders(len(ids))+`)) FROM messages WHERE session_id IN (`+placeholders(len(ids))+`) AND active = 1`,
		append(anys(ids), anys(ids)...)...).Scan(&f.Active, &f.MaxID, &f.Revision)
	return f, err
}

// Page is one read of a conversation's transcript.
type Page struct {
	// Lineage is the conversation as this read found it.
	Lineage Lineage
	// Rows are the rows read, in display (backfill) or id (tail) order.
	Rows []Row
	// Cursor is the id the next tail reads past: the highest id of any row of
	// the lineage this read saw, whatever its active flag.
	Cursor int64
	// Fingerprint is the lineage's, in the same read.
	Fingerprint Fingerprint
	// More: the backfill had more display rows than it returned.
	More bool
	// Prompts is a backfill's prompt index: every prompt of the conversation
	// (its Line is its row id), not only the returned rows'.
	Prompts []cctranscript.PromptRef
}

// promptPeek is how much of a row's content the backfill's light pass reads
// to index a prompt by its first line.
const promptPeek = 1024

// Backfill reads the last n display rows of root's conversation, the cursor
// past every row, and the fingerprint — one read transaction.
func (s *State) Backfill(ctx context.Context, root string, n int) (Page, error) {
	var p Page
	err := s.read(ctx, func(tx *sql.Tx) error {
		l, err := lineage(ctx, tx, root)
		if err != nil {
			return err
		}
		p = Page{Lineage: l}
		ids := l.IDs()
		in := "session_id IN (" + placeholders(len(ids)) + ")"
		// The light pass: which rows are drawn, by id.
		rows, err := tx.QueryContext(ctx, "SELECT id, COALESCE(message_uid, ''), active, role, COALESCE(display_kind, ''),"+
			" substr(COALESCE(content, ''), 1, ?) FROM messages WHERE "+in+
			" AND (active = 1 OR compacted = 1) ORDER BY id", append([]any{promptPeek}, anys(ids)...)...)
		if err != nil {
			return err
		}
		type pick struct {
			id     int64
			active bool
			prompt string // the row's text, when it is a prompt
		}
		var order []string // copy keys, in first-seen order
		chosen := map[string]pick{}
		for rows.Next() {
			var (
				id                   int64
				uid, role, kind, txt string
				active               bool
			)
			if err := rows.Scan(&id, &uid, &active, &role, &kind, &txt); err != nil {
				rows.Close()
				return err
			}
			key := uid
			if key == "" {
				key = "#" + strconv.FormatInt(id, 10) // no uid: its own message
			}
			cur, seen := chosen[key]
			if !seen {
				order = append(order, key)
			}
			// The live copy wins, then the newest.
			if !seen || (active && !cur.active) || (active == cur.active && id > cur.id) {
				pk := pick{id: id, active: active}
				if r := Render(Row{ID: id, Role: role, DisplayKind: kind, Content: txt}); len(r) == 1 && r[0].Kind == cctranscript.KindPrompt {
					pk.prompt = txt
				}
				chosen[key] = pk
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		// The prompt index covers the WHOLE conversation, not only the rows
		// the backfill returns, so the message index can list prompts the
		// window above never loaded.
		for _, k := range order {
			if c := chosen[k]; c.prompt != "" {
				p.Prompts = append(p.Prompts, cctranscript.PromptRef{Line: c.id,
					Text: cctranscript.Cap(firstLine(c.prompt), cctranscript.MaxPromptRefText)})
			}
		}
		if len(order) > n {
			order, p.More = order[len(order)-n:], true
		}
		want := make([]int64, 0, len(order))
		for _, k := range order {
			want = append(want, chosen[k].id)
		}
		if len(want) > 0 {
			args := make([]any, len(want))
			for i, id := range want {
				args[i] = id
			}
			rs, err := tx.QueryContext(ctx, "SELECT "+rowColumns+" FROM messages WHERE id IN ("+placeholders(len(want))+")", args...)
			if err != nil {
				return err
			}
			got, err := scanRows(rs)
			if err != nil {
				return err
			}
			byID := map[int64]Row{}
			for _, r := range got {
				byID[r.ID] = r
			}
			for _, id := range want {
				if r, ok := byID[id]; ok {
					p.Rows = append(p.Rows, r)
				}
			}
		}
		if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(id), 0) FROM messages WHERE "+in, anys(ids)...).Scan(&p.Cursor); err != nil {
			return err
		}
		p.Fingerprint, err = fingerprint(ctx, tx, ids)
		return err
	})
	return p, err
}

// Tail reads up to limit rows of root's conversation past cursor, in id
// order, with the lineage and the fingerprint — one read transaction. The
// page's Cursor is the last row's id (cursor itself when there is none).
func (s *State) Tail(ctx context.Context, root string, cursor int64, limit int) (Page, error) {
	var p Page
	err := s.read(ctx, func(tx *sql.Tx) error {
		l, err := lineage(ctx, tx, root)
		if err != nil {
			return err
		}
		p = Page{Lineage: l, Cursor: cursor}
		ids := l.IDs()
		rs, err := tx.QueryContext(ctx, "SELECT "+rowColumns+" FROM messages WHERE session_id IN ("+placeholders(len(ids))+
			") AND id > ? ORDER BY id LIMIT ?", append(anys(ids), cursor, limit)...)
		if err != nil {
			return err
		}
		if p.Rows, err = scanRows(rs); err != nil {
			return err
		}
		if len(p.Rows) > 0 {
			p.Cursor = p.Rows[len(p.Rows)-1].ID
		}
		p.Fingerprint, err = fingerprint(ctx, tx, ids)
		return err
	})
	return p, err
}

// SameLineage: a and b are the same sessions in the same order.
func SameLineage(a, b Lineage) bool { return slices.Equal(a.IDs(), b.IDs()) }
