package hermeshost

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// THE HOOK's CORE (`muster-mirror hermes-hook`, cmd/muster-mirror): one Hermes
// shell-hook event in, at most one status file out.
//
// Hermes runs a shell hook with a JSON payload on stdin —
// {hook_event_name, tool_name, tool_input, session_id, cwd, profile, extra}
// (P:agent/shell_hooks.py, _payload_fields) — synchronously, in the GATEWAY
// (S2), and parses its stdout as a directive. So the command around this:
//
//   - 🔴 PRINTS NOTHING TO STDOUT: `{"decision": "block"}` or `{"context": …}`
//     there would block an action or inject text into the prompt;
//   - 🔴 ALWAYS EXITS 0, never the "block" exit code 2;
//   - never blocks: the lock wait is bounded (lockWait) and the declared
//     timeout is short; on any failure it writes nothing, which reads as
//     unknown, never idle;
//   - 🔴 NEVER READS TMUX_PANE OR THE TTY. The gateway's environment holds the
//     TMUX_PANE of whichever pane auto-started it, which is wrong for every
//     other pane — worse than absent (S2). Nothing here takes the
//     environment.

// The events the hook is registered for, and what each writes.
const (
	EventPreLLMCall         = "pre_llm_call"
	EventHumanInputRequest  = "on_human_input_request"
	EventHumanInputResolved = "on_human_input_resolved"
	EventSessionEnd         = "on_session_end"
	EventSessionFinalize    = "on_session_finalize"
)

// HookEvents is every event the host's Hermes config must register the hook
// for, in the order the docs list them. The allowlist writer makes exactly
// these (event, command) pairs present.
var HookEvents = []string{EventPreLLMCall, EventHumanInputRequest, EventHumanInputResolved, EventSessionEnd, EventSessionFinalize}

// hookStatus is the status each event writes.
var hookStatus = map[string]string{
	EventPreLLMCall:         mirror.StatusBusy,
	EventHumanInputRequest:  mirror.StatusWaiting,
	EventHumanInputResolved: mirror.StatusBusy,
	EventSessionEnd:         mirror.StatusIdle,
	EventSessionFinalize:    mirror.StatusIdle,
}

// hookPayload is the part of Hermes's payload the hook reads. 🔴 NOTHING ELSE
// IS DECODED: extra.prompt (the approval's command, the clarify's question)
// and tool_input never reach a variable.
type hookPayload struct {
	Event     string `json:"hook_event_name"`
	SessionID string `json:"session_id"`
	Extra     struct {
		Kind      string `json:"kind"`
		RequestID string `json:"request_id"`
	} `json:"extra"`
}

// maxHookPayload bounds what the hook reads from stdin.
const maxHookPayload = 1 << 20

// ErrIgnored: the event is not one the hook writes for, or names no valid
// session; nothing was written, and that is not a failure.
var ErrIgnored = errors.New("ignored")

// Writer is the process the hook runs under (the gateway): its pid and /proc
// start time.
type Writer struct {
	PID   int
	Start string
}

// HandleHook writes the status file payload's event calls for, into dir, at
// now, recording writer. ErrIgnored for an event it does not handle or a
// session id it does not accept (the gateway also fires hooks for API, cron
// and bot sessions, whose ids are validated the same way).
func HandleHook(payload []byte, dir StatusDir, now time.Time, writer Writer) error {
	if len(payload) > maxHookPayload {
		return fmt.Errorf("%w: the payload is larger than %d bytes", ErrIgnored, maxHookPayload)
	}
	var p hookPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("the payload is not JSON: %w", err)
	}
	status, ok := hookStatus[p.Event]
	if !ok {
		return fmt.Errorf("%w: event %q", ErrIgnored, p.Event)
	}
	// 🔴 THE SESSION ID BECOMES A FILE NAME: validated against the wire's
	// session-id pattern, and anything else is ignored.
	if !mirror.ValidAgentSession(p.SessionID) {
		return fmt.Errorf("%w: not a session id", ErrIgnored)
	}
	f := StatusFile{SessionID: p.SessionID, Status: status, Since: now.UnixMilli(), WriterPID: writer.PID, WriterStart: writer.Start}
	if status == mirror.StatusWaiting {
		if !waitingKinds[p.Extra.Kind] {
			return fmt.Errorf("%w: a request of kind %q", ErrIgnored, p.Extra.Kind)
		}
		f.Kind = p.Extra.Kind
		if requestIDPattern.MatchString(p.Extra.RequestID) {
			f.RequestID = p.Extra.RequestID
		}
	}
	err := dir.Write(f)
	if err == nil && p.Event == EventSessionFinalize {
		dir.prune(now)
	}
	return err
}

// statusRetention: a status file untouched this long is removed when a
// session is finalised (the gateway also runs API, cron and bot sessions, each
// leaving a file).
const statusRetention = 30 * 24 * time.Hour

// prune removes status files older than statusRetention. Best-effort.
func (d StatusDir) prune(now time.Time) {
	ents, err := os.ReadDir(string(d))
	if err != nil {
		return
	}
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".json" {
			continue
		}
		if !mirror.ValidAgentSession(name[:len(name)-len(".json")]) {
			continue
		}
		if fi, err := e.Info(); err == nil && now.Sub(fi.ModTime()) > statusRetention {
			_ = os.Remove(filepath.Join(string(d), name))
		}
	}
}
