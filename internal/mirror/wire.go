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

// MaxSessions bounds the list one host may register. A host with more is
// truncated to this many, sorted, so the set is stable between refreshes.
const MaxSessions = 64

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
	// Interactive (hello) is the host ADVERTISING that it accepts interactive
	// attaches — muster-mirror's -interactive flag. Without it the host is
	// read-only: muster offers no Interactive switch and refuses a rw attach,
	// and the host refuses one itself.
	Interactive bool `json:"interactive,omitempty"`
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
	// Text is what a paste types into the session's Claude pane.
	Text string `json:"text,omitempty"`
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
)

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

// SanitizePaste is text ready to paste into a Claude pane, or why it cannot be:
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
