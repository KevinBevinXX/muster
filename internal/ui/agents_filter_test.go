package ui

import (
	"regexp"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

var dataFilter = regexp.MustCompile(` data-filter="([^"]*)"`)

func haystackOf(t *testing.T, card string) string {
	t.Helper()
	m := dataFilter.FindStringSubmatch(card)
	if m == nil {
		t.Fatalf("the card carries no data-filter:\n%s", card)
	}
	return m[1]
}

// TestEveryCardCanBeFoundByWhatItIs: each card carries, lower-cased, what the
// filter matches it by. A mirrored window: its label ("Gold:8"), its raw tmux
// session name, its host, what runs in it, and "mirrored". An agent: its
// display name, its slug, its repository, its kind, its account, its status.
// The values are pairwise distinct, so a card found by the wrong field fails.
func TestEveryCardCanBeFoundByWhatItIs(t *testing.T) {
	win := haystackOf(t, renderNode(t, mirrorCard(MirrorCardView{Host: "Tower", Session: "Scratch2", Window: "@7", Label: "Gold:8",
		Kind: "opencode", Online: true, Interactive: true})))
	for _, want := range []string{"gold:8", "scratch2", "tower", "opencode", "mirrored", "interactive"} {
		if !strings.Contains(win, want) {
			t.Errorf("a mirrored window's haystack %q lacks %q", win, want)
		}
	}
	if win != strings.ToLower(win) || strings.Contains(win, "offline") {
		t.Errorf("the haystack is not lower-case, or calls an online card offline: %q", win)
	}
	// A Claude Code window is found by "claude code" whatever its command is, an
	// offline one by "offline", and a window with no codename by "<session>:<index>".
	cc := haystackOf(t, renderNode(t, mirrorCard(MirrorCardView{Host: "tower", Session: "solo", Window: "@2", Title: "solo:3", Kind: "node", Claude: true})))
	for _, want := range []string{"claude code", "solo:3", "offline"} {
		if !strings.Contains(cc, want) {
			t.Errorf("the haystack %q lacks %q", cc, want)
		}
	}
	if strings.Contains(cc, "node") {
		t.Errorf("a Claude Code window is filed under its command: %q", cc)
	}

	agent := haystackOf(t, renderNode(t, agentCard(AgentCardView{ID: 7, Name: "keen-wren", DisplayName: "#12 · Fix The Chip", Repo: "Acme/Widgets",
		Kind: agents.KindClaudeCode, CCAccount: "Alpha", Status: "running"})))
	for _, want := range []string{"#12 · fix the chip", "keen-wren", "acme/widgets", "claude code", "alpha", "running", "agent"} {
		if !strings.Contains(agent, want) {
			t.Errorf("an agent's haystack %q lacks %q", agent, want)
		}
	}
	if strings.Contains(agent, "mirrored") {
		t.Errorf("an agent is filed as mirrored: %q", agent)
	}
}

// TestTheFilterBoxIsOutsideTheListItFilters: the search box, its clear button
// and its count are in the Agents panel but NOT inside #agents-list — the list
// re-renders itself (an htmx morph) on every nudge, and an input inside it
// would lose its focus and caret each time. And the box is phone-shaped: a 16px
// input (iOS zooms the page on focusing a smaller one) and 44px targets.
func TestTheFilterBoxIsOutsideTheListItFilters(t *testing.T) {
	panel := renderNode(t, AgentsPanel(true))
	list := strings.Index(panel, `id="agents-list"`)
	for _, id := range []string{"agents-search", "agents-search-clear", "agents-count", "agents-nomatch"} {
		i := strings.Index(panel, `id="`+id+`"`)
		if i < 0 {
			t.Fatalf("the Agents panel has no #%s", id)
		}
		if i > list {
			t.Errorf("#%s is drawn after #agents-list opens — inside or below what a refresh re-renders", id)
		}
	}
	tag := func(id string) string {
		i := strings.Index(panel, `id="`+id+`"`)
		start := strings.LastIndex(panel[:i], "<")
		return panel[start : i+strings.Index(panel[i:], ">")]
	}
	input := tag("agents-search")
	for _, want := range []string{" text-base ", "min-h-[44px]", `aria-controls="agents-list"`, `type="text"`} {
		if !strings.Contains(input, want) {
			t.Errorf("the filter input lacks %q: %s", want, input)
		}
	}
	if strings.Contains(input, "text-sm") || strings.Contains(input, "text-xs") {
		t.Errorf("the filter input is smaller than 16px: %s", input)
	}
	// The clear button is hidden by ATTRIBUTE until there is something to
	// clear, so it must carry no display utility that would beat it.
	clear := tag("agents-search-clear")
	if !strings.Contains(clear, ` hidden=""`) {
		t.Errorf("the clear button is not hidden while the box is empty: %s", clear)
	}
	for _, display := range []string{"inline-flex", " flex ", " grid ", " block "} {
		if strings.Contains(clear, display) {
			t.Errorf("the clear button carries %q, which beats its hidden attribute: %s", display, clear)
		}
	}
	if !strings.Contains(clear, "h-11") || !strings.Contains(clear, "w-11") {
		t.Errorf("the clear button is not a 44px target: %s", clear)
	}
}

// TestTheFilterHidesWithOneRulePerWordAndCountsWithTheSameSelector pins the two
// decisions the script is written around, where a rewrite would lose them:
// the hiding is a stylesheet rule over data-filter (so the list's own refresh
// cannot undo it), and the count is taken with the SAME selector fragment (so
// what is counted is what is shown).
func TestTheFilterHidesWithOneRulePerWordAndCountsWithTheSameSelector(t *testing.T) {
	page := renderString(t, Page("agents", featuresAllOn))
	script := scriptWith(t, page, "__musterAgentsFilter")
	for name, want := range map[string]string{
		"the rule hides a card lacking the term": `'#agents-list > article:not(' + sel(t) + '){display:none !important}'`,
		"the count uses the same selector":       `list.querySelectorAll(':scope > article:not([data-held])' + ts.map(sel).join(''))`,
		"a held arrival is hidden and uncounted": `'#agents-list > article[data-held]{display:none !important}'`,
		"the terms are lower-cased and parsed":   `tokens(String(q || '').toLowerCase()).forEach(`,
		"typing replaces the URL, never pushes":  `history.replaceState(history.state, '', next)`,
		"it re-counts when the list settles":     `if (e.target && e.target.id === 'agents-list') settled();`,
		"it remembers the text for the session":  `sessionStorage.setItem(KEY, q)`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("%s: the filter script no longer contains %s", name, want)
		}
	}
	if strings.Count(page, "__musterAgentsFilter = true") != 1 {
		t.Error("the filter script is not rendered exactly once on the shell")
	}
	// It never marks a card itself: nothing for a morph to undo.
	if strings.Contains(script, ".hidden = true") || strings.Contains(script, "setAttribute('hidden'") {
		t.Error("the filter script hides cards one by one; a list refresh would un-hide them")
	}
}

var dataField = regexp.MustCompile(` data-f-([a-z][a-z0-9_]*)="([^"]*)"`)

// fieldsOf is every data-f-<field> attribute on a card's own <article> tag.
func fieldsOf(t *testing.T, card string) map[string]string {
	t.Helper()
	end := strings.Index(card, ">")
	if end < 0 {
		t.Fatalf("not a tag: %s", card)
	}
	out := map[string]string{}
	for _, m := range dataField.FindAllStringSubmatch(card[:end], -1) {
		if _, dup := out[m[1]]; dup {
			t.Errorf("data-f-%s is written twice: %s", m[1], card[:end])
		}
		out[m[1]] = m[2]
	}
	return out
}

func sameFields(t *testing.T, what string, got, want map[string]string) {
	t.Helper()
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s: data-f-%s = %q, want %q", what, k, got[k], w)
		}
	}
	for k, v := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("%s: unexpected data-f-%s = %q", what, k, v)
		}
	}
}

// TestEveryCardCarriesEachFieldInItsOwnAttribute: besides the free-text
// haystack, a card carries ONE attribute per field, so `host:tower` can ask
// about the host alone. Every fixture value is pairwise distinct (and none is
// the literal "cluster"), so a field filled from another field — or from the
// whole haystack — fails here by name.
//
// 🔴 A DISPATCHED POD AGENT'S HOST IS THE LITERAL "cluster": muster has no
// cluster name to put there, and naming it after a machine would merge "my
// tmux on that machine" with "pods on that cluster" — the very cut host:
// exists to make.
func TestEveryCardCarriesEachFieldInItsOwnAttribute(t *testing.T) {
	agent := renderNode(t, agentCard(AgentCardView{ID: 7, Name: "keen-wren", DisplayName: "#12 · Fix The Chip", Repo: "Acme/Widgets",
		Kind: agents.KindClaudeCode, CCAccount: "Alpha", Status: "running", Namespace: "Pods-Ns"}))
	sameFields(t, "a dispatched agent", fieldsOf(t, agent), map[string]string{
		"host": "cluster", "type": "agent", "name": "#12 · fix the chip keen-wren", "status": "running",
		"kind": "claude code", "repo": "acme/widgets", "account": "alpha", "ns": "pods-ns",
	})

	win := renderNode(t, mirrorCard(MirrorCardView{Host: "Tower", Session: "Scratch2", Window: "@7", Label: "Gold:8", Title: "Ignored:8",
		Kind: "opencode", Online: true, Interactive: true}))
	sameFields(t, "a mirrored window", fieldsOf(t, win), map[string]string{
		"host": "tower", "type": "mirrored", "name": "gold:8", "session": "scratch2", "status": "running",
		"kind": "opencode", "mode": "interactive",
	})

	// Offline, Claude Code, no label: its name is its title, it is stopped and
	// offline, and it is filed under Claude Code whatever its command is.
	cc := renderNode(t, mirrorCard(MirrorCardView{Host: "dock", Session: "solo", Window: "@2", Title: "solo:3", Kind: "node", Claude: true}))
	sameFields(t, "an offline Claude Code window", fieldsOf(t, cc), map[string]string{
		"host": "dock", "type": "mirrored", "name": "solo:3", "session": "solo", "status": "stopped",
		"kind": "claude code", "state": "offline",
	})

	// The free-text haystack is untouched by the fields: still exactly today's.
	if got, want := haystackOf(t, agent), "#12 · fix the chip keen-wren acme/widgets claude code alpha running agent"; got != want {
		t.Errorf("the agent's free-text haystack changed:\n got %q\nwant %q", got, want)
	}
	if got, want := haystackOf(t, win), "gold:8 ignored:8 gold:8 scratch2 tower opencode mirrored interactive"; got != want {
		t.Errorf("the mirrored window's free-text haystack changed:\n got %q\nwant %q", got, want)
	}
}

// TestEveryFieldIsARegisteredSafeIdentifier: the attribute NAME in a selector
// is built from the field name, so the set is closed (the registry) and every
// name is a plain lower-case identifier; and the script is handed exactly that
// set, so a field the cards carry is a field the box understands.
func TestEveryFieldIsARegisteredSafeIdentifier(t *testing.T) {
	ident := regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	seen := map[string]bool{}
	for _, f := range cardFieldRegistry {
		if !ident.MatchString(f.Key) {
			t.Errorf("field %q is not a safe CSS identifier", f.Key)
		}
		if seen[f.Key] {
			t.Errorf("field %q is registered twice", f.Key)
		}
		seen[f.Key] = true
	}
	for _, want := range []string{"host", "status", "kind", "repo", "name", "account", "session", "mode", "state", "type", "ns"} {
		if !seen[want] {
			t.Errorf("field %q is not registered", want)
		}
	}
	page := renderString(t, Page("agents", featuresAllOn))
	script := scriptWith(t, page, "__musterAgentsFilter")
	if !strings.Contains(script, "var FIELDS = "+cardFieldKeysJSON()+";") {
		t.Errorf("the filter script is not handed the registry's field set %s", cardFieldKeysJSON())
	}
	// Both card kinds carry the fields every card has, whatever optional ones
	// (repo, account, kind…) are empty. (An agent row always has a status.)
	for what, card := range map[string]string{
		"agent":  renderNode(t, agentCard(AgentCardView{ID: 1, Name: "a", Status: "pending"})),
		"mirror": renderNode(t, mirrorCard(MirrorCardView{Host: "h", Session: "s"})),
	} {
		got := fieldsOf(t, card)
		for _, k := range []string{"host", "type", "name", "status"} {
			if got[k] == "" {
				t.Errorf("a bare %s card has no data-f-%s: %v", what, k, got)
			}
		}
		for k := range got {
			if !seen[k] {
				t.Errorf("a %s card writes unregistered data-f-%s", what, k)
			}
		}
	}
}

// 🔴 A REGRESSION TEST: two `function place` declarations in the one IIFE —
// the later one (the box-token placer) silently replaced the card mover, and
// the reorder wrote the list element into the search box: every card hidden.
// Function declarations hoist and the last one wins, with no error anywhere.
func TestTheFilterScriptDeclaresEachFunctionOnce(t *testing.T) {
	var b strings.Builder
	if err := agentsFilterScript().Render(&b); err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, m := range regexp.MustCompile(`(?m)^  function (\w+)\s*\(`).FindAllStringSubmatch(b.String(), -1) {
		seen[m[1]]++
	}
	if len(seen) < 10 {
		t.Fatalf("found only %d top-level functions — the pattern no longer matches the script", len(seen))
	}
	for name, n := range seen {
		if n > 1 {
			t.Errorf("function %s is declared %d times in agentsFilterScript; the last silently wins", name, n)
		}
	}
}
