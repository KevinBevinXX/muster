package hermeshost

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ZacxDev/muster/internal/hermeshost/hermestest"
)

// clarifyCall is an assistant row's tool_calls holding one clarify call (id)
// with questions, as Hermes writes it at the pin (measured: the arguments are
// a JSON string).
func clarifyCall(id string, questions []map[string]any) string {
	args, _ := json.Marshal(map[string]any{"questions": questions})
	b, _ := json.Marshal([]map[string]any{{"id": id, "type": "function", "function": map[string]any{"name": "clarify", "arguments": string(args)}}})
	return string(b)
}

// A pending clarify is read from the database: the newest clarify call no
// tool row answers yet; once answered it is gone.
func TestAPendingClarifyIsReadFromTheDatabase(t *testing.T) {
	h := hermestest.New(t)
	root := hermestest.LocalID()
	h.Session(root, hermestest.SessionOpts{})
	h.Message(root, hermestest.Msg{Role: "user", Content: "which colour?"})
	// An older clarify, answered: never shown again.
	h.Message(root, hermestest.Msg{Role: "assistant", ToolCalls: clarifyCall("call_old", []map[string]any{{"question": "Old?"}})})
	h.Message(root, hermestest.Msg{Role: "tool", ToolCallID: "call_old", Content: `{"responses":[]}`})
	h.Message(root, hermestest.Msg{Role: "assistant", ToolCalls: clarifyCall("call_new", []map[string]any{
		{"question": "Which colour should it use?", "choices": []string{"red", "blue", "", "green", "teal", "pink"}},
		{"question": "Both sides?", "multi_select": true},
		{"question": ""},
		{"question": "3"}, {"question": "4"}, {"question": "5"}, {"question": "6 — over the tool's five"},
	})})
	st := NewHome(h.Dir).OpenState()
	defer st.Close()
	got, err := st.PendingClarify(ctx(t), root)
	if err != nil {
		t.Fatal(err)
	}
	want := []ClarifyQuestion{
		{Question: "Which colour should it use?", Choices: []string{"red", "blue", "green", "teal"}},
		{Question: "Both sides?"},
		{Question: "3"}, {Question: "4"}, {Question: "5"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v\nwant %+v", got, want)
	}
	h.Message(root, hermestest.Msg{Role: "tool", ToolCallID: "call_new", Content: `{"responses":[]}`})
	if got, err := st.PendingClarify(ctx(t), root); err != nil || got != nil {
		t.Fatalf("an answered clarify is still pending: %+v %v", got, err)
	}
}

// Only a clarify call is a clarify: a pending terminal call (an approval's)
// is not one, whatever its arguments hold.
func TestAPendingTerminalCallIsNotAClarify(t *testing.T) {
	b, _ := json.Marshal([]map[string]any{{"id": "c1", "type": "function", "function": map[string]any{"name": "terminal", "arguments": `{"command":"rm -r x","questions":[{"question":"not a clarify"}]}`}}})
	if got := PendingClarifyOf([]Row{{Role: "assistant", ToolCalls: string(b)}}); got != nil {
		t.Fatalf("%+v", got)
	}
}

// A pending clarify keeps only what the card shows (audit #214 D1): no field
// nobody reads.
func TestAClarifyKeepsOnlyWhatTheCardShows(t *testing.T) {
	rt := reflect.TypeOf(ClarifyQuestion{})
	var got []string
	for i := 0; i < rt.NumField(); i++ {
		got = append(got, rt.Field(i).Name)
	}
	if !reflect.DeepEqual(got, []string{"Question", "Choices"}) {
		t.Fatalf("ClarifyQuestion fields = %v", got)
	}
}
