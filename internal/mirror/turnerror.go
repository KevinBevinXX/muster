package mirror

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"
)

// A TURN THAT ENDED IN AN ERROR (Window.TurnError): the host's reading of an
// IDLE Claude window's transcript, when the latest turn in it ended in a record
// Claude Code wrote itself to say it got no answer — a usage limit ("You've hit
// your session limit · resets …"), "Not logged in", an API error
// (cctranscript.ErrorEndOf). nil when the latest turn ended in a real answer,
// or has not ended.
//
// 🔴 WHY THE SEND QUEUE NEEDS IT: such a turn ends in a second or so and
// Claude Code goes back to its prompt, so its status reads idle with a newer
// time — exactly what a turn that was answered looks like. Without this the
// queue sent the next message into the same wall, and the next, until the
// queue was empty: every message consumed by a limit and lost
// (internal/sendqueue's Decide holds on it now, and marks the one message the
// limit bounced failed, for a Retry by hand).
//
// 🔴 BACKWARD-COMPATIBLE BOTH WAYS, like Pending: an older muster ignores it;
// an older host never sends it, and its windows' queues behave as they did.
//
// ⚠ SIZE: it rides the window list, ONE message within MaxFrame
// (TestTheLargestWindowListFitsOneFrame). So it carries no text — a kind, two
// times, a flag, a count and a 16-hex digest, every field a closed alphabet
// that JSON writes as itself — and at most MaxTurnErrorWindows windows of a list
// carry one. (The digest and PrevAt cost ~45 bytes a window, ~720 at the
// bound: what it takes for the queue to know WHICH prompt was refused.)
type TurnError struct {
	// Kind is TurnErrorLimit for a usage limit, else the record's own error
	// code ("authentication_failed", "server_error", …: ValidTurnErrorKind),
	// TurnErrorOther when it names none.
	Kind string `json:"kind"`
	// At is when the record was written: its timestamp, Unix ms ON THE HOST'S
	// CLOCK (the CLI's).
	At int64 `json:"at,omitempty"`
	// ResetsAt is when a usage limit resets, Unix ms: an absolute time the
	// limit itself names (cctranscript.LimitReset), compared with muster's
	// clock. 0 when it names none, and for every other kind.
	ResetsAt int64 `json:"resetsAt,omitempty"`
	// Rejected: the prompt this turn answered got NO real reply at all — the
	// error is the turn's only reply, no tool ran, nothing was written. False
	// when any real reply came first (the limit hit mid-turn: the prompt was
	// processed) or no prompt was found. The send queue marks a message it
	// sent failed ("Rejected: usage limit") only on a turn that says this.
	Rejected bool `json:"rejected,omitempty"`
	// Prompt is PromptDigest of the prompt a Rejected turn answered; "" when
	// the turn is not Rejected. Never the text itself.
	Prompt string `json:"prompt,omitempty"`
	// PrevAt is the time of the newest message record (user or assistant,
	// any) BEFORE a Rejected turn's prompt — when the turn before it ended —
	// Unix ms on the host's clock (the CLI's, like Window.StatusSince). 0 when
	// the transcript has none before it. A Rejected turn whose host could not
	// read back that far is reported not Rejected.
	PrevAt int64 `json:"prevAt,omitempty"`
	// Quick counts the latest turns, this one first, that EACH ended in such a
	// record within QuickTurnMS of their prompt with no real reply between:
	// the send queue's circuit breaker. 0 when this turn was not one.
	Quick int `json:"quick,omitempty"`
}

// The turn error kinds muster names; any other is the CLI's own code.
const (
	// TurnErrorLimit: a usage limit.
	TurnErrorLimit = "usage_limit"
	// TurnErrorOther: an error record that names no code.
	TurnErrorOther = "error"
)

// The TurnError bounds.
const (
	// QuickTurnMS is how soon after its prompt an error must end a turn to
	// count as an immediate failure (TurnError.Quick): measured 0.3–1.0 s from
	// prompt to limit row on real transcripts; a turn that ran longer did work.
	QuickTurnMS = 10_000
	// MaxTurnErrorQuick bounds TurnError.Quick (the host stops counting).
	MaxTurnErrorQuick = 20
	// MaxTurnErrorWindows bounds how many windows of ONE list carry a
	// TurnError.
	MaxTurnErrorWindows = 16
)

var turnErrorKind = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

var promptDigest = regexp.MustCompile(`^[0-9a-f]{16}$`)

// PromptDigest is what names a prompt's text on the wire (TurnError.Prompt):
// the first 8 bytes of the SHA-256 of the text with its surrounding
// whitespace trimmed and CRLF read as LF — the host digests the prompt as the
// transcript holds it, muster the message it sent, and the two agree however
// the terminal carried the line ends. Nothing stores it.
func PromptDigest(text string) string {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:8])
}

// ValidTurnErrorKind reports whether s is a kind the wire carries.
func ValidTurnErrorKind(s string) bool { return turnErrorKind.MatchString(s) }

// CleanTurnError is e as the wire carries it: nil for no kind or one the wire
// does not carry, no negative time, a reset only on a limit, a digest and a
// PrevAt only on a Rejected turn (and a Rejected turn only with a digest),
// Quick within its bound.
func CleanTurnError(e *TurnError) *TurnError {
	if e == nil || !ValidTurnErrorKind(e.Kind) {
		return nil
	}
	out := *e
	out.At = max(out.At, 0)
	if out.Kind != TurnErrorLimit || out.ResetsAt < 0 {
		out.ResetsAt = 0
	}
	out.Quick = min(max(out.Quick, 0), MaxTurnErrorQuick)
	if !out.Rejected || !promptDigest.MatchString(out.Prompt) || out.PrevAt < 0 {
		out.Rejected, out.Prompt, out.PrevAt = false, "", 0
	}
	return &out
}

// boundTurnError applies the TurnError rules to a normalised list: none on a
// window that is not an IDLE Claude pane (a turn that ended has nothing to say
// about one still running), each cleaned, and at most MaxTurnErrorWindows
// kept — the most recent errors first, then list order.
func boundTurnError(out []Window) {
	var with []int
	for i := range out {
		if out[i].AgentRuntime() != RuntimeClaude || out[i].Status != StatusIdle {
			out[i].TurnError = nil
			continue
		}
		if out[i].TurnError = CleanTurnError(out[i].TurnError); out[i].TurnError != nil {
			with = append(with, i)
		}
	}
	if len(with) <= MaxTurnErrorWindows {
		return
	}
	sort.SliceStable(with, func(a, b int) bool { return out[with[a]].TurnError.At > out[with[b]].TurnError.At })
	for _, i := range with[MaxTurnErrorWindows:] {
		out[i].TurnError = nil
	}
}

// HostLimit is a usage limit a host reports for all its Claude Code windows
// (Msg.HostLimit): the newest limit record across them, and the reset it
// names — taken by the host from EVERY idle Claude window's transcript, before
// the window list's MaxTurnErrorWindows bound, and not reported once any of
// its Claude windows got a real answer after it (the limit has lifted).
type HostLimit struct {
	// At is that limit record's time, Unix ms on the host's clock.
	At int64 `json:"at"`
	// ResetsAt is the reset it names, Unix ms (absolute); 0 for none.
	ResetsAt int64 `json:"resetsAt,omitempty"`
}

// CleanHostLimit is l as muster keeps it: nil for none or no time, no
// negative reset.
func CleanHostLimit(l *HostLimit) *HostLimit {
	if l == nil || l.At <= 0 {
		return nil
	}
	out := *l
	out.ResetsAt = max(out.ResetsAt, 0)
	return &out
}

func turnErrorEqual(a, b *TurnError) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
