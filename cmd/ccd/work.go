package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/ccsubagents"
	"github.com/ZacxDev/muster/internal/cctasks"
	"github.com/ZacxDev/muster/internal/procstat"
)

// LIVE WORK COUNTS (docs/design/agent-cards.md (a), "ccd: POST /agent/work"):
// how many subagents and background shells this pod's Claude Code CLIs — the
// primary and every lane — have running now, reported to muster for the
// agent's card, which draws the chip a mirrored window's card does.
//
// 🔴 THE SAME CODE AS A MIRRORED WINDOW'S COUNT, OVER THE SAME THREE INPUTS,
// through the same rule (cctasks.Decide):
//
//   - the CLI's RAW session status, from its own sessions file
//     (<CLAUDE_CONFIG_DIR>/sessions/<pid>.json — measured in a live pod on
//     2.1.296: "idle", with pid, sessionId, startedAt, procStart). With no live
//     file there is no Work: the CLI says nothing, so the card shows nothing.
//   - its background shells, from its process tree: cctasks.CountShells —
//     exact while nothing can run in the foreground, "1+" while a turn may be
//     running (operator decision 3).
//   - its running subagents, read only while the status is busy or waiting:
//     internal/ccsubagents' Ledger over the session's transcript, the ledger
//     muster-mirror counts with (internal/mirrorhost/work.go). So a dispatched
//     card and a mirrored one count subagents identically.
//
// Each CLI is found by its own process — the primary's pane, each lane's pane
// (server.cliPID) — and read through ITS sessions file and the transcript that
// file names, never through the session the last hook named (cliSessionOf).
//
// STALE-SAFE: the report is re-sent every workHeartbeat while non-zero, and
// muster drops one it has not seen refreshed within its TTL
// (internal/api/agent_work.go) — a pod that died loses its chip. A CLI that
// exited leaves its sessions file behind, but not its process: a file is live
// only while its pid runs with the start time the file records.

// workInterval is how often ccd samples its CLIs' work. The chip is display
// only: this, plus the card's 10 s status poll, bounds how stale it can be.
const workInterval = 5 * time.Second

// workHeartbeat is how often an unchanged non-zero report is re-sent.
const workHeartbeat = time.Minute

// cliSession is a sessions file, the fields read.
type cliSession struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	Status    string `json:"status"`
	StartedAt int64  `json:"startedAt"`
	ProcStart string `json:"procStart"`
}

// procTable is the pod's processes: each one's argv, and each one's children.
type procTable struct {
	argv     map[int][]string
	children map[int][]int
}

func readProcTable(root string) procTable {
	t := procTable{argv: map[int][]string{}, children: map[int][]int{}}
	ents, _ := os.ReadDir(root)
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, e.Name(), "stat"))
		if err != nil {
			continue
		}
		st, ok := procstat.Parse(b)
		if !ok {
			continue
		}
		t.children[st.PPID] = append(t.children[st.PPID], pid)
		if cmd, err := os.ReadFile(filepath.Join(root, e.Name(), "cmdline")); err == nil {
			for _, a := range bytes.Split(bytes.TrimRight(cmd, "\x00"), []byte{0}) {
				t.argv[pid] = append(t.argv[pid], string(a))
			}
		}
	}
	for _, c := range t.children {
		sort.Ints(c)
	}
	return t
}

// cliSessionOf is the sessions file of CLI process pid — <configDir>/sessions/
// <pid>.json — while that process is alive with the start time the file
// records (a sessions file outlives its process, and a pid can be reused), and
// the session's transcript under <configDir>/projects ("" when there is none
// yet). ok is false when there is no live file.
//
// 🔴 BY THE CLI'S PID, NEVER BY THE LAST HOOK'S SESSION. A `claude` that a tool
// call runs inside the CLI inherits its hook settings and environment, so its
// SessionStart and SessionEnd reach this CLI's server (proctree.go). Picked by
// the hook's session, the card would count that nested CLI while it ran and
// nothing at all once it exited; picked by the pane's pid it is always this
// CLI's own file.
func cliSessionOf(configDir, root string, pid int) (cs cliSession, transcript string, ok bool) {
	if pid <= 1 {
		return cliSession{}, "", false
	}
	b, err := os.ReadFile(filepath.Join(configDir, "sessions", strconv.Itoa(pid)+".json"))
	if err != nil || json.Unmarshal(b, &cs) != nil || cs.PID != pid || !sessionIDPattern.MatchString(cs.SessionID) {
		return cliSession{}, "", false
	}
	stat, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "stat"))
	if err != nil {
		return cliSession{}, "", false
	}
	st, okStat := procstat.Parse(stat)
	if !okStat || st.State == "Z" || (cs.ProcStart != "" && cs.ProcStart != st.Start) {
		return cliSession{}, "", false
	}
	name := cs.SessionID + ".jsonl"
	if cs.Cwd != "" {
		if p := filepath.Join(projectDir(configDir, cs.Cwd), name); fileIsRegular(p) {
			return cs, p, true
		}
	}
	// The cwd moved, or its encoding differs: the id is unique, so find it.
	if m, _ := filepath.Glob(filepath.Join(configDir, "projects", "*", name)); len(m) > 0 {
		return cs, m[0], true
	}
	return cs, "", true
}

func fileIsRegular(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// cliPIDOf is the pid of s's CLI now, 0 when ccd does not know it.
func (s *server) cliPIDOf(ctx context.Context) int {
	if s.cliPID == nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	pid, err := s.cliPID(ctx)
	if err != nil {
		return 0
	}
	return pid
}

// podCounter counts the pod's work, keeping one subagent ledger per session it
// has read (as muster-mirror's workCounter does per window): a session no
// longer seen drops its ledger, and a new one starts from the backfill window.
type podCounter struct {
	configDir, root string
	// now is time.Now; a test moves it.
	now func() time.Time

	mu      sync.Mutex
	ledgers map[string]*ccsubagents.Ledger
}

// work is the work of every CLI in servers, summed: subagents and shells
// added, a lower bound if any is one. ok is false when no CLI has a live
// sessions file to read a status from — nothing may be claimed.
func (c *podCounter) work(ctx context.Context, servers []*server) (cctasks.Work, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ledgers == nil {
		c.ledgers = map[string]*ccsubagents.Ledger{}
	}
	now := time.Now
	if c.now != nil {
		now = c.now
	}
	var tbl *procTable
	var sum cctasks.Work
	any := false
	seen := map[string]bool{}
	for _, s := range servers {
		cs, transcript, ok := cliSessionOf(c.configDir, c.root, s.cliPIDOf(ctx))
		if !ok {
			continue
		}
		id := cs.SessionID
		seen[id] = true
		var sh cctasks.Shells
		switch cs.Status {
		case cctasks.StatusShell, cctasks.StatusBusy, cctasks.StatusWaiting:
			if tbl == nil {
				t := readProcTable(c.root)
				tbl = &t
			}
			sh = cctasks.CountShells(c.root, cs.PID, tbl.children, func(pid int) []string { return tbl.argv[pid] }, c.configDir)
		}
		w, ok := cctasks.Decide(cs.Status, sh, func() int {
			if transcript == "" {
				return 0
			}
			l := c.ledgers[id]
			if l == nil || l.Path() != transcript || l.Started() != cs.StartedAt {
				l = ccsubagents.NewLedger(transcript, cs.StartedAt)
				c.ledgers[id] = l
			}
			l.Read()
			return l.Running(now())
		})
		if !ok {
			continue
		}
		any = true
		sum.Subagents += w.Subagents
		sum.Shells += w.Shells
		sum.ShellsAtLeast = sum.ShellsAtLeast || w.ShellsAtLeast
	}
	for id := range c.ledgers {
		if !seen[id] {
			delete(c.ledgers, id)
		}
	}
	return sum, any
}

// workServers is every CLI's server: the primary's and each running lane's.
func (s *server) workServers() []*server {
	out := []*server{s}
	if s.lanes == nil {
		return out
	}
	s.lanes.mu.Lock()
	defer s.lanes.mu.Unlock()
	ids := make([]string, 0, len(s.lanes.set))
	for id := range s.lanes.set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		out = append(out, s.lanes.set[id].srv)
	}
	return out
}

// workReporter samples the pod's work every interval and posts it to muster
// (POST /agent/work, the agent's own token, like /agent/blocked) when it
// changes, and every heartbeat while it is non-zero.
type workReporter struct {
	base, token string
	client      *http.Client
	interval    time.Duration
	heartbeat   time.Duration
	sample      func() cctasks.Work
	now         func() time.Time
	logf        func(format string, args ...any)
}

func newWorkReporter(base, token string, sample func() cctasks.Work, logf func(string, ...any)) *workReporter {
	return &workReporter{
		base: strings.TrimRight(base, "/"), token: token,
		client:   &http.Client{Timeout: 5 * time.Second, CheckRedirect: noRedirects.CheckRedirect},
		interval: workInterval, heartbeat: workHeartbeat,
		sample: sample, now: time.Now, logf: logf,
	}
}

func (r *workReporter) run(ctx context.Context) {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	var last cctasks.Work
	var sent time.Time
	failing := false
	for {
		w := r.sample()
		if w != last || (w != (cctasks.Work{}) && r.now().Sub(sent) >= r.heartbeat) {
			if err := r.post(ctx, w); err != nil {
				if !failing {
					r.logf("ccd: reporting work %+v to muster: %v", w, err)
				}
				failing = true // retried at the next sample; logged once
			} else {
				last, sent, failing = w, r.now(), false
			}
		}
		select {
		case <-ctx.Done():
			// ccd is shutting down (the agent is being stopped): withdraw what it
			// last reported, best effort — muster also clears a stopped agent's
			// chip itself, and the TTL ends it if neither reaches it.
			if last != (cctasks.Work{}) {
				cctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				_ = r.post(cctx, cctasks.Work{})
				cancel()
			}
			return
		case <-t.C:
		}
	}
}

// workBody is POST /agent/work's body: internal/mirror.WindowWork's JSON.
type workBody struct {
	Subagents     int  `json:"subagents,omitempty"`
	Shells        int  `json:"shells,omitempty"`
	ShellsAtLeast bool `json:"shellsAtLeast,omitempty"`
}

func (r *workReporter) post(ctx context.Context, w cctasks.Work) error {
	b, _ := json.Marshal(workBody{Subagents: w.Subagents, Shells: w.Shells, ShellsAtLeast: w.ShellsAtLeast})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.base+"/agent/work", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("X-Muster-Token", r.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return &httpStatusError{resp.StatusCode}
	}
	return nil
}

type httpStatusError struct{ code int }

func (e *httpStatusError) Error() string { return "status " + strconv.Itoa(e.code) }
