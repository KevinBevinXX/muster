package plugins

import (
	"strings"
	"testing"
)

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
		{1, "x<carol@corp.example.invalid", false}, // a From that did not parse is not an address at corp
		{1, "carol@corp.example.invalid>", false},
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
		"1:mail:<ada@example.invalid>", "1:mail:x<a@corp.example.invalid",
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

// 🔴 Agent-mail senders ("<owner>/<handle>", the agent-mail plugin's verified
// "from") are allowlisted exactly or per owner ("<owner>/*"), in the protocol's
// own lower-case ASCII grammar with NO folding, and never across kinds: a mail
// entry does not match an agent-mail sender, nor the reverse.
func TestTheRelayAllowlistMatchesAgentMailAddresses(t *testing.T) {
	l, err := ParseRelayAllowlist("1:agent-mail:ada-lab/agent-infra, 1:agent-mail:x/*, 1:mail:*@corp.example.invalid, 2:agent-mail:hq/tasks")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name   string
		owner  int64
		plugin string
		sender string
		want   bool
	}{
		{"exact", 1, "agent-mail", "ada-lab/agent-infra", true},
		{"exact, padded", 1, "agent-mail", "  ada-lab/agent-infra ", true},
		{"exact: another handle of that owner", 1, "agent-mail", "ada-lab/other", false},
		{"exact: a prefix of the handle", 1, "agent-mail", "ada-lab/agent", false},
		{"exact: a longer handle", 1, "agent-mail", "ada-lab/agent-infra-2", false},
		{"owner wildcard", 1, "agent-mail", "x/anyone", true},
		{"owner wildcard, another handle", 1, "agent-mail", "x/z9", true},
		{"x/* is not x-y/h", 1, "agent-mail", "x-y/h1", false},
		{"x/* is not xy/h", 1, "agent-mail", "xy/h1", false},
		{"x/* is not a nested path", 1, "agent-mail", "x/h1/h2", false},
		{"upper-case handle: not folded", 1, "agent-mail", "ada-lab/Agent-infra", false},
		{"upper-case owner: not folded", 1, "agent-mail", "Ada-lab/agent-infra", false},
		{"upper-case owner under a wildcard", 1, "agent-mail", "X/anyone", false},
		{"a Kelvin-sign look-alike", 1, "agent-mail", "x/Kate", false},
		{"a Cyrillic look-alike owner", 1, "agent-mail", "х/anyone", false},
		{"a bracketed agent address", 1, "agent-mail", "Ada <ada-lab/agent-infra>", false},
		{"another owner's entry", 2, "agent-mail", "ada-lab/agent-infra", false},
		{"owner 2's exact entry", 2, "agent-mail", "hq/tasks", true},
		{"another plugin", 1, "mail", "ada-lab/agent-infra", false},
		{"a mail entry never matches an agent sender", 1, "mail", "corp.example.invalid/x1", false},
		{"an agent wildcard never matches mail", 1, "agent-mail", "x@corp.example.invalid", false},
		{"an agent wildcard never matches mail with a slash", 1, "agent-mail", "x/anyone@corp.example.invalid", false},
		{"the mail wildcard still matches mail", 1, "mail", "carol@corp.example.invalid", true},
	} {
		if got := l.Allows(c.owner, c.plugin, c.sender, true); got != c.want {
			t.Errorf("%s: owner %d, %s, %q: %v, want %v", c.name, c.owner, c.plugin, c.sender, got, c.want)
		}
	}
	for _, sender := range []string{"ada-lab/agent-infra", "x/anyone", "hq/tasks"} {
		for _, owner := range []int64{1, 2} {
			if l.Allows(owner, "agent-mail", sender, false) {
				t.Errorf("an UNVERIFIED agent-mail sender %q is allowed for owner %d", sender, owner)
			}
		}
	}
	// The cross-kind rule from the entry's side: an agent-mail owner wildcard
	// spelled like a mail domain matches no mail sender.
	l2, err := ParseRelayAllowlist("1:agent-mail:corp/*")
	if err != nil {
		t.Fatal(err)
	}
	if !l2.Allows(1, "agent-mail", "corp/b1", true) {
		t.Fatal("the control: corp/* does not match corp/b1")
	}
	for _, s := range []string{"a/b@corp", "corp@x.invalid", "corp/b1@x.invalid"} {
		if l2.Allows(1, "agent-mail", s, true) {
			t.Errorf("the agent-mail wildcard corp/* matches the mail address %q", s)
		}
	}
}

func TestTheRelayAllowlistRefusesMalformedAgentMailEntries(t *testing.T) {
	for _, good := range []string{"1:agent-mail:example-owner/*", "1:agent-mail:hq/tasks", "1:agent-mail:0x/ab"} {
		if _, err := ParseRelayAllowlist(good); err != nil {
			t.Errorf("%q: %v", good, err)
		}
	}
	for _, bad := range []string{
		"1:agent-mail:Example-owner/*", // upper case is refused, not folded
		"1:agent-mail:example-owner/Agent",
		"1:agent-mail:*/*", "1:agent-mail:*/tasks", // no any-owner wildcard
		"1:agent-mail:hq/ta*", "1:agent-mail:hq/*s",
		"1:agent-mail:/tasks", "1:agent-mail:hq/", "1:agent-mail:hq",
		"1:agent-mail:-hq/tasks",     // the owner starts with a letter or digit
		"1:agent-mail:hq/1tasks",     // the handle starts with a letter
		"1:agent-mail:hq/t",          // the handle is at least two characters
		"1:agent-mail:hq/tasks/more", // exactly one slash
		"1:agent-mail:h_q/tasks", "1:agent-mail:hq/ta_sks", "1:agent-mail:h.q/tasks",
		"1:agent-mail:хq/tasks", // a Cyrillic look-alike
		"1:agent-mail:hq/Kasks", // the Kelvin sign
		"1:agent-mail:" + strings.Repeat("a", 40) + "/tasks",
		"1:agent-mail:hq/" + strings.Repeat("a", 33),
	} {
		if _, err := ParseRelayAllowlist(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}
