package ui

import (
	"bytes"
	"strings"
	"testing"

	g "maragu.dev/gomponents"

	"github.com/ZacxDev/muster/internal/notes"
)

func renderTagNode(t *testing.T, n g.Node) string {
	t.Helper()
	var b bytes.Buffer
	if err := n.Render(&b); err != nil {
		t.Fatalf("render: %v", err)
	}
	return b.String()
}

func renderTasks(t *testing.T, v TasksView) string {
	t.Helper()
	var b bytes.Buffer
	if err := NotesCards(v).Render(&b); err != nil {
		t.Fatalf("render: %v", err)
	}
	return b.String()
}

func renderTagCard(t *testing.T, n notes.Note) string {
	t.Helper()
	var b bytes.Buffer
	if err := noteCard(TaskCardView{Note: n}).Render(&b); err != nil {
		t.Fatalf("render: %v", err)
	}
	return b.String()
}

// TestCardTagChipsRender asserts chips render for each tag, that >3 collapses to
// a "+N" overflow chip, and that ZERO tags renders NOTHING (an untagged card must
// look exactly as it did before tags existed).
func TestCardTagChipsRender(t *testing.T) {
	t.Run("zero tags renders nothing", func(t *testing.T) {
		out := renderTagCard(t, notes.Note{ID: 1, Body: "plain"})
		if strings.Contains(out, "data-tag=") || strings.Contains(out, "data-tag-overflow") {
			t.Fatalf("an untagged card must render no tag markup; body=%s", out)
		}
	})

	t.Run("chips render", func(t *testing.T) {
		out := renderTagCard(t, notes.Note{ID: 2, Body: "x", Tags: []string{"api", "bug"}})
		for _, want := range []string{`data-tag="api"`, `data-tag="bug"`} {
			if !strings.Contains(out, want) {
				t.Fatalf("card must contain %s; body=%s", want, out)
			}
		}
		if strings.Contains(out, "data-tag-overflow") {
			t.Fatal("two tags must not produce an overflow chip")
		}
	})

	t.Run("more than three collapses to +N", func(t *testing.T) {
		out := renderTagCard(t, notes.Note{ID: 3, Body: "x", Tags: []string{"a", "b", "c", "d", "e"}})
		if !strings.Contains(out, `data-tag-overflow="2"`) {
			t.Fatalf("5 tags must render 3 chips + a +2 overflow chip; body=%s", out)
		}
		if !strings.Contains(out, ">+2<") {
			t.Fatalf("the overflow chip must read \"+2\"; body=%s", out)
		}
	})

	t.Run("routing tags are never the ones hidden behind +N", func(t *testing.T) {
		out := renderTagCard(t, notes.Note{ID: 4, Body: "x",
			Tags: []string{"a", "b", "c", "d", "gate:blocked"}})
		if !strings.Contains(out, `data-tag="gate:blocked"`) {
			t.Fatalf("a routing tag must always be shown, never collapsed; body=%s", out)
		}
	})
}

// TestRoutingChipsVisuallyDistinct asserts the two tag classes are rendered
// distinguishably — a behaviour-bearing tag must not look like a plain label.
func TestRoutingChipsVisuallyDistinct(t *testing.T) {
	out := renderTagCard(t, notes.Note{ID: 1, Body: "x", Tags: []string{"bug", "initiative:launch"}})

	if !strings.Contains(out, `data-tag-kind="routing"`) {
		t.Fatalf("an initiative: chip must be marked routing; body=%s", out)
	}
	if !strings.Contains(out, `data-tag-kind="descriptive"`) {
		t.Fatalf("a free-form chip must be marked descriptive; body=%s", out)
	}

	// And the STYLING must differ, not just the data attribute.
	routingClass, _ := tagChipClass("initiative:launch")
	descClass, _ := tagChipClass("bug")
	if routingClass == descClass {
		t.Fatal("routing and descriptive chips must have different styling")
	}
	// Each reserved namespace gets its own treatment + an explanatory title.
	for _, tag := range []string{"initiative:launch", "initiative:remix", "gate:blocked", "auto:dispatch"} {
		cls, title := tagChipClass(tag)
		if cls == descClass {
			t.Errorf("%s must not be styled as a descriptive chip", tag)
		}
		if !strings.Contains(strings.ToLower(title), "routing") && !strings.Contains(strings.ToLower(title), "gated") {
			t.Errorf("%s chip title %q must explain the behaviour", tag, title)
		}
	}
}

// TestTagFilterRowRenders asserts the chip row renders the vocabulary, marks the
// ACTIVE selection, and drives the right filtered request.
func TestTagFilterRowRenders(t *testing.T) {
	view := TasksView{
		Cards:      []TaskCardView{{Note: notes.Note{ID: 1, Body: "x", Tags: []string{"api"}}}},
		Vocabulary: []notes.TagCount{{Tag: "api", Count: 2}, {Tag: "bug", Count: 1}},
		ActiveTags: []string{"api"},
	}
	out := renderTasks(t, view)

	if !strings.Contains(out, `id="tag-filter-row"`) {
		t.Fatalf("the filter row must render; body=%s", out)
	}
	for _, want := range []string{`data-tag-filter="api"`, `data-tag-filter="bug"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("the filter row must contain %s; body=%s", want, out)
		}
	}
	// Active selection is marked (aria-pressed=true on the selected chip only).
	if !strings.Contains(out, `data-tag-filter="api" aria-pressed="true"`) {
		t.Fatalf("the ACTIVE chip must be marked aria-pressed=true; body=%s", out)
	}
	if !strings.Contains(out, `data-tag-filter="bug" aria-pressed="false"`) {
		t.Fatalf("an inactive chip must be aria-pressed=false; body=%s", out)
	}
	// The active set is exposed so the client can reconcile, and a Clear control
	// is always available while a filter is on.
	if !strings.Contains(out, `data-active-tags="api"`) {
		t.Fatalf("the row must expose the active tag set; body=%s", out)
	}
	if !strings.Contains(out, "data-tag-filter-clear") {
		t.Fatalf("a Clear control must be present while filtering; body=%s", out)
	}

	// The correct hx-get URL shape (repeated tag= params = the AND filter).
	if got := boardURL([]string{"a", "b"}, "", 0); got != "/ui/tasks?tag=a&tag=b" {
		t.Fatalf("boardURL = %q, want /ui/tasks?tag=a&tag=b", got)
	}
	if got := boardURL(nil, "", 0); got != "/ui/tasks" {
		t.Fatalf("boardURL(nil) = %q, want /ui/tasks", got)
	}
}

// TestBoardURLCarriesEveryDimension.
//
// 🔴 boardURL IS THE BOARD'S STATE. It is what lands in #tasks-list's hx-get,
// which tagScript's htmx:configRequest listener turns into the request an
// SSE-triggered refetch issues — so a dimension boardURL drops is a filter that
// silently resets on the next event any browser causes. (The listener half is
// NOT optional and did not exist before PR #727's audit round: without it the
// attribute never reached the request at all. This comment used to call the URL
// "the only thing that survives", which was true of the design and false of the
// code.) Every combination is pinned as a LITERAL expected string
// (never re-derived from url.Values) so a mutant that drops or renames a param
// cannot restate itself into passing.
func TestBoardURLCarriesEveryDimension(t *testing.T) {
	cases := []struct {
		name   string
		tags   []string
		status string
		limit  int
		want   string
	}{
		{"nothing", nil, "", 0, "/ui/tasks"},
		{"tags only", []string{"api", "ui"}, "", 0, "/ui/tasks?tag=api&tag=ui"},
		{"status only", nil, "in_progress", 0, "/ui/tasks?status=in_progress"},
		{"limit only", nil, "", 137, "/ui/tasks?limit=137"},
		{"status+limit", nil, "done", 71, "/ui/tasks?limit=71&status=done"},
		{"all three", []string{"project:civitai"}, "open", 23,
			"/ui/tasks?limit=23&status=open&tag=project%3Acivitai"},
		// 0 means "no cap", and it must not appear as limit=0 — the server reads
		// that back as the default page, so emitting it would be a silent lie about
		// what was asked for.
		{"zero limit is omitted", []string{"x"}, "open", 0, "/ui/tasks?status=open&tag=x"},
	}
	for _, c := range cases {
		if got := boardURL(c.tags, c.status, c.limit); got != c.want {
			t.Errorf("%s: boardURL(%v, %q, %d) = %q, want %q", c.name, c.tags, c.status, c.limit, got, c.want)
		}
	}
}

// TestTagFilterRowHiddenWhenUnused asserts a queue that has never used tags gets
// NO empty toolbar (the feature stays invisible until it's used).
func TestTagFilterRowHiddenWhenUnused(t *testing.T) {
	out := renderTasks(t, TasksView{Cards: []TaskCardView{{Note: notes.Note{ID: 1, Body: "x"}}}})
	if strings.Contains(out, `id="tag-filter-row"`) {
		t.Fatalf("with no vocabulary and no filter, the row must not render; body=%s", out)
	}
}

// TestFilteredEmptyIsNotAllClear is the guard for the classic trap: a filter that
// matches nothing must NOT render the generic "No tasks yet" all-clear, or the
// user concludes their queue is empty when their work is merely hidden.
func TestFilteredEmptyIsNotAllClear(t *testing.T) {
	out := renderTasks(t, TasksView{
		Cards:      nil,
		Vocabulary: []notes.TagCount{{Tag: "api", Count: 2}},
		ActiveTags: []string{"never-used"},
	})

	if strings.Contains(out, "No tasks yet") {
		t.Fatalf("filtered-empty must NOT render the all-clear copy; body=%s", out)
	}
	if !strings.Contains(out, "data-tasks-filtered-empty") {
		t.Fatalf("filtered-empty must render its own distinct state; body=%s", out)
	}
	if !strings.Contains(out, "No tasks match") {
		t.Fatalf("filtered-empty must say what is filtered; body=%s", out)
	}
	if !strings.Contains(out, "never-used") {
		t.Fatalf("filtered-empty must NAME the active tags; body=%s", out)
	}
	if !strings.Contains(out, "data-tag-filter-clear") {
		t.Fatalf("filtered-empty must ship a Clear-filter CONTROL, not just prose; body=%s", out)
	}

	// The genuinely-empty (unfiltered) queue still gets the all-clear.
	allClear := renderTasks(t, TasksView{})
	if !strings.Contains(allClear, "No tasks yet") {
		t.Fatalf("an unfiltered empty queue must still render the all-clear; body=%s", allClear)
	}
	if strings.Contains(allClear, "data-tasks-filtered-empty") {
		t.Fatalf("an unfiltered empty queue must NOT render the filtered-empty state; body=%s", allClear)
	}
}

// TestEditorRendersTagsAndDatalist asserts the edit modal shows the current tags
// as removable chips carrying hidden `tag` inputs, plus the vocabulary datalist.
func TestEditorRendersTagsAndDatalist(t *testing.T) {
	v := NoteEditView{
		Note:       notes.Note{ID: 7, Body: "x", Status: notes.StatusOpen, Tags: []string{"api", "initiative:launch"}},
		Vocabulary: []notes.TagCount{{Tag: "api", Count: 3}, {Tag: "bug", Count: 1}},
	}
	var b bytes.Buffer
	if err := NotesEditModalBody(v).Render(&b); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := b.String()

	if !strings.Contains(out, "data-tag-editor") {
		t.Fatalf("the edit modal must contain the tag editor; body=%s", out)
	}
	for _, want := range []string{
		`data-tag-chip="api"`,
		`data-tag-chip="initiative:launch"`,
		`<input type="hidden" name="tag" value="api">`,
		`<input type="hidden" name="tag" value="initiative:launch">`,
		`data-tag-input`,
		`<datalist id="task-tags-vocabulary">`,
		`<option value="bug">`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("the editor must contain %s; body=%s", want, out)
		}
	}
	// The composer input must NOT be named, or a half-typed value would submit.
	if strings.Contains(out, `data-tag-input list="task-tags-vocabulary" autocomplete="off" name=`) {
		t.Fatal("the tag composer input must be unnamed")
	}
	// Both chips are removable on an OPEN task.
	if n := strings.Count(out, "data-tag-remove"); n != 2 {
		t.Fatalf("both chips must be removable on an open task; found %d remove controls", n)
	}
	// The title field is present (the §9 companion fix).
	if !strings.Contains(out, `name="title"`) {
		t.Fatalf("the edit modal must expose the title field; body=%s", out)
	}
}

// TestEditorRoutingTagsLockedInProgress asserts routing-tag inputs are disabled
// while the task is in progress (descriptive labels stay editable) — the UI
// mirror of the refined server-side 409 rule.
func TestEditorRoutingTagsLockedInProgress(t *testing.T) {
	out := renderTagNode(t, tagEditor(
		[]string{"api", "initiative:launch"},
		[]notes.TagCount{{Tag: "api", Count: 1}},
		true, // in progress
	))

	if !strings.Contains(out, `data-tag-editor-locked="true"`) {
		t.Fatalf("the editor must mark itself locked while in progress; body=%s", out)
	}
	// The routing chip keeps its hidden input (so it survives the submit) but
	// loses its × ; the descriptive one keeps both.
	if !strings.Contains(out, `<input type="hidden" name="tag" value="initiative:launch">`) {
		t.Fatalf("a locked routing chip must still submit its value; body=%s", out)
	}
	if n := strings.Count(out, "data-tag-remove"); n != 1 {
		t.Fatalf("only the descriptive chip may be removable while in progress; found %d remove controls", n)
	}
	if !strings.Contains(out, "Remove tag api") {
		t.Fatalf("the descriptive chip must remain removable; body=%s", out)
	}
	if !strings.Contains(out, "routing tags are locked") {
		t.Fatalf("the editor must explain WHY routing tags are locked; body=%s", out)
	}
}

// TestGateTagDisablesDispatch asserts a `gate:` tag disables the card's Dispatch
// button and surfaces the reason (the server 409s independently).
func TestGateTagDisablesDispatch(t *testing.T) {
	out := renderTagCard(t, notes.Note{ID: 5, Body: "x", Tags: []string{"gate:needs-decision"}})

	if !strings.Contains(out, `data-dispatch-gated="needs-decision"`) {
		t.Fatalf("a gate: tag must mark the Dispatch button gated; body=%s", out)
	}
	if !strings.Contains(out, "disabled") {
		t.Fatalf("the gated Dispatch button must be disabled; body=%s", out)
	}
	if !strings.Contains(out, "Gated: needs-decision") {
		t.Fatalf("the gated button must surface the REASON; body=%s", out)
	}
	// It must NOT still open the dispatch modal.
	if strings.Contains(out, "/ui/agents/new?") {
		t.Fatalf("a gated card must not wire the dispatch modal; body=%s", out)
	}

	// An ungated card keeps the working Dispatch button.
	ok := renderTagCard(t, notes.Note{ID: 6, Body: "x", Tags: []string{"bug"}})
	if !strings.Contains(ok, "/ui/agents/new?") {
		t.Fatalf("an ungated card must keep its Dispatch button; body=%s", ok)
	}
}

// TestTaskTitlePrefersTitleOverDirectory pins the §9 companion fix: the display
// label prefers `title` and falls back to `directory`, and the card renders a
// title heading ONLY when a real title is set (so the deliberate 0.7.x declutter
// — the directory chip was REMOVED — is not undone for existing tasks).
func TestTaskTitlePrefersTitleOverDirectory(t *testing.T) {
	if got := TaskTitle(notes.Note{Title: "Real", Directory: "/srv/x"}); got != "Real" {
		t.Fatalf("taskTitle = %q, want the explicit title", got)
	}
	if got := TaskTitle(notes.Note{Directory: "/srv/x"}); got != "/srv/x" {
		t.Fatalf("taskTitle = %q, want the directory fallback", got)
	}
	if got := TaskTitle(notes.Note{Title: "   ", Directory: "/srv/x"}); got != "/srv/x" {
		t.Fatalf("a whitespace-only title must fall back to the directory; got %q", got)
	}

	titled := renderTagCard(t, notes.Note{ID: 1, Body: "x", Title: "Ship task tags"})
	if !strings.Contains(titled, "data-task-title") || !strings.Contains(titled, "Ship task tags") {
		t.Fatalf("a titled card must render its title heading; body=%s", titled)
	}

	// A directory-only card renders NO title heading (regression guard for the
	// intentional declutter that removed the directory chip).
	dirOnly := renderTagCard(t, notes.Note{ID: 2, Body: "x", Directory: "/srv/project-x"})
	if strings.Contains(dirOnly, "data-task-title") {
		t.Fatalf("a directory-only card must NOT render a title heading; body=%s", dirOnly)
	}
	if strings.Contains(dirOnly, "/srv/project-x") {
		t.Fatalf("the directory must stay off the card summary; body=%s", dirOnly)
	}
}
