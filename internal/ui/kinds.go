package ui

import (
	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/internal/agents"
)

// KindChoices is what the dispatch form offers for agent kinds: the kinds the
// deployment enables (agents.Kinds order) and, for claude-code, the configured
// Claude account NAMES the operator may pin. Never a token.
//
// ⚠ ONE KIND RENDERS NO PICKER AT ALL. A deployment that enables only the
// gateway kind — every deployment before kinds existed — gets the form it had,
// and the POST carries no `kind`, which the server reads as the gateway kind.
type KindChoices struct {
	Kinds    []string
	Accounts []string
}

func (k KindChoices) offersClaudeCode() bool {
	for _, kind := range k.Kinds {
		if kind == agents.KindClaudeCode {
			return true
		}
	}
	return false
}

// kindToggleScript shows the claude-code-only controls when that kind is
// checked, and DISABLES the controls that do not apply — a disabled input is not
// submitted. So a claude-code dispatch carries no `model`, `repo` or
// `grant_profile` (the server refuses each: the CLI chooses its model, nothing in
// the image clones a repo, and the pod mounts no ServiceAccount token) and a
// gateway dispatch carries no `cc_account` (refused for any other kind).
//
// 🔴 DISABLED WAS NOT ENOUGH, AND HIDING IS THE FIX FOR AN OPERATOR REPORT. The
// model combobox (curated OpenRouter slugs plus a live OpenRouter search), the
// repository picker and the privilege grants stayed ON SCREEN for a Claude Code
// dispatch — greyed out under "Advanced", and on the task-scoped confirm as
// fully-rendered "Model / Repository / Privileges" rows with live Edit buttons.
// Every control marked data-gateway-only (gatewayOnlyAttr) is now hidden while
// Claude Code is picked, in BOTH dispatch forms; the disabling stays, because a
// hidden input is still submitted and the disable is what keeps it off the wire.
const kindToggleScript = "var f=this.closest('form'),cc=!!f.querySelector('input[name=kind][value=" + agents.KindClaudeCode + "]:checked');" +
	"this.querySelectorAll('[data-cc-only]').forEach(function(e){e.classList.toggle('hidden',!cc)});" +
	"f.querySelectorAll('[" + gatewayOnlyAttr + "]').forEach(function(e){e.classList.toggle('hidden',cc)});" +
	"var a=f.querySelector('select[name=cc_account]');if(a){a.disabled=!cc}" +
	"f.querySelectorAll('input[name=model],input[name=repo],input[name=repo_branch],input[name=grant_profile]').forEach(function(e){e.disabled=cc});"

// gatewayOnlyAttr marks a dispatch-form control that means nothing to a
// claude-code agent — the model selector (OpenRouter-specific), the repository
// and the privilege grants. kindToggleScript hides every one while Claude Code is
// picked. ONE spelling, shared by the attribute and the script's selector.
const gatewayOnlyAttr = "data-gateway-only"

// gatewayOnly wraps nodes in a container kindToggleScript hides for claude-code.
// It renders VISIBLE: the first kind (agents.Kinds order) is the gateway kind,
// which is what kindPicker checks by default.
func gatewayOnly(nodes ...g.Node) g.Node {
	return Div(append([]g.Node{g.Attr(gatewayOnlyAttr, "")}, nodes...)...)
}

// kindPicker is the dispatch form's "Agent kind" control: a segmented radio pair
// sized for a thumb (44px rows, two columns that fit a 360px phone), plus the
// Claude account pin, shown only while Claude Code is selected.
func kindPicker(k KindChoices) g.Node {
	if len(k.Kinds) < 2 {
		return g.Text("")
	}
	options := make([]g.Node, 0, len(k.Kinds))
	for i, kind := range k.Kinds {
		options = append(options, Label(
			g.Attr("data-kind-option", kind),
			Class("press flex min-h-[44px] cursor-pointer items-center justify-center rounded-lg px-3 py-2 text-sm font-medium text-fg ring-1 ring-inset ring-edge transition has-[:checked]:bg-accent has-[:checked]:text-on-accent has-[:focus-visible]:ring-2 has-[:focus-visible]:ring-focus"),
			Input(Type("radio"), Name("kind"), Value(kind), Class("sr-only"), g.If(i == 0, Checked())),
			g.Text(agents.KindLabel(kind)),
		))
	}
	var account g.Node = g.Text("")
	if k.offersClaudeCode() {
		accts := make([]g.Node, 0, len(k.Accounts)+1)
		accts = append(accts, Option(Value(""), g.Text("Auto (least rate-limited)")))
		for _, a := range k.Accounts {
			accts = append(accts, Option(Value(a), g.Text(a)))
		}
		account = Div(
			g.Attr("data-cc-only", ""),
			Class("hidden flex flex-col gap-1.5"),
			Label(g.Attr("for", "cc-account"), Class("text-xs font-medium text-muted"), g.Text("Claude account")),
			Select(
				ID("cc-account"),
				Name("cc_account"),
				Disabled(),
				Class("min-h-[44px] w-full rounded-lg border-0 bg-bg px-3 py-2 text-sm text-fg ring-1 ring-inset ring-edge focus:outline-none focus:ring-2 focus:ring-focus"),
				g.Group(accts),
			),
			P(Class("text-xs text-muted"), g.Text("The agent keeps this account for life. The model is set by the Claude Code CLI.")),
		)
	}
	return FieldSet(
		g.Attr("data-kind-picker", ""),
		Class("flex flex-col gap-1.5"),
		hx("hx-on:change", kindToggleScript),
		Legend(Class("mb-1.5 text-xs font-medium text-muted"), g.Text("Agent kind")),
		Div(g.Attr("role", "radiogroup"), g.Attr("aria-label", "Agent kind"), Class("grid grid-cols-2 gap-2"), g.Group(options)),
		account,
	)
}
