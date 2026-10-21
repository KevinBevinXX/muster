package ui

import (
	"html"
	"regexp"
	"strings"
	"testing"
)

// TestDraftKeysNameTheWholeAddress: two message boxes share a draft only when
// they send to the same place. Every pair below differs in ONE part of the
// address — the view, the host, the session, the window, the agent, the chat
// session, the kind of box — or only in where a separator would fall, and each
// pair's keys must differ: a draft typed for one is never restored into the
// other. The control is the same address built twice, which must agree.
func TestDraftKeysNameTheWholeAddress(t *testing.T) {
	pairs := []struct{ name, a, b string }{
		{"Rendered vs Terminal view of one window", MirrorDraftKey("view", "h", "s", "@1"), MirrorDraftKey("terminal", "h", "s", "@1")},
		{"another host", MirrorDraftKey("view", "h1", "s", "@1"), MirrorDraftKey("view", "h2", "s", "@1")},
		{"another session", MirrorDraftKey("view", "h", "s1", "@1"), MirrorDraftKey("view", "h", "s2", "@1")},
		{"another window", MirrorDraftKey("view", "h", "s", "@1"), MirrorDraftKey("view", "h", "s", "@2")},
		{"a window vs the session's active window", MirrorDraftKey("view", "h", "s", "@1"), MirrorDraftKey("view", "h", "s", "")},
		{"a separator moved between parts", MirrorDraftKey("view", "a|b", "c", ""), MirrorDraftKey("view", "a", "b|c", "")},
		{"another agent", AgentChatDraftKey("keen-wren", 1), AgentChatDraftKey("calm-owl", 1)},
		{"another chat session", AgentChatDraftKey("keen-wren", 1), AgentChatDraftKey("keen-wren", 2)},
		{"an agent's chat vs its Terminal", AgentChatDraftKey("keen-wren", 0), AgentTerminalDraftKey("keen-wren")},
		{"an agent vs a mirror of the same name", AgentTerminalDraftKey("h"), MirrorDraftKey("terminal", "h", "", "")},
	}
	for _, p := range pairs {
		if p.a == "" || p.b == "" || p.a == p.b {
			t.Errorf("%s: keys %q and %q; want two different keys", p.name, p.a, p.b)
		}
		if !strings.HasPrefix(p.a, DraftKeyPrefix) || !strings.HasPrefix(p.b, DraftKeyPrefix) {
			t.Errorf("%s: a key without the prefix the script sweeps: %q, %q", p.name, p.a, p.b)
		}
	}
	if MirrorDraftKey("view", "h", "s", "@1") != MirrorDraftKey("view", "h", "s", "@1") {
		t.Error("CONTROL: the same address gives two keys")
	}
}

// draftKeyOf is the data-draft-key of the textarea id in page, unescaped; ""
// for none.
func draftKeyOf(page, id string) string {
	m := regexp.MustCompile(`<textarea id="` + id + `"[^>]*data-draft-key="([^"]*)"`).FindStringSubmatch(page)
	if m == nil {
		return ""
	}
	return html.UnescapeString(m[1])
}

// TestEveryMessageBoxKeepsItsDraftUnderItsOwnAddress: the Rendered view's box,
// the agent chat's box, and a disabled box (none). The draft's script runs
// after the box's form and before the queue's script, so a restored text is in
// the box first.
func TestEveryMessageBoxKeepsItsDraftUnderItsOwnAddress(t *testing.T) {
	view := renderString(t, MirrorViewPage(MirrorViewView{Host: "host-a", Session: "main", Window: "@3", Interactive: true, StepUp: stepUpFixture(),
		Queue: &ComposerQueue{Src: SendQueuePath}}))
	if got, want := draftKeyOf(view, "mv-input"), MirrorDraftKey("view", "host-a", "main", "@3"); got != want {
		t.Fatalf("the Rendered view's box keeps its draft under %q, want %q", got, want)
	}
	formEnd := strings.Index(view, "</form>")
	draft := strings.Index(view, "window.musterDraft.scan()")
	queue := strings.Index(view, "var box = document.getElementById('mv-queue');")
	if formEnd < 0 || draft < formEnd || queue < draft {
		t.Fatalf("script order: form ends at %d, the draft's script at %d, the queue's at %d; want form < draft < queue", formEnd, draft, queue)
	}
	if !strings.Contains(view, `data-draft-note="mv-note"`) {
		t.Fatal("the Rendered view's box does not name its status line")
	}

	readonly := renderString(t, MirrorViewPage(MirrorViewView{Host: "host-a", Session: "main", Window: "@3", StepUp: stepUpFixture()}))
	if k := draftKeyOf(readonly, "mv-input"); k != "" || strings.Contains(readonly, "window.musterDraft.scan()") {
		t.Fatalf("a disabled box keeps a draft (%q) or loads the draft script", k)
	}

	chat := renderString(t, agentChatPane(AgentDetailView{Name: "keen-wren", ActiveSessionID: 7}))
	if got, want := draftKeyOf(chat, "chat-input"), AgentChatDraftKey("keen-wren", 7); got != want {
		t.Fatalf("the agent chat's box keeps its draft under %q, want %q", got, want)
	}
	if !strings.Contains(chat, `data-draft-note="chat-note"`) || !strings.Contains(chat, "window.musterDraft.scan()") {
		t.Fatal("the agent chat's pane does not carry its draft's note or script")
	}
}
