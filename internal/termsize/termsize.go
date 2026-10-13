// Package termsize is the ONE rule for how big a muster terminal's tmux client
// is, and the in-band report that tells the browser what was decided.
//
// A browser terminal is a tmux client. A client's size changes the window it is
// attached to (tmux's default `window-size latest`; and an `attach -r` client is
// NOT exempt — measured on tmux 3.7c, see agents.TerminalCommand), and the window
// KEEPS that size after the client leaves. So a phone attaching to a session the
// operator uses at 200 columns used to leave it 45 columns wide.
//
// 🔴 THE RULE: A TERMINAL NEVER SHRINKS THE WINDOW. Decide gives the client the
// size the browser asked for only when that size COVERS the window in both
// dimensions (a desktop: the window grows to fit it, exactly as before);
// otherwise the client gets the window's own size, which changes nothing, and
// the browser renders that wider screen in a scrollable, zoomable area. It is
// the same for read-only and interactive attaches: a phone never reflows a
// session, whichever mode it is in.
//
// The rule is applied where the window's size is KNOWN — the far side of the
// attach: internal/mirrorhost on a mirrored host, internal/agentprovision for
// an agent pod. The browser does not have to be right for it to hold.
//
// THE REPORT is an OSC escape sequence written into the terminal's own byte
// stream (Report): the browser page reads it out of the frame before handing
// the rest to xterm. In-band, so nothing between the far side and the browser —
// the mirror tunnel, the hub, the socket bridge — needs a new message type; a
// far side that predates it simply sends none, and the page falls back to
// fitting its container as it always did.
package termsize

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Size is a terminal size in character cells.
type Size struct {
	Cols, Rows uint16
}

// Known reports whether s is a real size (both dimensions set).
func (s Size) Known() bool { return s.Cols > 0 && s.Rows > 0 }

// Covers reports whether s is at least o in both dimensions.
func (s Size) Covers(o Size) bool { return s.Cols >= o.Cols && s.Rows >= o.Rows }

// Decide is the size to give a tmux client whose browser asked for req, when
// the window it attaches to is currently window.
//
//   - req covers window (or the window is unknown): req. A client at least as
//     big as the window can only grow it — a desktop, unchanged.
//   - otherwise: window. Shrinking either dimension would reflow the session for
//     everyone else; the client takes the window's size instead and the browser
//     scrolls.
//   - req unknown (a zero dimension): window when known, else req — the far
//     side's own default applies.
func Decide(req, window Size) Size {
	if !window.Known() {
		return req
	}
	if !req.Known() || !req.Covers(window) {
		return window
	}
	return req
}

// ReportOSC is the OSC number of the size report. It is not a number any
// terminal or tmux defines; xterm.js ignores an OSC it has no handler for.
const ReportOSC = 7701

// Report is the in-band size report for s: ESC ] 7701 ; <cols> ; <rows> BEL.
func Report(s Size) []byte {
	return fmt.Appendf(nil, "\x1b]%d;%d;%d\x07", ReportOSC, s.Cols, s.Rows)
}

var reportPattern = regexp.MustCompile(`^\x1b\]` + strconv.Itoa(ReportOSC) + `;([0-9]{1,5});([0-9]{1,5})\x07`)

// ParsePrefix reads a size report at the very start of p, returning the size and
// how many bytes it took; ok is false when p does not start with one. It is the
// Go twin of the page's reader, for tests.
func ParsePrefix(p []byte) (Size, int, bool) {
	m := reportPattern.FindSubmatchIndex(p)
	if m == nil {
		return Size{}, 0, false
	}
	c, err1 := strconv.ParseUint(string(p[m[2]:m[3]]), 10, 16)
	r, err2 := strconv.ParseUint(string(p[m[4]:m[5]]), 10, 16)
	if err1 != nil || err2 != nil {
		return Size{}, 0, false
	}
	return Size{uint16(c), uint16(r)}, m[1], true
}

// TmuxWindowFormat is the tmux format ParseTmuxWindow reads: the window's
// size and its status-line setting.
const TmuxWindowFormat = "#{window_width} #{window_height} #{status}"

// ParseTmuxWindow reads TmuxWindowFormat's output into the CLIENT size that
// shows that window exactly: the window, plus the rows the status line takes.
//
// ⚠ THE STATUS LINE IS NOT PART OF THE WINDOW. A client at the window's own
// height would leave the window one row SHORTER (status on), so that row is
// added back — measured on tmux 3.7c: a 160x45 client makes a 160x44 window.
// `status` is off, on, or 2..5 lines.
func ParseTmuxWindow(s string) (Size, error) {
	f := strings.Fields(s)
	if len(f) != 3 {
		return Size{}, fmt.Errorf("window size: unexpected %q", s)
	}
	c, err1 := strconv.ParseUint(f[0], 10, 16)
	r, err2 := strconv.ParseUint(f[1], 10, 16)
	if err1 != nil || err2 != nil || c == 0 || r == 0 {
		return Size{}, fmt.Errorf("window size: unexpected %q", s)
	}
	status := uint64(0)
	switch f[2] {
	case "off":
	case "on":
		status = 1
	default:
		n, err := strconv.ParseUint(f[2], 10, 8)
		if err != nil || n > 5 {
			return Size{}, fmt.Errorf("window size: unexpected status %q", f[2])
		}
		status = n
	}
	if r+status > 0xffff {
		return Size{}, fmt.Errorf("window size: unexpected %q", s)
	}
	return Size{Cols: uint16(c), Rows: uint16(r + status)}, nil
}
