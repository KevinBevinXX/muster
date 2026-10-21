package agents

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestSetGatewaySecretRoundTripsAndStaysOutOfTheJSON: on real Postgres, a new
// row has no turn credential (”), SetGatewaySecret stores one that every read
// path returns (Get, GetByName, GetByHooksToken, List), only for THAT row, and
// the row's JSON never carries it.
func TestSetGatewaySecretRoundTripsAndStaysOutOfTheJSON(t *testing.T) {
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 30*time.Second)
	defer cancel()
	store := requirePGStore(t, ctx)

	stamp := time.Now().Format("150405.000000")
	mk := func(name, tok string) Agent {
		a, err := store.Create(ctx, Agent{Name: name, Namespace: "muster-agent-" + name,
			Status: StatusProvisioning, HooksToken: tok, Kind: KindClaudeCode, CCAccount: "acct"})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		t.Cleanup(func() { cleanupAgent(dbtest.AsOperator(context.Background()), store.pool, a.ID) })
		return a
	}
	a := mk("turn-cred-a-"+stamp, "fixture-hooks-a-"+stamp)
	b := mk("turn-cred-b-"+stamp, "fixture-hooks-b-"+stamp)
	if a.GatewaySecret != "" || b.GatewaySecret != "" {
		t.Fatalf("a new row carries a turn credential: %q %q", a.GatewaySecret, b.GatewaySecret)
	}

	const secret = "fixture-turn-credential-pg-5e21"
	if err := store.SetGatewaySecret(ctx, a.ID, secret); err != nil {
		t.Fatal(err)
	}
	reads := map[string]func() (Agent, error){
		"Get":             func() (Agent, error) { return store.Get(ctx, a.ID) },
		"GetByName":       func() (Agent, error) { return store.GetByName(ctx, a.Name) },
		"GetByHooksToken": func() (Agent, error) { return store.GetByHooksToken(ctx, a.HooksToken) },
	}
	for what, read := range reads {
		got, err := read()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if got.GatewaySecret != secret {
			t.Errorf("%s returned turn credential %q, want the stored one", what, got.GatewaySecret)
		}
		if blob, _ := json.Marshal(got); strings.Contains(string(blob), secret) {
			t.Errorf("%s: the row's JSON carries the turn credential", what)
		}
	}
	other, err := store.Get(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if other.GatewaySecret != "" {
		t.Errorf("SetGatewaySecret on one row set another's: %q", other.GatewaySecret)
	}
}
