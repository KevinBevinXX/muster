package ui

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/cctranscript"
)

// A harness notice is drawn as its own compact row — never as the human's
// bubble — carrying its source, its status and its summary.
func TestOpsDrawsANoticeAsARowNotAUserBubble(t *testing.T) {
	var tr MirrorTranscript
	ops := tr.Ops([]cctranscript.Event{
		{Kind: cctranscript.KindNotice, Notice: cctranscript.NoticeTask, Status: "completed",
			Text: `Agent "Audit the widget" finished`, Output: "The widget is <fine>."},
		{Kind: cctranscript.KindPrompt, Text: "a real prompt"},
	})
	if len(ops) != 2 {
		t.Fatalf("ops = %d, want 2", len(ops))
	}
	n := ops[0]
	if n.Op != "append" {
		t.Fatalf("notice op = %q", n.Op)
	}
	for _, want := range []string{
		`data-notice-row="task"`, `data-notice-status="completed"`, `<details`,
		`Agent &#34;Audit the widget&#34; finished`, `aria-label="status: completed"`,
		// The detail is escaped text, never markup.
		`The widget is &lt;fine&gt;.`,
	} {
		if !strings.Contains(n.HTML, want) {
			t.Errorf("notice row lacks %q:\n%s", want, n.HTML)
		}
	}
	if strings.Contains(n.HTML, "data-bubble") {
		t.Errorf("a notice was drawn as a bubble:\n%s", n.HTML)
	}
	if !strings.Contains(ops[1].HTML, `data-bubble="user"`) {
		t.Errorf("the prompt is not a user bubble:\n%s", ops[1].HTML)
	}
}

// Each source has its own label; a status the row does not know is shown by
// name; no status and no detail draw neither.
func TestNoticeRowSourcesAndStatuses(t *testing.T) {
	cases := []struct {
		ev           cctranscript.Event
		want, reject []string
	}{
		{cctranscript.Event{Kind: cctranscript.KindNotice, Notice: cctranscript.NoticeTask, Status: "failed", Text: "x failed"},
			[]string{`data-notice-status="failed"`, "✗", ">task<"}, []string{"✓", ">detail<"}},
		{cctranscript.Event{Kind: cctranscript.KindNotice, Notice: cctranscript.NoticeTask, Status: "killed", Text: "x killed"},
			[]string{`data-notice-status="killed"`, ">killed<"}, []string{"✓", "✗"}},
		{cctranscript.Event{Kind: cctranscript.KindNotice, Notice: cctranscript.NoticeTask, Text: "x stopped"},
			[]string{`data-notice-row="task"`, "x stopped"}, []string{"data-notice-status", ">detail<"}},
		{cctranscript.Event{Kind: cctranscript.KindNotice, Notice: cctranscript.NoticeCommandOutput, Text: "line one", Output: "line one\nline two"},
			[]string{`data-notice-row="command-output"`, ">output<", "line two", ">detail<"}, nil},
	}
	for _, c := range cases {
		html := render(noticeRow(c.ev))
		for _, w := range c.want {
			if !strings.Contains(html, w) {
				t.Errorf("%s: lacks %q:\n%s", c.ev.Text, w, html)
			}
		}
		for _, r := range c.reject {
			if strings.Contains(html, r) {
				t.Errorf("%s: has %q:\n%s", c.ev.Text, r, html)
			}
		}
	}
}

// Both clamped chat surfaces — the mirrored view and the agent chat page —
// carry the bubble clamp, and every bubble it should act on is marked for it:
// the server-rendered bubble (chatBubbleOf) and the agent chat's live one
// (agentChatScript's bubble()).
func TestBothClampedChatSurfacesCarryTheBubbleClamp(t *testing.T) {
	pages := map[string]string{
		"mirror view": renderString(t, MirrorViewPage(MirrorViewView{Host: "host-a", Session: "main"})),
	}
	var b strings.Builder
	if err := RenderAgentDetail(&b, sampleAgentDetailView(), featuresAllOn); err != nil {
		t.Fatal(err)
	}
	pages["agent chat"] = b.String()
	for name, html := range pages {
		if strings.Count(html, "window.muClamp = {") != 1 {
			t.Errorf("%s: carries the bubble clamp %d times, want 1", name, strings.Count(html, "window.muClamp = {"))
		}
	}
	if got := render(chatBubbleOf("user", nil)); !strings.Contains(got, `data-bubble="user"`) {
		t.Errorf("chatBubbleOf is not marked data-bubble: %s", got)
	}
	if !strings.Contains(pages["agent chat"], "d.setAttribute('data-bubble', role);") {
		t.Error("the agent chat's live bubble() is not marked data-bubble")
	}
}

// The chief panel is deliberately NOT clamped: neither its body nor its frame
// carries the clamp script.
func TestTheChiefPanelCarriesNoBubbleClamp(t *testing.T) {
	cv := sampleAgentDetailView()
	cv.Surface = ChatSurfaceChiefPanel
	var b strings.Builder
	if err := RenderChiefPanelBody(&b, ChiefPanelView{Configured: true, Chat: cv}); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(b.String(), "window.muClamp"); n != 0 {
		t.Errorf("the chief panel body carries the bubble clamp (%d)", n)
	}
	if n := strings.Count(renderString(t, ChiefPanel()), "window.muClamp"); n != 0 {
		t.Errorf("the chief panel frame carries the bubble clamp (%d)", n)
	}
}
