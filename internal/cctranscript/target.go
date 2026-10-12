package cctranscript

import (
	"encoding/json"
	"sort"
	"strings"
)

// MaxTarget bounds a tool row's one-line summary, in runes.
const MaxTarget = 80

// targetKeys is, per tool, the input field that names what the call acts on.
// A tool not listed falls back to the first of fallbackKeys it carries, then to
// its first string field by name — so a new tool still shows SOMETHING useful
// rather than nothing, and muster's own tools (note_create{title}, …) work too.
var targetKeys = map[string][]string{
	"Bash":         {"command"},
	"Read":         {"file_path"},
	"Write":        {"file_path"},
	"Edit":         {"file_path"},
	"MultiEdit":    {"file_path"},
	"NotebookEdit": {"notebook_path"},
	"Grep":         {"pattern"},
	"Glob":         {"pattern"},
	"WebFetch":     {"url"},
	"WebSearch":    {"query"},
	"Task":         {"description"},
	"Agent":        {"description"},
	"Skill":        {"skill"},
}

var fallbackKeys = []string{"command", "file_path", "path", "pattern", "url", "query", "description", "name", "title", "skill"}

// ToolTarget is a tool call's one-line summary — the file a Read reads, the
// command a Bash runs, the pattern a Grep searches — from its input JSON. It is
// "" when the input carries no string worth showing (or is not JSON at all).
//
// ONE function for every surface that draws a tool row: the mirrored rendered
// view (computed on the host, where the full input is), and the agent chat's
// persisted and live rows (computed from the stored args).
func ToolTarget(tool, input string) string {
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(strings.TrimSpace(input)), &m) != nil {
		return ""
	}
	str := func(k string) string {
		raw, ok := m[k]
		if !ok {
			return ""
		}
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return ""
		}
		return s
	}
	keys := append(append([]string(nil), targetKeys[tool]...), fallbackKeys...)
	for _, k := range keys {
		if s := oneLine(str(k)); s != "" {
			return s
		}
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if s := oneLine(str(k)); s != "" {
			return s
		}
	}
	return ""
}

// oneLine is s's first non-blank line, whitespace-collapsed, capped at
// MaxTarget runes (a cut is marked "…"). A multi-line command shows its first
// line, which is the one a reader recognises.
func oneLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.Join(strings.Fields(line), " ")
		if line == "" {
			continue
		}
		if r := []rune(line); len(r) > MaxTarget {
			line = string(r[:MaxTarget-1]) + "…"
		}
		return line
	}
	return ""
}
