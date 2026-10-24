package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/api"
)

// liveHermesEnv is the shape of the live deployment's agent-kind environment at
// the time the hermes kind was removed: the kind still listed and its image
// still set, on the kubernetes driver with claude-code enabled. Every value is
// a fixture; only the variable NAMES and the KINDS list are the live ones.
func liveHermesEnv() map[string]string {
	return map[string]string{
		envDatabase:                    "postgres://unused",
		envAgentProvisioner:            provisionerK8s,
		envAgentImageRepo:              "registry.example.test/muster/agent-runtime",
		envAgentAPIURL:                 "http://muster.example.test:8105",
		envAgentKinds:                  "gateway,claude-code,hermes",
		envAgentHermesImage:            "ghcr.io/example-org/hermes-agent:hermes-5fc490e2",
		envAgentCCImage:                "ghcr.io/example-org/claude-code-agent:0a1b2c3",
		envAgentCCAccounts:             "work",
		envAgentCCTokenPrefix + "WORK": fakeTokWork,
		envAgentNetpolFromNS:           "control-plane-7",
		envAgentNetpolFromLabels:       "app=muster",
	}
}

// TestTheLiveEnvWithALeftoverHermesKindBootsAndWarns: the binary BOOTS on the
// live deployment's current env (MUSTER_AGENT_KINDS=gateway,claude-code,hermes
// plus MUSTER_AGENT_HERMES_IMAGE) — it must not crash-loop its first rollout —
// with hermes dropped from the kinds it offers, and the banner says, loudly and
// by NAME (never the image value), what it ignored.
//
// RED before the kind's removal: hermes was enabled (offered by the picker) and
// no warning was printed.
func TestTheLiveEnvWithALeftoverHermesKindBootsAndWarns(t *testing.T) {
	env := liveHermesEnv()
	cfg, err := loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("loadConfig refused the live env: %v", err)
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate refused the live env (the rollout would crash-loop): %v", err)
	}
	if got := strings.Join(cfg.agentKinds(), ","); got != "gateway,claude-code" {
		t.Fatalf("agent kinds = %s, want gateway,claude-code", got)
	}
	if got := (kindSet{kinds: cfg.agentKinds()}).Enabled(); slices.Contains(got, "hermes") || len(got) != 2 {
		t.Fatalf("the picker's kinds = %v", got)
	}
	out := renderBanner(t, cfg, api.Extensions{})
	warn := lineNaming(out, "WARNING — agent kinds: IGNORING")
	if !strings.Contains(warn, envAgentKinds+` entry "hermes"`) || !strings.Contains(warn, envAgentHermesImage) ||
		!strings.Contains(warn, "can only be stopped or destroyed") {
		t.Fatalf("no loud warning naming the ignored settings; warning line %q in:\n%s", warn, out)
	}
	if strings.Contains(out, env[envAgentHermesImage]) {
		t.Fatalf("the banner prints the ignored image's value:\n%s", out)
	}

	// The control: the same env without the leftovers boots with no warning.
	delete(env, envAgentHermesImage)
	env[envAgentKinds] = "gateway,claude-code"
	clean, err := loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if err := clean.validate(); err != nil {
		t.Fatalf("control: %v", err)
	}
	if out := renderBanner(t, clean, api.Extensions{}); strings.Contains(out, "IGNORING") {
		t.Fatalf("control: a clean env warns:\n%s", out)
	}
}
