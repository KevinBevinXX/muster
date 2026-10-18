//go:build liveclaude

package mirrorhost

import (
	"context"
	"encoding/json"
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
// claude and spends model turns), never in CI:
//
//	MUSTER_LIVE_SOCKET=<tmux -L name> MUSTER_LIVE_SESSION=<session> \
//	MUSTER_LIVE_PROJECT=<~/.claude/projects/<slug>> \
//	go test -tags liveclaude -run TestLiveClaudePicker ./internal/mirrorhost/
//
// The session's pane must run `claude` at its prompt in a scratch directory.
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
			parts = append(parts, "("+string(rune('1'+i))+") header '"+q.Header+"', question '"+q.Question+"', "+kind+", options "+strings.Join(labels, ", "))
		}
		return "Call the AskUserQuestion tool exactly once with these questions, in this order, each option with a one-word description: " +
			strings.Join(parts, "; ") + ". Then reply with the single word done."
	}
	cases := []struct {
		name string
		qs   []askq.Question
		cs   []askdrive.Choice
	}{
		{"single-select", []askq.Question{color}, []askdrive.Choice{{Picks: []int{2}}}},
		{"single-select, four options, the last", []askq.Question{size}, []askdrive.Choice{{Picks: []int{3}}}},
		{"own words", []askq.Question{name}, []askdrive.Choice{{Other: "Gamma, the third 42"}}},
		{"multi-select", []askq.Question{pets}, []askdrive.Choice{{Picks: []int{0, 2}}}},
		{"multi-select with own words", []askq.Question{pets}, []askdrive.Choice{{Picks: []int{1}, Other: "a hamster"}}},
		{"three questions", []askq.Question{color, pets, name}, []askdrive.Choice{{Other: "teal, mostly"}, {Picks: []int{1}, Other: "a hamster"}, {Picks: []int{1}}}},
		{"four questions", []askq.Question{color, pets, name, size}, []askdrive.Choice{{Picks: []int{0}}, {Picks: []int{0, 1, 2}}, {Other: "Delta"}, {Picks: []int{1}}}},
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
			start := time.Now()
			if err := tm.AnswerAsk(ctx, pane, c.qs, c.cs); err != nil {
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
