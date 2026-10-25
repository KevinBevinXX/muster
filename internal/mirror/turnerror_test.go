package mirror

import (
	"strconv"
	"testing"
)

func TestCleanTurnError(t *testing.T) {
	d := PromptDigest("x")
	cases := []struct {
		name string
		in   *TurnError
		want *TurnError
	}{
		{"nil", nil, nil},
		{"a kind the wire does not carry", &TurnError{Kind: "Not Logged In"}, nil},
		{"no kind", &TurnError{At: 5}, nil},
		{"a limit keeps its reset, digest and PrevAt", &TurnError{Kind: TurnErrorLimit, At: 5, ResetsAt: 9, Rejected: true, Prompt: d, PrevAt: 4, Quick: 2},
			&TurnError{Kind: TurnErrorLimit, At: 5, ResetsAt: 9, Rejected: true, Prompt: d, PrevAt: 4, Quick: 2}},
		{"another kind has no reset", &TurnError{Kind: "server_error", At: 5, ResetsAt: 9}, &TurnError{Kind: "server_error", At: 5}},
		{"Rejected with no digest is not Rejected", &TurnError{Kind: "error", Rejected: true, PrevAt: 4}, &TurnError{Kind: "error"}},
		{"a digest in another shape: not Rejected", &TurnError{Kind: "error", Rejected: true, Prompt: "not-a-digest"}, &TurnError{Kind: "error"}},
		{"a negative PrevAt: not Rejected", &TurnError{Kind: "error", Rejected: true, Prompt: d, PrevAt: -1}, &TurnError{Kind: "error"}},
		{"a digest without Rejected is dropped", &TurnError{Kind: "error", Prompt: d, PrevAt: 4}, &TurnError{Kind: "error"}},
		{"negative times and counts", &TurnError{Kind: TurnErrorLimit, At: -1, ResetsAt: -1, Quick: -3}, &TurnError{Kind: TurnErrorLimit}},
		{"quick is capped", &TurnError{Kind: "error", Quick: MaxTurnErrorQuick + 5}, &TurnError{Kind: "error", Quick: MaxTurnErrorQuick}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := CleanTurnError(c.in)
			if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
				t.Fatalf("CleanTurnError(%+v) = %+v; want %+v", c.in, got, c.want)
			}
		})
	}
}

func TestPromptDigestAgreesAcrossLineEndsAndSurroundingSpace(t *testing.T) {
	a := PromptDigest("line one\nline two")
	for _, s := range []string{"line one\r\nline two", "  line one\nline two\n", "\tline one\r\nline two\r\n"} {
		if got := PromptDigest(s); got != a {
			t.Errorf("PromptDigest(%q) = %s; want %s", s, got, a)
		}
	}
	if PromptDigest("line one\nline three") == a || len(a) != 16 {
		t.Fatalf("digest %q: different text, same digest, or not 16 hex", a)
	}
}

func TestCleanHostLimit(t *testing.T) {
	if CleanHostLimit(nil) != nil || CleanHostLimit(&HostLimit{At: 0, ResetsAt: 5}) != nil {
		t.Fatal("a host limit with no time is kept")
	}
	if got := CleanHostLimit(&HostLimit{At: 3, ResetsAt: -1}); got == nil || *got != (HostLimit{At: 3}) {
		t.Fatalf("CleanHostLimit = %+v", got)
	}
}

// TestATurnErrorRidesOnlyAnIdleClaudeWindowAndAtMostTheBound: NormalizeWindows
// drops one on a busy, waiting or non-Claude window, and keeps it on at most
// MaxTurnErrorWindows windows of a list, the most recent first.
func TestATurnErrorRidesOnlyAnIdleClaudeWindowAndAtMostTheBound(t *testing.T) {
	e := func(at int64) *TurnError { return &TurnError{Kind: TurnErrorLimit, At: at} }
	in := []Window{
		{Session: "a", ID: "@1", Claude: true, Status: StatusBusy, StatusSince: 1, TurnError: e(1)},
		{Session: "a", ID: "@2", Claude: true, Status: StatusWaiting, StatusSince: 1, TurnError: e(1)},
		{Session: "a", ID: "@3", Command: "zsh", TurnError: e(1)},
		{Session: "a", ID: "@4", Runtime: RuntimeHermes, Status: StatusIdle, StatusSince: 1, TurnError: e(1)},
		{Session: "a", ID: "@5", Claude: true, Status: StatusIdle, StatusSince: 1, TurnError: e(1)},
	}
	for i, w := range NormalizeWindows(in, 0) {
		if (w.ID == "@5") != (w.TurnError != nil) {
			t.Fatalf("window %d (%s, %q) carries %+v; want only the idle Claude window's", i, w.ID, w.Status, w.TurnError)
		}
	}
	var many []Window
	for i := 0; i < MaxTurnErrorWindows+4; i++ {
		many = append(many, Window{Session: "s", ID: "@" + strconv.Itoa(i+1), Index: i, Claude: true, Status: StatusIdle, StatusSince: 1,
			TurnError: e(int64(1000 + i))})
	}
	out := NormalizeWindows(many, 0)
	kept := 0
	for _, w := range out {
		if w.TurnError != nil {
			kept++
			if w.TurnError.At < int64(1000+4) {
				t.Fatalf("kept an older error (%d) over a newer one", w.TurnError.At)
			}
		}
	}
	if kept != MaxTurnErrorWindows {
		t.Fatalf("kept %d; want %d", kept, MaxTurnErrorWindows)
	}
	// A window list whose turn error moved is not Equal: the host re-sends it.
	a, b := out[0], out[0]
	b.TurnError = &TurnError{Kind: "error"}
	if a.Equal(b) || !a.Equal(out[0]) {
		t.Fatal("Window.Equal does not see the turn error")
	}
}
