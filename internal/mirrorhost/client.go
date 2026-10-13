// Package mirrorhost is the HOST half of mirrored sessions: what `muster-mirror`
// runs on an operator's own machine. It lists the machine's tmux sessions,
// dials OUT to muster (the server half is internal/mirror), registers them, and
// serves the terminal attaches muster asks for over that one connection.
//
// 🔴 IT OPENS NO LISTENER. Everything arrives on the connection this process
// dialled, authenticated by this host's own mirror token — the only muster
// credential it ever holds.
package mirrorhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/mirror"
)

// Backend is where sessions come from: the host's tmux (Tmux), or a test's fake.
type Backend interface {
	// List is the current session names.
	List(ctx context.Context) ([]string, error)
	// Attach starts a client on session at the given size (0 = backend default).
	Attach(ctx context.Context, session string, readOnly bool, cols, rows uint16) (Terminal, error)
}

// Terminal is one attached client: Read is its screen, Write its keyboard.
// Close ends the client and never the session.
type Terminal interface {
	io.ReadWriteCloser
	Resize(cols, rows uint16) error
}

// Config is one muster-mirror's configuration.
type Config struct {
	// URL is muster's base URL (https://muster.example.com, or a path prefix).
	URL string
	// Token is this host's mirror token (mirror.TokenPrefix…).
	Token string
	// Host is this host's name; it must be the host the token was minted for.
	Host    string
	Backend Backend
	// Refresh is how often the session list is re-read (default 5s); a change is
	// sent at once.
	Refresh time.Duration
	// ViewPoll is how often an open rendered view's transcript is checked for
	// new lines (default 300ms): what "live while a turn runs" costs — one stat
	// per open view per tick.
	ViewPoll time.Duration
	// Interactive lets muster open INTERACTIVE terminals on this host. It is OFF
	// by default (muster-mirror's -interactive flag): a host that did not opt in
	// is read-only — it advertises so in its hello, so muster offers no
	// Interactive switch and refuses a rw attach, and this side refuses one too.
	//
	// 🔴 INTERACTIVE IS A SHELL ON THIS HOST. An interactive tmux client can
	// switch to any session on the server or open a new window, so whoever can
	// use muster's Interactive switch for this host can run commands here as the
	// user muster-mirror runs as. Turn it on only where that is intended.
	Interactive bool
	// Codenames is session name → codename (LoadCodenames). A listed session
	// with one is shown in muster as "<codename>:<active window index>"
	// (BuildLabels); nil labels nothing, and then no window index is read.
	Codenames map[string]string
	// AllowHTTP permits a plain http:// URL — FOR TESTS AND LOCAL DEVELOPMENT
	// ONLY; the documented install is https. Without it only https is accepted:
	// the token travels in the Authorization header of every connect.
	AllowHTTP bool
	Logger    *log.Logger
}

// ErrUnauthorized: muster refused the token (unknown, revoked, or not a mirror
// token). Run keeps retrying at the slowest backoff, so a token re-issued under
// the same id is picked up, but every attempt is logged.
var ErrUnauthorized = errors.New("muster refused this mirror token (unknown or revoked)")

// ConnectURL is the WebSocket URL for base: http→ws, https→wss, plus
// mirror.ConnectPath under any path prefix base carries.
func ConnectURL(base string, allowHTTP bool) (string, error) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil {
		return "", fmt.Errorf("muster-mirror: bad muster URL %q: %w", base, err)
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		if !allowHTTP {
			return "", fmt.Errorf("muster-mirror: %q is plain http, which would send the mirror token in cleartext; use https (-allow-http exists for tests and local development only)", base)
		}
		u.Scheme = "ws"
	default:
		return "", fmt.Errorf("muster-mirror: muster URL %q must be https:// (or http:// with -allow-http)", base)
	}
	if u.Host == "" {
		return "", fmt.Errorf("muster-mirror: muster URL %q has no host", base)
	}
	u.Path = strings.TrimRight(u.Path, "/") + mirror.ConnectPath
	u.RawQuery, u.Fragment = "", ""
	return u.String(), nil
}

// Client runs one host's connection to muster.
type Client struct {
	cfg Config
	url string
	log *log.Logger
}

// New validates cfg and builds a client.
func New(cfg Config) (*Client, error) {
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	if cfg.Refresh <= 0 {
		cfg.Refresh = 5 * time.Second
	}
	if cfg.ViewPoll <= 0 {
		cfg.ViewPoll = 300 * time.Millisecond
	}
	if cfg.Backend == nil {
		return nil, errors.New("muster-mirror: no backend")
	}
	if err := mirror.ValidHost(cfg.Host); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(cfg.Token, mirror.TokenPrefix) {
		return nil, fmt.Errorf("muster-mirror: the token does not start with %q, so it is not a mirror token; mint one with `muster-server mirror token create %s`", mirror.TokenPrefix, cfg.Host)
	}
	u, err := ConnectURL(cfg.URL, cfg.AllowHTTP)
	if err != nil {
		return nil, err
	}
	return &Client{cfg: cfg, url: u, log: cfg.Logger}, nil
}

// Run connects, and reconnects with jittered exponential backoff (1s → 60s),
// until ctx ends. A connection that stayed up for a minute resets the backoff.
func (c *Client) Run(ctx context.Context) error {
	backoff := time.Second
	for {
		start := time.Now()
		err := c.RunOnce(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		if errors.Is(err, ErrUnauthorized) {
			backoff = time.Minute
		}
		wait := backoff/2 + rand.N(backoff/2+1)
		c.log.Printf("muster-mirror: disconnected (%v); reconnecting in %s", err, wait.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

// RunOnce makes one connection and serves it until it ends.
//
// It speaks the newest tunnel version (mirror.ProtocolVersion) and, when a v1
// muster refuses that with its version error, reconnects AT ONCE as v1: the
// sessions and terminals work as before, with no rendered view, instead of
// the host staying offline until muster is upgraded. The next connection tries
// the newest version again, so an upgraded muster is picked up.
func (c *Client) RunOnce(ctx context.Context) error {
	err := c.runOnce(ctx, mirror.ProtocolVersion)
	var vr *versionRefusal
	if errors.As(err, &vr) && ctx.Err() == nil {
		c.log.Printf("muster-mirror: muster speaks tunnel v1 (%s); reconnecting as v1, without the rendered view", vr.msg)
		return c.runOnce(ctx, 1)
	}
	return err
}

// versionRefusal is a v1 muster refusing this host's newer tunnel version.
type versionRefusal struct{ msg string }

func (v *versionRefusal) Error() string { return "muster refused the registration: " + v.msg }

func (c *Client) runOnce(ctx context.Context, version int) error {
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+c.cfg.Token)
	dctx, dcancel := context.WithTimeout(ctx, 20*time.Second)
	ws, resp, err := websocket.Dial(dctx, c.url, &websocket.DialOptions{HTTPHeader: hdr})
	dcancel()
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			return fmt.Errorf("%w (HTTP %d)", ErrUnauthorized, resp.StatusCode)
		}
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf("muster answered 404 for %s: the mirror plugin is off there (MUSTER_MIRROR unset)", c.url)
		}
		return fmt.Errorf("dial %s: %w", c.url, err)
	}
	defer ws.CloseNow()
	ws.SetReadLimit(mirror.MaxFrame)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	conn := &hostSide{c: c, ws: ws, ctx: ctx, version: version, streams: map[uint32]*hostStream{},
		listed: map[string]bool{}, watches: map[uint32]context.CancelFunc{}}
	defer conn.closeAll()

	sessions := c.list(ctx)
	conn.setListed(sessions)
	claude := conn.resolveClaude(sessions)
	labels := conn.labels(sessions)
	activity := conn.activity(sessions)
	if err := conn.send(mirror.Msg{Type: mirror.TypeHello, Version: version, Host: c.cfg.Host,
		Sessions: sessions, Claude: claude, Labels: labels, Activity: activity, Interactive: c.cfg.Interactive, PanePaste: conn.panePaste()}); err != nil {
		return err
	}
	wctx, wcancel := context.WithTimeout(ctx, 15*time.Second)
	typ, data, err := ws.Read(wctx)
	wcancel()
	if err != nil {
		return fmt.Errorf("no welcome: %w", err)
	}
	var welcome mirror.Msg
	if typ != websocket.MessageText || json.Unmarshal(data, &welcome) != nil || welcome.Type != mirror.TypeWelcome {
		return errors.New("muster's first message was not a welcome")
	}
	if welcome.Error != "" {
		if version > 1 && strings.HasPrefix(welcome.Error, mirror.V1RefusalPrefix) {
			return &versionRefusal{msg: welcome.Error}
		}
		return fmt.Errorf("muster refused the registration: %s", welcome.Error)
	}
	c.log.Printf("muster-mirror: connected to %s as host %s with %d session(s)", c.url, c.cfg.Host, len(sessions))

	go conn.refresh(sessions, claude, labels, activity)
	go conn.keepalive(cancel)
	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			return err
		}
		switch typ {
		case websocket.MessageBinary:
			id, p, err := mirror.DecodeData(data)
			if err == nil {
				conn.input(id, p)
			}
		case websocket.MessageText:
			var m mirror.Msg
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			switch m.Type {
			case mirror.TypeOpen:
				conn.open(m)
			case mirror.TypeResize:
				if st := conn.get(m.ID); st != nil {
					_ = st.term.Resize(m.Cols, m.Rows)
				}
			case mirror.TypeClose:
				if st := conn.get(m.ID); st != nil {
					_ = st.term.Close()
				}
				conn.unwatch(m.ID)
			case mirror.TypeWatch:
				if conn.version >= 2 {
					conn.watch(m)
				}
			case mirror.TypePaste:
				if conn.version >= 2 {
					go conn.paste(m)
				}
			}
		}
	}
}

// list is the backend's sessions, valid names only; a failed read is logged and
// registers none (a transient tmux failure must not take the connection down).
func (c *Client) list(ctx context.Context) []string {
	l, err := c.cfg.Backend.List(ctx)
	if err != nil {
		c.log.Printf("muster-mirror: list sessions: %v", err)
		return []string{}
	}
	out := make([]string, 0, len(l))
	for _, s := range l {
		if mirror.ValidSession(s) == nil {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return out
}

type hostSide struct {
	c   *Client
	ws  *websocket.Conn
	ctx context.Context
	// version is the tunnel version this connection speaks.
	version int

	mu      sync.Mutex
	streams map[uint32]*hostStream
	// listed is the session list this host last SENT. An open for any other
	// name is refused: muster can only attach what this host chose to show.
	listed map[string]bool
	// watches are the open transcript streams (rendered views), by stream id.
	watches map[uint32]context.CancelFunc
	// labelErr is the last window-index read error logged ("" = none), so a
	// failing read is logged when it starts failing, not every Refresh. Only
	// the hello and then the one refresh goroutine touch it, in that order.
	labelErr string
	// activityErr is labelErr for the activity read.
	activityErr string
}

// activity is what this host sends as sessions' last-active times
// (SessionActivity): already floored to mirror.ActivityBucket, so it is equal
// between two reads unless a session crossed into a newer bucket.
func (h *hostSide) activity(sessions []string) map[string]int64 {
	activity, err := SessionActivity(h.ctx, h.c.cfg.Backend, sessions, time.Now())
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	if msg != h.activityErr {
		if msg != "" {
			h.c.log.Printf("muster-mirror: read session activity (muster keeps the name order): %v", err)
		}
		h.activityErr = msg
	}
	return activity
}

// labels is sessions' display labels (SessionLabels over Config.Codenames).
func (h *hostSide) labels(sessions []string) map[string]string {
	labels, err := SessionLabels(h.ctx, h.c.cfg.Backend, h.c.cfg.Codenames, sessions)
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	if msg != h.labelErr {
		if msg != "" {
			h.c.log.Printf("muster-mirror: read active windows for labels (labelling by codename alone): %v", err)
		}
		h.labelErr = msg
	}
	return labels
}

func (h *hostSide) setListed(list []string) {
	m := make(map[string]bool, len(list))
	for _, s := range list {
		m[s] = true
	}
	h.mu.Lock()
	h.listed = m
	h.mu.Unlock()
}

func (h *hostSide) isListed(session string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.listed[session]
}

type hostStream struct {
	term     Terminal
	readOnly bool
}

func (h *hostSide) send(m mirror.Msg) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(h.ctx, 10*time.Second)
	defer cancel()
	return h.ws.Write(wctx, websocket.MessageText, b)
}

func (h *hostSide) get(id uint32) *hostStream {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.streams[id]
}

// input types p into stream id — unless the stream is read-only.
//
// 🔴 A READ-ONLY STREAM DROPS INPUT HERE, WHATEVER muster SENT. muster sends none
// for one, and tmux was started with -r; this is the layer that does not depend
// on muster being right.
func (h *hostSide) input(id uint32, p []byte) {
	st := h.get(id)
	if st == nil || st.readOnly {
		return
	}
	_, _ = st.term.Write(p)
}

func (h *hostSide) open(m mirror.Msg) {
	readOnly := m.ReadOnly
	fail := func(err error) {
		h.c.log.Printf("muster-mirror: open %q: %v", m.Session, err)
		_ = h.send(mirror.Msg{Type: mirror.TypeClosed, ID: m.ID, Error: err.Error()})
	}
	if err := mirror.ValidSession(m.Session); err != nil {
		fail(err)
		return
	}
	// 🔴 ONLY A SESSION THIS HOST LISTED. tmux would attach any session on the
	// server; muster may OPEN only the ones this host last reported. ⚠ This
	// bounds read-only viewing. It is NOT a boundary for an interactive client,
	// which can switch sessions or open a shell once attached (see Interactive).
	if !h.isListed(m.Session) {
		fail(fmt.Errorf("session %q is not in this host's latest list", m.Session))
		return
	}
	// 🔴 INTERACTIVE ONLY IF THIS HOST OPTED IN (-interactive), whatever muster
	// asks — muster refuses this too, and this layer does not depend on it.
	if !readOnly && !h.c.cfg.Interactive {
		fail(errors.New("this host is read-only (muster-mirror was not started with -interactive)"))
		return
	}
	if h.get(m.ID) != nil {
		fail(fmt.Errorf("stream %d is already open", m.ID))
		return
	}
	term, err := h.c.cfg.Backend.Attach(h.ctx, m.Session, readOnly, m.Cols, m.Rows)
	if err != nil {
		fail(err)
		return
	}
	st := &hostStream{term: term, readOnly: readOnly}
	h.mu.Lock()
	h.streams[m.ID] = st
	h.mu.Unlock()
	mode := "interactive"
	if readOnly {
		mode = "read-only"
	}
	h.c.log.Printf("muster-mirror: attached %q (%s, stream %d)", m.Session, mode, m.ID)
	go func() {
		defer func() {
			_ = term.Close()
			h.mu.Lock()
			delete(h.streams, m.ID)
			h.mu.Unlock()
			h.c.log.Printf("muster-mirror: detached %q (stream %d)", m.Session, m.ID)
		}()
		buf := make([]byte, 32*1024)
		for {
			n, err := term.Read(buf)
			if n > 0 {
				wctx, cancel := context.WithTimeout(h.ctx, 10*time.Second)
				werr := h.ws.Write(wctx, websocket.MessageBinary, mirror.EncodeData(m.ID, buf[:n]))
				cancel()
				if werr != nil {
					return
				}
			}
			if err != nil {
				msg := ""
				if !errors.Is(err, io.EOF) {
					msg = err.Error()
				}
				_ = h.send(mirror.Msg{Type: mirror.TypeClosed, ID: m.ID, Error: msg})
				return
			}
		}
	}()
}

func (h *hostSide) closeAll() {
	h.mu.Lock()
	all := make([]Terminal, 0, len(h.streams))
	for _, st := range h.streams {
		all = append(all, st.term)
	}
	h.mu.Unlock()
	for _, t := range all {
		_ = t.Close()
	}
}

// refresh re-reads the session list (and, on v2, which sessions run Claude Code,
// each session's label, and each session's activity) every Refresh and sends it
// when any of them changed.
//
// 🔴 A LABEL CHANGE ALONE IS A CHANGE. A label carries the session's ACTIVE
// window, which moves while the session set stays the same; comparing only the
// sessions would leave muster showing "mango:1" after the operator moved to
// window 3.
//
// 🔴 ACTIVITY IS COMPARED FLOORED (mirror.ActivityBucket), SO IT IS NOT A
// RE-SEND EVERY REFRESH. A busy pane's raw time differs on every read; what is
// compared here changes only when a session enters a newer bucket. Sessions
// that stay busy all cross a bucket boundary on the same refresh and share ONE
// message per bucket, however many they are; a session that wakes up in the
// middle of a bucket adds one more. Those messages are what keep this host's
// times comparable with another host's; muster nudges its open views only if
// the ORDER came out different (mirror.Hub).
func (h *hostSide) refresh(last, lastClaude []string, lastLabels map[string]string, lastActivity map[string]int64) {
	t := time.NewTicker(h.c.cfg.Refresh)
	defer t.Stop()
	for {
		select {
		case <-h.ctx.Done():
			return
		case <-t.C:
			now := h.c.list(h.ctx)
			claude := h.resolveClaude(now)
			labels := h.labels(now)
			activity := h.activity(now)
			if slices.Equal(now, last) && slices.Equal(claude, lastClaude) && maps.Equal(labels, lastLabels) && maps.Equal(activity, lastActivity) {
				continue
			}
			// The allowlist moves BEFORE muster hears of a new session, so an open
			// for it is never refused for being unlisted; a session that ended
			// is unattachable from this instant.
			h.setListed(now)
			if err := h.send(mirror.Msg{Type: mirror.TypeSessions, Sessions: now, Claude: claude, Labels: labels, Activity: activity}); err != nil {
				return
			}
			last, lastClaude, lastLabels, lastActivity = now, claude, labels, activity
		}
	}
}

func (h *hostSide) keepalive(cancel context.CancelFunc) {
	t := time.NewTicker(25 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-h.ctx.Done():
			return
		case <-t.C:
			pctx, pc := context.WithTimeout(h.ctx, 10*time.Second)
			err := h.ws.Ping(pctx)
			pc()
			if err != nil {
				cancel()
				return
			}
		}
	}
}
