package ui

import (
	"io"
	"strconv"
	"strings"
	"time"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// The Connectors tab: the secure connector store's operator surface — pending
// agent requests (approve / deny, or ENTER the credential when the connector does
// not exist yet), the connector list (rotate, delete, per-agent grants, revoke)
// and the recent audit trail.
//
// 🔴 NO VIEW HERE CAN RENDER A SECRET, BECAUSE NO VIEW TYPE HAS A FIELD FOR ONE.
// The secret is write-only: it travels from the operator's form to the store and
// is never read back into a page — not even a masked fragment of it. What the
// operator sees again is when it was created and last rotated.
//
// ⚠ THE CREDENTIAL FORM LIVES IN #connector-editor, WHICH NOTHING REFRESHES.
// The request list polls and listens for SSE; a form inside it would be wiped
// mid-typing by the next refresh, secret and all. So a request card's "Enter
// credential" button LOADS the form into the editor, and only a submit, a cancel,
// or another editor button replaces it.

// ConnectorView is one connector's public description, with its grants.
type ConnectorView struct {
	Name       string
	Platform   string
	Hosts      []string
	Scheme     string
	HeaderName string
	Username   string
	CreatedAt  time.Time
	RotatedAt  *time.Time
	LastUsedAt *time.Time
	Grants     []ConnectorGrantView
	// Managed, when non-empty, says the connector is provided by the server's
	// configuration (a virtual connector: the GitHub App) and why it has no
	// Rotate or Delete — the card shows it in their place.
	Managed string
	// Problem is a managed connector's current operator-facing failure, if any.
	Problem string
}

// ConnectorGrantView is one agent's live grant on a connector.
type ConnectorGrantView struct {
	ID          int64
	AgentName   string
	Status      string // pending | approved
	LastUsedAt  *time.Time
	RequestedAt time.Time
}

// ConnectorRequestView is one pending request.
type ConnectorRequestView struct {
	ID          int64
	AgentName   string
	Connector   string
	Reason      string
	RequestedAt time.Time
	// Exists is false when the agent asked for a connector nobody has configured:
	// the operator's answer is then the form that creates it.
	Exists bool
	// Problem, when non-empty, is why the connector cannot currently serve this
	// agent (a virtual connector's operator-facing failure, e.g. the GitHub App's
	// credentials refused): approving still records the decision, but the agent's
	// requests will fail until it is fixed.
	Problem string
}

// ConnectorAuditView is one audit line.
type ConnectorAuditView struct {
	At        time.Time
	Kind      string
	AgentName string
	Connector string
	Method    string
	Host      string
	Path      string
	Status    int
	Detail    string
}

// ConnectorPresetView is a preset offered in the form.
type ConnectorPresetView struct {
	Key, Label, Help, SecretLabel string
	Hosts                         []string
}

// ConnectorFormView is the create / enter-credential form.
type ConnectorFormView struct {
	// RequestID, when set, approves that pending request in the same submit.
	RequestID int64
	AgentName string
	Name      string
	Preset    string // a preset key, or "custom"
	Presets   []ConnectorPresetView
	Hosts     string
	Scheme    string
	Header    string
	Username  string
	Error     string
}

// ConnectorsContent is the whole /ui/connectors partial.
type ConnectorsContent struct {
	// Unconfigured, when non-empty, is why this server has no connector store;
	// the panel says so instead of rendering empty lists.
	Unconfigured string
	Notice       string
	Error        string
	Requests     []ConnectorRequestView
	Connectors   []ConnectorView
	Audit        []ConnectorAuditView
	// EditorForm, when set, is rendered into #connector-editor — a submit that
	// failed validation comes back with its fields (never its secret) and the
	// error, rather than losing what the operator typed.
	EditorForm *ConnectorFormView
}

// ConnectorsPanel is the tab's static shell; it lazy-loads /ui/connectors.
func ConnectorsPanel(active bool) g.Node {
	return Div(
		ID(panelID("connectors")),
		g.Attr("role", "tabpanel"),
		// destructiveSwap=true: the panel is its own innerHTML target.
		Class(panelClass(active, true)),
		hx("hx-get", "/ui/connectors"),
		hx("hx-trigger", "load"),
		hx("hx-target", "#"+panelID("connectors")),
		hx("hx-swap", "innerHTML"),
	)
}

// RenderConnectors writes the /ui/connectors partial.
func RenderConnectors(w io.Writer, c ConnectorsContent) error { return Connectors(c).Render(w) }

// RenderConnectorRequests writes the /ui/connector-requests partial.
func RenderConnectorRequests(w io.Writer, list []ConnectorRequestView) error {
	return ConnectorRequests(list).Render(w)
}

// RenderConnectorForm writes the create / enter-credential form.
func RenderConnectorForm(w io.Writer, f ConnectorFormView) error { return ConnectorForm(f).Render(w) }

// RenderConnectorRotateForm writes the rotate form for one connector.
func RenderConnectorRotateForm(w io.Writer, name, errMsg string) error {
	return ConnectorRotateForm(name, errMsg).Render(w)
}

const (
	btnPrimary   = "press min-h-[44px] inline-flex items-center justify-center rounded-lg bg-accent px-3 py-1.5 text-xs font-semibold text-on-accent transition hover:bg-accent/90"
	btnSecondary = "press min-h-[44px] inline-flex items-center justify-center rounded-lg bg-s2 px-3 py-1.5 text-xs font-medium text-fg ring-1 ring-inset ring-edge transition hover:bg-s3"
	btnDanger    = "press min-h-[44px] inline-flex items-center justify-center rounded-lg px-3 py-1.5 text-xs font-medium text-fg2 ring-1 ring-inset ring-edge transition hover:bg-s2 hover:text-st-error-fg"
	fieldCls     = "w-full rounded-lg border-0 bg-bg px-2.5 py-2 text-sm text-fg ring-1 ring-inset ring-edge focus:outline-none focus:ring-2 focus:ring-focus"
)

// panelTarget is what every mutating control swaps: the whole panel content.
func panelTarget() []g.Node {
	return []g.Node{hx("hx-target", "#"+panelID("connectors")), hx("hx-swap", "innerHTML")}
}

// Connectors renders the panel content.
func Connectors(c ConnectorsContent) g.Node {
	if c.Unconfigured != "" {
		return Section(
			ID("connectors-unconfigured"),
			Class("rounded-2xl border border-line bg-s1/50 p-4 ring-1 ring-line"),
			H2(Class("text-sm font-semibold text-fg"), g.Text("Connectors are not enabled")),
			P(Class("mt-2 text-xs text-muted"), g.Text(c.Unconfigured)),
		)
	}
	return Div(
		ID("connectors-content"),
		Class("flex min-w-0 flex-col gap-4"),
		g.If(c.Notice != "", P(ID("connectors-notice"), Class("rounded-xl bg-st-done-bg px-3 py-2 text-xs text-st-done-fg ring-1 ring-inset ring-st-done-fg/40"), g.Text(c.Notice))),
		g.If(c.Error != "", P(ID("connectors-error"), g.Attr("role", "alert"), Class("rounded-xl bg-st-error-bg px-3 py-2 text-xs text-st-error-fg ring-1 ring-inset ring-st-error-fg/40"), g.Text(c.Error))),
		Div(
			ID("connector-requests"),
			Class("empty:hidden"),
			hx("hx-get", "/ui/connector-requests"),
			hx("hx-trigger", "sse:connector.changed from:body, every 20s"),
			// 🔴 EXPLICIT, NOT INHERITED: the panel above carries
			// hx-target="#panel-connectors", and htmx inherits it — so without
			// this, a refresh of the request list (empty once the last request is
			// decided) replaced the WHOLE PANEL with nothing.
			hx("hx-target", "this"),
			hx("hx-swap", "innerHTML"),
			ConnectorRequests(c.Requests),
		),
		editor(c.EditorForm),
		connectorList(c.Connectors),
		connectorAudit(c.Audit),
	)
}

func editor(f *ConnectorFormView) g.Node {
	if f == nil {
		return Div(ID("connector-editor"), Class("empty:hidden"))
	}
	return Div(ID("connector-editor"), Class("empty:hidden"), ConnectorForm(*f))
}

// ConnectorRequests renders the pending-request cards (empty when none).
func ConnectorRequests(list []ConnectorRequestView) g.Node {
	if len(list) == 0 {
		return g.Text("")
	}
	return Section(
		Class("rounded-2xl border border-st-warning-fg/40 bg-st-warning-bg p-4 ring-1 ring-st-warning-fg/40"),
		Div(
			Class("mb-3 flex items-center gap-2"),
			Span(Class("text-base"), g.Text("🔌")),
			H2(Class("text-sm font-semibold text-st-warning-fg"), g.Text("Connector requests")),
			Span(Class("rounded-full bg-st-warning-bg px-2 py-0.5 text-xs font-semibold text-st-warning-fg"), g.Text(strconv.Itoa(len(list)))),
		),
		Div(Class("flex flex-col gap-2"), g.Map(list, connectorRequestCard)),
	)
}

func connectorRequestCard(r ConnectorRequestView) g.Node {
	ids := strconv.FormatInt(r.ID, 10)
	var actions g.Node
	if r.Exists {
		actions = Form(
			Class("mt-3 flex flex-wrap items-center gap-2"),
			hx("hx-post", "/ui/connector-requests/"+ids+"/approve"),
			g.Group(panelTarget()),
			Button(Type("submit"), Class(btnPrimary), g.Attr("data-approve", ids), g.Text("Approve")),
			Button(Type("button"), Class(btnDanger), g.Attr("data-deny", ids),
				hx("hx-post", "/ui/connector-requests/"+ids+"/deny"), g.Group(panelTarget()), g.Text("Deny")),
		)
	} else {
		actions = Div(
			Class("mt-3 flex flex-wrap items-center gap-2"),
			Button(Type("button"), Class(btnPrimary), g.Attr("data-enter-credential", ids),
				hx("hx-get", "/ui/connectors/new?request="+ids),
				hx("hx-target", "#connector-editor"),
				hx("hx-swap", "innerHTML show:#connector-editor:top"),
				g.Text("Enter credential…")),
			Button(Type("button"), Class(btnDanger), g.Attr("data-deny", ids),
				hx("hx-post", "/ui/connector-requests/"+ids+"/deny"), g.Group(panelTarget()), g.Text("Deny")),
		)
	}
	return Div(
		Class("rounded-xl bg-s1/70 p-3 ring-1 ring-inset ring-line"),
		g.Attr("data-connector-request", r.Connector),
		Div(
			Class("flex flex-wrap items-center gap-2"),
			Span(Class("min-w-0 break-all text-sm font-semibold text-fg"), g.Text(r.AgentName)),
			Span(Class("text-muted"), g.Text("wants")),
			Span(Class("inline-flex items-center rounded-full bg-st-warning-bg px-2.5 py-1 text-xs font-semibold text-st-warning-fg ring-1 ring-inset ring-st-warning-fg/40"), g.Text(r.Connector)),
			Span(Class("flex-1")),
			cardTime(r.RequestedAt),
		),
		g.If(!r.Exists, P(Class("mt-2 text-xs text-muted"), g.Text("No connector by that name exists yet. Enter a credential to create it and approve this request in one step."))),
		g.If(r.Problem != "", connectorProblem(r.Problem)),
		g.If(r.Reason != "", P(Class("mt-2 whitespace-pre-wrap break-words text-xs leading-relaxed text-muted"), g.Text(r.Reason))),
		actions,
	)
}

// ConnectorForm is the create / enter-credential form.
func ConnectorForm(f ConnectorFormView) g.Node {
	var preset *ConnectorPresetView
	for i := range f.Presets {
		if f.Presets[i].Key == f.Preset {
			preset = &f.Presets[i]
		}
	}
	title := "Add a connector"
	if f.RequestID != 0 {
		title = "Enter a credential for " + f.Name
	}
	presetOpts := []g.Node{}
	for _, p := range f.Presets {
		presetOpts = append(presetOpts, Option(Value(p.Key), g.If(p.Key == f.Preset, Selected()), g.Text(p.Label)))
	}
	presetOpts = append(presetOpts, Option(Value("custom"), g.If(preset == nil, Selected()), g.Text("Custom platform")))
	// 🔴 EVERY preset FIELD IS READ HERE, BEHIND THE nil CHECK, NOT INSIDE g.If:
	// gomponents' g.If evaluates its node argument eagerly, so `g.If(preset !=
	// nil, …preset.Hosts…)` dereferences nil for a custom platform.
	secretLabel := "Secret (token, key or password)"
	presetHelp, presetHosts := "", ""
	if preset != nil {
		if preset.SecretLabel != "" {
			secretLabel = preset.SecretLabel
		}
		presetHelp, presetHosts = preset.Help, strings.Join(preset.Hosts, ", ")
	}
	reqID := ""
	if f.RequestID != 0 {
		reqID = strconv.FormatInt(f.RequestID, 10)
	}
	return Section(
		ID("connector-form"),
		Class("rounded-2xl border border-line bg-s1/70 p-4 ring-1 ring-line"),
		H2(Class("mb-1 break-all text-sm font-semibold text-fg"), g.Text(title)),
		g.If(f.AgentName != "", P(Class("mb-3 break-all text-xs text-muted"), g.Text("Requested by "+f.AgentName+". Submitting creates the connector and approves this agent."))),
		g.If(f.Error != "", P(g.Attr("role", "alert"), Class("mb-3 rounded-lg bg-st-error-bg px-3 py-2 text-xs text-st-error-fg"), g.Text(f.Error))),
		Form(
			Class("flex flex-col gap-3"),
			hx("hx-post", "/connectors"),
			g.Group(panelTarget()),
			g.Attr("autocomplete", "off"),
			Input(Type("hidden"), Name("request_id"), Value(reqID)),
			labelledFieldFor("connector-preset", "Platform",
				Select(ID("connector-preset"), Name("preset"), Class(fieldCls),
					hx("hx-get", "/ui/connectors/new"),
					hx("hx-trigger", "change"),
					hx("hx-include", "closest form"),
					hx("hx-target", "#connector-editor"),
					hx("hx-swap", "innerHTML"),
					g.Group(presetOpts)),
			),
			g.If(presetHelp != "", P(Class("text-xs leading-relaxed text-muted"), g.Text(presetHelp))),
			labelledFieldFor("connector-name", "Name",
				Input(ID("connector-name"), Name("name"), Type("text"), Value(f.Name), Required(),
					g.If(f.RequestID != 0, g.Attr("readonly", "")),
					g.Attr("pattern", "[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?"), g.Attr("spellcheck", "false"),
					g.Attr("autocapitalize", "off"), Class(fieldCls))),
			g.If(preset != nil, P(Class("break-all text-xs text-muted"), g.Text("Hosts: "+presetHosts))),
			g.If(preset == nil, g.Group([]g.Node{
				labelledFieldFor("connector-hosts", "Allowed hosts (one per line, e.g. api.example.com)",
					Textarea(ID("connector-hosts"), Name("hosts"), Rows("2"), Required(), g.Attr("spellcheck", "false"),
						g.Attr("autocapitalize", "off"), Class(fieldCls), g.Text(f.Hosts))),
				labelledFieldFor("connector-scheme", "Authentication",
					Select(ID("connector-scheme"), Name("scheme"), Class(fieldCls),
						Option(Value("bearer"), g.If(f.Scheme == "" || f.Scheme == "bearer", Selected()), g.Text("Bearer token (Authorization: Bearer …)")),
						Option(Value("header"), g.If(f.Scheme == "header", Selected()), g.Text("Custom header")),
						Option(Value("basic"), g.If(f.Scheme == "basic", Selected()), g.Text("Basic auth (username + password)")),
					)),
				labelledFieldFor("connector-header", "Header name (custom header only)",
					Input(ID("connector-header"), Name("header_name"), Type("text"), Value(f.Header), g.Attr("spellcheck", "false"),
						g.Attr("autocapitalize", "off"), Class(fieldCls))),
				labelledFieldFor("connector-username", "Username (basic auth only)",
					Input(ID("connector-username"), Name("username"), Type("text"), Value(f.Username), g.Attr("spellcheck", "false"),
						g.Attr("autocapitalize", "off"), Class(fieldCls))),
			})),
			labelledFieldFor("connector-secret", secretLabel,
				Input(ID("connector-secret"), Name("secret"), Type("password"), Required(), g.Attr("minlength", "8"),
					g.Attr("autocomplete", "new-password"), g.Attr("spellcheck", "false"), Class(fieldCls))),
			Div(
				Class("flex flex-wrap items-center gap-2"),
				Button(Type("submit"), Class(btnPrimary), g.Attr("data-save-connector", ""), hx("hx-disabled-elt", "this"),
					g.If(f.RequestID != 0, g.Text("Save and approve")), g.If(f.RequestID == 0, g.Text("Save connector"))),
				Button(Type("button"), Class(btnDanger), g.Attr("data-cancel-editor", ""),
					hx("hx-get", "/ui/connectors"), g.Group(panelTarget()), g.Text("Cancel")),
			),
		),
	)
}

// ConnectorRotateForm replaces a connector's secret.
func ConnectorRotateForm(name, errMsg string) g.Node {
	return Section(
		ID("connector-rotate-form"),
		Class("rounded-2xl border border-line bg-s1/70 p-4 ring-1 ring-line"),
		H2(Class("mb-3 break-all text-sm font-semibold text-fg"), g.Text("Rotate the secret of "+name)),
		g.If(errMsg != "", P(g.Attr("role", "alert"), Class("mb-3 rounded-lg bg-st-error-bg px-3 py-2 text-xs text-st-error-fg"), g.Text(errMsg))),
		Form(
			Class("flex flex-col gap-3"),
			hx("hx-post", "/connectors/"+name+"/rotate"),
			g.Group(panelTarget()),
			g.Attr("autocomplete", "off"),
			labelledFieldFor("connector-rotate-secret", "New secret",
				Input(ID("connector-rotate-secret"), Name("secret"), Type("password"), Required(), g.Attr("minlength", "8"),
					g.Attr("autocomplete", "new-password"), Class(fieldCls))),
			P(Class("text-xs text-muted"), g.Text("Every agent's grant is kept; the new secret is used from the next request.")),
			Div(
				Class("flex flex-wrap items-center gap-2"),
				Button(Type("submit"), Class(btnPrimary), hx("hx-disabled-elt", "this"), g.Text("Replace secret")),
				Button(Type("button"), Class(btnDanger), hx("hx-get", "/ui/connectors"), g.Group(panelTarget()), g.Text("Cancel")),
			),
		),
	)
}

func connectorList(list []ConnectorView) g.Node {
	return Section(
		ID("connector-list"),
		Class("rounded-2xl border border-line bg-s1/50 p-4 ring-1 ring-line"),
		Div(
			Class("mb-3 flex flex-wrap items-center gap-2"),
			Span(Class("text-base"), g.Text("🔌")),
			H2(Class("text-sm font-semibold text-fg"), g.Text("Connectors")),
			Span(Class("rounded-full bg-s3/50 px-2 py-0.5 text-xs font-semibold text-fg2"), g.Text(strconv.Itoa(len(list)))),
			Span(Class("flex-1")),
			Button(Type("button"), Class(btnSecondary), g.Attr("data-add-connector", ""),
				hx("hx-get", "/ui/connectors/new"), hx("hx-target", "#connector-editor"),
				hx("hx-swap", "innerHTML show:#connector-editor:top"), g.Text("Add connector")),
		),
		g.If(len(list) == 0, P(Class("text-xs text-muted"), g.Text("No connectors yet. Agents can request one by name, or add one here."))),
		g.If(len(list) > 0, Div(Class("flex flex-col gap-2"), g.Map(list, connectorCard))),
	)
}

func optTime(t *time.Time, none string) g.Node {
	if t == nil {
		return g.Text(none)
	}
	return cardTime(*t)
}

func connectorCard(c ConnectorView) g.Node {
	auth := c.Scheme
	switch c.Scheme {
	case "header":
		auth = "header " + c.HeaderName
	case "basic":
		auth = "basic (" + c.Username + ")"
	}
	return Div(
		Class("min-w-0 rounded-xl bg-bg/50 p-3 ring-1 ring-inset ring-line"),
		g.Attr("data-connector", c.Name),
		Div(
			Class("flex flex-wrap items-center gap-2"),
			Span(Class("rounded-md bg-st-review-bg px-2 py-0.5 text-xs font-semibold text-st-review-fg ring-1 ring-inset ring-st-review-fg/40"), g.Text(c.Name)),
			Span(Class("text-xs text-muted"), g.Text(c.Platform)),
		),
		P(Class("mt-1 break-all text-xs text-muted"), g.Text(strings.Join(c.Hosts, ", ")+" · "+auth)),
		P(Class("mt-1 flex flex-wrap gap-x-3 text-xs text-muted"),
			Span(g.Text("created "), cardTime(c.CreatedAt)),
			Span(g.Attr("data-last-used", ""), g.Text("last used "), optTime(c.LastUsedAt, "never")),
			Span(g.Attr("data-rotated", ""), g.Text("rotated "), optTime(c.RotatedAt, "never")),
		),
		g.If(len(c.Grants) > 0, Div(Class("mt-2 flex flex-col gap-1"), g.Map(c.Grants, connectorGrantRow))),
		g.If(len(c.Grants) == 0, P(Class("mt-2 text-xs text-muted"), g.Text("No agent holds a grant."))),
		g.If(c.Problem != "", connectorProblem(c.Problem)),
		connectorCardActions(c),
	)
}

// connectorCardActions is Rotate and Delete, or — for a connector the server's
// configuration provides (ConnectorView.Managed) — the sentence saying why
// there are none: the store refuses both for it (connectors.ErrManaged).
func connectorCardActions(c ConnectorView) g.Node {
	if c.Managed != "" {
		return P(Class("mt-2 text-xs text-muted"), g.Attr("data-connector-managed", c.Name), g.Text(c.Managed))
	}
	return Div(
		Class("mt-3 flex flex-wrap items-center gap-2"),
		Button(Type("button"), Class(btnSecondary), g.Attr("data-rotate", c.Name),
			hx("hx-get", "/ui/connectors/"+c.Name+"/rotate"), hx("hx-target", "#connector-editor"),
			hx("hx-swap", "innerHTML show:#connector-editor:top"), g.Text("Rotate")),
		Button(Type("button"), Class(btnDanger), g.Attr("data-delete", c.Name),
			hx("hx-delete", "/connectors/"+c.Name), g.Group(panelTarget()),
			hx("hx-confirm", "Delete connector "+c.Name+"? Its secret is destroyed and every agent's grant on it is removed."),
			g.Text("Delete")),
	)
}

// connectorProblem is a virtual connector's operator-facing failure, on a
// request card or a connector card. The text is the server's own (githubapp's
// wording, quoting at most GitHub's error message) and never carries a secret.
func connectorProblem(msg string) g.Node {
	return P(g.Attr("role", "alert"), g.Attr("data-connector-problem", ""),
		Class("mt-2 break-words rounded-lg bg-st-error-bg px-3 py-2 text-xs text-st-error-fg ring-1 ring-inset ring-st-error-fg/40"),
		g.Text(msg))
}

func connectorGrantRow(gv ConnectorGrantView) g.Node {
	ids := strconv.FormatInt(gv.ID, 10)
	return Div(
		Class("flex flex-wrap items-center gap-2 rounded-lg bg-s1/60 px-2 py-1"),
		g.Attr("data-grant", gv.AgentName),
		Span(Class("min-w-0 break-all text-xs font-semibold text-fg"), g.Text(gv.AgentName)),
		Span(Class("text-xs text-muted"), g.Attr("data-grant-status", gv.Status), g.Text(gv.Status)),
		Span(Class("flex-1")),
		Button(Type("button"), Class("press min-h-[44px] rounded-lg px-2 text-xs font-medium text-fg2 transition hover:text-st-error-fg"),
			g.Attr("data-revoke", gv.AgentName),
			hx("hx-delete", "/connector-grants/"+ids), g.Group(panelTarget()),
			hx("hx-confirm", "Revoke "+gv.AgentName+"'s access?"),
			g.Text("Revoke")),
	)
}

func connectorAudit(list []ConnectorAuditView) g.Node {
	rows := make([]g.Node, 0, len(list))
	for _, e := range list {
		what := e.Detail
		if e.Kind == "proxy" {
			what = e.Method + " " + e.Host + e.Path
			if e.Status != 0 {
				what += " → " + strconv.Itoa(e.Status)
			}
		}
		rows = append(rows, Li(
			Class("flex min-w-0 flex-wrap gap-x-2 text-xs text-muted"),
			cardTime(e.At),
			Span(Class("font-semibold text-fg2"), g.Text(e.AgentName)),
			Span(g.Text(e.Connector)),
			Span(Class("min-w-0 break-all font-mono"), g.Text(what)),
		))
	}
	return Section(
		ID("connector-audit"),
		Class("rounded-2xl border border-line bg-s1/50 p-4 ring-1 ring-line"),
		H2(Class("mb-2 text-sm font-semibold text-fg"), g.Text("Recent activity")),
		g.If(len(rows) == 0, P(Class("text-xs text-muted"), g.Text("Nothing yet."))),
		g.If(len(rows) > 0, Ul(Class("flex flex-col gap-1"), g.Group(rows))),
	)
}
