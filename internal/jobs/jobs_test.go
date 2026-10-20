package jobs

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// TestClassify pins how a finished turn becomes a job outcome, with literal
// expected values.
func TestClassify(t *testing.T) {
	transport := errors.New(`Post "http://agent/v1/responses": context deadline exceeded`)
	for _, tc := range []struct {
		name     string
		reply    string
		err      error
		deadline bool
		want     Outcome
	}{
		{"success keeps the reply verbatim", " a\n\tb \n", nil, false,
			Outcome{Status: StatusSucceeded, Result: " a\n\tb \n"}},
		{"rate_limited is failed with ccd's type and message", "", &agents.RuntimeError{Status: 429, Type: "rate_limited",
			Message: "You've hit your session limit · resets 9:45pm"}, false,
			Outcome{Status: StatusFailed, Reason: "rate_limited", Error: "You've hit your session limit · resets 9:45pm"}},
		{"auth_failed likewise", "", &agents.RuntimeError{Status: 502, Type: "auth_failed", Message: "Invalid API key"}, false,
			Outcome{Status: StatusFailed, Reason: "auth_failed", Error: "Invalid API key"}},
		{"a wrapped typed failure is still seen", "", fmt.Errorf("turn: %w", &agents.RuntimeError{Status: 409, Type: "busy", Message: "a turn is already running"}), false,
			Outcome{Status: StatusFailed, Reason: "busy", Error: "a turn is already running"}},
		{"ccd's own turn budget is a timeout", "", &agents.RuntimeError{Status: 504, Type: "turn_timeout", Message: "no Stop within 30m"}, false,
			Outcome{Status: StatusTimedOut, Reason: "turn_timeout", Error: "no Stop within 30m"}},
		{"the job's deadline wins over the error it caused", "", transport, true,
			Outcome{Status: StatusTimedOut, Reason: ReasonTimeout, Error: "the job's timeout elapsed before the agent's turn finished: " + transport.Error()}},
		{"the deadline wins over a typed failure too", "", &agents.RuntimeError{Status: 429, Type: "rate_limited", Message: "x"}, true,
			Outcome{Status: StatusTimedOut, Reason: ReasonTimeout, Error: "the job's timeout elapsed before the agent's turn finished: responses HTTP 429: "}},
		{"an untyped error is turn_failed", "", errors.New("dial tcp: connection refused"), false,
			Outcome{Status: StatusFailed, Reason: ReasonTurnFailed, Error: "dial tcp: connection refused"}},
		{"a 429 with no typed body is not rate_limited", "", &agents.RuntimeError{Status: 429}, false,
			Outcome{Status: StatusFailed, Reason: ReasonTurnFailed, Error: "responses HTTP 429: "}},
	} {
		if got := Classify(tc.reply, tc.err, tc.deadline); got != tc.want {
			t.Errorf("%s:\n got %#v\nwant %#v", tc.name, got, tc.want)
		}
	}
}

func TestValidSHA(t *testing.T) {
	for s, want := range map[string]bool{
		"0123abc": true, "0123456789abcdef0123456789abcdef01234567": true,
		"": false, "0123ab": false, "0123456789abcdef0123456789abcdef012345678": false,
		"0123ABC": false, "HEAD": false, "main": false, "0123abc;rm": false, "-0123abc": false,
	} {
		if got := ValidSHA(s); got != want {
			t.Errorf("ValidSHA(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestValidResultPath(t *testing.T) {
	for p, want := range map[string]bool{
		"review.md": true, "out/review.md": true, ".github/out.txt": true, "a..b/c": true,
		"": false, "/etc/passwd": false, "../x": false, "a/../../x": false, "./x": false, "a/./b": false,
		"a//b": false, "a/": false, "..": false, ".": false, "a\x00b": false, strings.Repeat("a", 513): false,
	} {
		if got := ValidResultPath(p); got != want {
			t.Errorf("ValidResultPath(%q) = %v, want %v", p, got, want)
		}
	}
}

// TestNormalizeKeepsARowTheDatabaseAccepts: an outcome that would break a
// CHECK becomes a failed one, so Finish never leaves a job running for ever.
func TestNormalizeKeepsARowTheDatabaseAccepts(t *testing.T) {
	big := strings.Repeat("x", MaxResultBytes+1)
	if got := normalize(Outcome{Status: StatusSucceeded, Result: big}); got.Status != StatusFailed || got.Reason != ReasonInternal {
		t.Fatalf("an oversize result normalized to %s/%s, want failed/internal", got.Status, got.Reason)
	}
	if got := normalize(Outcome{Status: StatusSucceeded, Result: "a\x00b"}); got.Status != StatusFailed {
		t.Fatalf("a NUL result normalized to %s, want failed", got.Status)
	}
	if got := normalize(Outcome{Status: StatusFailed}); got.Reason != ReasonInternal {
		t.Fatalf("a failure with no reason normalized to reason %q, want internal", got.Reason)
	}
	long := strings.Repeat("é", MaxErrorBytes)
	got := normalize(Outcome{Status: StatusFailed, Reason: "x", Error: long})
	if len(got.Error) > MaxErrorBytes || !strings.HasPrefix(long, got.Error) || !utf8Valid(got.Error) {
		t.Fatalf("a long error normalized to %d bytes (valid UTF-8: %v)", len(got.Error), utf8Valid(got.Error))
	}
	if got := normalize(Outcome{Status: StatusRunning}); got.Status != StatusFailed {
		t.Fatalf("a non-terminal outcome normalized to %s, want failed", got.Status)
	}
}

func utf8Valid(s string) bool { return strings.ToValidUTF8(s, "\uFFFD") == s }
