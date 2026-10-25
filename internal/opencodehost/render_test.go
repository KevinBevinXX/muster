package opencodehost

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/cctranscript"
)

// msg decodes a synthetic Session.Message.Info (field names from the pinned
// openapi.json; every value invented).
func msg(t *testing.T, s string) Message {
	t.Helper()
	var m Message
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("%s: %v", s, err)
	}
	return m
}

func kinds(ks []Keyed) string {
	var b []string
	for _, k := range ks {
		e := k.Event
		b = append(b, k.Key+"="+e.Kind+":"+e.Text+e.Tool+"@"+e.Target+e.Notice+e.Output+e.Input)
	}
	return strings.Join(b, " | ")
}

// TestRenderMapsEachMessageType is the mapping table: a prompt, the
// assistant's text, its tool calls as NAME and TARGET only — never the input
// JSON nor the output — and their results as ok or not; reasoning is never
// drawn; a failed turn is an error; opencode's own messages are notices; an
// unknown type is a notice naming it, never the person's bubble.
func TestRenderMapsEachMessageType(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"user", `{"id":"msg_1","type":"user","text":"list the fixtures","files":[],"agents":[],"time":{"created":1700000000123}}`,
			"u=prompt:list the fixtures@"},
		{"user, blank", `{"id":"msg_1","type":"user","text":"  ","time":{"created":1}}`, ""},
		{"assistant, complete",
			`{"id":"msg_2","type":"assistant","agent":"build","model":{"id":"m","providerID":"p"},"finish":"tool-calls","time":{"created":2,"completed":3},
			 "content":[{"type":"reasoning","text":"private chain of thought","state":{"reasoningDetails":[{"type":"x","data":"opaque"}]}},
			            {"type":"text","text":"Listing them now."},
			            {"type":"tool","id":"call_a","name":"shell","executed":false,"time":{"created":2},
			             "state":{"status":"completed","input":{"command":"ls fixtures/\nrm -rf nothing","description":"list"},"content":[{"type":"text","text":"a.txt SECRET-OUTPUT"}]}},
			            {"type":"tool","id":"call_b","name":"read","time":{"created":2},
			             "state":{"status":"error","input":{"filePath":"/srv/fixture/b.txt"},"error":{"message":"no such file"},"content":[{"type":"text","text":"x"}]}}]}`,
			"t1=text:Listing them now.@ | c:call_a=tool_call:shell@ls fixtures/ | r:call_a=tool_result:@ | c:call_b=tool_call:read@/srv/fixture/b.txt | r:call_b=tool_result:@"},
		{"assistant, error finish", `{"id":"msg_3","type":"assistant","finish":"error","error":{"type":"api","message":"rate limited"},"time":{"created":1,"completed":2},"content":[]}`,
			"e=error:rate limited@"},
		{"idle, failed", `{"id":"msg_4","type":"idle","outcome":"failed","time":{"created":1}}`, "i=error:The turn failed.@"},
		{"idle, interrupted", `{"id":"msg_4","type":"idle","outcome":"interrupted","time":{"created":1}}`, "i=notice:turn interrupted@opencode"},
		{"idle, succeeded", `{"id":"msg_4","type":"idle","outcome":"succeeded","time":{"created":1}}`, ""},
		{"synthetic", `{"id":"msg_5","type":"synthetic","text":"background shell exited 0","description":"shell job","time":{"created":1}}`,
			"n=notice:synthetic: shell job@opencodebackground shell exited 0"},
		{"system", `{"id":"msg_6","type":"system","text":"instructions loaded","time":{"created":1}}`,
			"n=notice:system: instructions loaded@opencodeinstructions loaded"},
		{"skill", `{"id":"msg_7","type":"skill","skill":"sk_1","name":"fixture-skill","text":"the skill body","time":{"created":1}}`,
			"n=notice:skill: fixture-skill@opencodethe skill body"},
		{"shell, exited", `{"id":"msg_8","type":"shell","shellID":"sh_1","command":"make check","status":"exited","time":{"created":1}}`,
			"n=notice:shell exited: make check@opencode"},
		{"shell, running", `{"id":"msg_8","type":"shell","shellID":"sh_1","command":"make check","status":"running","time":{"created":1}}`, ""},
		{"compaction, running", `{"id":"msg_9","type":"compaction","status":"running","reason":"auto","time":{"created":1}}`, ""},
		{"compaction, done", `{"id":"msg_9","type":"compaction","status":"completed","reason":"auto","time":{"created":1}}`, "n=notice:compaction@opencode"},
		{"location-switched", `{"id":"msg_10","type":"location-switched","time":{"created":1}}`, "n=notice:moved to another directory@opencode"},
		{"unknown", `{"id":"msg_11","type":"brand-new-kind","text":"never a bubble","time":{"created":1}}`, "n=notice:brand-new-kind@opencode"},
	}
	for _, c := range cases {
		got := kinds(Render(msg(t, c.in)))
		if got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
		for _, leak := range []string{"private chain of thought", "opaque", "SECRET-OUTPUT", `"command"`, "rm -rf"} {
			if strings.Contains(got, leak) {
				t.Errorf("%s: %q is drawn: %s", c.name, leak, got)
			}
		}
	}
	if e := Render(msg(t, `{"id":"m","type":"user","text":"x","time":{"created":1700000000123}}`))[0].Event; e.Line != 1700000000123 {
		t.Errorf("a prompt's Line is %d, want its time.created", e.Line)
	}
}

// TestRenderEmitsOnlyWhatIsFinal: an assistant message is rewritten in place
// while it streams. Its last text item is not drawn until a later item exists
// or the message completes; a tool whose input is still streaming is not
// drawn; a running tool draws its call and, once it finishes, its result. So
// every key, once rendered, renders the same event in every later version —
// what lets the tail append rather than start over.
func TestRenderEmitsOnlyWhatIsFinal(t *testing.T) {
	versions := []string{
		`{"id":"m","type":"assistant","time":{"created":1},"content":[{"type":"text","text":"Look"}]}`,
		`{"id":"m","type":"assistant","time":{"created":1},"content":[{"type":"text","text":"Looking at it."},{"type":"tool","id":"c1","name":"grep","time":{"created":1},"state":{"status":"streaming","input":"{\"patt"}}]}`,
		`{"id":"m","type":"assistant","time":{"created":1},"content":[{"type":"text","text":"Looking at it."},{"type":"tool","id":"c1","name":"grep","time":{"created":1},"state":{"status":"running","input":{"pattern":"TODO"},"metadata":{}}}]}`,
		`{"id":"m","type":"assistant","time":{"created":1},"content":[{"type":"text","text":"Looking at it."},{"type":"tool","id":"c1","name":"grep","time":{"created":1},"state":{"status":"completed","input":{"pattern":"TODO"},"content":[{"type":"text","text":"3 hits"}]}},{"type":"text","text":"Found thr"}]}`,
		`{"id":"m","type":"assistant","finish":"stop","time":{"created":1,"completed":9},"content":[{"type":"text","text":"Looking at it."},{"type":"tool","id":"c1","name":"grep","time":{"created":1},"state":{"status":"completed","input":{"pattern":"TODO"},"content":[{"type":"text","text":"3 hits"}]}},{"type":"text","text":"Found three."}]}`,
	}
	wantKeys := []string{"", "t0", "t0 c:c1", "t0 c:c1 r:c1", "t0 c:c1 r:c1 t2"}
	seen := map[string]cctranscript.Event{}
	for i, v := range versions {
		ks := Render(msg(t, v))
		var keys []string
		for _, k := range ks {
			keys = append(keys, k.Key)
			if prev, ok := seen[k.Key]; ok && prev != k.Event {
				t.Fatalf("version %d changed key %s: %+v → %+v", i, k.Key, prev, k.Event)
			}
			seen[k.Key] = k.Event
		}
		if got := strings.Join(keys, " "); got != wantKeys[i] {
			t.Errorf("version %d: keys %q, want %q", i, got, wantKeys[i])
		}
	}
	if seen["t2"].Text != "Found three." || seen["c:c1"].Target != "TODO" || !seen["r:c1"].OK {
		t.Fatalf("final events %+v", seen)
	}
}
