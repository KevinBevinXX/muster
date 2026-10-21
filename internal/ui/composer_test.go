package ui

import (
	"html"
	"regexp"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// formOf is the element with id — from its opening tag to its closing tag —
// on page; "" when it is not there.
func formOf(page, id string) string {
	i := strings.Index(page, `<form id="`+id+`"`)
	if i < 0 {
		return ""
	}
	rest := page[i:]
	return rest[:strings.Index(rest, "</form>")]
}

var hiddenField = regexp.MustCompile(`<input( id="[^"]*")? type="hidden" name="([^"]+)" value="([^"]*)"`)

// hiddenFields is a form's hidden inputs, name → value: what a browser posts
// beside what was typed.
func hiddenFields(form string) map[string]string {
	out := map[string]string{}
	for _, m := range hiddenField.FindAllStringSubmatch(form, -1) {
		out[m[2]] = html.UnescapeString(m[3])
	}
	return out
}

// TestTheComposerIsAFormTheBrowserCanSubmitByItself is the property a JS-only
// composer did not have: with no script at all the element is still a complete
// form. It has a method and an action, the address it sends to is in hidden
// inputs (not in data attributes only a script reads), the text has a name,
// and the button submits.
//
// 🔴 A REGRESSION TEST: on the commit before this the form was `<form
// id="mv-form" data-send=… data-host=… novalidate>` — no method, no action, no
// hidden input — so every assertion below about method, action and fields fails
// there. A browser submitting THAT form GETs the current page with `?text=…`.
func TestTheComposerIsAFormTheBrowserCanSubmitByItself(t *testing.T) {
	c := &ComposerView{SendPath: MirrorTerminalSendPath, Host: "host-a", Session: "work main", Window: "@12",
		Placeholder: "Message…", Aria: "x", Nonce: "0123456789abcdef0123456789abcdef"}
	page := renderString(t, AgentTerminalPage(AgentTerminalView{Name: "work main", StepUp: stepUpFixture(), Composer: c}))
	form := formOf(page, "mv-form")
	if form == "" {
		t.Fatal("no composer form")
	}
	open := form[:strings.Index(form, ">")]
	if !strings.Contains(open, `method="post"`) {
		t.Errorf("the form has no method=post — a browser would GET it: %s", open)
	}
	// The action is the send route, with the address in its query too (for a
	// post that comes back from a sign-in as a GET).
	if want := `action="/mirror/terminal/send?host=host-a&amp;session=work+main&amp;window=%4012"`; !strings.Contains(open, want) {
		t.Errorf("the form's action is not %s: %s", want, open)
	}
	if got := hiddenFields(form); got["host"] != "host-a" || got["session"] != "work main" || got["window"] != "@12" || got["nonce"] != c.Nonce || len(got) != 4 {
		t.Errorf("hidden fields = %v; want host, session, window and the nonce", got)
	}
	if !strings.Contains(form, `<textarea id="mv-input" name="text"`) {
		t.Error("the text has no name: a browser's own submit would send none")
	}
	if !strings.Contains(form, `<button id="mv-send" type="submit"`) {
		t.Error("Send is not a submit button")
	}
	// The nonce is not part of the ADDRESS: it is not in the action.
	if strings.Contains(open, "nonce") {
		t.Errorf("the nonce leaked into the action: %s", open)
	}
	// With no window (a per-session host) there is no window field, in the form
	// or the action — the URL is what it always was.
	plain := renderString(t, AgentTerminalPage(AgentTerminalView{Name: "main", StepUp: stepUpFixture(),
		Composer: &ComposerView{SendPath: MirrorTerminalSendPath, Host: "host-a", Session: "main"}}))
	pf := formOf(plain, "mv-form")
	if _, has := hiddenFields(pf)["window"]; has || strings.Contains(pf, "window=") || strings.Contains(pf, `name="nonce"`) {
		t.Errorf("a composer with no window / nonce still carries one: %s", pf[:strings.Index(pf, "<textarea")])
	}
}

// TestTheComposerDrawsANoticeTheServerGaveIt: the outcome of a send the BROWSER
// submitted comes back as a redirect, and the page it lands on draws it — with
// no script involved. With none, the status line is there, empty and hidden,
// for the script to use.
func TestTheComposerDrawsANoticeTheServerGaveIt(t *testing.T) {
	render := func(c ComposerView) string {
		c.SendPath, c.Host, c.Session = MirrorTerminalSendPath, "h", "s"
		page := renderString(t, AgentTerminalPage(AgentTerminalView{Name: "s", StepUp: stepUpFixture(), Composer: &c}))
		i := strings.Index(page, `<p id="mv-note"`)
		return page[i : i+strings.Index(page[i:], "</p>")]
	}
	none := render(ComposerView{})
	if !strings.Contains(none, ` hidden=""`) || !strings.HasSuffix(none, ">") {
		t.Errorf("with no notice the status line should be hidden and empty: %s", none)
	}
	bad := render(ComposerView{Notice: `The host is <offline> & "gone".`})
	if strings.Contains(bad, ` hidden=""`) || strings.Contains(bad, "data-ok") || !strings.Contains(bad, "text-st-error-fg") ||
		!strings.HasSuffix(bad, `The host is &lt;offline&gt; &amp; &#34;gone&#34;.`) {
		t.Errorf("a failure notice should be visible, in the error colour, as text: %s", bad)
	}
	good := render(ComposerView{Notice: "Sent.", NoticeOK: true})
	if strings.Contains(good, ` hidden=""`) || !strings.Contains(good, `data-ok=""`) || strings.Contains(good, "text-st-error-fg") || !strings.HasSuffix(good, "Sent.") {
		t.Errorf("a success notice should be visible, marked ok, not in the error colour: %s", good)
	}
}

// TestEveryMessageBoxIsOneComposer: 🔴 a SEAM guard, widened from the two
// mirrored views to the third page that now has a message box — a dispatched
// agent's Terminal. All three carry the same composer script byte for byte,
// exactly once, and the same form element; they differ in the route, the
// address fields and the placeholder.
func TestEveryMessageBoxIsOneComposer(t *testing.T) {
	const marker = "HTMLFormElement.prototype.submit.call(form);"
	pages := map[string]string{
		"a mirrored session's rendered view": renderString(t, MirrorViewPage(MirrorViewView{Host: "host-a", Session: "main", Interactive: true, StepUp: stepUpFixture()})),
		"a mirrored session's terminal": renderString(t, AgentTerminalPage(AgentTerminalView{Name: "main", StepUp: stepUpFixture(),
			Composer: &ComposerView{SendPath: MirrorTerminalSendPath, Host: "host-a", Session: "main",
				DraftKey: MirrorDraftKey(1, "terminal", "host-a", "main", "")}})),
		"an agent's terminal": renderString(t, AgentTerminalPage(AgentTerminalView{Name: "keen-wren", RenderedHref: AgentPath("keen-wren"),
			Composer: &ComposerView{SendPath: AgentSendPath("keen-wren"), Extra: []ComposerField{{Name: "from", Value: AgentSendFromTerminal}},
				DraftKey: AgentTerminalDraftKey(1, "keen-wren")}})),
	}
	var script, shape string
	for name, page := range pages {
		if n := strings.Count(page, marker); n != 1 {
			t.Fatalf("%s carries the composer script %d times, want once", name, n)
		}
		if n := strings.Count(page, `<form id="mv-form"`); n != 1 {
			t.Fatalf("%s has %d composer forms, want 1", name, n)
		}
		s := scriptWith(t, page, marker)
		if script == "" {
			script = s
		} else if s != script {
			t.Errorf("%s runs a DIFFERENT composer script — it is supposed to be one piece of code", name)
		}
		// The form with everything that legitimately differs taken out.
		form := formOf(page, "mv-form")
		form = regexp.MustCompile(` action="[^"]*"`).ReplaceAllString(form, "")
		form = regexp.MustCompile(`<input( id="[^"]*")? type="hidden"[^>]*>`).ReplaceAllString(form, "")
		form = regexp.MustCompile(` (placeholder|aria-label)="[^"]*"`).ReplaceAllString(form, "")
		// The draft's key is the address too (composer_draft.go); that the box
		// HAS one is part of the shape.
		form = regexp.MustCompile(` data-draft-key="[^"]+"`).ReplaceAllString(form, ` data-draft-key="…"`)
		if shape == "" {
			shape = form
		} else if form != shape {
			t.Errorf("%s draws a different composer form:\n%s\n-- want --\n%s", name, form, shape)
		}
	}
	// The agent's composer posts to the agent's route, and says where it came from.
	af := formOf(pages["an agent's terminal"], "mv-form")
	if !strings.Contains(af, `action="/agents/keen-wren/send?from=terminal"`) || hiddenFields(af)["from"] != "terminal" {
		t.Errorf("the agent's composer does not post to its send route from the terminal: %s", af[:strings.Index(af, "<textarea")])
	}
}

// TestAnAgentsRenderedViewIsItsDefaultAndBothViewsHaveTheSwitch: an agent's
// card opens its chat page (the rendered view of its work), and a claude-code
// agent — the kind with a terminal — carries the SAME Rendered | Terminal
// switch a mirrored Claude Code window has, on both pages. A kind with no
// terminal has one view and no switch.
//
// 🔴 A REGRESSION TEST for the switch: before this the chat page had a lone
// "Terminal" link (#open-terminal) and the terminal page had nothing leading
// back but the back arrow.
func TestAnAgentsRenderedViewIsItsDefaultAndBothViewsHaveTheSwitch(t *testing.T) {
	card := renderNode(t, agentCard(AgentCardView{ID: 7, Name: "keen-wren", Kind: agents.KindClaudeCode, CCAccount: "alpha", Status: "running"}))
	if !strings.Contains(card, `<a href="/agents/keen-wren"`) || strings.Contains(card, "/terminal") {
		t.Errorf("an agent's card does not open its rendered view:\n%s", card)
	}

	cc := sampleAgentDetailView()
	cc.Name, cc.Kind, cc.CCAccount = "keen-wren", agents.KindClaudeCode, "alpha"
	chat := renderString(t, AgentDetailPage(cc, featuresAllOn))
	if !strings.Contains(chat, `<span id="view-rendered" aria-current="page"`) {
		t.Error("the chat page does not mark Rendered as the current view")
	}
	if !strings.Contains(chat, `<a id="view-terminal" href="/agents/keen-wren/terminal" hx-boost="false"`) {
		t.Error("the chat page's switch does not lead to the terminal with a real load (hx-boost=false)")
	}
	if strings.Contains(chat, `id="open-terminal"`) {
		t.Error("the old lone Terminal link is still drawn beside the switch")
	}

	term := renderString(t, AgentTerminalPage(AgentTerminalView{Name: "keen-wren", RenderedHref: AgentPath("keen-wren")}))
	if !strings.Contains(term, `<a id="view-rendered" href="/agents/keen-wren" hx-boost="false"`) || !strings.Contains(term, `<span id="view-terminal" aria-current="page"`) {
		t.Error("the terminal page has no switch back to the rendered view")
	}

	gw := sampleAgentDetailView()
	gw.Kind = agents.KindGateway
	if plain := renderString(t, AgentDetailPage(gw, featuresAllOn)); strings.Contains(plain, `id="view-terminal"`) || strings.Contains(plain, `id="view-rendered"`) {
		t.Error("an agent of a kind with no terminal draws a view switch")
	}
}

// TestTheChatFormCanBeSentWithoutItsSocket: the agent chat's own form. Its
// message normally travels over the page's WebSocket; before this it had no
// action and no method, so with the script not run — or the socket not open,
// where the script simply returned — pressing Send did nothing at all. Now it
// is a real form posting to the agent's send route for the session on screen,
// the script hands it to the browser when its socket cannot take it, and it
// wears the composer's clothes (a 16px input: it was 14px, which iOS zooms).
func TestTheChatFormCanBeSentWithoutItsSocket(t *testing.T) {
	v := sampleAgentDetailView()
	v.Name, v.ActiveSessionID, v.SendNonce = "keen-wren", 41, "0123456789abcdef0123456789abcdef"
	page := renderString(t, AgentDetailPage(v, featuresAllOn))
	form := formOf(page, "chat-form")
	open := form[:strings.Index(form, ">")]
	for _, want := range []string{`method="post"`, `action="/agents/keen-wren/send?session=41"`, `hx-boost="false"`} {
		if !strings.Contains(open, want) {
			t.Errorf("the chat form lacks %s: %s", want, open)
		}
	}
	if got := hiddenFields(form); got["session"] != "41" || got["nonce"] != v.SendNonce {
		t.Errorf("the chat form's hidden fields = %v, want the session on screen and the nonce", got)
	}
	input := page[strings.Index(page, `<textarea id="chat-input"`):]
	input = input[:strings.Index(input, ">")]
	if !strings.Contains(input, `name="text"`) || !strings.Contains(input, " text-base ") || strings.Contains(input, "text-sm") || !strings.Contains(input, "min-h-[44px]") {
		t.Errorf("the chat input is not a named, 16px, 44px field: %s", input)
	}
	if !strings.Contains(input, composerInputClass) {
		t.Error("the chat input does not wear the composer's input class")
	}
	// The script's half: a socket that is not open hands over to the browser,
	// it does not return.
	script := scriptWith(t, page, "function nativeSend()")
	if !strings.Contains(script, "if (!ws || ws.readyState !== 1) { nativeSend(); return; }") {
		t.Error("the chat script no longer hands a message to the browser when its socket is not open")
	}
	if strings.Contains(script, "if (!t || !ws || ws.readyState !== 1) return;") {
		t.Error("the chat script still returns silently when its socket is not open")
	}
	// A send the browser made comes back with a notice, drawn above the form.
	v.SendNotice, v.SendNoticeOK = "Sent.", true
	if noted := renderString(t, AgentDetailPage(v, featuresAllOn)); !strings.Contains(noted, `<p id="chat-note" role="status" data-ok=""`) || !strings.Contains(noted, ">Sent.</p>") {
		t.Error("the chat page does not draw the notice it was given")
	}
	if strings.Contains(page, `id="chat-note"`) {
		t.Error("control: a chat page with no notice drew one")
	}
}

// TestThePlainConfirmPageCarriesThePendingSendAndNeedsNoScript: the step-up for
// a send the browser submitted. It is one form — the password, and the send
// that was waiting in hidden fields, text included and inert — posting to the
// route that completes it. Its controls work with no script.
func TestThePlainConfirmPageCarriesThePendingSendAndNeedsNoScript(t *testing.T) {
	const text = `run </textarea><script>alert(1)</script> "it"`
	page := renderString(t, StepUpFormPage(StepUpFormView{Action: "/mirror/confirm-send", BackHref: "/mirror/view?host=h&session=s", Window: "30 minutes",
		Error:  "That password is not right.",
		Fields: []ComposerField{{Name: "kind", Value: "view"}, {Name: "host", Value: "h"}, {Name: "text", Value: text}}}))
	form := formOf(page, "stepup-form")
	if !strings.Contains(form, `method="post" action="/mirror/confirm-send"`) {
		t.Fatalf("the confirm page is not a form posting to its route: %s", form[:min(len(form), 160)])
	}
	if got := hiddenFields(form); got["kind"] != "view" || got["host"] != "h" || got["text"] != text {
		t.Errorf("the pending send is not carried whole: %v", got)
	}
	if strings.Contains(page, "<script>alert(1)</script>") {
		t.Fatal("the pending text was written into the page as markup")
	}
	for _, want := range []string{
		`<input id="stepup-password" name="password" type="password"`,
		`<button id="stepup-submit" type="submit"`,
		`<a id="stepup-cancel" href="/mirror/view?host=h&amp;session=s"`,
		"from the last 30 minutes.",
		`<p id="stepup-error" role="alert"`,
		"That password is not right.",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the confirm page lacks %s", want)
		}
	}
	if first := renderString(t, StepUpFormPage(StepUpFormView{Action: "/x", BackHref: "/agents", Window: "12 hours"})); strings.Contains(first, `id="stepup-error"`) {
		t.Error("the first showing of the confirm page draws an error")
	}
}
