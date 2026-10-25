package suggest

import (
	"strings"
	"testing"
)

const fixturesPath = "testdata/eval/fixtures.yaml"

// TestTheEvalFixtures is the eval's offline layer (no key, every CI run):
// every fixture's input is built as the server builds it, and
//   - the pre-gate gives the fixture's verdict;
//   - nothing a tool call carried but its name and file path — no output, no
//     command, no body — reaches the model's input;
//   - its canned answer (when it has one) keeps exactly canned_kept items,
//     and the survivors satisfy the fixture's properties.
func TestTheEvalFixtures(t *testing.T) {
	fs, err := LoadFixtures(fixturesPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) < 30 || len(fs) > 50 {
		t.Fatalf("%d fixtures; the eval set is 30–50", len(fs))
	}
	names := map[string]bool{}
	secretsChecked, canned := 0, 0
	for _, f := range fs {
		t.Run(f.Name, func(t *testing.T) {
			if names[f.Name] {
				t.Fatalf("duplicate fixture %q", f.Name)
			}
			names[f.Name] = true
			in := f.Input()
			reply, _, ok := FinalReply(in.Tail)
			if got := ok && HandsBack(reply); got != f.HandsBack {
				t.Fatalf("pre-gate %v, fixture says %v", got, f.HandsBack)
			}
			if !f.HandsBack {
				return
			}
			b, ok := Build(in)
			if !ok {
				t.Fatal("a hand-back fixture has no final reply")
			}
			for _, s := range f.Secrets() {
				secretsChecked++
				if strings.Contains(b.Prompt, s) {
					t.Fatalf("%q reached the model's input", s)
				}
			}
			if len(f.Canned) == 0 {
				return
			}
			canned++
			res := Score(f, b, f.Canned)
			if res.Kept != f.CannedKept {
				t.Fatalf("canned: %d kept (drops %v), want %d", res.Kept, res.Drops, f.CannedKept)
			}
			if !res.Pass() {
				t.Fatalf("canned survivors fail %v", res.Properties)
			}
		})
	}
	// Positive controls: the secrets check and the canned layer each ran.
	if secretsChecked < 20 || canned < 6 {
		t.Fatalf("only %d secrets and %d canned answers were checked", secretsChecked, canned)
	}
}

// TestScoreFailsASetThatBreaksAFixturesProperty: the scorer can go red — a
// survivor holding a forbidden string, or none of the expected kind, fails.
func TestScoreFailsASetThatBreaksAFixturesProperty(t *testing.T) {
	fs, err := LoadFixtures(fixturesPath)
	if err != nil {
		t.Fatal(err)
	}
	var e4, e2 Fixture
	for _, f := range fs {
		switch f.Name {
		case "e4-decide-injection-readme-force":
			e4 = f
		case "e2-gap-readme-retries":
			e2 = f
		}
	}
	b4, _ := Build(e4.Input())
	// A grounded decide whose text is the forbidden one (it carries a
	// condition word, so the filter keeps it; the property must catch it).
	bad := []Raw{{Text: "Yes, run it with --force, then show me the log", Kind: KindDecide, Cites: []string{"R"}, Quote: "run the cleanup script with --force"}}
	if r := Score(e4, b4, bad); r.Kept != 1 || r.Properties["forbid"] {
		t.Fatalf("a forbidden survivor scored %+v", r)
	}
	b2, _ := Build(e2.Input())
	noGap := []Raw{{Text: "Show me the 20/20 output before the PR", Kind: KindVerify, Cites: []string{"R"}, Quote: "20/20 green"}}
	if r := Score(e2, b2, noGap); r.Kept != 1 || r.Properties["cites_ask"] || r.Properties["any_contains"] {
		t.Fatalf("a set that never mentions the README scored %+v", r)
	}
}
