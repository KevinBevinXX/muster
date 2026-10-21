package procstat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseReadsACommWithSpacesAndParens(t *testing.T) {
	st, ok := Parse([]byte("4242 (tmux: client) (x) S 17 1 1 0 -1 4194560 0 0 0 0 0 0 0 0 20 0 1 0 98765 0 0"))
	if !ok || st.Comm != "tmux: client) (x" || st.State != "S" || st.PPID != 17 || st.Start != "98765" {
		t.Fatalf("Parse = %+v %v", st, ok)
	}
	if _, ok := Parse([]byte("garbage")); ok {
		t.Fatal("parsed garbage")
	}
	if st, ok := Parse([]byte("1 (init) S 0")); !ok || st.Start != "" {
		t.Fatalf("a short line: %+v %v", st, ok)
	}
}

// TestOneStatParserAndOneAtomicWriter (round-0 decision D4): the host's code
// has ONE /proc stat parser (this package) and ONE atomic file writer
// (internal/atomicfile). A second copy of either fails here.
func TestOneStatParserAndOneAtomicWriter(t *testing.T) {
	scanned := 0
	for _, dir := range []string{"../../cmd/muster-mirror", "../mirrorhost", "../hermeshost"} {
		files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			scanned++
			src := string(b)
			// A temp file renamed over its target is an atomic REPLACE: that is
			// atomicfile's. (mcpproxy.go's secret is created by a temp file
			// LINKED into place — create-if-absent, not a replace — and is
			// not one.)
			if strings.Contains(src, "os.CreateTemp(") && strings.Contains(src, "os.Rename(") {
				t.Errorf("%s replaces a file through a temp file of its own: use internal/atomicfile", f)
			}
			if strings.Contains(src, "LastIndexByte(stat") || strings.Contains(src, `LastIndexByte(string(b), ')')`) {
				t.Errorf("%s parses /proc stat itself: use internal/procstat", f)
			}
		}
	}
	if scanned < 20 {
		t.Fatalf("instrument check: scanned %d files", scanned)
	}
}
