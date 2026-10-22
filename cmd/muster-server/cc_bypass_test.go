package main

import (
	"log"
	"reflect"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/api"
)

// TestTheBypassPoolListIsReadLowerCasedAndReachesTheSpecConfig: the list is read
// from MUSTER_AGENT_CC_BYPASS_POOLS trimmed, in order and LOWER-CASED (pool
// names are lower-case only, so an upper-case spelling names the same pool),
// and the claude-code spec config carries exactly it.
func TestTheBypassPoolListIsReadLowerCasedAndReachesTheSpecConfig(t *testing.T) {
	cfg, err := loadConfig(func(k string) string {
		return map[string]string{envAgentCCBypassPools: " pr-review , ,Triage-2"}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"pr-review", "triage-2"}; !reflect.DeepEqual(cfg.AgentCCBypassPools, want) {
		t.Fatalf("read %q, want %q", cfg.AgentCCBypassPools, want)
	}
	cc := ccTestConfig()
	cc.AgentCCBypassPools = []string{"pr-review", "triage-2"}
	spec := claudeCodeSpecConfig(cc)
	if want := []string{"pr-review", "triage-2"}; !reflect.DeepEqual(spec.BypassPools, want) {
		t.Fatalf("spec config BypassPools = %q, want %q", spec.BypassPools, want)
	}
	spec.BypassPools[0] = "mutated"
	if cc.AgentCCBypassPools[0] != "pr-review" {
		t.Fatal("the spec config shares the configuration's slice: a mutation downstream reaches it")
	}
}

// 🔴 EACH BOOT REFUSAL OF THE BYPASS LIST, reached from the VALID claude-code
// config by one mutation and asserted by its own words: set without the kind
// (an armed switch), and a name the pool rules refuse. The valid list boots. (A
// repeated name is not refused: it is harmless.)
func TestEveryBypassPoolRefusalIsReachedOnItsOwn(t *testing.T) {
	ok := ccTestConfig()
	ok.AgentCCBypassPools = []string{"pr-review", "a", "0-x", "pr-review"}
	if err := ok.validate(); err != nil {
		t.Fatalf("control: a valid bypass list is refused: %v", err)
	}
	for _, c := range []struct {
		name string
		mut  func(*config)
		want string
	}{
		{"without the kind", func(c *config) {
			c.AgentKinds, c.AgentCCImage, c.AgentCCAccountNames, c.AgentCCBypassPools = nil, "", nil, []string{"pr-review"}
		}, "MUSTER_AGENT_CC_BYPASS_POOLS is set but MUSTER_AGENT_KINDS does not include claude-code"},
		{"leading hyphen", func(c *config) { c.AgentCCBypassPools = []string{"-review"} },
			`invalid MUSTER_AGENT_CC_BYPASS_POOLS entry "-review"`},
		{"underscore", func(c *config) { c.AgentCCBypassPools = []string{"pr_review"} },
			`invalid MUSTER_AGENT_CC_BYPASS_POOLS entry "pr_review"`},
		{"too long", func(c *config) { c.AgentCCBypassPools = []string{strings.Repeat("a", 64)} },
			`invalid MUSTER_AGENT_CC_BYPASS_POOLS entry "` + strings.Repeat("a", 64) + `"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := ccTestConfig()
			c.mut(&cfg)
			err := cfg.validate()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one containing %q", err, c.want)
			}
		})
	}
}

// The claude-code arm of the `agent kinds:` line says which state the bypass is
// in, both ways: the pools listed, or that none is. The two lines differ.
func TestTheKindsBannerLineAnnouncesTheJobLaneBypassBothWays(t *testing.T) {
	render := func(pools []string) string {
		logger := log.New(&strings.Builder{}, "", 0)
		cc := ccTestConfig()
		cc.AgentCCBypassPools = pools
		pool, err := buildClaudePool(cc, envPlane(t, cc), memMarks{})
		if err != nil {
			t.Fatal(err)
		}
		prov, _, err := buildAgentPlaneWith(cc, stubStore{}, pool, nil, logger)
		if err != nil {
			t.Fatal(err)
		}
		return lineNaming(renderBanner(t, cc, api.Extensions{Provisioner: prov}), envAgentKinds)
	}
	on, off := render([]string{"pr-review", "triage"}), render(nil)
	if !strings.Contains(on, "Job-lane permission bypass CONFIGURED for worker pools [pr-review, triage] (MUSTER_AGENT_CC_BYPASS_POOLS)") ||
		!strings.Contains(on, "running agents keep the mode they were started with") {
		t.Fatalf("bypass on: %q", on)
	}
	if !strings.Contains(off, "No worker pool is configured for job-lane permission bypass (MUSTER_AGENT_CC_BYPASS_POOLS unset); "+
		"running agents keep the mode they were started with until restarted") {
		t.Fatalf("bypass off: %q", off)
	}
	if strings.Contains(off, "CONFIGURED") {
		t.Fatalf("bypass off announces a bypass: %q", off)
	}
}
