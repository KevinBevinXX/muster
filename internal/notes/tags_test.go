package notes

import (
	"strings"
	"testing"
)

// TestNormalizeTags is the table test for the whole normalization grammar:
// case-folding, whitespace→'-', dedupe, sort, and the SILENT drop of
// empty/whitespace-only entries. Normalization is deliberately TOTAL (never
// errors) — loud failure is ValidateTags' job — so this asserts shape only.
func TestNormalizeTags(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil is an empty non-nil slice", nil, []string{}},
		{"lowercases", []string{"Frontend", "CIVITAI"}, []string{"civitai", "frontend"}},
		{"trims", []string{"  spaced  "}, []string{"spaced"}},
		{"collapses internal whitespace to a dash", []string{"needs   review"}, []string{"needs-review"}},
		{"collapses tabs/newlines too", []string{"a\t\nb"}, []string{"a-b"}},
		{"dedupes after normalizing", []string{"Bug", "bug", " bug "}, []string{"bug"}},
		{"sorts", []string{"zeta", "alpha", "mid"}, []string{"alpha", "mid", "zeta"}},
		{"drops empty and whitespace-only silently", []string{"", "   ", "\t", "keep"}, []string{"keep"}},
		{"keeps a ns:value tag intact", []string{"Runbook:Deploy"}, []string{"runbook:deploy"}},
		{"keeps the allowed charset", []string{"a.b_c/d-e0"}, []string{"a.b_c/d-e0"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeTags(tc.in)
			if got == nil {
				t.Fatalf("NormalizeTags must never return nil (the tags column is NOT NULL)")
			}
			if len(got) != len(tc.want) {
				t.Fatalf("NormalizeTags(%q) = %q, want %q", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("NormalizeTags(%q) = %q, want %q", tc.in, got, tc.want)
				}
			}
		})
	}
}

// TestValidateTags asserts the LOUD half: every grammar violation is an error
// that NAMES the offending tag (a routing tag silently ignored is the exact
// failure mode tags must avoid), and legal input passes.
func TestValidateTags(t *testing.T) {
	long := strings.Repeat("a", MaxTagRunes+1)
	many := make([]string, MaxTags+1)
	for i := range many {
		many[i] = string(rune('a'+i%26)) + string(rune('0'+i/26))
	}

	t.Run("valid input passes", func(t *testing.T) {
		if err := ValidateTags([]string{"bug", "runbook:deploy", "a.b_c/d-e0"}); err != nil {
			t.Fatalf("ValidateTags: unexpected error %v", err)
		}
	})

	bad := []struct {
		name string
		in   []string
		// mustMention is a substring the error MUST contain, so the message
		// actually identifies what the caller has to fix.
		mustMention string
	}{
		{"invalid charset", []string{"needs!review"}, "needs!review"},
		{"uppercase survives only if un-normalized (caller bug)", []string{"Bug"}, "Bug"},
		{"too long", []string{long}, "too long"},
		{"too many tags", many, "too many tags"},
		{"multiple colons", []string{"a:b:c"}, "more than one ':'"},
		{"empty namespace", []string{":value"}, "empty namespace"},
		{"empty value", []string{"ns:"}, "empty namespace or value"},
		{"empty tag reaching validation is reported, not ignored", []string{""}, "empty tag"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTags(tc.in)
			if err == nil {
				t.Fatalf("ValidateTags(%q) = nil, want an error", tc.in)
			}
			if !strings.Contains(err.Error(), tc.mustMention) {
				t.Fatalf("error %q must mention %q", err.Error(), tc.mustMention)
			}
		})
	}
}

// TestValidateTagsErrorIsBounded pins that a pathological tag can't produce a
// pathological error string (which would land in logs and HTTP bodies).
func TestValidateTagsErrorIsBounded(t *testing.T) {
	err := ValidateTags([]string{strings.Repeat("z", 10_000)})
	if err == nil {
		t.Fatal("a 10k-rune tag must be rejected")
	}
	if n := len([]rune(err.Error())); n > 200 {
		t.Fatalf("error message is %d runes; it must stay bounded", n)
	}
}

// TestNormalizeAndValidate covers the combined write-path entry point, including
// the >20-tags and >64-rune bounds after normalization.
func TestNormalizeAndValidate(t *testing.T) {
	t.Run("normalizes then passes", func(t *testing.T) {
		got, err := NormalizeAndValidate([]string{" Bug ", "bug", "Runbook:Deploy"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 2 || got[0] != "bug" || got[1] != "runbook:deploy" {
			t.Fatalf("got %q, want [bug runbook:deploy]", got)
		}
	})
	t.Run("rejects >64 runes", func(t *testing.T) {
		if _, err := NormalizeAndValidate([]string{strings.Repeat("a", MaxTagRunes+1)}); err == nil {
			t.Fatal("want an error for a 65-rune tag")
		}
	})
	t.Run("rejects >20 tags", func(t *testing.T) {
		in := make([]string, 0, MaxTags+1)
		for i := 0; i <= MaxTags; i++ {
			in = append(in, "tag-"+string(rune('a'+i)))
		}
		if _, err := NormalizeAndValidate(in); err == nil {
			t.Fatalf("want an error for %d tags", len(in))
		}
	})
	t.Run("dedupe brings a 21-duplicate payload back under the cap", func(t *testing.T) {
		in := make([]string, MaxTags+5)
		for i := range in {
			in[i] = "same"
		}
		got, err := NormalizeAndValidate(in)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("got %q, want one deduped tag", got)
		}
	})
}

// TestParseTagAndIsRoutingTag asserts EVERY reserved namespace is recognised as
// routing and that an unknown namespace stays DESCRIPTIVE — the reserved set is a
// closed allowlist, so `civitai:frontend` must be a plain label.
func TestParseTagAndIsRoutingTag(t *testing.T) {
	cases := []struct {
		tag     string
		ns      string
		val     string
		routing bool
	}{
		// runbook: was a routing namespace until runbooks were removed; a stale
		// tag is now an ordinary descriptive label.
		{"runbook:deploy", "runbook", "deploy", false},
		{"initiative:remix", NSInitiative, "remix", true},
		{"gate:needs-decision", NSGate, "needs-decision", true},
		{"auto:dispatch", NSAuto, "dispatch", true},
		{"civitai:frontend", "civitai", "frontend", false},
		{"bug", "", "bug", false},
		{"a/b-c", "", "a/b-c", false},
	}
	for _, tc := range cases {
		t.Run(tc.tag, func(t *testing.T) {
			ns, val := ParseTag(tc.tag)
			if ns != tc.ns || val != tc.val {
				t.Fatalf("ParseTag(%q) = (%q,%q), want (%q,%q)", tc.tag, ns, val, tc.ns, tc.val)
			}
			if got := IsRoutingTag(tc.tag); got != tc.routing {
				t.Fatalf("IsRoutingTag(%q) = %v, want %v", tc.tag, got, tc.routing)
			}
		})
	}
}

// TestRoutingNamespacesIsClosed pins the reserved set: adding a namespace must be
// a deliberate code change (it also bounds the Prometheus label space).
func TestRoutingNamespacesIsClosed(t *testing.T) {
	got := RoutingNamespaces()
	want := []string{"auto", "gate", "initiative"} // sorted; literal, not the constants
	if len(got) != len(want) {
		t.Fatalf("RoutingNamespaces() = %q, want %q", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("RoutingNamespaces() = %q, want %q", got, want)
		}
	}
}

// TestRoutingHelpers covers the behaviour accessors the HTTP/UI layers rely on.
func TestRoutingHelpers(t *testing.T) {
	// runbook:deploy is a STALE tag from before runbooks were removed: it must not
	// count as routing.
	tags := []string{"bug", "gate:needs-decision", "initiative:remix", "runbook:deploy"}

	if reason, ok := GateReason(tags); !ok || reason != "needs-decision" {
		t.Fatalf("GateReason = (%q,%v), want (needs-decision,true)", reason, ok)
	}
	if !IsGated(tags) {
		t.Fatal("IsGated must be true when a gate: tag is present")
	}
	if IsGated([]string{"bug"}) {
		t.Fatal("IsGated must be false with no gate: tag")
	}
	if got := RoutingTags(tags); strings.Join(got, ",") != "gate:needs-decision,initiative:remix" {
		t.Fatalf("RoutingTags = %q, want [gate:needs-decision initiative:remix]", got)
	}
}

// TestAutoDispatchEligible pins the SHIPPED-OFF default: the tag is recognised,
// but eligibility requires the flag AND a non-gated task.
func TestAutoDispatchEligible(t *testing.T) {
	auto := []string{AutoDispatchTag}
	autoGated := []string{AutoDispatchTag, "gate:needs-decision"}

	if !HasAutoDispatch(auto) {
		t.Fatal("HasAutoDispatch must recognise auto:dispatch")
	}
	if AutoDispatchEligible(false, auto) {
		t.Fatal("with the flag OFF, auto:dispatch must NOT be eligible (it is a descriptive tag)")
	}
	if !AutoDispatchEligible(true, auto) {
		t.Fatal("with the flag ON, an ungated auto:dispatch task must be eligible")
	}
	if AutoDispatchEligible(true, autoGated) {
		t.Fatal("a gated task must never be auto-dispatch eligible, even with the flag ON")
	}
}

// TestMergeAndSubtractTags covers the pure set operations behind the store's
// single-statement merges (order-independent, idempotent, normalized).
func TestMergeAndSubtractTags(t *testing.T) {
	got := MergeTags([]string{"b", "a"}, []string{"A", "c"})
	if strings.Join(got, ",") != "a,b,c" {
		t.Fatalf("MergeTags = %q, want [a b c]", got)
	}
	if strings.Join(MergeTags(got, got), ",") != "a,b,c" {
		t.Fatal("MergeTags must be idempotent")
	}
	if strings.Join(SubtractTags(got, []string{"B"}), ",") != "a,c" {
		t.Fatalf("SubtractTags = %q, want [a c]", SubtractTags(got, []string{"B"}))
	}
	if strings.Join(SubtractTags(got, []string{"zzz"}), ",") != "a,b,c" {
		t.Fatal("removing an absent tag must be a no-op")
	}
}

// TestHasAllTags pins the AND filter semantics used by the in-memory path and by
// the UI's filtered-empty decision.
func TestHasAllTags(t *testing.T) {
	have := []string{"a", "b", "c"}
	if !HasAllTags(have, nil) {
		t.Fatal("an empty filter must match everything")
	}
	if !HasAllTags(have, []string{"a", "c"}) {
		t.Fatal("a subset must match")
	}
	if HasAllTags(have, []string{"a", "z"}) {
		t.Fatal("AND semantics: a missing tag must NOT match")
	}
}
