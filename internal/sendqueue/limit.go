package sendqueue

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// 🔴 A TURN THAT FAILED AT ONCE IS NOT A TURN THAT ENDED. When Claude Code
// hits a usage limit it ends the turn with a record it wrote itself ("You've
// hit your session limit · resets 10:30pm (…)") and goes back to its prompt,
// so its status reads idle with a NEWER time — exactly what Decide waits for
// after a paste. Before this, the queue then sent the next message, which the
// limit rejected the same way, and the next, until the queue was empty: every
// message consumed by the limit and lost. The host now reads how the latest
// turn ended from the transcript (mirror.TurnError), and:
//
//   - A USAGE LIMIT HOLDS THE QUEUE (holdFor). Until the reset time it names
//     plus LimitMargin; with no reset time it can place, until a real answer
//     shows the limit has lifted (the host then reports no TurnError) or the
//     owner taps Send now. A real answer lifts it at once, reset or not. The
//     limit is the ACCOUNT's, so a limit the window's host reports for all its
//     Claude windows (AgentState.HostLimit) holds a window whose own latest
//     turn says nothing.
//
//   - A MESSAGE CLAUDE CODE REFUSED IS MARKED FAILED (rejectBounced), with its
//     reason and the row's ordinary Retry — by hand, the operator's decision;
//     nothing re-sends it on its own. ONLY when the transcript shows it was
//     THIS message that was refused, and nothing happened between its paste
//     and the refusal (Bounced). A Retry of a message that was in fact
//     answered would run it twice, so in any doubt the row stays sent.
//
//   - A CIRCUIT BREAKER HOLDS ANY OTHER IMMEDIATE-FAILURE LOOP: when the
//     latest BreakerAfter turns each ended within mirror.QuickTurnMS of their
//     prompt in an error the CLI wrote itself ("Not logged in", an API error),
//     the queue holds, saying so, and lifts by itself after a back-off
//     (BreakerBackoff: 1, 2, 5, then every 15 minutes) counted from when muster
//     first saw that failure. Each message it lets through that fails at once
//     too counts one more and backs off further; a real answer resets it.
//
// Send now is the override throughout: it bypasses Decide and sends exactly
// the one message it was tapped on (Dispatcher.SendNow).

const (
	// LimitMargin is how long after a usage limit's reset the queue waits
	// before sending: the reset is the limit's own minute, and a clock a few
	// seconds off either side must not send into it.
	LimitMargin = time.Minute
	// BreakerAfter is how many turns in a row must each fail at once before
	// the breaker holds the queue.
	BreakerAfter = 2
	// BounceWithin is how long after its claim a sent message may still be
	// found refused: a refusal is written within a second or two, and the
	// host reports it within a refresh.
	BounceWithin = 10 * time.Minute
)

// BreakerBackoff is how long the breaker holds: the first trip (BreakerAfter
// quick failures in a row) for the first entry, each further quick failure the
// next, the last repeating. Counted on muster's clock from when muster first
// saw the failing turn's status (AgentState.Observed), never the host's.
var BreakerBackoff = []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 15 * time.Minute}

// What a held queue says (Decision.Why). The time it resumes, when there is
// one, is Decision.ResumeAt — drawn after these words in the READER's zone
// (internal/ui), never in the server's.
const (
	// LimitHeldText: a usage limit that names its reset.
	LimitHeldText = "Held — usage limit, resumes"
	// LimitHeldNoReset: a usage limit that names no reset time this can place.
	LimitHeldNoReset = "Held — usage limit; no reset time given. Resumes once Claude Code answers a message, or tap Send now."
	// RejectedLimitDetail is the detail of a sent message a usage limit
	// refused, marked failed (Reject).
	RejectedLimitDetail = "Rejected: usage limit — Claude Code refused it before working on it. Retry it once the limit resets."
)

// RejectedDetail is the detail of a sent message Claude Code refused with a
// turn error of kind (Reject): RejectedLimitDetail for a usage limit.
func RejectedDetail(kind string) string {
	switch kind {
	case mirror.TurnErrorLimit:
		return RejectedLimitDetail
	case "authentication_failed":
		return "Rejected: not logged in — Claude Code refused it before working on it. Retry it once it is logged in."
	}
	return "Rejected: " + kindText(kind) + " — Claude Code refused it before working on it. Retry it once that is fixed."
}

// kindText is a turn error's kind as words ("server_error" → "server error").
func kindText(kind string) string { return strings.ReplaceAll(kind, "_", " ") }

// BreakerHeldText is what a queue the circuit breaker holds says, before the
// time it retries.
func BreakerHeldText(e *mirror.TurnError) string {
	return fmt.Sprintf("Held — the last %d messages each failed at once (%s); retries at", e.Quick, kindText(e.Kind))
}

// breakerBackoff is the breaker's hold for quick failures in a row.
func breakerBackoff(quick int) time.Duration {
	return BreakerBackoff[min(max(quick-BreakerAfter, 0), len(BreakerBackoff)-1)]
}

// holdFor is the hold st's latest turn error — or its host's limit — puts on
// its queue at now: a usage limit until it resets (plus LimitMargin), or until
// a real answer when it names no reset; the breaker on BreakerAfter quick
// failures in a row, until its back-off has run from st.Observed. false: no
// hold — no error, a limit that has reset, a back-off that has run, or fewer
// quick failures. A limit that has reset is NOT then held by the breaker: the
// quick turns it counted were the limit's.
func holdFor(st AgentState, now time.Time) (Decision, bool) {
	e := st.TurnError
	if l := st.HostLimit; l != nil && (e == nil || e.Kind != mirror.TurnErrorLimit || l.At > e.At) {
		// The account is limited and this window's own latest turn does not
		// say so — it says nothing, another error, or an OLDER limit (one that
		// may have passed): the newer limit, by its record's time, is the one
		// that holds (another window hit it, or this one's report was cut by
		// the window list's bound).
		e = &mirror.TurnError{Kind: mirror.TurnErrorLimit, At: l.At, ResetsAt: l.ResetsAt}
	}
	switch {
	case e == nil:
		return Decision{}, false
	case e.Kind == mirror.TurnErrorLimit && e.ResetsAt <= 0:
		return Decision{Why: LimitHeldNoReset, Held: true}, true
	case e.Kind == mirror.TurnErrorLimit:
		reset := time.UnixMilli(e.ResetsAt)
		if now.Before(reset.Add(LimitMargin)) {
			return Decision{Why: LimitHeldText, Held: true, ResumeAt: reset}, true
		}
		return Decision{}, false
	case e.Quick >= BreakerAfter:
		retry := st.Observed.Add(breakerBackoff(e.Quick))
		if now.Before(retry) {
			return Decision{Why: BreakerHeldText(e), Held: true, ResumeAt: retry}, true
		}
	}
	return Decision{}, false
}

// Bounced reports whether sent message it — pasted into Claude Code session
// pastedIn — was REFUSED by Claude Code before it worked on it, as st shows
// its window now. Every one of these, or the row stays sent:
//
//   - the refused turn is in the SAME Claude Code session the message was
//     pasted into (st.Session == pastedIn, both known): a fresh `claude` in
//     the same pane, whose first prompt happens to be the message's text,
//     has nothing before it in ITS transcript (PrevAt 0), and is not the
//     message's refusal;
//   - the window is idle, on a status newer than the one it was pasted on
//     (it.AfterSince: the turn after the paste has ended);
//   - the latest turn ended in an error the CLI wrote itself that was its
//     prompt's ONLY reply (TurnError.Rejected: no tool ran, nothing written);
//   - that prompt's text is this message's (TurnError.Prompt, a digest);
//   - 🔴 NOTHING CAME BETWEEN this paste and that prompt: the newest record
//     before the refused prompt (TurnError.PrevAt) is no newer than the status
//     the message was pasted on. Both are the CLI's own times, on the host's
//     clock. A message that was ANSWERED, followed by another prompt typed in
//     the terminal that hit the limit, has its answer there — after its paste
//     — and stays sent, restart or not (the hub's Pasted, checked as well,
//     lives in memory and a restart forgets it).
func Bounced(it Item, pastedIn string, st AgentState) bool {
	e := st.TurnError
	return pastedIn != "" && st.Session == pastedIn &&
		it.State == Sent && st.Idle && st.Since > it.AfterSince && st.Pasted <= it.AfterSince &&
		e != nil && e.Rejected && e.Prompt == mirror.PromptDigest(it.Body) && e.PrevAt <= it.AfterSince
}

// rejectBounced marks failed (Reject, with RejectedDetail of the error's kind)
// every message sent in the last BounceWithin that Claude Code refused
// (Bounced). A row is rejected at most once: Reject moves only a row still
// sent.
func (d *Dispatcher) rejectBounced(ctx context.Context) {
	d.forgetPastes()
	items, err := d.Store.RecentSent(ctx, d.now().Add(-BounceWithin))
	if err != nil {
		d.logf("sendqueue: %v", err)
		return
	}
	for _, it := range items {
		if ctx.Err() != nil {
			return
		}
		pastedIn := d.pastedSession(it.ID)
		if pastedIn == "" {
			continue // not this dispatcher's send, or its record is gone: sent
		}
		st := d.Probe(d.AsOwner(ctx, it.Owner), Head{Item: it})
		if !Bounced(it, pastedIn, st) {
			continue
		}
		ok, err := d.Store.Reject(ctx, it.ID, RejectedDetail(st.TurnError.Kind))
		switch {
		case err != nil:
			d.logf("sendqueue: reject message %d (%s): %v", it.ID, it.Target, err)
		case ok:
			d.logf("sendqueue: message %d (%d bytes) to %s (owner %d) was refused (%s) before Claude Code worked on it; "+
				"marked failed for a Retry by hand", it.ID, len(it.Body), it.Target, it.Owner, st.TurnError.Kind)
		}
	}
}

// pasteRecord is what rememberPaste keeps of one send.
type pasteRecord struct {
	session string
	at      time.Time
}

// rememberPaste records, for a send that may have reached the window, the
// Claude session it was pasted into ("" for unknown, which Bounced never
// matches).
func (d *Dispatcher) rememberPaste(id int64, session string, o Outcome) {
	if !o.Sent && !o.MayHaveDelivered {
		return
	}
	d.pasteMu.Lock()
	defer d.pasteMu.Unlock()
	if d.pastedIn == nil {
		d.pastedIn = map[int64]pasteRecord{}
	}
	d.pastedIn[id] = pasteRecord{session: session, at: d.now()}
}

// pastedSession is the Claude session message id was pasted into by this
// dispatcher, "" when it does not know.
func (d *Dispatcher) pastedSession(id int64) string {
	d.pasteMu.Lock()
	defer d.pasteMu.Unlock()
	return d.pastedIn[id].session
}

// forgetPastes drops the records older than BounceWithin: past it no send is
// checked again.
func (d *Dispatcher) forgetPastes() {
	d.pasteMu.Lock()
	defer d.pasteMu.Unlock()
	for id, r := range d.pastedIn {
		if d.now().Sub(r.at) > BounceWithin {
			delete(d.pastedIn, id)
		}
	}
}
