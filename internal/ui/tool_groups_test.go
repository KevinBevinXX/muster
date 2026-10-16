package ui

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/ZacxDev/muster/internal/cctranscript"
)

// shape reads a transcript's TOP-LEVEL layout out of rendered HTML, one token
// per child of the root: "T" a bubble, "R:<target>" a plain tool row, and
// "G<n>[<state>](<target>,…)" a group of n rows with its state — the rows read
// from INSIDE the group's body, the count from its summary.
//
// 🔴 IT PARSES THE MARKUP, IT DOES NOT COUNT SUBSTRINGS. A group's count is read
// from data-tool-group-count AND checked against the rows actually in its body,
// so a summary that says 3 over a body holding 2 fails here rather than reading
// as a correct "3".
func shape(t *testing.T, h string) string {
	t.Helper()
	root, err := html.Parse(strings.NewReader("<div id=root>" + h + "</div>"))
	if err != nil {
		t.Fatal(err)
	}
	var find func(*html.Node) *html.Node
	find = func(n *html.Node) *html.Node {
		if n.Type == html.ElementNode && attr(n, "id") == "root" {
			return n
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if f := find(c); f != nil {
				return f
			}
		}
		return nil
	}
	var out []string
	for c := find(root).FirstChild; c != nil; c = c.NextSibling {
		if c.Type != html.ElementNode {
			continue
		}
		switch {
		case hasAttr(c, "data-tool-row"):
			out = append(out, "R:"+targetOf(c))
		case hasAttr(c, "data-tool-group"):
			var body, count *html.Node
			walk(c, func(n *html.Node) {
				if hasAttr(n, "data-tool-group-body") && body == nil {
					body = n
				}
				if hasAttr(n, "data-tool-group-count") && count == nil {
					count = n
				}
			})
			if body == nil || count == nil {
				t.Fatalf("a group without a body or a count:\n%s", h)
			}
			var rows []string
			for r := body.FirstChild; r != nil; r = r.NextSibling {
				if r.Type == html.ElementNode {
					if !hasAttr(r, "data-tool-row") {
						t.Fatalf("a group's body holds something that is not a tool row:\n%s", h)
					}
					rows = append(rows, targetOf(r))
				}
			}
			n := attr(count, "data-tool-group-count")
			if n != fmt.Sprint(len(rows)) || text(count) != n+" tool calls" {
				t.Fatalf("a group's summary says %q (%q) over %d rows:\n%s", n, text(count), len(rows), h)
			}
			out = append(out, fmt.Sprintf("G%d[%s](%s)", len(rows), attr(c, "data-tool-status"), strings.Join(rows, ",")))
		case hasAttr(c, "data-bubble"):
			out = append(out, "T")
		case hasAttr(c, "data-notice-row"):
			out = append(out, "N")
		}
	}
	return strings.Join(out, " ")
}

func attr(n *html.Node, k string) string {
	for _, a := range n.Attr {
		if a.Key == k {
			return a.Val
		}
	}
	return ""
}

func hasAttr(n *html.Node, k string) bool {
	for _, a := range n.Attr {
		if a.Key == k {
			return true
		}
	}
	return false
}

func walk(n *html.Node, f func(*html.Node)) {
	f(n)
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walk(c, f)
	}
}

func text(n *html.Node) string {
	var b strings.Builder
	walk(n, func(x *html.Node) {
		if x.Type == html.TextNode {
			b.WriteString(x.Data)
		}
	})
	return strings.TrimSpace(b.String())
}

func targetOf(row *html.Node) string {
	var s string
	walk(row, func(n *html.Node) {
		if s == "" && hasAttr(n, "data-tool-target") {
			s = text(n)
		}
	})
	return s
}

// A transcript fixture: c("x") a call (Bash, target x), r("x", ok) its result,
// u/a a user/assistant bubble.
func c(id string) ChatLine {
	return ChatLine{Role: "assistant", Kind: "tool_call", ToolID: id, ToolName: "Bash", Content: `{"command":"` + id + `"}`}
}
func r(id string, ok bool) ChatLine {
	return ChatLine{Role: "assistant", Kind: "tool_result", ToolID: id, ToolName: "Bash", ToolOK: ok, Content: "out " + id}
}
func u(s string) ChatLine  { return ChatLine{Role: "user", Content: s} }
func as(s string) ChatLine { return ChatLine{Role: "assistant", Content: s} }

func renderLog(t *testing.T, msgs ...ChatLine) string {
	t.Helper()
	var b bytes.Buffer
	if err := chatLogInner(AgentDetailView{Name: "a", Status: "running", Messages: msgs}).Render(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// TestToolRunsGroupInThePersistedTranscript is the rule over a whole
// transcript (groupToolRuns via chatLogInner), at 0, 1, 2 and N consecutive
// calls, with text breaking a run, and with the group's state.
//
// Every expected value is a literal layout, written out by hand.
func TestToolRunsGroupInThePersistedTranscript(t *testing.T) {
	for _, tc := range []struct {
		name string
		msgs []ChatLine
		want string
	}{
		{"no calls", []ChatLine{u("hi"), as("hello")}, "T T"},
		{"one call is a plain row", []ChatLine{u("go"), c("a"), r("a", true), as("done")}, "T R:a T"},
		{"two in a row are one group", []ChatLine{u("go"), c("a"), c("b"), r("a", true), r("b", true), as("done")},
			"T G2[ok](a,b) T"},
		{"N in a row are one group", []ChatLine{c("a"), r("a", true), c("b"), r("b", true), c("c"), r("c", true), c("d"), r("d", true)},
			"G4[ok](a,b,c,d)"},
		{"text breaks a run, and stays where it was", []ChatLine{
			c("a"), c("b"), r("a", true), r("b", true), as("between"), c("c"), r("c", true), as("again"), c("d"), c("e"), c("f"),
			r("d", true), r("e", true), r("f", true)},
			"G2[ok](a,b) T R:c T G3[ok](d,e,f)"},
		{"a single call between texts is not a group", []ChatLine{as("x"), c("a"), as("y"), c("b"), as("z")},
			"T R:a T R:b T"},
		// The sentinel reply is not drawn, so it does not break the run.
		{"an undrawn sentinel does not split a run", []ChatLine{c("a"), r("a", true), as("NO_REPLY"), c("b"), r("b", true)},
			"G2[ok](a,b)"},
		{"a failure shows on the closed group", []ChatLine{c("a"), c("b"), c("c"), r("a", true), r("b", false)},
			"G3[error](a,b,c)"},
		{"an unfinished call shows as running", []ChatLine{c("a"), c("b"), r("a", true)},
			"G2[running](a,b)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := shape(t, renderLog(t, tc.msgs...)); got != tc.want {
				t.Errorf("layout\n  got  %s\n  want %s", got, tc.want)
			}
		})
	}
}

// opsShape is a MirrorOp sequence as one token per op: op, id class, into.
// Ids are replaced by the tool id they were derived from, so the expectation
// can be written by hand.
func opsShape(ops []MirrorOp, names map[string]string) []string {
	out := make([]string, 0, len(ops))
	for _, o := range ops {
		s := o.Op
		if o.ID != "" {
			s += " " + names[o.ID]
		}
		if o.Into != "" {
			s += " into " + names[o.Into]
		}
		if o.Op == "attr" {
			s += " " + o.Name + "=" + o.Value
		}
		if o.Op == "replace" && strings.HasSuffix(names[o.ID], "-s") {
			// a group's summary: what its count says
			s += " (" + strings.TrimSpace(attrValue(o.HTML, `data-tool-group-count="`)) + ")"
		}
		out = append(out, s)
	}
	return out
}

// attrValue is the value after the first occurrence of prefix (which ends in
// the opening quote), up to the closing quote.
func attrValue(s, prefix string) string {
	i := strings.Index(s, prefix)
	if i < 0 {
		return ""
	}
	s = s[i+len(prefix):]
	if j := strings.Index(s, `"`); j >= 0 {
		return s[:j]
	}
	return s
}

// TestToolRunsGroupInTheMirroredView is the same rule, incrementally: the
// mirrored transcript arrives as events and leaves as DOM operations, so the
// group is made when the SECOND call arrives (wrap) and grows after that, its
// count redrawn each time. A notice and text each end a run.
func TestToolRunsGroupInTheMirroredView(t *testing.T) {
	call := func(id string) cctranscript.Event {
		return cctranscript.Event{Kind: cctranscript.KindToolCall, ToolID: id, Tool: "Bash", Target: id}
	}
	res := func(id string, ok bool) cctranscript.Event {
		return cctranscript.Event{Kind: cctranscript.KindToolResult, ToolID: id, OK: ok, Output: "out"}
	}
	names := map[string]string{}
	for _, id := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		names[toolRowID(id)] = id
		names[toolGroupID(id)] = "G(" + id + ")"
		names[toolGroupID(id)+"-b"] = "G(" + id + ")-b"
		names[toolGroupID(id)+"-s"] = "G(" + id + ")-s"
	}
	var tr MirrorTranscript
	got := opsShape(tr.Ops([]cctranscript.Event{
		call("a"),
		call("b"),
		call("c"),
		res("b", false),
		{Kind: cctranscript.KindNotice, Notice: cctranscript.NoticeTask, Text: "a task"},
		call("d"),
		call("e"),
		{Kind: cctranscript.KindText, Text: "words"},
		call("f"),
		res("f", true),
	}), names)
	// A LATER batch continues the same run state: g follows f with nothing between.
	got = append(got, opsShape(tr.Ops([]cctranscript.Event{call("g")}), names)...)

	want := []string{
		"append a",                       // the first call: a plain row
		"wrap a", "append b into G(a)-b", // the second: a group takes a's place, b joins
		"append c into G(a)-b", "replace G(a)-s (3)", "attr G(a) data-tool-status=running",
		"replace b", "replace G(a)-s (3)", "attr G(a) data-tool-status=error", // b failed: the group says so
		"append",   // the notice ends the run
		"append d", // so d starts a new one
		"wrap d", "append e into G(d)-b",
		"append",   // text ends that run
		"append f", // a plain row again
		"replace f",
		"wrap f", "append g into G(f)-b", // across batches
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("ops\n  got  %q\n  want %q", got, want)
	}
}

// TestToolGroupShellIsTheServersMarkup pins the seam between the server's
// group and the live stream's: the script's template IS toolGroup rendered,
// carrying every hook the script fills in, so the two cannot be two designs.
func TestToolGroupShellIsTheServersMarkup(t *testing.T) {
	js := jsSource(t, toolGroupScript())
	shell := render(toolGroup("", nil))
	if !strings.Contains(js, toolGroupShellJSON()) {
		t.Fatal("toolGroupScript does not embed toolGroupShellJSON")
	}
	for _, hook := range []string{"data-tool-group", "data-tool-group-body", "data-tool-group-count", "data-tool-group-status", "data-tool-group-chevron", "<summary"} {
		if !strings.Contains(shell, hook) {
			t.Errorf("the group shell lacks %s, which the script and the stylesheet address", hook)
		}
		if hook != "<summary" && hook != "data-tool-group-chevron" && !strings.Contains(js, hook) {
			t.Errorf("the script never addresses %s", hook)
		}
	}
	// The label the script writes is the server's.
	if !strings.Contains(js, "' tool calls'") || toolGroupLabel(3) != "3 tool calls" {
		t.Error("the script's count label and toolGroupLabel disagree")
	}
}
