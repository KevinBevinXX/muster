package suggest

import "testing"

// TestATermIsCheckedByItsNumbersAndAPlainHyphenatedWordIsNot: a number must
// appear in the input as a number of its own ("9" inside "429" does not), a
// path or flag must appear whole, and English like "re-reading" is not a term.
func TestATermIsCheckedByItsNumbersAndAPlainHyphenatedWordIsNot(t *testing.T) {
	b, _ := Build(Input{Tail: []Entry{{Role: RoleAssistant, Text: "It updates about 12,400 orders; the API returned 429 twice. " +
		"(b) is simpler but re-reads the source. Should I run it on 12,400 orders now?"}}})
	q := "It updates about 12,400 orders"
	cases := []struct {
		text string
		drop bool
	}{
		{"Run it on the 12,400-order set, but only after a backup of the orders", false},
		{"Run the orders update, but only after release 9.9 of the API", true},
		{"Update the orders, but first try a re-reading pass", false},
		{"Update the orders, but only with --batch-size", true},
		{"Update the orders, but first read scripts/orders_fix.sql", true},
	}
	for _, c := range cases {
		_, dropped := Filter([]Raw{{Text: c.text, Kind: KindDecide, Cites: []string{"R"}, Quote: q}}, b)
		got := len(dropped) == 1 && dropped[0].Reason == DropUnknownTerm
		if got != c.drop {
			t.Errorf("%q: dropped as an unknown term = %v (drops %+v), want %v", c.text, got, dropped, c.drop)
		}
	}
}
