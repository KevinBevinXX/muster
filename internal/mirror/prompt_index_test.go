package mirror_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/mirror"
)

func refs(n int, text string) []cctranscript.PromptRef {
	out := make([]cctranscript.PromptRef, n)
	for i := range out {
		out[i] = cctranscript.PromptRef{Line: int64(1000 + 7*i), Text: text}
	}
	return out
}

// BoundPrompts keeps the NEWEST prompts within the count bound and counts the
// ones it dropped; refs that name no prompt (no Line, or one out of order) are
// dropped without being counted, and every text is capped again.
func TestBoundPromptsKeepsTheNewestAndCountsTheRest(t *testing.T) {
	in := refs(mirror.MaxIndexPrompts+37, "p")
	got, more := mirror.BoundPrompts(in)
	if len(got) != mirror.MaxIndexPrompts || more != 37 {
		t.Fatalf("kept %d, more %d; want %d, 37", len(got), more, mirror.MaxIndexPrompts)
	}
	if got[0] != in[37] || got[len(got)-1] != in[len(in)-1] {
		t.Fatalf("kept %v…%v, want the newest %v…%v", got[0], got[len(got)-1], in[37], in[len(in)-1])
	}

	odd := []cctranscript.PromptRef{
		{Line: 0, Text: "no place"},
		{Line: 50, Text: "a"},
		{Line: 40, Text: "out of order"},
		{Line: 50, Text: "a repeat"},
		{Line: 90, Text: strings.Repeat("x", 4*cctranscript.MaxPromptRefText)},
	}
	got, more = mirror.BoundPrompts(odd)
	want := []cctranscript.PromptRef{{Line: 50, Text: "a"}, {Line: 90, Text: cctranscript.Cap(strings.Repeat("x", 4*cctranscript.MaxPromptRefText), cctranscript.MaxPromptRefText)}}
	if !reflect.DeepEqual(got, want) || more != 0 {
		t.Fatalf("got %+v more %d, want %+v more 0", got, more, want)
	}
}

// 🔴 THE INDEX TRAVELS IN ONE MESSAGE, SO ITS WORST CASE MUST FIT ONE FRAME:
// the most prompts, each text at its cap and written six-fold by JSON (`<` is
// <), plus the message around it. The BYTE bound is what makes it fit —
// the count alone would not — so this also shows the byte bound bites.
func TestTheLargestPromptIndexFitsOneFrame(t *testing.T) {
	worst := refs(mirror.MaxIndexPrompts*4, strings.Repeat("<", cctranscript.MaxPromptRefText))
	kept, more := mirror.BoundPrompts(worst)
	if more == 0 || len(kept) >= mirror.MaxIndexPrompts {
		t.Fatalf("kept %d (more %d): the byte bound did not bite on the worst case", len(kept), more)
	}
	b, err := json.Marshal(mirror.Msg{Type: mirror.TypeEvents, ID: 1 << 31, Gen: strings.Repeat("g", 64), Offset: 1 << 50, Busy: true,
		PromptIndex: true, Prompts: kept, PromptsMore: more})
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > mirror.MaxFrame {
		t.Fatalf("the worst-case index message is %d bytes, over MaxFrame %d", len(b), mirror.MaxFrame)
	}
}
