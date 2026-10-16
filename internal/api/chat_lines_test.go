package api

import (
	"reflect"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/ui"
)

// chatLines is the one conversion of a transcript for the chat; it carries
// every field the chat draws — including the row id and the time the message
// index reads — so no surface can draw a message without them.
func TestChatLinesCarriesEveryDrawnField(t *testing.T) {
	at := time.Date(2026, 1, 2, 10, 3, 0, 0, time.UTC)
	got := chatLines([]agents.ChatMessage{
		{ID: 7, Role: "user", Content: "hello", CreatedAt: at},
		{ID: 8, Role: "assistant", Kind: "tool_result", ToolID: "t1", ToolName: "Bash", ToolOK: true, Content: "out", CreatedAt: at.Add(time.Second)},
	})
	want := []ui.ChatLine{
		{ID: 7, Role: "user", Content: "hello", At: at},
		{ID: 8, Role: "assistant", Kind: "tool_result", ToolID: "t1", ToolName: "Bash", ToolOK: true, Content: "out", At: at.Add(time.Second)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("chatLines:\n got %+v\nwant %+v", got, want)
	}
}
