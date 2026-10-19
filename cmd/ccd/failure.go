package main

import (
	"encoding/json"
	"net/http"
)

// A failed turn is ALWAYS a non-200 with a JSON body naming a stable type.
//
// 🔴 NEVER A 200 WITH AN EMPTY REPLY. muster's client (internal/agents
// RunToollessTurn) assembles the reply from the stream and returns it with a nil
// error; a 200 whose stream carries no text is, to every caller, "the agent
// answered nothing" — a kickoff recorded as delivered, a chat bubble left blank.
// The CLI itself makes this mistake easy to copy: a 401 shows up in `-p` JSON as
// {"subtype":"success","is_error":true,"api_error_status":401} with exit code 0
// in a pipeline, so neither `subtype` nor the exit status can be trusted.
//
// 🔴 AND NEVER A 404. muster's client maps exactly 404 to "this runtime has no
// /v1/responses" and retries the turn on /v1/chat/completions — which ccd does not
// serve, so a 404 here would turn a real failure into a misleading transport error.
//
// The `type` vocabulary is the contract a muster-side account pool keys on: an
// account whose turns come back `auth_failed` needs a new token; one that comes
// back `rate_limited` is healthy and should be rested until its reset time, which
// the CLI states in `message` ("You've hit your session limit · resets 9:45pm").
const (
	failUnauthorized = "unauthorized"  // 401: the caller's bearer is wrong (NOT the model account)
	failBadRequest   = "bad_request"   // 400
	failInvalidInput = "invalid_input" // 400: the prompt holds a control character (server.go sanitizePrompt)
	failBusy         = "busy"          // 409: a turn is already running in the session
	failNotReady     = "not_ready"     // 503: the TUI has not reached its prompt
	failNotSubmitted = "not_submitted" // 504: no UserPromptSubmit matching the pasted prompt (server.go reportedPromptMatches)
	failLocalCommand = "local_command" // 502: a `/` prompt the CLI ran locally — no model turn, no reply
	failTurnTimeout  = "turn_timeout"  // 504: no Stop/StopFailure within the turn budget
	failTerminal     = "terminal"      // 502: tmux could not be driven
	failTranscript   = "transcript"    // 502: the reply could not be read back
	failAuth         = "auth_failed"   // 502: the model account's credential was rejected
	failRateLimited  = "rate_limited"  // 429: the model account hit a usage limit
	failBilling      = "billing_error" // 502
	failTurn         = "turn_failed"   // 502: any other API failure
	failEmptyReply   = "empty_reply"   // 502: the turn ended with no answer text
	failCapacity     = "capacity"      // 503: every Claude Code session ccd may run at once is mid-turn (lanes.go)
)

type failure struct {
	Status   int    `json:"-"`
	Type     string `json:"type"`
	Code     string `json:"code,omitempty"`            // the CLI's own error code, when there is one
	Upstream int    `json:"upstream_status,omitempty"` // the API's HTTP status, when known
	Message  string `json:"message"`
}

func (f *failure) Error() string { return f.Type + ": " + f.Message }

func writeFailure(w http.ResponseWriter, f *failure) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(f.Status)
	_ = json.NewEncoder(w).Encode(map[string]*failure{"error": f})
}

// classifyAPIError maps the CLI's error code and the upstream status to a failure.
//
// The CODE is checked first because it is the CLI's own classification and is
// present even when no HTTP status is (a "Not logged in" record carries
// error=authentication_failed and no apiErrorStatus). The status is the fallback
// for codes this list does not know.
//
// ⚠ OWED: the rate-limit row is backed by real transcript records
// (error=rate_limit, apiErrorStatus=429, "You've hit your session|weekly limit ·
// resets …") written by an OLDER CLI than the one the image pins, and by no
// StopFailure hook payload at all — a limit was never hit while recording.
// CLOSING CONDITION: one rate-limited turn through ccd on the pinned CLI, whose
// StopFailure payload and transcript record are captured into testdata/ and
// asserted here; checked by whoever runs the operator live check and first sees
// a limit. Capture: run the pod with CCD_HOOK_LOG=/data/claude/hooks.jsonl (every
// raw hook payload is appended there) and copy the StopFailure line plus the
// transcript's isApiErrorMessage record, with content replaced, into testdata/.
//
// 🔴 THE PR THAT CLOSES THIS ALSO REMOVES CCD_HOOK_LOG (serverConfig.HookLog and
// its env wiring in main.go). It exists only to make this capture possible; it is
// off by default, and when on it writes every prompt's text to the volume.
func classifyAPIError(e apiError) *failure {
	f := &failure{Code: e.Code, Upstream: e.Status, Message: e.Message}
	switch {
	case e.Code == "rate_limit":
		f.Status, f.Type = http.StatusTooManyRequests, failRateLimited
	case e.Code == "authentication_failed":
		f.Status, f.Type = http.StatusBadGateway, failAuth
	case e.Code == "billing_error":
		f.Status, f.Type = http.StatusBadGateway, failBilling
	case e.Status == http.StatusTooManyRequests:
		f.Status, f.Type = http.StatusTooManyRequests, failRateLimited
	case e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden:
		f.Status, f.Type = http.StatusBadGateway, failAuth
	default:
		f.Status, f.Type = http.StatusBadGateway, failTurn
	}
	if f.Message == "" {
		f.Message = "the turn failed with no message from the CLI"
	}
	return f
}
