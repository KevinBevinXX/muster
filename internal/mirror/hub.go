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
type HostView struct {
	Host     string
	Sessions []string
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
	notify  func(owner int64)
	now     func() time.Time
	// maxHosts is MaxHosts; a test lowers it.
	maxHosts int
	// debounce coalesces change nudges (notifyDebounce; a test shortens it), and
	// notifyPending is whether one is already scheduled.
	debounce      time.Duration
	notifyPending map[int64]bool
}

// notifyDebounce is the window change nudges are coalesced in: however fast
// hosts connect, drop or change their lists, open Agents views are told at most
// once per window.
const notifyDebounce = time.Second

// ErrTokenRevoked is the cause a connection ends with when its token is
// revoked. Such a host is dropped from the view at once — not kept as offline.
var ErrTokenRevoked = errors.New("mirror: the host's token was revoked")

type offlineHost struct {
	sessions    []string
	claude      []string
	labels      map[string]string
	activity    map[string]int64
	interactive bool
	since       time.Time
}

// NewHub builds an empty hub.
func NewHub(logger *log.Logger) *Hub {
	if logger == nil {
		logger = log.Default()
	}
	return &Hub{logger: logger, hosts: map[hostKey]*hostConn{}, offline: map[hostKey]offlineHost{}, now: time.Now, maxHosts: MaxHosts, debounce: notifyDebounce}
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
	// version is the tunnel version this connection speaks (its hello's).
	version int

	mu       sync.Mutex
	sessions []string
	claude   []string
	labels   map[string]string
	activity map[string]int64
	streams  map[uint32]*stream
	watches  map[uint32]*watch
	pastes   map[uint32]chan error
	next     uint32
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
	defer h.mu.Unlock()
	var out []HostView
	for k, hc := range h.hosts {
		if k.owner != owner {
			continue
		}
		hc.mu.Lock()
		out = append(out, HostView{Host: k.host, Sessions: append([]string(nil), hc.sessions...),
			Claude: append([]string(nil), hc.claude...), Labels: maps.Clone(hc.labels), Activity: maps.Clone(hc.activity), Rendered: hc.version >= 2, Online: true, Interactive: hc.interactive})
		hc.mu.Unlock()
	}
	now := h.now()
	for k, o := range h.offline {
		if now.Sub(o.since) > offlineRetention {
			delete(h.offline, k)
			continue
		}
		if k.owner == owner {
			out = append(out, HostView{Host: k.host, Sessions: append([]string(nil), o.sessions...),
				Claude: append([]string(nil), o.claude...), Labels: maps.Clone(o.labels), Activity: maps.Clone(o.activity), Interactive: o.interactive})
		}
	}
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

// isClaude reports whether session's pane runs Claude Code, per the host.
func (hc *hostConn) isClaude(session string) bool {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	i := sort.SearchStrings(hc.claude, session)
	return i < len(hc.claude) && hc.claude[i] == session
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
	sessions := normalizeSessions(hello.Sessions)
	hc := &hostConn{key: key, ws: ws, ctx: ctx, cancel: cancel, interactive: hello.Interactive, version: hello.Version,
		sessions: sessions, labels: NormalizeLabels(hello.Labels, sessions), activity: NormalizeActivity(hello.Activity, sessions, h.now().Unix()), streams: map[uint32]*stream{}, watches: map[uint32]*watch{}, pastes: map[uint32]chan error{}}
	if hc.version >= 2 {
		hc.claude = normalizeClaude(hello.Claude, sessions)
	}
	h.mu.Lock()
	old := h.hosts[key]
	if old == nil && len(h.hosts) >= h.maxHosts {
		h.mu.Unlock()
		refuse(ctx, ws, ErrTooManyHosts)
		return ErrTooManyHosts
	}
	h.hosts[key] = hc
	delete(h.offline, key)
	h.mu.Unlock()
	if old != nil {
		old.cancel(errors.New("mirror: replaced by a newer connection from the same host"))
	}
	defer h.drop(hc)

	// The welcome names the version this connection SPEAKS: the host's own,
	// which readHello accepted. A v1 host is never sent anything newer.
	if err := writeJSON(ctx, ws, Msg{Type: TypeWelcome, Version: hc.version, Host: host}); err != nil {
		return err
	}
	mode := "read-only"
	if hc.interactive {
		mode = "interactive"
	}
	h.logger.Printf("mirror: host %s (owner %d) connected, %s, tunnel v%d, with %d session(s)", host, owner, mode, hc.version, len(hc.sessions))
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
				list := normalizeSessions(m.Sessions)
				// Labels and activity ride every sessions message, so one that
				// changed with the same session set (the active window moved, a
				// pane produced output) still lands here.
				labels := NormalizeLabels(m.Labels, list)
				activity := NormalizeActivity(m.Activity, list, h.now().Unix())
				var claude []string
				if hc.version >= 2 {
					claude = normalizeClaude(m.Claude, list)
				}
				before := orderKeys(h.Hosts(hc.key.owner))
				hc.mu.Lock()
				same := slices.Equal(hc.sessions, list) && slices.Equal(hc.claude, claude) && maps.Equal(hc.labels, labels)
				hc.sessions, hc.labels, hc.activity, hc.claude = list, labels, activity, claude
				hc.mu.Unlock()
				// 🔴 ONLY A CHANGE SOMEBODY CAN SEE NUDGES THE OPEN VIEWS. Activity
				// moves all the time; a host re-sends it once per ActivityBucket
				// while a session stays busy, and most of those leave the list in
				// the same order. Re-rendering the Agents view for each would move
				// nothing and still cost every open page a re-fetch.
				if !same || !slices.Equal(before, orderKeys(h.Hosts(hc.key.owner))) {
					h.changed(hc.key.owner)
				}
			case TypeEvents:
				if hc.version >= 2 {
					hc.deliverEvents(m)
				}
			case TypePasted:
				if hc.version >= 2 {
					hc.pasted(m.ID, m.Error)
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
		if revoked {
			// A revoked host is GONE, not offline: nothing it listed stays on
			// anyone's view.
			delete(h.offline, hc.key)
		} else {
			hc.mu.Lock()
			claude := append([]string(nil), hc.claude...)
			labels := maps.Clone(hc.labels)
			activity := maps.Clone(hc.activity)
			hc.mu.Unlock()
			h.offline[hc.key] = offlineHost{sessions: hc.sessionsCopy(), claude: claude, labels: labels, activity: activity, interactive: hc.interactive, since: h.now()}
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
	h.mu.Lock()
	hc := h.hosts[hostKey{owner, host}]
	h.mu.Unlock()
	if hc == nil {
		return fmt.Errorf("%w (host %q)", ErrHostOffline, host)
	}
	if !hc.has(session) {
		return fmt.Errorf("%w (host %q, session %q)", ErrSessionGone, host, session)
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

	open := Msg{Type: TypeOpen, ID: id, Session: session, ReadOnly: readOnly}
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
