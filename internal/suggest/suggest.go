// Package suggest is SUGGESTED REPLIES: up to three short drafts the operator
// can tap into a mirrored session's composer when the agent has handed control
// back ("Which do you prefer?", "Should I open the PR?").
//
// 🔴 A SUGGESTION IS NEVER SENT BY MUSTER. A tap only fills the composer (and
// appends below a draft that is already there); the operator edits it and
// presses Send, which keeps its own step-up gate. Nothing in this package can
// reach a pane: it produces text, and the only consumer is the composer.
//
// The pieces, in the order a suggestion passes through them:
//
//   - HandsBack: the cheap deterministic pre-gate. A final reply that does not
//     ask a question, offer options or say "should I" gets no model call.
//   - Build: the model's input, exactly three parts — the operator's own
//     messages (numbered A<n> from the prompt index, newest first), the
//     session's focus history (labelled untrusted), and the transcript tail
//     (the final reply R, its END kept, and the few entries before it). Every
//     field goes through redact.Secrets first; a tool call is its name and file
//     path only; tool output and thinking never enter it.
//   - Generator (OpenRouter): one chat call, strict JSON out.
//   - Filter: the deterministic post-filter. A suggestion survives only when
//     its citations exist, its quote is verbatim in a cited source, and it
//     passes the shape rules (no leading / or !, one line, no URL, no code
//     fence, no secret). Zero survivors means no
//     chips, which is a correct outcome.
//   - Engine: the cache (by input fingerprint: a new turn regenerates, the same
//     input never calls again), the rate cap, the kill switch and the
//     per-user toggle.
//
// 🔴 SUGGESTION TEXT IS NEVER STORED. It lives in the Engine's in-memory cache
// for at most CacheTTL and is then dropped (Engine.Sweep); only the input's
// fingerprint outlives it, so an unchanged session is not paid for twice.
package suggest

import (
	"errors"
	"strconv"
)

// The five kinds a suggestion can be. The model is told what each means
// (Instructions) and the filter refuses any other.
const (
	// KindPick chooses one of the options the agent offered (or combines them).
	KindPick = "pick"
	// KindGap points at an operator ask the reply did not address.
	KindGap = "gap"
	// KindVerify asks for a check the agent skipped or deferred.
	KindVerify = "verify"
	// KindDecide approves or declines a proposed action, WITH its reason or
	// condition.
	KindDecide = "decide"
	// KindRedirect narrows the work or stops it.
	KindRedirect = "redirect"
)

// Kinds is every kind, in the order the instructions list them.
var Kinds = []string{KindPick, KindGap, KindVerify, KindDecide, KindRedirect}

// ValidKind reports whether k is one of Kinds.
func ValidKind(k string) bool {
	for _, x := range Kinds {
		if k == x {
			return true
		}
	}
	return false
}

// The bounds of what is generated and what is kept.
const (
	// MaxSuggestions is how many chips a set holds at most.
	MaxSuggestions = 3
	// MaxTextRunes bounds one suggestion's text.
	MaxTextRunes = 140
	// MinQuoteBytes is the shortest quote that counts as grounding (a shorter
	// one matches almost anything), unless it is the whole cited source.
	MinQuoteBytes = 6
)

// Suggestion is one chip: what a tap fills, and its kind. It is what the
// endpoint serves; the citation and quote it was checked against stay here.
type Suggestion struct {
	Text string `json:"text"`
	Kind string `json:"kind"`
}

// Raw is one suggestion as the model wrote it, before Filter.
type Raw struct {
	Text  string   `json:"text"`
	Kind  string   `json:"kind"`
	Cites []string `json:"cites"`
	Quote string   `json:"quote"`
}

// The errors a generation can end in. Each maps to a short fixed Reason.
var (
	ErrNoKey       = errors.New("suggest: no OpenRouter key for this owner")
	ErrKeyLookup   = errors.New("suggest: the owner's OpenRouter key could not be read")
	ErrRateLimited = errors.New("suggest: the provider is rate-limiting (429)")
	ErrMalformed   = errors.New("suggest: the model's answer is not the JSON asked for")
	ErrOversized   = errors.New("suggest: the model's answer is larger than a suggestion set can be")
	ErrUnreachable = errors.New("suggest: the provider could not be reached")
	// ErrCapped is a generation the rate cap refused: nothing was called.
	ErrCapped = errors.New("suggest: the suggestion rate cap is reached; try again later")
)

// The fixed reasons a failed or skipped generation records (Reason). Short,
// and carrying nothing from the transcript, the key or the provider's body,
// in focus's vocabulary (internal/focus Reason*).
const (
	ReasonNoKey       = "no OpenRouter key"
	ReasonKeyLookup   = "the OpenRouter key could not be read"
	ReasonRateLimited = "rate-limited"
	ReasonUnusable    = "the suggester's answer was unusable"
	ReasonUnreachable = "the suggester could not be reached"
	ReasonFailed      = "the suggestion failed"
)

// Reason is err as a short, non-secret reason.
func Reason(err error) string {
	var he *HTTPError
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrNoKey):
		return ReasonNoKey
	case errors.Is(err, ErrKeyLookup):
		return ReasonKeyLookup
	case errors.Is(err, ErrRateLimited), errors.Is(err, ErrCapped):
		return ReasonRateLimited
	case errors.As(err, &he):
		return "suggester error (HTTP " + strconv.Itoa(he.Status) + ")"
	case errors.Is(err, ErrMalformed), errors.Is(err, ErrOversized):
		return ReasonUnusable
	case errors.Is(err, ErrUnreachable):
		return ReasonUnreachable
	}
	return ReasonFailed
}
