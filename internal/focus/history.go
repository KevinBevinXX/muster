package focus

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

// FOCUS HISTORY (migration 0022): every focus a session has had, appended by
// Store.Upsert when the title or description changes, read newest first a page
// at a time, and pruned after HistoryRetention by the retention sweep.

const (
	// DefaultHistoryRetention is how long a history row is kept
	// (MUSTER_FOCUS_HISTORY_RETENTION unset): 30 days, the operator's call.
	DefaultHistoryRetention = 30 * 24 * time.Hour
	// DefaultHistoryPage is a history page's size when the caller names none.
	DefaultHistoryPage = 20
	// MaxHistoryPage bounds a history page.
	MaxHistoryPage = 50
)

// HistoryEntry is one past focus.
type HistoryEntry struct {
	ID          int64
	Title       string
	Description string
	CreatedAt   time.Time
}

// HistoryPage is one page of a subject's history, newest first. Next is the
// cursor of the page after it — older entries — and "" when this page reaches
// the oldest.
type HistoryPage struct {
	Entries []HistoryEntry
	Next    string
}

// ErrBadCursor refuses a history cursor this package did not write.
var ErrBadCursor = errors.New("focus: a history cursor is <created_at in unix microseconds>.<id>")

// historyCursor is a parsed cursor: the (created_at, id) the next page starts
// strictly before. set is false for "" (from the newest).
type historyCursor struct {
	set bool
	at  time.Time
	id  int64
}

// historyCursorOf is the cursor that continues after e.
func historyCursorOf(e HistoryEntry) string {
	return strconv.FormatInt(e.CreatedAt.UnixMicro(), 10) + "." + strconv.FormatInt(e.ID, 10)
}

// parseHistoryCursor reads a cursor History returned ("" is the first page).
func parseHistoryCursor(raw string) (historyCursor, error) {
	if raw == "" {
		return historyCursor{}, nil
	}
	at, id, ok := strings.Cut(raw, ".")
	if !ok {
		return historyCursor{}, ErrBadCursor
	}
	us, err1 := strconv.ParseInt(at, 10, 64)
	n, err2 := strconv.ParseInt(id, 10, 64)
	if err1 != nil || err2 != nil || us <= 0 || n <= 0 {
		return historyCursor{}, ErrBadCursor
	}
	return historyCursor{set: true, at: time.UnixMicro(us), id: n}, nil
}
