package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteReplacesWholeAt0600AndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.json")
	if err := os.WriteFile(p, []byte("old, longer content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Write(p, []byte("new")); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	fi, _ := os.Stat(p)
	if string(b) != "new" || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%q %v", b, fi.Mode())
	}
	if m, _ := filepath.Glob(filepath.Join(dir, ".*.tmp")); len(m) != 0 {
		t.Fatalf("left %v", m)
	}
	if err := Write(filepath.Join(dir, "missing", "f"), []byte("x")); err == nil {
		t.Fatal("wrote into a missing directory")
	}
}
