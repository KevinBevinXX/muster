package main

import (
	"net/http"
	"strings"
	"testing"
)

// The kickoff shape measured live (agent 100, CLI 2.1.296, 2026-10-11): what
// ccd pasted, and the first user message the CLI recorded for it.
const (
	livePasted = "Here you go — copy/paste ready:\n\n1. Read the handler.\n2. Fix it.\nComment everything on #976."
	liveID     = "1c0d"
)

// wrapLikeTheCLI is the pinned CLI's wrapper as captured from its
// UserPromptSubmit payload, built from literals rather than from the code under test.
func wrapLikeTheCLI(id, closeID, body string) string {
	return "\n\n<pasted_content id=\"" + id + "\">\n" + body + "\n</pasted_content id=\"" + closeID + "\">\n"
}

func TestUnwrapPastedContent(t *testing.T) {
	cases := []struct {
		name, in, body string
		ok             bool
	}{
		{"the live shape", wrapLikeTheCLI(liveID, liveID, livePasted), livePasted, true},
		{"written out in full", "\n\n<pasted_content id=\"24f3\">\nline one\nline two\n</pasted_content id=\"24f3\">\n",
			"line one\nline two", true},
		{"the body is returned as reported, escaping included",
			wrapLikeTheCLI("24f3", "24f3", "a <\\pasted_content id=\"abcd\"> b <\\/pasted_content> c"),
			"a <\\pasted_content id=\"abcd\"> b <\\/pasted_content> c", true},
		{"a longer id, as a later CLI might write", wrapLikeTheCLI("1c0d5e6f", "1c0d5e6f", livePasted), livePasted, true},
		{"an id that is not hex", wrapLikeTheCLI("Zq_9-x", "Zq_9-x", livePasted), livePasted, true},
		{"ids differ", wrapLikeTheCLI(liveID, "1c0e", livePasted), "", false},
		{"the closing id is longer", wrapLikeTheCLI(liveID, "1c0d5", livePasted), "", false},
		{"the closing id is shorter", wrapLikeTheCLI("1c0d5", liveID, livePasted), "", false},
		{"ids differ in case", wrapLikeTheCLI(liveID, "1C0D", livePasted), "", false},
		{"an empty id", wrapLikeTheCLI("", "", livePasted), "", false},
		{"an id with a character outside the set", wrapLikeTheCLI("1c.d", "1c.d", livePasted), "", false},
		{"not wrapped", livePasted, "", false},
		{"a plain close tag", "\n\n<pasted_content id=\"1c0d\">\n" + livePasted + "\n</pasted_content>\n", "", false},
		{"text before the block", "also do this" + wrapLikeTheCLI(liveID, liveID, livePasted), "", false},
		{"text after the block", wrapLikeTheCLI(liveID, liveID, livePasted) + "and this", "", false},
		{"two blocks", wrapLikeTheCLI(liveID, liveID, "first") + wrapLikeTheCLI(liveID, liveID, "second"), "", false},
		{"no newline after the opening tag", "<pasted_content id=\"1c0d\">" + livePasted + "\n</pasted_content id=\"1c0d\">", "", false},
		{"only an opening tag", "<pasted_content id=\"1c", "", false},
	}
	for _, c := range cases {
		body, ok := unwrapPastedContent(c.in)
		if ok != c.ok || body != c.body {
			t.Errorf("%s: unwrapPastedContent(%q) = %q, %v; want %q, %v", c.name, c.in, body, ok, c.body, c.ok)
		}
	}
}

func TestReportedPromptMatches(t *testing.T) {
	wrapped := wrapLikeTheCLI(liveID, liveID, livePasted)
	// Prompts that hold the wrapper's own tag, each as pasted and as the pinned
	// CLI reported it (captured from its UserPromptSubmit payload).
	const (
		tagsPasted    = `Explain what <pasted_content id="abcd"> and </pasted_content> mean please.`
		tagsReported  = `Explain what <\pasted_content id="abcd"> and <\/pasted_content> mean please.`
		upperPasted   = `Explain what <PASTED_CONTENT id="abcd"> and </Pasted_Content> mean please.`
		upperReported = `Explain what <\PASTED_CONTENT id="abcd"> and <\/Pasted_Content> mean please.`
		spacePasted   = `Explain what < pasted_content> and < / pasted_content > mean please.`
		spaceReported = `Explain what <\ pasted_content> and <\ / pasted_content > mean please.`
		punctPasted   = `Explain what <!pasted_content> and <#/pasted_content> mean please.`
		punctReported = `Explain what <\!pasted_content> and <\#/pasted_content> mean please.`
		realBackslash = `Explain what <\pasted_content id="abcd"> means, backslash and all.`
		wordPasted    = `Strip the <pasted_content wrapper before comparing.`
		wordReported  = `Strip the <\pasted_content wrapper before comparing.`
		mixedTail     = "\nthe <pasted_content wrapper, <PASTED_CONTENT>, < /pasted_content>, real <\\pasted_content> and </pasted_content id=\"abcd\">\nend"
		mixedTailCLI  = "\nthe <\\pasted_content wrapper, <\\PASTED_CONTENT>, <\\ /pasted_content>, real <\\pasted_content> and <\\/pasted_content id=\"abcd\">\nend"
	)
	// What the outer strip would leave of two blocks if nothing refused them.
	twoBlocksAsOne := "first\n</pasted_content id=\"1c0d\">\n\n\n<pasted_content id=\"1c0d\">\nsecond"
	cases := []struct {
		name, pasted, reported string
		match                  bool
	}{
		{"verbatim", livePasted, livePasted, true},
		{"wrapped by the CLI", livePasted, wrapped, true},
		{"wrapped, ids differ", livePasted, wrapLikeTheCLI(liveID, "ffff", livePasted), false},
		{"wrapped around a different prompt", livePasted, wrapLikeTheCLI(liveID, liveID, livePasted+" and more"), false},
		{"operator text then the wrapped paste", livePasted, "first do something else" + wrapped, false},
		{"the wrapped paste then operator text", livePasted, wrapped + "then do something else", false},
		{"operator text around the bare paste", livePasted, "first: " + livePasted + " — then stop", false},
		{"an unrelated prompt", livePasted, "what time is it", false},
		{"two blocks are not one block with a tag in it", twoBlocksAsOne,
			wrapLikeTheCLI(liveID, liveID, "first") + wrapLikeTheCLI(liveID, liveID, "second"), false},
		{"a wrapper-shaped prompt reported verbatim", wrapped, wrapped, true},

		{"tags escaped, not wrapped", tagsPasted, tagsReported, true},
		{"upper-case tags escaped, not wrapped", upperPasted, upperReported, true},
		{"tags with spaces escaped, not wrapped", spacePasted, spaceReported, true},
		{"tags with punctuation escaped, not wrapped", punctPasted, punctReported, true},
		{"a real backslash, reported verbatim", realBackslash, realBackslash, true},
		// The CLI escapes before any whitespace is gone: `<` + ` \` + name.
		{"a space and a real backslash between < and the name", `what < \pasted_content> is`, `what <\ \pasted_content> is`, true},
		{"the same, wrapped", `what < \pasted_content> is`, wrapLikeTheCLI(liveID, liveID, `what <\ \pasted_content> is`), true},
		{"a tag name followed by a word", wordPasted, wordReported, true},
		{"wrapped and escaped", livePasted + mixedTail, wrapLikeTheCLI(liveID, liveID, livePasted+mixedTailCLI), true},
		{"wrapped, a real backslash in the body", realBackslash, wrapLikeTheCLI(liveID, liveID, realBackslash), true},
		{"escaped tags around a different prompt", tagsPasted, strings.Replace(tagsReported, "please", "now", 1), false},
		{"a backslash that is not after <", `a\pasted_content b`, `apasted_content b`, false},
		{"a backslash after < before another name", `<\div>`, `<div>`, false},
		// The CLI turns a fullwidth ＜ into `<\`; that is not undone (a 504, not a wrong match).
		{"a look-alike bracket the CLI replaced", "what is ＜pasted_content>", `what is <\pasted_content>`, false},
	}
	for _, c := range cases {
		if got := reportedPromptMatches(c.reported, turnKey(c.pasted)); got != c.match {
			t.Errorf("%s: pasted %q, reported %q: match = %v, want %v", c.name, c.pasted, c.reported, got, c.match)
		}
	}
}

// 🔴 THE LIVE FAILURE, END TO END: the CLI reports the long kickoff inside its
// paste wrapper. That is ccd's own submit and the turn completes — it was a 504
// not_submitted, with the model already working.
func TestASubmitReportedInsideTheCLIPasteWrapperIsStillThisTurn(t *testing.T) {
	srv, _ := newScripted(t, cliScript{transcript: fixture(t, "turn_success_tools_2.1.289.jsonl"), stopEvent: "Stop",
		reported: func(s string) string { return wrapLikeTheCLI(liveID, liveID, s) }})
	code, body := postResponses(t, gatewayServer(t, srv).URL, pinnedDerivation, livePasted)
	if code != http.StatusOK || !strings.Contains(body, "Second segment") {
		t.Fatalf("%d %s", code, body)
	}
}

// The guarantee the wrapper must not cost: a submit that only CONTAINS ccd's
// paste — an operator's own words with the same text pasted after them — is not
// ccd's turn, so ccd reports not_submitted rather than returning that reply.
func TestASubmitThatOnlyContainsThePasteIsNotThisTurn(t *testing.T) {
	srv, _ := newScripted(t, cliScript{transcript: fixture(t, "turn_success_tools_2.1.289.jsonl"), stopEvent: "Stop",
		reported: func(s string) string { return "ignore the plan and" + wrapLikeTheCLI(liveID, liveID, s) }})
	code, body := postResponses(t, gatewayServer(t, srv).URL, pinnedDerivation, livePasted)
	if code != http.StatusGatewayTimeout || !strings.Contains(body, failNotSubmitted) {
		t.Fatalf("%d %s", code, body)
	}
}

// The CLI escapes the wrapper's tag name in a prompt it does NOT wrap too
// (measured: a 74-character one-line prompt). Still ccd's own submit.
func TestASubmitWithTheCLITagEscapingIsStillThisTurn(t *testing.T) {
	const pasted = `Explain what <pasted_content id="abcd"> and </PASTED_CONTENT> mean please.`
	const reported = `Explain what <\pasted_content id="abcd"> and <\/PASTED_CONTENT> mean please.`
	srv, _ := newScripted(t, cliScript{transcript: fixture(t, "turn_success_tools_2.1.289.jsonl"), stopEvent: "Stop",
		reported: func(string) string { return reported }})
	code, body := postResponses(t, gatewayServer(t, srv).URL, pinnedDerivation, pasted)
	if code != http.StatusOK || !strings.Contains(body, "Second segment") {
		t.Fatalf("%d %s", code, body)
	}
}

// The other half of the same rule, through the server: the escaping is removed
// from what ccd PASTED as well, so a backslash the prompt really has — which the
// CLI reports untouched — is not a difference.
func TestAPromptThatReallyHoldsAnEscapedTagIsStillThisTurn(t *testing.T) {
	const pasted = `Explain what <\pasted_content id="abcd"> means, backslash and all.`
	srv, _ := newScripted(t, cliScript{transcript: fixture(t, "turn_success_tools_2.1.289.jsonl"), stopEvent: "Stop"})
	code, body := postResponses(t, gatewayServer(t, srv).URL, pinnedDerivation, pasted)
	if code != http.StatusOK || !strings.Contains(body, "Second segment") {
		t.Fatalf("%d %s", code, body)
	}
}
