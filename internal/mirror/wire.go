// Package mirror is the server half of MIRRORED sessions: tmux sessions that live
// on an operator's own machines (a workstation, a laptop) and appear in muster's
// Agents view beside its provisioned agents, so the view is complete.
//
// 🔴 A MIRRORED SESSION IS NOT AN AGENT. Nothing about it is persisted: the hub
// holds each connected host's session list IN MEMORY, keyed by the host's token
// OWNER and host name, and the Agents view renders it from there. No agents row,
// no kind, no chat, no lifecycle — so it can never be dispatched, deleted,
// chatted with, or mistaken for an agent by name (a tmux session named "chief"
// is not the chief). The only table is mirror_tokens (migration 0008).
//
// It is an OPTIONAL PLUGIN, off unless MUSTER_MIRROR=1. The other half is the
// `muster-mirror` binary (cmd/muster-mirror, internal/mirrorhost), installed only
// on the machines whose sessions should be mirrored.
//
// 🔴 THE HOST DIALS OUT; muster NEVER DIALS IN. muster holds no SSH key, no
// address and no inbound route to any host. A host's muster-mirror opens ONE
// WebSocket to muster (GET /mirror/connect), authenticated by a per-host mirror
// token, registers its session list on it, and serves terminal attaches that
// muster multiplexes back down the same connection. Nothing crosses from muster
// to the host but the tunnel's own frames: no muster credential, no agent token,
// no configuration.
//
// This file is the WIRE, shared by both halves so they cannot disagree about it.
package mirror

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/cctranscript"
)

// ProtocolVersion is the newest tunnel version this side speaks.
//
//   - 1: session names, terminals.
//   - 2: adds the RENDERED VIEW — which sessions' panes run Claude Code (Msg.Claude),
//     a transcript stream per open view (watch / events), and pasting a message
//     into such a pane (paste / pasted).
//
// 🔴 THE BUMP IS BACKWARD-COMPATIBLE IN BOTH DIRECTIONS, because hosts and the
// server upgrade at different times:
//
//   - a v2 muster accepts a v1 host (MinProtocolVersion) and speaks v1 to it: its
//     sessions are listed and attachable exactly as before, with no rendered view
//     (nothing v2 is ever sent to it);
//   - a v2 host that reaches a v1 muster is refused with that muster's version
//     error, and retries AT ONCE as v1 (mirrorhost.Client) rather than staying
//     offline until muster is upgraded.
//
// Anything else outside [MinProtocolVersion, ProtocolVersion] is refused, so a
// genuinely skewed pair still fails loudly at connect rather than mid-attach.
const ProtocolVersion = 2

// MinProtocolVersion is the oldest host version this muster accepts.
const MinProtocolVersion = 1

// V1RefusalPrefix is how a v1 muster's refusal of a newer host begins — its
// readHello error, frozen in every v1 build. A v2 host that sees it falls back
// to v1. (A v2 muster never sends it for a version it accepts.)
const V1RefusalPrefix = "mirror: this muster speaks tunnel version 1 and the host sent "

// ConnectPath is the route a host's muster-mirror dials.
const ConnectPath = "/mirror/connect"

// TokenPrefix starts every mirror token. It is how an operator (and a leak
// scanner) recognises one, and how muster-mirror refuses a pasted credential
// that is plainly something else — the hook token or an agent's token.
const TokenPrefix = "mmt_"

// MaxFrame bounds one WebSocket message in either direction. A pty read is at
// most a few KiB; a control message is a session list. Both ends set it as the
// read limit (coder/websocket defaults to 32 KiB, which a burst of terminal
// output can exceed).
const MaxFrame = 1 << 20

// MaxSessions bounds the SESSION list a per-session host may register (a
// muster-mirror from before per-window registration; Msg.Sessions). A host with
// more is truncated to this many, sorted, so the set is stable between
// refreshes. A per-window host is bounded by MaxWindows instead.
const MaxSessions = 64

// MaxWindows bounds the WINDOW list one host may register (Msg.Windows). A host
// with more is truncated to this many in (session, index) order, so the set is
// stable between refreshes.
//
// 🔴 IT REPLACES MaxSessions FOR A PER-WINDOW HOST, AND THE NUMBER IS A DECISION:
// one entry per tmux WINDOW is several times one per session (an operator runs
// one coding agent per window), and 64 was already exceeded by a real host. 512
// leaves that host room to grow several-fold.
//
// 🔴 IT IS ALSO A SIZE, AND THE SIZE IS WHAT CAPS IT. The whole list travels in
// ONE message, and a message over MaxFrame ends the connection. A count is not
// a size — JSON writes some bytes six-fold (`<` is \u003c) — so the bound that
// matters is the WORST-CASE encoding of MaxWindows entries, each field at its
// own limit, beside the per-session fields at theirs: it must stay within
// MaxFrame. TestTheLargestWindowListFitsOneFrame measures that, so raising
// this number (or a field's limit) past what fits fails there rather than on a
// host with many windows.
const MaxWindows = 512

// MaxCommand bounds Window.Command, in bytes.
const MaxCommand = 32

// ViewSessionPrefix starts the name of every tmux session muster-mirror itself
// creates: the short-lived session a terminal viewer attaches to, holding a
// link to the one window it shows, so that viewing a window never moves the
// operator's own current window (internal/mirrorhost). Such a session is never
// registered, on either side.
const ViewSessionPrefix = "muster-view-"

// MaxStreamsPerHost bounds the terminals open on one host at once.
const MaxStreamsPerHost = 8

// MaxHosts bounds the hosts connected to one muster at once (all owners).
const MaxHosts = 64

// Message types. Host → muster: hello, sessions, closed. muster → host:
// welcome, open, resize, close. Binary frames carry terminal bytes in both
// directions (see EncodeData).
const (
	TypeHello    = "hello"
	TypeWelcome  = "welcome"
	TypeSessions = "sessions"
	TypeOpen     = "open"
	TypeResize   = "resize"
	TypeClose    = "close"
	TypeClosed   = "closed"

	// v2. muster → host: watch (open a transcript stream), paste. Host → muster:
	// events (a batch of one stream), pasted (a paste's outcome). A watch is
	// ended like a terminal — close / closed — and shares the stream id space.
	TypeWatch  = "watch"
	TypeEvents = "events"
	TypePaste  = "paste"
	TypePasted = "pasted"

	// Scrolling a pane's history (tmux copy-mode). muster → host: scroll. Host →
	// muster: scrolled (its outcome). Sent only to a host that advertised
	// Msg.PaneScroll — see that field for the compatibility.
	TypeScroll   = "scroll"
	TypeScrolled = "scrolled"
)

// Msg is every control message on the tunnel, as one JSON text frame.
type Msg struct {
	Type    string `json:"type"`
	Version int    `json:"version,omitempty"`
	// Host is the registering host's name (hello only). It must equal the host
	// the presented token was minted for.
	Host string `json:"host,omitempty"`
	// Sessions is the host's full tmux session list (hello, sessions).
	Sessions []string `json:"sessions,omitempty"`
	// Labels (hello, sessions) is an optional DISPLAY name per session —
	// session → label, e.g. "scratch13" → "mango:1" (the host's codename for the
	// session plus its active window). Display only: a session is still
	// addressed by its name everywhere (opens, URLs), because a label changes
	// whenever the active window does.
	//
	// 🔴 NO VERSION BUMP, BACKWARD-COMPATIBLE BOTH WAYS: an older muster's JSON
	// decode ignores the unknown field, and a host that sends none is shown by
	// its raw session names. muster keeps only entries NormalizeLabels accepts
	// and drops the rest — never the hello.
	Labels map[string]string `json:"labels,omitempty"`
	// Activity (hello, sessions) is, per session, WHEN it was last active on
	// the host: Unix seconds, floored to ActivityBucket. muster lists mirrored
	// sessions most recently active first (Ordered), and each card prints it
	// as "active … ago". Ordering and display only — it never addresses
	// anything.
	//
	// 🔴 NO VERSION BUMP, BACKWARD-COMPATIBLE BOTH WAYS, exactly like Labels: an
	// older muster ignores the field, and a host that sends none is listed in
	// the order it always was (host, then session name). muster keeps only
	// entries NormalizeActivity accepts and drops the rest — never the hello.
	Activity map[string]int64 `json:"activity,omitempty"`
	// Interactive (hello) is the host ADVERTISING that it accepts interactive
	// attaches — muster-mirror's -interactive flag. Without it the host is
	// read-only: muster offers no Interactive switch and refuses a rw attach,
	// and the host refuses one itself.
	Interactive bool `json:"interactive,omitempty"`
	// PanePaste (hello) is the host ADVERTISING that a paste may name a
	// session's ACTIVE PANE whatever runs in it (AnyPane) — a shell, another
	// TUI — and not only a Claude Code pane: what the Terminal view's composer
	// needs for a session with no rendered view. Only an -interactive v2 host
	// whose backend can do it sets it.
	//
	// 🔴 NO VERSION BUMP, BACKWARD-COMPATIBLE BOTH WAYS, like Labels: an older
	// muster ignores the field and never asks; an older host never sets it, so
	// muster never sends it AnyPane (Hub.Paste) — and if it somehow did, that
	// host would ignore the unknown field and apply its Claude-only rule, which
	// REFUSES a pane that is not Claude Code. The failure direction is "not
	// sent", never "sent somewhere unintended".
	PanePaste bool `json:"panePaste,omitempty"`
	// PaneScroll (hello) is the host ADVERTISING that it scrolls a window's
	// active pane through its history in tmux COPY-MODE (scroll / scrolled):
	// what the Terminal view's drag-to-scroll needs, since the viewer's own tmux
	// client paints only the visible screen and so the browser holds no history.
	// Only an -interactive v2 host whose backend can do it sets it.
	//
	// 🔴 NO VERSION BUMP, BACKWARD-COMPATIBLE BOTH WAYS, like PanePaste: an older
	// muster ignores the field and never asks; an older host never sets it, so
	// muster never sends it a scroll and the page draws no gesture at all. (An
	// older host that somehow got one would ignore the unknown type — its read
	// loop has no default case — and muster's wait would time out; it is never
	// sent one.)
	PaneScroll bool `json:"paneScroll,omitempty"`
	// ID names one terminal stream (open, resize, close, closed).
	ID uint32 `json:"id,omitempty"`
	// Session is the tmux session an open attaches to.
	Session string `json:"session,omitempty"`
	// ReadOnly asks the host to attach `tmux attach -r` and to drop input.
	ReadOnly bool   `json:"readOnly,omitempty"`
	Cols     uint16 `json:"cols,omitempty"`
	Rows     uint16 `json:"rows,omitempty"`
	// Error is why a stream ended (closed), why a paste failed (pasted), or why
	// a connection is refused.
	Error string `json:"error,omitempty"`

	// ---- v2 ----

	// Claude (hello, sessions) is the subset of Sessions whose active pane runs
	// Claude Code with a transcript the host can read: the sessions muster opens
	// in the rendered view by default.
	Claude []string `json:"claude,omitempty"`
	// Gen names the transcript a watch / events refers to (the Claude Code
	// session id). Offset is a byte offset into it — always a line boundary
	// (cctranscript): a watch's Offset is where to RESUME (honoured only when
	// Gen still names the session's current transcript, else the host backfills),
	// an events' Offset is where the next batch continues from.
	Gen    string `json:"gen,omitempty"`
	Offset int64  `json:"offset,omitempty"`
	// Reset (events): drop what was drawn; Events starts the transcript Gen
	// over (a fresh backfill, a /clear, a rotated or truncated file).
	Reset bool `json:"reset,omitempty"`
	// Busy (events): Claude Code reports a turn running (its session status).
	Busy   bool                 `json:"busy,omitempty"`
	Events []cctranscript.Event `json:"events,omitempty"`
	// PromptIndex (events) marks the message that carries the transcript's
	// PROMPT INDEX: Prompts is every prompt in the WHOLE file up to the
	// backfill's end (cctranscript.Prompts), the newest BoundPrompts keeps, and
	// PromptsMore how many earlier ones it dropped. The host sends it once per
	// Reset, as its own events message after the backfill, with no events and
	// the backfill's own Offset and Busy — so the message index can list a
	// prompt the backfill never loaded.
	//
	// 🔴 NO VERSION BUMP, BACKWARD-COMPATIBLE BOTH WAYS, like Labels: an older
	// muster ignores the fields and relays the message as an empty batch at an
	// offset it already had; an older host never sends it, and the view lists
	// the prompts on the page and says that is all it can show.
	PromptIndex bool                     `json:"promptIndex,omitempty"`
	Prompts     []cctranscript.PromptRef `json:"prompts,omitempty"`
	PromptsMore int                      `json:"promptsMore,omitempty"`
	// Text is what a paste types into the session's pane.
	Text string `json:"text,omitempty"`
	// AnyPane (paste) asks for the session's ACTIVE pane whatever runs in it.
	// Unset — every paste before this field existed — the host pastes only into
	// a pane it resolves as Claude Code NOW, and refuses otherwise: the rendered
	// view's send must never land a prompt meant for Claude in a shell.
	AnyPane bool `json:"anyPane,omitempty"`
	// Lines (scroll) is how far to scroll the pane's history: positive is BACK
	// (tmux scroll-up, older output), negative is forward (scroll-down, toward
	// the live screen). 0 and anything beyond MaxScrollLines either way is
	// refused (ClampScroll is the bound both halves apply).
	Lines int `json:"lines,omitempty"`
	// Refusal (scrolled) is WHICH KIND of refusal Error is, for muster to act
	// on without reading the host's prose: ScrollRefusalMode or
	// ScrollRefusalUnsupported; "" for any other (and for every refusal an
	// older host sends — which muster then treats as a plain refusal, never as
	// "this host cannot scroll"). Backward-compatible both ways, like PaneScroll.
	Refusal string `json:"refusal,omitempty"`

	// ---- per-window registration ----

	// Windows (hello, sessions) is EVERY WINDOW of every session on the host,
	// one entry each: what muster lists when it is present. A host that sends
	// it is a PER-WINDOW host; it still sends Sessions / Labels / Activity /
	// Claude beside it, describing each session by its ACTIVE window exactly as
	// before, for a muster that predates this field.
	//
	// 🔴 NO VERSION BUMP, BACKWARD-COMPATIBLE BOTH WAYS, like Labels:
	//   - an older muster's JSON decode ignores the field and lists the host per
	//     session from the fields it always read; it never sends Window, so the
	//     host serves it the session's active window as it always did;
	//   - an older host sends none, and muster lists it per session, addresses it
	//     by session name alone, and never sends it Window.
	// muster keeps only what NormalizeWindows accepts and derives its own
	// per-session view of a per-window host from Windows — never from the
	// host's Sessions, so the two cannot disagree.
	Windows []Window `json:"windows,omitempty"`
	// Window (open, watch, paste) is the tmux window id (Window.ID) the request
	// is for, within Session. Empty — every request before this field existed,
	// and every request to a per-session host — is the session's ACTIVE window.
	Window string `json:"window,omitempty"`
}

// Window is one tmux window of a host (Msg.Windows).
//
// 🔴 A WINDOW IS ADDRESSED BY (Session, ID), NEVER BY ITS INDEX. tmux renumbers
// indexes (renumber-windows, move-window, a window closing below it), so an
// index names whatever sits there NOW; the id (@12) is the window's own for as
// long as it exists. Index is carried for DISPLAY only ("Gold:8").
type Window struct {
	// Session is the tmux session the window belongs to.
	Session string `json:"session"`
	// ID is tmux's window id, "@<n>" (#{window_id}).
	ID string `json:"id"`
	// Index is the window's index in its session NOW. Display only.
	Index int `json:"index"`
	// Active: this is its session's current window — the one a request that
	// names only the session reaches.
	Active bool `json:"active,omitempty"`
	// Label is the host's display name, "<codename>:<index>", "" for none
	// (muster then shows "<session>:<index>").
	Label string `json:"label,omitempty"`
	// Activity is when the window was last active (Unix seconds, floored to
	// ActivityBucket); 0 = unknown. Ordering and display only (the card's
	// "active … ago").
	Activity int64 `json:"activity,omitempty"`
	// Claude: the window's active pane runs Claude Code with a transcript the
	// host can read — it has a rendered view.
	Claude bool `json:"claude,omitempty"`
	// Command is what runs in the window's active pane (#{pane_current_command}:
	// "zsh", "opencode", "vim"), "" for unknown. Display and search only.
	Command string `json:"command,omitempty"`
}

// The v2 bounds. Each is a property of the wire, so both halves read it here.
const (
	// MaxWatchesPerHost bounds the transcript streams open on one host.
	MaxWatchesPerHost = 8
	// MaxBatchEvents and MaxBatchBytes bound one events message: at most this
	// many events, and at most this many bytes of them ENCODED (Batches). Bytes,
	// not a count alone, because JSON escaping can inflate a capped field up to
	// six-fold (`<` is written \u003c), so a count bound alone could exceed
	// MaxFrame and end the whole connection.
	MaxBatchEvents = 64
	MaxBatchBytes  = 256 << 10
	// MaxBackfillEvents bounds a fresh view's history; BackfillWindow is the
	// tail of the transcript file it is read from. The file is never sent
	// wholesale: a viewer gets the recent conversation, not tens of MiB.
	MaxBackfillEvents = 300
	BackfillWindow    = 4 << 20
	// MaxPaste bounds a paste's text.
	MaxPaste = 16 << 10
	// MaxIndexPrompts and MaxIndexBytes bound a prompt index (Msg.Prompts): at
	// most this many prompts, at most this many bytes of them ENCODED — the
	// count alone could not keep a message inside MaxFrame, for the reason
	// MaxBatchBytes gives. The newest are kept (BoundPrompts).
	MaxIndexPrompts = 2000
	MaxIndexBytes   = 512 << 10
	// MaxScrollLines bounds one scroll request either way: a page coalesces a
	// drag into one request per animation frame, and a frame's worth of a fast
	// fling is tens of lines, not hundreds.
	MaxScrollLines = 200
)

// BoundPrompts is the newest of refs that fit the index bounds, in order, and
// how many earlier ones it dropped. A ref with no Line, or whose Line does not
// follow the one before it, is dropped too — not counted, since it names no
// prompt — and every Text is capped again: muster applies this to what a host
// sent as well as the host to what it read.
func BoundPrompts(refs []cctranscript.PromptRef) ([]cctranscript.PromptRef, int) {
	valid := make([]cctranscript.PromptRef, 0, len(refs))
	var last int64
	for _, r := range refs {
		if r.Line <= last {
			continue
		}
		last = r.Line
		r.Text = cctranscript.Cap(r.Text, cctranscript.MaxPromptRefText)
		valid = append(valid, r)
	}
	size, keep := 0, 0
	for i := len(valid) - 1; i >= 0 && keep < MaxIndexPrompts; i-- {
		b, _ := json.Marshal(valid[i])
		if size+len(b)+1 > MaxIndexBytes {
			break
		}
		size += len(b) + 1
		keep++
	}
	return valid[len(valid)-keep:], len(valid) - keep
}

// The kinds of scroll refusal a host names (Msg.Refusal).
const (
	// ScrollRefusalMode: the pane is in a mode other than copy-mode at the top
	// of its stack (a tmux menu, choose-tree, view-mode): nothing scrolls until
	// it is closed. The pane can scroll again later.
	ScrollRefusalMode = "mode"
	// ScrollRefusalUnsupported: this host cannot scroll a pane at all (read-only,
	// or its backend has no copy-mode). The only refusal that means "stop asking".
	ScrollRefusalUnsupported = "unsupported"
)

// ClampScroll is n bounded to [-MaxScrollLines, MaxScrollLines], and ok=false
// for 0 (nothing to scroll). Both halves apply it: muster to what a browser
// asked for, the host again to what muster sent.
func ClampScroll(n int) (int, bool) {
	if n == 0 {
		return 0, false
	}
	return max(-MaxScrollLines, min(MaxScrollLines, n)), true
}

// dataHeader is the stream id prefixed to every binary frame.
const dataHeader = 4

// EncodeData frames terminal bytes for stream id.
func EncodeData(id uint32, p []byte) []byte {
	b := make([]byte, dataHeader+len(p))
	binary.BigEndian.PutUint32(b, id)
	copy(b[dataHeader:], p)
	return b
}

// DecodeData splits a binary frame into its stream id and bytes.
func DecodeData(b []byte) (uint32, []byte, error) {
	if len(b) < dataHeader {
		return 0, nil, fmt.Errorf("mirror: a %d-byte data frame is shorter than its %d-byte header", len(b), dataHeader)
	}
	return binary.BigEndian.Uint32(b), b[dataHeader:], nil
}

// hostPattern is a DNS label, lowercase: what a host is called in a token, a
// hello, an agent name and a card badge.
var hostPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ErrBadHost is the refusal for a host name that is not a lowercase DNS label.
var ErrBadHost = errors.New("mirror: a host name must be a lowercase DNS label ([a-z0-9-], 1-63 chars, not starting or ending with '-')")

// ValidHost reports whether host is a usable host name.
func ValidHost(host string) error {
	if !hostPattern.MatchString(host) {
		return fmt.Errorf("%w: got %q", ErrBadHost, host)
	}
	return nil
}

// maxSessionName bounds a session name. tmux has no limit of its own; muster
// renders the name, keys a row on it and passes it back to `tmux attach -t`.
const maxSessionName = 128

// ValidSession reports whether name is a session name the tunnel carries: valid
// UTF-8, 1-128 bytes, and no control characters. The name is passed to tmux as
// ONE argv element (never through a shell), behind tmux's `=` exact-match
// prefix, so nothing else about it is interpreted.
func ValidSession(name string) error {
	if name == "" || len(name) > maxSessionName || !utf8.ValidString(name) {
		return fmt.Errorf("mirror: session name %q must be 1-%d bytes of valid UTF-8", name, maxSessionName)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("mirror: session name %q carries a control character", name)
		}
	}
	return nil
}

// MaxLabel bounds a session's display label (Msg.Labels), in bytes.
const MaxLabel = 64

// ValidLabel reports whether label is a display label the tunnel carries: valid
// UTF-8, 1-MaxLabel bytes, no control characters (C0, DEL or C1) and not all
// whitespace. It is rendered as text only, never parsed or passed to tmux.
func ValidLabel(label string) error {
	if strings.TrimSpace(label) == "" || len(label) > MaxLabel || !utf8.ValidString(label) {
		return fmt.Errorf("mirror: label %q must be 1-%d bytes of valid UTF-8, not blank", label, MaxLabel)
	}
	for _, r := range label {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return fmt.Errorf("mirror: label %q carries a control character", label)
		}
	}
	return nil
}

// NormalizeLabels is labels restricted to sessions (a host cannot label a
// session it does not list) with every invalid label dropped; nil when nothing
// is left. Both halves apply it: the host to what it sends, muster to what it
// keeps — muster never trusts the host to have done it.
func NormalizeLabels(labels map[string]string, sessions []string) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	var out map[string]string
	for _, s := range sessions {
		l, ok := labels[s]
		if !ok || ValidLabel(l) != nil {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[s] = l
	}
	return out
}

// ActivityBucket is the granularity of Msg.Activity, in seconds. A session's
// activity is carried and compared floored to it.
//
// 🔴 IT IS THE RE-SEND AND RE-ORDER POLICY, NOT A ROUNDING NICETY. A busy pane's
// raw activity time moves every second; sent raw, every refresh would differ,
// every one would be re-sent, and two busy sessions would swap places on the
// Agents view each time. Floored, a host's map changes at most once per bucket
// per session, sessions active within the same bucket TIE (and keep their
// name order), and the list reorders only when a session falls a whole bucket
// behind another.
const ActivityBucket = 60

// NormalizeActivity is activity restricted to sessions (a host cannot rank a
// session it does not list), each time floored to ActivityBucket; nil when
// nothing is left. An entry that is not a positive time is dropped, and one
// later than now is CAPPED at now — a host whose clock runs ahead ties with
// "active now" rather than outranking every other host forever (now <= 0
// skips the cap). Both halves apply it: the host to what it sends, muster to
// what it keeps.
func NormalizeActivity(activity map[string]int64, sessions []string, now int64) map[string]int64 {
	if len(activity) == 0 {
		return nil
	}
	var out map[string]int64
	for _, s := range sessions {
		v, ok := activity[s]
		if !ok || v <= 0 {
			continue
		}
		if now > 0 && v > now {
			v = now
		}
		if v -= v % ActivityBucket; v <= 0 {
			continue
		}
		if out == nil {
			out = map[string]int64{}
		}
		out[s] = v
	}
	return out
}

// windowIDPattern is a tmux window id.
var windowIDPattern = regexp.MustCompile(`^@[0-9]{1,10}$`)

// maxWindowIndex bounds Window.Index (tmux's own base-index arithmetic stays
// far below it).
const maxWindowIndex = 1 << 20

// ValidWindowID reports whether id is a tmux window id ("@12"): the only thing
// the tunnel carries as a window address, and — being digits behind an `@` —
// safe as one argv element of a tmux target.
func ValidWindowID(id string) error {
	if !windowIDPattern.MatchString(id) {
		return fmt.Errorf("mirror: %q is not a tmux window id (@<n>)", id)
	}
	return nil
}

// IsViewSession reports whether name is one of muster-mirror's own viewer
// sessions (ViewSessionPrefix), which are never listed.
func IsViewSession(name string) bool { return strings.HasPrefix(name, ViewSessionPrefix) }

// validCommand is command when it is a usable Window.Command, else "".
func validCommand(command string) string {
	if command == "" || len(command) > MaxCommand || !utf8.ValidString(command) {
		return ""
	}
	for _, r := range command {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return ""
		}
	}
	return command
}

// NormalizeWindows is in as the tunnel carries it: every entry with a valid
// session and window id, de-duplicated on (session, id), sorted by session,
// then index, then id, and capped at MaxWindows. Within an entry an invalid
// label or command is blanked and the activity is floored and capped exactly
// as NormalizeActivity does; at most ONE window per session stays Active (the
// first in order). A viewer session's windows (IsViewSession) are dropped.
// Both halves apply it: the host to what it sends, muster to what it keeps.
//
// It returns nil for no input and a non-nil empty slice when every entry was
// dropped, so "sent no window list" and "sent one that was all invalid" stay
// distinguishable to the caller.
func NormalizeWindows(in []Window, now int64) []Window {
	if in == nil {
		return nil
	}
	type key struct{ session, id string }
	seen := map[key]bool{}
	out := make([]Window, 0, len(in))
	for _, w := range in {
		if ValidSession(w.Session) != nil || IsViewSession(w.Session) || ValidWindowID(w.ID) != nil ||
			w.Index < 0 || w.Index > maxWindowIndex || seen[key{w.Session, w.ID}] {
			continue
		}
		seen[key{w.Session, w.ID}] = true
		if w.Label != "" && ValidLabel(w.Label) != nil {
			w.Label = ""
		}
		w.Command = validCommand(w.Command)
		if w.Activity < 0 {
			w.Activity = 0
		}
		if now > 0 && w.Activity > now {
			w.Activity = now
		}
		w.Activity -= w.Activity % ActivityBucket
		out = append(out, w)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Session != b.Session {
			return a.Session < b.Session
		}
		if a.Index != b.Index {
			return a.Index < b.Index
		}
		return a.ID < b.ID
	})
	if len(out) > MaxWindows {
		out = out[:MaxWindows]
	}
	active := map[string]bool{}
	for i := range out {
		if out[i].Active {
			if active[out[i].Session] {
				out[i].Active = false
			}
			active[out[i].Session] = true
		}
	}
	return out
}

// ActiveWindow is the window of session a request naming only the session
// reaches: the one marked Active, else (a list with none marked) the session's
// first. ok is false when windows lists none for session. windows must be
// NormalizeWindows' output.
func ActiveWindow(windows []Window, session string) (Window, bool) {
	var first Window
	found := false
	for _, w := range windows {
		if w.Session != session {
			continue
		}
		if w.Active {
			return w, true
		}
		if !found {
			first, found = w, true
		}
	}
	return first, found
}

// SessionsOf is the PER-SESSION view of a per-window list — what a muster that
// predates Windows is sent, and what muster itself answers a request that names
// only a session with: the distinct session names (sorted), and per session its
// ACTIVE window's label and Claude flag and the NEWEST activity of any of its
// windows. windows must be NormalizeWindows' output.
func SessionsOf(windows []Window) (sessions, claude []string, labels map[string]string, activity map[string]int64) {
	sessions = []string{}
	for _, w := range windows {
		if n := len(sessions); n == 0 || sessions[n-1] != w.Session {
			sessions = append(sessions, w.Session)
		}
		if w.Activity > activity[w.Session] {
			if activity == nil {
				activity = map[string]int64{}
			}
			activity[w.Session] = w.Activity
		}
	}
	for _, s := range sessions {
		w, _ := ActiveWindow(windows, s)
		if w.Claude {
			claude = append(claude, s)
		}
		if w.Label != "" {
			if labels == nil {
				labels = map[string]string{}
			}
			labels[s] = w.Label
		}
	}
	return sessions, claude, labels, activity
}

// BatchLines frames lines — read from a transcript starting at offset from —
// as events messages, each within MaxBatchEvents and MaxBatchBytes ENCODED,
// with Offset set to that message's exact resume point: just past the last line
// it carries whole. A line whose events do not fit one message is split; the
// messages before its last part then resume at the line's START, so a resume
// there re-sends the line — and a line with several events is tool results,
// which the view keys by tool id, so a repeat redraws rather than duplicates.
// Type, ID and Gen are the caller's to fill.
func BatchLines(lines []cctranscript.Line, from int64) []Msg {
	var out []Msg
	var cur Msg
	size, done := 0, from // done: the end of the last line fully read
	flush := func(offset int64) {
		if len(cur.Events) > 0 {
			cur.Offset = offset
			out = append(out, cur)
		}
		cur, size = Msg{}, 0
	}
	for _, l := range lines {
		for _, ev := range l.Events {
			b, _ := json.Marshal(ev)
			if len(cur.Events) > 0 && (len(cur.Events) >= MaxBatchEvents || size+len(b) > MaxBatchBytes) {
				// done is the end of the last WHOLE line: everything in cur when
				// this line has not started (ev is its first event), else this
				// line's start.
				flush(done)
			}
			cur.Events = append(cur.Events, ev)
			size += len(b) + 1
		}
		done = l.End
	}
	flush(done)
	return out
}

// SanitizePaste is text ready to paste into a pane, or why it cannot be:
// CRLF and lone CR fold to LF, and every other control character is REFUSED
// (tab and newline excepted), with the size bounded by MaxPaste.
//
// 🔴 A CONTROL CHARACTER CAN END A BRACKETED PASTE, AND WHAT FOLLOWS IT IS
// TYPED. tmux wraps the buffer in ESC[200~ … ESC[201~ and passes an ESC inside
// it through raw on some versions (ccd measured 3.3a and 3.4), so "\x1b[201~…"
// would close the paste and type the rest as keys. muster checks this before
// sending and the host checks it again before pasting.
func SanitizePaste(text string) (string, error) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	if strings.TrimSpace(text) == "" || len(text) > MaxPaste {
		return "", fmt.Errorf("mirror: a message must be 1-%d bytes of text", MaxPaste)
	}
	if !utf8.ValidString(text) {
		return "", errors.New("mirror: a message must be valid UTF-8")
	}
	for i, r := range text {
		if r == '\n' || r == '\t' {
			continue
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return "", fmt.Errorf("mirror: the message contains control character U+%04X at byte %d; only newline and tab are accepted", r, i)
		}
	}
	return text, nil
}
