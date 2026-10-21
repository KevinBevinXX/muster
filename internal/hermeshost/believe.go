package hermeshost

import (
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// Believed is a Hermes window's status as muster reports it: a status file
// cross-checked against the turn lease and the newest message row
// (docs/design/hermes.md, "Believing a status file"). Status "" is UNKNOWN,
// which holds the send queue and refuses every send; Reason then says why.
type Believed struct {
	Status string
	// Since is the status file's time (Unix ms) when Status is believed, 0
	// otherwise.
	Since int64
	// Reason is why Status is unknown, "" when it is believed.
	Reason string
	// Kind is what a believed waiting status waits on — "approval",
	// "clarify" or "sudo", as the hook recorded it (S2) — "" for any other
	// status. It says which ask muster shows (Phase 4).
	Kind string
}

// The reasons a status is unknown, as -list -hermes prints them.
const (
	ReasonNoStatusFile = "no status file: the hermes-hook has written none for this session (is it configured and allowlisted? see muster-mirror -list -hermes)"
	ReasonWriterGone   = "the Hermes gateway that wrote the status has exited"
	ReasonNoLease      = "the status says a turn is running but no turn lease is live"
	ReasonLeaseLive    = "the status says idle but a turn lease is live"
	ReasonNewerMessage = "the status says idle but the conversation has a newer message"
)

// Believe is the status muster reports for a conversation whose newest status
// file is f (found false: none), given snap, read at now. alive reports
// whether a process with that pid and /proc start time still runs.
//
//   - none: unknown, naming the missing hook — never idle.
//   - busy or waiting: believed only while its writer (the gateway) is alive
//     AND the root has a live lease (expires_at > now). A stale busy is
//     unknown.
//   - idle — the dangerous direction, a false idle invites a send: believed
//     only when NO lease is live for the root AND no message in the lineage is
//     newer than the file (slack 0: the final row precedes on_session_end,
//     and a new turn's user row precedes its pre_llm_call, S2).
func Believe(f StatusFile, found bool, snap Snapshot, now time.Time, alive func(pid int, start string) bool) Believed {
	if !found {
		return Believed{Reason: ReasonNoStatusFile}
	}
	nowSec := float64(now.UnixNano()) / 1e9
	switch f.Status {
	case mirror.StatusBusy, mirror.StatusWaiting:
		if !alive(f.WriterPID, f.WriterStart) {
			return Believed{Reason: ReasonWriterGone}
		}
		if !snap.LeaseLive(nowSec) {
			return Believed{Reason: ReasonNoLease}
		}
		b := Believed{Status: f.Status, Since: f.Since}
		if f.Status == mirror.StatusWaiting {
			b.Kind = f.Kind
		}
		return b
	case mirror.StatusIdle:
		if snap.LeaseLive(nowSec) {
			return Believed{Reason: ReasonLeaseLive}
		}
		if snap.NewestMessage*1000 > float64(f.Since) {
			return Believed{Reason: ReasonNewerMessage}
		}
		return Believed{Status: mirror.StatusIdle, Since: f.Since}
	}
	return Believed{Reason: ReasonNoStatusFile}
}
