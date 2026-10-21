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
		{"Rendered vs Terminal view of one window", MirrorDraftKey(1, "view", "h", "s", "@1"), MirrorDraftKey(1, "terminal", "h", "s", "@1")},
		{"another host", MirrorDraftKey(1, "view", "h1", "s", "@1"), MirrorDraftKey(1, "view", "h2", "s", "@1")},
		{"another session", MirrorDraftKey(1, "view", "h", "s1", "@1"), MirrorDraftKey(1, "view", "h", "s2", "@1")},
		{"another window", MirrorDraftKey(1, "view", "h", "s", "@1"), MirrorDraftKey(1, "view", "h", "s", "@2")},
		{"a window vs the session's active window", MirrorDraftKey(1, "view", "h", "s", "@1"), MirrorDraftKey(1, "view", "h", "s", "")},
		{"a separator moved between parts", MirrorDraftKey(1, "view", "a|b", "c", ""), MirrorDraftKey(1, "view", "a", "b|c", "")},
		{"another agent", AgentChatDraftKey(1, "keen-wren", 1), AgentChatDraftKey(1, "calm-owl", 1)},
		{"another chat session", AgentChatDraftKey(1, "keen-wren", 1), AgentChatDraftKey(1, "keen-wren", 2)},
		{"an agent's chat vs its Terminal", AgentChatDraftKey(1, "keen-wren", 0), AgentTerminalDraftKey(1, "keen-wren")},
		{"another account, the same mirrored address", MirrorDraftKey(1, "view", "h", "s", "@1"), MirrorDraftKey(2, "view", "h", "s", "@1")},
		{"another account, the same agent chat", AgentChatDraftKey(1, "keen-wren", 1), AgentChatDraftKey(2, "keen-wren", 1)},
		{"another account, the same agent Terminal", AgentTerminalDraftKey(1, "keen-wren"), AgentTerminalDraftKey(2, "keen-wren")},
		{"an agent vs a mirror of the same name", AgentTerminalDraftKey(1, "h"), MirrorDraftKey(1, "terminal", "h", "", "")},
	}
	for _, p := range pairs {
		if p.a == "" || p.b == "" || p.a == p.b {
			t.Errorf("%s: keys %q and %q; want two different keys", p.name, p.a, p.b)
		}
		if !strings.HasPrefix(p.a, DraftKeyPrefix) || !strings.HasPrefix(p.b, DraftKeyPrefix) {
			t.Errorf("%s: a key without the prefix the script sweeps: %q, %q", p.name, p.a, p.b)
		}
	}
	if MirrorDraftKey(1, "view", "h", "s", "@1") != MirrorDraftKey(1, "view", "h", "s", "@1") {
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
	view := renderString(t, MirrorViewPage(MirrorViewView{DraftUser: 1, Host: "host-a", Session: "main", Window: "@3", Interactive: true, StepUp: stepUpFixture(),
		Queue: &ComposerQueue{Src: SendQueuePath}}))
	if got, want := draftKeyOf(view, "mv-input"), MirrorDraftKey(1, "view", "host-a", "main", "@3"); got != want {
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

	chat := renderString(t, agentChatPane(AgentDetailView{DraftUser: 1, Name: "keen-wren", ActiveSessionID: 7}))
	if got, want := draftKeyOf(chat, "chat-input"), AgentChatDraftKey(1, "keen-wren", 7); got != want {
		t.Fatalf("the agent chat's box keeps its draft under %q, want %q", got, want)
	}
	if !strings.Contains(chat, `data-draft-note="chat-note"`) || !strings.Contains(chat, "window.musterDraft.scan()") {
		t.Fatal("the agent chat's pane does not carry its draft's note or script")
	}
}

// TestTwoAccountsOnOneBrowserKeepSeparateDrafts: the same mirrored window's
// page drawn for account 1 and for account 2 keys its box differently, and
// each key starts with that account's id — so a draft account 1 typed is never
// restored for account 2 in the same browser. Same for an agent's chat.
func TestTwoAccountsOnOneBrowserKeepSeparateDrafts(t *testing.T) {
	page := func(user int64) string {
		return renderString(t, MirrorViewPage(MirrorViewView{DraftUser: user, Host: "host-a", Session: "main", Window: "@3", Interactive: true,
			StepUp: stepUpFixture()}))
	}
	a, b := draftKeyOf(page(1), "mv-input"), draftKeyOf(page(2), "mv-input")
	if a == "" || b == "" || a == b {
		t.Fatalf("one window's box for two accounts: keys %q and %q; want two different keys", a, b)
	}
	if !strings.HasPrefix(a, DraftKeyPrefix+"[1,") || !strings.HasPrefix(b, DraftKeyPrefix+"[2,") {
		t.Fatalf("the keys do not start with their account: %q, %q", a, b)
	}
	chat := func(user int64) string {
		return draftKeyOf(renderString(t, agentChatPane(AgentDetailView{DraftUser: user, Name: "keen-wren", ActiveSessionID: 7})), "chat-input")
	}
	if chat(1) == "" || chat(1) == chat(2) {
		t.Fatalf("one agent chat for two accounts: %q and %q", chat(1), chat(2))
	}
}
