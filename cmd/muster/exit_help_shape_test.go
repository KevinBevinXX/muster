package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 🔴 WHAT THESE TWO GUARDS CLAIM, AND WHAT THEY DO NOT.
//
// The FIRST is a real two-way relationship: every exit code this binary can
// return is documented, and every documented code exists. It is structural — it
// reads the constants out of exit.go's AST — and it is the guard worth having.
//
// The SECOND is narrower than the defect it descends from, and says so. The
// upstream version of this help text SHIPPED TRUNCATED: one entry ended on the
// word "which" and the next entry began, so `--help` printed half a sentence.
// Nothing saw it, because the only assertion was that four substrings were
// present — and a substring assertion cannot see what comes AFTER the substring.
// This catches an entry that ends on a DANGLING FUNCTION WORD, which is the
// shape that truncation had. A truncation ending on a noun walks straight past
// it. It is here because it is cheap and it fires on the exact mutant, NOT
// because it proves the help is complete. Nothing mechanical can prove that.
//
// ⚠ A TERMINAL-PUNCTUATION RULE WAS CONSIDERED AND IS WRONG FOR THIS TEXT.
// Several entries deliberately end without a full stop, so that guard would be
// red on arrival — a permanently-red gate, which is worse than no gate.
// ---------------------------------------------------------------------------

// helpEntryHead matches the start of one exit-code entry: leading spaces, the
// code, two spaces, then the text. Continuation lines are indented further and
// deliberately do not match.
var helpEntryHead = regexp.MustCompile(`^ *(\d+)  (.*)$`)

// helpEntries splits exitCodeHelp into {code: joined text}, in the order the
// help prints them.
func helpEntries(t *testing.T) (map[int]string, []int) {
	t.Helper()
	body, ok := strings.CutPrefix(exitCodeHelp, "Exit codes:\n")
	if !ok {
		t.Fatalf("exitCodeHelp no longer starts with the `Exit codes:` header; this parser is "+
			"reading something else:\n%s", exitCodeHelp)
	}
	// The trailer after the blank line is output-discipline prose, not an entry.
	body, _, _ = strings.Cut(body, "\n\nOutput discipline")

	entries := map[int]string{}
	var order []int
	cur := -1
	for _, line := range strings.Split(body, "\n") {
		if m := helpEntryHead.FindStringSubmatch(line); m != nil {
			code, err := strconv.Atoi(m[1])
			if err != nil {
				t.Fatalf("unparseable exit code %q in exitCodeHelp", m[1])
			}
			if _, dup := entries[code]; dup {
				t.Errorf("exitCodeHelp documents code %d twice", code)
			}
			cur = code
			order = append(order, code)
			entries[code] = m[2]
			continue
		}
		if cur >= 0 {
			entries[cur] += " " + strings.TrimSpace(line)
		}
	}
	return entries, order
}

// declaredExitCodes reads the `exit… = N` constants out of exit.go itself, so a
// new code cannot be added without this ledger noticing.
func declaredExitCodes(t *testing.T) map[string]int {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "exit.go", nil, 0)
	if err != nil {
		t.Fatalf("parse exit.go: %v", err)
	}
	out := map[string]int{}
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
				continue
			}
			name := vs.Names[0].Name
			if !strings.HasPrefix(name, "exit") {
				continue
			}
			lit, ok := vs.Values[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.INT {
				continue
			}
			n, err := strconv.Atoi(lit.Value)
			if err != nil {
				continue
			}
			out[name] = n
		}
	}
	return out
}

// TestEveryExitCodeIsDocumentedAndEveryDocumentedCodeExists pins the ledger
// both ways.
func TestEveryExitCodeIsDocumentedAndEveryDocumentedCodeExists(t *testing.T) {
	declared := declaredExitCodes(t)
	// POSITIVE CONTROL: a parser that read nothing would make both loops below
	// vacuous and this test would pass green against an empty help string.
	if len(declared) < 8 {
		t.Fatalf("read only %d exit* constants out of exit.go (%v); this ledger is not reading "+
			"the source", len(declared), declared)
	}
	entries, order := helpEntries(t)
	if len(entries) < 8 {
		t.Fatalf("parsed only %d entries out of exitCodeHelp; the parser is not reading the "+
			"help text:\n%s", len(entries), exitCodeHelp)
	}

	byCode := map[int]string{}
	for name, code := range declared {
		byCode[code] = name
		if _, ok := entries[code]; !ok {
			t.Errorf("🔴 %s = %d is a code this binary can RETURN and `--help` does not document "+
				"it. A script branching on exit codes reads that help; an undocumented code is "+
				"one it will treat as an unknown failure.", name, code)
		}
	}
	for _, code := range order {
		if _, ok := byCode[code]; !ok {
			t.Errorf("🔴 `--help` documents exit code %d and no exit* constant has that value. "+
				"Either the code was renumbered and the help was not, or the help promises a code "+
				"no path produces.", code)
		}
	}
}

// danglingTail is the closed set of words an entry must not END on. Every one
// is a word that REQUIRES something after it, so its presence at the end of an
// entry is a sentence that stopped rather than finished.
//
// ⚠ `this` is deliberately ABSENT: an entry ends "A DEADLINE is 10, not this",
// where it is a complete object rather than a dangling determiner.
var danglingTail = map[string]bool{
	"which": true, "that": true, "the": true, "a": true, "an": true,
	"and": true, "or": true, "of": true, "to": true, "for": true,
	"from": true, "in": true, "on": true, "with": true, "by": true,
	"is": true, "are": true, "names": true, "because": true, "when": true,
}

var tailWord = regexp.MustCompile(`([A-Za-z]+)[^A-Za-z]*$`)

// TestNoExitCodeHelpEntryStopsMidSentence is the guard for the shipped defect.
func TestNoExitCodeHelpEntryStopsMidSentence(t *testing.T) {
	entries, order := helpEntries(t)
	if len(order) < 8 {
		t.Fatalf("parsed only %d entries; this guard is inspecting nothing", len(order))
	}
	for _, code := range order {
		text := strings.TrimSpace(entries[code])
		m := tailWord.FindStringSubmatch(text)
		if m == nil {
			t.Errorf("🔴 exit-code entry %d ends with no word at all: %q", code, text)
			continue
		}
		if danglingTail[strings.ToLower(m[1])] {
			t.Errorf("🔴 THE EXIT-%d ENTRY IN `--help` STOPS MID-SENTENCE, on %q.\n"+
				"It is rendered verbatim to whoever runs --help, and the next entry starts "+
				"immediately after it, so the reader gets half a sentence.\n"+
				"Full entry: %q", code, m[1], text)
		}
	}
}

// TestTheHelpNeverSpellsAFixedProgramName is the alias guard's documentation
// half.
//
// 🔴 A HELP TEXT THAT SPELLS ONE OF THE BINARY'S NAMES IS WRONG UNDER THE OTHER,
// and a caller reading it is told to type a command that may not exist on their
// machine. The usage line cobra renders is derived from argv[0] — that is
// covered in progname_test.go; this covers the PROSE, which is a literal and can
// therefore be wrong.
func TestTheHelpNeverSpellsAFixedProgramName(t *testing.T) {
	// Positive control: prove we are inspecting a real, non-trivial help body.
	if len(rootLong) < 500 {
		t.Fatalf("rootLong is %d bytes; this guard is inspecting nothing", len(rootLong))
	}
	for name, body := range map[string]string{"rootLong": rootLong, "exitCodeHelp": exitCodeHelp} {
		// The default name may appear as a SERVICE name ("muster's JSON API",
		// "in front of muster") — that is the project, not the command. What must
		// not appear is the command-invocation shape: the name followed by a verb
		// this binary has.
		for _, verb := range []string{"task", "agent"} {
			bad := defaultProgName + " " + verb
			if strings.Contains(body, bad) {
				t.Errorf("🔴 %s spells the command invocation %q. The binary answers to whatever "+
					"name it is invoked as, so a literal here tells an aliased caller to type a "+
					"command that does not exist for them. Refer to the verb alone.", name, bad)
			}
		}
	}
}
