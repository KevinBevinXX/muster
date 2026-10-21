package ui

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"
)

// THE PREDICATES UNDER THE AGENTS LIST UX BATCH (list_order.go,
// list_signals.go), each pinned with literal expected values — never derived
// from the code under test. The rendered behaviour is in
// agents_list_ux_render_test.go.

// TestOrderListPinsFirstThenRecencyThenArrival exercises orderList's four
// rules one at a time, each on fixtures whose times are distinct except where
// the rule under test needs a tie.
func TestOrderListPinsFirstThenRecencyThenArrival(t *testing.T) {
	t0 := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	key := func(es []listEntry) string {
		var out []string
		for _, e := range es {
			if e.agent != nil {
				out = append(out, e.agent.Name)
			} else {
				out = append(out, e.mirror.Session)
			}
		}
		return strings.Join(out, ",")
	}
	for _, c := range []struct {
		name    string
		agents  []AgentCardView
		mirrors []MirrorCardView
		want    string
	}{
		{"flat: a window newer than every agent leads",
			[]AgentCardView{{Name: "a", LastActiveAt: at(30)}, {Name: "b", LastActiveAt: at(10)}},
			[]MirrorCardView{{Session: "w", ActiveAt: at(40)}, {Session: "x", ActiveAt: at(20)}},
			"w,a,x,b"},
		{"pins first, by recency among themselves, ahead of anything newer",
			[]AgentCardView{{Name: "a", LastActiveAt: at(50)}, {Name: "p", LastActiveAt: at(5), Pinned: true}},
			[]MirrorCardView{{Session: "q", ActiveAt: at(7), Pinned: true}, {Session: "w", ActiveAt: at(60)}},
			"q,p,w,a"},
		{"unknown activity after every known one, even a very old one",
			[]AgentCardView{{Name: "old", LastActiveAt: at(-100000)}},
			[]MirrorCardView{{Session: "unknown"}, {Session: "w", ActiveAt: at(1)}},
			"w,old,unknown"},
		{"a pinned entry with unknown activity is still above every unpinned one",
			[]AgentCardView{{Name: "a", LastActiveAt: at(9)}},
			[]MirrorCardView{{Session: "pu", Pinned: true}},
			"pu,a"},
		{"ties keep arrival order: agents before windows, each kind in its own order",
			[]AgentCardView{{Name: "a2", LastActiveAt: at(3)}, {Name: "a1", LastActiveAt: at(3)}},
			[]MirrorCardView{{Session: "w2", ActiveAt: at(3)}, {Session: "w1", ActiveAt: at(3)}},
			"a2,a1,w2,w1"},
		{"an agent with no LastActiveAt falls back to UpdatedAt",
			[]AgentCardView{{Name: "u", UpdatedAt: at(8)}},
			[]MirrorCardView{{Session: "w", ActiveAt: at(4)}},
			"u,w"},
	} {
		if got := key(orderList(c.agents, c.mirrors)); got != c.want {
			t.Errorf("%s: order = %s, want %s", c.name, got, c.want)
		}
	}
	// STABLE: sixty entries in three activity minutes, arriving interleaved —
	// more than the 12 a plain sort.Slice orders by insertion (stably, by
	// accident). Each minute's entries keep their arrival order, newest minute
	// first; the expectation is built by bucketing, not by sorting.
	var ag []AgentCardView
	var ms []MirrorCardView
	byMinute := map[int][]string{}
	for i := 0; i < 40; i++ {
		n := "a" + string(rune('A'+i%26)) + string(rune('a'+i/26))
		m := (i * 7) % 3
		ag = append(ag, AgentCardView{Name: n, LastActiveAt: at(m)})
		byMinute[m] = append(byMinute[m], n)
	}
	for i := 0; i < 20; i++ {
		n := "w" + string(rune('A'+i))
		m := (i * 5) % 3
		ms = append(ms, MirrorCardView{Session: n, ActiveAt: at(m)})
		byMinute[m] = append(byMinute[m], n)
	}
	want := strings.Join(append(append(byMinute[2], byMinute[1]...), byMinute[0]...), ",")
	for i := 0; i < 20; i++ {
		if got := key(orderList(ag, ms)); got != want {
			t.Fatalf("render %d: ties did not keep their arrival order:\n got %s\nwant %s", i, got, want)
		}
	}
}

// TestRecencyBucketsAreTheTmuxThresholds: tmux-idle-update.sh tests
// `idle_secs < 600`, `< 1800`, `< 3600`, `< 7200`, `< 14400`, `< 28800`,
// `< 86400`, else grey — so each boundary second is the NEXT bucket.
func TestRecencyBucketsAreTheTmuxThresholds(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, c := range []struct {
		secs int
		want int
	}{
		{0, 0}, {599, 0}, {600, 1}, {1799, 1}, {1800, 2}, {3599, 2}, {3600, 3}, {7199, 3}, {7200, 4},
		{14399, 4}, {14400, 5}, {28799, 5}, {28800, 6}, {86399, 6}, {86400, 7}, {30 * 86400, 7},
		{-120, 0}, // a clock ahead of ours: the freshest bucket
	} {
		if got := recencyBucket(now, now.Add(-time.Duration(c.secs)*time.Second)); got != c.want {
			t.Errorf("idle %ds: bucket %d, want %d", c.secs, got, c.want)
		}
	}
	if got := recencyBucket(now, time.Time{}); got != -1 {
		t.Errorf("unknown activity: bucket %d, want -1 (no dot)", got)
	}
	if n := len(recencySteps); n != 8 {
		t.Fatalf("%d buckets, want tmux's 8", n)
	}
}

// TestRecencyTokensAreTheTmuxPalette: each bucket's colour, in BOTH themes, is
// tmux-idle-update.sh's (spelled here literally, from that script), and each
// bucket's utility is the role token reading it.
func TestRecencyTokensAreTheTmuxPalette(t *testing.T) {
	tmux := []string{"#B8BB26", "#98971A", "#689D6A", "#D79921", "#D65D0E", "#CC241D", "#B16286", "#665C54"}
	dark, light := themes(t)
	for i, hex := range tmux {
		name := "idle-" + string(rune('0'+i))
		for theme, toks := range map[string]themeTokens{"dark": dark, "light": light} {
			c, ok := toks[name]
			if !ok {
				t.Errorf("%s theme has no --mu-%s", theme, name)
				continue
			}
			if c.hex() != hex {
				t.Errorf("%s --mu-%s = %s, want tmux's %s", theme, name, c.hex(), hex)
			}
		}
		if got, want := recencySteps[i].Class, "bg-"+name; got != want {
			t.Errorf("bucket %d's class = %s, want %s", i, got, want)
		}
	}
	// The script is handed the same classes, in the same order.
	var scale []struct {
		U int64  `json:"u"`
		C string `json:"c"`
	}
	if err := json.Unmarshal([]byte(recencyScaleJSON()), &scale); err != nil || len(scale) != 8 {
		t.Fatalf("recencyScaleJSON: %v (%d steps)", err, len(scale))
	}
	wantU := []int64{600000, 1800000, 3600000, 7200000, 14400000, 28800000, 86400000, 0}
	for i, s := range scale {
		if s.U != wantU[i] || !strings.HasSuffix(s.C, " bg-idle-"+string(rune('0'+i))) {
			t.Errorf("script step %d = %+v, want u=%d and class bg-idle-%d", i, s, wantU[i], i)
		}
	}
}

// TestIdleTokensAreStrictlyPastEachThreshold: "Idle >15m" means MORE than 15
// minutes; an unknown activity time is never idle.
func TestIdleTokensAreStrictlyPastEachThreshold(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, c := range []struct {
		idle time.Duration
		want string
	}{
		{0, ""}, {15 * time.Minute, ""}, {15*time.Minute + time.Second, "15m"},
		{time.Hour, "15m"}, {time.Hour + time.Second, "15m 1h"},
		{24 * time.Hour, "15m 1h"}, {24*time.Hour + time.Second, "15m 1h 1d"}, {90 * 24 * time.Hour, "15m 1h 1d"},
	} {
		if got := idleTokens(now, now.Add(-c.idle)); got != c.want {
			t.Errorf("idle %s: %q, want %q", c.idle, got, c.want)
		}
	}
	if got := idleTokens(now, time.Time{}); got != "" {
		t.Errorf("unknown activity: %q, want none", got)
	}
}

// TestWaitTokensFileEachAttentionKind: an agent's attention_ask and a session
// stopped at a prompt are "ask", and so is a dispatched agent awaiting approval
// (a permission prompt, carried beside the badge); an AskUserQuestion picker is
// "question"; a reply draft is neither.
func TestWaitTokensFileEachAttentionKind(t *testing.T) {
	for _, c := range []struct {
		kinds    []string
		awaiting bool
		want     string
	}{
		{nil, false, ""},
		{[]string{"ask"}, false, "ask"},
		{[]string{"session"}, false, "ask"},
		{[]string{"question"}, false, "question"},
		{[]string{"plugin"}, false, ""},
		{[]string{"question", "plugin", "session"}, false, "ask question"},
		{[]string{"ask", "session"}, false, "ask"},
		{nil, true, "ask"},
		{[]string{"question"}, true, "ask question"},
		{[]string{"plugin"}, true, "ask"},
	} {
		var a *CardAttention
		if c.kinds != nil {
			a = &CardAttention{Count: len(c.kinds), Kinds: c.kinds}
		}
		if got := waitTokens(a, c.awaiting); got != c.want {
			t.Errorf("kinds %v awaiting=%v: %q, want %q", c.kinds, c.awaiting, got, c.want)
		}
	}
}

// matchesChipSelector evaluates the selector grammar chipSelector writes — one
// `[data-<g>~="<t>"]` test, or several ORed in `:is(…)` — against a card's
// data-wait and data-idle. An independent reading of the CSS the browser
// runs; any other shape fails the test rather than being guessed at.
func matchesChipSelector(t *testing.T, sel, wait, idle string) bool {
	t.Helper()
	attr := map[string][]string{"wait": strings.Fields(wait), "idle": strings.Fields(idle)}
	has := func(part string) bool {
		m := regexp.MustCompile(`^\[data-(wait|idle)~="([a-z0-9]+)"\]$`).FindStringSubmatch(part)
		if m == nil {
			t.Fatalf("unparseable selector part %q in %q", part, sel)
		}
		for _, w := range attr[m[1]] {
			if w == m[2] {
				return true
			}
		}
		return false
	}
	if strings.HasPrefix(sel, ":is(") && strings.HasSuffix(sel, ")") {
		for _, p := range strings.Split(sel[4:len(sel)-1], ",") {
			if has(p) {
				return true
			}
		}
		return false
	}
	return has(sel)
}

// TestTheChipSelectorsOrTheChosenChips: the coordinator's rule — a card is
// shown when it matches ANY chosen chip — pinned on the SELECTORS the script
// looks up (chipSelectorsJSON, the rule's only statement), with literal cases.
// Positive control: the evaluator reads a hand-written selector both ways.
func TestTheChipSelectorsOrTheChosenChips(t *testing.T) {
	if !matchesChipSelector(t, `:is([data-wait~="ask"],[data-idle~="1h"])`, "", "15m 1h") ||
		matchesChipSelector(t, `:is([data-wait~="ask"],[data-idle~="1h"])`, "question", "15m") {
		t.Fatal("instrument check FAILED: the selector evaluator misreads a known selector")
	}
	var sels map[string]string
	if err := json.Unmarshal([]byte(chipSelectorsJSON()), &sels); err != nil {
		t.Fatal(err)
	}
	// 4 sets of wait chips × (no idle chip or one of 3), less the empty one.
	if len(sels) != 15 {
		t.Fatalf("%d selections, want 15: %v", len(sels), sels)
	}
	for key, want := range map[string]string{
		"ask":                  `[data-wait~="ask"]`,
		"idle-1d":              `[data-idle~="1d"]`,
		"ask,question,idle-1h": `:is([data-wait~="ask"],[data-wait~="question"],[data-idle~="1h"])`,
	} {
		if got := sels[key]; got != want {
			t.Errorf("selector for %s = %s, want %s", key, got, want)
		}
	}
	for _, c := range []struct {
		key, wait, idle string
		want            bool
	}{
		{"ask", "ask", "", true},
		{"ask", "question", "15m 1h 1d", false},
		{"question", "ask question", "", true},
		{"ask,question", "question", "", true},
		{"ask,question", "", "15m 1h 1d", false},
		{"idle-1h", "", "15m 1h", true},
		{"idle-1h", "", "15m", false},
		{"idle-1d", "ask", "15m", false},
		// OR across groups: waiting OR idle, either is enough.
		{"ask,idle-15m", "ask", "", true},
		{"ask,idle-15m", "", "15m", true},
		{"ask,idle-15m", "question", "", false},
		{"question,idle-1d", "", "15m 1h 1d", true},
		{"ask,question,idle-1h", "", "15m", false},
		{"ask,question,idle-1h", "", "15m 1h", true},
	} {
		sel, ok := sels[c.key]
		if !ok {
			t.Fatalf("no selector for %s", c.key)
		}
		if got := matchesChipSelector(t, sel, c.wait, c.idle); got != c.want {
			t.Errorf("chips %s on a card wait=%q idle=%q: shown=%v, want %v (selector %s)", c.key, c.wait, c.idle, got, c.want, sel)
		}
	}
	// Two idle chips are never a selection (they are exclusive).
	if _, ok := sels["idle-15m,idle-1h"]; ok {
		t.Error("two idle chips have a selector; they are exclusive")
	}
}

// TestClampTitle: 60 characters fit; 61 are cut to 60 (by character, not
// byte), and a trailing space is not kept.
func TestClampTitle(t *testing.T) {
	sixty := strings.Repeat("é", 60)
	if got, cut := clampTitle(sixty); cut || got != sixty {
		t.Errorf("60 characters were clamped: %q %v", got, cut)
	}
	if got, cut := clampTitle(sixty + "x"); !cut || got != sixty {
		t.Errorf("61 characters: %q %v, want the first 60", got, cut)
	}
	if got, cut := clampTitle(strings.Repeat("a", 59) + " tail"); !cut || got != strings.Repeat("a", 59) {
		t.Errorf("a cut at a space kept it: %q %v", got, cut)
	}
}
