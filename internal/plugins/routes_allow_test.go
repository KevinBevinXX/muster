package plugins

import "testing"

func TestTheRelayAllowlistIsPerOwnerAndExact(t *testing.T) {
	l, err := ParseRelayAllowlist(" 1:Alice@Example.invalid , 1:*@corp.example.invalid,2:bob@example.invalid ")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		owner  int64
		sender string
		want   bool
	}{
		{1, "alice@example.invalid", true},
		{1, "Alice Smith <ALICE@example.invalid>", true},
		{1, "carol@corp.example.invalid", true},
		{1, "carol@sub.corp.example.invalid", false}, // exactly that domain
		{1, "carol@corp.example.invalid.evil.invalid", false},
		{1, "bob@example.invalid", false},   // owner 2's, not owner 1's
		{2, "alice@example.invalid", false}, // owner 1's, not owner 2's
		{2, "bob@example.invalid", true},
		{3, "alice@example.invalid", false},
		{1, "", false},
		{1, "not an address", false},
		{1, "alice@example.invalid, mallory@evil.invalid", false},
	} {
		if got := l.Allows(c.owner, c.sender); got != c.want {
			t.Errorf("owner %d, %q: %v, want %v", c.owner, c.sender, got, c.want)
		}
	}
	empty, err := ParseRelayAllowlist("")
	if err != nil || empty.Allows(1, "alice@example.invalid") {
		t.Fatal("an empty list allows something")
	}
	for _, bad := range []string{"alice@example.invalid", "x:a@b", "0:a@b", "1:a", "1:*@*", "1:a*@b", "1:@b", "1:a@"} {
		if _, err := ParseRelayAllowlist(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}
