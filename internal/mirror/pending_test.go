package mirror

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// TestCleanPendingIsOnePhoneSafeLinePerField: control characters (C0, DEL,
// C1, U+2028/9) become spaces, whitespace collapses, invalid UTF-8 is dropped,
// each field is capped at its rune bound with "…", and nothing left is nil.
func TestCleanPendingIsOnePhoneSafeLinePerField(t *testing.T) {
	got := CleanPending(&Pending{Summary: "Bash:\tnpm\x1b[31m publish\r\n--tag\u0085next\u2028x\xff", Last: "  a \x00 b\x7f  "})
	if got == nil || got.Summary != "Bash: npm [31m publish --tag next x" || got.Last != "a b" {
		t.Fatalf("cleaned %+v", got)
	}
	long := CleanPending(&Pending{Summary: strings.Repeat("é", MaxPendingSummary+5), Last: strings.Repeat("w ", MaxPendingLast)})
	if r := []rune(long.Summary); len(r) != MaxPendingSummary || r[len(r)-1] != '…' {
		t.Fatalf("summary not capped at %d runes: %d", MaxPendingSummary, len(r))
	}
	if r := []rune(long.Last); len(r) > MaxPendingLast || r[len(r)-1] != '…' {
		t.Fatalf("last not capped at %d runes: %d %q", MaxPendingLast, len(r), long.Last)
	}
	exact := CleanPending(&Pending{Summary: strings.Repeat("s", MaxPendingSummary)})
	if exact.Summary != strings.Repeat("s", MaxPendingSummary) {
		t.Fatal("a summary AT the bound was cut")
	}
	if CleanPending(&Pending{Summary: " \x1b \n", Last: "\t"}) != nil || CleanPending(nil) != nil {
		t.Fatal("an empty pending survived")
	}
}

// TestOnlyWaitingClaudeWindowsCarryWhatTheyWaitOnAndOnlySoMany: a pending on a
// window that is not a waiting Claude pane is dropped; past
// MaxPendingWindows, the most recently waiting keep theirs.
func TestOnlyWaitingClaudeWindowsCarryWhatTheyWaitOnAndOnlySoMany(t *testing.T) {
	p := &Pending{Summary: "Edit x.go"}
	in := []Window{
		{Session: "a", ID: "@1", Claude: true, Status: StatusIdle, StatusSince: 5, Pending: p},
		{Session: "a", ID: "@2", Status: StatusWaiting, StatusSince: 5, Pending: p}, // not Claude: its status is dropped too
		{Session: "a", ID: "@3", Claude: true, Status: StatusWaiting, StatusSince: 5, Pending: p},
	}
	out := NormalizeWindows(in, 0)
	if out[0].Pending != nil || out[1].Pending != nil || out[2].Pending == nil || *out[2].Pending != *p {
		t.Fatalf("pending kept on %+v %+v %+v", out[0].Pending, out[1].Pending, out[2].Pending)
	}
	var many []Window
	for i := 0; i < MaxPendingWindows+3; i++ {
		many = append(many, Window{Session: "s", ID: "@" + strconv.Itoa(i+1), Index: i, Claude: true, Status: StatusWaiting,
			StatusSince: int64(1000 - i*10), Pending: &Pending{Summary: "x"}}) // the earlier the index, the newer
	}
	// A late index that waited most recently of all keeps its own.
	many[len(many)-1].StatusSince = 5000
	out = NormalizeWindows(many, 0)
	kept := 0
	for i, w := range out {
		if w.Pending != nil {
			kept++
			if i >= MaxPendingWindows-1 && i != len(out)-1 {
				t.Fatalf("window %d (older) kept its pending over a newer one", i)
			}
		}
	}
	if kept != MaxPendingWindows || out[len(out)-1].Pending == nil {
		t.Fatalf("kept %d pendings (want %d), newest kept: %v", kept, MaxPendingWindows, out[len(out)-1].Pending != nil)
	}
}

// TestPendingIsCompatibleBothWays: an older host's window (no field) decodes
// to no pending; a newer host's decodes on a muster that predates the field
// (a struct without it) with every other field intact; Equal sees it.
func TestPendingIsCompatibleBothWays(t *testing.T) {
	var old Window
	if err := json.Unmarshal([]byte(`{"session":"a","id":"@1","claude":true,"status":"waiting","statusSince":9}`), &old); err != nil || old.Pending != nil {
		t.Fatalf("an older host's window: %+v %v", old, err)
	}
	w := Window{Session: "a", ID: "@1", Claude: true, Status: StatusWaiting, StatusSince: 9, ClaudeSession: "s1",
		Pending: &Pending{Summary: "Bash: ls", Last: "Listing."}}
	b, _ := json.Marshal(Msg{Type: TypeSessions, Windows: []Window{w}})
	if !strings.Contains(string(b), `"pending":{"summary":"Bash: ls","last":"Listing."}`) {
		t.Fatalf("encoded %s", b)
	}
	var predates struct {
		Type    string `json:"type"`
		Windows []struct {
			Session       string `json:"session"`
			ID            string `json:"id"`
			Status        string `json:"status"`
			ClaudeSession string `json:"claudeSession"`
		} `json:"windows"`
	}
	if err := json.Unmarshal(b, &predates); err != nil || len(predates.Windows) != 1 || predates.Windows[0].Status != StatusWaiting || predates.Windows[0].ClaudeSession != "s1" {
		t.Fatalf("an older muster's decode: %+v %v", predates, err)
	}
	o := w
	o.Pending = &Pending{Summary: "Bash: ls", Last: "Listing!"}
	if w.Equal(o) || !w.Equal(w) {
		t.Fatal("Equal does not see the pending")
	}
}

// TestWhatAWindowWaitsOnMovesTheWaitingSet: the attention queue's nudge
// (waitingWindows) moves when a waiting window's pending arrives or changes,
// not only when it starts or stops waiting.
func TestWhatAWindowWaitsOnMovesTheWaitingSet(t *testing.T) {
	w := Window{Session: "a", ID: "@1", Claude: true, Status: StatusWaiting}
	with := w
	with.Pending = &Pending{Summary: "Bash: ls"}
	if strings.Join(waitingWindows([]Window{w}), ",") == strings.Join(waitingWindows([]Window{with}), ",") {
		t.Fatal("a pending that arrived did not move the waiting set")
	}
}

// TestCleanLineRemovesFormatCharacters: bidi embeddings/overrides/isolates,
// zero-width characters and the BOM (Unicode category Cf) are removed, so
// nothing shown can be reordered or hide a character; the visible text is
// kept, and each field (Tool too) gets the same rule.
func TestCleanLineRemovesFormatCharacters(t *testing.T) {
	in := "Bash: rm \u202Egnp.exe\u202C \u2066x\u2069 a\u200Bb\u200Cc\u200Dd\u2060e \uFEFFf\u00ADg"
	got := CleanPending(&Pending{Summary: in, Tool: "Ba\u200Bsh", Last: "\u202Bok\u202C"})
	if got == nil || got.Summary != "Bash: rm gnp.exe x abcde fg" || got.Tool != "Bash" || got.Last != "ok" {
		t.Fatalf("cleaned %+v", got)
	}
	if CleanPending(&Pending{Summary: "\u200B\u2066\uFEFF"}) != nil {
		t.Fatal("a field of format characters alone survived")
	}
}
