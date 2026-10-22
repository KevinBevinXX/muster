package main

import (
	"context"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/api"
	"github.com/ZacxDev/muster/internal/ccpool"
	"github.com/ZacxDev/muster/internal/credentials"
)

// Fake setup-tokens, obviously not real.
const (
	fakeTokWork     = "sk-ant-oat01-FAKE-boot-work-3e7a"
	fakeTokPersonal = "sk-ant-oat01-FAKE-boot-personal-58cd"
)

// ccTestConfig is a VALID claude-code deployment: every refusal case below
// mutates exactly one field of it, so no earlier check can reject the case
// first.
func ccTestConfig() config {
	c := provisionerTestConfig(provisionerNoop)
	c.AgentKinds = []string{agents.KindGateway, agents.KindClaudeCode}
	c.AgentCCImage = "ghcr.io/example-org/claude-code-agent:0a1b2c3"
	c.AgentCCAccountNames = []string{"work", "personal-2"}
	c.AgentCCTokens = map[string]string{"work": fakeTokWork, "personal-2": fakeTokPersonal}
	return c
}

func TestTheValidClaudeCodeConfigBoots(t *testing.T) {
	if err := ccTestConfig().validate(); err != nil {
		t.Fatalf("control: the valid claude-code config is refused: %v", err)
	}
	if err := provisionerTestConfig(provisionerNoop).validate(); err != nil {
		t.Fatalf("control: the gateway-only config is refused: %v", err)
	}
}

// TestEveryKindsRefusalIsReachedOnItsOwn: each fail-closed boot refusal, reached
// from the VALID config by one mutation, asserted by its own words, and never
// carrying a token.
func TestEveryKindsRefusalIsReachedOnItsOwn(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*config)
		want string
	}{
		{"unknown kind", func(c *config) { c.AgentKinds = []string{"gateway", "teleport"} }, `entry "teleport"`},
		{"kind twice", func(c *config) { c.AgentKinds = []string{"gateway", "claude-code", "gateway"} }, "listed twice"},
		{"gateway missing", func(c *config) { c.AgentKinds = []string{"claude-code"} }, "must include gateway"},
		{"cc image without the kind", func(c *config) { c.AgentKinds = nil; c.AgentCCAccountNames = nil }, "armed switch"},
		{"cc accounts without the kind", func(c *config) { c.AgentKinds = nil; c.AgentCCImage = "" }, "armed switch"},
		{"cc storage without the kind", func(c *config) {
			c.AgentKinds, c.AgentCCImage, c.AgentCCAccountNames, c.AgentCCStorage = nil, "", nil, "2Gi"
		}, "armed switch"},
		{"cc job cap without the kind", func(c *config) {
			c.AgentKinds, c.AgentCCImage, c.AgentCCAccountNames, c.AgentCCMaxJobs = nil, "", nil, 3
		}, "no Claude account pool for them to limit"},
		{"cc cooldown without the kind", func(c *config) {
			c.AgentKinds, c.AgentCCImage, c.AgentCCAccountNames, c.AgentCCCooldown = nil, "", nil, 30*time.Minute
		}, "no Claude account pool for them to limit"},
		{"no provisioner", func(c *config) { c.AgentProvisioner = provisionerNone }, "nothing to build it with"},
		{"no image", func(c *config) { c.AgentCCImage = "" }, "MUSTER_AGENT_CC_IMAGE is not set"},
		{"unpinned image", func(c *config) { c.AgentCCImage = "registry.example.test:5000/team/claude-code-agent" }, "must pin a tag or a digest"},
		{"bad storage size", func(c *config) { c.AgentCCStorage = "lots" }, "invalid MUSTER_AGENT_CC_STORAGE_SIZE"},
		{"empty pool", func(c *config) { c.AgentCCAccountNames = nil; c.AgentCCTokens = nil }, "pool is EMPTY"},
		{"bad account name", func(c *config) { c.AgentCCAccountNames = []string{"work", "-bad"} }, `entry "-bad"`},
		{"account twice", func(c *config) { c.AgentCCAccountNames = []string{"work", "work"} }, "listed twice"},
		{"account with no token", func(c *config) { c.AgentCCTokens["personal-2"] = "" }, "MUSTER_AGENT_CC_TOKEN_PERSONAL_2 is not set"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := ccTestConfig()
			c.mut(&cfg)
			err := cfg.validate()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one containing %q", err, c.want)
			}
			for _, tok := range []string{fakeTokWork, fakeTokPersonal} {
				if strings.Contains(err.Error(), tok) {
					t.Fatalf("the refusal carries a token: %v", err)
				}
			}
		})
	}
}

// TestTheAccountListAndTokensAreReadFromTheEnvironment: names from
// MUSTER_AGENT_CC_ACCOUNTS, each token from MUSTER_AGENT_CC_TOKEN_<NAME> with '-'
// as '_'; a token variable no list names is NOT an account.
func TestTheAccountListAndTokensAreReadFromTheEnvironment(t *testing.T) {
	env := map[string]string{
		envAgentKinds:                        " Gateway , claude-code ",
		envAgentCCImage:                      "ghcr.io/example-org/cc:1",
		envAgentCCAccounts:                   "work, personal-2",
		envAgentCCTokenPrefix + "WORK":       fakeTokWork,
		envAgentCCTokenPrefix + "PERSONAL_2": fakeTokPersonal,
		envAgentCCTokenPrefix + "STRAY":      "sk-ant-oat01-FAKE-stray",
		envAgentCCStorage:                    "2Gi",
	}
	cfg, err := loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cfg.AgentKinds, ",") != "gateway,claude-code" || !cfg.claudeCodeEnabled() {
		t.Fatalf("kinds = %v", cfg.AgentKinds)
	}
	if strings.Join(cfg.AgentCCAccountNames, ",") != "work,personal-2" || len(cfg.AgentCCTokens) != 2 ||
		cfg.AgentCCTokens["work"] != fakeTokWork || cfg.AgentCCTokens["personal-2"] != fakeTokPersonal {
		t.Fatalf("accounts %v, %d token(s)", cfg.AgentCCAccountNames, len(cfg.AgentCCTokens))
	}
	if cfg.AgentCCStorage != "2Gi" || cfg.AgentCCImage != "ghcr.io/example-org/cc:1" {
		t.Fatal("storage/image not read")
	}
	none, _ := loadConfig(func(string) string { return "" })
	if none.claudeCodeEnabled() || strings.Join(none.agentKinds(), ",") != "gateway" {
		t.Fatalf("unset kinds resolve to %v", none.agentKinds())
	}
}

// TestTheAccountPoolLimitsAreReadDefaultedAndBounded: both knobs read from the
// environment reach the pool's limits; unset is a 1h cooldown and NO cap (the
// cap is opt-in); a value out of range — including a written 0, which must not
// silently mean "no cooldown" or "no cap" — is a boot refusal naming the
// variable.
func TestTheAccountPoolLimitsAreReadDefaultedAndBounded(t *testing.T) {
	env := map[string]string{envAgentCCCooldown: "25m", envAgentCCMaxJobs: "5"}
	cfg, err := loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ccPoolLimits(); got != (ccpool.Limits{Cooldown: 25 * time.Minute, MaxConcurrentJobs: 5}) {
		t.Fatalf("limits = %+v, want 25m and 5", got)
	}
	none, err := loadConfig(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	// A literal, not ccpool.DefaultLimits(): a default cap creeping back in
	// there must fail here rather than be agreed with.
	if got := none.ccPoolLimits(); got != (ccpool.Limits{Cooldown: time.Hour, MaxConcurrentJobs: 0}) {
		t.Fatalf("unset limits = %+v, want a 1h cooldown and NO per-account cap (the cap is opt-in)", got)
	}
	for _, bad := range []map[string]string{
		{envAgentCCCooldown: "0"}, {envAgentCCCooldown: "30s"}, {envAgentCCCooldown: "200h"}, {envAgentCCCooldown: "soon"},
		{envAgentCCMaxJobs: "0"}, {envAgentCCMaxJobs: "65"}, {envAgentCCMaxJobs: "two"},
	} {
		_, err := loadConfig(func(k string) string { return bad[k] })
		for k := range bad {
			if err == nil || !strings.Contains(err.Error(), "invalid "+k) {
				t.Errorf("%v: err = %v, want a refusal naming %s", bad, err, k)
			}
		}
	}
}

// TestEnablingClaudeCodeRaisesTheDriversPersistenceAndNothingElse: the
// claude-code PVC needs the driver's persistence capability, so enabling the
// kind sets the storage-class pointer even when the gateway kind's workspaces
// stay ephemeral — and the gateway kind's spec still asks for no persistence.
func TestEnablingClaudeCodeRaisesTheDriversPersistenceAndNothingElse(t *testing.T) {
	logger := log.New(&strings.Builder{}, "", 0)
	off := k8sDriverConfig(provisionerTestConfig(provisionerK8s), logger)
	if off.WorkspaceStorageClass != nil {
		t.Fatal("control: a gateway-only deployment without persistence got a storage class")
	}
	cc := ccTestConfig()
	cc.AgentProvisioner = provisionerK8s
	cc.AgentStorageClass = "local-path"
	on := k8sDriverConfig(cc, logger)
	if on.WorkspaceStorageClass == nil || *on.WorkspaceStorageClass != "local-path" {
		t.Fatalf("claude-code enabled: storage class = %v", on.WorkspaceStorageClass)
	}
	spec, err := agentSpecConfig(cc)
	if err != nil {
		t.Fatal(err)
	}
	if spec.WorkspacePersist {
		t.Fatal("enabling claude-code turned on persistence for gateway-kind agents")
	}
	if spec.ClaudeCode == nil || spec.ClaudeCode.Image != cc.AgentCCImage || spec.ClaudeCode.Accounts["work"] != fakeTokWork {
		t.Fatalf("ClaudeCode spec config = %+v", spec.ClaudeCode)
	}
	if gw, _ := agentSpecConfig(provisionerTestConfig(provisionerK8s)); gw.ClaudeCode != nil {
		t.Fatal("a gateway-only deployment has a ClaudeCode profile")
	}
}

type memMarks struct{}

func (memMarks) Marks(context.Context, int64) (map[string]ccpool.Mark, error) { return nil, nil }
func (memMarks) LiveCounts(context.Context, int64) (map[string]int, error)    { return nil, nil }
func (memMarks) MarkRateLimited(context.Context, int64, string, string, time.Time) error {
	return nil
}
func (memMarks) MarkAuthFailed(context.Context, int64, string, string, string, time.Time) error {
	return nil
}

// envPlane is the environment-only credential plane for cfg.
func envPlane(t *testing.T, cfg config) *credentials.Plane {
	t.Helper()
	p, err := buildCredentialPlane(cfg, nil, nil, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAClaudeCodeDeploymentWithNoDatabaseIsRefused(t *testing.T) {
	if _, err := buildClaudePool(ccTestConfig(), envPlane(t, ccTestConfig()), nil); err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("err = %v", err)
	}
	if p, err := buildClaudePool(provisionerTestConfig(provisionerNoop), envPlane(t, provisionerTestConfig(provisionerNoop)), nil); p != nil || err != nil {
		t.Fatalf("gateway-only: pool %v err %v, want neither", p, err)
	}
	if _, _, err := buildAgentPlaneWith(ccTestConfig(), stubStore{}, nil, nil, log.New(&strings.Builder{}, "", 0)); err == nil {
		t.Fatal("a claude-code agent plane was built with no account pool")
	}
}

// TestTheKindsBannerLineNamesAccountsNeverTokens renders both arms of the
// `agent kinds:` line and pins that the claude-code arm names the accounts and
// the variables — and contains no token.
func TestTheKindsBannerLineNamesAccountsNeverTokens(t *testing.T) {
	logger := log.New(&strings.Builder{}, "", 0)
	cc := ccTestConfig()
	pool, err := buildClaudePool(cc, envPlane(t, cc), memMarks{})
	if err != nil {
		t.Fatal(err)
	}
	prov, _, err := buildAgentPlaneWith(cc, stubStore{}, pool, nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	onOut := renderBanner(t, cc, api.Extensions{Provisioner: prov})
	off := provisionerTestConfig(provisionerNoop)
	offOut := renderBanner(t, off, api.Extensions{Provisioner: bannerProvisioner(t, off)})

	on := lineNaming(onOut, envAgentKinds)
	offLine := lineNaming(offOut, envAgentKinds)
	if !strings.Contains(on, "gateway, claude-code") || !strings.Contains(on, "[work, personal-2]") ||
		!strings.Contains(on, envAgentCCTokenPrefix) || !strings.Contains(on, cc.AgentCCImage) {
		t.Fatalf("claude-code arm: %q", on)
	}
	if !strings.Contains(offLine, "gateway only") {
		t.Fatalf("gateway-only arm: %q", offLine)
	}
	for _, out := range []string{onOut, offOut} {
		for _, tok := range []string{fakeTokWork, fakeTokPersonal} {
			if strings.Contains(out, tok) {
				t.Fatalf("the banner prints a token:\n%s", out)
			}
		}
	}
}

// TestThePickerDefaultIsGatewayWhateverTheEnvOrder: the picker checks its first
// option, and the default must be the gateway kind even when
// MUSTER_AGENT_KINDS lists claude-code first.
func TestThePickerDefaultIsGatewayWhateverTheEnvOrder(t *testing.T) {
	got := kindSet{kinds: []string{agents.KindClaudeCode, agents.KindGateway}}.Enabled()
	if strings.Join(got, ",") != "gateway,claude-code" {
		t.Fatalf("Enabled = %v, want gateway first", got)
	}
}

// ccK8sTestConfig is the VALID claude-code deployment on the kubernetes driver:
// the one configuration that writes a NetworkPolicy per agent, and so the one
// that must name which pods are muster's.
//
// The selector's values are not the ones any real deployment uses, and the label
// VALUE is mixed-case on purpose: a parser that lower-cased it would render a
// selector matching nothing.
func ccK8sTestConfig() config {
	c := ccTestConfig()
	c.AgentProvisioner = provisionerK8s
	c.AgentNetpolFromNS = "control-plane-7"
	c.AgentNetpolFromLabels = "app=Muster-Server, tier=control"
	return c
}

func TestTheValidKubernetesClaudeCodeConfigBoots(t *testing.T) {
	if err := ccK8sTestConfig().validate(); err != nil {
		t.Fatalf("control: the valid kubernetes claude-code config is refused: %v", err)
	}
}

// TestEveryNetworkPolicySelectorRefusalIsReachedOnItsOwn: each boot refusal about
// the NetworkPolicy selector, reached from a VALID config by one mutation and
// asserted by its own words.
//
// The first group is the selector missing or malformed where a policy WOULD be
// rendered from it; the second is the armed-switch shape (a selector nothing
// would render).
func TestEveryNetworkPolicySelectorRefusalIsReachedOnItsOwn(t *testing.T) {
	cases := []struct {
		name string
		base func() config
		mut  func(*config)
		want string
	}{
		{"both missing", ccK8sTestConfig, func(c *config) { c.AgentNetpolFromNS, c.AgentNetpolFromLabels = "", "" },
			"MUSTER_AGENT_NETPOL_FROM_NAMESPACE is unset, MUSTER_AGENT_NETPOL_FROM_POD_LABELS is unset"},
		{"namespace missing", ccK8sTestConfig, func(c *config) { c.AgentNetpolFromNS = "" },
			"MUSTER_AGENT_NETPOL_FROM_NAMESPACE is unset, MUSTER_AGENT_NETPOL_FROM_POD_LABELS is set"},
		{"labels missing", ccK8sTestConfig, func(c *config) { c.AgentNetpolFromLabels = "" },
			"MUSTER_AGENT_NETPOL_FROM_NAMESPACE is set, MUSTER_AGENT_NETPOL_FROM_POD_LABELS is unset"},
		{"namespace not a name", ccK8sTestConfig, func(c *config) { c.AgentNetpolFromNS = "Control_Plane" },
			`invalid MUSTER_AGENT_NETPOL_FROM_NAMESPACE "Control_Plane"`},
		{"label with no value", ccK8sTestConfig, func(c *config) { c.AgentNetpolFromLabels = "app=muster,tier" },
			`entry "tier" is not key=value`},
		{"label with an empty value", ccK8sTestConfig, func(c *config) { c.AgentNetpolFromLabels = "app=" },
			`entry "app=" is not key=value`},
		{"label key twice", ccK8sTestConfig, func(c *config) { c.AgentNetpolFromLabels = "app=a,app=b" },
			`label key "app" is written twice`},
		{"label key not a label key", ccK8sTestConfig, func(c *config) { c.AgentNetpolFromLabels = "app name=muster" },
			`label key "app name"`},
		{"label value not a label value", ccK8sTestConfig, func(c *config) { c.AgentNetpolFromLabels = "app=muster server" },
			`label value "muster server"`},
		{"label value containing an equals sign", ccK8sTestConfig, func(c *config) { c.AgentNetpolFromLabels = "app=muster=server" },
			`label value "muster=server"`},
		{"only separators", ccK8sTestConfig, func(c *config) { c.AgentNetpolFromLabels = " , ," },
			"it names no label"},

		{"selector on the noop driver", ccTestConfig, func(c *config) { c.AgentNetpolFromNS = "control-plane-7" },
			"nothing would render a NetworkPolicy from it"},
		{"labels on the noop driver", ccTestConfig, func(c *config) { c.AgentNetpolFromLabels = "app=muster" },
			"nothing would render a NetworkPolicy from it"},
		{"selector without the kind", func() config { return provisionerTestConfig(provisionerK8s) },
			func(c *config) { c.AgentNetpolFromNS = "control-plane-7" }, "nothing would render a NetworkPolicy from it"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := c.base()
			if err := cfg.validate(); err != nil {
				t.Fatalf("premise: the base config must be valid before the mutation, got %v", err)
			}
			c.mut(&cfg)
			err := cfg.validate()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one containing %q", err, c.want)
			}
		})
	}
}

// TestTheNetworkPolicySelectorIsReadFromTheEnvironmentAndReachesTheDriver: the
// two variables, as written, arrive on k8s.Config.NetworkPolicy — with the label
// value's CASE intact — and only when the claude-code kind is on the kubernetes
// driver. A gateway-only deployment's driver gets none, which is what keeps it
// from ever needing networkpolicies RBAC.
func TestTheNetworkPolicySelectorIsReadFromTheEnvironmentAndReachesTheDriver(t *testing.T) {
	env := map[string]string{
		envAgentProvisioner:            "kubernetes",
		envAgentKinds:                  "gateway,claude-code",
		envAgentNetpolFromNS:           " control-plane-7 ",
		envAgentNetpolFromLabels:       " app=Muster-Server , example.test/tier=control ",
		envAgentCCImage:                "ghcr.io/example-org/cc:1",
		envAgentCCAccounts:             "work",
		envAgentCCTokenPrefix + "WORK": fakeTokWork,
	}
	cfg, err := loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AgentNetpolFromNS != "control-plane-7" || cfg.AgentNetpolFromLabels != "app=Muster-Server , example.test/tier=control" {
		t.Fatalf("read %q / %q", cfg.AgentNetpolFromNS, cfg.AgentNetpolFromLabels)
	}
	logger := log.New(&strings.Builder{}, "", 0)
	np := k8sDriverConfig(cfg, logger).NetworkPolicy
	if np == nil {
		t.Fatal("claude-code on kubernetes: the driver was given no NetworkPolicy configuration, so it would refuse every claude-code spec")
	}
	if np.ControllerNamespace != "control-plane-7" {
		t.Errorf("ControllerNamespace = %q", np.ControllerNamespace)
	}
	if len(np.ControllerPodLabels) != 2 || np.ControllerPodLabels["app"] != "Muster-Server" || np.ControllerPodLabels["example.test/tier"] != "control" {
		t.Errorf("ControllerPodLabels = %v, want app=Muster-Server and example.test/tier=control exactly", np.ControllerPodLabels)
	}
	if np.DNSNamespace != "" || np.DNSPodLabels != nil {
		t.Errorf("the DNS selector is not configurable from the environment and must be left to the driver's default: %q %v", np.DNSNamespace, np.DNSPodLabels)
	}

	// Neither a gateway-only kubernetes deployment nor a claude-code noop one
	// configures the driver for it.
	if got := k8sDriverConfig(provisionerTestConfig(provisionerK8s), logger).NetworkPolicy; got != nil {
		t.Errorf("a gateway-only deployment's driver was configured for NetworkPolicies: %+v", got)
	}
	if got := k8sDriverConfig(ccTestConfig(), logger).NetworkPolicy; got != nil {
		t.Errorf("a noop claude-code deployment produced a NetworkPolicy configuration: %+v", got)
	}
}

// TestTheNetworkBannerLineSaysWhetherAgentsAreConfined renders both arms of the
// `claude-code network:` line. The kubernetes arm names the selector and both
// variables and says a pre-existing agent has no policy; the other arm says NOT
// CONFINED. A gateway-only deployment prints neither.
func TestTheNetworkBannerLineSaysWhetherAgentsAreConfined(t *testing.T) {
	logger := log.New(&strings.Builder{}, "", 0)
	render := func(c config) string {
		t.Helper()
		// The provisioner is built on the noop driver in both arms: the banner
		// reads the CONFIG, and the kubernetes driver needs a pod to construct.
		build := c
		build.AgentProvisioner = provisionerNoop
		build.AgentNetpolFromNS, build.AgentNetpolFromLabels = "", ""
		pool, err := buildClaudePool(build, envPlane(t, build), memMarks{})
		if err != nil {
			t.Fatal(err)
		}
		prov, _, err := buildAgentPlaneWith(build, stubStore{}, pool, nil, logger)
		if err != nil {
			t.Fatal(err)
		}
		return lineNaming(renderBanner(t, c, api.Extensions{Provisioner: prov}), "claude-code network:")
	}
	on := render(ccK8sTestConfig())
	for _, want := range []string{
		"a NetworkPolicy is WRITTEN per agent", "[app=Muster-Server, tier=control]", "namespace control-plane-7",
		envAgentNetpolFromNS, envAgentNetpolFromLabels, "TCP 443",
		"an agent created before this build has NO policy", "until that agent is stopped and started",
		"Enforcement is the cluster network plugin's",
		"needs get/create/update/delete on networkpolicies.networking.k8s.io", "to DESTROY ANY agent",
	} {
		if !strings.Contains(on, want) {
			t.Errorf("kubernetes arm does not say %q:\n  %s", want, on)
		}
	}
	off := render(ccTestConfig())
	for _, want := range []string{"NOT CONFINED", envAgentNetpolFromNS, envAgentNetpolFromLabels, "MUSTER_AGENT_PROVISIONER=noop"} {
		if !strings.Contains(off, want) {
			t.Errorf("noop arm does not say %q:\n  %s", want, off)
		}
	}
	if on == off {
		t.Fatal("the two arms print the same line")
	}
	gw := provisionerTestConfig(provisionerNoop)
	if out := renderBanner(t, gw, api.Extensions{Provisioner: bannerProvisioner(t, gw)}); strings.Contains(out, "claude-code network:") {
		t.Errorf("a gateway-only deployment prints a claude-code network line:\n%s", out)
	}
}
