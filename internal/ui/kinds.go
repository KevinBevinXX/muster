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
//
// 🔴 Accounts ARE THE CALLER'S OWN. A member's list is the accounts they
// entered in Account → Credentials; the deployment's environment pool is the
// admin's and never appears on a member's form.
//
// Unavailable maps a kind the caller has no credential for to the sentence
// saying what to add. Such a kind renders DISABLED with that sentence beside
// it; the server refuses it regardless (api.resolveKindAccount).
type KindChoices struct {
	Kinds       []string
	Accounts    []string
	Unavailable map[string]string
}

// CredentialsPath is the page where a user adds their own credentials.
const CredentialsPath = "/account/credentials"

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
// submitted. So a claude-code dispatch carries no `model` (the server refuses
// it: the CLI chooses its model) and a gateway dispatch carries no `cc_account`
// (refused for any other kind). The repository applies to both kinds and is
// neither hidden nor disabled.
//
// 🔴 DISABLED WAS NOT ENOUGH, AND HIDING IS THE FIX FOR AN OPERATOR REPORT. The
// model combobox (curated OpenRouter slugs plus a live OpenRouter search), the
// repository picker (and, then, the privilege grants) stayed ON SCREEN for a
// Claude Code dispatch — greyed out under "Advanced", and on the task-scoped
// confirm as fully-rendered rows with live Edit buttons.
// Every control marked data-gateway-only (gatewayOnlyAttr) is now hidden while
// Claude Code is picked, in BOTH dispatch forms; the disabling stays, because a
// hidden input is still submitted and the disable is what keeps it off the wire.
const kindToggleScript = "var f=this.closest('form'),cc=!!f.querySelector('input[name=kind][value=" + agents.KindClaudeCode + "]:checked');" +
	"this.querySelectorAll('[data-cc-only]').forEach(function(e){e.classList.toggle('hidden',!cc)});" +
	"f.querySelectorAll('[" + gatewayOnlyAttr + "]').forEach(function(e){e.classList.toggle('hidden',cc)});" +
	"var a=f.querySelector('select[name=cc_account]');if(a){a.disabled=!cc}" +
	"f.querySelectorAll('input[name=model]').forEach(function(e){e.disabled=cc});"

// gatewayOnlyAttr marks a dispatch-form control that means nothing to a
// claude-code agent — the model selector (OpenRouter-specific).
// kindToggleScript hides every one while Claude Code is picked. ONE spelling, shared by the attribute and the script's selector.
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
	notice := unavailableNotice(k)
	if len(k.Kinds) < 2 {
		return notice
	}
	// The first AVAILABLE kind is checked (the gateway kind when the caller has
	// its credential, as before).
	first := -1
	for i, kind := range k.Kinds {
		if k.Unavailable[kind] == "" {
			first = i
			break
		}
	}
	options := make([]g.Node, 0, len(k.Kinds))
	for i, kind := range k.Kinds {
		off := k.Unavailable[kind] != ""
		options = append(options, Label(
			g.Attr("data-kind-option", kind),
			g.If(off, g.Attr("data-kind-unavailable", "")),
			Class("press flex min-h-[44px] cursor-pointer items-center justify-center rounded-lg px-3 py-2 text-sm font-medium text-fg ring-1 ring-inset ring-edge transition has-[:checked]:bg-accent has-[:checked]:text-on-accent has-[:focus-visible]:ring-2 has-[:focus-visible]:ring-focus has-[:disabled]:cursor-not-allowed has-[:disabled]:opacity-50"),
			Input(Type("radio"), Name("kind"), Value(kind), Class("sr-only"), g.If(i == first, Checked()), g.If(off, Disabled())),
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
	// 🔴 WHEN THE FIRST USABLE KIND IS NOT THE FIRST KIND (a member with a Claude
	// token and no OpenRouter key), the form opens on Claude Code — and every
	// control kindToggleScript manages was rendered for the gateway kind. The
	// script below runs that same toggle once on open, so the account pin is
	// live and the model/repo fields are off the wire from the first paint. ONE
	// toggle, not a second rendering of the same rules.
	preselectOther := first > 0
	return g.Group([]g.Node{
		FieldSet(
			g.Attr("data-kind-picker", ""),
			g.If(preselectOther, g.Attr("data-kind-init", "")),
			Class("flex flex-col gap-1.5"),
			hx("hx-on:change", kindToggleScript),
			Legend(Class("mb-1.5 text-xs font-medium text-muted"), g.Text("Agent kind")),
			Div(g.Attr("role", "radiogroup"), g.Attr("aria-label", "Agent kind"), Class("grid grid-cols-2 gap-2"), g.Group(options)),
			notice,
			account,
		),
		g.If(preselectOther, Script(g.Raw(kindInitScript))),
	})
}

// kindInitScript runs the picker's own toggle once for a picker that opened on
// a kind other than the first (see kindPicker).
//
// ⚠ IT CALLS kindToggleScript DIRECTLY (bound to the fieldset), RATHER THAN
// DISPATCHING A change EVENT, BECAUSE THE EVENT WAS MEASURED TO DO NOTHING:
// htmx evaluates a swapped-in <script> during the swap, before it has wired the
// fieldset's hx-on:change listener, so the dispatched event reached no handler
// and the form opened on Claude Code with its account pin hidden (the e2e
// byo-credentials spec caught it). Same string, so still one toggle.
const kindInitScript = "document.querySelectorAll('[data-kind-picker][data-kind-init]').forEach(function(p){" +
	"p.removeAttribute('data-kind-init');(function(){" + kindToggleScript + "}).call(p)})"

// unavailableNotice lists, for each kind the caller cannot use, what to add —
// with a link to the page where they add it. Empty when every kind is usable.
func unavailableNotice(k KindChoices) g.Node {
	var lines []g.Node
	for _, kind := range k.Kinds {
		if why := k.Unavailable[kind]; why != "" {
			lines = append(lines, Li(g.Attr("data-kind-unavailable-reason", kind), g.Text(why)))
		}
	}
	if len(lines) == 0 {
		return g.Text("")
	}
	return Div(
		g.Attr("data-kind-unavailable-notice", ""),
		g.Attr("role", "note"),
		Class("flex flex-col gap-1 rounded-lg bg-st-warning-bg px-3 py-2 text-xs text-st-warning-fg ring-1 ring-inset ring-st-warning-fg/40"),
		Ul(Class("flex flex-col gap-1"), g.Group(lines)),
		A(Href(CredentialsPath), Class("inline-flex min-h-[44px] items-center font-semibold underline"), g.Text("Open Account → Credentials")),
	)
}
