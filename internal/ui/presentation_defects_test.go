package ui

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/taskstatus"
)

// presentation_defects_test.go — regression guards for a batch of UI defects
// measured on the running board, one guard per defect.
//
// 🔴 EVERY ONE OF THESE ASSERTS RENDERED STATE, NOT A WORD THE SOURCE HAPPENS TO
// CONTAIN. The chip guard compares the chip SET against the status enum rather
// than four literal strings; the markdown guards look for `<hr>` and for an
// `href` attribute rather than for the absence of `---` or the presence of
// "http"; the contrast guard COMPUTES a ratio from the palette rather than
// naming a class. A guard another feature can spell its way past is not a guard.
//
// ⚠ WHAT NO TEST HERE CAN SEE. This repository has no browser suite, so nothing
// below renders at a VIEWPORT — these assert the markup and the class ladder the
// browser would then lay out. For the two defects that are layout (the chat
// column width, the contrast ratio) the guards therefore pin the INPUTS to the
// layout — which element carries the width cap, which palette entry the label
// uses — and the measured pixel/ratio figures that motivated them live in the
// comments at the code, where they were taken. Say that rather than let a green
// tick imply a rendering was checked.

// --- Defect 1: the agent chat column -----------------------------------------

// TestTheSidebarOffsetAndTheWidthCapAreNeverOnOneElement is the regression guard
// for a chat column that measured 272px wide on a 2256px screen.
//
// THE SHAPE OF THE BUG, which is what this asserts: `max-w-*` caps the BORDER
// box, and `lg:pl-72` (18rem = 288px) is padding INSIDE that box. Put both on one
// element with max-w-xl (576px) and the content area is 288px before anything
// else happens — narrower than a phone, with ~800px of dead space either side.
// The shell always had the right shape (offset on an outer column, width on the
// element inside it); /agents/{name} was the odd one out.
//
// It sweeps EVERY class attribute of EVERY document rather than the one element
// that was wrong, because the next instance of this will be a different element.
func TestTheSidebarOffsetAndTheWidthCapAreNeverOnOneElement(t *testing.T) {
	checked := 0
	docs := 0
	forEachFeatureShape(t, func(t *testing.T, shape string, feat Features) {
		for name, doc := range shellDocuments(t, feat) {
			docs++
			for _, cls := range classAttrs(doc) {
				if !hasClass(cls, "lg:pl-72") {
					continue
				}
				checked++
				for _, c := range strings.Fields(cls) {
					if strings.Contains(c, "max-w-") {
						t.Errorf("%s, %s: one element carries BOTH the sidebar offset lg:pl-72 and the "+
							"width cap %s — the 288px offset is subtracted from the cap, not added "+
							"outside it, so the content column collapses. Put the offset on an outer "+
							"column and the width on the element inside it (contentWidth()). class=%q",
							shape, name, c, cls)
					}
				}
			}
		}
	})
	if checked == 0 {
		t.Fatal("no element carried lg:pl-72 in any document — this guard measured nothing. " +
			"Either the sidebar offset was renamed (update the guard) or the sweep is broken.")
	}
	t.Logf("checked %d lg:pl-72 elements across %d rendered documents", checked, docs)
}

// TestTheAgentDetailPageSizesItselfWithTheSharedMeasure pins the other half: the
// page uses contentWidth(), the app's ONE horizontal measure, for BOTH its header
// row and its chat column. A page that re-spells a ladder of its own is a page
// that drifts away from the shell again — which is how this defect happened.
func TestTheAgentDetailPageSizesItselfWithTheSharedMeasure(t *testing.T) {
	forEachFeatureShape(t, func(t *testing.T, shape string, feat Features) {
		doc := renderString(t, AgentDetailPage(AgentDetailView{Name: "demo", ID: 1}, feat))
		n := 0
		for _, cls := range classAttrs(doc) {
			if strings.HasPrefix(cls, contentWidth()) {
				n++
			}
		}
		if n < 2 {
			t.Errorf("%s: the agent detail document uses contentWidth() on %d elements, want at "+
				"least 2 (the sticky header row and the chat column). The two must come from the "+
				"SAME helper or the brand and the transcript sit at different left edges.", shape, n)
		}
	})
}

// --- Defect 2: thematic breaks ------------------------------------------------

// TestAThematicBreakRendersAsAnHR asserts the ELEMENT, not the absence of the
// source text: `---` used to reach the page as `<p class="my-1">---</p>` and the
// document contained zero <hr>. Every ClickUp-mirrored task body carries one
// above its footer, so this was visible on most of the board.
func TestAThematicBreakRendersAsAnHR(t *testing.T) {
	cases := []struct{ name, src string }{
		// The shape that was actually broken: no blank line, so the paragraph
		// gather swallowed the rule into the text above it.
		{"tight, between two paragraphs", "the body text\n---\nmirrored from task 123"},
		{"blank lines around it", "above\n\n---\n\nbelow"},
		{"asterisk form", "above\n\n***\n\nbelow"},
		{"underscore form", "above\n\n___\n\nbelow"},
		{"longer run", "above\n\n--------\n\nbelow"},
		{"indented up to three spaces", "above\n\n   ---\n\nbelow"},
	}
	for _, tc := range cases {
		got := markdownHTML(tc.src)
		if !strings.Contains(got, "<hr") {
			t.Errorf("%s: no <hr> in the output for %q:\n%s", tc.name, tc.src, got)
		}
		if strings.Contains(got, ">---<") || strings.Contains(got, ">***<") || strings.Contains(got, ">___<") {
			t.Errorf("%s: the rule reached the page as literal text for %q:\n%s", tc.name, tc.src, got)
		}
		// The text on either side must survive — a rule that eats its neighbours
		// would pass the two checks above and destroy the body.
		for _, want := range []string{"above", "below", "the body text", "mirrored from task 123"} {
			if strings.Contains(tc.src, want) && !strings.Contains(got, want) {
				t.Errorf("%s: %q vanished from the output:\n%s", tc.name, want, got)
			}
		}
	}
}

// TestAThematicBreakDoesNotEatListsOrText is the other direction. `-` and `*`
// are also the bullet markers and `_` is an emphasis marker, so a rule pattern
// that is too eager silently destroys ordinary content.
//
// ⚠ INVARIANT GUARD, NOT REGRESSION COVERAGE. It is green at origin/main too —
// vacuously, because no thematic break was recognised there at all. Mutation-
// checked instead: widening mdThematicBreak to `-{2,}` without the end anchor
// fails THIS test (and only this one) on "two dashes only" and "a dash with
// trailing text".
func TestAThematicBreakDoesNotEatListsOrText(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"a bullet item", "- first\n- second"},
		{"emphasised text", "a _word_ here"},
		{"two dashes only", "--"},
		{"a dash with trailing text", "--- not a rule"},
		{"a table separator row", "a | b\n--- | ---\n1 | 2"},
	} {
		got := markdownHTML(tc.src)
		if strings.Contains(got, "<hr") {
			t.Errorf("%s: %q was mis-read as a thematic break:\n%s", tc.name, tc.src, got)
		}
	}
}

// --- Defect 3: bare-URL autolinking -------------------------------------------

// TestABareURLBecomesASafeAnchor asserts the rendered ANCHOR and its attributes.
// The measured defect: a comment containing a plain pull-request URL rendered as
// text, and the whole task page held six <a> elements, all of them sidebar nav.
// Markdown links already worked, so the gap was specifically bare URLs.
func TestABareURLBecomesASafeAnchor(t *testing.T) {
	got := markdownHTML("landed in https://example.test/owner/repo/pull/141 — see there")

	hrefs := hrefsIn(got)
	if len(hrefs) != 1 || hrefs[0] != "https://example.test/owner/repo/pull/141" {
		t.Fatalf("want exactly one href of the bare URL, got %v in:\n%s", hrefs, got)
	}
	// 🔴 rel AND target are asserted on the ANCHOR, because a new-tab link without
	// rel=noopener hands the opened page a window.opener back-reference to this
	// document.
	for _, want := range []string{`target="_blank"`, `rel="noopener"`} {
		if !strings.Contains(got, want) {
			t.Errorf("the autolinked anchor is missing %s:\n%s", want, got)
		}
	}
}

// TestAutolinkingCannotBecomeAnInjectionVector is the safety half. The rendered
// body is user/agent-authored, so the new pass must not widen what can reach the
// browser as markup or as a dangerous scheme.
//
// ⚠ INVARIANT GUARD, NOT REGRESSION COVERAGE — green at origin/main too, where
// nothing was autolinked at all. Mutation-checked: widening mdBareURL from
// `https?://` to `[a-z]+:` fails THIS test (and only this one) on the
// javascript: and data: cases.
func TestAutolinkingCannotBecomeAnInjectionVector(t *testing.T) {
	cases := []struct {
		name, src string
		wantNo    []string
	}{
		{
			name:   "a quote in the URL cannot break out of the href",
			src:    `https://example.test/a"onmouseover="alert(1)`,
			wantNo: []string{`onmouseover="alert`},
		},
		{
			name:   "an angle bracket in the URL cannot open a tag",
			src:    `https://example.test/a<script>alert(1)</script>`,
			wantNo: []string{"<script>"},
		},
		{
			name:   "javascript: is not a scheme this pass can match",
			src:    `javascript:alert(1) and JAVASCRIPT:alert(1)`,
			wantNo: []string{`href="javascript:`, `href="JAVASCRIPT:`},
		},
		{
			name:   "data: is not a scheme this pass can match",
			src:    `data:text/html;base64,PHNjcmlwdD4=`,
			wantNo: []string{`href="data:`},
		},
	}
	for _, tc := range cases {
		got := markdownHTML(tc.src)
		for _, bad := range tc.wantNo {
			if strings.Contains(got, bad) {
				t.Errorf("%s: output contains %q for %q:\n%s", tc.name, bad, tc.src, got)
			}
		}
		for _, href := range hrefsIn(got) {
			if !strings.HasPrefix(strings.ToLower(href), "http://") && !strings.HasPrefix(strings.ToLower(href), "https://") {
				t.Errorf("%s: emitted a non-http(s) href %q for %q", tc.name, href, tc.src)
			}
		}
	}
}

// TestAutolinkingLeavesMarkdownLinksAndCodeAlone: the new pass runs over the
// output of the earlier ones, so the two ways it can misfire are double-linking a
// markdown link's URL and linking a URL inside a code span.
func TestAutolinkingLeavesMarkdownLinksAndCodeAlone(t *testing.T) {
	got := markdownHTML("[the PR](https://example.test/p/1) and `https://example.test/p/2` and https://example.test/p/3")

	hrefs := hrefsIn(got)
	want := []string{"https://example.test/p/1", "https://example.test/p/3"}
	if len(hrefs) != len(want) {
		t.Fatalf("want %d anchors (the markdown link and the bare URL — NOT the one in code), got %v in:\n%s",
			len(want), hrefs, got)
	}
	for i := range want {
		if hrefs[i] != want[i] {
			t.Errorf("href %d = %q, want %q", i, hrefs[i], want[i])
		}
	}
	if !strings.Contains(got, ">the PR</a>") {
		t.Errorf("the markdown link lost its label — the autolink pass chewed on the anchor:\n%s", got)
	}
	if !strings.Contains(got, "<code") || !strings.Contains(got, "https://example.test/p/2</code>") {
		t.Errorf("the URL inside a code span did not stay literal:\n%s", got)
	}
}

// TestAutolinkingLeavesTrailingSentencePunctuationOut: `see https://x.test/a.`
// must not link the full stop, or every URL at the end of a sentence 404s.
func TestAutolinkingLeavesTrailingSentencePunctuationOut(t *testing.T) {
	for _, tc := range []struct{ src, want string }{
		{"see https://example.test/a.", "https://example.test/a"},
		{"see https://example.test/a, then", "https://example.test/a"},
		{"(https://example.test/a)", "https://example.test/a"},
		{"see https://example.test/a?b=1#c", "https://example.test/a?b=1#c"},
	} {
		hrefs := hrefsIn(markdownHTML(tc.src))
		if len(hrefs) != 1 || hrefs[0] != tc.want {
			t.Errorf("%q linked %v, want [%q]", tc.src, hrefs, tc.want)
		}
	}
}

// --- Defect 4: the status filter chips ----------------------------------------

// TestTheStatusChipSetIsTheStatusEnum is the board-level half of the lane fix
// (the package-level half is taskstatus.TestTheLaneVocabularyIsTheStatusVocabulary).
//
// Measured before: the chips were ""|open|in_progress|done while the per-card
// <select> offered open|in_progress|ready_for_review|complete. Clicking "In
// progress" loaded 30 ready_for_review cards beside 20 in_progress ones, and a
// card labelled "Ready for review" matched no chip of that name — 20% of the
// board (106 of 525 tasks) could not be isolated by any filter.
//
// 🔴 IT IS COMPARED AGAINST taskstatus.All(), NOT AGAINST FOUR STRINGS. A guard
// listing the values would be satisfied by renaming an enum member on one side
// only, which is the same two-vocabularies shape that produced the defect.
func TestTheStatusChipSetIsTheStatusEnum(t *testing.T) {
	got := attrValues(renderString(t, statusFilterRow("")), "data-status-filter")

	want := append([]string{""}, taskstatus.All()...)
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("status chips are %v, want %v (the empty All chip plus exactly one chip per "+
			"status). A status with no chip of its own cannot be isolated by the filter at all.",
			got, want)
	}

	// Every chip must also carry distinct human copy, or two of them read the same.
	labels := attrValues(renderString(t, statusFilterRow("")), "title")
	seen := map[string]bool{}
	for _, l := range labels {
		if seen[l] {
			t.Errorf("two status chips share the title %q", l)
		}
		seen[l] = true
	}
}

// TestTheSelectedStatusChipIsTheOneAsked: aria-pressed must follow the active
// lane, or the row shows a filter that is not the one narrowing the board.
func TestTheSelectedStatusChipIsTheOneAsked(t *testing.T) {
	for _, s := range taskstatus.All() {
		html := renderString(t, statusFilterRow(s))
		want := `data-status-filter="` + s + `" aria-pressed="true"`
		if !strings.Contains(html, want) {
			t.Errorf("with %q active, no chip renders %s:\n%s", s, want, html)
		}
		if n := strings.Count(html, `aria-pressed="true"`); n != 1 {
			t.Errorf("with %q active, %d chips render as pressed, want exactly 1", s, n)
		}
	}
}

// --- Defect 5: the tag filter row ---------------------------------------------

// TestExternalIDTagsAreNotOfferedAsFilterChips is the regression guard for a
// filter row holding 241 chips, 202 of them `clickup:<id>` — a 33,967px row in a
// 1,736px viewport with the scrollbar suppressed and the overflow cue
// pointer-events-none, i.e. about twenty screens of controls with no way to reach
// them.
//
// It asserts the RELATIONSHIP — the offered set excludes exactly the external-id
// namespaces and keeps everything else — rather than the absence of the word
// "clickup", so adding a third such namespace is covered the day it is added.
func TestExternalIDTagsAreNotOfferedAsFilterChips(t *testing.T) {
	vocab := []notes.TagCount{
		{Tag: "backend", Count: 12},
		{Tag: "runbook:deploy", Count: 3},
		{Tag: "ui", Count: 7},
	}
	// One chip per external-id namespace, built from the closed allowlist so a
	// new namespace joins this fixture automatically.
	for _, ns := range notes.ExternalIDNamespaces() {
		vocab = append(vocab, notes.TagCount{Tag: ns + ":86a1b2c3d", Count: 1})
	}

	offered := attrValues(renderString(t, tagFilterRow(vocab, nil)), "data-tag-filter")
	for _, tag := range offered {
		if notes.IsExternalIDTag(tag) {
			t.Errorf("the filter row offers %q — an external-id tag names ONE task, so its chip can "+
				"only narrow the board to a task you must already be looking at. It belongs on the "+
				"card, not in the filter row.", tag)
		}
	}
	for _, want := range []string{"backend", "runbook:deploy", "ui"} {
		if !contains(offered, want) {
			t.Errorf("the filter row dropped %q — the exclusion is too wide. Offered: %v", want, offered)
		}
	}
}

// TestAnActiveExternalIDTagStillGetsAChip is the asymmetry, and it is the half
// that keeps the exclusion safe: a filter you cannot see is a filter you cannot
// clear, and the board would look mysteriously empty with no control to fix it.
//
// ⚠ INVARIANT GUARD, NOT REGRESSION COVERAGE — green at origin/main, which
// offered every tag and so offered this one too. Mutation-checked: applying the
// exclusion to the ACTIVE loop as well fails THIS test (and only this one).
func TestAnActiveExternalIDTagStillGetsAChip(t *testing.T) {
	for _, ns := range notes.ExternalIDNamespaces() {
		active := ns + ":86a1b2c3d"
		html := renderString(t, tagFilterRow([]notes.TagCount{{Tag: "ui", Count: 1}}, []string{active}))
		if !contains(attrValues(html, "data-tag-filter"), active) {
			t.Errorf("%q is the ACTIVE filter and has no chip — there is no way to turn it off:\n%s", active, html)
		}
		if !strings.Contains(html, `data-tag-filter="`+active+`" aria-pressed="true"`) {
			t.Errorf("the active chip for %q does not render as pressed:\n%s", active, html)
		}
	}
}

// --- Defect 7: the runbook Run disclosure -------------------------------------

// --- Defect 8: raw markdown in an agent name ----------------------------------

// TestBothSurfacesRenderAnAgentNameTheSameWay is the regression guard for the
// measured defect, and it is a RELATIONSHIP guard rather than two independent
// ones: the agents LIST showed a name's raw `**` and backticks while the DETAIL
// header rendered the same string as markup. Neither surface was wrong on its
// own — they DISAGREED, and only a test that renders both can see that.
//
// Asserting they are EQUAL (rather than that each contains some expected text)
// is what makes it unwalkable: changing the treatment on one surface reds this
// until the other follows.
func TestBothSurfacesRenderAnAgentNameTheSameWay(t *testing.T) {
	const name = "**fix** the `chip` row"
	const want = "fix the chip row"

	card := renderString(t, agentCard(AgentCardView{ID: 1, Name: "agent-1", DisplayName: name, Status: "running"}))
	// spanContaining, not between(): between() would start mid-ATTRIBUTE and the
	// rest of the class string would read as body text. This backs up to the
	// <span> element itself.
	cardTitle := textOf(spanContaining(card, "text-base font-semibold leading-tight"))

	// ⚠ BOTH Features SHAPES, AND THE REASON IS THE EXTRACTION RATHER THAN THE
	// ASSERTION. between() takes the FIRST <h1>, and the sidebar — whose contents
	// DO vary with Features — is rendered before the header on this document.
	// Today AgentDetailPage has exactly one <h1> (the title) under either shape,
	// so the extraction is unambiguous; rendering both is what keeps that true if
	// a heading is ever added to the sidebar, instead of silently comparing the
	// wrong element under one of them.
	forEachFeatureShape(t, func(t *testing.T, shape string, feat Features) {
		// 🔴 documentSource, NOT renderString. Every document embeds appScript,
		// whose COMMENTS quote the markup they are about — including a literal
		// `<h1 id="page-heading">`. Counting `<h1` over the raw render therefore
		// reads prose as markup and reports two headings on a document that has
		// one; measured here, and it is the same trap helpers_test.go records
		// documentSource as existing for. Stripping script comments first makes
		// both the count and the extraction claims about MARKUP.
		detail := documentSource(t, AgentDetailPage(AgentDetailView{Name: "agent-1", DisplayName: name, ID: 1}, feat))
		if n := strings.Count(detail, "<h1"); n != 1 {
			t.Fatalf("%s: the agent detail document has %d <h1> elements — this test extracts the "+
				"FIRST one and can no longer claim it is the title", shape, n)
		}
		detailTitle := textOf(between(detail, "<h1", "</h1>"))

		if cardTitle != detailTitle {
			t.Errorf("%s: the list and the detail page render one name two ways:\n  card:   %q\n  detail: %q",
				shape, cardTitle, detailTitle)
		}
		for surface, got := range map[string]string{"card": cardTitle, "detail": detailTitle} {
			if got != want {
				t.Errorf("%s, %s title = %q, want %q", shape, surface, got, want)
			}
			if strings.ContainsAny(got, "*`_#") {
				t.Errorf("%s, %s title still carries raw markdown markers: %q", shape, surface, got)
			}
		}
	})
}

// TestAnAgentNameCannotEmitMarkupOnEitherSurface. The name is agent-authored
// text; stripping markdown must not become a route for anything else to reach
// the browser as markup, and neither surface may emit an anchor — the card's
// title sits inside the card's own <a>, where a nested one is invalid HTML that
// browsers resolve by closing the outer link.
//
// ⚠ INVARIANT GUARD, NOT REGRESSION COVERAGE — green at 8700b02 too, where the
// card was plain text and the detail header's inline renderer emitted no anchor
// for this input either. Mutation-checked instead: rendering the stripped name
// with g.Raw instead of g.Text fails THIS test (and only this one) on the raw
// tag.
func TestAnAgentNameCannotEmitMarkupOnEitherSurface(t *testing.T) {
	const name = "<img src=x onerror=alert(1)> see https://example.test/p/1"

	card := renderString(t, agentCard(AgentCardView{ID: 1, Name: "agent-1", DisplayName: name, Status: "running"}))
	cardTitle := spanContaining(card, "text-base font-semibold leading-tight")

	forEachFeatureShape(t, func(t *testing.T, shape string, feat Features) {
		// documentSource for the same reason as the test above: appScript's
		// comments quote markup, and this assertion is about markup.
		detail := documentSource(t, AgentDetailPage(AgentDetailView{Name: "agent-1", DisplayName: name, ID: 1}, feat))
		detailTitle := between(detail, "<h1", "</h1>")

		for surface, frag := range map[string]string{"card": cardTitle, "detail": detailTitle} {
			if strings.Contains(frag, "<img") {
				t.Errorf("%s, %s title emitted a raw tag:\n%s", shape, surface, frag)
			}
			if strings.Contains(frag, "<a ") || strings.Contains(frag, "<a href") {
				t.Errorf("%s, %s title emitted an anchor:\n%s", shape, surface, frag)
			}
		}
	})
}

// --- Defect 10: the duplicated registries -------------------------------------

// TestEachLazyRegistryIsMountedExactlyOnce is the regression guard for the
// runbooks (and privileges) list being rendered TWICE in one document: once in
// the Agents panel's "Advanced" disclosure and once as its own tab panel. Both
// mounts were live and both fetched the same partial.
//
// 🔴 THE DUPLICATE WAS NOT MERELY WASTE. Each runbook card's Delete and Dispatch
// targeted `#runbooks` BY ID, and the hidden copy owned that id — so acting on
// the visible list swapped the server's response into the invisible one. The
// write landed and the screen never changed, which reads as "the button does
// nothing". The actions now target `closest section`, a relationship a second
// mount cannot capture.
//
// 🔴 "AT MOST ONCE" IS SATISFIED BY ZERO, AND THAT IS WHY THIS RUNS UNDER BOTH
// Features SHAPES *AND* CARRIES A POSITIVE CONTROL. `/ui/repos` is mounted only
// when the GitHub store was built; under the shipping default the count is 0, so
// the `n > 1` test cannot fire for it and the row passes without observing
// anything. A zero and a one are indistinguishable to that assertion — so the
// control below demands the count actually MOVE to 1 on the shape that draws the
// panel, which is what makes the zero on the other shape a fact rather than a
// silence.
func TestEachLazyRegistryIsMountedExactlyOnce(t *testing.T) {
	// Mounted on every shell regardless of Features.
	always := []string{"/ui/agents", "/ui/tasks"}
	// Mounted only when its subsystem was built — the pair is (partial, the
	// Features predicate that draws it).
	gated := map[string]func(Features) bool{
		"/ui/repos": func(f Features) bool { return f.GitHub },
	}

	forEachFeatureShape(t, func(t *testing.T, shape string, feat Features) {
		for _, tab := range visibleTabs(feat) {
			doc := renderString(t, Page(tab.Key, feat))

			for _, partial := range always {
				n := strings.Count(doc, `hx-get="`+partial+`"`)
				if n > 1 {
					t.Errorf("%s, Page(%q) mounts %s %d times — two live mounts of one list fetch it "+
						"twice and, worse, split the id their in-list buttons target", shape, tab.Key, partial, n)
				}
				if n == 0 {
					t.Errorf("%s, Page(%q) does not mount %s at all — this guard would then pass "+
						"without observing anything. Either the mount moved (update `always`) or it "+
						"was lost.", shape, tab.Key, partial)
				}
			}

			for partial, drawn := range gated {
				n := strings.Count(doc, `hx-get="`+partial+`"`)
				switch {
				case n > 1:
					t.Errorf("%s, Page(%q) mounts %s %d times", shape, tab.Key, partial, n)
				case drawn(feat) && n != 1:
					// POSITIVE CONTROL: on the shape that DOES draw it, the count must
					// be exactly 1. Without this the zero above proves nothing.
					t.Errorf("%s, Page(%q) mounts %s %d times, want exactly 1 — this shape builds "+
						"the subsystem, so a 0 here means the guard's other shape is comparing "+
						"against a panel that is never drawn at all", shape, tab.Key, partial, n)
				case !drawn(feat) && n != 0:
					t.Errorf("%s, Page(%q) mounts %s %d times, want 0 — this shape did not build "+
						"the subsystem, so the panel must not be drawn over nothing",
						shape, tab.Key, partial, n)
				}
			}
		}
	})
}

// --- Defect 11: the empty chat transcript -------------------------------------

// TestAnEmptyChatTranscriptSaysSo: #chat-log is a flex-1 box filling the viewport,
// so an empty thread rendered as a tall blank panel whose only content was the
// `hidden` working indicator.
func TestAnEmptyChatTranscriptSaysSo(t *testing.T) {
	empty := renderString(t, chatLogInner(AgentDetailView{Name: "demo", Status: "running"}))
	if !strings.Contains(empty, "data-chat-empty") {
		t.Errorf("an empty transcript renders no empty state:\n%s", empty)
	}

	// With a message, the empty state must be gone — an empty state beside content
	// is worse than none.
	full := renderString(t, chatLogInner(AgentDetailView{
		Name:     "demo",
		Status:   "running",
		Messages: []ChatLine{{Role: "user", Content: "hello"}},
	}))
	if strings.Contains(full, "data-chat-empty") {
		t.Errorf("the empty state rendered beside an actual message:\n%s", full)
	}

	// While PROVISIONING, the provisioning row owns the screen — two "nothing here
	// yet" messages at once would contradict each other.
	prov := renderString(t, chatLogInner(AgentDetailView{Name: "demo", Status: "provisioning"}))
	if strings.Contains(prov, "data-chat-empty") {
		t.Errorf("the empty state rendered beside the provisioning indicator:\n%s", prov)
	}
	if !strings.Contains(prov, "chat-provisioning") {
		t.Fatalf("the provisioning indicator is missing — the fixture is wrong, not the code:\n%s", prov)
	}
}

// TestTheEmptyStateCountsRenderedBubblesNotRows is the case chatLogInner's own
// comment names as unreachable-but-open: an assistant sentinel is DROPPED by the
// render loop, so a thread made only of sentinels has rows but no bubbles. A
// condition on len(v.Messages) would leave that thread blank with no affordance.
func TestTheEmptyStateCountsRenderedBubblesNotRows(t *testing.T) {
	got := renderString(t, chatLogInner(AgentDetailView{
		Name:     "demo",
		Status:   "running",
		Messages: []ChatLine{{Role: "assistant", Content: "NO_REPLY"}},
	}))
	if !strings.Contains(got, "data-chat-empty") {
		t.Errorf("a thread whose only row renders NO bubble got no empty state — it is a blank "+
			"panel with no affordance:\n%s", got)
	}
}

// --- helpers ------------------------------------------------------------------

// forEachFeatureShape runs fn once per ui.Features shape a document can be
// rendered with.
//
// 🔴 A SUITE WHOSE FIXTURE PINS A DIMENSION IS STRUCTURALLY BLIND TO THAT
// DIMENSION'S BUGS, AND Features IS SUCH A DIMENSION. It decides whether the
// Repos tab, its panel and its entries in the browser's tab registry are DRAWN AT
// ALL (see features.go). Every guard below that sweeps a document — for a class
// pair, for an undefined call, for a duplicated mount — would therefore be
// looking at a different, smaller document under the zero value, and any defect
// living only in the repos surface would pass vacuously.
//
// 🔴 AND THE ZERO VALUE IS THE ONE THAT DRAWS LESS, WHICH IS THE DANGEROUS
// DIRECTION FOR A SWEEP. `Features{}` is the shipping default (the GitHub store
// is built only when an encryption key is set), so the value a test reaches for
// by reflex is exactly the one that hides the surface. Picking it and moving on
// is how a green tick comes to mean "I asserted about a document that no longer
// contains the thing".
//
// Both shapes, named, every time. A guard that genuinely cannot vary with
// Features costs one extra render to say so.
func forEachFeatureShape(t *testing.T, fn func(t *testing.T, shape string, feat Features)) {
	t.Helper()
	for _, tc := range []struct {
		shape string
		feat  Features
	}{
		// Named by what they DRAW, not by the field, so a second feature added to
		// the struct does not make these names lie.
		{"no GitHub store (the shipping default)", Features{}},
		{"GitHub store built", Features{GitHub: true}},
	} {
		tc := tc
		t.Run(tc.shape, func(t *testing.T) { fn(t, tc.shape, tc.feat) })
	}
}

// shellDocuments renders every document shape this package serves, under one
// Features value. Keyed by a human name so a failure says which document.
//
// ⚠ THE TAB LOOP GOES THROUGH visibleTabs, NOT musterTabs. Page() clamps an
// invisible tab to the default (normalizeVisibleTab), so asking for "repos" with
// no GitHub store silently renders the TASKS shell — two entries of the same
// document under different names, and a guard that thought it had covered the
// repos shell when it had not.
func shellDocuments(t *testing.T, feat Features) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, tab := range visibleTabs(feat) {
		out["shell /"+tab.Key] = renderString(t, Page(tab.Key, feat))
	}
	out["agent detail"] = renderString(t, AgentDetailPage(AgentDetailView{Name: "demo", ID: 1}, feat))
	out["task not found"] = renderString(t, TaskNotFoundPage("42", feat))
	return out
}

var (
	classAttrRe = regexp.MustCompile(`class="([^"]*)"`)
	slateShade  = regexp.MustCompile(`^text-slate-(\d+)$`)
)

// classAttrs returns the value of every class attribute in a rendered document.
func classAttrs(html string) []string {
	var out []string
	for _, m := range classAttrRe.FindAllStringSubmatch(html, -1) {
		out = append(out, m[1])
	}
	return out
}

// attrValues returns the value of every `name="..."` attribute in a fragment, in
// document order.
func attrValues(html, name string) []string {
	re := regexp.MustCompile(regexp.QuoteMeta(name) + `="([^"]*)"`)
	var out []string
	for _, m := range re.FindAllStringSubmatch(html, -1) {
		out = append(out, m[1])
	}
	return out
}

// hasClass reports whether a class attribute contains cls as a whole token
// (so `max-w-xl` does not match inside `sm:max-w-xl`).
func hasClass(attr, cls string) bool {
	for _, c := range strings.Fields(attr) {
		if c == cls {
			return true
		}
	}
	return false
}

// classOf returns the class attribute of the first element in a fragment.
func classOf(fragment string) string {
	m := classAttrRe.FindStringSubmatch(fragment)
	if m == nil {
		return ""
	}
	return m[1]
}

// between returns the text from the first occurrence of start through the first
// following end, or "" if either is absent.
func between(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	rest := s[i:]
	j := strings.Index(rest, end)
	if j < 0 {
		return ""
	}
	return rest[:j+len(end)]
}

// spanContaining returns the <span …> element whose markup contains needle.
func spanContaining(html, needle string) string {
	i := strings.Index(html, needle)
	if i < 0 {
		return ""
	}
	open := strings.LastIndex(html[:i+len(needle)], "<span")
	if open < 0 {
		return ""
	}
	rest := html[open:]
	j := strings.Index(rest, "</span>")
	if j < 0 {
		return rest
	}
	return rest[:j+len("</span>")]
}

// textOf returns the visible text of an HTML fragment: tags removed, entities
// for the five escaped characters decoded, whitespace collapsed. Comparing TEXT
// is what lets one assertion span two surfaces whose surrounding markup
// legitimately differs (a <span> in a card, an <h1> on a page).
func textOf(fragment string) string {
	var b strings.Builder
	depth := 0
	for _, r := range fragment {
		switch {
		case r == '<':
			depth++
		case r == '>':
			if depth > 0 {
				depth--
			}
		case depth == 0:
			b.WriteRune(r)
		}
	}
	out := b.String()
	for _, e := range [][2]string{{"&amp;", "&"}, {"&lt;", "<"}, {"&gt;", ">"}, {"&quot;", `"`}, {"&#39;", "'"}} {
		out = strings.ReplaceAll(out, e[0], e[1])
	}
	return strings.TrimSpace(regexp.MustCompile(`\s+`).ReplaceAllString(out, " "))
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
