package mirror

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/provision"
)

// The attach refusals. Each carries its own remedy, so internal/api passes the
// text to the browser verbatim.
var (
	// ErrHostOffline: the host's muster-mirror is not connected.
	ErrHostOffline = errors.New("mirror: the host is offline — its muster-mirror is not connected to muster")
	// ErrSessionGone: the host is connected and does not list the session.
	ErrSessionGone = errors.New("mirror: the host is online but no longer lists this tmux session")
	// ErrWindowGone: the host lists the session, and not this window of it (the
	// window was closed, or the host registers per session and has no windows
	// to name).
	ErrWindowGone = errors.New("mirror: the host is online but no longer lists this tmux window")
	// ErrHostDisconnected: the host's connection ended while attached (it went
	// away, its token was revoked, or a newer connection replaced it).
	ErrHostDisconnected = errors.New("mirror: the host's connection ended while the terminal was attached")
	// ErrOverrun: the browser could not keep up with the terminal's output.
	ErrOverrun = errors.New("mirror: the terminal produced output faster than the browser read it; reconnect to resume")
	// ErrInteractiveRefused: the host did not advertise interactive attaches.
	ErrInteractiveRefused = errors.New("mirror: this host is read-only — its muster-mirror was not started with -interactive")
	// ErrTooManyStreams: the host already has MaxStreamsPerHost terminals open.
	ErrTooManyStreams = fmt.Errorf("mirror: this host already has %d terminals open; close one first", MaxStreamsPerHost)
	// ErrTooManyHosts: the hub already holds MaxHosts connected hosts.
	ErrTooManyHosts = fmt.Errorf("mirror: muster already has %d mirror hosts connected", MaxHosts)
)

const (
	// helloTimeout bounds how long a fresh connection may take to say hello.
	helloTimeout = 10 * time.Second
	// pingInterval keeps the tunnel alive through proxies that close quiet
	// connections, and detects a host that vanished without a close frame.
	pingInterval = 25 * time.Second
	// writeTimeout bounds one control write to the host.
	writeTimeout = 10 * time.Second
	// streamBuffer is how many host output frames one stream may hold while the
	// browser is busy. A stream that fills it is ended (ErrOverrun) rather than
	// stalling the host's read loop, which serves EVERY stream on that host.
	streamBuffer = 256
	// offlineRetention is how long a disconnected host's last session list is
	// still shown (as offline) before it is dropped from the view.
	offlineRetention = 24 * time.Hour
)

// hostKey names one host: its token's OWNER and its name. Two owners' hosts of
// the same name are two hosts; neither ever sees the other's.
type hostKey struct {
	owner int64
	host  string
}

// HostView is one host as its owner sees it.
//
// A PER-WINDOW host (Windows != nil) lists every tmux window; Sessions, Claude,
// Labels and Activity are then muster's own per-session view of that list
// (SessionsOf) — what a request naming only a session resolves through. A
// PER-SESSION host (an older muster-mirror) has no Windows, and those four are
// what it sent.
type HostView struct {
	Host     string
	Sessions []string
	// Windows is every window the host lists (NormalizeWindows' order); nil for
	// a per-session host.
	Windows []Window
	// Claude is the subset of Sessions whose active pane runs Claude Code (a v2
	// host's report; always empty for a v1 host): the sessions opened in the
	// rendered view by default.
	Claude []string
	// Labels is the host's display label per session (Msg.Labels), restricted
	// to Sessions; a session without one is shown by its name. nil when the
	// host sends none (an older muster-mirror, or no labels configured).
	Labels map[string]string
	// Activity is when each session was last active on the host
	// (Msg.Activity: Unix seconds, floored to ActivityBucket), restricted to
	// Sessions. nil when the host sends none (an older muster-mirror); a
	// session without one sorts after every session with one (Ordered).
	Activity map[string]int64
	// Rendered: the host speaks tunnel v2, so its Claude sessions have a
	// rendered view. False for a v1 host and for an offline one.
	Rendered bool
	// Online: connected now. False is a host that disconnected within
	// offlineRetention; Sessions is then its last list.
	Online bool
	// Interactive: the host advertised -interactive.
	Interactive bool
	// HermesSend: the host advertised Msg.HermesSend (it runs with
	// -hermes-send and is -interactive, v2): a Hermes window's rendered view
	// offers Send and Queue, through the host's gated send (Hermes Phase 3).
	HermesSend bool
	// PanePaste: the host advertised that a paste may target a session's active
	// pane whatever runs there (Msg.PanePaste). False for an older muster-mirror
	// and for an offline host.
	PanePaste bool
	// PaneScroll: the host advertised that it scrolls a window's pane through
	// its history in copy-mode (Msg.PaneScroll). False for an older
	// muster-mirror and for an offline host.
	PaneScroll bool
	// StatusReport: the host advertised that it reports each window's Claude
	// Code status (Msg.StatusReport), which is what a queued message waits on.
	// False for an older muster-mirror and for an offline host.
	StatusReport bool
	// AskAnswer: the host advertised that it types answers into Claude Code's
	// AskUserQuestion picker (Msg.AskAnswer) and is -interactive. False for an
	// older or read-only muster-mirror and for an offline host.
	AskAnswer bool
	// Repos is the short repo name per WINDOW ID the host last sent
	// (TypeRepos, repos.go), restricted to its windows when it arrived; nil when
	// it sent none (an older muster-mirror, a per-session host). Display and
	// search only: Entries copies each window's onto its SessionView.Repo.
	Repos map[string]string
	// OfflineSince is when an offline host disconnected; zero while Online.
	// Its Windows keep the Status they last reported only when it reported
	// status (StatusReport) — the attention queue keeps such a host's waiting
	// windows for a short grace, so a blip neither drops nor re-notifies them.
	OfflineSince time.Time
}

// Hub is the set of connected hosts, their session lists and their live
// terminal streams. It is the plugin's whole state: nothing here is persisted.
//
// ⚠ IT IS IN-PROCESS STATE, like internal/sse's bus: a host is connected to ONE
// muster process, and a terminal opened on another replica cannot reach it. The
// deployment runs one replica, which is what makes this sufficient.
type Hub struct {
	logger *log.Logger

	mu      sync.Mutex
	hosts   map[hostKey]*hostConn
	offline map[hostKey]offlineHost
	// joining is the hosts whose hello was accepted and whose welcome is being
	// written: not in hosts yet (nothing may be sent them before the welcome,
	// see ServeConn), but holding their place under maxHosts.
	joining map[hostKey]int
	// beforeWelcome, when set, runs just before a host's welcome is written
	// (a test holds the welcome there).
	beforeWelcome func()
	notify        func(owner int64)
	// waitingNotify is called with a host's owner whenever the set of its
	// windows whose Claude Code waits on the person changes (SetWaitingNotify).
	waitingNotify func(owner int64)
	now           func() time.Time
	// maxHosts is MaxHosts; a test lowers it.
	maxHosts int
	// debounce coalesces change nudges (notifyDebounce; a test shortens it), and
	// notifyPending is whether one is already scheduled.
	debounce      time.Duration
	notifyPending map[int64]bool
	// activityEvery is the most often ONE OWNER's activity-only changes nudge
	// its open views (activityNudgeEvery; a test shortens it), and activity is
	// that throttle's state per owner. See activityMoved.
	activityEvery time.Duration
	activity      map[int64]*activityThrottle
	// activityAfter is time.AfterFunc for activityMoved's trailing nudge (a
	// test counts the timers it schedules).
	activityAfter func(time.Duration, func()) *time.Timer

	// turns is, per window, its status as first observed and the status time
	// it held at its latest paste (turns.go). Under turnsMu.
	turnsMu sync.Mutex
	turns   map[turnKey]*turnState
}

// activityThrottle is one owner's activity-only throttle: when its last such
// nudge was sent, and the trailing nudge pending for the window's end (nil for
// none). At most ONE is ever pending.
type activityThrottle struct {
	nudged   time.Time
	trailing *time.Timer
}

// notifyDebounce is the window change nudges are coalesced in: however fast
// hosts connect, drop or change their lists, open Agents views are told at most
// once per window.
const notifyDebounce = time.Second

// activityNudgeEvery is the most often one OWNER's ACTIVITY-ONLY changes (a
// card's "active … ago" moved; nothing else did) nudge its open views: once per
// ActivityBucket, the granularity the time is carried at, however many of its
// hosts are busy. A host re-sends whenever a session enters a newer bucket —
// once per bucket for the sessions that stay busy, plus once for each that
// wakes mid-bucket — so without it an owner whose sessions wake at different
// moments re-renders every open page several times a minute. It is per OWNER,
// not per host, because the cost is the owner's open pages: one Agents view
// lists every host of its owner, so a per-host throttle would let 64 busy hosts
// re-render it 64 times a minute.
const activityNudgeEvery = ActivityBucket * time.Second

// ErrTokenRevoked is the cause a connection ends with when its token is
// revoked. Such a host is dropped from the view at once — not kept as offline.
var ErrTokenRevoked = errors.New("mirror: the host's token was revoked")

type offlineHost struct {
	sessions    []string
	windows     []Window
	claude      []string
	labels      map[string]string
	activity    map[string]int64
	repos       map[string]string
	interactive bool
	since       time.Time
}

// NewHub builds an empty hub.
func NewHub(logger *log.Logger) *Hub {
	if logger == nil {
		logger = log.Default()
	}
	return &Hub{logger: logger, hosts: map[hostKey]*hostConn{}, offline: map[hostKey]offlineHost{}, joining: map[hostKey]int{}, now: time.Now, maxHosts: MaxHosts, debounce: notifyDebounce,
		activityEvery: activityNudgeEvery, activity: map[int64]*activityThrottle{}, activityAfter: time.AfterFunc}
}

// SetNotify installs the change nudge (a host connected, disconnected or
// changed its list), called with the OWNER of the host that changed — the one
// user whose Agents view shows it (accounts PR 3b: an SSE event reaches only
// its owner's streams). It is called on its own goroutine.
func (h *Hub) SetNotify(fn func(owner int64)) {
	h.mu.Lock()
	h.notify = fn
	h.mu.Unlock()
}

// SetWaitingNotify installs the hook called (on its own goroutine) with a
// host's owner whenever the set of that host's windows whose Claude Code
// session reports StatusWaiting changes — a change no card draws, so the
// ordinary nudge (SetNotify) does not fire for it.
func (h *Hub) SetWaitingNotify(fn func(owner int64)) {
	h.mu.Lock()
	h.waitingNotify = fn
	h.mu.Unlock()
}

// waitingWindows is the sorted addresses of ws's windows whose agent session
// (any runtime, Window.AgentRuntime) reports StatusWaiting.
func waitingWindows(ws []Window) []string {
	var out []string
	for _, w := range ws {
		if w.AgentRuntime() != "" && w.Status == StatusWaiting {
			k := w.Session + "\x00" + w.ID
			// What it waits on is part of the item: a pending action that
			// arrives (or changes) after the status does still updates the queue.
			if p := w.Pending; p != nil {
				k += "\x00" + p.Summary + "\x00" + p.Tool + "\x00" + p.Last
			}
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// changed schedules ONE nudge for the end of the debounce window, unless one is
// already scheduled: a burst of changes (a flapping host, a session list that
// changes every refresh) costs open views one re-fetch per window, not one per
// change.
//
// The window is PER OWNER: one owner's flapping host cannot delay (or swallow)
// another owner's nudge.
func (h *Hub) changed(owner int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.notify == nil || h.notifyPending[owner] {
		return
	}
	if h.notifyPending == nil {
		h.notifyPending = map[int64]bool{}
	}
	h.notifyPending[owner] = true
	time.AfterFunc(h.debounce, func() {
		h.mu.Lock()
		delete(h.notifyPending, owner)
		fn := h.notify
		h.mu.Unlock()
		if fn != nil {
			fn(owner)
		}
	})
}

type hostConn struct {
	key         hostKey
	ws          *websocket.Conn
	ctx         context.Context
	cancel      context.CancelCauseFunc
	interactive bool
	// panePaste: the host advertised Msg.PanePaste (and speaks v2, where paste
	// exists at all).
	panePaste bool
	// paneScroll: the host advertised Msg.PaneScroll (and speaks v2, and is
	// -interactive).
	paneScroll bool
	// version is the tunnel version this connection speaks (its hello's).
	version int
	// statusReport: the host advertised Msg.StatusReport, speaks v2 and
	// registers per window — the only shape whose Window.Status is believed.
	statusReport bool
	// catalogReport: the host advertised Msg.CatalogReport and speaks v2.
	catalogReport bool
	// subagentReport: the host advertised Msg.SubagentReport and speaks v2.
	subagentReport bool
	// askAnswer: the host advertised Msg.AskAnswer, speaks v2 and is
	// -interactive.
	askAnswer bool
	// hermesSend: the host advertised Msg.HermesSend, speaks v2 and is
	// -interactive — the server half of the hermesSend switch (PasteWindow).
	hermesSend bool

	mu       sync.Mutex
	sessions []string
	// windows is the host's window list; nil for a per-session host. When it is
	// set, sessions / claude / labels / activity are DERIVED from it (list).
	windows  []Window
	claude   []string
	labels   map[string]string
	activity map[string]int64
	// repos is the host's last TypeRepos map (NormalizeRepos over windows).
	repos   map[string]string
	streams map[uint32]*stream
	watches map[uint32]*watch
	pastes  map[uint32]chan error
	// scrolls are the scroll requests waiting for the host's scrolled, by id
	// (the same id space as streams and pastes).
	scrolls map[uint32]chan error
	// catalogs are the catalog requests waiting for the host's cataloged, by
	// id (the same id space).
	catalogs map[uint32]chan catalogAnswer
	// tails are the subagent tail requests waiting for the host's
	// subagent-tailed, by id (the same id space).
	tails map[uint32]chan tailAnswer
	// answers are the AskUserQuestion answers waiting for the host's
	// answered, by id (the same id space).
	answers map[uint32]chan error
	next    uint32
}

type stream struct {
	out  chan []byte
	done chan struct{}
	once sync.Once
	err  error
}

func (st *stream) finish(err error) {
	st.once.Do(func() {
		st.err = err
		close(st.done)
	})
}

// Hosts is every host of owner — connected, or disconnected within
// offlineRetention — sorted by name.
func (h *Hub) Hosts(owner int64) []HostView {
	h.mu.Lock()
	var out []HostView
	for k, hc := range h.hosts {
		if k.owner != owner {
			continue
		}
		hc.mu.Lock()
		out = append(out, HostView{Host: k.host, Sessions: append([]string(nil), hc.sessions...), Windows: slices.Clone(hc.windows),
			Claude: append([]string(nil), hc.claude...), Labels: maps.Clone(hc.labels), Activity: maps.Clone(hc.activity), Repos: maps.Clone(hc.repos), Rendered: hc.version >= 2, Online: true, Interactive: hc.interactive, PanePaste: hc.panePaste, PaneScroll: hc.paneScroll,
			StatusReport: hc.statusReport, AskAnswer: hc.askAnswer, HermesSend: hc.hermesSend})
		hc.mu.Unlock()
	}
	now := h.now()
	for k, o := range h.offline {
		if now.Sub(o.since) > offlineRetention {
			delete(h.offline, k)
			continue
		}
		if k.owner == owner {
			out = append(out, HostView{Host: k.host, Sessions: append([]string(nil), o.sessions...), Windows: slices.Clone(o.windows),
				Claude: append([]string(nil), o.claude...), Labels: maps.Clone(o.labels), Activity: maps.Clone(o.activity), Repos: maps.Clone(o.repos), Interactive: o.interactive,
				OfflineSince: o.since})
		}
	}
	h.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}

// Online reports whether owner's host is connected and lists session.
func (h *Hub) Online(owner int64, host, session string) bool {
	h.mu.Lock()
	hc := h.hosts[hostKey{owner, host}]
	h.mu.Unlock()
	return hc != nil && hc.has(session)
}

func (hc *hostConn) has(session string) bool {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	i := sort.SearchStrings(hc.sessions, session)
	return i < len(hc.sessions) && hc.sessions[i] == session
}

// normalizeClaude is claude restricted to sessions (a host cannot mark a
// session it does not list), deduplicated and sorted.
func normalizeClaude(claude, sessions []string) []string {
	listed := map[string]bool{}
	for _, s := range sessions {
		listed[s] = true
	}
	seen := map[string]bool{}
	out := []string{}
	for _, c := range claude {
		if listed[c] && !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}

// target resolves a request for (session, window) on this host: the window id
// to SEND the host ("" for a per-session host, whose requests name only the
// session) and whether that window's pane runs Claude Code.
//
// window == "" is "the session's ACTIVE window" — every URL from before
// windows were addressable. On a per-window host it is resolved HERE, to an
// explicit id, so the host is never left to decide which window was meant.
func (hc *hostConn) target(session, window string) (id string, claude bool, err error) {
	id, runtime, err := hc.targetRuntime(session, window)
	return id, runtime == RuntimeClaude, err
}

// targetRuntime is target with the agent runtime of the window it resolved to
// (Window.AgentRuntime): RuntimeClaude, RuntimeHermes or "" for none. A
// per-session host reports Claude Code only (Msg.Claude), never Hermes.
func (hc *hostConn) targetRuntime(session, window string) (id, runtime string, err error) {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	i := sort.SearchStrings(hc.sessions, session)
	listed := i < len(hc.sessions) && hc.sessions[i] == session
	if !listed {
		return "", "", fmt.Errorf("%w (host %q, session %q)", ErrSessionGone, hc.key.host, session)
	}
	if hc.windows == nil {
		if window != "" {
			return "", "", fmt.Errorf("%w (host %q registers per session; session %q, window %q)", ErrWindowGone, hc.key.host, session, window)
		}
		if j := sort.SearchStrings(hc.claude, session); j < len(hc.claude) && hc.claude[j] == session {
			return "", RuntimeClaude, nil
		}
		return "", "", nil
	}
	if window == "" {
		w, ok := ActiveWindow(hc.windows, session)
		if !ok {
			return "", "", fmt.Errorf("%w (host %q, session %q)", ErrSessionGone, hc.key.host, session)
		}
		return w.ID, w.AgentRuntime(), nil
	}
	for _, w := range hc.windows {
		if w.Session == session && w.ID == window {
			return w.ID, w.AgentRuntime(), nil
		}
	}
	return "", "", fmt.Errorf("%w (host %q, session %q, window %q)", ErrWindowGone, hc.key.host, session, window)
}

// list is what a hello or a sessions message registers: the window list when
// the host sent one (and then muster's OWN per-session view of it), else the
// per-session fields as sent. Each through its normalizer; nothing of the
// host's is kept unchecked.
func (hc *hostConn) list(m Msg, now int64) (sessions []string, windows []Window, claude []string, labels map[string]string, activity map[string]int64) {
	if windows = NormalizeWindows(m.Windows, now); windows != nil {
		if hc.version < 2 {
			// A v1 host has no rendered view whatever it claims.
			for i := range windows {
				windows[i].Claude, windows[i].Status, windows[i].StatusSince, windows[i].ClaudeSession, windows[i].Pending = false, "", 0, "", nil
				windows[i].Runtime, windows[i].AgentSession = "", ""
			}
		}
		sessions, claude, labels, activity = SessionsOf(windows)
		if claude == nil {
			claude = []string{}
		}
		return sessions, windows, claude, labels, activity
	}
	sessions = normalizeSessions(m.Sessions)
	if hc.version >= 2 {
		claude = normalizeClaude(m.Claude, sessions)
	}
	return sessions, nil, claude, NormalizeLabels(m.Labels, sessions), NormalizeActivity(m.Activity, sessions, now)
}

// sameWindows reports whether a and b are the same windows in everything but
// Activity. Activity is compared on its own, as the card prints it (listed):
// it moves all the time, and a change in it is shown — in the card's "active …
// ago" — but is throttled (activityMoved) rather than nudged at once.
// Status and StatusSince are ignored too: no card draws them and they move
// with every turn (the send queue reads them, through AgentState).
func sameWindows(a, b []Window) bool {
	if (a == nil) != (b == nil) || len(a) != len(b) {
		return false
	}
	// OtherClaude is ignored too: no card draws it (the MCP door reads it). So
	// is Pending: only the attention queue draws it (waitingWindows nudges it).
	card := func(w Window) Window {
		w.Activity, w.Status, w.StatusSince, w.OtherClaude, w.Pending = 0, "", 0, nil, nil
		return w
	}
	for i := range a {
		if !card(a[i]).Equal(card(b[i])) {
			return false
		}
	}
	return true
}

// activityMoved nudges owner's open views for an ACTIVITY-ONLY change, at most
// once per activityEvery per owner. One inside the window is not dropped: it
// schedules a trailing nudge for the window's end, so the last change of a
// burst still reaches the cards — and only ONE, however many changes the
// window holds: a change that finds one pending is already covered by it. The
// trailing nudge stamps the window it opens, so a change just after it waits
// for that window's end too. A disconnect cancels it (drop): the disconnect
// nudges the owner's views itself, and a timer must not outlive what it is for.
func (h *Hub) activityMoved(owner int64) {
	h.mu.Lock()
	now := h.now()
	t := h.activity[owner]
	if t == nil {
		t = &activityThrottle{}
		h.activity[owner] = t
	}
	if t.trailing != nil {
		h.mu.Unlock()
		return
	}
	wait := t.nudged.Add(h.activityEvery).Sub(now)
	if wait <= 0 {
		t.nudged = now
		h.mu.Unlock()
		h.changed(owner)
		return
	}
	var timer *time.Timer
	timer = h.activityAfter(wait, func() {
		h.mu.Lock()
		cur := h.activity[owner]
		if cur == nil || cur.trailing != timer {
			// Cancelled (drop) after it had already started.
			h.mu.Unlock()
			return
		}
		cur.trailing = nil
		cur.nudged = h.now()
		h.mu.Unlock()
		h.changed(owner)
	})
	t.trailing = timer
	h.mu.Unlock()
}

// normalizeSessions drops invalid and duplicate names, sorts, and caps the list.
func normalizeSessions(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if ValidSession(s) != nil || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	if len(out) > MaxSessions {
		out = out[:MaxSessions]
	}
	return out
}

// ServeConn runs one host's connection until it ends, and returns why.
//
// 🔴 owner AND host ARE THE TOKEN'S, DECIDED BY THE CALLER BEFORE THE UPGRADE,
// AND THE HELLO MUST NAME THE SAME HOST. The hello's own host field is never
// trusted to pick where sessions land: a token minted for host A that says
// hello as host B is refused here, before anything is registered.
//
// A newer connection for the same (owner, host) replaces the older one (a
// restarted muster-mirror whose previous TCP connection has not timed out yet).
// A NEW host beyond MaxHosts is refused.
//
// Registration is in memory only: nothing on this path waits on a database.
func (h *Hub) ServeConn(ctx context.Context, owner int64, host string, ws *websocket.Conn) error {
	ws.SetReadLimit(MaxFrame)
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	hello, err := readHello(ctx, ws)
	if err == nil && hello.Host != host {
		err = fmt.Errorf("mirror: this token was minted for host %q and cannot register host %q", host, hello.Host)
	}
	if err != nil {
		refuse(ctx, ws, err)
		return err
	}

	key := hostKey{owner, host}
	hc := &hostConn{key: key, ws: ws, ctx: ctx, cancel: cancel, interactive: hello.Interactive, version: hello.Version,
		streams: map[uint32]*stream{}, watches: map[uint32]*watch{}, pastes: map[uint32]chan error{}, scrolls: map[uint32]chan error{},
		catalogs: map[uint32]chan catalogAnswer{}, answers: map[uint32]chan error{}, tails: map[uint32]chan tailAnswer{}}
	hc.sessions, hc.windows, hc.claude, hc.labels, hc.activity = hc.list(hello, h.now().Unix())
	h.observe(owner, host, hc.windows, h.now())
	if hc.version >= 2 {
		// A read-only host cannot be pasted into whatever it claims.
		hc.panePaste = hello.PanePaste && hello.Interactive
		// Nor scrolled: copy-mode is a change to the pane its operator sees,
		// which a read-only host did not opt into.
		hc.paneScroll = hello.PaneScroll && hello.Interactive
		// Believed only of a window (AgentState requires a per-window list).
		hc.statusReport = hello.StatusReport
		// Read-only or not: the catalog is names and descriptions shown to the
		// host's own owner, never a write into the pane.
		hc.catalogReport = hello.CatalogReport
		// Like the catalog: a read of the host's own transcripts, shown to its
		// owner.
		hc.subagentReport = hello.SubagentReport
		// Typing an answer is a write into the pane: -interactive hosts only.
		hc.askAnswer = hello.AskAnswer && hello.Interactive
		// The hermesSend switch: a paste into a Hermes window is a write into
		// the pane like any other, so -interactive hosts only.
		hc.hermesSend = hello.HermesSend && hello.Interactive
	}
	// 🔴 THE WELCOME IS THE FIRST FRAME THE HOST READS, SO hc IS NOT IN h.hosts
	// UNTIL IT HAS BEEN WRITTEN. Everything that sends a host a request (an
	// open, a watch, a paste…) finds it in h.hosts and writes on this same
	// socket; registered before the welcome, an attach that raced it put its
	// open FIRST, the host read that as "muster's first message was not a
	// welcome" and hung up — the terminal ended before it showed anything (the
	// tmuxit TestTmuxThroughTheTunnel flake). The place under maxHosts is held
	// meanwhile (joining), so the cap is decided BEFORE the welcome, as before.
	h.mu.Lock()
	if h.hosts[key] == nil && h.joining[key] == 0 && len(h.hosts)+h.joiningNew() >= h.maxHosts {
		h.mu.Unlock()
		refuse(ctx, ws, ErrTooManyHosts)
		return ErrTooManyHosts
	}
	h.joining[key]++
	h.mu.Unlock()
	joined := false
	defer func() {
		if !joined {
			h.mu.Lock()
			h.leaveJoining(key)
			h.mu.Unlock()
		}
	}()

	if h.beforeWelcome != nil {
		h.beforeWelcome()
	}
	// The welcome names the version this connection SPEAKS: the host's own,
	// which readHello accepted. A v1 host is never sent anything newer.
	if err := writeJSON(ctx, ws, Msg{Type: TypeWelcome, Version: hc.version, Host: host}); err != nil {
		return err
	}

	h.mu.Lock()
	old := h.hosts[key]
	h.hosts[key] = hc
	delete(h.offline, key)
	h.leaveJoining(key)
	joined = true
	h.mu.Unlock()
	if old != nil {
		old.cancel(errors.New("mirror: replaced by a newer connection from the same host"))
	}
	defer h.drop(hc)
	mode := "read-only"
	if hc.interactive {
		mode = "interactive"
	}
	if hc.windows != nil {
		h.logger.Printf("mirror: host %s (owner %d) connected, %s, tunnel v%d, with %d window(s) in %d session(s)", host, owner, mode, hc.version, len(hc.windows), len(hc.sessions))
	} else {
		h.logger.Printf("mirror: host %s (owner %d) connected, %s, tunnel v%d, with %d session(s) (per session: its muster-mirror predates per-window registration)", host, owner, mode, hc.version, len(hc.sessions))
	}
	h.changed(owner)

	go func() {
		t := time.NewTicker(pingInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				pctx, pcancel := context.WithTimeout(ctx, writeTimeout)
				err := ws.Ping(pctx)
				pcancel()
				if err != nil {
					cancel(fmt.Errorf("mirror: keepalive: %w", err))
					return
				}
			}
		}
	}()

	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
				err = cause
			}
			_ = ws.Close(websocket.StatusGoingAway, truncate(err.Error()))
			return err
		}
		switch typ {
		case websocket.MessageBinary:
			id, p, err := DecodeData(data)
			if err != nil {
				continue
			}
			hc.deliver(id, p)
		case websocket.MessageText:
			var m Msg
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			switch m.Type {
			case TypeSessions:
				// Labels and activity ride every sessions message, so one that
				// changed with the same session set (the active window moved, a
				// pane produced output) still lands here.
				list, windows, claude, labels, activity := hc.list(m, h.now().Unix())
				order, shown := listed(h.Hosts(hc.key.owner))
				hc.mu.Lock()
				same := slices.Equal(hc.sessions, list) && slices.Equal(hc.claude, claude) && maps.Equal(hc.labels, labels) && sameWindows(hc.windows, windows)
				waitingMoved := !slices.Equal(waitingWindows(hc.windows), waitingWindows(windows))
				hc.sessions, hc.windows, hc.labels, hc.activity, hc.claude = list, windows, labels, activity, claude
				hc.mu.Unlock()
				// 🔴 A STATUS-ONLY CHANGE NUDGES NO CARD (sameWindows ignores
				// Status), so it gets its own hook: the attention queue must hear
				// that a window started — or stopped — waiting on the person.
				if waitingMoved {
					h.mu.Lock()
					fn := h.waitingNotify
					h.mu.Unlock()
					if fn != nil {
						go fn(hc.key.owner)
					}
				}
				h.observe(hc.key.owner, hc.key.host, windows, h.now())
				// 🔴 ONLY A CHANGE SOMEBODY CAN SEE NUDGES THE OPEN VIEWS, and an
				// activity-only one is THROTTLED. A changed list or order nudges
				// at once. A time that moved with nothing else (a busy window that
				// stays where it is) still changes that card's "active … ago" — a
				// view never told would show it going stale while the window works
				// — so it nudges too, but at most once per activityEvery per owner
				// (activityNudgeEvery says why). Both are read off the list the
				// view renders (listed), never off the raw message.
				orderAfter, shownAfter := listed(h.Hosts(hc.key.owner))
				switch {
				case !same || !slices.Equal(order, orderAfter):
					h.changed(hc.key.owner)
				case !slices.Equal(shown, shownAfter):
					h.activityMoved(hc.key.owner)
				}
			case TypeRepos:
				// The window list's repo names (repos.go): a v2 host only,
				// restricted to the windows it lists now. A change is one a card
				// draws (its chip), so it nudges the open views.
				if hc.version >= 2 {
					hc.mu.Lock()
					repos := NormalizeRepos(m.Repos, hc.windows)
					same := maps.Equal(hc.repos, repos)
					hc.repos = repos
					hc.mu.Unlock()
					if !same {
						h.changed(hc.key.owner)
					}
				}
			case TypeEvents:
				if hc.version >= 2 {
					hc.deliverEvents(m)
				}
			case TypePasted:
				if hc.version >= 2 {
					hc.pasted(m.ID, m.Error, m.Refusal)
				}
			case TypeScrolled:
				if hc.version >= 2 {
					hc.scrolled(m.ID, m.Error, m.Refusal)
				}
			case TypeCataloged:
				if hc.version >= 2 {
					hc.cataloged(m)
				}
			case TypeSubagentTailed:
				if hc.version >= 2 {
					hc.subagentTailed(m)
				}
			case TypeAnswered:
				if hc.version >= 2 {
					hc.answered(m.ID, m.Error, m.Refusal)
				}
			case TypeClosed:
				var why error
				if m.Error != "" {
					why = fmt.Errorf("mirror: the host ended the terminal: %s", m.Error)
				}
				hc.finish(m.ID, why)
			}
		}
	}
}

// joiningNew is how many hosts are joining that are not registered already (a
// reconnect holds no new place). Under h.mu.
func (h *Hub) joiningNew() int {
	n := 0
	for k := range h.joining {
		if h.hosts[k] == nil {
			n++
		}
	}
	return n
}

// leaveJoining ends one joining connection of key. Under h.mu.
func (h *Hub) leaveJoining(key hostKey) {
	if h.joining[key]--; h.joining[key] <= 0 {
		delete(h.joining, key)
	}
}

// drop removes hc (if it is still the host's current connection), remembers its
// last list as offline (unless its token was revoked), and ends every stream on it with ErrHostDisconnected.
// It runs for EVERY ending of ServeConn — read error, revocation, replacement —
// so it is the one place a disconnect ends a terminal.
func (h *Hub) drop(hc *hostConn) {
	revoked := errors.Is(context.Cause(hc.ctx), ErrTokenRevoked)
	h.mu.Lock()
	current := h.hosts[hc.key] == hc
	if current {
		delete(h.hosts, hc.key)
		// The disconnect nudges the owner's views below, which re-fetches every
		// card's time: a pending trailing activity nudge has nothing left to do,
		// so it is stopped, and the owner's throttle starts afresh (at most one
		// extra activity nudge per disconnect, which already costs one).
		if t := h.activity[hc.key.owner]; t != nil {
			if t.trailing != nil {
				t.trailing.Stop()
			}
			delete(h.activity, hc.key.owner)
		}
		if revoked {
			// A revoked host is GONE, not offline: nothing it listed stays on
			// anyone's view.
			delete(h.offline, hc.key)
			defer h.forgetTurns(hc.key.owner, hc.key.host)
		} else {
			hc.mu.Lock()
			claude := append([]string(nil), hc.claude...)
			labels := maps.Clone(hc.labels)
			activity := maps.Clone(hc.activity)
			windows := slices.Clone(hc.windows)
			repos := maps.Clone(hc.repos)
			if !hc.statusReport {
				// A status is believed only from a host that reports it: what an
				// offline host's last list keeps is what it was believed about.
				for i := range windows {
					windows[i].Status, windows[i].StatusSince, windows[i].Pending = "", 0, nil
				}
			}
			hc.mu.Unlock()
			h.offline[hc.key] = offlineHost{sessions: hc.sessionsCopy(), windows: windows, claude: claude, labels: labels, activity: activity, repos: repos, interactive: hc.interactive, since: h.now()}
		}
	}
	h.mu.Unlock()
	hc.mu.Lock()
	for id, st := range hc.streams {
		st.finish(ErrHostDisconnected)
		delete(hc.streams, id)
	}
	for id, w := range hc.watches {
		w.finish(ErrHostDisconnected)
		delete(hc.watches, id)
	}
	for id, ch := range hc.pastes {
		ch <- ErrHostDisconnected
		delete(hc.pastes, id)
	}
	for id, ch := range hc.scrolls {
		ch <- ErrHostDisconnected
		delete(hc.scrolls, id)
	}
	for id, ch := range hc.catalogs {
		ch <- catalogAnswer{err: ErrHostDisconnected}
		delete(hc.catalogs, id)
	}
	for id, ch := range hc.answers {
		ch <- ErrHostDisconnected
		delete(hc.answers, id)
	}
	for id, ch := range hc.tails {
		ch <- tailAnswer{err: ErrHostDisconnected}
		delete(hc.tails, id)
	}
	hc.mu.Unlock()
	if current {
		h.logger.Printf("mirror: host %s (owner %d) disconnected", hc.key.host, hc.key.owner)
		h.changed(hc.key.owner)
	}
}

func (hc *hostConn) sessionsCopy() []string {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	return append([]string(nil), hc.sessions...)
}

func (hc *hostConn) deliver(id uint32, p []byte) {
	hc.mu.Lock()
	st := hc.streams[id]
	hc.mu.Unlock()
	if st == nil {
		return
	}
	select {
	case st.out <- append([]byte(nil), p...):
	default:
		st.finish(ErrOverrun)
	}
}

// finish ends stream id — a terminal or a transcript watch, which share the id
// space — with err.
func (hc *hostConn) finish(id uint32, err error) {
	hc.mu.Lock()
	st := hc.streams[id]
	w := hc.watches[id]
	hc.mu.Unlock()
	if st != nil {
		st.finish(err)
	}
	if w != nil {
		w.finish(err)
	}
}

func readHello(ctx context.Context, ws *websocket.Conn) (Msg, error) {
	rctx, cancel := context.WithTimeout(ctx, helloTimeout)
	defer cancel()
	typ, data, err := ws.Read(rctx)
	if err != nil {
		return Msg{}, fmt.Errorf("mirror: no hello: %w", err)
	}
	var m Msg
	if typ != websocket.MessageText || json.Unmarshal(data, &m) != nil || m.Type != TypeHello {
		return Msg{}, errors.New("mirror: the first message must be a hello")
	}
	if m.Version < MinProtocolVersion || m.Version > ProtocolVersion {
		return Msg{}, fmt.Errorf("mirror: this muster speaks tunnel versions %d-%d and the host sent %d; upgrade the older side", MinProtocolVersion, ProtocolVersion, m.Version)
	}
	if err := ValidHost(m.Host); err != nil {
		return Msg{}, err
	}
	return m, nil
}

// refuse tells the host why, then closes. The host logs the message, so an
// operator reading muster-mirror's output sees the reason rather than a reset.
func refuse(ctx context.Context, ws *websocket.Conn, err error) {
	_ = writeJSON(ctx, ws, Msg{Type: TypeWelcome, Error: err.Error()})
	_ = ws.Close(websocket.StatusPolicyViolation, truncate(err.Error()))
}

// truncate fits a close reason into a control frame (123 bytes).
func truncate(s string) string {
	if len(s) > 120 {
		return s[:120]
	}
	return s
}

func writeJSON(ctx context.Context, ws *websocket.Conn, m Msg) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return ws.Write(wctx, websocket.MessageText, b)
}

// Attach opens a terminal on owner's host's session and runs it until ctx ends,
// the host ends it, or the host's connection drops: what is written to stdin is
// typed (never, when readOnly), what the session shows arrives on stdout, and
// each size on resize is applied. It has provision.Provisioner's terminal shape,
// so internal/api's one terminal bridge serves both.
//
// 🔴 owner IS THE VIEWING PRINCIPAL'S USER, AND ANOTHER OWNER'S HOST IS NOT
// THERE AT ALL: the lookup is keyed by (owner, host), so it answers
// ErrHostOffline exactly as for a host that does not exist.
//
// 🔴 READ-ONLY IS ENFORCED HERE AS WELL AS ON EITHER SIDE OF IT. internal/api
// forwards no input to a read-only terminal; this drops stdin for a read-only
// stream without sending a byte, and refuses an interactive one outright when
// the host did not advertise -interactive; and the host attaches `tmux attach
// -r`, drops input for a read-only stream and refuses an interactive open it did
// not advertise. No single layer being wrong types into an operator's session.
//
// It returns nil when the host reports the session's client exited cleanly, and
// when ctx ends (the browser left).
func (h *Hub) Attach(ctx context.Context, owner int64, host, session string, readOnly bool, stdin io.Reader, stdout io.Writer, resize <-chan provision.TermSize) error {
	return h.AttachWindow(ctx, owner, host, session, "", readOnly, stdin, stdout, resize)
}

// AttachWindow is Attach on one WINDOW of the session (its tmux window id), or
// — window == "" — on the session's active window.
//
// 🔴 ON A PER-WINDOW HOST THE OPEN ALWAYS NAMES A WINDOW ID, resolved here even
// when the caller named only the session (hostConn.target). The host then shows
// that window through a viewer session of its own (internal/mirrorhost) and
// never through the operator's session, so neither viewing nor typing into a
// window moves what the operator is looking at.
func (h *Hub) AttachWindow(ctx context.Context, owner int64, host, session, window string, readOnly bool, stdin io.Reader, stdout io.Writer, resize <-chan provision.TermSize) error {
	h.mu.Lock()
	hc := h.hosts[hostKey{owner, host}]
	h.mu.Unlock()
	if hc == nil {
		return fmt.Errorf("%w (host %q)", ErrHostOffline, host)
	}
	windowID, _, err := hc.target(session, window)
	if err != nil {
		return err
	}
	if !readOnly && !hc.interactive {
		return fmt.Errorf("%w (host %q)", ErrInteractiveRefused, host)
	}

	hc.mu.Lock()
	if len(hc.streams) >= MaxStreamsPerHost {
		hc.mu.Unlock()
		return fmt.Errorf("%w (host %q)", ErrTooManyStreams, host)
	}
	hc.next++
	id := hc.next
	st := &stream{out: make(chan []byte, streamBuffer), done: make(chan struct{})}
	hc.streams[id] = st
	hc.mu.Unlock()
	defer func() {
		hc.mu.Lock()
		delete(hc.streams, id)
		hc.mu.Unlock()
	}()

	open := Msg{Type: TypeOpen, ID: id, Session: session, Window: windowID, ReadOnly: readOnly}
	select {
	case sz := <-resize:
		open.Cols, open.Rows = sz.Cols, sz.Rows
	default:
	}
	if err := writeJSON(ctx, hc.ws, open); err != nil {
		return fmt.Errorf("mirror: ask the host to open the terminal: %w", err)
	}
	// Tell the host to end the stream whatever ends it here. On the host's own
	// connection context, so a browser that already left still sends it.
	defer func() { _ = writeJSON(hc.ctx, hc.ws, Msg{Type: TypeClose, ID: id}) }()

	actx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := stdin.Read(buf)
			if n > 0 && !readOnly {
				wctx, wcancel := context.WithTimeout(actx, writeTimeout)
				werr := hc.ws.Write(wctx, websocket.MessageBinary, EncodeData(id, buf[:n]))
				wcancel()
				if werr != nil {
					cancel()
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	for {
		select {
		case <-actx.Done():
			return nil
		case <-st.done:
			// Output the host sent before it ended the stream is still owed.
			for {
				select {
				case p := <-st.out:
					if _, err := stdout.Write(p); err != nil {
						return err
					}
				default:
					return st.err
				}
			}
		case p := <-st.out:
			if _, err := stdout.Write(p); err != nil {
				return err
			}
		case sz := <-resize:
			if err := writeJSON(actx, hc.ws, Msg{Type: TypeResize, ID: id, Cols: sz.Cols, Rows: sz.Rows}); err != nil {
				return fmt.Errorf("mirror: resize: %w", err)
			}
		}
	}
}
