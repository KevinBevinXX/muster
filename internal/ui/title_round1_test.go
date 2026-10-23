package ui

import (
	"strings"
	"testing"
)

// TestATitleThatIsAllMarkupFallsBack: a display name that the title rule
// reduces to nothing ("<b></b>") does not draw an empty heading; each sink falls
// back to its identifier, AS WRITTEN — the agent's slug, the window's raw tmux
// session name. Red before: the card, the <h1>, the <title>, the terminal and
// the recents entry all drew "" for the name.
func TestATitleThatIsAllMarkupFallsBack(t *testing.T) {
	for _, c := range []struct {
		name, want, out string
	}{
		{"agent card", ">agent_17<", renderString(t, agentCard(AgentCardView{ID: 17, Name: "agent_17", Status: "running", DisplayName: "<b></b>"}))},
		{"agent detail <title>", "<title>agent_18 · muster</title>", renderString(t, AgentDetailPage(AgentDetailView{Name: "agent_18", Status: "running", DisplayName: "<b></b>"}, Features{}))},
		{"agent detail <h1>", ">agent_19</h1>", renderString(t, agentTitle(AgentDetailView{Name: "agent_19", Status: "running", DisplayName: "<i> </i>"}))},
		{"terminal <title>", "<title>Terminal · agent_20 · muster</title>", renderString(t, AgentTerminalPage(AgentTerminalView{Name: "agent_20", DisplayName: "<b></b>"}))},
		{"mirror card", ">_s21_<", renderString(t, mirrorCard(MirrorCardView{Host: "h", Session: "_s21_", Label: "<b></b>", Online: true}))},
		{"recents", ">agent_22<", renderString(t, recentStrip(recentItems([]AgentCardView{{ID: 22, Name: "agent_22", DisplayName: "<b></b>"}}, nil, []RecentRef{{AgentID: 22}}), false))},
	} {
		if n := strings.Count(c.out, c.want); n < 1 {
			t.Errorf("%s: %q drawn %d time(s), want ≥1 (the fallback, as written)", c.name, c.want, n)
		}
	}
}

// TestTheRawSessionNameIsNeverPlained: a mirrored window with no label is
// called by its raw tmux session name — what the operator types into
// `tmux attach -t` — so the title rule must not touch it: "_scratch_" is not
// italic "scratch". Red before: mirrorEntryTitle plained the session name too.
func TestTheRawSessionNameIsNeverPlained(t *testing.T) {
	out := renderString(t, mirrorEntryTitle("", "", "_scratch_", "", ""))
	if !strings.Contains(out, ">_scratch_<") {
		t.Errorf("the raw session name was changed: %s", out)
	}
	// With a label that plains to nothing, the session name stands in, raw.
	out = renderString(t, mirrorEntryTitle("<b></b>", "", "_scratch_", "", ""))
	if !strings.Contains(out, ">_scratch_<") {
		t.Errorf("an all-markup label did not fall back to the raw session name: %s", out)
	}
	// And a real label is still plained, with the session beside it as written.
	out = renderString(t, mirrorEntryTitle("**Build** box", "", "_scratch_", "", ""))
	if !strings.Contains(out, ">Build box<") || !strings.Contains(out, ">_scratch_<") {
		t.Errorf("label/session: %s", out)
	}
}
