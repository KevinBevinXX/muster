package opencodehost

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExpiryReadsTheTokensOwnPrefixAndRefusesOtherShapes(t *testing.T) {
	exp, ok := Expiry("1893456000.c2lnbmF0dXJl")
	if !ok || !exp.Equal(time.Unix(1893456000, 0)) {
		t.Fatalf("Expiry = %v %v", exp, ok)
	}
	for _, bad := range []string{"", "1893456000", "1893456000.", "x.sig", "-5.sig", "1.a.b", "password-not-a-token"} {
		if _, ok := Expiry(bad); ok {
			t.Errorf("Expiry(%q) accepted", bad)
		}
	}
}

func TestFingerprintIsShortStableAndRevealsNothing(t *testing.T) {
	tok := "1893456000.c2lnbmF0dXJlLW9mLWEtZml4dHVyZS10b2tlbg"
	fp := Fingerprint(tok)
	if len(fp) != 8 || fp != Fingerprint(tok) || fp == Fingerprint(tok+"x") {
		t.Fatalf("fingerprint %q", fp)
	}
	if strings.Contains(tok, fp) {
		t.Fatalf("the fingerprint %q is a substring of the token", fp)
	}
}

// writeRaw writes a token file by hand with mode perm.
func writeRaw(t *testing.T, path, body string, perm os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
}

// TestReadTokenFileRefusesAFileOthersCanReadASymlinkAndAForeignServer: the
// token reaches the whole opencode API, so a token file anyone else can read
// is refused (as the MCP secret is), and so is one whose server is not this
// host's own — muster-mirror never sends the token off the host.
func TestReadTokenFileRefusesAFileOthersCanReadASymlinkAndAForeignServer(t *testing.T) {
	dir := t.TempDir()
	good := `{"url":"http://127.0.0.1:4096","token":"1893456000.c2ln"}`
	p := filepath.Join(dir, "tok")
	writeRaw(t, p, good, 0o600)
	if tf, err := ReadTokenFile(p); err != nil || tf.URL != "http://127.0.0.1:4096" || tf.Token != "1893456000.c2ln" {
		t.Fatalf("the good file: %+v %v", tf, err)
	}
	writeRaw(t, p, good, 0o640)
	if _, err := ReadTokenFile(p); err == nil || !strings.Contains(err.Error(), "readable by others") {
		t.Fatalf("a 0640 file: %v", err)
	}
	writeRaw(t, p, good, 0o600)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(p, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadTokenFile(link); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("a symlink: %v", err)
	}
	for _, url := range []string{"http://192.0.2.10:4096", "https://127.0.0.1:4096", "http://opencode.example:4096", "http://127.0.0.1:4096/api", "http://user@127.0.0.1:4096"} {
		writeRaw(t, p, `{"url":"`+url+`","token":"1893456000.c2ln"}`, 0o600)
		if _, err := ReadTokenFile(p); err == nil {
			t.Errorf("a token for %s was accepted", url)
		}
	}
	writeRaw(t, p, `{"url":"http://127.0.0.1:4096","token":"the-server-password"}`, 0o600)
	if _, err := ReadTokenFile(p); err == nil {
		t.Error("a file holding no pair token was accepted")
	}
}

// TestWriteTokenFileIsWholeAnd0600EvenOverAWiderFile: a token file another
// tool left 0644 is REPLACED by a 0600 one, never written through.
func TestWriteTokenFileIsWholeAnd0600EvenOverAWiderFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "tok")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	writeRaw(t, p, "old and much longer content than the new one will be, really", 0o644)
	if err := WriteTokenFile(p, TokenFile{URL: "http://127.0.0.1:4096", Token: "1893456000.c2ln"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", fi.Mode(), err)
	}
	if tf, err := ReadTokenFile(p); err != nil || tf.Token != "1893456000.c2ln" {
		t.Fatalf("read back %+v %v", tf, err)
	}
	// A missing directory is created 0700.
	q := filepath.Join(dir, "new", "tok")
	if err := WriteTokenFile(q, TokenFile{URL: "http://127.0.0.1:4096", Token: "1893456000.c2ln"}); err != nil {
		t.Fatal(err)
	}
	if di, _ := os.Stat(filepath.Dir(q)); di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", di.Mode())
	}
}
