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
	"errors"
	"fmt"
	"regexp"
	"unicode/utf8"
)

// ProtocolVersion is the tunnel's version. A hello carrying another one is
// refused, so a skewed pair fails loudly at connect rather than mid-attach.
const ProtocolVersion = 1

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
	// Error is why a stream ended (closed), or why a connection is refused.
	Error string `json:"error,omitempty"`
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
