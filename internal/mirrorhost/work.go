package mirrorhost

import (
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/ccsubagents"
	"github.com/ZacxDev/muster/internal/cctasks"
	"github.com/ZacxDev/muster/internal/mirror"
)

// THE LIVE WORK COUNTS' host half (docs/design/agent-cards.md (a)): per
// Claude Code window, how many subagents and background shells its CLI has
// running now, sent on a work message of its own (mirror/work.go).
//
// The shells come from the CLI's process tree (internal/cctasks, read by
// Tmux.Windows); the rule that combines them with the CLI's own status is
// cctasks.Decide. What this file adds is the SUBAGENT count while the CLI is
// busy: a ledger of the parent transcript's Agent calls, followed from a byte
// cursor, applying the subagent cards' own state rule — internal/ccsubagents'
// Ledger, the same one ccd counts with — so a card and its rendered view's
// subagent list cannot disagree.
//
// 🔴 THE LEDGER IS READ ONLY WHILE THE STATUS IS busy OR waiting. An idle
// window costs no transcript read: the CLI's own "idle"/"shell" already says
// no subagent runs.

// mayRunWork is whether a CLI whose raw status is raw may have shells or
// subagents running — what makes reading its process tree worth it.
func mayRunWork(raw string) bool {
	switch raw {
	case cctasks.StatusShell, cctasks.StatusBusy, cctasks.StatusWaiting:
		return true
	}
	return false
}

// workCounter is the client's ledgers, one per Claude Code session (Gen) the
// listing shows: a session that is no longer listed — a /clear, a restart, a
// closed window — drops its ledger, and a new one starts from the backfill
// window.
type workCounter struct {
	mu      sync.Mutex
	ledgers map[string]*ccsubagents.Ledger
	// now is time.Now; a test moves it.
	now func() time.Time
}

// windowWork is the work map for windows (each window's Claude pane, by window
// id): cctasks.Decide over each pane's raw status, its shells and — read only
// when the status needs it — its ledger's running count. A window with no
// Claude pane, or no status, has no entry.
func (w *workCounter) windowWork(windows []WindowInfo) map[string]mirror.WindowWork {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ledgers == nil {
		w.ledgers = map[string]*ccsubagents.Ledger{}
	}
	now := time.Now
	if w.now != nil {
		now = w.now
	}
	seen := map[string]bool{}
	out := map[string]mirror.WindowWork{}
	for _, win := range windows {
		c := win.Claude
		if c == nil {
			continue
		}
		if c.Gen != "" {
			seen[c.Gen] = true
		}
		work, ok := cctasks.Decide(c.RawStatus, c.Shells, func() int {
			if c.Gen == "" || c.Transcript == "" {
				return 0
			}
			l := w.ledgers[c.Gen]
			if l == nil || l.Path() != c.Transcript || l.Started() != c.StartedAt {
				l = ccsubagents.NewLedger(c.Transcript, c.StartedAt)
				w.ledgers[c.Gen] = l
			}
			l.Read()
			return l.Running(now())
		})
		if !ok {
			continue
		}
		out[win.ID] = mirror.WindowWork{Subagents: work.Subagents, Shells: work.Shells, ShellsAtLeast: work.ShellsAtLeast}
	}
	for gen := range w.ledgers {
		if !seen[gen] {
			delete(w.ledgers, gen)
		}
	}
	return out
}

// windowColors is each window's identity colour by window id (WindowInfo.Color),
// for every window that has one.
func windowColors(windows []WindowInfo) map[string]string {
	out := map[string]string{}
	for _, w := range windows {
		if w.Color != "" {
			out[w.ID] = w.Color
		}
	}
	return out
}
