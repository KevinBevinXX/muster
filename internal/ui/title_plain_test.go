package ui

import (
	"strings"
	"testing"
	"time"

	g "maragu.dev/gomponents"

	"github.com/ZacxDev/muster/internal/notes"
)

// TestEveryTitleSinkShowsThePlainTitle renders each title sink with a title
// that carries markdown and asserts it shows the text a reader of that
// markdown sees — never the markers. One row per sink, each with its own
// distinct title and expectation, so no row passes on another's output.
//
// The SET of sinks is held structurally by TestTheTitleSinkLedger
// (internal/textplain); this holds that each one actually draws plain text.
// The assertion is the plain title CONTIGUOUS in the render, which no
// marker-leaving or marker-mangling render can produce. (A "no ** anywhere"
// check would be wrong here: the cards' filter and rank attributes carry the
// raw title on purpose — see the ledger's whyFilter.)
//
// Every row keeps a non-markdown trap in its title (#12, C#, snake_case, a*b)
// that the previous stripper (markdownPlain, which removed every ` * _ # >)
// also mangled.
func TestEveryTitleSinkShowsThePlainTitle(t *testing.T) {
	now := time.Now()
	sinks := []struct {
		name, want string
		render     func() g.Node
	}{
		{"agent card", "#12 · Fix login old flow", func() g.Node {
			return agentCard(AgentCardView{ID: 1, Name: "a1", Status: "running", DisplayName: "#12 · **Fix** [login](https://example.com/l) ~~old~~ flow"})
		}},
		{"agent detail <title>", "<title>Port C# parser · muster</title>", func() g.Node {
			return AgentDetailPage(AgentDetailView{Name: "a2", Status: "running", DisplayName: "Port [C#](https://example.com/c) `parser`"}, Features{})
		}},
		{"task card heading", ">Rename snake_case_names now</h3>", func() g.Node {
			return noteCard(TaskCardView{Note: notes.Note{ID: 3, Title: "Rename `snake_case_names` **now**", Status: notes.StatusInProgress}})
		}},
		{"task detail <title>", "<title>Ship v2 for a*b · muster</title>", func() g.Node {
			return TaskDetailPage(TaskCardView{Note: notes.Note{ID: 4, Title: "~~Ship~~ <b>v2</b> for a*b", Status: notes.StatusInProgress}}, Features{})
		}},
		{"focus title", "Fixing auth flow #7", func() g.Node {
			return cardFocusTitle("c5", &FocusView{Title: "Fixing [auth](https://example.com/a) **flow** #7"}, "")
		}},
		{"focus history", "Drafted the release notes", func() g.Node {
			return focusHistoryRows(FocusHistoryPageView{Entries: []FocusHistoryEntry{{Title: "## Drafted the _release_ notes", At: now}}})
		}},
		{"mirror card", "Hermes chat about x_y", func() g.Node {
			return mirrorCard(MirrorCardView{Host: "h1", Session: "s6", Label: "**Hermes** chat about `x_y`", Online: true})
		}},
		{"mirror view <title>", "<title>Session one · Mirrored · muster</title>", func() g.Node {
			return MirrorViewPage(MirrorViewView{Host: "h1", Session: "s7", Label: "> Session ~~one~~"})
		}},
		{"chat session drawer", "Plan the 2*3 migration", func() g.Node {
			return sessionDrawer(AgentDetailView{Name: "a8", Sessions: []SessionView{{ID: 8, Title: "Plan the **2*3** migration"}}})
		}},
		{"chief thread list", "Review PR 9 today", func() g.Node {
			return chiefThreadRows(ChiefThreadsView{Threads: []SessionView{{ID: 9, Title: "Review [PR 9](https://example.com/9) _today_"}}})
		}},
		{"notification row", "Agent ten done", func() g.Node {
			return notifRow(NotificationRow{Name: "a10", DisplayName: "Agent `ten` **done**", Count: 1})
		}},
		{"recent strip", "Eleven builds", func() g.Node {
			items := recentItems([]AgentCardView{{ID: 11, Name: "a11", DisplayName: "<i>Eleven</i> builds"}}, nil, []RecentRef{{AgentID: 11}})
			return recentStrip(items, false)
		}},
		{"dispatch picker option", "Twelve tasks", func() g.Node {
			return g.Text(noteOptions([]NoteOption{{ID: 12, Label: "1. **Twelve** tasks"}})[0].Label)
		}},
		{"merge select label", `data-task-select-label="Thirteen merged"`, func() g.Node {
			return taskSelectControl(notes.Note{ID: 13, Title: "__Thirteen__ merged", Status: notes.StatusInProgress})
		}},
		{"terminal <title>", "<title>Terminal · icon Fourteen shell · muster</title>", func() g.Node {
			return AgentTerminalPage(AgentTerminalView{Name: "a14", DisplayName: "![icon](i.png) Fourteen shell"})
		}},
	}
	for _, s := range sinks {
		t.Run(s.name, func(t *testing.T) {
			out := renderString(t, s.render())
			if !strings.Contains(out, s.want) {
				t.Errorf("the plain title %q is not in the render:\n%s", s.want, excerpt(out, 1200))
			}
		})
	}
}

func excerpt(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// TestTheTerminalTitleAndHeadingAgree: the terminal page's <title> and its <h1>
// show the SAME name, for an input on which the title rule is not idempotent.
// `\*\*x\*\*` (escaped markers) reads "**x**" once plained; plained a second
// time it reads "x". The heading used to be plained twice — once on the page,
// again inside mirrorEntryTitle — so the tab said "**x**" and the header "x".
func TestTheTerminalTitleAndHeadingAgree(t *testing.T) {
	out := renderString(t, AgentTerminalPage(AgentTerminalView{Name: "a15", DisplayName: `\*\*x\*\*`, TitleQualifier: "laptop"}))
	if !strings.Contains(out, "<title>Terminal · **x** · laptop · muster</title>") {
		t.Fatalf("<title> is not the once-plained name:\n%s", excerpt(out, 800))
	}
	h1 := out[strings.Index(out, "<h1"):]
	h1 = h1[:strings.Index(h1, "</h1>")]
	if !strings.Contains(h1, ">**x**<") {
		t.Errorf("the <h1> does not show the same name as the <title> (%q):\n%s", "**x**", h1)
	}
}

// TestAFocusStoredWithMarkdownLookalikesRendersRight: the focus store keeps
// "C#", "a*b" and "#123" as written (internal/focus), and the card renders them
// as written while still removing real markup around them.
func TestAFocusStoredWithMarkdownLookalikesRendersRight(t *testing.T) {
	out := renderString(t, cardFocusTitle("c16", &FocusView{Title: "**Port** C# to a*b for #123", Description: "Uses `cfg` and C#."}, ""))
	for _, want := range []string{">Port C# to a*b for #123<", ">Uses cfg and C#.<"} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q:\n%s", want, out)
		}
	}
}
