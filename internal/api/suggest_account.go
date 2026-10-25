package api

import (
	"net/http"
	"strconv"

	"github.com/ZacxDev/muster/internal/suggest"
)

// accountSuggestSection is the Account page's "Suggested replies" section:
// the caller's own toggle (POST /account/suggest, back to this page), saying
// what is on and why it is not — the server's kill switch, the auto-pause —
// and the caller's usage (suggest.Engine.Stats: shown, filled, sent, the fill
// rate, the mean edit distance). "" when the server has no suggestions at all.
func (s *Server) accountSuggestSection(r *http.Request) string {
	eng := s.ext.Suggest
	if eng == nil {
		return ""
	}
	const head = `<section id="suggest" data-testid="suggest-settings" class="flex flex-col gap-3 rounded-2xl border border-line bg-s1/70 px-5 py-6 ring-1 ring-line">
<h2 class="text-base font-semibold text-fg">Suggested replies</h2>
<p class="text-sm text-fg2">When a mirrored Claude Code session hands control back, up to three AI-suggested replies appear above the message box. A tap fills the box and never sends. Generating them sends your messages, the session's focus and the end of the conversation to OpenRouter on your key.</p>
`
	saved := ""
	if r.URL.Query().Get("suggest") == "saved" {
		saved = `<p role="status" class="text-sm font-semibold text-fg">Saved.</p>
`
	}
	form := func(on bool, label string) string {
		v := "0"
		if on {
			v = "1"
		}
		return `<form method="POST" action="/account/suggest"><input type="hidden" name="on" value="` + v + `"><input type="hidden" name="back" value="account">
<button type="submit" id="suggest-toggle" class="press min-h-[44px] w-full rounded-md bg-bg px-3 py-2 text-base font-semibold text-fg ring-1 ring-inset ring-edge">` + label + `</button></form>
`
	}
	state := func(id, text string) string {
		return `<p id="suggest-state" data-state="` + id + `" class="text-sm text-fg">` + text + `</p>
`
	}
	if eng.Off {
		return head + state("server-off", "Off for everyone on this server (MUSTER_SUGGEST=off).") + `</section>`
	}
	owner := requestOwner(r.Context())
	usage := s.suggestUsageReadout(r, owner)
	outcome := eng.Enabled(r.Context(), owner)
	if eng.Prefs == nil && outcome != suggest.OutcomePaused {
		return head + state("on", "On. (This server has no database, so it cannot be turned off per user here.)") + usage + `</section>`
	}
	switch outcome {
	case suggest.OutcomeUserOff:
		return head + saved + state("off", "Off.") + usage + form(true, "Turn suggested replies on") + `</section>`
	case suggest.OutcomePaused:
		return head + saved + state("paused", "Paused: after "+strconv.Itoa(suggest.AutoPauseMinShown)+" sets were shown, fewer than "+
			strconv.Itoa(suggest.AutoPauseRatePercent)+"% of them were tapped. Turning them back on starts the count over.") +
			usage + form(true, "Turn suggested replies back on") + `</section>`
	}
	return head + saved + state("on", "On.") + usage + form(false, "Turn suggested replies off") + `</section>`
}

// suggestUsageReadout is the caller's counts as one short list; "" when they
// cannot be read.
func (s *Server) suggestUsageReadout(r *http.Request, owner int64) string {
	t, err := s.ext.Suggest.Stats(r.Context(), owner)
	if err != nil {
		s.logger.Printf("suggest: owner %d: read the counts: %v", owner, err)
		return ""
	}
	n := func(v int64) string { return strconv.FormatInt(v, 10) }
	rate, edit := "—", "—"
	if t.Shown > 0 {
		rate = strconv.FormatFloat(t.FillPercent(), 'f', 1, 64) + "%"
	}
	if t.Sent > 0 {
		edit = strconv.FormatFloat(t.MeanEdit(), 'f', 1, 64) + " characters"
	}
	paused := "no"
	if t.Paused {
		paused = "yes"
	}
	row := func(id, k, v string) string {
		return `<div class="flex justify-between gap-3"><dt class="text-fg2">` + k + `</dt><dd id="suggest-usage-` + id + `" class="tabular-nums text-fg">` + v + `</dd></div>`
	}
	return `<dl id="suggest-usage" data-testid="suggest-usage" class="flex flex-col gap-1 text-sm">` +
		row("shown", "Sets shown", n(t.Shown)) + row("filled", "Tapped", n(t.Filled)) + row("sent", "Sent after a tap", n(t.Sent)) +
		row("rate", "Tap rate", rate) + row("edit", "Mean edit before sending", edit) + row("paused", "Paused", paused) + `</dl>
`
}
