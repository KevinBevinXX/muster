package termsize

import (
	"bytes"
	"testing"
)

// TestDecideNeverShrinksTheWindow: every case where the request is smaller in
// EITHER dimension keeps the window's size; a request that covers it (a
// desktop) is granted as asked. The sizes are pairwise distinct so a mutant
// that returns the wrong operand, or mixes cols and rows, cannot pass.
func TestDecideNeverShrinksTheWindow(t *testing.T) {
	win := Size{201, 53}
	for name, tc := range map[string]struct {
		req, want Size
	}{
		"a phone (narrower and shorter)": {Size{47, 29}, win},
		"narrower, taller (portrait)":    {Size{47, 71}, win},
		"wider, shorter":                 {Size{233, 31}, win},
		"exactly the window":             {win, win},
		"a desktop that covers it":       {Size{239, 61}, Size{239, 61}},
		"wider only by a column":         {Size{202, 53}, Size{202, 53}},
		"no request size":                {Size{}, win},
		"half a request size":            {Size{90, 0}, win},
	} {
		if got := Decide(tc.req, win); got != tc.want {
			t.Errorf("%s: Decide(%v, %v) = %v, want %v", name, tc.req, win, got, tc.want)
		}
	}
	// An unknown window: the request stands (the far side's default otherwise).
	if got := Decide(Size{47, 29}, Size{}); got != (Size{47, 29}) {
		t.Errorf("unknown window: %v", got)
	}
}

func TestTheReportRoundTrips(t *testing.T) {
	b := Report(Size{211, 57})
	if !bytes.Equal(b, []byte("\x1b]7701;211;57\x07")) {
		t.Fatalf("Report = %q", b)
	}
	got, n, ok := ParsePrefix(append(b, "rest"...))
	if !ok || got != (Size{211, 57}) || n != len(b) {
		t.Fatalf("ParsePrefix = %v %d %v", got, n, ok)
	}
	for _, bad := range []string{"rest\x1b]7701;1;2\x07", "\x1b]7702;1;2\x07", "\x1b]7701;1\x07", "\x1b]7701;a;2\x07"} {
		if _, _, ok := ParsePrefix([]byte(bad)); ok {
			t.Errorf("ParsePrefix(%q) accepted", bad)
		}
	}
}

// TestParseTmuxWindowAddsTheStatusLine: the client size is the window plus
// the status line's rows.
func TestParseTmuxWindowAddsTheStatusLine(t *testing.T) {
	for in, want := range map[string]Size{
		"160 44 on\n": {Cols: 160, Rows: 45},
		"160 44 off":  {Cols: 160, Rows: 44},
		"160 44 3":    {Cols: 160, Rows: 47},
	} {
		got, err := ParseTmuxWindow(in)
		if err != nil || got != want {
			t.Errorf("ParseTmuxWindow(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "160 44", "160 x on", "0 44 on", "160 44 9", "160 44 maybe"} {
		if _, err := ParseTmuxWindow(bad); err == nil {
			t.Errorf("ParseTmuxWindow(%q) accepted", bad)
		}
	}
}
