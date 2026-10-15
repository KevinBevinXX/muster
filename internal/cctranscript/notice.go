package cctranscript

import (
	"regexp"
	"strings"
)

// A NOTICE is a user record the HARNESS wrote, not the human: a background
// task finishing, a slash command, a local command's output. Claude Code writes
// each as a plain main-conversation user record (no isMeta), so without this
// it reads exactly like a prompt and is drawn as the human's own bubble.
//
// 🔴 CHOSEN FROM THE RECORD'S STRUCTURE, NEVER FROM A WORD IN IT. A record is a
// notice when EITHER
//
//   - its origin is a task notification ("origin":{"kind":"task-notification"},
//     CLI 2.1.22+ — including the plain-text "Background agent … was stopped by
//     the user." records, which carry no tag at all), OR
//   - its text is made ENTIRELY of <tag>…</tag> blocks (whitespace between,
//     nothing else) and the FIRST tag is one of noticeTags.
//
// So a human prompt that MENTIONS <task-notification> mid-sentence, or pastes
// one and then writes beneath it, stays a prompt: it has text outside a block.
// Measured over 832 real transcripts (CLI 2.1.22–2.1.296): every non-meta user
// record that starts with one of these tags is wholly tag blocks — not one had
// text after its last block.
//
// The tags, from those transcripts (counts of leading occurrences):
//
//	task-notification     11025  a background task / agent / monitor reported
//	command-message         526  a slash command (with command-name, -args)
//	command-name            155  the same, in the other order
//	local-command-stdout    151  what a local command (/model, /cost) printed
//
// <system-reminder> and <local-command-caveat> lead only isMeta records, which
// are dropped before this is asked. local-command-stderr is listed beside its
// stdout sibling.

// The notice sources: Event.Notice.
const (
	NoticeTask          = "task"
	NoticeCommand       = "command"
	NoticeCommandOutput = "command-output"
)

// noticeTags maps a leading tag to its source.
var noticeTags = map[string]string{
	"task-notification":    NoticeTask,
	"command-name":         NoticeCommand,
	"command-message":      NoticeCommand,
	"local-command-stdout": NoticeCommandOutput,
	"local-command-stderr": NoticeCommandOutput,
}

// tagBlock is one <tag>…</tag> block at the start of a string. Go's RE2 has no
// backreferences, so the closing tag is checked by the caller.
var tagBlock = regexp.MustCompile(`^\s*<([a-z][a-z0-9-]*)>`)

// tagBlocks splits s into its top-level <tag>body</tag> blocks, in order, and
// reports false when anything but whitespace lies outside them.
func tagBlocks(s string) ([][2]string, bool) {
	var out [][2]string
	for strings.TrimSpace(s) != "" {
		m := tagBlock.FindStringSubmatchIndex(s)
		if m == nil {
			return nil, false
		}
		name := s[m[2]:m[3]]
		rest := s[m[1]:]
		end := strings.Index(rest, "</"+name+">")
		if end < 0 {
			return nil, false
		}
		out = append(out, [2]string{name, rest[:end]})
		s = rest[end+len("</"+name+">"):]
	}
	return out, len(out) > 0
}

// innerTag is the body of <name>…</name> inside s, trimmed ("" when absent).
func innerTag(s, name string) string {
	i := strings.Index(s, "<"+name+">")
	if i < 0 {
		return ""
	}
	rest := s[i+len(name)+2:]
	j := strings.Index(rest, "</"+name+">")
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:j])
}

// ansi is a terminal escape sequence (a local command's stdout carries them).
var ansi = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]")

// firstLine is s's first non-blank line.
func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

// noticeOf is r's notice event, and whether r is a notice at all (see the
// comment at the top of this file). text is r's PromptText.
func noticeOf(r Record, text string) (Event, bool) {
	blocks, whole := tagBlocks(text)
	src := ""
	if whole {
		src = noticeTags[blocks[0][0]]
	}
	if src == "" && r.Origin.Kind == "task-notification" {
		// A tagless task notification ("Background agent … was stopped").
		return Event{Kind: KindNotice, Notice: NoticeTask, Text: Cap(firstLine(text), MaxText), Output: Cap(text, MaxOutput)}, true
	}
	switch src {
	case NoticeTask:
		body := blocks[0][1]
		detail := innerTag(body, "result")
		if detail == "" {
			detail = innerTag(body, "event")
		}
		summary := innerTag(body, "summary")
		if summary == "" {
			summary = firstLine(detail)
		}
		return Event{Kind: KindNotice, Notice: NoticeTask, Status: innerTag(body, "status"),
			Text: Cap(summary, MaxText), Output: Cap(detail, MaxOutput)}, true
	case NoticeCommand:
		var name, args string
		for _, b := range blocks {
			switch b[0] {
			case "command-name":
				name = strings.TrimSpace(b[1])
			case "command-args":
				args = strings.TrimSpace(b[1])
			}
		}
		if name == "" {
			for _, b := range blocks {
				if b[0] == "command-message" {
					name = "/" + strings.TrimSpace(b[1])
				}
			}
		}
		return Event{Kind: KindNotice, Notice: NoticeCommand, Text: Cap(strings.TrimSpace(name+" "+args), MaxText)}, true
	case NoticeCommandOutput:
		out := strings.TrimSpace(ansi.ReplaceAllString(blocks[0][1], ""))
		status := ""
		if blocks[0][0] == "local-command-stderr" {
			status = "error"
		}
		return Event{Kind: KindNotice, Notice: NoticeCommandOutput, Status: status, Text: Cap(firstLine(out), MaxText), Output: Cap(out, MaxOutput)}, true
	}
	return Event{}, false
}
