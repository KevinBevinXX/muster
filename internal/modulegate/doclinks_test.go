package modulegate

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestEveryRelativeDocLinkResolves checks the human-facing documentation's
// relative links: every `[text](path)`, `[text](path#anchor)`, `(#anchor)` and
// `<img src="path">` in README.md, CONTRIBUTING.md and docs/**/*.md must name a
// file (or directory) that exists, and an anchor must name a heading in the
// target Markdown file (GitHub's slug rule). Absolute URLs are not fetched —
// this is about the tree, not the internet. Fenced code blocks are skipped.
func TestEveryRelativeDocLinkResolves(t *testing.T) {
	root := moduleRoot(t)
	files := []string{filepath.Join(root, "README.md"), filepath.Join(root, "CONTRIBUTING.md")}
	_ = filepath.WalkDir(filepath.Join(root, "docs"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".md") {
			files = append(files, p)
		}
		return nil
	})
	checked, broken := checkDocLinks(t, root, files)
	for _, b := range broken {
		t.Error(b)
	}
	// POSITIVE CONTROL: a checker that extracts nothing passes every tree.
	if checked < 40 {
		t.Errorf("positive control FAILED: only %d relative link(s) were checked across %d file(s); "+
			"the extraction has stopped matching", checked, len(files))
	}
	t.Logf("%d relative link(s) checked in %d file(s), %d broken", checked, len(files), len(broken))
}

// TestTheDocLinkCheckerRefusesWhatItShould is the negative control: a missing
// file, a missing anchor and a missing image are each reported, a good link is
// not, and a link inside a code fence is ignored.
func TestTheDocLinkCheckerRefusesWhatItShould(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "b.md"), []byte("# Real heading: `x`, (y)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := "# A\n\n[ok](b.md#real-heading-x-y) [self](#a) [web](https://example.com/nope)\n" +
		"[gone](missing.md) [bad anchor](b.md#nope) <img src=\"img/none.png\">\n" +
		"```\n[in a fence](also-missing.md)\n```\n"
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte(a), 0o644); err != nil {
		t.Fatal(err)
	}
	checked, broken := checkDocLinks(t, dir, []string{filepath.Join(dir, "a.md")})
	if checked != 5 {
		t.Errorf("checked %d relative links, want 5 (two good, three bad; the web link and the fenced one are not checked)", checked)
	}
	want := []string{"missing.md", "b.md#nope", "img/none.png"}
	if len(broken) != len(want) {
		t.Fatalf("broken = %q, want one entry each for %q", broken, want)
	}
	for i, w := range want {
		if !strings.Contains(broken[i], w) {
			t.Errorf("broken[%d] = %q, want it to name %q", i, broken[i], w)
		}
	}
}

var (
	mdLink   = regexp.MustCompile(`\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)
	imgSrc   = regexp.MustCompile(`<img[^>]*\ssrc="([^"]+)"`)
	mdHead   = regexp.MustCompile(`^#{1,6}\s+(.+?)\s*#*\s*$`)
	slugDrop = regexp.MustCompile(`[^\p{L}\p{N} _-]`)
)

func checkDocLinks(t *testing.T, root string, files []string) (checked int, broken []string) {
	t.Helper()
	anchors := map[string]map[string]bool{}
	headingsOf := func(p string) map[string]bool {
		if h, ok := anchors[p]; ok {
			return h
		}
		h := map[string]bool{}
		b, err := os.ReadFile(p)
		if err == nil {
			seen := map[string]int{}
			for _, line := range fenceless(string(b)) {
				if m := mdHead.FindStringSubmatch(line); m != nil {
					s := slugDrop.ReplaceAllString(strings.ToLower(m[1]), "")
					s = strings.ReplaceAll(s, " ", "-")
					if n := seen[s]; n > 0 {
						h[s+"-"+strconv.Itoa(n)] = true
					} else {
						h[s] = true
					}
					seen[s]++
				}
			}
		}
		anchors[p] = h
		return h
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		rel, _ := filepath.Rel(root, f)
		for _, line := range fenceless(string(b)) {
			var targets []string
			for _, m := range mdLink.FindAllStringSubmatch(line, -1) {
				targets = append(targets, m[1])
			}
			for _, m := range imgSrc.FindAllStringSubmatch(line, -1) {
				targets = append(targets, m[1])
			}
			for _, target := range targets {
				if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
					continue
				}
				checked++
				path, anchor, _ := strings.Cut(target, "#")
				dest := f
				if path != "" {
					dest = filepath.Join(filepath.Dir(f), filepath.FromSlash(path))
					if _, err := os.Stat(dest); err != nil {
						broken = append(broken, rel+": link to "+target+" — no such file")
						continue
					}
				}
				if anchor != "" && strings.HasSuffix(dest, ".md") && !headingsOf(dest)[anchor] {
					broken = append(broken, rel+": link to "+target+" — no heading with that anchor")
				}
			}
		}
	}
	return checked, broken
}

// fenceless returns the lines of a Markdown document outside ``` fences.
func fenceless(doc string) []string {
	var out []string
	in := false
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			in = !in
			continue
		}
		if !in {
			out = append(out, line)
		}
	}
	return out
}
