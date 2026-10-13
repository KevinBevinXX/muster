package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/push"
)

// TestHalfAVAPIDConfigurationRefusesBoot: the three MUSTER_VAPID_* variables
// are all-or-nothing. Every one-or-two subset refuses boot, naming all three
// and printing no value; none is push off; all three (a working pair) boots;
// three that are not a working identity refuse.
func TestHalfAVAPIDConfigurationRefusesBoot(t *testing.T) {
	priv, pub, err := push.GenerateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}
	base := func() config { return config{Database: "postgres://x/y", Standalone: true} }
	full := map[string]string{envVAPIDPublicKey: pub, envVAPIDPrivateKey: priv, envVAPIDSubject: "mailto:ops@example.com"}
	names := []string{envVAPIDPublicKey, envVAPIDPrivateKey, envVAPIDSubject}

	load := func(set map[string]string) config {
		c, err := loadConfig(func(k string) string {
			switch k {
			case envDatabase:
				return "postgres://x/y"
			case envStandalone:
				return "1"
			}
			return set[k]
		})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	for mask := 1; mask < 7; mask++ { // every non-empty proper subset of the three
		set := map[string]string{}
		for i, n := range names {
			if mask&(1<<i) != 0 {
				set[n] = full[n]
			}
		}
		err := load(set).validate()
		if err == nil {
			t.Errorf("subset %v booted: half a push configuration must refuse", set)
			continue
		}
		// THIS guard's refusal, not Validate's (a missing key also fails to
		// decode, which would refuse for the wrong reason and say less).
		if !strings.Contains(err.Error(), "are a triple and only") {
			t.Errorf("subset %v refused, but not by the all-or-nothing check: %v", set, err)
		}
		for _, n := range names {
			if !strings.Contains(err.Error(), n) {
				t.Errorf("subset %v: the refusal does not name %s: %v", set, n, err)
			}
		}
		if strings.Contains(err.Error(), priv) || strings.Contains(err.Error(), pub) {
			t.Errorf("subset %v: the refusal prints a key", set)
		}
	}

	if err := base().validate(); err != nil {
		t.Errorf("no VAPID variables (push off) refused: %v", err)
	}
	c := load(full)
	if err := c.validate(); err != nil || !c.pushConfigured() {
		t.Fatalf("a full, working triple: validate=%v configured=%v", err, c.pushConfigured())
	}

	otherPriv, _, _ := push.GenerateVAPIDKeys()
	for name, set := range map[string]map[string]string{
		"mismatched pair": {envVAPIDPublicKey: pub, envVAPIDPrivateKey: otherPriv, envVAPIDSubject: "mailto:ops@example.com"},
		"bare subject":    {envVAPIDPublicKey: pub, envVAPIDPrivateKey: priv, envVAPIDSubject: "ops@example.com"},
	} {
		err := load(set).validate()
		if err == nil {
			t.Errorf("%s booted", name)
			continue
		}
		if strings.Contains(err.Error(), priv) || strings.Contains(err.Error(), otherPriv) {
			t.Errorf("%s: the refusal prints the private key", name)
		}
	}
}

// TestVAPIDGenerateEmitsAWorkingPairAsEnvLines: `muster-server vapid generate`
// prints exactly the two env lines, and the pair it prints passes the same
// validation boot applies.
func TestVAPIDGenerateEmitsAWorkingPairAsEnvLines(t *testing.T) {
	var out, errOut bytes.Buffer
	if rc := runVAPIDCLI([]string{"generate"}, &out, &errOut); rc != 0 {
		t.Fatalf("rc %d: %s", rc, errOut.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines on stdout, got %q", out.String())
	}
	got := map[string]string{}
	for _, l := range lines {
		k, v, ok := strings.Cut(l, "=")
		if !ok || v == "" {
			t.Fatalf("not an env line: %q", l)
		}
		got[k] = v
	}
	vc := push.VAPIDConfig{PublicKey: got[envVAPIDPublicKey], PrivateKey: got[envVAPIDPrivateKey], Subject: "mailto:ops@example.com"}
	if err := vc.Validate(); err != nil {
		t.Fatalf("the generated pair does not validate: %v", err)
	}
	if strings.Contains(errOut.String(), got[envVAPIDPrivateKey]) {
		t.Error("the private key reached stderr")
	}
	if !strings.Contains(errOut.String(), envVAPIDSubject) {
		t.Errorf("stderr does not tell the operator to set %s: %q", envVAPIDSubject, errOut.String())
	}
	if rc := runVAPIDCLI(nil, &out, &errOut); rc != 2 {
		t.Errorf("no subcommand: rc %d, want 2", rc)
	}
}
