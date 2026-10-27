package api

import (
	"io"
	"log"
	"sort"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/flows"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plugins"
)

// gatewayRuntimeBuiltinToolNames are the tool names the gateway runtime image
// (tag 2026.5.7) reserves against a request's client tools: a client tool whose
// normalised name is one of these fails the WHOLE TURN with "client tool name
// conflict: <name>" before the model is called.
//
// 🔴 READ OUT OF THE IMAGE, NOT GUESSED, AND A SUPERSET ON PURPOSE. The runtime
// compares against its CORE built-in tools for the turn plus a fixed reserved
// list; which core tools a turn has depends on its tool profile, so this is every
// tool its core catalogue declares (whatever the profile), every tool-shaped
// definition (a name with parameters and an execute/description) in its bundle,
// and the reserved list. Extracted from the image's own JavaScript (the core
// tool catalogue, the reserved-name constant, and a scan of the bundle for
// tool definitions). A superset can only refuse a name that would have been
// safe; a subset would let the next conflict through.
//
// ⚠ RE-EXTRACT ON AN IMAGE BUMP. A new built-in in a later image is a new way
// for every gateway turn to fail; nothing in this repository can see the image.
var gatewayRuntimeBuiltinToolNames = []string{
	// The core tool catalogue.
	"agents_list", "apply_patch", "browser", "canvas", "code_execution", "cron", "edit", "exec",
	"gateway", "heartbeat_respond", "image", "image_generate", "memory_get", "memory_search",
	"message", "music_generate", "nodes", "process", "read", "sessions_history", "sessions_list",
	"sessions_send", "sessions_spawn", "session_status", "sessions_yield", "subagents", "tts",
	"update_plan", "video_generate", "web_fetch", "web_search", "write", "x_search",
	// Tool definitions in the bundle outside the catalogue.
	"pdf", "dir_list", "file_write", "ping",
	// The reserved list.
	"bash", "find", "grep", "ls",
}

// gatewayRuntimeToolName is the runtime's normalisation before it compares:
// lower-case, then its two aliases. Copied from the image's code, not derived
// from muster's: a copy that agreed with muster by construction could not catch
// a name that only collides after the runtime folds it.
func gatewayRuntimeToolName(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	switch n {
	case "bash":
		return "exec"
	case "apply-patch":
		return "apply_patch"
	}
	return n
}

// toolNameConflicts is the runtime's check, applied to a list of client tool
// names: every name that collides with a built-in, or with another name in the
// list, after normalisation.
func toolNameConflicts(names []string) []string {
	builtin := map[string]bool{}
	for _, b := range gatewayRuntimeBuiltinToolNames {
		builtin[gatewayRuntimeToolName(b)] = true
	}
	seen := map[string]string{}
	var out []string
	for _, n := range names {
		k := gatewayRuntimeToolName(n)
		if builtin[k] {
			out = append(out, n)
		}
		if prior, dup := seen[k]; dup {
			out = append(out, prior+"="+n)
		}
		seen[k] = n
	}
	sort.Strings(out)
	return out
}

// everyExtensionOn is a Server whose registry offers EVERY tool it can: each
// store a tool is gated on is present (as a value that is non-nil and is never
// called — agentTools only checks presence), task routing with agent dispatch
// on. A tool added later behind a new gate needs its gate set here; the
// ledger-length check below fails until it is.
func everyExtensionOn(t *testing.T) *Server {
	t.Helper()
	s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{
		Agents:       stubAgentsStore{},
		Notes:        struct{ notes.Store }{},
		Connectors:   struct{ connectors.Store }{},
		Flows:        struct{ flows.Store }{},
		Plugins:      struct{ plugins.Store }{},
		Focus:        struct{ focus.Store }{},
		Routes:       struct{ plugins.RouteStore }{},
		Routing:      &Routing{},
		TaskRouting:  &TaskRouting{DispatchOwners: map[int64]bool{1: true}},
		TaskDispatch: struct{ plugins.DispatchStore }{},
		Attention:    struct{ attention.Store }{},
	})
	return s
}

// TestNoChiefToolCollidesWithTheGatewayRuntimesBuiltins: no tool muster's
// registry can offer — with every gate open, so the agent's whole set — has a
// name the gateway runtime reserves, or a name another muster tool already has,
// under the runtime's own normalisation. Red on the tree before the rename:
// agents_list.
func TestNoChiefToolCollidesWithTheGatewayRuntimesBuiltins(t *testing.T) {
	s := everyExtensionOn(t)
	var names []string
	for _, tl := range s.agentTools() {
		names = append(names, tl.def.Name)
	}
	// Positive control on the registry: every gate above is open, so every
	// ledgered tool is here (plus the gated ones the ledgers leave out).
	want := len(mcpLedger) + len(gatewayOnlyLedger)
	if len(names) <= want {
		t.Fatalf("positive control: the all-gates registry has %d tools, want more than the %d the ledgers name "+
			"(route_to_session and the task-routing tools are behind gates the ledger fixture leaves shut): %v", len(names), want, names)
	}
	for _, n := range []string{"route_to_session", "task_dispatch", "task_forward", "attention_list", "sessions_overview", "connector_request"} {
		found := false
		for _, have := range names {
			found = found || have == n
		}
		if !found {
			t.Fatalf("positive control: the all-gates registry lacks %s: %v", n, names)
		}
	}
	// Positive control on the check: it does see a built-in, after folding.
	if got := toolNameConflicts([]string{"Agents_List", "bash", "fine_name"}); strings.Join(got, ",") != "Agents_List,bash" {
		t.Fatalf("positive control: the conflict check on a list holding two built-ins found %v", got)
	}
	if got := toolNameConflicts(names); len(got) != 0 {
		t.Errorf("muster's agent tools collide with the gateway runtime's built-ins (or each other): %v — every "+
			"gateway-runtime turn would fail with \"client tool name conflict\"", got)
	}
	// And the retired name is never listed, under any gate.
	for _, n := range names {
		if _, retired := toolNameAliases[n]; retired {
			t.Errorf("the retired tool name %s is listed; listing it brings the conflict back", n)
		}
	}
}

// TestARetiredToolNameStillRunsForEveryAgent: an agent that still calls the
// retired agents_list gets fleet_list's answer, whichever agent it is.
//
// ⚠ IT USED TO ALSO PIN THAT THE ALIAS COULD NOT STEP AROUND THE AGENT-ONLY
// CHECK. There is no such check to step around now, so that half is replaced by
// its opposite: the alias resolves for a worker exactly as for any other agent,
// which is the property the canonicalisation has to keep.
func TestARetiredToolNameStillRunsForEveryAgent(t *testing.T) {
	g := newGatedFixture(t)
	for old, now := range toolNameAliases {
		if got, want := g.srv.dispatchAgentTool(g.asOwner, g.agent, old, "{}"), g.srv.dispatchAgentTool(g.asOwner, g.agent, now, "{}"); got != want {
			t.Errorf("a fleet agent calling %s got %s, want %s's answer %s", old, got, now, want)
		}
		if got, want := g.srv.dispatchAgentTool(g.asOwner, g.worker, old, "{}"), g.srv.dispatchAgentTool(g.asOwner, g.worker, now, "{}"); got != want {
			t.Errorf("a worker calling %s got %s, want %s's answer %s", old, got, now, want)
		}
		r, _ := g.mcp(t, g.chiefTok, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"`+old+`","arguments":{}}}`, nil)
		if r == nil || r.Error != nil {
			t.Errorf("tools/call %s over /mcp for the agent: %+v, want fleet_list's result", old, r)
		}
	}
	// Control: a name that is neither listed nor retired is still unknown.
	if out := g.srv.dispatchAgentTool(g.asOwner, g.agent, "agents_lists", "{}"); out != toolErr("unknown tool %q", "agents_lists") {
		t.Errorf("control: an unregistered name answered %s", out)
	}
	_ = agents.Agent{}
}
