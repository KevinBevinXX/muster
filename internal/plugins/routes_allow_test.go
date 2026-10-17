package plugins

import "testing"

func TestTheRelayAllowlistIsPerOwnerAndExact(t *testing.T) {
	l, err := ParseRelayAllowlist(" 1:mail:Alice@Example.invalid , 1:mail:*@corp.example.invalid,2:mail:bob@example.invalid ")
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
		if got := l.Allows(c.owner, "mail", c.sender, true); got != c.want {
			t.Errorf("owner %d, %q: %v, want %v", c.owner, c.sender, got, c.want)
		}
	}
	empty, err := ParseRelayAllowlist("")
	if err != nil || empty.Allows(1, "mail", "alice@example.invalid", true) {
		t.Fatal("an empty list allows something")
	}
	for _, bad := range []string{
		"alice@example.invalid", "1:alice@example.invalid", // no plugin: the old format is refused, not misread
		"x:mail:a@b", "0:mail:a@b", "1:mail:a", "1:mail:*@*", "1:mail:a*@b", "1:mail:@b", "1:mail:a@",
		"1:Not A Plugin:a@b", "1::a@b",
		"1:mail:Kate@example.invalid", // the Kelvin sign: not ASCII, refused
		"1:mail:ada@exämple.invalid",
	} {
		if _, err := ParseRelayAllowlist(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

// 🔴 R1: an entry matches only mail its OWN plugin delivered and marked
// verified. The same From line through another plugin, or unverified, waits
// for the owner.
func TestTheRelayAllowlistIsBoundToAPluginAndAVerifiedSender(t *testing.T) {
	l, err := ParseRelayAllowlist("1:mail:ada@example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	if !l.Allows(1, "mail", "Ada <ada@example.invalid>", true) {
		t.Fatal("the good path: a verified sender through its own plugin is not allowed")
	}
	if l.Allows(1, "webhook", "Ada <ada@example.invalid>", true) {
		t.Error("a SECOND plugin that claims the same From (and even says verified) is allowed")
	}
	if l.Allows(1, "mail", "Ada <ada@example.invalid>", false) {
		t.Error("an UNVERIFIED sender is allowed")
	}
}

// 🔴 R1: folding is ASCII's alone. strings.ToLower folds the Kelvin sign
// (U+212A) to "k", so a Unicode fold would let "Kate" with a Kelvin K match
// kate@… on the list; a non-ASCII address matches nothing.
func TestAKelvinSignNeverMatchesAnAllowlistedK(t *testing.T) {
	l, err := ParseRelayAllowlist("1:mail:kate@example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	if !l.Allows(1, "mail", "kate@example.invalid", true) {
		t.Fatal("the control: the ASCII address is not allowed")
	}
	for _, spoof := range []string{"Kate@example.invalid", "KATE@example.invalid", "Kate <Kate@example.invalid>"} {
		if l.Allows(1, "mail", spoof, true) {
			t.Errorf("%q (a Kelvin sign) matches kate@example.invalid", spoof)
		}
	}
}
