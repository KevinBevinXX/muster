package main

import (
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/askdrive"
)

// errDialogShown: the CLI's pane shows a dialog — a permission prompt, the
// AskUserQuestion picker, any numbered select menu — so a paste or an Enter would
// ANSWER it rather than reach the model. Nothing was typed. The server answers it
// 409 `busy` with code `awaiting_approval` (server.go runTurn): `busy` because
// that is the refusal muster already reads as "nothing was pasted, send it later"
// (internal/agentgateway.Unsent).
var errDialogShown = errors.New("the CLI's pane shows a dialog that a paste or Enter would answer")

// 🔴 WHY ccd READS THE SCREEN BEFORE IT TYPES — AND WHAT THAT DOES NOT STOP.
// Whether a turn may be pasted is decided from hook state (sessionStarted, busy,
// awaitingApproval), and hook state can be wrong: a hook that never arrived, a
// stale one, or a forged one. ccd's keys then land on whatever the pane shows,
// and on a dialog they answer it — Enter takes the highlighted option, which on
// the permission prompt is "1. Yes". This check is for ccd's OWN keys: the paste
// and its Enter, and runTurn's submit-retry Enter, which before this would answer
// a dialog that opened after a paste whose UserPromptSubmit never arrived.
//
// IT IS NOT A DEFENCE AGAINST A PROCESS OF ccd's OWN USER. Every tool the CLI
// runs inherits TMUX and can `tmux send-keys` into the pane itself, answering
// any dialog with no help from ccd. What the check closes is the path where such
// a process — or anything else that gets a hook accepted — makes ccd type into
// a dialog for it.
//
// ⚠ A DENY-LIST, NOT AN ALLOW-LIST, AND READ FROM ONE CAPTURE. It recognises the
// dialogs below by their structure; a dialog drawn some other way is not
// recognised. It is a check before the keys, not a lock: a dialog that opens in
// the milliseconds between the capture and the keys is not caught — which needs
// a turn running in the session at that moment, and so a session ccd already
// believes busy, or a forged hook timed to that window.

// selectCursor is the CLI's select-menu cursor on a numbered option, as the
// permission prompt and the picker draw it: "❯ 1. Yes" (a space, or a no-break
// space, after the ❯). Leading spaces are the dialog's indent.
var selectCursor = regexp.MustCompile(`^\s*❯[ \x{00a0}]+\d+\.\s`)

// isRule reports whether line is one of the CLI's horizontal rules (a run of
// U+2500 box-drawing characters, as above and below its input box).
func isRule(line string) bool {
	t := strings.TrimSpace(line)
	return utf8.RuneCountInString(t) >= 10 && strings.Trim(t, "─") == ""
}

// dialogOnScreen reports whether capture (tmux capture-pane -p -J of the CLI's
// pane) shows a dialog that keys typed into the pane would answer, and which.
//
// 🔴 A FALSE "YES" WEDGES THE AGENT, SO ONLY THE BOTTOM OF THE SCREEN COUNTS.
// Every paste is refused while this answers true, and an idle screen does not
// change by itself: text that merely LOOKS like a dialog — model output quoting a
// picker's "Review your answers" / "Submit answers" or a permission prompt's
// "❯ 1. Yes" and "Esc to cancel", or such text sitting in the input box — would
// refuse every turn for as long as it is on screen. (The first version ran
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
// Read against captures of the real CLI: the pinned 2.1.296 at its prompt, at a
// Bash permission prompt, and at its prompt with those words in the transcript
// and in the input box (testdata/pane_*_2.1.296.txt, captured in the image); and
// 2.1.289's permission prompt, pickers and finished turns
// (internal/askdrive/testdata).
func dialogOnScreen(capture string) (what string, shown bool) {
	switch askdrive.Parse(capture).Kind {
	case askdrive.Question, askdrive.Review, askdrive.Clipped:
		return "the AskUserQuestion picker", true
	}
	lines := strings.Split(strings.ReplaceAll(capture, "\r", ""), "\n")
	from := 0
	for i := len(lines) - 1; i >= 0; i-- {
		if isRule(lines[i]) {
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
