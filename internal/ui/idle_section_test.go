package ui

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

// The clock every case is read against, and the threshold: distinct from the
// default (14d) so a predicate that ignores its argument is caught.
var (
	idleNow   = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	idleAfter = 10 * 24 * time.Hour
)

// TestInIdleSection is the rule, at both sides of the boundary and exactly on
// it, with pins, unknown activity and the section off. Every activity time is
// pairwise distinct and none is a multiple of the threshold from now except
// the boundary-exact case.
func TestInIdleSection(t *testing.T) {
	for _, c := range []struct {
		name   string
		at     time.Time
		pinned bool
		after  time.Duration
		want   bool
	}{
		{"active 3 minutes ago", idleNow.Add(-3 * time.Minute), false, idleAfter, false},
		{"idle 9d23h (inside)", idleNow.Add(-(idleAfter - time.Hour)), false, idleAfter, false},
		{"idle EXACTLY the threshold (not past it)", idleNow.Add(-idleAfter), false, idleAfter, false},
		{"idle the threshold and one second", idleNow.Add(-idleAfter - time.Second), false, idleAfter, true},
		{"idle 41 days", idleNow.Add(-41*24*time.Hour - 7*time.Minute), false, idleAfter, true},
		{"idle 41 days but PINNED", idleNow.Add(-41*24*time.Hour - 11*time.Minute), true, idleAfter, false},
		{"activity unknown (a host that did not say)", time.Time{}, false, idleAfter, false},
		{"idle 41 days, section OFF", idleNow.Add(-41*24*time.Hour - 13*time.Minute), false, 0, false},
		// A threshold that is not the one above: 11 days is idle at 10d, not at 12d.
		{"idle 11 days, threshold 12 days", idleNow.Add(-11*24*time.Hour - 17*time.Minute), false, 12 * 24 * time.Hour, false},
		{"active in the future (clock skew)", idleNow.Add(5 * time.Minute), false, idleAfter, false},
	} {
		if got := InIdleSection(idleNow, c.at, c.pinned, c.after); got != c.want {
			t.Errorf("%s: InIdleSection = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestMarkIdleReadsWhatTheCardPrints: an agent's mark is decided by the time its
// card prints (LastActiveAt, or UpdatedAt when the view has none) — and a
// window's by its ActiveAt; a reactivated entry (its time moved to now) is
// unmarked by the next build, with the same clock.
func TestMarkIdleReadsWhatTheCardPrints(t *testing.T) {
	old := idleNow.Add(-30 * 24 * time.Hour)
	agents := []AgentCardView{
		{ID: 1, Name: "by-last-active", LastActiveAt: old, UpdatedAt: idleNow.Add(-time.Minute)},
		{ID: 2, Name: "by-updated", UpdatedAt: old.Add(-time.Hour)},
		{ID: 3, Name: "fresh", LastActiveAt: idleNow.Add(-2 * time.Hour), UpdatedAt: old},
	}
	mirrors := []MirrorCardView{
		{Host: "h", Session: "old", ActiveAt: old.Add(-2 * time.Hour)},
		{Host: "h", Session: "pinned", ActiveAt: old.Add(-3 * time.Hour), Pinned: true},
		{Host: "h", Session: "fresh", ActiveAt: idleNow.Add(-4 * time.Minute)},
	}
	MarkIdle(agents, mirrors, idleNow, idleAfter)
	got := []bool{agents[0].Idle, agents[1].Idle, agents[2].Idle, mirrors[0].Idle, mirrors[1].Idle, mirrors[2].Idle}
	want := []bool{true, true, false, true, false, false}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d: Idle = %v, want %v (all: %v)", i, got[i], want[i], got)
		}
	}
	// Reactivated: the window is active again; the next build unmarks it.
	mirrors[0].ActiveAt = idleNow.Add(-time.Minute)
	MarkIdle(agents, mirrors, idleNow, idleAfter)
	if mirrors[0].Idle {
		t.Error("a window active again is still marked idle by the next build")
	}
}

// articleOrHead is each card's id and the Idle header, in document order.
var articleOrHead = regexp.MustCompile(`<article id="([^"]+)"([^>]*)>|<div id="(agents-idle-head)"`)

// TestTheListDrawsTheIdleSectionLast: the working entries (pins first, then by
// activity), then the header with the count, then the idle entries — each idle
// card carrying data-idle-section, no working card carrying it.
func TestTheListDrawsTheIdleSectionLast(t *testing.T) {
	old := idleNow.Add(-30 * 24 * time.Hour)
	agents := []AgentCardView{
		{ID: 1, Name: "idle-agent", Status: "running", LastActiveAt: old.Add(-5 * time.Hour)},
		{ID: 2, Name: "busy-agent", Status: "running", LastActiveAt: idleNow.Add(-9 * time.Minute)},
		{ID: 3, Name: "pinned-old-agent", Status: "stopped", LastActiveAt: old.Add(-6 * time.Hour), Pinned: true},
	}
	mirrors := []MirrorCardView{
		{Host: "h", Session: "w-old", Window: "@1", Online: true, ActiveAt: old.Add(-time.Hour)},
		{Host: "h", Session: "w-new", Window: "@2", Online: true, ActiveAt: idleNow.Add(-2 * time.Minute)},
	}
	MarkIdle(agents, mirrors, idleNow, idleAfter)
	html := renderNode(t, agentsListBody(agents, mirrors, idleAfter))
	var order []string
	idleAttr := map[string]bool{}
	for _, m := range articleOrHead.FindAllStringSubmatch(html, -1) {
		if m[3] != "" {
			order = append(order, "HEAD")
			continue
		}
		order = append(order, m[1])
		idleAttr[m[1]] = strings.Contains(m[2], "data-idle-section")
	}
	wOld, wNew := mirrorCardID(mirrors[0]), mirrorCardID(mirrors[1])
	want := []string{"agent-3", wNew, "agent-2", "HEAD", wOld, "agent-1"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("list order = %v, want %v", order, want)
	}
	for id, idle := range idleAttr {
		if wantIdle := id == wOld || id == "agent-1"; idle != wantIdle {
			t.Errorf("%s: data-idle-section = %v, want %v", id, idle, wantIdle)
		}
	}
	head := regexp.MustCompile(`(?s)<div id="agents-idle-head".*?</button>`).FindString(html)
	for _, want := range []string{`data-idle-count="" class="tabular-nums">(2)</span>`, `aria-expanded="false"`, "no activity in over 10 days"} {
		if !strings.Contains(head, want) {
			t.Errorf("the header lacks %q:\n%s", want, head)
		}
	}
	// Control: with the section off (0), nothing is marked and no header is drawn.
	MarkIdle(agents, mirrors, idleNow, 0)
	off := renderNode(t, agentsListBody(agents, mirrors, 0))
	if strings.Contains(off, "agents-idle-head") || strings.Contains(off, "data-idle-section") {
		t.Error("the section is drawn with the threshold 0 (off)")
	}
}

// TestTheIdleSectionStartsCollapsed: the server draws no open state — the
// stylesheet hides idle cards unless <html> carries data-agents-idle-open
// (set from localStorage by the head script before the first paint) or the
// search box has a term — and places them after the header.
func TestTheIdleSectionStartsCollapsed(t *testing.T) {
	page := renderString(t, Page("agents", featuresAllOn))
	if strings.Contains(page, "data-agents-idle-open") && !strings.Contains(scriptWith(t, page, "__musterAgentsRules ="), "data-agents-idle-open") {
		t.Error("the server draws the Idle section open")
	}
	head := scriptWith(t, page, "__musterAgentsRules =")
	for _, want := range []string{"'" + agentsIdleOpenKey + "'", "toggleAttribute('data-agents-idle-open'", "toggleAttribute('data-agents-searching'"} {
		if !strings.Contains(head, want) {
			t.Errorf("the head script does not set the section's state before the first paint: missing %s", want)
		}
	}
}

func TestParseIdleSectionAfter(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"": DefaultIdleSectionAfter, "0": 0, "off": 0, "OFF": 0, "14d": 14 * 24 * time.Hour, " 3d ": 72 * time.Hour,
		"36h": 36 * time.Hour, "1h": time.Hour, "365d": 365 * 24 * time.Hour,
	} {
		got, err := ParseIdleSectionAfter(in)
		if err != nil || got != want {
			t.Errorf("ParseIdleSectionAfter(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"59m", "366d", "-1d", "1.5d", "fortnight", "14"} {
		if got, err := ParseIdleSectionAfter(bad); err == nil {
			t.Errorf("ParseIdleSectionAfter(%q) = %v, want an error", bad, got)
		}
	}
	for d, want := range map[time.Duration]string{14 * 24 * time.Hour: "14 days", 24 * time.Hour: "1 day", 36 * time.Hour: "36 hours", time.Hour: "1 hour", 90 * time.Minute: "1h30m0s"} {
		if got := IdleSectionAfterLabel(d); got != want {
			t.Errorf("IdleSectionAfterLabel(%s) = %q, want %q", d, got, want)
		}
	}
}
