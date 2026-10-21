package mirrorhost

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/hermeshost"
)

// HermesSource is what a host reads Hermes sessions from (Phase 2,
// docs/design/hermes.md): one Hermes home, its state.db opened read-only ONCE
// for the host's life, and the directory `muster-mirror hermes-hook` writes
// its status files in. A Tmux with no HermesSource still DETECTS a Hermes
// window (the strict rule) but resolves no session: no rendered view, unknown
// status — exactly Phase 1.
type HermesSource struct {
	Home   hermeshost.Home
	Status hermeshost.StatusDir

	once  sync.Once
	state *hermeshost.State
}

// NewHermesSource is the source for the Hermes home at homeDir with the hook's
// status files in statusDir.
func NewHermesSource(homeDir, statusDir string) *HermesSource {
	return &HermesSource{Home: hermeshost.NewHome(homeDir), Status: hermeshost.StatusDir(statusDir)}
}

// State is the home's one read-only state.db connection.
func (s *HermesSource) State() *hermeshost.State {
	s.once.Do(func() { s.state = s.Home.OpenState() })
	return s.state
}

// SourceBreadcrumb: a Hermes pane whose session was resolved through its
// terminal's breadcrumb (hermeshost.Locator).
const SourceBreadcrumb = "breadcrumb"

// hermesLocateTimeout bounds one pane's resolution (a breadcrumb read and one
// short state.db read): the listing runs every refresh.
const hermesLocateTimeout = 2 * time.Second

// resolve fills a claimed Hermes pane's session and status from src. cli is
// the Hermes CLI's pid, tty the pane's terminal; procs is the listing's proc
// table, which also says whether a status file's writer is still alive.
func (src *HermesSource) resolve(ap *AgentPane, cli int, tty, proc string, procs map[int]procInfo) {
	start := procStartTime(proc, procs[cli].start)
	loc := &hermeshost.Locator{State: src.State(), Status: src.Status,
		Alive: func(pid int, start string) bool { p, ok := procs[pid]; return ok && start != "" && p.start == start }}
	ctx, cancel := context.WithTimeout(context.Background(), hermesLocateTimeout)
	defer cancel()
	ps := loc.Locate(ctx, tty, start)
	ap.Hermes = &ps
	ap.Gen, ap.Status, ap.StatusSince = ps.Session, ps.Status.Status, ps.Status.Since
	ap.Busy = ap.Status == "busy"
	if ps.Session != "" {
		ap.Source = SourceBreadcrumb
	}
}

// clockTicks is the kernel's USER_HZ, the unit of /proc/<pid>/stat's start
// time: 100 on every Linux architecture muster-mirror runs on (it is fixed
// for the proc ABI, unlike the scheduler's HZ).
const clockTicks = 100

// bootTimes caches each proc root's boot time (btime in <proc>/stat).
var bootTimes sync.Map

// procStartTime is a process's start as wall time: the boot time plus its
// start in clock ticks. Zero when either cannot be read — which the locator
// takes as "no proof the breadcrumb is this process's", never as a pass.
//
// ⚠ btime has a resolution of one second, so the result can be up to a
// second EARLY: a breadcrumb written in the second before the process really
// started can pass. The previous CLI on the same terminal would have had to
// write it (on /new) and exit within that second.
func procStartTime(proc, ticks string) time.Time {
	t, err := strconv.ParseInt(ticks, 10, 64)
	if err != nil || t <= 0 {
		return time.Time{}
	}
	bt, ok := bootTimes.Load(proc)
	if !ok {
		b, err := readBootTime(proc)
		if err != nil {
			return time.Time{}
		}
		bt, _ = bootTimes.LoadOrStore(proc, b)
	}
	return time.Unix(bt.(int64), 0).Add(time.Duration(t) * time.Second / clockTicks)
}

// readBootTime is the btime line of <proc>/stat.
func readBootTime(proc string) (int64, error) {
	f, err := os.Open(filepath.Join(proc, "stat"))
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "btime "); ok {
			return strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		}
	}
	return 0, os.ErrNotExist
}
