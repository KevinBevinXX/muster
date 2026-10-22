//go:build liveclaude

package mirrorhost

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/askdrive"
	"github.com/ZacxDev/muster/internal/askq"
)

// THE DRIVER AGAINST A REAL CLAUDE CODE PICKER. Opt-in (needs a logged-in
// claude and spends model turns), never in CI — and 🔴 RUN IT ON EVERY CLAUDE
// CODE UPGRADE: `make test-liveclaude` (tests/liveclaude.sh starts a
// scratch claude in a private tmux server and runs this). By hand:
//
//	MUSTER_LIVE_SOCKET=<tmux -L name> MUSTER_LIVE_SESSION=<session> \
//	MUSTER_LIVE_PROJECT=<~/.claude/projects/<slug>> \
//	go test -tags liveclaude -run TestLiveClaudePicker ./internal/mirrorhost/
//
// The session's pane must run `claude` at its prompt in a scratch directory.
// Before the multi-select case is driven it also re-measures the picker's
// redraw lag, and fails if the worst is not well under askdrive.SteadyDefault.
// Each case asks Claude to call AskUserQuestion, waits for the picker, drives
// it with Tmux.AnswerAsk, and reads the transcript's toolUseResult: the answers
// the CLI recorded must be the ones driven.
func TestLiveClaudePicker(t *testing.T) {
	sock, sess, proj := os.Getenv("MUSTER_LIVE_SOCKET"), os.Getenv("MUSTER_LIVE_SESSION"), os.Getenv("MUSTER_LIVE_PROJECT")
	if sock == "" || sess == "" || proj == "" {
		t.Skip("set MUSTER_LIVE_SOCKET, MUSTER_LIVE_SESSION and MUSTER_LIVE_PROJECT")
	}
	tm := Tmux{Socket: sock}
	ctx := context.Background()
	pane, ok := tm.ActivePane(ctx, sess)
	if !ok {
		t.Fatalf("no pane for %s", sess)
	}
	p := tmuxPane{t: tm, id: pane}
	color := askq.Question{Question: "Which color do you prefer?", Header: "Color", Options: []askq.Option{{Label: "Red"}, {Label: "Blue"}, {Label: "Green"}}}
	pets := askq.Question{Question: "Which pets do you have?", Header: "Pets", MultiSelect: true, Options: []askq.Option{{Label: "Cat"}, {Label: "Dog"}, {Label: "Fish"}}}
	name := askq.Question{Question: "What should the project be called?", Header: "Name", Options: []askq.Option{{Label: "Alpha"}, {Label: "Beta"}}}
	size := askq.Question{Question: "Which size?", Header: "Size", Options: []askq.Option{{Label: "Small"}, {Label: "Medium"}, {Label: "Large"}, {Label: "Huge"}}}
	// A question too long for a line (the CLI boxes it, "│ " down its left
	// edge) and an answer in its own words over two rows (the CLI wraps the
	// text row): the two live failures this case pins.
	where := askq.Question{Question: "The shared widget package renders every tool call the same way regardless of its status, so the spinner on a half-finished call never stops. Should we fix it upstream, or patch our copy for now and file the upstream change separately?", Header: "Where", Options: []askq.Option{{Label: "Upstream"}, {Label: "Patch"}}}
	own := "The build agent needs to support running nightly batch-jobs, alongside the interactive sessions, where we are using models via a proxy."
	describe := func(qs []askq.Question) string {
		var parts []string
		for i, q := range qs {
			var labels []string
			for _, o := range q.Options {
				labels = append(labels, o.Label)
			}
			kind := "single-select"
			if q.MultiSelect {
				kind = "multiSelect true"
			}
			if q.Previews() {
				kind += ", each option ALSO with a 'preview' field of two short lines"
			}
			parts = append(parts, "("+string(rune('1'+i))+") header '"+q.Header+"', question '"+q.Question+"', "+kind+", options "+strings.Join(labels, ", "))
		}
		return "Call the AskUserQuestion tool exactly once with these questions, in this order, each option with a one-word description: " +
			strings.Join(parts, "; ") + ". Then reply with the single word done."
	}
	// layout's options carry previews: the CLI draws them in two columns, with
	// no row for an answer of one's own, and a digit only moves the cursor.
	layout := askq.Question{Question: "Which layout should the report use?", Header: "Layout", Options: []askq.Option{
		{Label: "Table", Preview: "x"}, {Label: "Cards", Preview: "x"}, {Label: "List", Preview: "x"}}}
	cases := []struct {
		name string
		qs   []askq.Question
		cs   []askdrive.Choice
		// lose, when set, is a key (from 1) the first drive's pane never
		// delivers: it is lost there, and a second drive handed what it sent
		// must finish the answer (askdrive.DriveFrom).
		lose int
	}{
		{"options with previews", []askq.Question{layout, name}, []askdrive.Choice{{Picks: []int{1}}, {Picks: []int{0}}}, 0},
		{"three questions, lost after question 1 and continued", []askq.Question{color, pets, name}, []askdrive.Choice{{Picks: []int{1}}, {Picks: []int{0, 2}}, {Picks: []int{1}}}, 2},
		{"single-select", []askq.Question{color}, []askdrive.Choice{{Picks: []int{2}}}, 0},
		{"single-select, four options, the last", []askq.Question{size}, []askdrive.Choice{{Picks: []int{3}}}, 0},
		{"own words", []askq.Question{name}, []askdrive.Choice{{Other: "Gamma, the third 42"}}, 0},
		{"multi-select", []askq.Question{pets}, []askdrive.Choice{{Picks: []int{0, 2}}}, 0},
		{"multi-select with own words", []askq.Question{pets}, []askdrive.Choice{{Picks: []int{1}, Other: "a hamster"}}, 0},
		{"three questions", []askq.Question{color, pets, name}, []askdrive.Choice{{Other: "teal, mostly"}, {Picks: []int{1}, Other: "a hamster"}, {Picks: []int{1}}}, 0},
		{"four questions", []askq.Question{color, pets, name, size}, []askdrive.Choice{{Picks: []int{0}}, {Picks: []int{0, 1, 2}}, {Other: "Delta"}, {Picks: []int{1}}}, 0},
		{"a long boxed question", []askq.Question{where}, []askdrive.Choice{{Picks: []int{1}}}, 0},
		{"a long answer of its own to question 2", []askq.Question{color, where}, []askdrive.Choice{{Picks: []int{0}}, {Other: own}}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := liveResults(t, proj)
			if err := tm.PastePane(ctx, pane, describe(c.qs)); err != nil {
				t.Fatal(err)
			}
			// The picker on screen.
			deadline := time.Now().Add(90 * time.Second)
			for {
				scr, _ := p.Capture(ctx)
				if askdrive.Match(askdrive.Parse(scr), askdrive.Start(), c.qs, nil) == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("no picker for %s:\n%s", c.name, scr)
				}
				time.Sleep(200 * time.Millisecond)
			}
			if c.name == "multi-select" {
				measureRedraw(t, ctx, p)
			}
			start := time.Now()
			if c.lose > 0 {
				keys, err := askdrive.DriveFrom(ctx, &losingPane{tmuxPane: p, lose: c.lose}, c.qs, c.cs, nil, askdrive.Options{})
				if !errors.Is(err, askdrive.ErrLost) || keys != c.lose {
					scr, _ := p.Capture(ctx)
					t.Fatalf("the first drive: %d keys, %v\n%s", keys, err, scr)
				}
				if _, err := tm.AnswerAsk(ctx, pane, c.qs, c.cs, &askq.Progress{Choices: c.cs, Keys: keys}); err != nil {
					scr, _ := p.Capture(ctx)
					t.Fatalf("the continuing drive: %v\n%s", err, scr)
				}
			} else if err := answer1(tm, ctx, pane, c.qs, c.cs); err != nil {
				scr, _ := p.Capture(ctx)
				t.Fatalf("drive: %v\n%s", err, scr)
			}
			t.Logf("%s: driven in %s", c.name, time.Since(start).Round(time.Millisecond))
			want := askdrive.Answers(c.qs, c.cs)
			deadline = time.Now().Add(20 * time.Second)
			for {
				for id, got := range liveResults(t, proj) {
					if _, old := before[id]; old {
						continue
					}
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("the CLI recorded %v, want %v", got, want)
					}
					// Let the turn finish before the next prompt.
					time.Sleep(8 * time.Second)
					return
				}
				if time.Now().After(deadline) {
					t.Fatal("no toolUseResult in the transcript")
				}
				time.Sleep(100 * time.Millisecond)
			}
		})
	}
}

// losingPane is tmuxPane whose lose-th key (from 1) never reaches the pane.
type losingPane struct {
	tmuxPane
	lose, sent int
}

func (l *losingPane) Send(ctx context.Context, keys string) error {
	l.sent++
	if l.sent == l.lose {
		return nil
	}
	return l.tmuxPane.Send(ctx, keys)
}

// liveResults is every AskUserQuestion toolUseResult in proj's newest
// transcript, by tool_use id.
func liveResults(t *testing.T, proj string) map[string]map[string]string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(proj, "*.jsonl"))
	sort.Slice(files, func(i, j int) bool {
		a, _ := os.Stat(files[i])
		b, _ := os.Stat(files[j])
		return a.ModTime().After(b.ModTime())
	})
	out := map[string]map[string]string{}
	if len(files) == 0 {
		return out
	}
	b, _ := os.ReadFile(files[0])
	for _, line := range strings.Split(string(b), "\n") {
		var r struct {
			ToolUseResult json.RawMessage `json:"toolUseResult"`
			Message       struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &r) != nil || len(r.ToolUseResult) == 0 {
			continue
		}
		res, ok := askq.ParseResult(r.ToolUseResult)
		if !ok {
			continue
		}
		var blocks []struct {
			Type      string `json:"type"`
			ToolUseID string `json:"tool_use_id"`
		}
		_ = json.Unmarshal(r.Message.Content, &blocks)
		for _, bl := range blocks {
			if bl.Type == "tool_result" {
				out[bl.ToolUseID] = res.Answers
			}
		}
	}
	return out
}

// measureRedraw toggles the first option 20 times (an even number: the picker
// ends untouched), timing each from the key pasted to the screen changed, and
// fails if the worst is not under a third of askdrive.SteadyDefault — the
// margin the driver's second look relies on.
func measureRedraw(t *testing.T, ctx context.Context, p tmuxPane) {
	t.Helper()
	var worst time.Duration
	var all []time.Duration
	for i := 0; i < 20; i++ {
		before, _ := p.Capture(ctx)
		start := time.Now()
		if err := p.Send(ctx, "1"); err != nil {
			t.Fatal(err)
		}
		for {
			s, _ := p.Capture(ctx)
			if s != before {
				break
			}
			if time.Since(start) > 2*time.Second {
				t.Fatalf("key %d never drew", i+1)
			}
			time.Sleep(2 * time.Millisecond)
		}
		d := time.Since(start)
		all = append(all, d)
		worst = max(worst, d)
		time.Sleep(100 * time.Millisecond)
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	t.Logf("redraw lag over %d keys: median %s, worst %s (Steady %s)", len(all), all[len(all)/2].Round(time.Millisecond), worst.Round(time.Millisecond), askdrive.SteadyDefault)
	if worst*3 > askdrive.SteadyDefault {
		t.Fatalf("the picker's worst redraw lag (%s) is over a third of askdrive.SteadyDefault (%s): re-measure and raise it", worst, askdrive.SteadyDefault)
	}
}
