package main

import (
	"errors"

	"github.com/ZacxDev/muster/internal/ccdialog"
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
// dialogs ccdialog.OnScreen lists by their structure; a dialog drawn some other way is not
// recognised. It is a check before the keys, not a lock: a dialog that opens in
// the milliseconds between the capture and the keys is not caught — which needs
// a turn running in the session at that moment, and so a session ccd already
// believes busy, or a forged hook timed to that window.

// dialogOnScreen is ccdialog.OnScreen: the reading, and what it does and does
// not recognise, are documented there — it is shared with muster-mirror's
// Claude prompt guard, which runs the same check before it pastes into a
// mirrored Claude Code pane.
func dialogOnScreen(capture string) (what string, shown bool) { return ccdialog.OnScreen(capture) }

// isRule is ccdialog.IsRule.
var isRule = ccdialog.IsRule
