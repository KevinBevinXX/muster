package ui

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestNoInlineScriptCallsAnUndefinedFunction is the regression guard for a defect
// class the Go suite was structurally unable to see: a CALL to a function that
// does not exist.
//
// 🔴 WHY IT IS A CLASS AND NOT A TYPO. muster's UI was carved out of a larger
// app, and the carve deleted whole surfaces (the permission-request queue, its
// popover, its auto-approve popover) while leaving CALLS to their helpers behind
// in scripts that survived. Go cannot type-check a string, and a browser reports
// an undefined call as a ReferenceError at RUN time — which unwinds the rest of
// the enclosing handler and everything after it, silently.
//
// Three such calls were live when this test was written, and their damage was not
// cosmetic:
//
//	popOpen()      in appScript's Escape handler, BEFORE the cgModalDismiss line.
//	aaPopOpen()    same handler, same line.
//	  → Escape threw on every keypress, so neither bottom sheet ever closed.
//	    From the outside that is indistinguishable from having no handler at all,
//	    and that is exactly how it was reported.
//	renderQueued() the LAST statement of initGlobal(), which init() calls BEFORE
//	  → initPage(). So on a cold load initPage never ran: the sidebar buttons and
//	    the SPA tab wiring were unbound until a later htmx:load re-entered init()
//	    with __cgInit already set.
//
// WHAT IT ASSERTS is a relationship — every called name is defined in the same
// script, or is a browser/language global named in jsKnownGlobals — rather than
// the absence of three particular words. A fourth carve residue is caught the day
// it lands; a rename of any of the three does not walk past it.
//
// ⚠ IT IS A LEXICAL SCAN, NOT A PARSER, and that bounds it in one direction
// only: it can MISS a call (e.g. one reached through a computed property), but a
// name it reports is genuinely called and genuinely not defined in the script. A
// false positive is therefore a signal to add a real global to jsKnownGlobals,
// never to loosen the scan.
// ⚠ IT SWEEPS BOTH ui.Features SHAPES, AND THAT IS NOT BOILERPLATE. Features
// decides whether the Repos tab, its panel and its entries in the browser's tab
// registry are drawn at all, and appScript's own TABS/HEADINGS/PANEL_SELECTOR are
// generated FROM it (navRegistryJS). Scanning only the zero value — the shipping
// default, and the one that draws LESS — would make a dead call reachable only on
// a GitHub-enabled deployment structurally invisible to this guard, which is the
// same "the config pinned a dimension" blindness the defect class above is made
// of. See forEachFeatureShape.
func TestNoInlineScriptCallsAnUndefinedFunction(t *testing.T) {
	total, scanned := 0, 0
	forEachFeatureShape(t, func(t *testing.T, shape string, feat Features) {
		// Every document this package serves, so a script is covered wherever it is
		// mounted rather than only in the one view a test remembered.
		docs := shellDocuments(t, feat)
		names := make([]string, 0, len(docs))
		for name := range docs {
			names = append(names, name)
		}
		sort.Strings(names)

		for _, name := range names {
			scanned++
			for i, body := range inlineScriptBodies(t, docs[name]) {
				src := mustStripJSComments(t, body)
				for _, call := range undefinedCalls(src, jsDefinedNames(src)) {
					total++
					t.Errorf("%s, %s, inline script #%d: calls %s(), which nothing in that script "+
						"defines — in a browser this is a ReferenceError that unwinds the rest of the "+
						"handler. Either the function was deleted with the surface it belonged to "+
						"(delete the call), or it is a real browser global (add it to jsKnownGlobals).",
						shape, name, i, call)
				}
			}
		}
	})
	t.Logf("scanned %d rendered documents, %d undefined call sites", scanned, total)
}

// TestUndefinedCallScannerIsAWorkingInstrument is the pair of controls for the
// scan above. A scanner that cannot go red reports zero findings exactly the way
// a clean tree does, and a scanner that flags everything is no better.
func TestUndefinedCallScannerIsAWorkingInstrument(t *testing.T) {
	// NEGATIVE control: it must report the exact shape the three live defects had
	// — a bare call to a name declared nowhere.
	bad := `(function(){ function keep(){} keep(); popOpen(false); })();`
	defined := jsDefinedNames(bad)
	got := undefinedCalls(bad, defined)
	if len(got) != 1 || got[0] != "popOpen" {
		t.Fatalf("scanner found %v on a source whose only undefined call is popOpen() — it cannot "+
			"see the defect it exists for", got)
	}

	// POSITIVE control: the four ways this script legitimately defines a callable
	// must all read as DEFINED, or the scan drowns in false positives and gets
	// disabled.
	good := `(function(){
  function decl(a){ return a; }
  var expr = function (x) { return x; };
  var arrow = 0;
  window.exported = function () {};
  try { decl(1); expr(2); setTimeout(function(){}, 0); } catch (e) { String(e); }
  decl(arrow);
})();`
	if got := undefinedCalls(good, jsDefinedNames(good)); len(got) != 0 {
		t.Fatalf("scanner reported %v on a source with no undefined calls — a scan that flags "+
			"legitimate code gets turned off, which is the same as not having it", got)
	}
}

// undefinedCalls is the scan's body, factored out so the controls above exercise
// the SAME code path the real assertion does rather than a second copy of it.
func undefinedCalls(src string, defined map[string]bool) []string {
	var out []string
	for _, call := range jsCalledNames(jsBlankLiterals(src)) {
		if defined[call] || jsKnownGlobals[call] {
			continue
		}
		out = append(out, call)
	}
	return out
}

var (
	// jsFuncDecl matches `function NAME(`.
	jsFuncDecl = regexp.MustCompile(`\bfunction\s+([A-Za-z_$][\w$]*)\s*\(`)
	// jsVarDecl matches `var NAME =` / `var NAME,` / `var NAME;` — a var holding a
	// function expression is callable, and this scan cannot tell which do.
	jsVarDecl = regexp.MustCompile(`\bvar\s+([A-Za-z_$][\w$]*)`)
	// jsParams matches a parameter list, so a callback's own parameters count as
	// defined inside it. Deliberately whole-script rather than scope-aware: the
	// failure direction is a missed finding, never a false one.
	jsParams = regexp.MustCompile(`\bfunction\s*[A-Za-z_$\w]*\s*\(([^)]*)\)`)
	// jsCatchVar matches `catch (e)`.
	jsCatchVar = regexp.MustCompile(`\bcatch\s*\(\s*([A-Za-z_$][\w$]*)\s*\)`)
	// jsCall matches `NAME(` where NAME is not a property access (`.NAME(`) and
	// not part of a longer identifier.
	jsCall = regexp.MustCompile(`(^|[^.\w$])([A-Za-z_$][\w$]*)\s*\(`)
	// jsIdent splits a parameter list.
	jsIdent = regexp.MustCompile(`[A-Za-z_$][\w$]*`)
)

// jsBlankLiterals replaces the CONTENTS of every string, template and regex
// literal with spaces, leaving the delimiters and the byte length alone.
//
// 🔴 WITHOUT IT THE SCAN READS DATA AS CODE, and the false positives it produces
// are the kind that get a test deleted rather than fixed: `:not(.hidden)` inside
// a querySelectorAll string reads as a call to not(), and `'' + i` reads as
// a call to uE000(). Measured before this existed — 18 findings, 12 of them
// string contents. A scan whose noise exceeds its signal stops being read.
//
// Comments are assumed already stripped (mustStripJSComments), so the only
// states left are code and the three literal kinds.
func jsBlankLiterals(src string) string {
	const (
		code = iota
		inSingle
		inDouble
		inTick
		inRegex
		inRegexClass
	)
	out := []byte(src)
	m := code
	blank := func(i int) {
		if out[i] != '\n' {
			out[i] = ' '
		}
	}
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch m {
		case code:
			switch c {
			case '\'':
				m = inSingle
			case '"':
				m = inDouble
			case '`':
				m = inTick
			case '/':
				if regexCanStartAfter(src[:i]) {
					m = inRegex
				}
			}
		case inRegex, inRegexClass:
			if c == '\\' && i+1 < len(src) {
				blank(i)
				i++
				blank(i)
				continue
			}
			switch {
			case m == inRegex && c == '[':
				m = inRegexClass
			case m == inRegexClass && c == ']':
				m = inRegex
			case m == inRegex && c == '/':
				m = code
				continue
			}
			blank(i)
		default: // inside a string / template literal
			if c == '\\' && i+1 < len(src) {
				blank(i)
				i++
				blank(i)
				continue
			}
			if (m == inSingle && c == '\'') || (m == inDouble && c == '"') || (m == inTick && c == '`') {
				m = code
				continue
			}
			blank(i)
		}
	}
	return string(out)
}

func jsDefinedNames(src string) map[string]bool {
	out := map[string]bool{}
	for _, m := range jsFuncDecl.FindAllStringSubmatch(src, -1) {
		out[m[1]] = true
	}
	for _, m := range jsVarDecl.FindAllStringSubmatch(src, -1) {
		out[m[1]] = true
	}
	for _, m := range jsCatchVar.FindAllStringSubmatch(src, -1) {
		out[m[1]] = true
	}
	for _, m := range jsParams.FindAllStringSubmatch(src, -1) {
		for _, p := range jsIdent.FindAllString(m[1], -1) {
			out[p] = true
		}
	}
	return out
}

func jsCalledNames(src string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range jsCall.FindAllStringSubmatch(src, -1) {
		n := m[2]
		if seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// jsKnownGlobals is every name that may legitimately be followed by `(` without
// this script defining it: language keywords the lexical scan cannot tell from a
// call, and the browser globals the scripts actually use.
//
// 🔴 ADDING A NAME HERE IS A DECISION, NOT BOOKKEEPING. Each entry is a promise
// that the browser supplies it. A helper this app owns must never be added — that
// is precisely the finding the scan exists to make.
var jsKnownGlobals = map[string]bool{
	// Keywords a `(` can follow. The scan is lexical, so these look like calls.
	"if": true, "for": true, "while": true, "switch": true, "catch": true,
	"return": true, "typeof": true, "function": true, "else": true, "do": true,
	"in": true, "of": true, "new": true, "delete": true, "void": true,
	"throw": true, "case": true, "yield": true, "await": true, "try": true,
	"instanceof": true,
	// Language built-ins.
	"Array": true, "Boolean": true, "Date": true, "Error": true, "JSON": true,
	"Map": true, "Math": true, "Number": true, "Object": true, "Promise": true,
	"RegExp": true, "Set": true, "String": true, "Symbol": true, "WeakMap": true, "WeakSet": true,
	"parseInt": true, "parseFloat": true, "isNaN": true, "isFinite": true,
	"encodeURIComponent": true, "decodeURIComponent": true, "encodeURI": true,
	"decodeURI": true, "structuredClone": true, "queueMicrotask": true,
	// Browser globals.
	"setTimeout": true, "setInterval": true, "clearTimeout": true,
	"clearInterval": true, "requestAnimationFrame": true, "cancelAnimationFrame": true,
	"fetch": true, "alert": true, "confirm": true, "prompt": true,
	"atob": true, "btoa": true, "matchMedia": true, "getComputedStyle": true,
	"MutationObserver": true, "IntersectionObserver": true, "ResizeObserver": true,
	"CustomEvent": true, "Event": true, "KeyboardEvent": true, "FormData": true,
	"URL": true, "URLSearchParams": true, "EventSource": true, "WebSocket": true,
	"Notification": true, "AbortController": true, "Blob": true, "Image": true,
	"Intl": true, "TextEncoder": true, "TextDecoder": true,
	"MouseEvent": true, "DOMParser": true, "Uint8Array": true, "Uint16Array": true,
	"ArrayBuffer": true, "FileReader": true, "XMLHttpRequest": true, "Element": true,
}

// inlineScriptBodies returns the JavaScript body of every inline <script> in a
// rendered document (external ones — those carrying src= — have no body here).
func inlineScriptBodies(t *testing.T, doc string) []string {
	t.Helper()
	var out []string
	rest := doc
	for {
		open := strings.Index(rest, "<script")
		if open < 0 {
			return out
		}
		rest = rest[open:]
		gt := strings.Index(rest, ">")
		end := strings.Index(rest, "</script>")
		if gt < 0 || end < 0 || end < gt {
			return out
		}
		if body := strings.TrimSpace(rest[gt+1 : end]); body != "" {
			out = append(out, body)
		}
		rest = rest[end+len("</script>"):]
	}
}
