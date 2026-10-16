package cctranscript

import (
	"regexp"
	"strings"
)

// A NOTICE is a user record the HARNESS wrote, not the human: a background
// task finishing, or a local command's output. Claude Code writes each as a
// plain main-conversation user record (no isMeta), so without this it reads
// exactly like a prompt and is drawn as the human's own bubble.
//
// 🔴 CHOSEN FROM THE RECORD'S STRUCTURE, NEVER FROM A WORD IN IT. A record is a
// notice when EITHER
//
//   - its origin is a task notification ("origin":{"kind":"task-notification"},
//     CLI 2.1.22+ — including the plain-text "Background agent … was stopped by
//     the user." records, which carry no tag at all), OR
//   - its origin is NOT "human", its text is made ENTIRELY of <tag>…</tag>
//     blocks (whitespace between, nothing else) and the FIRST tag is one of
//     noticeTags.
//
// So a human prompt that MENTIONS <task-notification> mid-sentence, or pastes
// one and then writes beneath it, stays a prompt: it has text outside a block.
// 🔴 And a record whose origin says "human" is NEVER a notice by its tags, even
// when it is wholly a pasted <task-notification>: the CLI stamped it as typed
// by the human, and the tag rule exists only for records that carry no origin
// at all (CLI before 2.1.22). In the same survey (830 transcripts) no
// human-origin record leads with a notice tag, so this costs no real notice.
// Measured over 832 real transcripts (CLI 2.1.22–2.1.296): every non-meta user
// record that starts with one of these tags is wholly tag blocks — not one had
// text after its last block.
//
// The tags, from those transcripts (counts of leading occurrences):
//
//	task-notification     11025  a background task / agent / monitor reported
//	local-command-stdout    151  what a local command (/model, /cost) printed
//
// <system-reminder> and <local-command-caveat> lead only isMeta records, which
// are dropped before this is asked.
//
// ⚠ A SLASH COMMAND IS NOT A NOTICE. Its record is wholly <command-name>,
// <command-message> and <command-args> blocks (526 + 155 leading in the same
// survey), but the HUMAN typed it (origin "human"), so it is the human's bubble:
// slashCommand turns those blocks back into the "/name args" they typed.

// The notice sources: Event.Notice.
const (
	NoticeTask          = "task"
	NoticeCommandOutput = "command-output"
)

// noticeTags maps a leading tag to its source.
var noticeTags = map[string]string{
	"task-notification":    NoticeTask,
	"local-command-stdout": NoticeCommandOutput,
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

// childOpen is the next <tag> opening anywhere in a string.
var childOpen = regexp.MustCompile(`<([a-z][a-z0-9_-]*)>`)

// innerTag is the body of the first <name>…</name> that is a DIRECT child of s,
// trimmed ("" when absent). It walks s block by block and jumps over each
// block's whole body, so a <status> or <summary> written INSIDE another block
// (a task's <result> or <event> quoting one) is never read as the notice's
// own. Text between blocks is skipped, not an error.
func innerTag(s, name string) string {
	for {
		m := childOpen.FindStringSubmatchIndex(s)
		if m == nil {
			return ""
		}
		tag := s[m[2]:m[3]]
		rest := s[m[1]:]
		end := strings.Index(rest, "</"+tag+">")
		if end < 0 {
			// An unclosed tag has no body; look past its opening.
			s = rest
			continue
		}
		if tag == name {
			return strings.TrimSpace(rest[:end])
		}
		s = rest[end+len("</"+tag+">"):]
	}
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
	// 🔴 Never by its tags when the CLI says the human wrote it.
	if whole && r.Origin.Kind != "human" {
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
	case NoticeCommandOutput:
		out := strings.TrimSpace(ansi.ReplaceAllString(blocks[0][1], ""))
		return Event{Kind: KindNotice, Notice: NoticeCommandOutput, Text: Cap(firstLine(out), MaxText), Output: Cap(out, MaxOutput)}, true
	}
	return Event{}, false
}

// commandTags are the only blocks the CLI writes for a slash command.
var commandTags = map[string]bool{"command-name": true, "command-message": true, "command-args": true}

// slashCommand is the "/name args" a human typed, when text is the record the
// CLI writes for a slash command, and false for anything else — so a prompt
// that merely mentions or pastes the tags keeps its own text.
//
// 🔴 The record must be wholly tag blocks AND every block must be one of
// commandTags. The rewrite DROPS what it does not print, so a record with any
// other block (`<command-name>/model</command-name>` and then a `<note>`) is
// shown raw: a rewrite would hide that block's text from the reader.
func slashCommand(text string) (string, bool) {
	blocks, whole := tagBlocks(text)
	if !whole {
		return "", false
	}
	var name, msg, args string
	for _, b := range blocks {
		if !commandTags[b[0]] {
			return "", false
		}
		switch b[0] {
		case "command-name":
			name = strings.TrimSpace(b[1])
		case "command-message":
			msg = strings.TrimSpace(b[1])
		case "command-args":
			args = strings.TrimSpace(b[1])
		}
	}
	if name == "" && msg != "" {
		name = "/" + msg
	}
	if name == "" {
		return "", false
	}
	return strings.TrimSpace(name + " " + args), true
}
