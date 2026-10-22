package main

import (
	"context"
	"log"
	"reflect"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/api"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/dbtest"
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
		"a running agent keeps its mode until its spec is re-applied (Start or reapply), and leaving "+
		"bypass means destroying and recreating the agent.") {
		t.Fatalf("bypass off: %q", off)
	}
	if strings.Contains(off, "CONFIGURED") {
		t.Fatalf("bypass off announces a bypass: %q", off)
	}
}

// grantOnlyStore is a connector store holding only connectors and grants; any
// other method is the nil interface's (a panic if called).
type grantOnlyStore struct {
	connectors.Store
	grants []connectors.Grant
}

func (g grantOnlyStore) ListConnectors(context.Context) ([]connectors.Connector, error) {
	return nil, nil
}
func (g grantOnlyStore) ListGrants(context.Context) ([]connectors.Grant, error) { return g.grants, nil }

// 🔴 THE GITHUB-GRANT VETO IS WIRED FROM THE STORE THIS BOOT BUILT, OR BOOT IS
// REFUSED. Built the way buildApp builds it (buildAgentPlaneWith, then
// wireGitHubGrantVeto with the connector store): a listed-pool agent holding an
// approved `github` grant is decided NO bypass, naming the connector, while the
// same agent with no grant (the control) IS bypassed — so a wiring that handed
// the lookup an empty or nil store reddens here. A deployment that configures a
// connector store but hands none to the veto (built after it, or not passed) is
// refused, rather than bypassing every listed agent whatever it holds; one with
// no connector store at all boots, and no grant can exist there.
func TestTheGitHubGrantVetoIsWiredFromTheConnectorStore(t *testing.T) {
	logger := log.New(&strings.Builder{}, "", 0)
	cc := ccTestConfig()
	cc.AgentCCBypassPools = []string{"pr-review"}
	cc.Database = "postgres://wiring-test.invalid/muster"
	cc.ConnectorEncKey = []byte("0123456789abcdef0123456789abcdef")
	if !cc.connectorStoreConfigured() {
		t.Fatal("fixture: the config does not configure a connector store")
	}
	pool, err := buildClaudePool(cc, envPlane(t, cc), memMarks{})
	if err != nil {
		t.Fatal(err)
	}
	row := agents.Agent{ID: 7301, Name: "review-worker-4", Kind: agents.KindClaudeCode, CCAccount: "work", WorkerPool: "pr-review"}
	decide := func(cfg config, conns connectors.Store) (bool, string, error) {
		prov, _, err := buildAgentPlaneWith(cfg, stubStore{}, pool, nil, logger)
		if err != nil {
			t.Fatal(err)
		}
		if err := wireGitHubGrantVeto(cfg, prov, conns); err != nil {
			return false, "", err
		}
		on, veto := prov.JobBypassDecision(context.Background(), row)
		return on, veto, nil
	}

	held := grantOnlyStore{grants: []connectors.Grant{{ID: 1, AgentID: row.ID, Connector: "github", Status: connectors.StatusApproved}}}
	if on, veto, err := decide(cc, held); err != nil || on || !strings.Contains(veto, `connector "github"`) {
		t.Fatalf("agent holding an approved github grant: bypass=%v veto=%q err=%v; want no bypass, vetoed by the grant", on, veto, err)
	}
	if on, veto, err := decide(cc, grantOnlyStore{}); err != nil || !on || veto != "" {
		t.Fatalf("control, no grant: bypass=%v veto=%q err=%v; want bypass", on, veto, err)
	}
	if _, _, err := decide(cc, nil); err == nil || !strings.Contains(err.Error(), "refusing to boot") {
		t.Fatalf("connector store configured but none handed to the veto: err=%v; want a boot refusal", err)
	}
	none := cc
	none.ConnectorEncKey = nil
	if on, veto, err := decide(none, nil); err != nil || !on || veto != "" {
		t.Fatalf("no connector store configured: bypass=%v veto=%q err=%v; want bypass (no grant can exist)", on, veto, err)
	}
}

// 🔴 buildApp ITSELF WIRES THE VETO FROM THE STORE IT BUILT. With a real database
// and a connector store configured beside the bypass list, the whole service
// builds — so the veto was handed a connector store. A buildApp that handed it
// nil, or that built the agent plane before the connector store existed, is
// refused at boot by wireGitHubGrantVeto (pinned above), and this test then
// fails: that is what keeps those two wirings from silently bypassing every
// listed agent whatever grants it holds.
func TestBuildAppWiresTheGitHubGrantVetoFromItsConnectorStore(t *testing.T) {
	cfg := ccTestConfig()
	cfg.Database = dbtest.DSN(t)
	cfg.ConnectorEncKey = []byte(strings.Repeat("v", 32))
	cfg.AgentCCBypassPools = []string{"pr-review"}
	var buf strings.Builder
	app, err := buildApp(context.Background(), cfg, log.New(&buf, "", 0))
	if err != nil {
		t.Fatalf("buildApp with a connector store and a bypass list: %v", err)
	}
	app.Close()
	if !strings.Contains(buf.String(), "Job-lane permission bypass CONFIGURED for worker pools [pr-review]") {
		t.Fatalf("instrument check: the app built is not the bypass-configured one:\n%s", buf.String())
	}
}
