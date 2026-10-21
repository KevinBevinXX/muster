package credentials

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/ccpool"
)

// Fake credentials: obviously fake, pairwise distinct, and none equal to any
// constant the code names.
const (
	envTokWork    = "sk-ant-oat01-FAKE-env-work-0000000000"
	envTokSpare   = "sk-ant-oat01-FAKE-env-spare-111111111"
	memberTok     = "sk-ant-oat01-FAKE-member-zulu-2222222"
	envORKey      = "sk-or-v1-FAKE-env-operator-key-3333333"
	memberORKey   = "sk-or-v1-FAKE-api-key-of-member-444444"
	admin         = int64(1)
	memberA       = int64(41)
	memberB       = int64(42)
	memberUnknown = int64(43)
)

// fakePersonal is the per-owner store in memory.
type fakePersonal struct {
	claude map[int64]map[string]string
	or     map[int64]string
}

func (f fakePersonal) ClaudeTokens(_ context.Context, owner int64) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range f.claude[owner] {
		out[k] = v
	}
	return out, nil
}
func (f fakePersonal) OpenRouterKey(_ context.Context, owner int64) (string, bool, error) {
	k, ok := f.or[owner]
	return k, ok, nil
}
func (f fakePersonal) HasOpenRouterKey(_ context.Context, owner int64) (bool, error) {
	_, ok := f.or[owner]
	return ok, nil
}

// fakeRoles: user 1 admin, 41/42 members, anything else unknown (an error).
type fakeRoles struct{ sawSystem *bool }

func (r fakeRoles) Role(ctx context.Context, id int64) (authz.Role, error) {
	if p, ok := authz.From(ctx); ok && p.All() && r.sawSystem != nil {
		*r.sawSystem = true
	}
	switch id {
	case admin:
		return authz.RoleAdmin, nil
	case memberA, memberB:
		return authz.RoleMember, nil
	}
	return "", errors.New("no such user")
}

type noMarks struct{}

func (noMarks) Marks(context.Context, int64) (map[string]ccpool.Mark, error) { return nil, nil }
func (noMarks) LiveCounts(context.Context, int64) (map[string]int, error)    { return nil, nil }
func (noMarks) MarkRateLimited(context.Context, int64, string, string, time.Time) error {
	return nil
}
func (noMarks) MarkAuthFailed(context.Context, int64, string, string, string, time.Time) error {
	return nil
}

func testPlane(t *testing.T) (*Plane, *ccpool.Pool) {
	t.Helper()
	env, err := ccpool.NewStatic(map[string]string{"work": envTokWork, "spare": envTokSpare})
	if err != nil {
		t.Fatal(err)
	}
	p := &Plane{
		EnvClaude:        env,
		EnvOpenRouterKey: envORKey,
		Personal: fakePersonal{
			claude: map[int64]map[string]string{memberA: {"zulu": memberTok}},
			or:     map[int64]string{memberA: memberORKey},
		},
		Roles: fakeRoles{},
	}
	pool, err := ccpool.New(p, noMarks{})
	if err != nil {
		t.Fatal(err)
	}
	return p, pool
}

// TestAMemberDispatchNeverSelectsAnAdminPoolAccount is the operator's required
// guard: across many automatic selections and every pin, a member's dispatch
// lands only on the member's own account — and a member with NO account is
// refused, never handed the environment pool. The token a member's pod would be
// built with is the member's, and the environment's tokens are unreachable.
func TestAMemberDispatchNeverSelectsAnAdminPoolAccount(t *testing.T) {
	_, pool := testPlane(t)
	ctx := context.Background()
	envNames := map[string]bool{"work": true, "spare": true}

	for i := 0; i < 200; i++ {
		got, err := pool.Select(ctx, memberA, "")
		if err != nil {
			t.Fatalf("dispatch %d: %v", i, err)
		}
		if envNames[got] || got != "zulu" {
			t.Fatalf("dispatch %d: member A was given account %q; want only their own (zulu)", i, got)
		}
	}
	for pin := range envNames {
		if got, err := pool.Select(ctx, memberA, pin); err == nil {
			t.Fatalf("member A pinned the admin's %q and got %q", pin, got)
		}
		if tok, err := pool.Token(ctx, memberA, pin); err == nil {
			t.Fatalf("a pod of member A's on the admin's %q would be built with a token (%d bytes)", pin, len(tok))
		}
	}
	if tok, err := pool.Token(ctx, memberA, "zulu"); err != nil || tok != memberTok {
		t.Fatalf("member A's own token: err %v, right token %v", err, tok == memberTok)
	}
	if names, _ := pool.Names(ctx, memberA); strings.Join(names, ",") != "zulu" {
		t.Fatalf("member A's form lists %v; want [zulu]", names)
	}

	// A member with NO account of their own: refused, never the pool.
	for _, m := range []int64{memberB, memberUnknown} {
		got, err := pool.Select(ctx, m, "")
		if !errors.Is(err, ccpool.ErrNoUsableAccount) {
			t.Fatalf("user %d (no account) got %q, %v; want ErrNoUsableAccount", m, got, err)
		}
		if names, _ := pool.Names(ctx, m); len(names) != 0 {
			t.Fatalf("user %d's form lists %v; want none", m, names)
		}
	}

	// The control: the admin DOES select from the environment pool.
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		got, err := pool.Select(ctx, admin, "")
		if err != nil {
			t.Fatal(err)
		}
		seen[got] = true
	}
	if !seen["spare"] && !seen["work"] {
		t.Fatalf("control: the admin's selection never landed on the environment pool: %v", seen)
	}
	if tok, err := pool.Token(ctx, admin, "work"); err != nil || tok != envTokWork {
		t.Fatalf("control: the admin's pod on work: err %v", err)
	}
}

// TestOpenRouterKeySelectionIsPerOwner: the admin's gateway pods get the
// environment key; member A's get A's own API key; a
// member with none is refused with the sentence that says what to add — never
// the admin's key.
func TestOpenRouterKeySelectionIsPerOwner(t *testing.T) {
	p, _ := testPlane(t)
	ctx := context.Background()
	if k, err := p.OpenRouterKey(ctx, admin); err != nil || k != envORKey {
		t.Fatalf("admin: %v (env key: %v)", err, k == envORKey)
	}
	if k, err := p.OpenRouterKey(ctx, memberA); err != nil || k != memberORKey {
		t.Fatalf("member A: %v (own key: %v)", err, k == memberORKey)
	}
	for _, m := range []int64{memberB, memberUnknown} {
		k, err := p.OpenRouterKey(ctx, m)
		if !errors.Is(err, ErrNoCredential) || k != "" {
			t.Fatalf("user %d: got a %d-byte key, %v; want ErrNoCredential", m, len(k), err)
		}
		if !strings.Contains(err.Error(), NeedOpenRouter) {
			t.Fatalf("user %d's refusal does not say what to add: %v", m, err)
		}
	}
	// No personal store at all: a member still gets nothing.
	bare := &Plane{EnvOpenRouterKey: envORKey, Roles: fakeRoles{}}
	if k, err := bare.OpenRouterKey(ctx, memberA); !errors.Is(err, ErrNoCredential) || k != "" {
		t.Fatalf("no store: member A got a %d-byte key, %v", len(k), err)
	}
}

// TestUnavailableNamesWhatToAdd: per kind, per owner.
func TestUnavailableNamesWhatToAdd(t *testing.T) {
	p, _ := testPlane(t)
	ctx := context.Background()
	kinds := []string{agents.KindGateway, agents.KindClaudeCode, agents.KindHermes}
	if u := p.Unavailable(ctx, admin, kinds); len(u) != 0 {
		t.Fatalf("admin: %v; want everything available", u)
	}
	if u := p.Unavailable(ctx, memberA, kinds); len(u) != 0 {
		t.Fatalf("member A (both credentials): %v", u)
	}
	u := p.Unavailable(ctx, memberB, kinds)
	// The hermes kind runs on the OpenRouter key, so it needs what the gateway
	// kind needs.
	if u[agents.KindGateway] != NeedOpenRouter || u[agents.KindClaudeCode] != NeedClaude || u[agents.KindHermes] != NeedOpenRouter || len(u) != 3 {
		t.Fatalf("member B (none): %v", u)
	}
}

// TestTheRoleIsReadUnderSystemAndEveryFailureIsTheOwnersOwn: the role read
// runs as System (a member's principal cannot read another user's row), and an
// unreadable role is a MEMBER — the environment is never lent on an error.
func TestTheRoleIsReadUnderSystemAndEveryFailureIsTheOwnersOwn(t *testing.T) {
	var sys bool
	p := &Plane{EnvOpenRouterKey: envORKey, Roles: fakeRoles{sawSystem: &sys}}
	member := authz.WithPrincipal(context.Background(), authz.Principal{UserID: memberA, Role: authz.RoleMember, Via: authz.ViaSession})
	if !p.UsesEnvironment(member, admin) {
		t.Fatal("control: the admin does not use the environment")
	}
	if !sys {
		t.Fatal("the role was not read under authz.System")
	}
	if p.UsesEnvironment(context.Background(), memberUnknown) || p.UsesEnvironment(context.Background(), 0) {
		t.Fatal("an unreadable role (or no owner) was given the environment's credentials")
	}
	nilRoles := &Plane{}
	if !nilRoles.UsesEnvironment(context.Background(), authz.OperatorUserID) || nilRoles.UsesEnvironment(context.Background(), memberA) {
		t.Fatal("with no role store, only user 1 may use the environment")
	}
}

func TestShapeValidationNeverEchoesTheValue(t *testing.T) {
	good := map[string]func(string) error{
		"sk-ant-oat01-AbCdEf_0123456789-xyz": ValidateClaudeToken,
		"sk-or-v1-0123456789abcdef0123":      ValidateOpenRouterKey,
	}
	for v, f := range good {
		if err := f(v); err != nil {
			t.Errorf("%T rejected a well-shaped value: %v", f, err)
		}
	}
	for _, c := range []struct {
		v string
		f func(string) error
	}{
		{"sk-or-v1-0123456789abcdef0123", ValidateClaudeToken},
		{"sk-ant-oat01-short", ValidateClaudeToken},
		{"sk-ant-oat01-0123456789abcdef hidden-space", ValidateClaudeToken},
		{"sk-ant-oat01-0123456789abcdef\n", ValidateClaudeToken},
		{"sk-ant-oat01-0123456789abcdef0123", ValidateOpenRouterKey},
	} {
		err := c.f(c.v)
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: %v; want ErrInvalid", c.v, err)
			continue
		}
		if strings.Contains(err.Error(), c.v[len(c.v)-6:]) {
			t.Errorf("the refusal echoes the value: %v", err)
		}
	}
	if err := ValidateAccountName("Work"); !errors.Is(err, ErrInvalid) {
		t.Errorf("an uppercase account name: %v", err)
	}
}
