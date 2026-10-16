package ui

import (
	"strings"
	"testing"
)

// 🔴 A persisted agent-chat bubble carries its row id as data-msg-id: the key
// the bubble clamp remembers an opened message by across the chat-log refresh
// (bubble_clamp.go). A row with no id (0) carries none, so it is never
// remembered under a shared empty key.
func TestPersistedChatBubbleCarriesItsMessageID(t *testing.T) {
	html := render(chatLogInner(AgentDetailView{Name: "a", Status: "running", Messages: []ChatLine{
		{Role: "user", Content: "first", ID: 41},
		{Role: "assistant", Content: "second", ID: 42},
		{Role: "user", Content: "unsaved"},
	}}))
	for _, want := range []string{`data-msg-id="41"`, `data-msg-id="42"`} {
		if !strings.Contains(html, want) {
			t.Errorf("chat log lacks %s:\n%s", want, html)
		}
	}
	if n := strings.Count(html, "data-msg-id="); n != 2 {
		t.Errorf("data-msg-id count = %d, want 2 (the id-0 row carries none):\n%s", n, html)
	}
}
