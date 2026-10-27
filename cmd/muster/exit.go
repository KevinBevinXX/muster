package main

import (
	"errors"
	"fmt"
)

// Exit codes are part of this CLI's contract with scripts. They must stay
// stable — a script branching on 4 (not found) vs 7 (route absent) is the whole
// point of having more than "0 or 1".
//
// ⚠ THE SET IS DUPLICATED FROM THE UPSTREAM CLI THIS ONE WAS EXTRACTED FROM,
// DELIBERATELY AND WITH THE SAME NUMBERS. The two binaries speak to two
// services and share no code, but a caller that drives both — a hook script, a
// harness, an agent — branches on one table. Renumbering here would make the
// same integer mean two things depending on which binary answered, which is a
// worse failure than the duplication.
const (
	exitOK          = 0 // success
	exitServerError = 1 // generic server error (5xx)
	exitUsage       = 2 // usage error: bad flags, ambiguous name, EMPTY PATH PARAM
	exitAuth        = 3 // 401/403
	exitNotFound    = 4 // 404 on a route that exists (resource missing)
	exitConflict    = 5 // 409
	exitNetwork     = 6 // connection refused / timeout / DNS
	exitRouteAbsent = 7 // 404 from the mux itself: this CLI is newer than the server
	exitNonJSON     = 8 // a non-JSON body where JSON was expected (an auth portal, a proxy page)
	// exitUnarmed is its own code because "the server refuses to serve this
	// surface at all" is a DIFFERENT operational fact from "the server broke"
	// (1) and from "your credential is wrong" (3), and a script needs to tell
	// them apart: the first is fixed by arming the server, the second by
	// retrying, the third by fixing a token.
	//
	// 🔴 THE 503 IT MAPS FROM IS NOT AN OUTAGE, AND THE DISCRIMINATOR IS THE
	// BODY RATHER THAN THE STATUS. Nearly every hook-tier route is
	// enforce-when-set, so a 503 there really is a sick backend and stays
	// exitServerError. api.requireArmedHookToken is the exception — it is
	// fail-CLOSED and marks its refusal with api.HookUnarmedField. Folding that
	// into exitServerError would report "the server broke" about a server doing
	// exactly what it was built to do. See unarmedHookSurface.
	exitUnarmed = 9 // a SERVER-side surface is not armed (503)
	// exitAborted is its own code because "THIS CLIENT stopped waiting" is not a
	// statement about the network, and folding it into exitNetwork (6) reports
	// "network unreachable" for a deadline that simply elapsed.
	//
	// 🔴 THE DISTINCTION IS OPERATIONAL, NOT COSMETIC. On exitNetwork nothing
	// reached the server, so a retry is free. On exitAborted the request DID
	// leave this process and its outcome is unknown here, so a blind retry can
	// repeat a write that already landed. The remedy is a longer --timeout.
	exitAborted = 10 // this CLIENT's own deadline or context ended the wait
)

// cliError carries the exit code alongside the message. Every failure path in
// this binary produces one; anything else (a raw cobra flag-parse error, say)
// falls back to exitUsage in exitCodeFor.
type cliError struct {
	code int
	msg  string
	err  error
}

func (e *cliError) Error() string {
	if e.err != nil && e.msg == "" {
		return e.err.Error()
	}
	if e.err != nil {
		return e.msg + ": " + e.err.Error()
	}
	return e.msg
}

func (e *cliError) Unwrap() error { return e.err }

func failf(code int, format string, args ...any) *cliError {
	return &cliError{code: code, msg: fmt.Sprintf(format, args...)}
}

func wrapf(code int, err error, format string, args ...any) *cliError {
	return &cliError{code: code, msg: fmt.Sprintf(format, args...), err: err}
}

// exitCodeFor maps an error returned from the command tree to a process exit
// code. A nil error is 0; a cliError carries its own code; anything else is a
// usage error, because the only non-cliError paths are cobra's own argument and
// flag validation.
func exitCodeFor(err error) int {
	if err == nil {
		return exitOK
	}
	var ce *cliError
	if errors.As(err, &ce) {
		return ce.code
	}
	return exitUsage
}

// exitCodeHelp is appended to the root command's help so the contract is
// discoverable from the binary itself, not only from a design document.
//
// 🔴 TestEveryExitCodeIsDocumentedAndEveryDocumentedCodeExists reads the
// constants above out of THIS FILE's AST and pins the ledger both ways, so a
// code this binary can return and does not document fails the build rather than
// reaching a script that treats it as an unknown failure.
const exitCodeHelp = `Exit codes:
  0  success
  1  server error (5xx)
  2  usage error (bad flags, ambiguous agent name, empty path parameter)
  3  auth failure (401/403) — the server examined the credential this request presented
     and rejected it. The message names the variable to check; the value is never printed
  4  not found (404 on a known route)
  5  conflict (409 — e.g. editing an in-progress task)
  6  network unreachable (DNS, connection refused, a broken connection) — nothing
     arrived, so a retry is free. A DEADLINE is 10, not this
  7  route absent (404 from the router: this client is newer than the server it called)
  8  non-JSON response where JSON was expected (an auth portal or a proxy error page)
  9  a SERVER-side surface is not armed — not an outage, and not your credential,
     which was not even checked. The write half of ` + "`agent ask`" + ` is fail-CLOSED
     (it answers 503 carrying ` + "`\"unarmed\": true`" + `), so a server with no hook token
     configured lands here. Arm the server; do not rotate your token
 10  THIS CLIENT gave up waiting (its --timeout, or a cancelled context) — the request
     DID leave this process, so its outcome is unknown here. Raise --timeout rather than
     retrying blindly, because a write that already landed would be repeated

Output discipline: JSON goes to stdout and NOTHING else ever goes to stdout.
Diagnostics, warnings and version-skew notices go to stderr, so piping stdout
into ` + "`jq`" + ` can never be corrupted.`
