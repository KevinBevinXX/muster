package hermeshost

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ZacxDev/muster/internal/hermeshost/hermestest"
)

func pendingCall(id, name string, args map[string]any) string {
	a, _ := json.Marshal(args)
	b, _ := json.Marshal([]map[string]any{{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": string(a)}}})
	return string(b)
}

// The conversation's pending terminal commands are the RAW `command` of each
// `terminal` call no tool row answers — never another tool's arguments, never
// an answered call's.
func TestPendingTerminalCommandsAreTheRawCallsWithNoResult(t *testing.T) {
	h := hermestest.New(t)
	root := hermestest.LocalID()
	h.Session(root, hermestest.SessionOpts{})
	h.Message(root, hermestest.Msg{Role: "assistant", ToolCalls: pendingCall("c1", "terminal", map[string]any{"command": "ls"})})
	h.Message(root, hermestest.Msg{Role: "tool", ToolCallID: "c1", Content: "{}"})
	h.Message(root, hermestest.Msg{Role: "assistant", ToolCalls: pendingCall("c2", "write_file", map[string]any{"path": "~/.ssh/config", "command": "rm -r x"})})
	h.Message(root, hermestest.Msg{Role: "assistant", ToolCalls: pendingCall("c3", "terminal", map[string]any{"command": "curl -H X-Api-Key:a;rm -rf ~/w"})})
	st := NewHome(h.Dir).OpenState()
	defer st.Close()
	got, err := st.PendingTerminalCommands(ctx(t), root)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"curl -H X-Api-Key:a;rm -rf ~/w"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("%q, want %q", got, want)
	}
}
