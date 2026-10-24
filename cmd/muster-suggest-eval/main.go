// Command muster-suggest-eval runs the suggested-replies eval set
// (internal/suggest/testdata/eval/fixtures.yaml) against the REAL model and
// reports each fixture's property pass rate, the latency of every call (p50,
// p95) and its cost.
//
// 🔴 IT SPENDS MONEY, SO IT CALLS NOTHING WITHOUT -live, and nothing in CI runs
// it (it has no tests; `go test ./...` only compiles it). Its input is the
// SYNTHETIC fixtures only — never a real transcript.
//
//	MUSTER_AGENT_OPENROUTER_KEY=… go run ./cmd/muster-suggest-eval -live [-runs 2] [-model openai/gpt-6-luna]
//
// The key is read from MUSTER_AGENT_OPENROUTER_KEY — the environment key the
// server spends for an admin (credentials.Plane) — and is never printed.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/suggest"
)

func main() {
	fixtures := flag.String("fixtures", "internal/suggest/testdata/eval/fixtures.yaml", "the eval fixtures")
	live := flag.Bool("live", false, "call the real model (spends the OpenRouter key)")
	model := flag.String("model", suggest.DefaultModel, "the OpenRouter model")
	effort := flag.String("effort", "", `reasoning effort ("" = the production default, "none" = send none)`)
	runs := flag.Int("runs", 1, "how many times to run each fixture")
	verbose := flag.Bool("v", false, "also print each dropped item and why")
	flag.Parse()
	showDrops = *verbose
	if err := run(*fixtures, *live, *model, *effort, *runs); err != nil {
		fmt.Fprintln(os.Stderr, "muster-suggest-eval:", err)
		os.Exit(1)
	}
}

var showDrops bool

func run(path string, live bool, model, effort string, runs int) error {
	fs, err := suggest.LoadFixtures(path)
	if err != nil {
		return err
	}
	if !live {
		return errors.New("refusing to call the model without -live (it spends the OpenRouter key)")
	}
	key := strings.TrimSpace(os.Getenv("MUSTER_AGENT_OPENROUTER_KEY"))
	if key == "" {
		return errors.New("MUSTER_AGENT_OPENROUTER_KEY is not set")
	}
	gen := &suggest.OpenRouter{Model: model, ReasoningEffort: effort,
		Key: func(context.Context, int64) (string, error) { return key, nil }}

	var lat []time.Duration
	var cost float64
	var inTok, outTok, calls, failed int
	props := map[string][2]int{} // property -> {held, checked}
	drops := map[suggest.Drop]int{}
	fixturePass, fixtureRuns, keptTotal, rawTotal := 0, 0, 0, 0
	for r := 0; r < runs; r++ {
		for _, f := range fs {
			in := f.Input()
			reply, _, ok := suggest.FinalReply(in.Tail)
			if !ok || !suggest.HandsBack(reply) {
				continue // the server would not call either
			}
			b, _ := suggest.Build(in)
			start := time.Now()
			g, err := gen.Generate(context.Background(), 1, b.Prompt)
			d := time.Since(start)
			calls++
			lat = append(lat, d)
			if err != nil {
				failed++
				fmt.Printf("%-42s FAIL %s (%s)\n", f.Name, suggest.Reason(err), d.Round(time.Millisecond))
				continue
			}
			cost += g.Usage.Cost
			inTok += g.Usage.PromptTokens
			outTok += g.Usage.CompletionTokens
			res := suggest.Score(f, b, g.Raw)
			fixtureRuns++
			keptTotal += res.Kept
			rawTotal += res.Raw
			if res.Pass() {
				fixturePass++
			}
			for _, dr := range res.Drops {
				drops[dr]++
			}
			var bad []string
			for p, okp := range res.Properties {
				c := props[p]
				c[1]++
				if okp {
					c[0]++
				} else {
					bad = append(bad, p)
				}
				props[p] = c
			}
			sort.Strings(bad)
			verdict := "pass"
			if len(bad) > 0 {
				verdict = "FAIL " + strings.Join(bad, ",")
			}
			// Kinds only: the suggestion TEXT is printed for synthetic fixtures,
			// which hold nothing real, so the operator can read what it made.
			var texts []string
			kept, _ := suggest.Filter(g.Raw, b)
			for _, k := range kept {
				texts = append(texts, k.Kind+": "+k.Text)
			}
			fmt.Printf("%-42s %-24s kept %d/%d  %6s  %s\n", f.Name, verdict, res.Kept, res.Raw, d.Round(time.Millisecond), strings.Join(texts, " | "))
			if showDrops {
				_, dropped := suggest.Filter(g.Raw, b)
				for _, x := range dropped {
					fmt.Printf("    dropped %-20s %s: %q cites=%v quote=%q\n", x.Reason, x.Raw.Kind, x.Raw.Text, x.Raw.Cites, x.Raw.Quote)
				}
			}
		}
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	fmt.Println()
	fmt.Printf("model %s, effort %q, %d calls (%d failed)\n", model, effortOr(effort), calls, failed)
	if fixtureRuns > 0 {
		fmt.Printf("fixtures passing every property: %d/%d (%.0f%%)\n", fixturePass, fixtureRuns, 100*float64(fixturePass)/float64(fixtureRuns))
		var names []string
		for p := range props {
			names = append(names, p)
		}
		sort.Strings(names)
		for _, p := range names {
			c := props[p]
			fmt.Printf("  %-13s %d/%d (%.0f%%)\n", p, c[0], c[1], 100*float64(c[0])/float64(c[1]))
		}
		fmt.Printf("survivors: %d of %d raw (%.0f%%); drops: %v\n", keptTotal, rawTotal, 100*float64(keptTotal)/float64(max(rawTotal, 1)), drops)
	}
	if len(lat) > 0 {
		fmt.Printf("latency p50 %s, p95 %s, max %s\n", pct(lat, 50), pct(lat, 95), lat[len(lat)-1].Round(time.Millisecond))
	}
	if calls-failed > 0 {
		n := float64(calls - failed)
		fmt.Printf("cost: $%.6f total, $%.6f per generation; tokens per generation: %.0f in, %.0f out\n", cost, cost/n, float64(inTok)/n, float64(outTok)/n)
	}
	return nil
}

func effortOr(e string) string {
	if e == "" {
		return suggest.DefaultReasoningEffort
	}
	return e
}

// pct is the p-th percentile of sorted ds (nearest rank).
func pct(ds []time.Duration, p int) time.Duration {
	i := (p*len(ds)+99)/100 - 1
	if i < 0 {
		i = 0
	}
	if i >= len(ds) {
		i = len(ds) - 1
	}
	return ds[i].Round(time.Millisecond)
}
