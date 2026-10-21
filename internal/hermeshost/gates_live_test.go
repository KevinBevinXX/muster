//go:build livehermes

package hermeshost

import (
	"encoding/json"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// pinnedApprovalSites is every place the pinned Hermes raises an approval a
// gateway viewer can be shown — each function (in a file the hermes_agent
// distribution installs) that names one of the approval entry points — as
// "file:function:entry point". Reviewed against the pin (audit #212 round
// 3): the terminal gate (check_dangerous_command, check_all_command_guards),
// execute_code, a plugin's rule, both write guards, computer use, the
// elicitation-consent routes (MCP elicitation, an untrusted MCP tool, a
// payment-card fill) and the editor's edit, plus the plumbing between them.
// A pin bump that adds or moves one is RED: classify the new gate — a label
// shape for NonTerminalLabels, or a description that no terminal gate
// emits — and update this list.
var pinnedApprovalSites = []string{
	"gateway/session_local_editor.py:request_editor_edit:_await_gateway_decision",
	"tools/approval.py:_human_decision:_await_gateway_decision",
	"tools/approval.py:_human_decision:notify_cb",
	"tools/approval.py:_run_approval_gate:_human_decision",
	"tools/approval.py:check_all_command_guards:_human_decision",
	"tools/approval.py:check_dangerous_command:_run_approval_gate",
	"tools/approval.py:check_execute_code_guard:_human_decision",
	"tools/approval.py:request_tool_approval:_run_approval_gate",
	"tools/approval_context.py:_should_fall_through_to_cli_approval:notify_cb",
	"tools/approval_gateway_wait.py:_await_gateway_decision:notify_cb",
	"tools/approval_prompt.py:request_elicitation_consent:_await_gateway_decision",
	"tools/approval_prompt.py:request_elicitation_consent:notify_cb",
	"tools/browser_vault_tool.py:_confirm_payment_fill:request_elicitation_consent",
	"tools/computer_use/tool.py:_request_approval:_run_approval_gate",
	"tools/file_tools_write_guards.py:_check_approval_required_write:_run_approval_gate",
	"tools/file_tools_write_guards.py:_request_protected_instruction_approval:_await_gateway_decision",
	"tools/file_tools_write_guards.py:_request_protected_instruction_approval:notify_cb",
	"tools/mcp_tool_handlers.py:_trust_gate_check:request_elicitation_consent",
	"tools/mcp_tool_sampling.py:_consent_thunk:request_elicitation_consent",
}

// pinnedLabels is each non-terminal gate's label, as the pin's source
// spells it (ast.unparse of the f-string), by "file:function".
var pinnedLabels = map[string]string{
	"tools/approval.py:request_tool_approval":                                  "f'<{tool_name}> (plugin approval rule)'",
	"tools/file_tools_write_guards.py:_check_approval_required_write":          "f'<write to {display_targets}>'",
	"tools/file_tools_write_guards.py:_request_protected_instruction_approval": "f'<write to {targets}>'",
	"tools/computer_use/tool.py:_request_approval":                             "f'computer_use: {_summarize_action(action, args)}'",
	"tools/approval.py:check_execute_code_guard":                               `f"execute_code <<'PY'\n{code}\nPY"`,
}

const approvalSitesScript = `import ast, glob, json, os, sys
src = sys.argv[1]
labels = json.loads(sys.argv[2])
rec = glob.glob(os.path.join(src, "hermes_agent-*.dist-info/RECORD"))
assert len(rec) == 1, rec
files = sorted(l.split(",")[0] for l in open(rec[0]) if l.split(",")[0].endswith(".py") and not l.startswith(".."))
GATES = {"_run_approval_gate", "_human_decision", "_await_gateway_decision", "request_elicitation_consent", "notify_cb"}
sites, found = set(), {}
class Sample(dict):
    def __missing__(self, k):
        return "rm -r ./build"
for rel in files:
    tree = ast.parse(open(os.path.join(src, rel)).read())
    for fn in ast.walk(tree):
        if not isinstance(fn, (ast.FunctionDef, ast.AsyncFunctionDef)):
            continue
        for n in ast.walk(fn):
            name = n.attr if isinstance(n, ast.Attribute) else n.id if isinstance(n, ast.Name) else None
            if name in GATES and fn.name != name:
                sites.add(rel + ":" + fn.name + ":" + name)
        key = rel + ":" + fn.name
        if key in labels:
            for n in ast.walk(fn):
                if isinstance(n, ast.JoinedStr) and ast.unparse(n) == labels[key]:
                    found[key] = eval(labels[key], {}, Sample(_summarize_action=lambda a, b: "click 120,300"))
print(json.dumps({"files": len(files), "sites": sorted(sites), "labels": found}))`

// TestTheApprovalGatesMatchThePin holds NonTerminalLabels to the pin (audit
// #212 round 3): every approval site the pinned Hermes has is one this list
// was reviewed against, each non-terminal gate's label f-string is still in
// its function as spelled here, and each — evaluated by the pinned Python
// with a terminal command for every value it interpolates — is caught by
// NonTerminalLabel.
func TestTheApprovalGatesMatchThePin(t *testing.T) {
	src, _ := pinnedSource(t)
	lb, _ := json.Marshal(pinnedLabels)
	cmd := exec.Command(pinnedPython(t), "-c", approvalSitesScript, src, string(lb))
	cmd.Env = append(os.Environ(), "HERMES_HOME="+t.TempDir())
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("read the pinned approval sites: %v\n%s", err, stderr.String())
	}
	var got struct {
		Files  int
		Sites  []string
		Labels map[string]string
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Files < 500 {
		t.Fatalf("positive control: only %d files in the distribution's RECORD", got.Files)
	}
	for _, s := range got.Sites {
		if !slices.Contains(pinnedApprovalSites, s) {
			t.Errorf("the pin raises an approval at %s, which was never classified: a label for NonTerminalLabels, or a description no terminal gate emits", s)
		}
	}
	for _, s := range pinnedApprovalSites {
		if !slices.Contains(got.Sites, s) {
			t.Errorf("%s is no longer an approval site in the pin: re-derive", s)
		}
	}
	for key, src := range pinnedLabels {
		label, ok := got.Labels[key]
		if !ok {
			t.Errorf("%s no longer spells its label %s: re-derive NonTerminalLabels", key, src)
			continue
		}
		if !NonTerminalLabel(label) {
			t.Errorf("%s's label %q is not caught by NonTerminalLabel", key, label)
		}
		if !strings.Contains(label, "rm -r ./build") && !strings.Contains(label, "click 120,300") {
			t.Errorf("%s's label %q interpolated nothing: the sample never reached it", key, label)
		}
	}
	t.Logf("%d files, %d approval sites, %d labels caught", got.Files, len(got.Sites), len(got.Labels))
}
