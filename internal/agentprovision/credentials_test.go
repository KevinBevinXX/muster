package agentprovision

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/provision"
)

// ownerCreds is a CredentialResolver over fixed per-owner maps.
type ownerCreds struct {
	claude map[int64]map[string]string
	or     map[int64]string
	asked  []string
}

func (c *ownerCreds) ClaudeToken(_ context.Context, owner int64, account string) (string, error) {
	c.asked = append(c.asked, fmt.Sprintf("claude:%d:%s", owner, account))
	if tok, ok := c.claude[owner][account]; ok {
		return tok, nil
	}
	return "", errors.New("no such account")
}

func (c *ownerCreds) OpenRouterKey(_ context.Context, owner int64) (string, error) {
	c.asked = append(c.asked, fmt.Sprintf("openrouter:%d", owner))
	if k, ok := c.or[owner]; ok {
		return k, nil
	}
	return "", errors.New("no credential: add your OpenRouter key")
}

const (
	envTok    = "sk-ant-oat01-FAKE-provision-ENV-0000"
	memberTok = "sk-ant-oat01-FAKE-provision-MEMBER-1"
	envKey    = "sk-or-v1-FAKE-provision-ENV-key-2222"
	memberKey = "sk-or-v1-FAKE-provision-MEMBER-key-3"
)

func secretsOf(spec provision.Spec) string {
	var b strings.Builder
	for _, e := range spec.Secrets {
		b.WriteString(e.Name + "=" + e.Value + "\n")
	}
	for _, e := range spec.Env {
		b.WriteString(e.Name + "=" + e.Value + "\n")
	}
	return b.String()
}

// TestASpecIsBuiltFromItsOwnersCredentialsOnly: the deployment-wide config
// carries the ENVIRONMENT's token and key (the admin's). A member's claude-code
// and gateway agents are built with the member's own — and no spec of the
// member's carries an environment value; a member with no credential is
// refused, never built from the environment. The admin's agent gets the
// environment's (the control).
func TestASpecIsBuiltFromItsOwnersCredentialsOnly(t *testing.T) {
	creds := &ownerCreds{
		claude: map[int64]map[string]string{1: {"work": envTok}, 41: {"work": memberTok}},
		or:     map[int64]string{1: envKey, 41: memberKey},
	}
	cfg := fixtureSpecConfig()
	cfg.OpenRouterAPIKey = envKey
	cfg.ClaudeCode = &agentspec.ClaudeCodeConfig{Image: "ghcr.io/example-org/claude-code-agent:1", Accounts: map[string]string{"work": envTok, "spare": envTok + "-spare"}}
	a, err := New(Config{Driver: provision.MustNewNoop(), Store: &recordingStore{}, Spec: cfg, Credentials: creds, KickoffDeliverable: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	row := func(owner int64, kind string) agents.Agent {
		r := fixtureAgent()
		r.OwnerID, r.Kind, r.HooksToken = owner, kind, "fixture-hooks-owner"
		if kind == agents.KindClaudeCode {
			r.CCAccount = "work"
		}
		return r
	}
	build := func(owner int64, kind string) (string, error) {
		spec, err := a.buildSpec(ctx, row(owner, kind))
		return secretsOf(spec), err
	}

	for _, kind := range []string{agents.KindClaudeCode, agents.KindGateway} {
		got, err := build(41, kind)
		if err != nil {
			t.Fatalf("member %s: %v", kind, err)
		}
		if strings.Contains(got, envTok) || strings.Contains(got, envKey) {
			t.Fatalf("a MEMBER's %s spec carries an environment credential:\n%s", kind, got)
		}
		want := map[string]string{agents.KindClaudeCode: agentspec.EnvClaudeOAuthToken + "=" + memberTok,
			agents.KindGateway: agentspec.EnvOpenRouterKey + "=" + memberKey}[kind]
		if !strings.Contains(got, want) {
			t.Fatalf("member %s spec lacks its own credential (%s):\n%s", kind, want, got)
		}
	}
	// No credential: refused, and never the environment's.
	for _, kind := range []string{agents.KindClaudeCode, agents.KindGateway} {
		got, err := build(42, kind)
		if err == nil {
			t.Fatalf("member 42 (no credential) %s: built a spec:\n%s", kind, got)
		}
		if strings.Contains(err.Error(), envTok) || strings.Contains(err.Error(), envKey) {
			t.Fatalf("the refusal carries a credential: %v", err)
		}
	}
	// The control: the admin's agents get the environment's.
	if got, err := build(1, agents.KindClaudeCode); err != nil || !strings.Contains(got, envTok) {
		t.Fatalf("admin claude-code: %v\n%s", err, got)
	}
	if got, err := build(1, agents.KindGateway); err != nil || !strings.Contains(got, envKey) {
		t.Fatalf("admin gateway: %v\n%s", err, got)
	}
	// The CONFIG a member's spec is built from carries none of the
	// environment's credentials either — replaced, not merged — so a kind that
	// read both (or another account name) still could not reach the admin's.
	for _, kind := range []string{agents.KindClaudeCode, agents.KindGateway} {
		c, err := a.specFor(ctx, row(41, kind))
		if err != nil {
			t.Fatal(err)
		}
		dump := fmt.Sprintf("%v %q", c.ClaudeCode.Accounts, c.OpenRouterAPIKey)
		if strings.Contains(dump, envTok) || strings.Contains(dump, envKey) {
			t.Fatalf("member %s spec config carries an environment credential: %s", kind, dump)
		}
	}
	if c, _ := a.specFor(ctx, row(41, agents.KindClaudeCode)); len(c.ClaudeCode.Accounts) != 1 || c.OpenRouterAPIKey != "" {
		t.Fatalf("member claude-code config: accounts %d, openrouter key set %v", len(c.ClaudeCode.Accounts), c.OpenRouterAPIKey != "")
	}
}

// specSpy is a Noop that remembers the last spec it was handed to create or
// update — the Secret a pod would boot with.
type specSpy struct {
	*provision.Noop
	last provision.Spec
	n    int
}

func (d *specSpy) Create(ctx context.Context, spec provision.Spec) error {
	d.last, d.n = spec, d.n+1
	return d.Noop.Create(ctx, spec)
}

func (d *specSpy) Update(ctx context.Context, spec provision.Spec) error {
	d.last, d.n = spec, d.n+1
	return d.Noop.Update(ctx, spec)
}

// TestAGatewayStartPicksUpARotatedKeyAndRefusesADeletedOne (round-1 audit on
// #69): a member's gateway agent dispatched on key OLD; the member saves NEW;
// Stop then Start must hand the pod NEW (and ask the owner's credentials to get
// it). The member deletes the key; Stop then Start must REFUSE, not revive the
// pod on the deleted key. Before the fix a gateway Start only scaled: the pod
// kept OLD and the credential lookup was never made.
func TestAGatewayStartPicksUpARotatedKeyAndRefusesADeletedOne(t *testing.T) {
	const oldKey, newKey = "sk-or-v1-FAKE-rotate-OLD-key-1111111", "sk-or-v1-FAKE-rotate-NEW-key-2222222"
	ctx := context.Background()
	// setup: a member's gateway agent, dispatched (kickoff) on OLD.
	setup := func(t *testing.T) (*Adapter, *ownerCreds, *specSpy, agents.Agent) {
		t.Helper()
		creds := &ownerCreds{or: map[int64]string{41: oldKey}}
		row := fixtureAgent()
		row.OwnerID, row.Kind = 41, agents.KindGateway
		drv := &specSpy{Noop: provision.MustNewNoop()}
		a, err := New(Config{Driver: drv, Store: &recordingStore{agent: row}, Spec: fixtureSpecConfig(), Credentials: creds, KickoffDeliverable: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Dispatch(ctx, row.ID, true); err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		if !strings.Contains(secretsOf(drv.last), oldKey) {
			t.Fatalf("control: the dispatched pod does not carry OLD:\n%s", secretsOf(drv.last))
		}
		if err := a.Stop(ctx, row.ID); err != nil {
			t.Fatal(err)
		}
		return a, creds, drv, row
	}

	t.Run("rotate", func(t *testing.T) {
		a, creds, drv, row := setup(t)
		creds.or[41] = newKey
		creds.asked = nil
		if err := a.Start(ctx, row.ID); err != nil {
			t.Fatalf("start after rotate: %v", err)
		}
		if got := secretsOf(drv.last); !strings.Contains(got, newKey) || strings.Contains(got, oldKey) {
			t.Fatalf("after rotate + Stop + Start the pod's spec is not on NEW:\n%s", got)
		}
		if len(creds.asked) == 0 {
			t.Fatal("Start never asked the owner's credentials")
		}
	})

	t.Run("delete", func(t *testing.T) {
		a, creds, drv, row := setup(t)
		delete(creds.or, 41)
		applied := drv.n
		if err := a.Start(ctx, row.ID); err == nil || !strings.Contains(err.Error(), "OpenRouter") {
			t.Fatalf("start after the key was deleted: %v; want a refusal naming the OpenRouter key", err)
		}
		if drv.n != applied {
			t.Fatal("a refused Start still applied a spec")
		}
		if inst, err := drv.Get(ctx, agents.RefOf(row)); err == nil && inst.Phase != provision.PhaseStopped {
			t.Fatalf("the pod came back on a deleted key: phase %s", inst.Phase)
		}
	})
}
