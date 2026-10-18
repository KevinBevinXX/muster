package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
)

// TestTheQueueSortsNewestFirstWithAStableTieBreak: newest Since first; equal
// times by Key (whatever order the sources were read in); an item with no time
// last.
func TestTheQueueSortsNewestFirstWithAStableTieBreak(t *testing.T) {
	t0 := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	for _, in := range [][]attentionItem{
		{{Key: "ask-1", Since: t0}, {Key: "session-b", Since: t0.Add(time.Minute)}, {Key: "draft-9", Since: t0}, {Key: "session-z"}, {Key: "ask-2", Since: t0.Add(2 * time.Minute)}},
		{{Key: "session-z"}, {Key: "draft-9", Since: t0}, {Key: "ask-2", Since: t0.Add(2 * time.Minute)}, {Key: "ask-1", Since: t0}, {Key: "session-b", Since: t0.Add(time.Minute)}},
	} {
		sortAttentionItems(in)
		var got []string
		for _, it := range in {
			got = append(got, it.Key)
		}
		if want := "ask-2 session-b ask-1 draft-9 session-z"; strings.Join(got, " ") != want {
			t.Fatalf("sorted %v, want %s", got, want)
		}
	}
}

// TestAHeldQuestionListsEveryQuestionCompactly: one question is itself; several
// are each one's first line, numbered, one per line.
func TestAHeldQuestionListsEveryQuestionCompactly(t *testing.T) {
	one := askq.Entry{Questions: []askq.Question{{Question: "Which database?\nWe need one."}}}
	if got := heldQuestionText(one); got != "Which database?\nWe need one." {
		t.Fatalf("one question: %q", got)
	}
	many := askq.Entry{Questions: []askq.Question{{Question: "Which database?\nWe need one."}, {Question: "  Migrate\tnow?  "}, {Question: strings.Repeat("x", 200)}}}
	want := "1. Which database?\n2. Migrate now?\n3. " + strings.Repeat("x", heldQuestionLineMax-1) + "…"
	if got := heldQuestionText(many); got != want {
		t.Fatalf("several questions: %q\nwant %q", got, want)
	}
	if got := heldQuestionText(askq.Entry{}); got != sessionWaitingText {
		t.Fatalf("no question: %q", got)
	}
}

// TestADraftsNotificationQuotesShortAndOnOneLine: the body names the sender
// and quotes the start of the message and the draft, control characters and
// line breaks gone; without the message it is muster's sentence.
func TestADraftsNotificationQuotesShortAndOnOneLine(t *testing.T) {
	m := plugins.Message{ID: 7, Sender: "Ada\x1b[2J", Body: "Line one\nline two " + strings.Repeat("y", 200)}
	got := draftPushBody(m, plugins.Draft{Body: "Yes.\r\nNoon " + strings.Repeat("z", 100)})
	if strings.ContainsAny(got, "\n\r\x1b") || !strings.HasPrefix(got, "Ada [2J: Line one line two y") || !strings.Contains(got, " · Draft: Yes. Noon z") {
		t.Fatalf("body %q", got)
	}
	if n := len([]rune(got)); n > draftPushInboundMax+len(" · Draft: ")+draftPushReplyMax {
		t.Fatalf("body is %d runes", n)
	}
	if got := draftPushBody(plugins.Message{}, plugins.Draft{Body: "x"}); got != DraftPushBody {
		t.Fatalf("without the message: %q", got)
	}
	// The subject stands in for an empty body.
	if got := draftInboundText(plugins.Message{Sender: "b@example.test", Subject: "Lunch?"}); got != "b@example.test: Lunch?" {
		t.Fatalf("subject fallback: %q", got)
	}
}

// TestWhoseWaitingWindowsTheQueueBelieves: an online host only when it reports
// status; a dropped host only within the grace — the rule the view and the
// push diff share.
func TestWhoseWaitingWindowsTheQueueBelieves(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name string
		h    mirror.HostView
		want bool
	}{
		{"online, reports status", mirror.HostView{Online: true, StatusReport: true}, true},
		{"online, no status", mirror.HostView{Online: true}, false},
		{"dropped just now", mirror.HostView{OfflineSince: now.Add(-time.Second)}, true},
		{"dropped at the grace", mirror.HostView{OfflineSince: now.Add(-attentionOfflineGrace)}, false},
		{"offline, never seen drop", mirror.HostView{}, false},
	} {
		if got := attentionHostBelieved(c.h, now); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// TestOnlyTheOperatorsSendsAnswerAsksInSession is the SEAM LEDGER: the exact
// set of functions that may answer an ask in session. Each is a door the
// OPERATOR writes through (the rendered/terminal Send, the send queue's add,
// an agent's chat form and socket). A new caller — a routed message, the
// chief's hand-back turn, a machine-token message, anything an agent sends —
// would make an agent's or muster's own words answer the operator's
// questions, so the set failing when it GROWS is the point; it fails when it
// SHRINKS too (a door that stopped clearing asks).
func TestOnlyTheOperatorsSendsAnswerAsksInSession(t *testing.T) {
	callers := map[string][]string{}
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("list sources: %v (%d)", err, len(files))
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
						switch sel.Sel.Name {
						case "askerAddressed", "mirrorAddressed", "AnswerInSession":
							callers[sel.Sel.Name] = append(callers[sel.Sel.Name], fn.Name.Name)
						}
					}
				}
				return true
			})
		}
	}
	for name, want := range map[string][]string{
		"AnswerInSession": {"askerAddressed"},
		"askerAddressed":  {"handleAgentSend", "handleAgentWS", "mirrorAddressed"},
		"mirrorAddressed": {"mirrorDeliver", "sendQueueAdd"},
	} {
		got := callers[name]
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s is called from %v, want exactly %v", name, got, want)
		}
	}
}
