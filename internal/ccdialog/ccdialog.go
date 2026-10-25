// Package ccdialog reads a Claude Code pane's screen for a dialog that keys
// typed into the pane would ANSWER rather than reach the model: a permission
// prompt, the AskUserQuestion picker, any numbered select menu.
//
// It is the one copy of that reading. ccd checks it before each of its own
// keys (cmd/ccd/dialog.go), and muster-mirror checks it before it pastes a
// message into a mirrored Claude Code pane (internal/mirrorhost, the Claude
// prompt guard). It was ccd's (#163) and moved here unchanged when the mirror
// host needed the same check.
package ccdialog

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/askdrive"
)

// ⚠ A DENY-LIST, NOT AN ALLOW-LIST, AND READ FROM ONE CAPTURE. It recognises the
// dialogs below by their structure; a dialog drawn some other way is not
// recognised. It is a check before the keys, not a lock: a dialog that opens in
// the milliseconds between the capture and the keys is not caught.

// selectCursor is the CLI's select-menu cursor on a numbered option, as the
// permission prompt and the picker draw it: "❯ 1. Yes" (a space, or a no-break
// space, after the ❯). Leading spaces are the dialog's indent.
var selectCursor = regexp.MustCompile(`^\s*❯[ \x{00a0}]+\d+\.\s`)

// IsRule reports whether line is one of the CLI's horizontal rules (a run of
// U+2500 box-drawing characters, as above and below its input box).
func IsRule(line string) bool {
	t := strings.TrimSpace(line)
	return utf8.RuneCountInString(t) >= 10 && strings.Trim(t, "─") == ""
}

// OnScreen reports whether capture (tmux capture-pane -p -J of the CLI's
// pane) shows a dialog that keys typed into the pane would answer, and which.
//
// 🔴 A FALSE "YES" WEDGES THE SENDER, SO ONLY THE BOTTOM OF THE SCREEN COUNTS.
// Every paste is refused while this answers true, and an idle screen does not
// change by itself: text that merely LOOKS like a dialog — model output quoting a
// picker's "Review your answers" / "Submit answers" or a permission prompt's
// "❯ 1. Yes" and "Esc to cancel", or such text sitting in the input box — would
// refuse every send for as long as it is on screen. (ccd's first version ran
// askdrive.Parse and kept its Unknown kind, a guess from those words ANYWHERE on
// the screen; a transcript quoting them wedged an idle prompt.) At its prompt the
// CLI draws the input box between two rules with its status line under the
// second, so the transcript and the box are never at the bottom, nor below the
// last rule. What is recognised:
//
//   - the AskUserQuestion picker, by askdrive.Parse's STRUCTURAL kinds only
//     (Question, Review, Clipped), each anchored to the bottom of the screen: its
//     footer among the last three lines, or its review screen's Submit/Cancel rows
//     as the last two. Not Unknown. (Not "below the last rule" either: the picker
//     draws a rule of its own between its options and "Chat about this".)
//   - below the screen's LAST horizontal rule: a numbered select menu's cursor on
//     an option ("❯ 1. Yes"), or the permission prompt's footer ("Esc to cancel ·
//     Tab to amend …") as one of the last three lines. The permission prompt
//     replaces the input box and draws a rule as its top border, so it is below
//     the last one.
//
// Read against captures of the real CLI: 2.1.296 at its prompt, at a Bash
// permission prompt, and at its prompt with those words in the transcript and
// in the input box (cmd/ccd/testdata/pane_*_2.1.296.txt); and 2.1.289's
// permission prompt, pickers and finished turns (internal/askdrive/testdata).
func OnScreen(capture string) (what string, shown bool) {
	switch askdrive.Parse(capture).Kind {
	case askdrive.Question, askdrive.Review, askdrive.Clipped:
		return "the AskUserQuestion picker", true
	}
	lines := strings.Split(strings.ReplaceAll(capture, "\r", ""), "\n")
	from := 0
	for i := len(lines) - 1; i >= 0; i-- {
		if IsRule(lines[i]) {
			from = i + 1
			break
		}
	}
	nonEmpty := 0
	for i := len(lines) - 1; i >= from; i-- {
		t := strings.TrimSpace(lines[i])
		if t == "" {
			continue
		}
		nonEmpty++
		if nonEmpty <= 3 && strings.HasPrefix(t, "Esc to cancel") {
			return "a permission prompt", true
		}
		if selectCursor.MatchString(lines[i]) {
			return "a select menu (" + t + ")", true
		}
	}
	return "", false
}

// InputBox is what Claude Code's input box holds on capture, and whether the
// box was found at all. The CLI draws the box between the screen's LAST TWO
// horizontal rules, its first row starting with the prompt mark `❯`; the
// status line is under the second rule. text is the box's rows with the mark
// and the padding trimmed, joined by newlines: "" for an empty box.
//
// found is false when the bottom of the screen is not that shape — fewer than
// two rules, nothing between them, or a first row that is not the prompt — and
// a caller that needs the box empty refuses on it: an unreadable box is not
// presumed empty. Call OnScreen first: a dialog replaces the box.
//
// Read against the real CLI (cmd/ccd/testdata/pane_*_2.1.296.txt): the empty
// prompt `❯ ` at idle, and `❯ ❯ 1. Yes` / `❯ [Pasted text #1 +4 lines]` with
// text in the box.
func InputBox(capture string) (text string, found bool) {
	lines := strings.Split(strings.ReplaceAll(capture, "\r", ""), "\n")
	last, prev := -1, -1
	for i := len(lines) - 1; i >= 0; i-- {
		if !IsRule(lines[i]) {
			continue
		}
		if last < 0 {
			last = i
			continue
		}
		prev = i
		break
	}
	if prev < 0 || last-prev < 2 {
		return "", false
	}
	box := lines[prev+1 : last]
	first := strings.TrimSpace(box[0])
	if !strings.HasPrefix(first, "❯") {
		return "", false
	}
	rows := []string{strings.TrimSpace(strings.TrimPrefix(first, "❯"))}
	for _, l := range box[1:] {
		rows = append(rows, strings.TrimSpace(l))
	}
	return strings.TrimSpace(strings.Join(rows, "\n")), true
}
