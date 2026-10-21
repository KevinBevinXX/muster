//go:build livehermes

package mirrorhost

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/hermeshost"
	_ "modernc.org/sqlite"
)

// TestLiveHermesTimings re-measures, on the pinned Hermes and UNDER LOAD,
// the three timings the gated send's safety rests on (docs/design/hermes.md,
// "Believing a status file", "The send path"):
//
//  1. HOOK SLACK. The false-idle rule believes `idle` only when no message
//     row is newer than the status file's `since`, with slack 0. That holds
//     only while Hermes writes a turn's final row BEFORE on_session_end, and a
//     new turn's user row BEFORE pre_llm_call. Every gap must be >= 0.
//  2. THE ENTER MARGIN. Enter within 50 ms of a buffer change is a newline,
//     bracketed paste included; muster waits enterDelay (150 ms). At 150 ms
//     every Enter must submit.
//  3. THE QUEUE GAP. In `queue` busy mode the CLI submits a queued message
//     shortly after a turn ends, while the hook already says idle: the window
//     in which the status is idle and no newer row exists yet. Reported (it is
//     a named residual risk), not gated.
//
// Load: 2 × NumCPU busy shell loops for the whole test, stopped by their own
// PIDs at its end. MUSTER_LIVEHERMES_NOLOAD=1 measures an idle host instead.
func TestLiveHermesTimings(t *testing.T) {
	r := newLiveRun(t)
	loaded := os.Getenv("MUSTER_LIVEHERMES_NOLOAD") == ""
	if loaded {
		stop := cpuLoad(t, 2*runtime.NumCPU())
		defer stop()
	}
	cond := "an idle host"
	if loaded {
		cond = fmt.Sprintf("load: %d busy loops on %d CPUs", 2*runtime.NumCPU(), runtime.NumCPU())
	}
	fmt.Printf("\nLIVEHERMES TIMINGS (pin %s, %s)\n", hermesPin[:7], cond)

	t.Run("hook slack", func(t *testing.T) {
		home := r.makeHome(homeOpts{name: "timing", hook: true})
		pane := r.cli(home, 160, 45, []string{"chat"})
		defer r.killWindow(pane)
		if !r.waitIdle(pane, 60*time.Second) {
			t.Fatal("never idle:\n" + showScreen(r.capture(pane), 10))
		}
		var finalGaps, userGaps []float64
		const turns = 12
		for i := 0; i < turns; i++ {
			rec := watchStatus(r.statusDir())
			typed(r, pane, fmt.Sprintf("timing turn %d", i))
			if !waitFor(30*time.Second, func() bool { return rec.has("idle", 1) && rec.has("busy", 1) }) {
				rec.stop()
				t.Fatalf("turn %d: the hook never reported busy then idle (seen %v)", i, rec.seen())
			}
			time.Sleep(300 * time.Millisecond)
			rec.stop()
			sid := rec.session()
			rows := messageRows(t, home, sid)
			busy, idle := rec.first("busy"), rec.last("idle")
			u, a := lastRow(rows, "user"), lastRow(rows, "assistant")
			if u == 0 || a == 0 {
				t.Fatalf("turn %d: no user/assistant rows in %s", i, sid)
			}
			userGaps = append(userGaps, float64(busy)-u*1000)
			finalGaps = append(finalGaps, float64(idle)-a*1000)
		}
		fmt.Printf("hook slack, %d turns:\n  user row -> pre_llm_call (busy):     %s\n  final row -> on_session_end (idle):   %s\n", turns, summary(userGaps), summary(finalGaps))
		for _, g := range append(append([]float64{}, userGaps...), finalGaps...) {
			if g < 0 {
				t.Errorf("a hook arrived BEFORE its row (gap %.1f ms): the false-idle rule's slack 0 does not hold", g)
			}
		}
	})

	t.Run("enter margin", func(t *testing.T) {
		home := r.makeHome(homeOpts{name: "enter"})
		pane := r.cli(home, 160, 45, []string{"chat"})
		defer r.killWindow(pane)
		if !r.waitIdle(pane, 60*time.Second) {
			t.Fatal("never idle")
		}
		delays := []time.Duration{0, 20 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond, enterDelay}
		const tries = 5
		var lines []string
		n := 0
		for _, d := range delays {
			ok := 0
			for k := 0; k < tries; k++ {
				n++
				text := fmt.Sprintf("enter margin %d", n)
				before := countUserRows(t, home, text)
				pasteEnter(t, r, pane, text, d)
				if waitFor(8*time.Second, func() bool { return countUserRows(t, home, text) > before }) {
					ok++
					if !r.waitIdle(pane, 40*time.Second) {
						t.Fatal("never idle after a turn")
					}
					continue
				}
				// Not submitted: the text is in the box with a newline. Clear it
				// (a double Escape moves it into the history) and settle.
				r.keys(pane, "\x1b\x1b")
				time.Sleep(1500 * time.Millisecond)
				if !r.waitIdle(pane, 20*time.Second) {
					t.Fatal("the box would not clear:\n" + showScreen(r.capture(pane), 8))
				}
			}
			lines = append(lines, fmt.Sprintf("  Enter %4d ms after the paste: submitted %d of %d", d.Milliseconds(), ok, tries))
			if d == enterDelay && ok != tries {
				t.Errorf("at muster's enterDelay (%v) only %d of %d Enters submitted", enterDelay, ok, tries)
			}
		}
		fmt.Println("enter margin:\n" + strings.Join(lines, "\n"))
	})

	t.Run("queue gap", func(t *testing.T) {
		home := r.makeHome(homeOpts{name: "queue", hook: true, busyMode: "queue"})
		pane := r.cli(home, 160, 45, []string{"chat"})
		defer r.killWindow(pane)
		if !r.waitIdle(pane, 60*time.Second) {
			t.Fatal("never idle")
		}
		var gaps []float64
		const runs = 6
		for i := 0; i < runs; i++ {
			rec := watchStatus(r.statusDir())
			typed(r, pane, "SLOW2 queue gap")
			if !inputStarts(r, pane, "☤", 20*time.Second) {
				rec.stop()
				t.Fatal("the turn never started")
			}
			text := fmt.Sprintf("queued message %d", i)
			typed(r, pane, text)
			if !waitFor(40*time.Second, func() bool { return countUserRows(t, home, text) > 0 }) {
				rec.stop()
				t.Fatalf("run %d: the queued message was never submitted", i)
			}
			if !r.waitIdle(pane, 40*time.Second) {
				rec.stop()
				t.Fatal("never idle after the queued turn")
			}
			rec.stop()
			rows := messageRows(t, home, rec.session())
			var queuedAt float64
			for _, row := range rows {
				if row.role == "user" && row.content == text {
					queuedAt = row.ts
				}
			}
			// The idle the SLOW2 turn ended with: the newest idle write
			// before the queued row.
			idle := int64(0)
			for _, s := range rec.writes() {
				if s.status == "idle" && float64(s.since) <= queuedAt*1000 && s.since > idle {
					idle = s.since
				}
			}
			if idle == 0 || queuedAt == 0 {
				t.Fatalf("run %d: no idle before the queued row (writes %v)", i, rec.writes())
			}
			gaps = append(gaps, queuedAt*1000-float64(idle))
		}
		fmt.Printf("queue gap (queue mode: idle written -> the queued message's row), %d runs: %s\n", runs, summary(gaps))
	})
}

// cpuLoad starts n busy shell loops; stop ends them, by their own PIDs.
func cpuLoad(t *testing.T, n int) (stop func()) {
	var cmds []*exec.Cmd
	for i := 0; i < n; i++ {
		c := exec.Command("sh", "-c", "while :; do :; done")
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, c)
	}
	return func() {
		for _, c := range cmds {
			_ = c.Process.Kill()
			_, _ = c.Process.Wait()
		}
	}
}

// pasteEnter is Tmux.PastePane with the Enter after d instead of enterDelay.
func pasteEnter(t *testing.T, r *liveRun, pane, text string, d time.Duration) {
	if out, err := r.tmuxRun("set-buffer", "-b", "lhm", "--", text); err != nil {
		t.Fatal(out)
	}
	if out, err := r.tmuxRun("paste-buffer", "-p", "-d", "-b", "lhm", "-t", pane); err != nil {
		t.Fatal(out)
	}
	if d > 0 {
		time.Sleep(d)
	}
	if out, err := r.tmuxRun("set-buffer", "-b", "lhm", "--", "\n"); err != nil {
		t.Fatal(out)
	}
	if out, err := r.tmuxRun("paste-buffer", "-d", "-b", "lhm", "-t", pane); err != nil {
		t.Fatal(out)
	}
}

// statusWrite is one status-file state the recorder saw.
type statusWrite struct {
	session, status string
	since           int64
}

// statusRecorder polls a status directory every 5 ms and keeps every
// distinct write.
type statusRecorder struct {
	mu   sync.Mutex
	ws   []statusWrite
	done chan struct{}
	wg   sync.WaitGroup
}

func watchStatus(dir string) *statusRecorder {
	rec := &statusRecorder{done: make(chan struct{})}
	start := time.Now().UnixMilli()
	seen := map[string]bool{}
	rec.wg.Add(1)
	go func() {
		defer rec.wg.Done()
		for {
			select {
			case <-rec.done:
				return
			default:
			}
			ents, _ := os.ReadDir(dir)
			for _, e := range ents {
				id := strings.TrimSuffix(e.Name(), ".json")
				if id == e.Name() {
					continue
				}
				f, err := hermeshost.StatusDir(dir).Read(id)
				if err != nil || f.Since < start {
					continue
				}
				key := fmt.Sprintf("%s/%s/%d", id, f.Status, f.Since)
				if seen[key] {
					continue
				}
				seen[key] = true
				rec.mu.Lock()
				rec.ws = append(rec.ws, statusWrite{id, f.Status, f.Since})
				rec.mu.Unlock()
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	return rec
}

func (rec *statusRecorder) stop() { close(rec.done); rec.wg.Wait() }

func (rec *statusRecorder) writes() []statusWrite {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]statusWrite(nil), rec.ws...)
}

func (rec *statusRecorder) seen() []string {
	var out []string
	for _, w := range rec.writes() {
		out = append(out, w.status)
	}
	return out
}

func (rec *statusRecorder) has(status string, n int) bool {
	c := 0
	for _, w := range rec.writes() {
		if w.status == status {
			c++
		}
	}
	return c >= n
}

func (rec *statusRecorder) first(status string) int64 {
	for _, w := range rec.writes() {
		if w.status == status {
			return w.since
		}
	}
	return 0
}

func (rec *statusRecorder) last(status string) int64 {
	var out int64
	for _, w := range rec.writes() {
		if w.status == status {
			out = w.since
		}
	}
	return out
}

func (rec *statusRecorder) session() string {
	ws := rec.writes()
	if len(ws) == 0 {
		return ""
	}
	return ws[len(ws)-1].session
}

// messageRow is one row of state.db's messages.
type messageRow struct {
	role, content string
	ts            float64
}

// messageRows is every row of session sid in home's state.db, by id.
func messageRows(t *testing.T, home, sid string) []messageRow {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(home, "state.db")+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT role, COALESCE(content, ''), timestamp FROM messages WHERE session_id = ? ORDER BY id`, sid)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []messageRow
	for rows.Next() {
		var m messageRow
		if err := rows.Scan(&m.role, &m.content, &m.ts); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

// countUserRows is how many user rows in home's state.db hold exactly text.
func countUserRows(t *testing.T, home, text string) int {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(home, "state.db")+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE role = 'user' AND content = ?`, text).Scan(&n); err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return 0
		}
		t.Fatal(err)
	}
	return n
}

// lastRow is the timestamp of the newest row of role, 0 for none.
func lastRow(rows []messageRow, role string) float64 {
	var ts float64
	for _, r := range rows {
		if r.role == role && strings.TrimSpace(r.content) != "" {
			ts = r.ts
		}
	}
	return ts
}

// waitFor polls ok every 20 ms for up to d.
func waitFor(d time.Duration, ok func() bool) bool {
	deadline := time.Now().Add(d)
	for {
		if ok() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// summary is min, median and max of ms values.
func summary(v []float64) string {
	if len(v) == 0 {
		return "no samples"
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return fmt.Sprintf("min %.0f ms, median %.0f ms, max %.0f ms (n=%d)", s[0], s[len(s)/2], s[len(s)-1], len(s))
}
