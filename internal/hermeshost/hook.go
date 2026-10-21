package hermeshost

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
func HandleHook(in io.Reader, dir StatusDir, now time.Time, writer Writer) error {
	p, err := decodeHook(in)
	if err != nil {
		return err
	}
	status, ok := hookStatus[p.Event]
	if !ok {
		return fmt.Errorf("%w: event %q", ErrIgnored, capField(p.Event))
	}
	// 🔴 THE SESSION ID BECOMES A FILE NAME: validated against the wire's
	// session-id pattern, and anything else is ignored.
	if !mirror.ValidAgentSession(p.SessionID) {
		return fmt.Errorf("%w: not a session id", ErrIgnored)
	}
	f := StatusFile{SessionID: p.SessionID, Status: status, Since: now.UnixMilli(), WriterPID: writer.PID, WriterStart: writer.Start}
	if status == mirror.StatusWaiting {
		if !waitingKinds[p.Extra.Kind] {
			return fmt.Errorf("%w: a request of kind %q", ErrIgnored, capField(p.Extra.Kind))
		}
		f.Kind = p.Extra.Kind
		if mirror.ValidPromptID(p.Extra.RequestID) {
			f.RequestID = p.Extra.RequestID
		}
	}
	return dir.Write(f)
}

// capField bounds a payload field quoted in an error.
func capField(s string) string {
	if len(s) > 64 {
		return s[:64] + "…"
	}
	return s
}

// decodeHook reads the four fields the hook needs — hook_event_name,
// session_id, extra.kind, extra.request_id — from a payload of ANY size,
// streaming. 🔴 THERE IS NO SIZE CAP: Hermes's pre_llm_call payload carries
// the whole conversation (megabytes on a long one), and a cap would drop
// exactly the turns of long conversations, which then never show busy.
// Every other value is skipped token by token (json.Decoder), so memory is
// bounded by the largest single token, never by the payload; nothing skipped
// is kept.
func decodeHook(in io.Reader) (hookPayload, error) {
	var p hookPayload
	dec := json.NewDecoder(in)
	bad := errors.New("the payload is not a JSON object")
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return p, bad
	}
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return p, bad
		}
		switch k {
		case "hook_event_name":
			p.Event, err = stringValue(dec)
		case "session_id":
			p.SessionID, err = stringValue(dec)
		case "extra":
			err = decodeExtra(dec, &p)
		default:
			err = skipValue(dec)
		}
		if err != nil {
			return p, bad
		}
	}
	return p, nil
}

// decodeExtra reads extra.kind and extra.request_id, skipping the rest (a
// prompt, a conversation, …); a non-object extra is skipped whole.
func decodeExtra(dec *json.Decoder, p *hookPayload) error {
	t, err := dec.Token()
	if err != nil {
		return err
	}
	if t != json.Delim('{') {
		return skipRest(dec, t)
	}
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return err
		}
		switch k {
		case "kind":
			p.Extra.Kind, err = stringValue(dec)
		case "request_id":
			p.Extra.RequestID, err = stringValue(dec)
		default:
			err = skipValue(dec)
		}
		if err != nil {
			return err
		}
	}
	_, err = dec.Token() // '}'
	return err
}

// stringValue is the next value when it is a string, "" for any other value
// (skipped).
func stringValue(dec *json.Decoder) (string, error) {
	t, err := dec.Token()
	if err != nil {
		return "", err
	}
	if s, ok := t.(string); ok {
		return s, nil
	}
	return "", skipRest(dec, t)
}

// skipValue skips the next value.
func skipValue(dec *json.Decoder) error {
	t, err := dec.Token()
	if err != nil {
		return err
	}
	return skipRest(dec, t)
}

// skipRest skips the rest of a value whose first token was t.
func skipRest(dec *json.Decoder, t json.Token) error {
	d, ok := t.(json.Delim)
	if !ok || d == '}' || d == ']' {
		return nil
	}
	for depth := 1; depth > 0; {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := t.(json.Delim); ok {
			if d == '{' || d == '[' {
				depth++
			} else {
				depth--
			}
		}
	}
	return nil
}
