package hermeshost

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// State is a read-only connection to a Hermes home's state.db
// (docs/design/hermes.md, "Transcript reading").
//
//   - ONE long-lived connection per home, opened read-only (Home.stateDBURI:
//     mode=ro, never immutable=1, busy_timeout, query_only);
//   - every read is a short transaction, so the writer's checkpoints are not
//     held back;
//   - SQLITE_BUSY and a transient SQLITE_IOERR are retried
//     (P:hermes_state_readpool.py does the same: a mode=ro reader sees a
//     transient IOERR mid-checkpoint);
//   - the columns it reads are feature-detected with PRAGMA table_info, and a
//     missing one is an error naming it: muster-mirror refuses a schema it
//     cannot read rather than guessing.
//
// It is safe for concurrent use.
type State struct {
	home Home

	mu sync.Mutex
	db *sql.DB
	// ino is state.db's inode when db was opened: a database REPLACED under it
	// (a restore) is reopened.
	ino uint64
	// cols is the feature detection's verdict for db.
	cols error
}

// OpenState is h's state.db, opened lazily on the first read.
func (h Home) OpenState() *State { return &State{home: h} }

// Home is the home this reads.
func (s *State) Home() Home { return s.home }

// Close closes the connection.
func (s *State) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// requiredColumns are the columns this reader uses, per table, at the pin
// (P:hermes_state_common.py SCHEMA_SQL). reasoning*, api_content and
// system_prompt are never selected: muster never shows thinking, and the
// system prompt is not the conversation.
var requiredColumns = map[string][]string{
	"sessions": {"id", "source", "model_config", "parent_session_id", "started_at", "ended_at", "end_reason",
		"last_activity_at", "archived", "hidden", "transcript_revision"},
	"messages": {"id", "session_id", "role", "content", "tool_call_id", "tool_calls", "tool_name", "timestamp",
		"finish_reason", "active", "compacted", "display_kind", "display_order", "message_uid"},
	"session_turn_leases": {"conversation_id", "expires_at"},
}

// ErrSchema: state.db does not have a column this reader needs.
var ErrSchema = errors.New("state.db is not the schema this muster-mirror reads")

// conn is the open connection, opening (or reopening, after the file was
// replaced) it first.
func (s *State) conn() (*sql.DB, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ino := fileIno(s.home.stateDBPath())
	if s.db != nil && ino != 0 && ino != s.ino {
		_ = s.db.Close()
		s.db = nil
	}
	if s.db != nil {
		return s.db, s.cols
	}
	if ino == 0 {
		// Never create one: a read-only open of a missing file fails anyway,
		// and the error should say what is missing.
		return nil, fmt.Errorf("%s: %w", s.home.stateDBPath(), os.ErrNotExist)
	}
	uri, err := s.home.stateDBURI()
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxIdleTime(0)
	s.db, s.ino = db, ino
	s.cols = s.checkColumns(db)
	return db, s.cols
}

// checkColumns is nil when every requiredColumns column exists.
func (s *State) checkColumns(db *sql.DB) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for table, want := range requiredColumns {
		have := map[string]bool{}
		err := retry(ctx, func() error {
			rows, err := db.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var n string
				if err := rows.Scan(&n); err != nil {
					return err
				}
				have[n] = true
			}
			return rows.Err()
		})
		if err != nil {
			return fmt.Errorf("read the columns of %s: %w", table, err)
		}
		if len(have) == 0 {
			return fmt.Errorf("%w: it has no table %s", ErrSchema, table)
		}
		for _, c := range want {
			if !have[c] {
				return fmt.Errorf("%w: %s has no column %s", ErrSchema, table, c)
			}
		}
	}
	return nil
}

// read runs fn in one short read transaction, retried on SQLITE_BUSY and a
// transient SQLITE_IOERR.
func (s *State) read(ctx context.Context, fn func(*sql.Tx) error) error {
	db, err := s.conn()
	if err != nil {
		return err
	}
	return retry(ctx, func() error {
		tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		return fn(tx)
	})
}

// retry runs fn up to four times while it fails with SQLITE_BUSY or
// SQLITE_IOERR, backing off a little each time.
func retry(ctx context.Context, fn func() error) error {
	var err error
	for i := 0; i < 4; i++ {
		if err = fn(); err == nil || !transient(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(time.Duration(25*(i+1)) * time.Millisecond):
		}
	}
	return err
}

// transient: a SQLite error a retry can clear.
func transient(err error) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	switch se.Code() & 0xff {
	case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED, sqlite3.SQLITE_IOERR:
		return true
	}
	return false
}

// placeholders is "?,?,…" for n arguments.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// anys is ids as query arguments.
func anys(ids []string) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}
