package main

import (
	"net/http"
	"strings"

	"github.com/ZacxDev/muster/internal/podagent"
)

// muster's agent wire is internal/podagent's (WriteResponse, WriteFailure,
// PromptFrom), shared with ccd; its rules — a failed turn is always a typed
// non-200, never a 200 with an empty reply and never a 404, and nothing streams
// during a turn — are stated there. hermesd's part is its failure vocabulary.

type failure = podagent.Failure

func writeFailure(w http.ResponseWriter, f *failure) { podagent.WriteFailure(w, f) }

const (
	failUnauthorized = "unauthorized"  // 401: the caller's bearer is wrong
	failBadRequest   = "bad_request"   // 400
	failBusy         = "busy"          // 409: a turn on this session has not settled
	failNotReady     = "not_ready"     // 503: the gateway is not started, recovering, or the checkout is not there
	failTurnTimeout  = "turn_timeout"  // 504: the run outlasted the turn budget and was stopped
	failRuntime      = "runtime_error" // 502: run.failed
	failInterrupted  = "interrupted"   // 502: run.interrupted / run.cancelled (a gateway shutdown, a stop); not retried
	failRateLimited  = "rate_limited"  // 429: the provider refused the turn for a rate limit
	failEmptyReply   = "empty_reply"   // 502: the run completed with no answer text
	// failMayHaveRun is a turn whose outcome hermesd cannot know: the POST's
	// answer was lost, or Hermes said the stored idempotency key was used with a
	// different body. 🔴 It is deliberately NOT not_ready, which muster re-sends:
	// a re-send of a turn that did run runs it twice.
	failMayHaveRun = "may_have_run" // 502
)

// mayHaveRunText is what a person reads for failMayHaveRun.
const mayHaveRunText = "the turn may have run; check the conversation before sending it again"

func notReady(why string) *failure {
	return &failure{Status: http.StatusServiceUnavailable, Type: failNotReady, Message: why}
}

// classifyRunFailure maps a run.failed error text to a failure. A provider rate
// limit is its own type; muster keys nothing on it for this kind (it marks only
// a Claude account), but the card names it.
func classifyRunFailure(errText string) *failure {
	if errText == "" {
		errText = "the Hermes run failed with no message"
	}
	low := strings.ToLower(errText)
	if strings.Contains(low, "429") || strings.Contains(low, "rate limit") || strings.Contains(low, "rate_limit") {
		return &failure{Status: http.StatusTooManyRequests, Type: failRateLimited, Message: errText}
	}
	return &failure{Status: http.StatusBadGateway, Type: failRuntime, Message: errText}
}
