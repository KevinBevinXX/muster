package ui

import (
	"regexp"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestTheWorkChipSaysWhatRunsAndNeverMore: each shape of mirror.WindowWork,
// with pairwise-distinct counts so a chip that swapped the fields or
// hardcoded one cannot pass. "1+" is a lower bound and says so; a count at the
// wire's bound reads "99+"; nothing running draws no chip at all.
func TestTheWorkChipSaysWhatRunsAndNeverMore(t *testing.T) {
	for _, c := range []struct {
		w           mirror.WindowWork
		text, label string
	}{
		{mirror.WindowWork{Subagents: 2, Shells: 1}, "2 subagents · 1 shell", "2 subagents and 1 background shell running"},
		{mirror.WindowWork{Subagents: 1}, "1 subagent", "1 subagent running"},
		{mirror.WindowWork{Shells: 3}, "3 shells", "3 background shells running"},
		{mirror.WindowWork{Shells: 1, ShellsAtLeast: true}, "1+ shell", "at least 1 background shell running"},
		{mirror.WindowWork{Subagents: 4, Shells: 2, ShellsAtLeast: true}, "4 subagents · 2+ shells", "4 subagents and at least 2 background shells running"},
		{mirror.WindowWork{Subagents: mirror.MaxWorkCount, Shells: mirror.MaxWorkCount, ShellsAtLeast: true}, "99+ subagents · 99+ shells", "99+ subagents and 99+ background shells running"},
		{mirror.WindowWork{}, "", ""},
	} {
		if got := workChipText(c.w); got != c.text {
			t.Errorf("workChipText(%+v) = %q, want %q", c.w, got, c.text)
		}
		if got := workChipLabel(c.w); got != c.label {
			t.Errorf("workChipLabel(%+v) = %q, want %q", c.w, got, c.label)
		}
	}
}

// TestAMirroredCardShowsItsWorkChipAndIsFoundByIt: the card draws the chip in
// its header with its accessible label; a card with no work (an older host, or
// nothing running) has none — and
// is otherwise byte-identical to a card rendered before work existed (the
// mixed-version case: an old host's card renders unchanged).
func TestAMirroredCardShowsItsWorkChipAndIsFoundByIt(t *testing.T) {
	base := MirrorCardView{Host: "h", Session: "s", Window: "@1", Online: true, Claude: true, Repo: "fixture-repo"}
	withWork := base
	withWork.Work = mirror.WindowWork{Subagents: 2, Shells: 1}
	html := renderNode(t, mirrorCard(withWork))
	if !regexp.MustCompile(`data-card-slot="header"[^\n]*data-mirror-work=""`).MatchString(html) {
		t.Fatalf("the chip is not in the card's header: %s", html)
	}
	for _, want := range []string{">2 subagents · 1 shell<", `class="sr-only">2 subagents and 1 background shell running<`} {
		if !strings.Contains(html, want) {
			t.Errorf("the card lacks %s", want)
		}
	}
	plain := renderNode(t, mirrorCard(base))
	for _, absent := range []string{"data-mirror-work", "data-f-work", "subagent", "shell"} {
		if strings.Contains(plain, absent) {
			t.Errorf("a card with no work carries %q", absent)
		}
	}
	// Mixed version: the zero Work and Color an older host leaves are exactly
	// what this card had before either field existed.
	old := renderNode(t, mirrorCard(MirrorCardView{Host: "h", Session: "s", Window: "@1", Online: true, Claude: true, Repo: "fixture-repo",
		Work: mirror.WindowWork{}, Color: ""}))
	if old != plain {
		t.Fatalf("an older host's card is not the untouched card")
	}
}

// TestTheWorkChipIsNotAFilterField: the chip is display only — no `work:`
// filter field is registered and no card carries one.
func TestTheWorkChipIsNotAFilterField(t *testing.T) {
	for _, f := range cardFieldRegistry {
		if f.Key == "work" {
			t.Fatal("a `work` filter field is registered")
		}
	}
	html := renderNode(t, mirrorCard(MirrorCardView{Host: "h", Session: "s", Window: "@1", Online: true, Claude: true,
		Work: mirror.WindowWork{Subagents: 2, Shells: 1}}))
	if !strings.Contains(html, "data-mirror-work") {
		t.Fatalf("instrument check: the card has no chip: %s", html)
	}
	if strings.Contains(html, "data-f-work") {
		t.Fatalf("the card carries a work filter field: %s", html)
	}
}
