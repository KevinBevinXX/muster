package hermeshost

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestATabIsComparedAsHermesDrawsIt: prompt_toolkit draws a tab in the box
// as `^I` (two cells). A message with a tab, pasted, shows `…^I…`, so the
// post-paste check compares the message with every tab as `^I` — before,
// the box never matched, and every message with a tab was left pasted and
// unsubmitted. InputRows counts a tab as its two cells.
func TestATabIsComparedAsHermesDrawsIt(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "screens", "pasted-short.ansi"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "hello from muster") {
		t.Fatal("instrument check: the fixture does not hold the pasted text")
	}
	s := ParseScreen(strings.Replace(string(b), "hello from muster", "hello^Ifrom muster", 1))
	if err := CheckPasted(s, "hello\tfrom muster"); err != nil {
		t.Fatalf("a tab drawn as ^I: %v", err)
	}
	if err := CheckPasted(s, "hello from muster"); err == nil {
		t.Fatal("the box shows ^I and the message has a space: admitted")
	}
	if n := InputRows("\t", 0, 2); n != 1 {
		t.Errorf("a tab in a 2-column box is %d rows, want 1 (two cells)", n)
	}
	if n := InputRows("a\t", 0, 2); n != 2 {
		t.Errorf("a + tab in a 2-column box is %d rows, want 2", n)
	}
}

// TestViModeIsReadAsHermesReadsIt: display.vim_mode is Python's bool() of the
// YAML 1.1 value (cli_init_mixin.py:566), so a quoted "no" is TRUE and an
// unreadable file fails closed (an error); no config.yaml, no display
// section, or a false value is off.
func TestViModeIsReadAsHermesReadsIt(t *testing.T) {
	for _, c := range []struct {
		yaml string
		on   bool
		err  bool
	}{
		{"", false, false},
		{"model:\n  default: x\n", false, false},
		{"display:\n  vim_mode: true\n", true, false},
		{"display:\n  vim_mode: True\n", true, false},
		{"display:\n  vim_mode: yes\n", true, false},
		{"display:\n  vim_mode: on\n", true, false},
		{"display:\n  vim_mode: 1\n", true, false},
		{"display:\n  vim_mode: \"no\"\n", true, false},
		{"display:\n  vim_mode: [x]\n", true, false},
		{"display:\n  vim_mode: false\n", false, false},
		{"display:\n  vim_mode: no\n", false, false},
		{"display:\n  vim_mode: off\n", false, false},
		{"display:\n  vim_mode: 0\n", false, false},
		{"display:\n  vim_mode: ~\n", false, false},
		{"display:\n  vim_mode: \"\"\n", false, false},
		{"display:\n  language: de\n", false, false},
		{"display: [\n", false, true},
		{"display: 3\n", false, true},
	} {
		dir := t.TempDir()
		if c.yaml != "" {
			if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(c.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		on, err := NewHome(dir).ViMode()
		if on != c.on || (err != nil) != c.err {
			t.Errorf("%q: ViMode = %t, %v; want %t, error %t", c.yaml, on, err, c.on, c.err)
		}
		if err != nil && strings.Contains(err.Error(), "display") && strings.Contains(err.Error(), "[") {
			t.Errorf("%q: the error quotes the file: %v", c.yaml, err)
		}
	}
}
