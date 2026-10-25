package mirrorhost

import (
	"context"
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/opencodehost"
)

// OPENCODE WINDOWS (docs/design/opencode.md): a pane whose process tree holds
// an opencode TUI on the pane's terminal is an opencode window, and — with an
// OpenCodeSource — the session its pane title names (opencodehost.MatchTitle)
// is its rendered view, read from the host's opencode v2 server with the
// host's pair token. VIEW ONLY: nothing is ever sent to opencode, and the
// paste guard refuses its panes.

// SourceTitle: an opencode pane whose session was matched by its pane title,
// a heuristic (view-only).
const SourceTitle = "title"

// OpenCodePane is how an opencode pane resolved: what -list -opencode prints.
type OpenCodePane struct {
	// PaneTitle is the pane's title as tmux reported it ("" when it could not
	// be read).
	PaneTitle string
	// Match is how the title resolved against the server's root sessions.
	Match opencodehost.Match
	// Server is the opencode server's state when the pane was resolved: ""
	// when it answered, else why muster cannot read it (no token, refused,
	// unreachable).
	Server string
	// NoView is why the pane has no rendered view, "" when it has one.
	NoView string
}

// openCodeSubcommands are opencode's subcommands at 6ec5682
// (packages/cli/src/commands/commands.ts): a process running one of them is
// not the TUI.
var openCodeSubcommands = map[string]bool{
	"upgrade": true, "uninstall": true, "acp": true, "api": true, "debug": true, "auth": true, "mcp": true, "plugin": true,
	"models": true, "stats": true, "mini": true, "run": true, "session": true, "service": true, "reload": true, "pair": true, "serve": true,
}

// openCodeValueFlags are the TUI's flags that take a value.
var openCodeValueFlags = map[string]bool{"--server": true, "-s": true, "--session": true, "--prompt": true}

// openCodeTUI reports whether p is the opencode TUI by its own identity —
// openCodeShape (comm or argv[0] opencode) and no subcommand — and whether it
// is a client of the SHARED server: --standalone runs a private one, and
// --server names another, whose sessions the host's server does not have.
func openCodeTUI(p procInfo) (tui, shared bool) {
	if !openCodeShape(p) {
		return false, false
	}
	shared = true
	args := p.argv
	if len(args) > 0 {
		args = args[1:]
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, _, hasValue := strings.Cut(a, "=")
		switch {
		case name == "--standalone" || name == "--server":
			shared = false
			if name == "--server" && !hasValue {
				i++
			}
		case openCodeValueFlags[name] && !hasValue:
			i++
		case strings.HasPrefix(a, "-"):
		case openCodeSubcommands[a]:
			return false, false
		default:
			// The positional is the directory to start in: still the TUI.
			return true, shared
		}
	}
	return true, shared
}

// openCodeCLI is the opencode TUI in the tree at or below root, breadth-first
// and bounded: the nearest that matches openCodeTUI AND whose stdin is tty,
// the pane's terminal. false when there is none, or anything needed cannot
// be read: this is a claim, so doubt claims nothing.
func openCodeCLI(proc string, root int, tty string, procs map[int]procInfo, children map[int][]int) (pid int, shared, ok bool) {
	if tty == "" {
		return 0, false, false
	}
	queue, seen := []int{root}, map[int]bool{}
	for len(queue) > 0 && len(seen) < procWalkBound {
		p := queue[0]
		queue = queue[1:]
		if seen[p] {
			continue
		}
		seen[p] = true
		info, ok := procs[p]
		if !ok {
			continue
		}
		if tui, sh := openCodeTUI(info); tui && info.state != "Z" {
			if fd0, err := os.Readlink(filepath.Join(proc, strconv.Itoa(p), "fd", "0")); err == nil && fd0 == tty {
				return p, sh, true
			}
		}
		queue = append(queue, children[p]...)
	}
	return 0, false, false
}

// LocatePanes claims each pane whose tree holds an opencode TUI on the pane's
// terminal, and — with a source — resolves its session by the pane's title.
// It walks nothing when the table holds no opencode process.
func (r openCodeRuntime) LocatePanes(panes []pane, procs map[int]procInfo, children map[int][]int) map[string]AgentPane {
	out := map[string]AgentPane{}
	if len(panes) == 0 || !anyOpenCode(procs) {
		return out
	}
	for _, p := range panes {
		tty := p.TTY
		if tty == "" {
			var err error
			if tty, err = os.Readlink(filepath.Join(r.proc, strconv.Itoa(p.PID), "fd", "0")); err != nil {
				continue
			}
		}
		pid, shared, ok := openCodeCLI(r.proc, p.PID, tty, procs, children)
		if !ok {
			continue
		}
		ap := AgentPane{Runtime: mirror.RuntimeOpenCode, Pane: p.ID, PID: pid, Source: SourceProcess}
		if r.src != nil {
			r.src.resolve(&ap, shared, r.title)
		}
		out[p.ID] = ap
	}
	return out
}

// anyOpenCode reports whether any process in procs is opencode by
// openCodeShape.
func anyOpenCode(procs map[int]procInfo) bool {
	for _, p := range procs {
		if openCodeShape(p) {
			return true
		}
	}
	return false
}

// paneTitle is pane's #{pane_title}, read now ("-u": UTF-8, so a title's "…"
// arrives as itself).
func (t Tmux) paneTitle(paneID string) (string, bool) {
	if !strings.HasPrefix(paneID, "%") {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, t.bin(), t.query("display-message", "-p", "-t", paneID, "#{pane_title}")...)
	cmd.Env = tmuxEnv()
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSuffix(string(out), "\n"), true
}

// OpenCodeSource is the host's opencode v2 server, as muster-mirror reads it
// with its pair token: the root sessions (refreshed when the event stream
// says they changed, and at most every sessionsMaxAge), each session's status
// from the event stream (session.status), and a change counter per session
// that a rendered view polls. ONE event stream for the whole host, opened on
// the first opencode pane and kept open (reconnecting) for the daemon's life.
type OpenCodeSource struct {
	creds *opencodehost.Credentials
	// why is the source's fixed refusal when it has no credential.
	why    string
	ctx    context.Context
	logger *log.Logger

	once sync.Once
	mu   sync.Mutex
	// connected: the event stream is open, so statuses are believed.
	connected bool
	// down is why the server cannot be read now ("" when it can).
	down string
	// sessions are the root sessions, read at sessionsAt; stale says an event
	// changed them since.
	sessions   []opencodehost.Session
	sessionsAt time.Time
	stale      bool
	// busy and since are each session's status from the stream (since: when
	// it last changed, Unix ms; the host's clock).
	busy  map[string]bool
	since map[string]int64
	// changes counts the events naming each session.
	changes map[string]uint64
	// now is the source's clock.
	now func() time.Time
}

// titleEvents are the events after which the root sessions' titles are read
// again (upstream's session events at 6ec5682, packages/schema/src/session-event.ts).
var titleEvents = map[string]bool{"session.created": true, "session.renamed": true, "session.deleted": true, "session.moved": true,
	"session.forked": true, "session.archived": true, "session.unarchived": true, "session.updated": true}

// sessionsMaxAge bounds how old a session list the locator matches against.
const sessionsMaxAge = 30 * time.Second

// NewOpenCodeSource is the source for creds (nil: why says why there is none
// — every opencode window then has no view, and that reason). Its event
// stream lives until ctx ends.
func NewOpenCodeSource(ctx context.Context, creds *opencodehost.Credentials, why string, logger *log.Logger) *OpenCodeSource {
	if logger == nil {
		logger = log.New(os.Stderr, "", log.LstdFlags)
	}
	return &OpenCodeSource{creds: creds, why: why, ctx: ctx, logger: logger, busy: map[string]bool{}, since: map[string]int64{},
		changes: map[string]uint64{}, now: time.Now}
}

// errNoOpenCodeCredential is a source without a token's refusal.
var errNoOpenCodeCredential = errors.New("muster-mirror has no opencode token on this host (pair it with `muster-mirror opencode-pair`)")

// start opens the event stream, once.
func (s *OpenCodeSource) start() {
	if s.creds == nil {
		return
	}
	s.once.Do(func() { go s.run() })
}

// unauthorizedWhy is what a refused token says.
const unauthorizedWhy = "the opencode server refused muster-mirror's token (it expired, or the server's password was rotated): pair the host again with `muster-mirror opencode-pair`"

// run keeps the event stream open until the source's context ends.
func (s *OpenCodeSource) run() {
	backoff := time.Second
	for s.ctx.Err() == nil {
		cl, err := s.creds.Client()
		if err == nil {
			err = cl.Events(s.ctx, func() { s.connect(cl) }, s.event)
		}
		if s.ctx.Err() != nil {
			return
		}
		why := "the opencode server cannot be read: " + err.Error()
		wait := backoff
		if errors.Is(err, opencodehost.ErrUnauthorized) {
			why, wait = unauthorizedWhy, time.Minute
		}
		s.mu.Lock()
		was := s.down
		s.connected, s.down = false, why
		s.mu.Unlock()
		if was != why {
			s.logger.Printf("muster-mirror: opencode: %s (retrying in %s)", why, wait)
		}
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(wait):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// connect is the stream's open: the running set seeds every status, and the
// session list is re-read on next use.
func (s *OpenCodeSource) connect(cl *opencodehost.Client) {
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	running, err := cl.Running(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.down = "the opencode server cannot be read: " + err.Error()
		return
	}
	now := s.now().UnixMilli()
	for id := range s.busy {
		if !running[id] {
			s.busy[id], s.since[id] = false, now
		}
	}
	for id := range running {
		if !s.busy[id] {
			s.busy[id], s.since[id] = true, now
		}
	}
	if s.down != "" {
		s.logger.Printf("muster-mirror: opencode: reading %s again", cl.URL())
	}
	s.connected, s.down, s.stale = true, "", true
	for id := range s.changes {
		s.changes[id]++ // anything may have happened while the stream was closed
	}
}

// event is one frame of the stream.
func (s *OpenCodeSource) event(e opencodehost.Event) {
	id := e.SessionID()
	s.mu.Lock()
	defer s.mu.Unlock()
	if titleEvents[e.Type] {
		s.stale = true // the root sessions or their titles may have changed
	}
	if id == "" {
		return
	}
	s.changes[id]++
	switch st := e.Status(); st {
	case "busy", "retry":
		if !s.busy[id] {
			s.busy[id], s.since[id] = true, s.now().UnixMilli()
		}
	case "idle":
		if b, ok := s.busy[id]; b || !ok {
			s.busy[id], s.since[id] = false, s.now().UnixMilli()
		}
	}
}

// state is why the server cannot be read now, "" when it can.
func (s *OpenCodeSource) state() string {
	if s.creds == nil {
		if s.why != "" {
			return s.why
		}
		return errNoOpenCodeCredential.Error()
	}
	s.start()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.down
}

// rootSessions is the root session list, re-read when stale or older than
// sessionsMaxAge.
func (s *OpenCodeSource) rootSessions() ([]opencodehost.Session, error) {
	s.mu.Lock()
	fresh := !s.stale && !s.sessionsAt.IsZero() && s.now().Sub(s.sessionsAt) < sessionsMaxAge
	list := s.sessions
	s.mu.Unlock()
	if fresh {
		return list, nil
	}
	cl, err := s.creds.Client()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(s.ctx, 3*time.Second)
	defer cancel()
	list, err = cl.Sessions(ctx)
	if err != nil {
		if errors.Is(err, opencodehost.ErrUnauthorized) {
			s.mu.Lock()
			s.down = unauthorizedWhy
			s.mu.Unlock()
		}
		return nil, err
	}
	s.mu.Lock()
	s.sessions, s.sessionsAt, s.stale = list, s.now(), false
	s.mu.Unlock()
	return list, nil
}

// status is session id's status (StatusBusy or StatusIdle) and its time;
// "" while the stream is not open — unknown, which fails closed.
func (s *OpenCodeSource) status(id string) (string, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.connected {
		return "", 0
	}
	since := s.since[id]
	if s.busy[id] {
		return mirror.StatusBusy, since
	}
	return mirror.StatusIdle, since
}

// changed is session id's change count: it moves on every event naming it.
func (s *OpenCodeSource) changed(id string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changes[id]
}

// client is a client of the server with the current token.
func (s *OpenCodeSource) client() (*opencodehost.Client, error) {
	if s.creds == nil {
		return nil, errNoOpenCodeCredential
	}
	return s.creds.Client()
}

// resolve fills a claimed opencode pane's session and status: the session
// its title names, when the server can be read and exactly one root session
// matches.
func (s *OpenCodeSource) resolve(ap *AgentPane, shared bool, title func(string) (string, bool)) {
	oc := &OpenCodePane{}
	ap.OpenCode = oc
	// The person reads the same reason on the window's card (AgentNote): why
	// it has no view, else what the token needs (Credentials.Health).
	defer func() {
		ap.Note = oc.NoView
		if ap.Note == "" {
			ap.Note = s.tokenHealth()
		}
	}()
	if !shared {
		oc.NoView = "this opencode runs on its own server (--standalone or --server), not the host's"
		return
	}
	// A server that refuses the token, or no token at all, is no view. A
	// stream that is merely down is a view with an UNKNOWN status (status()):
	// the REST reads still work, and the view re-reads on its own cadence.
	if oc.Server = s.state(); s.creds == nil {
		oc.NoView = notPairedWhy
		return
	}
	if oc.Server == unauthorizedWhy {
		oc.NoView = s.refusedWhy()
		return
	}
	t, ok := title(ap.Pane)
	if !ok {
		oc.NoView = "the pane's title could not be read"
		return
	}
	oc.PaneTitle = t
	sessions, err := s.rootSessions()
	if err != nil {
		oc.Server = "the opencode server cannot be read: " + err.Error()
		oc.NoView = oc.Server
		if errors.Is(err, opencodehost.ErrUnauthorized) {
			oc.Server, oc.NoView = unauthorizedWhy, s.refusedWhy()
		}
		return
	}
	oc.Match = opencodehost.MatchTitle(t, sessions)
	if oc.Match.Session == "" {
		oc.NoView = oc.Match.Why
		return
	}
	ap.Gen, ap.Source = oc.Match.Session, SourceTitle
	ap.Status, ap.StatusSince = s.status(oc.Match.Session)
	ap.Busy = ap.Status == mirror.StatusBusy
}

// notPairedWhy is a window's reason when the host has no opencode token.
const notPairedWhy = "opencode is not paired on this host: run muster-mirror opencode-pair"

// tokenHealth is what the token needs the person to know now
// (Credentials.Health), "" for nothing or no token.
func (s *OpenCodeSource) tokenHealth() string {
	if s.creds == nil {
		return ""
	}
	return s.creds.Health(s.now())
}

// refusedWhy is a refused token's reason: the token's own health when it says
// more (it expired), else that the server refused it.
func (s *OpenCodeSource) refusedWhy() string {
	if h := s.tokenHealth(); strings.Contains(h, "expired") {
		return h
	}
	return unauthorizedWhy
}

// ResolveOpenCodePane is the AgentPane of an opencode TUI (pid) in pane whose
// title is title, resolved against s exactly as the tmux backend resolves one
// (a client of the shared server). For a backend that knows its panes' titles
// another way — the browser suite's fake tmux, whose windows file names them.
func ResolveOpenCodePane(s *OpenCodeSource, pane string, pid int, title string) AgentPane {
	ap := AgentPane{Runtime: mirror.RuntimeOpenCode, Pane: pane, PID: pid, Source: SourceProcess}
	if s != nil {
		s.resolve(&ap, true, func(string) (string, bool) { return title, true })
	}
	return ap
}
