package authz

import (
	"context"
	"strings"
	"testing"
)

func TestWithPrincipalRoundTrips(t *testing.T) {
	// Pairwise-distinct fields, none equal to OperatorUserID, so a From that
	// hardcoded any constant would be visible.
	want := Principal{UserID: 42, Role: RoleMember, Via: ViaConnect}
	got, ok := From(WithPrincipal(context.Background(), want))
	if !ok || got != want {
		t.Fatalf("From(WithPrincipal(%v)) = %v, %v; want it back", want, got, ok)
	}
	if got.All() {
		t.Fatalf("a request principal must not be scope-all")
	}
	if MustFrom(WithPrincipal(context.Background(), want)) != want {
		t.Fatalf("MustFrom disagrees with From")
	}
}

func TestNoPrincipalIsAbsentNotZero(t *testing.T) {
	if p, ok := From(context.Background()); ok {
		t.Fatalf("a bare context carries principal %v", p)
	}
	defer func() {
		if recover() == nil {
			t.Fatalf("MustFrom on a bare context did not panic")
		}
	}()
	MustFrom(context.Background())
}

func TestSystemIsScopeAllAndNoUser(t *testing.T) {
	p, ok := From(System(context.Background()))
	if !ok || !p.All() || p.Via != ViaSystem || p.UserID != 0 {
		t.Fatalf("System principal = %+v, %v; want scope-all, ViaSystem, no user", p, ok)
	}
	if p.String() != "system" {
		t.Fatalf("System principal renders %q", p.String())
	}
}

// 🔴 A REQUEST PRINCIPAL CANNOT CLAIM THE SYSTEM DOOR, NOR NO USER, NOR A ROLE
// OUTSIDE THE SET. Each is a wrapper bug that would hand PR 3's row-level
// security a principal selecting everybody or nobody.
func TestWithPrincipalRefusesMalformedPrincipals(t *testing.T) {
	for name, p := range map[string]Principal{
		"system door": {UserID: 1, Role: RoleAdmin, Via: ViaSystem},
		"no user":     {UserID: 0, Role: RoleAdmin, Via: ViaSession},
		"negative":    {UserID: -1, Role: RoleAdmin, Via: ViaSession},
		"no role":     {UserID: 1, Via: ViaSession},
		"bad role":    {UserID: 1, Role: "root", Via: ViaSession},
		"no door":     {UserID: 1, Role: RoleAdmin},
		"bad door":    {UserID: 1, Role: RoleAdmin, Via: "cookie"},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				r := recover()
				if r == nil || !strings.HasPrefix(r.(string), "authz: request principal") {
					t.Fatalf("WithPrincipal(%+v) recovered %v; want its own refusal", p, r)
				}
			}()
			WithPrincipal(context.Background(), p)
		})
	}
}

// 🔴 THE COMPILE-LEVEL PROPERTY, AND THE LIMIT OF WHAT A TEST CAN SAY ABOUT IT.
// Outside this package principalKey cannot be named, so no other package can
// write `context.WithValue(ctx, principalKey{}, …)` — that line does not
// compile there, and no test can demonstrate a non-compiling line. What CAN be
// shown is that a look-alike key (any other type, even an identically-shaped
// one) is not read by From.
func TestALookAlikeKeyIsNotAPrincipal(t *testing.T) {
	type principalKey struct{} // same name and shape, different type
	ctx := context.WithValue(context.Background(), principalKey{}, Principal{UserID: 1, Role: RoleAdmin, Via: ViaSession})
	if p, ok := From(ctx); ok {
		t.Fatalf("From read a principal planted under a foreign key: %v", p)
	}
}
