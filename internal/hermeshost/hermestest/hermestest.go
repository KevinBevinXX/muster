// Package hermestest builds fake Hermes homes for tests: a scratch directory
// holding a REAL SQLite state.db created from the schema Hermes itself
// creates at the pin (schema-5fc490e.sql: its triggers included, so an insert
// gets its display_order and message_uid the way Hermes's own do), in WAL
// mode with a writer connection held open the way a running gateway holds
// one, plus breadcrumbs and decoy secret files.
//
// It is imported by tests only, and by the browser suite's fake host (a
// build-tagged fixture, never a shipped binary). 🔴 NEVER POINT IT AT A REAL
// ~/.hermes: every home New makes is a t.TempDir(), and Create is given one.
package hermestest

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// Schema is state.db's schema at the pin (see the file's header).
//
//go:embed schema-5fc490e.sql
var Schema string

// Home is one fake Hermes home.
type Home struct {
	Dir string
	db  *sql.DB
	t   testing.TB
}

// New is a fresh Hermes home with an empty state.db (WAL, schema at the pin)
// whose writer stays open until the test ends.
func New(t testing.TB) *Home {
	t.Helper()
	dir := t.TempDir()
	// The socket's safety check (home.go's ownedPrivate) refuses a home another
	// user could use, and t.TempDir()'s mode follows the process umask — in a
	// root-ish container that yields 0775. Force 0700 so the check always holds.
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := Create(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &Home{Dir: dir, t: t, db: db}
}

// Create makes dir/state.db from the pinned schema, in WAL mode, and returns
// its WRITER connection (one connection: the gateway's). It is for a browser
// test's fake host too (cmd/muster-mirror's e2emirror build), which has no
// testing.TB.
func Create(dir string) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: filepath.Join(dir, "state.db")}
	db, err := sql.Open("sqlite", u.String()+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(0)&_pragma=synchronous(OFF)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	// One transaction: statement by statement, the ~80 commits cost a second.
	if _, err := db.Exec("BEGIN;\n" + Schema + "\nCOMMIT;"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create the pinned schema: %w", err)
	}
	return db, nil
}

// DB is the home's WRITER connection: what plays Hermes's gateway.
func (h *Home) DB() *sql.DB { return h.db }

// Exec runs a statement on the writer, failing the test on an error.
func (h *Home) Exec(q string, args ...any) sql.Result {
	h.t.Helper()
	r, err := h.db.Exec(q, args...)
	if err != nil {
		h.t.Fatalf("%s: %v", q, err)
	}
	return r
}

var idSeq atomic.Int64

// LocalID is a fresh session id of the shape Hermes mints for a CLI session:
// "local-" and 64 hex (70 characters, longer than a Claude id).
func LocalID() string {
	return fmt.Sprintf("local-%064x", time.Now().UnixNano()+idSeq.Add(1))
}

// ChildID is a fresh bare 32-hex id: a rotated compression child's shape.
func ChildID() string { return fmt.Sprintf("%032x", time.Now().UnixNano()+idSeq.Add(1)) }

// SessionOpts are a session row's optional columns.
type SessionOpts struct {
	Parent      string
	Source      string // "" = "cli"
	ModelConfig string
	StartedAt   float64
	Archived    bool
	Hidden      bool
}

// Session inserts a session row.
func (h *Home) Session(id string, o SessionOpts) {
	h.t.Helper()
	if o.Source == "" {
		o.Source = "cli"
	}
	if o.StartedAt == 0 {
		o.StartedAt = Now()
	}
	var parent, mc any
	if o.Parent != "" {
		parent = o.Parent
	}
	if o.ModelConfig != "" {
		mc = o.ModelConfig
	}
	h.Exec(`INSERT INTO sessions (id, source, created_source, parent_session_id, model_config, started_at, archived, hidden)
VALUES (?, ?, 'cli', ?, ?, ?, ?, ?)`, id, o.Source, parent, mc, o.StartedAt, o.Archived, o.Hidden)
}

// End ends a session with reason.
func (h *Home) End(id, reason string) {
	h.t.Helper()
	h.Exec(`UPDATE sessions SET ended_at = ?, end_reason = ? WHERE id = ?`, Now(), reason, id)
}

// Msg is a messages row's columns.
type Msg struct {
	Role         string
	Content      string
	ToolCallID   string
	ToolCalls    string // JSON
	ToolName     string
	FinishReason string
	DisplayKind  string
	Reasoning    string
	Timestamp    float64 // 0 = now
	// Inactive marks the row active=0; Compacted sets compacted=1.
	Inactive  bool
	Compacted bool
	UID       string
	// DisplayMetadata is the display_metadata column (JSON text);
	// CompressedSummary sets _compressed_summary=1 (a compaction summary).
	DisplayMetadata   string
	CompressedSummary bool
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Message inserts a message row and returns its id.
func (h *Home) Message(session string, m Msg) int64 {
	h.t.Helper()
	if m.Timestamp == 0 {
		m.Timestamp = Now()
	}
	r := h.Exec(`INSERT INTO messages (session_id, role, content, tool_call_id, tool_calls, tool_name, finish_reason,
display_kind, reasoning, timestamp, active, compacted, message_uid, display_metadata, _compressed_summary)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		session, m.Role, nullable(m.Content), nullable(m.ToolCallID), nullable(m.ToolCalls), nullable(m.ToolName),
		nullable(m.FinishReason), nullable(m.DisplayKind), nullable(m.Reasoning), m.Timestamp, !m.Inactive, m.Compacted, nullable(m.UID),
		nullable(m.DisplayMetadata), m.CompressedSummary)
	id, err := r.LastInsertId()
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

// Lease sets the turn lease of root to expire at expires (Unix seconds).
func (h *Home) Lease(root string, expires float64) {
	h.t.Helper()
	h.Exec(`INSERT INTO session_turn_leases (conversation_id, holder, acquired_at, expires_at) VALUES (?, 'gw', ?, ?)
ON CONFLICT(conversation_id) DO UPDATE SET expires_at = excluded.expires_at`, root, Now(), expires)
}

// Unlease deletes root's lease (a turn ended).
func (h *Home) Unlease(root string) {
	h.t.Helper()
	h.Exec(`DELETE FROM session_turn_leases WHERE conversation_id = ?`, root)
}

// Breadcrumb writes the breadcrumb Hermes writes for terminal tty
// (/dev/pts/N): {session_id, cwd, ts}, at terminal-sessions/tty-dev-pts-N.
func (h *Home) Breadcrumb(tty, session string, ts float64) {
	h.t.Helper()
	name := "tty-" + strings.NewReplacer("/", "-").Replace(strings.Trim(tty, "/"))
	dir := filepath.Join(h.Dir, "terminal-sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		h.t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"session_id": session, "cwd": "/work", "ts": ts})
	if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
		h.t.Fatal(err)
	}
}

// Decoys are the secret-bearing paths of a Hermes home that muster must never
// open, relative to the home. Decoy writes each, with a marker body.
var Decoys = []string{
	"auth.json", ".env", "config.yaml", "profiles/coder/.env", "memories/MEMORY.md", "skills/s/SKILL.md",
	"logs/agent.log", "telemetry/t.json", "pastes/paste-1.txt", "runtime/active_sessions.json", "SOUL.md",
}

// DecoyMarker is every decoy's content: a string that must never appear in
// anything muster sends or logs.
const DecoyMarker = "HERMES-DECOY-SECRET-sk-or-v1-0000000000000000000000000000000000000000"

// Decoy writes every Decoys path.
func (h *Home) Decoy() {
	h.t.Helper()
	for _, p := range Decoys {
		full := filepath.Join(h.Dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			h.t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(DecoyMarker+"\n"), 0o600); err != nil {
			h.t.Fatal(err)
		}
	}
}

// Now is the current time in Unix seconds, as Hermes stores it.
func Now() float64 { return float64(time.Now().UnixNano()) / 1e9 }
