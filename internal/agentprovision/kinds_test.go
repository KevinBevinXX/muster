package agentprovision

import (
	"context"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/provision"
)

// specCapture is a Noop that remembers the last spec it was handed.
type specCapture struct {
	*provision.Noop
	updates, scales, creates int
	last                     provision.Spec
}

func (d *specCapture) Update(ctx context.Context, s provision.Spec) error {
	d.updates++
	d.last = s
	return d.Noop.Update(ctx, s)
}
func (d *specCapture) Create(ctx context.Context, s provision.Spec) error {
	d.creates++
	d.last = s
	return d.Noop.Create(ctx, s)
}
func (d *specCapture) Scale(ctx context.Context, r provision.Ref, n int) error {
	d.scales++
	return d.Noop.Scale(ctx, r, n)
}

func tokenIn(s provision.Spec) string {
	for _, e := range s.Secrets {
		if e.Name == agentspec.EnvClaudeOAuthToken {
			return e.Value
		}
	}
	return ""
}

// TestAClaudeCodeStartReappliesTheSpecWithTheAccountsCurrentToken: after an
// operator rotates an account's setup-token, Stop then Start must bring the
// agent back on the NEW token. A bare Scale (the gateway kind's Start) would
// resume the pod with the Secret it was created with — the revoked token.
func TestAClaudeCodeStartReappliesTheSpecWithTheAccountsCurrentToken(t *testing.T) {
	const oldTok, newTok = "sk-ant-oat01-FAKE-old-6b1d", "sk-ant-oat01-FAKE-new-90ce"
	row := fixtureAgent()
	row.Kind, row.CCAccount, row.HooksToken, row.Repo, row.RepoBranch, row.Model = agents.KindClaudeCode, "work", "fixture-hooks-ccprov", "", "", ""
	store := &recordingStore{agent: row}
	driver := &specCapture{Noop: provision.MustNewNoop()}

	adapterWithToken := func(tok string) *Adapter {
		cfg := fixtureSpecConfig()
		cfg.ClaudeCode = &agentspec.ClaudeCodeConfig{Image: "ghcr.io/example-org/claude-code-agent:1", Accounts: map[string]string{"work": tok}}
		a, err := New(Config{Driver: driver, Store: store, Spec: cfg, KickoffDeliverable: true})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}

	if err := adapterWithToken(oldTok).Dispatch(context.Background(), row.ID, true); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if tokenIn(driver.last) != oldTok {
		t.Fatal("control: the dispatched spec does not carry the account's token")
	}
	// The operator rotates the token (muster restarts with the new value), then
	// Stops and Starts the agent.
	a := adapterWithToken(newTok)
	if err := a.Stop(context.Background(), row.ID); err != nil {
		t.Fatal(err)
	}
	updatesBefore, scalesBefore := driver.updates, driver.scales
	if err := a.Start(context.Background(), row.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	if driver.updates != updatesBefore+1 {
		t.Fatalf("a claude-code Start did not re-apply the spec (%d update(s))", driver.updates-updatesBefore)
	}
	if got := tokenIn(driver.last); got != newTok {
		t.Fatalf("the re-applied spec carries %q, want the account's CURRENT token", got)
	}
	if driver.scales != scalesBefore {
		t.Fatalf("a claude-code Start also scaled; Update already sets the replica count")
	}
}
