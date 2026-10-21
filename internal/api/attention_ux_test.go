package api

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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

// TestADraftsNotificationNamesWhoAndWhereOnly: "Reply draft: <name>
// (<plugin>)" — the WHOLE body pinned — with the name only for a sender the
// plugin verified; never a word of the message or the draft.
func TestADraftsNotificationNamesWhoAndWhereOnly(t *testing.T) {
	for _, c := range []struct {
		name  string
		m     plugins.Message
		label string
		want  string
	}{
		{"verified, display name", plugins.Message{ID: 7, Sender: "Justin <justin@example.test>", SenderVerified: true, Body: "the secret plan"}, "Discord DMs", "Reply draft: Justin (Discord DMs)"},
		{"verified, bare handle", plugins.Message{ID: 7, Sender: "justin#0420", SenderVerified: true, Body: "hi"}, "Discord DMs", "Reply draft: justin#0420 (Discord DMs)"},
		{"verified, agent-mail address", plugins.Message{ID: 7, Sender: "example-owner/agent-infra", SenderVerified: true, Body: "hi"}, "Agent mail", "Reply draft: example-owner/agent-infra (Agent mail)"},
		{"UNVERIFIED agent-mail address: not shown", plugins.Message{ID: 7, Sender: "example-owner/agent-infra", Body: "hi"}, "Agent mail", "Reply draft: unverified sender (Agent mail)"},
		{"verified, quoted name", plugins.Message{ID: 7, Sender: `"Ada L" <ada@example.test>`, SenderVerified: true}, "Mail", "Reply draft: Ada L (Mail)"},
		{"UNVERIFIED: the claimed name is not shown", plugins.Message{ID: 7, Sender: "Your Bank <alerts@example.test>", Body: "reset now"}, "Mail", "Reply draft: unverified sender (Mail)"},
		{"control characters in a name", plugins.Message{ID: 7, Sender: "Ev\x1b[2Jil\u202E", SenderVerified: true}, "Mail", "Reply draft: Ev [2Jil (Mail)"},
		{"no plugin label", plugins.Message{ID: 7, Sender: "Justin", SenderVerified: true}, "", "Reply draft: Justin"},
		{"no message", plugins.Message{}, "Mail", DraftPushBody},
	} {
		if got := draftPushBody(c.m, c.label); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// TestEveryAttentionPushBodyIsWhoAndWhereOnly: an ask's and a held question's
// body is "<asker> is asking"; a waiting session's "Waiting: <tool> · <window
// @ host>" (or without the tool) — never the question, the command, the path
// or the URL, all of which the item carries for the page. Whole bodies pinned.
func TestEveryAttentionPushBodyIsWhoAndWhereOnly(t *testing.T) {
	for _, c := range []struct {
		name string
		it   attentionItem
		want string
	}{
		{"an ask", attentionItem{Kind: attentionKindAsk, Source: "deploy-bot", Text: "Delete prod DB with password hunter2?"}, "deploy-bot is asking"},
		{"a held question", attentionItem{Kind: attentionKindQuestion, Source: "main:1 @ ws", Text: "1. Use key AKIA…?\n2. Push?"}, "main:1 @ ws is asking"},
		{"a waiting session", attentionItem{Kind: attentionKindSession, Source: "main:1 @ ws", Text: "Bash: curl -H 'Authorization: «redacted»' https://x.test", tool: "Bash"}, "Waiting: Bash · main:1 @ ws"},
		{"an MCP tool", attentionItem{Kind: attentionKindSession, Source: "w:2 @ lap", Text: "MCP muster/attention_ask", tool: "MCP muster/attention_ask"}, "Waiting: MCP muster/attention_ask · w:2 @ lap"},
		{"a host that names no tool", attentionItem{Kind: attentionKindSession, Source: "main:1 @ ws", Text: sessionWaitingText}, "Waiting: main:1 @ ws"},
		{"a long window", attentionItem{Kind: attentionKindSession, Source: strings.Repeat("w", 200), tool: "Edit"}, "Waiting: Edit · " + strings.Repeat("w", attentionPushBodyMax-utf8.RuneCountInString("Waiting: Edit · ")) + "…"},
	} {
		if got := attentionPushBody(c.it); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
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
// set of calls that may answer an ask in session, pinned by caller AND by the
// paste TARGET each mirrored one names. Each is a door the OPERATOR writes
// through: the rendered view's Send (its own target — mirrorAddressed answers
// only mirror.PasteClaude, so a Terminal send to the active pane does not),
// the send queue's add and its Retry of a failed message (PasteClaude: the
// queue pastes into the Claude pane, as sendQueueSend's own paste pins below),
// an agent's chat form and socket. A
// new caller — a routed message, the chief's hand-back turn, a machine-token
// message, anything an agent sends — or a queue that pasted elsewhere fails
// here; so does a door that stopped answering.
func TestOnlyTheOperatorsSendsAnswerAsksInSession(t *testing.T) {
	type site struct{ caller, target string }
	calls := map[string][]site{}
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("list sources: %v (%d)", err, len(files))
	}
	fset := token.NewFileSet()
	expr := func(e ast.Expr) string {
		var b strings.Builder
		_ = printer.Fprint(&b, fset, e)
		return b.String()
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch sel.Sel.Name {
				case "askerAddressed", "askerAddressedBy", "AnswerInSession":
					calls[sel.Sel.Name] = append(calls[sel.Sel.Name], site{fn.Name.Name, ""})
				case "mirrorAddressed":
					calls[sel.Sel.Name] = append(calls[sel.Sel.Name], site{fn.Name.Name, expr(call.Args[3])})
				case "mirrorPaste":
					if fn.Name.Name == "sendQueueDeliver" {
						calls["queue paste"] = append(calls["queue paste"], site{fn.Name.Name, expr(call.Args[6])})
					}
				}
				return true
			})
		}
	}
	for name, want := range map[string][]site{
		"AnswerInSession":  {{"askerAddressedBy", ""}},
		"askerAddressedBy": {{"askerAddressed", ""}, {"mirrorAddressed", ""}},
		"askerAddressed":   {{"handleAgentSend", ""}, {"handleAgentWS", ""}},
		"mirrorAddressed":  {{"mirrorDeliver", "p.target"}, {"sendQueueAdd", "mirror.PasteClaude"}, {"sendQueueRetry", "mirror.PasteClaude"}},
		"queue paste":      {{"sendQueueDeliver", "mirror.PasteClaude"}},
	} {
		got := calls[name]
		slices.SortFunc(got, func(a, b site) int { return strings.Compare(a.caller, b.caller) })
		if !slices.Equal(got, want) {
			t.Errorf("%s is called as %v, want exactly %v", name, got, want)
		}
	}
}
