package hermeshost

import "testing"

// Every pinned non-terminal gate's label shape is caught, and a terminal
// command that merely contains a '<' or a gate's name is not (audit #212
// round 3). TestTheApprovalGatesMatchThePin (livehermes) evaluates the pin's
// own label f-strings through the same function.
func TestNonTerminalLabelCatchesEveryPinnedLabelShape(t *testing.T) {
	for _, s := range []string{
		"<sql_query> (plugin approval rule)",
		"<terminal> (plugin approval rule)",
		"<> (plugin approval rule)",
		"<a> b> (plugin approval rule)",
		"<write to ~/.ssh/config>",
		"<write to AGENTS.md, CLAUDE.md>",
		"computer_use: click 120,300",
		"execute_code <<'PY'\nprint(1)\nPY",
	} {
		if !NonTerminalLabel(s) {
			t.Errorf("%q: not caught", s)
		}
	}
	for _, s := range []string{
		"rm -r ./build",
		"<in.txt sort",
		"sort <in.txt >out.txt",
		"echo computer_use: x",
		"echo '<write to x>'",
		"deploy (plugin approval rule)",
		"<",
		"<write to >x",
	} {
		if NonTerminalLabel(s) {
			t.Errorf("%q: caught, but no pinned gate shows it", s)
		}
	}
}
