package textplain

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// THE TITLE SINK LEDGER.
//
// 🔴 A TITLE IS SHOWN THROUGH Title OR IT IS LEDGERED HERE WITH A REASON. The
// operator's report was "titles sometimes include markdown": not one surface,
// but whichever surface someone had not yet routed through the stripper. A
// table test on Title cannot see a sink that never calls it, so this test
// derives the set of title READS from the source and checks each one:
//
//   - internal/ui, every function: a read of a title-bearing name (titleNames)
//     must sit inside a rule application — textplain.Title(...) or plainOr's
//     first argument — or be passed, at a name position, to one of
//     plainingHelpers (each checked to apply the rule to exactly those
//     arguments and never draw them raw), or be in uiExempt with the reason
//     it is not a display of the title.
//   - internal/api, every function that hand-writes HTML (calls
//     html.EscapeString) or builds a notification BODY (pushBodyBuilders): the
//     same, against apiExempt. The api package's other title reads build view
//     models (rendered in internal/ui, checked above), JSON/MCP data for agents
//     (not a rendered title) or stored records (never rewritten), so they are
//     out of this ledger by construction.
//   - push: every way a notification leaves the server is reached from
//     pushEmitters only, and goNotify applies Title to the notification's
//     TITLE, once, and not to its body (TestGoNotifyPlainsTheTitleOnly in
//     internal/api holds the behaviour; this holds the structure).
//   - applied once: no value reaches the rule twice on its way to a sink.
//
// Both directions fail: a new unledgered read (a new sink, or an old one that
// stopped calling Title) and a ledger row that no longer matches a read (a
// stale exemption that would otherwise silently cover the next one).

// titleNames are the selectors and calls that yield a title: a task's, an
// agent's display name, a mirrored window's label/title, a chat session's, a
// focus summary's, an attention item's source.
var titleNames = map[string]bool{
	"Title": true, "DisplayName": true, "Label": true, "DisplayTitle": true,
	"TaskTitle": true,
	// A focus's description is the summary under its title, drawn as one line
	// of text: the same rule applies.
	"Description": true,
}

// bareAccessors are the title accessors package ui calls unqualified.
var bareAccessors = map[string]bool{"TaskTitle": true}

// apiTitleNames adds the attention item's fields, which are titles in api's
// hand-written attention HTML.
var apiTitleNames = map[string]bool{"Source": true, "Focus": true}

// plainingHelpers are ui functions that apply Title to their name arguments at
// emit. An argument read passed straight to one counts as covered; each is
// checked below to call Title (or another helper in this set).
var plainingHelpers = map[string]bool{
	"mirrorEntryTitle":    true,
	"mirrorCardTitle":     true,
	"entryTitle":          true,
	"mirrorSessionTitle":  true,
	"mirrorTerminalTitle": true,
}

// helperNameArgs are, per plaining helper, the argument positions that carry a
// NAME (and are plained); the others (a raw session name, a qualifier, a CSS
// class) are drawn as written. checkPlainingHelpers holds each helper to it.
var helperNameArgs = map[string][]int{
	"mirrorEntryTitle":    {0, 1}, // label, title
	"mirrorCardTitle":     {0, 1}, // label, title
	"entryTitle":          {0, 1}, // label, title
	"mirrorSessionTitle":  {0},    // label
	"mirrorTerminalTitle": {0},    // title
	"plainOr":             {0},    // s (the fallback is drawn as written)
}

type exemption struct {
	n   int // how many reads of expr in that function
	why string
}

const (
	whyFilter   = "filter haystack / rank key: matched against what the user types, never drawn"
	whyForm     = "edit form value: the raw stored title, edited as written"
	whyStatic   = "a fixed UI label (tab, status, preset, model, runtime, quick key, chip), never author text"
	whyAccessor = "the accessor itself; every display caller applies Title to its result"
	whyOption   = "an answer option: submitted verbatim as the answer, so shown verbatim"
	whyDispatch = "a query parameter into the dispatch form; DispatchModalTaskBody applies Title at emit"
	whyPerson   = "a person's (user account's) display name, not a title"
)

// uiExempt: "file func expr" → exemption.
var uiExempt = map[string]exemption{
	"focus.go focusHaystack f.Description":             {1, whyFilter},
	"mirror_ask.go askForm o.Description":              {2, whyOption},
	"mirror_ask.go askReadOnly o.Description":          {2, whyOption},
	"subagent_cards.go subagentCallOf in.Description":  {1, "a subagent call's input, copied into the view model; drawn by subagentSummary"},
	"subagent_cards.go subagentSummary s.Description":  {2, "a subagent's tool-call description, shown as Claude Code wrote it, like the transcript row it summarises"},
	"terminal.go AgentTerminalPage v.DisplayName":      {1, "the raw name, plained once into the <title> and once (inside the plaining helper mirrorTerminalTitle) into the header — never twice (\"applied once\")"},
	"agents.go agentListAttrs a.DisplayName":           {1, whyFilter},
	"mirror.go mirrorListAttrs m.Label":                {2, whyFilter},
	"mirror.go mirrorListAttrs m.Title":                {2, whyFilter},
	"pinned_chips.go pinnedChipName m.Label":           {2, "a presence/equality test (is there a codename that is not the session name?); the label the chip draws goes through plainOr"},
	"agents.go agentFields a.DisplayName":              {2, whyFilter},
	"agents.go agentRenameForm a.DisplayName":          {1, whyForm},
	"agents.go comboOption o.Label":                    {2, whyStatic},
	"agents.go seededComboOption o.Label":              {2, whyStatic},
	"agents.go modelFieldForDisplay m.Label":           {1, whyStatic},
	"chief_sessions.go chiefNewSessionPicker rt.Label": {2, whyStatic},
	"components.go sidebarIn t.Label":                  {1, whyStatic},
	"nav_layout.go NavDocument v.Title":                {1, "a server-authored page's fixed name (\"Attention\", \"Account\"), chosen by internal/api, never author text"},
	"connectors.go ConnectorForm p.Label":              {1, whyStatic},
	"list_signals.go agentsFilterChips c.Label":        {1, whyStatic},
	"recents.go recentEntry it.Title":                  {2, "recentItem.Title is built through Title by recentItems"},
	"switcher.go switcherEntry it.Label":               {3, "SwitcherItem.Label is built through Title by switcherItems (pinnedChipLabels: pinnedChipName's plainOr), the pinned chips' own labels; plaining it again here would apply Title twice"},
	"mirror.go mirrorFields m.Label":                   {2, whyFilter},
	"mirror.go mirrorFields m.Title":                   {2, whyFilter},
	"mirror_ask.go askForm o.Label":                    {2, whyOption},
	"mirror_ask.go askReadOnly o.Label":                {1, whyOption},
	"mirror_ask.go askRetryForm q.Options[p].Label":    {1, whyOption},
	"notes.go noteDispatchButton TaskTitle":            {1, whyDispatch},
	"notes.go NotesEditModalBody n.Title":              {1, whyForm},
	"notes.go statusFilterRow taskstatus.Label":        {1, whyStatic},
	"notes.go TaskTitle n.Title":                       {1, whyAccessor},
	"tabs.go TabsSelected t.Label":                     {1, whyStatic},
	"tags.go tasksFilteredEmpty taskstatus.Label":      {1, whyStatic},
	"terminal.go quickKeyButton k.Label":               {1, whyStatic},
	"focus.go focusHaystack f.Title":                   {1, whyFilter},
}

// apiExempt: the same, for api's hand-written HTML.
var apiExempt = map[string]exemption{
	"admin.go Server.renderAdminUsers u.DisplayName":                 {3, whyPerson},
	"passkeys.go Server.handleAdminUserPasskeysDelete u.DisplayName": {1, whyPerson},
	"plugins.go Server.renderPluginsPage p.Label":                    {1, "a plugin's configured label (its name), not a title"},
	"push_routes.go Server.pushPreferencesForm e.Label":              {1, whyStatic},
}

// pushEmitters are the only functions that may hand a notification to a push
// service or the router, and why each is safe.
var pushEmitters = map[string]string{
	"push_routes.go Server.sendPush":        "the wrapper goNotify and handlePushTest call",
	"server.go Server.goNotify":             "applies Title to the notification's title (bodies: see checkPushEmitters)",
	"push_routes.go Server.handlePushTest":  "a fixed test title and body",
	"devices.go Server.handleDevicePushPut": "a fixed probe payload (devicePushProbe)",
}

func TestTheTitleSinkLedger(t *testing.T) {
	t.Run("ui", func(t *testing.T) {
		checkReads(t, "../ui", titleNames, nil, uiExempt)
	})
	t.Run("api", func(t *testing.T) {
		names := map[string]bool{}
		for k := range titleNames {
			names[k] = true
		}
		for k := range apiTitleNames {
			names[k] = true
		}
		_, apiFiles := parseDir(t, "../api")
		bodies := pushBodyBuilders(apiFiles)
		// 🔴 POSITIVE CONTROL: the derivation must find the body builders it
		// exists for, or a clean verdict over them means nothing.
		for _, must := range []string{"attentionPushBody", "taskTitle", "draftPushBody"} {
			if !bodies[must] {
				t.Fatalf("positive control FAILED: %s was not derived as a push-body builder (found %v)", must, bodies)
			}
		}
		checkReads(t, "../api", names, func(fn *ast.FuncDecl) bool {
			return callsSelector(fn.Body, "html", "EscapeString") || bodies[fn.Name.Name]
		}, apiExempt)
	})
	t.Run("plaining helpers", checkPlainingHelpers)
	t.Run("applied once", func(t *testing.T) {
		for _, dir := range []string{"../ui", "../api"} {
			fset, files := parseDir(t, dir)
			for name, f := range files {
				for _, d := range doubleApplications(fset, name, f) {
					t.Errorf("DOUBLE title rule: %s — Title is not idempotent (an escaped marker becomes markup the second pass strips), so a value must be plained exactly once on its way to a sink", d)
				}
			}
		}
	})
	t.Run("push emitters", checkPushEmitters)
}

type read struct {
	key     string // "file func expr"
	covered bool
}

func parseDir(t *testing.T, dir string) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	out := map[string]*ast.File{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		out[filepath.Base(f)] = af
	}
	return fset, out
}

// pushBodyBuilders derives, from the api package, the functions whose result
// becomes a notification BODY: every function called in the Body: field of a
// RouterNotification literal, or in the right-hand side of an assignment to the
// variable named there. goNotify plains a notification's title only, so a
// title read inside one of these is a push sink of its own.
//
// ⚠ Syntactic: a body carried through a struct field (commentNotificationText's
// text rides the comment buffer into flushCommentedTasks) is not derived; that
// one is held by TestATaskPushBodyKeepsSnakeCase and its siblings instead.
func pushBodyBuilders(files map[string]*ast.File) map[string]bool {
	out := map[string]bool{}
	callsIn := func(e ast.Node) {
		ast.Inspect(e, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				switch f := c.Fun.(type) {
				case *ast.Ident:
					out[f.Name] = true
				case *ast.SelectorExpr:
					out[f.Sel.Name] = true
				}
			}
			return true
		})
	}
	for _, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			bodyVars := map[string]bool{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				cl, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				if id, ok := cl.Type.(*ast.Ident); !ok || id.Name != "RouterNotification" {
					return true
				}
				for _, el := range cl.Elts {
					kv, ok := el.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Body" {
						callsIn(kv.Value)
						if v, ok := kv.Value.(*ast.Ident); ok {
							bodyVars[v.Name] = true
						}
					}
				}
				return true
			})
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				as, ok := n.(*ast.AssignStmt)
				if !ok || len(as.Lhs) != len(as.Rhs) {
					return true
				}
				for i, l := range as.Lhs {
					if id, ok := l.(*ast.Ident); ok && bodyVars[id.Name] {
						callsIn(as.Rhs[i])
					}
				}
				return true
			})
		}
	}
	return out
}

func checkReads(t *testing.T, dir string, names map[string]bool, include func(*ast.FuncDecl) bool, exempt map[string]exemption) {
	fset, files := parseDir(t, dir)
	// 🔴 POSITIVE CONTROL: a walk over nothing reports a clean ledger.
	if len(files) < 20 {
		t.Fatalf("positive control FAILED: only %d files parsed in %s — the walk is not measuring", len(files), dir)
	}
	var reads []read
	for name, f := range files {
		reads = append(reads, titleReads(fset, name, f, names, include)...)
	}
	covered, got := 0, map[string]int{}
	for _, r := range reads {
		if r.covered {
			covered++
			continue
		}
		got[r.key]++
	}
	if covered == 0 {
		t.Fatalf("positive control FAILED: no read in %s is covered by Title — either nothing calls it or the walk cannot see the call", dir)
	}
	var problems []string
	for k, n := range got {
		e, ok := exempt[k]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("UNLEDGERED title read %q (×%d): show it through textplain.Title, or ledger it with the reason it is not a display", k, n))
		case e.n != n:
			problems = append(problems, fmt.Sprintf("ledger row %q expects %d raw read(s), found %d", k, e.n, n))
		}
	}
	for k := range exempt {
		if got[k] == 0 {
			problems = append(problems, fmt.Sprintf("STALE ledger row %q: no such raw read any more — delete the row", k))
		}
	}
	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}
	t.Logf("%s: %d title reads, %d through Title, %d ledgered raw", dir, len(reads), covered, len(reads)-covered)
}

// titleReads returns every read of a title-bearing name in f's function
// bodies, and whether each is covered. htmlOnly restricts the walk to
// functions that hand-write HTML.
func titleReads(fset *token.FileSet, file string, f *ast.File, names map[string]bool, include func(*ast.FuncDecl) bool) []read {
	var out []read
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		if include != nil && !include(fn) {
			continue
		}
		where := file + " " + funcName(fn) + " "
		var stack []ast.Node
		lhs := map[ast.Node]bool{}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)
			if as, ok := n.(*ast.AssignStmt); ok {
				for _, l := range as.Lhs {
					lhs[l] = true
				}
			}
			var hit bool
			switch x := n.(type) {
			case *ast.SelectorExpr:
				// textplain.Title is the rule itself, not a title.
				if id, ok := x.X.(*ast.Ident); ok && id.Name == "textplain" {
					// 🔴 Inspect does not call f(nil) for a node whose f returned
					// false, so pop it here or the stack drifts.
					stack = stack[:len(stack)-1]
					return false
				}
				hit = names[x.Sel.Name] && !lhs[x]
			case *ast.Ident:
				// A bare call of a title accessor (TaskTitle(n) in package ui); a
				// selector's own Sel ident is handled by the case above. Only the
				// accessors: a bare Label(...) is the <label> element.
				if bareAccessors[x.Name] && len(stack) >= 2 {
					if c, ok := stack[len(stack)-2].(*ast.CallExpr); ok && c.Fun == x {
						hit = true
					}
				}
			}
			if !hit {
				return true
			}
			var b strings.Builder
			_ = printer.Fprint(&b, fset, n)
			out = append(out, read{key: where + b.String(), covered: coveredBy(stack)})
			stack = stack[:len(stack)-1] // see above: no f(nil) after a false
			return false
		})
	}
	return out
}

// coveredBy reports whether the innermost node of stack sits inside the
// argument a rule application plains (textplain.Title's, plainOr's first), or
// is passed straight to a plaining helper at a name position.
func coveredBy(stack []ast.Node) bool {
	for i := len(stack) - 2; i >= 0; i-- {
		c, ok := stack[i].(*ast.CallExpr)
		if !ok {
			continue
		}
		if a := appliedArg(c); a != nil && a == stack[i+1] {
			return true
		}
		// A plaining helper covers only an argument passed STRAIGHT to it, at
		// a name position: helper(other(m.Label)) draws whatever other returns.
		if id, ok := c.Fun.(*ast.Ident); ok && plainingHelpers[id.Name] && i == len(stack)-2 {
			for _, pos := range helperNameArgs[id.Name] {
				if pos < len(c.Args) && c.Args[pos] == stack[i+1] {
					return true
				}
			}
		}
	}
	return false
}

// isTitleCall reports whether c applies the title rule: textplain.Title, or
// package ui's plainOr (Title with a raw fallback).
func isTitleCall(c *ast.CallExpr) bool { return appliedArg(c) != nil }

// appliedArg is the argument c runs the title rule over, or nil.
func appliedArg(c *ast.CallExpr) ast.Expr {
	if len(c.Args) == 0 {
		return nil
	}
	switch f := c.Fun.(type) {
	case *ast.SelectorExpr:
		if id, ok := f.X.(*ast.Ident); ok && id.Name == "textplain" && f.Sel.Name == "Title" {
			return c.Args[0]
		}
	case *ast.Ident:
		if f.Name == "plainOr" {
			return c.Args[0]
		}
	}
	return nil
}

func callsSelector(body ast.Node, pkg, sel string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if s, ok := n.(*ast.SelectorExpr); ok {
			if id, ok := s.X.(*ast.Ident); ok && id.Name == pkg && s.Sel.Name == sel {
				found = true
			}
		}
		return !found
	})
	return found
}

func funcName(fn *ast.FuncDecl) string {
	if fn.Recv != nil && len(fn.Recv.List) > 0 {
		t := fn.Recv.List[0].Type
		if s, ok := t.(*ast.StarExpr); ok {
			t = s.X
		}
		if id, ok := t.(*ast.Ident); ok {
			return id.Name + "." + fn.Name.Name
		}
	}
	return fn.Name.Name
}

// checkPlainingHelpers: each helper exists and calls Title or another helper,
// so "passed to a helper" cannot mean "passed to something that draws it raw".
// doubleApplications finds, in each function, a value the title rule reaches
// twice: Title(...Title(x)...), Title(p) or helper(p) where p was assigned from
// Title in the same function, and helper(...Title(x)...) — a plaining helper
// applies Title itself. Syntactic and per-function: it catches the shape that
// shipped (terminal.go plained a name, then handed it to mirrorEntryTitle),
// not a value carried through a struct field into another function.
func doubleApplications(fset *token.FileSet, file string, f *ast.File) []string {
	var out []string
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		plained := map[string]bool{}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if as, ok := n.(*ast.AssignStmt); ok && len(as.Lhs) == len(as.Rhs) {
				for i, r := range as.Rhs {
					if c, ok := r.(*ast.CallExpr); ok && isTitleCall(c) {
						if id, ok := as.Lhs[i].(*ast.Ident); ok {
							plained[id.Name] = true
						}
					}
				}
			}
			return true
		})
		carriesPlain := func(e ast.Expr) bool {
			found := false
			ast.Inspect(e, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					if isTitleCall(x) {
						found = true
					}
				case *ast.Ident:
					if plained[x.Name] {
						found = true
					}
				}
				return !found
			})
			return found
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			helper := false
			if id, ok := c.Fun.(*ast.Ident); ok && plainingHelpers[id.Name] {
				helper = true
			}
			if !isTitleCall(c) && !helper {
				return true
			}
			var plainedArgs []ast.Expr
			if a := appliedArg(c); a != nil {
				plainedArgs = append(plainedArgs, a)
			}
			if helper {
				for _, pos := range helperNameArgs[c.Fun.(*ast.Ident).Name] {
					if pos < len(c.Args) {
						plainedArgs = append(plainedArgs, c.Args[pos])
					}
				}
			}
			for _, a := range plainedArgs {
				if carriesPlain(a) {
					out = append(out, fmt.Sprintf("%s %s at %s", file, funcName(fn), fset.Position(c.Pos())))
				}
			}
			return true
		})
	}
	return out
}

func checkPlainingHelpers(t *testing.T) {
	_, files := parseDir(t, "../ui")
	seen := map[string]bool{}
	for _, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			positions, ok := helperNameArgs[fn.Name.Name]
			if !ok {
				continue
			}
			seen[fn.Name.Name] = true
			for _, problem := range helperProblems(fn, positions) {
				t.Errorf("plaining helper %s: %s", fn.Name.Name, problem)
			}
		}
	}
	for h := range helperNameArgs {
		if !seen[h] {
			t.Errorf("plaining helper %s is not declared in internal/ui — delete it from helperNameArgs", h)
		}
	}
}

// helperProblems holds a helper to "applies the title rule to its name
// arguments": each name parameter is used inside a rule application's plained
// argument (or handed, at a name position, to another helper), and NEVER drawn
// raw — inside g.Text(...) or g.Attr(...) outside a rule application.
func helperProblems(fn *ast.FuncDecl, positions []int) []string {
	var params []string
	for _, fld := range fn.Type.Params.List {
		for _, n := range fld.Names {
			params = append(params, n.Name)
		}
	}
	var out []string
	for _, pos := range positions {
		if pos >= len(params) {
			out = append(out, fmt.Sprintf("has no parameter %d", pos))
			continue
		}
		name := params[pos]
		plained, raw := false, false
		var stack []ast.Node
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)
			id, ok := n.(*ast.Ident)
			if !ok || id.Name != name {
				return true
			}
			inRule, inDraw := false, false
			for i := len(stack) - 2; i >= 0; i-- {
				c, ok := stack[i].(*ast.CallExpr)
				if !ok {
					continue
				}
				if a := appliedArg(c); a != nil && a == stack[i+1] {
					inRule = true
					break
				}
				if h, ok := c.Fun.(*ast.Ident); ok && plainingHelpers[h.Name] && h.Name != fn.Name.Name {
					for _, p := range helperNameArgs[h.Name] {
						if p < len(c.Args) && c.Args[p] == stack[i+1] {
							inRule = true
						}
					}
					if inRule {
						break
					}
				}
				if s, ok := c.Fun.(*ast.SelectorExpr); ok {
					if x, ok := s.X.(*ast.Ident); ok && x.Name == "g" && (s.Sel.Name == "Text" || s.Sel.Name == "Attr") {
						inDraw = true
					}
				}
			}
			if inRule {
				plained = true
			} else if inDraw {
				raw = true
			}
			return true
		})
		if !plained {
			out = append(out, fmt.Sprintf("never applies the title rule to its name argument %q", name))
		}
		if raw {
			out = append(out, fmt.Sprintf("draws its name argument %q raw (g.Text/g.Attr outside textplain.Title/plainOr)", name))
		}
	}
	return out
}

// checkPushEmitters: the functions that call a push service or the router are
// exactly pushEmitters, and goNotify applies Title.
func checkPushEmitters(t *testing.T) {
	_, files := parseDir(t, "../api")
	got := map[string]bool{}
	for name, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				c, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				s, ok := c.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				var via string
				switch s.Sel.Name {
				case "sendPush", "SendTo", "Probe":
					// s.sendPush, s.ext.Push.SendTo, s.ext.Push.Probe
					if s.Sel.Name == "sendPush" || selectorEndsIn(s.X, "Push") {
						via = s.Sel.Name
					}
				case "Notify":
					if selectorEndsIn(s.X, "router") {
						via = "router.Notify"
					}
				}
				if via != "" {
					got[name+" "+funcName(fn)] = true
				}
				return true
			})
			if funcName(fn) == "Server.goNotify" {
				// The title is plained here; the body is NOT (it is plained
				// upstream where it is a title-like line, and verbatim where it
				// is not) — applying the rule to it again is a double.
				fields := map[string]int{}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					if c, ok := n.(*ast.CallExpr); ok && isTitleCall(c) && len(c.Args) == 1 {
						if s, ok := c.Args[0].(*ast.SelectorExpr); ok {
							fields[s.Sel.Name]++
						}
					}
					return true
				})
				if fields["Title"] != 1 {
					t.Errorf("goNotify applies textplain.Title to the notification's Title %d time(s), want 1", fields["Title"])
				}
				if fields["Body"] != 0 {
					t.Errorf("goNotify applies textplain.Title to the notification's Body %d time(s), want 0 (bodies are plained upstream, once, or are not titles)", fields["Body"])
				}
			}
		}
	}
	if len(got) == 0 {
		t.Fatal("positive control FAILED: no push emitter found at all — the walk is matching nothing")
	}
	var problems []string
	for k := range got {
		if _, ok := pushEmitters[k]; !ok {
			problems = append(problems, fmt.Sprintf("UNLEDGERED push emitter %q: route it through goNotify (which applies Title) or ledger why its text is safe", k))
		}
	}
	for k := range pushEmitters {
		if !got[k] {
			problems = append(problems, fmt.Sprintf("STALE push emitter row %q", k))
		}
	}
	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}
}

func selectorEndsIn(x ast.Expr, name string) bool {
	switch v := x.(type) {
	case *ast.SelectorExpr:
		return v.Sel.Name == name
	case *ast.Ident:
		return v.Name == name
	}
	return false
}

// TestTheLedgerWalkSeesAnUnplainedRead is the negative control on the walk
// itself: a raw title drawn into a node is reported uncovered, the same read
// inside Title is covered, and a read passed to a plaining helper is covered.
// TestTheLedgerSeesADoubleApplication is the negative control for "applied
// once": each double shape is reported, and the single ones are not.
func TestTheLedgerSeesADoubleApplication(t *testing.T) {
	src := `package ui
func nested(n Note) string     { return textplain.Title(textplain.Title(n.Title)) }
func viaVar(v View) Node        { t := textplain.Title(v.Name); return mirrorEntryTitle(t, "", "", "", "") }
func viaVarTwice(v View) string { t := textplain.Title(v.Name); return textplain.Title(t) }
func intoHelper(v View) Node    { return mirrorEntryTitle(textplain.Title(v.Name), "", "", "", "") }
func once(v View) Node          { t := textplain.Title(v.Name); _ = t; return mirrorEntryTitle(v.Name, "", "", "", "") }
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, d := range doubleApplications(fset, "x.go", f) {
		got[strings.Fields(d)[1]]++
	}
	want := map[string]int{"nested": 1, "viaVar": 1, "viaVarTwice": 1, "intoHelper": 1}
	if len(got) != len(want) {
		t.Errorf("reported %v, want %v (once must not be reported)", got, want)
	}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("%s: reported %d time(s), want %d", k, got[k], n)
		}
	}
}

// TestTheHelperCheckSeesARawName is the negative control for the plaining
// helpers check: a helper that draws a name parameter raw, or never plains it,
// is reported; one that plains it (directly or through plainOr) is not, and a
// non-name parameter drawn raw (the session) is allowed.
func TestTheHelperCheckSeesARawName(t *testing.T) {
	src := `package ui
func good(label, title, session string) Node { return Span(g.Text(plainOr(title, session)), g.Text(textplain.Title(label)), g.Text(session)) }
func rawDraw(label, title, session string) Node { return Span(g.Text(textplain.Title(title)), g.Text(label)) }
func never(label, title, session string) Node { if label == session { return nil }; return Span(g.Text(textplain.Title(title))) }
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, d := range f.Decls {
		fn := d.(*ast.FuncDecl)
		got[fn.Name.Name] = helperProblems(fn, []int{0, 1})
	}
	if len(got["good"]) != 0 {
		t.Errorf("good: reported %v, want nothing", got["good"])
	}
	if p := strings.Join(got["rawDraw"], "; "); !strings.Contains(p, `draws its name argument "label" raw`) {
		t.Errorf("rawDraw: reported %q, want the raw draw of label", p)
	}
	if p := strings.Join(got["never"], "; "); !strings.Contains(p, `never applies the title rule to its name argument "label"`) {
		t.Errorf("never: reported %q, want label never plained", p)
	}
}

func TestTheLedgerWalkSeesAnUnplainedRead(t *testing.T) {
	src := `package ui
func raw(n Note) Node     { return Text(n.Title) }
func plain(n Note) Node   { return Text(textplain.Title(n.Title)) }
func helped(m View) Node  { return mirrorEntryTitle(m.Label, "", "", "", "") }
func helperArg(m View) Node { return mirrorEntryTitle(other(m.Label), "", "", "", "") }
func write(v *View)       { v.Title = "x" }
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range titleReads(fset, "x.go", f, titleNames, nil) {
		got[r.key] = r.covered
	}
	want := map[string]bool{
		"x.go raw n.Title":       false,
		"x.go plain n.Title":     true,
		"x.go helped m.Label":    true,
		"x.go helperArg m.Label": false, // not passed STRAIGHT to the helper
	}
	if len(got) != len(want) {
		t.Errorf("walk found %v, want %v (the assignment's LHS must not count as a read)", got, want)
	}
	for k, v := range want {
		if c, ok := got[k]; !ok || c != v {
			t.Errorf("%s: covered=%v (found=%v), want covered=%v", k, c, ok, v)
		}
	}
}
